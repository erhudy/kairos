package pkg

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-co-op/gocron"
	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kairosv1alpha1 "github.com/erhudy/kairos/api/v1alpha1"
)

// ChainReconciler reconciles RestartChain objects.
type ChainReconciler struct {
	client    client.Client
	clientset kubernetes.Interface
	logger    *zap.Logger
	cron      *gocron.Scheduler
	metrics   *KairosMetrics
	// cronJobs tracks gocron jobs by RestartChain name (cluster-scoped)
	cronJobs sync.Map
}

// NewChainReconciler creates a new ChainReconciler.
func NewChainReconciler(c client.Client, clientset kubernetes.Interface, logger *zap.Logger, timezone *time.Location, metrics *KairosMetrics) *ChainReconciler {
	scheduler := gocron.NewScheduler(timezone)
	scheduler.TagsUnique()
	return &ChainReconciler{
		client:    c,
		clientset: clientset,
		logger:    logger,
		cron:      scheduler,
		metrics:   metrics,
	}
}

// Start starts the gocron scheduler. Called by controller-runtime manager.
func (r *ChainReconciler) Start(ctx context.Context) error {
	r.cron.StartAsync()
	<-ctx.Done()
	r.cron.Stop()
	return nil
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *ChainReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kairosv1alpha1.RestartChain{}).
		Complete(r)
}

// Reconcile is the main reconciliation loop for RestartChain resources.
func (r *ChainReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := r.logger.With(zap.String("chain", req.String()))

	var chain kairosv1alpha1.RestartChain
	if err := r.client.Get(ctx, req.NamespacedName, &chain); err != nil {
		// Object deleted — clean up cron job
		r.removeCronJob(req.Name)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Ensure cron job is configured
	if err := r.ensureCronJob(&chain); err != nil {
		logger.Error("failed to configure cron job", zap.Error(err))
		return ctrl.Result{}, err
	}

	// If paused, ensure no cron job is running
	if chain.Spec.Paused {
		r.removeCronJob(req.Name)
		return ctrl.Result{}, nil
	}

	// Drive the state machine
	switch chain.Status.Phase {
	case kairosv1alpha1.ChainPhaseRunning:
		return r.reconcileRunning(ctx, &chain, logger)
	default:
		// Idle, Completed, Failed, or empty — nothing to do until cron fires
		return ctrl.Result{}, nil
	}
}

// reconcileRunning drives the chain execution state machine.
func (r *ChainReconciler) reconcileRunning(ctx context.Context, chain *kairosv1alpha1.RestartChain, logger *zap.Logger) (ctrl.Result, error) {
	stepIdx := int(chain.Status.CurrentStep)
	if stepIdx < 0 || stepIdx >= len(chain.Spec.Steps) {
		// All steps complete or invalid index
		return r.completeChain(ctx, chain, logger)
	}

	step := chain.Spec.Steps[stepIdx]
	ns := step.ResourceRef.Namespace

	// Initialize step statuses if needed
	if len(chain.Status.StepStatuses) <= stepIdx {
		for len(chain.Status.StepStatuses) <= stepIdx {
			idx := len(chain.Status.StepStatuses)
			chain.Status.StepStatuses = append(chain.Status.StepStatuses, kairosv1alpha1.StepStatus{
				ResourceRef: chain.Spec.Steps[idx].ResourceRef,
				Phase:       kairosv1alpha1.StepPhasePending,
			})
		}
	}

	stepStatus := &chain.Status.StepStatuses[stepIdx]

	switch stepStatus.Phase {
	case kairosv1alpha1.StepPhasePending:
		// Trigger the restart
		logger.Info("restarting resource",
			zap.String("kind", step.ResourceRef.Kind),
			zap.String("namespace", ns),
			zap.String("name", step.ResourceRef.Name),
			zap.Int("step", stepIdx),
		)

		result, err := RestartResource(ctx, r.clientset, step.ResourceRef.Kind, ns, step.ResourceRef.Name)
		if err != nil {
			logger.Error("failed to restart resource", zap.Error(err))
			stepStatus.Phase = kairosv1alpha1.StepPhaseFailed
			stepStatus.Message = err.Error()
			now := metav1.Now()
			stepStatus.CompletedAt = &now
			return r.failChain(ctx, chain, logger, fmt.Sprintf("step %d failed: %v", stepIdx, err))
		}

		now := metav1.Now()
		stepStatus.Phase = kairosv1alpha1.StepPhaseRestarting
		stepStatus.StartedAt = &now
		stepStatus.ObservedGeneration = result.Generation
		stepStatus.Message = "restart initiated"

		if r.metrics != nil {
			r.metrics.RestartTotal.WithLabelValues(step.ResourceRef.Kind, ns, step.ResourceRef.Name).Inc()
		}

		if err := r.client.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}
		// Requeue immediately to check generation
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil

	case kairosv1alpha1.StepPhaseRestarting:
		// Transition to WaitingForHealthy — the restart has been issued
		stepStatus.Phase = kairosv1alpha1.StepPhaseWaitingForHealthy
		stepStatus.Message = "waiting for health check"
		if err := r.client.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}
		pollInterval := time.Duration(step.HealthCheck.PollIntervalSeconds) * time.Second
		if pollInterval <= 0 {
			pollInterval = 5 * time.Second
		}
		return ctrl.Result{RequeueAfter: pollInterval}, nil

	case kairosv1alpha1.StepPhaseWaitingForHealthy:
		return r.checkHealth(ctx, chain, stepIdx, logger)

	case kairosv1alpha1.StepPhaseCompleted:
		// Advance to next step
		chain.Status.CurrentStep = int32(stepIdx + 1)
		if int(chain.Status.CurrentStep) >= len(chain.Spec.Steps) {
			return r.completeChain(ctx, chain, logger)
		}
		if err := r.client.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Millisecond}, nil

	case kairosv1alpha1.StepPhaseFailed:
		return r.failChain(ctx, chain, logger, stepStatus.Message)

	default:
		return ctrl.Result{}, nil
	}
}

// checkHealth checks the health of the current step's resource.
func (r *ChainReconciler) checkHealth(ctx context.Context, chain *kairosv1alpha1.RestartChain, stepIdx int, logger *zap.Logger) (ctrl.Result, error) {
	step := chain.Spec.Steps[stepIdx]
	stepStatus := &chain.Status.StepStatuses[stepIdx]
	ns := step.ResourceRef.Namespace

	strategy := step.HealthCheck.Strategy
	if strategy == "" {
		strategy = kairosv1alpha1.HealthCheckRolloutComplete
	}

	timeoutSeconds := step.HealthCheck.TimeoutSeconds
	if timeoutSeconds <= 0 {
		timeoutSeconds = 600
	}

	pollInterval := time.Duration(step.HealthCheck.PollIntervalSeconds) * time.Second
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}

	// Check if timed out
	elapsed := time.Since(stepStatus.StartedAt.Time)
	timedOut := elapsed > time.Duration(timeoutSeconds)*time.Second

	switch strategy {
	case kairosv1alpha1.HealthCheckFixedTimeout:
		if timedOut {
			now := metav1.Now()
			stepStatus.Phase = kairosv1alpha1.StepPhaseCompleted
			stepStatus.CompletedAt = &now
			stepStatus.Message = fmt.Sprintf("fixed timeout completed after %s", elapsed.Truncate(time.Second))
			if err := r.client.Status().Update(ctx, chain); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Millisecond}, nil
		}
		stepStatus.Message = fmt.Sprintf("waiting for fixed timeout (%s/%ds)", elapsed.Truncate(time.Second), timeoutSeconds)
		if err := r.client.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: pollInterval}, nil

	case kairosv1alpha1.HealthCheckRolloutComplete, kairosv1alpha1.HealthCheckRolloutWithTimeout:
		status, err := CheckRolloutStatus(ctx, r.clientset, step.ResourceRef.Kind, ns, step.ResourceRef.Name, stepStatus.ObservedGeneration)
		if err != nil {
			logger.Error("error checking rollout status", zap.Error(err))
			stepStatus.Message = fmt.Sprintf("error checking rollout: %v", err)
			if err := r.client.Status().Update(ctx, chain); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: pollInterval}, nil
		}

		if status.Done {
			now := metav1.Now()
			stepStatus.Phase = kairosv1alpha1.StepPhaseCompleted
			stepStatus.CompletedAt = &now
			stepStatus.Message = fmt.Sprintf("rollout completed in %s", elapsed.Truncate(time.Second))

			if r.metrics != nil {
				r.metrics.ChainStepDuration.WithLabelValues(
					chain.Name,
					step.ResourceRef.Kind, ns, step.ResourceRef.Name,
				).Observe(elapsed.Seconds())
			}

			if err := r.client.Status().Update(ctx, chain); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: time.Millisecond}, nil
		}

		if timedOut {
			if strategy == kairosv1alpha1.HealthCheckRolloutWithTimeout {
				// Proceed with warning
				now := metav1.Now()
				stepStatus.Phase = kairosv1alpha1.StepPhaseCompleted
				stepStatus.CompletedAt = &now
				stepStatus.Message = fmt.Sprintf("timed out after %s, proceeding (rollout incomplete: %s)", elapsed.Truncate(time.Second), status.Message)
				logger.Warn("step timed out, proceeding", zap.String("resource", step.ResourceRef.Name), zap.String("status", status.Message))
				if err := r.client.Status().Update(ctx, chain); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: time.Millisecond}, nil
			}
			// RolloutComplete strategy — fail
			now := metav1.Now()
			stepStatus.Phase = kairosv1alpha1.StepPhaseFailed
			stepStatus.CompletedAt = &now
			stepStatus.Message = fmt.Sprintf("timed out after %s waiting for rollout: %s", elapsed.Truncate(time.Second), status.Message)
			return r.failChain(ctx, chain, logger, stepStatus.Message)
		}

		stepStatus.Message = status.Message
		if err := r.client.Status().Update(ctx, chain); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: pollInterval}, nil

	default:
		return ctrl.Result{}, fmt.Errorf("unknown health check strategy: %s", strategy)
	}
}

// completeChain marks the chain as completed.
func (r *ChainReconciler) completeChain(ctx context.Context, chain *kairosv1alpha1.RestartChain, logger *zap.Logger) (ctrl.Result, error) {
	now := metav1.Now()
	chain.Status.Phase = kairosv1alpha1.ChainPhaseCompleted
	chain.Status.LastCompletionTime = &now
	logger.Info("chain completed successfully")

	if r.metrics != nil {
		r.metrics.ChainExecutionTotal.WithLabelValues(chain.Name, "completed").Inc()
	}

	if err := r.client.Status().Update(ctx, chain); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// failChain marks the chain as failed.
func (r *ChainReconciler) failChain(ctx context.Context, chain *kairosv1alpha1.RestartChain, logger *zap.Logger, message string) (ctrl.Result, error) {
	now := metav1.Now()
	chain.Status.Phase = kairosv1alpha1.ChainPhaseFailed
	chain.Status.LastCompletionTime = &now
	logger.Error("chain failed", zap.String("reason", message))

	if r.metrics != nil {
		r.metrics.ChainExecutionTotal.WithLabelValues(chain.Name, "failed").Inc()
	}

	if err := r.client.Status().Update(ctx, chain); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// ensureCronJob creates or updates the gocron job for the chain.
func (r *ChainReconciler) ensureCronJob(chain *kairosv1alpha1.RestartChain) error {
	key := chain.Name

	if chain.Spec.Paused {
		r.removeCronJob(key)
		return nil
	}

	// Check if the cron job already exists with the same pattern
	if existing, ok := r.cronJobs.Load(key); ok {
		entry := existing.(*chainCronEntry)
		if entry.pattern == chain.Spec.CronPattern {
			return nil // Already up to date
		}
		// Pattern changed, remove old job
		r.removeCronJob(key)
	}

	cpString := chain.Spec.CronPattern
	expectedCountForCron := 5
	expectedCountForCronWithSeconds := 6
	if strings.HasPrefix(cpString, "TZ=") || strings.HasPrefix(cpString, "CRON_TZ=") {
		expectedCountForCron++
		expectedCountForCronWithSeconds++
	}

	var cronFunc func(string) *gocron.Scheduler
	l := len(strings.Split(cpString, " "))
	switch l {
	case expectedCountForCron:
		cronFunc = r.cron.Cron
	case expectedCountForCronWithSeconds:
		cronFunc = r.cron.CronWithSeconds
	default:
		return fmt.Errorf("got %d fields splitting cron expression '%s', expected 5 or 6", l, cpString)
	}

	tag := fmt.Sprintf("chain--%s", key)
	chainName := chain.Name

	job, err := cronFunc(cpString).Tag(tag).Do(func() {
		r.triggerChain(chainName)
	})
	if err != nil {
		return fmt.Errorf("error creating cron job for chain %s: %w", key, err)
	}

	r.cronJobs.Store(key, &chainCronEntry{
		pattern: chain.Spec.CronPattern,
		job:     job,
	})

	return nil
}

// removeCronJob removes the gocron job for a chain.
func (r *ChainReconciler) removeCronJob(key string) {
	if existing, ok := r.cronJobs.LoadAndDelete(key); ok {
		entry := existing.(*chainCronEntry)
		_ = r.cron.RemoveByID(entry.job)
	}
}

// triggerChain updates a RestartChain's status to start a new execution.
func (r *ChainReconciler) triggerChain(name string) {
	ctx := context.Background()
	logger := r.logger.With(zap.String("chain", name))

	var chain kairosv1alpha1.RestartChain
	if err := r.client.Get(ctx, client.ObjectKey{Name: name}, &chain); err != nil {
		logger.Error("failed to get chain for trigger", zap.Error(err))
		return
	}

	if chain.Spec.Paused {
		logger.Info("chain is paused, skipping trigger")
		return
	}

	// Guard: skip if already running
	if chain.Status.Phase == kairosv1alpha1.ChainPhaseRunning {
		logger.Warn("chain is already running, skipping trigger")
		if r.metrics != nil {
			r.metrics.ChainSkippedTotal.WithLabelValues(name).Inc()
		}
		return
	}

	now := metav1.Now()
	chain.Status.Phase = kairosv1alpha1.ChainPhaseRunning
	chain.Status.CurrentStep = 0
	chain.Status.LastRunTime = &now
	chain.Status.LastCompletionTime = nil
	chain.Status.StepStatuses = nil
	chain.Status.Conditions = nil

	if err := r.client.Status().Update(ctx, &chain); err != nil {
		logger.Error("failed to update chain status for trigger", zap.Error(err))
		return
	}

	logger.Info("chain triggered")
}

type chainCronEntry struct {
	pattern string
	job     *gocron.Job
}

// chainStatusEntry is the JSON representation for the /api/chains endpoint.
type chainStatusEntry struct {
	Name              string                        `json:"name"`
	CronPattern       string                        `json:"cronPattern"`
	Paused            bool                          `json:"paused"`
	Phase             kairosv1alpha1.ChainPhase     `json:"phase"`
	CurrentStep       int32                         `json:"currentStep"`
	LastRunTime       string                        `json:"lastRunTime"`
	LastCompletionTime string                       `json:"lastCompletionTime"`
	StepStatuses      []chainStepStatusEntry        `json:"stepStatuses"`
}

type chainStepStatusEntry struct {
	Kind        string                    `json:"kind"`
	Name        string                    `json:"name"`
	Namespace   string                    `json:"namespace"`
	Phase       kairosv1alpha1.StepPhase  `json:"phase"`
	StartedAt   string                    `json:"startedAt"`
	CompletedAt string                    `json:"completedAt"`
	Message     string                    `json:"message"`
}

// ChainStatusJSON serves the /api/chains endpoint.
func (r *ChainReconciler) ChainStatusJSON(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	var chainList kairosv1alpha1.RestartChainList
	if err := r.client.List(ctx, &chainList); err != nil {
		r.logger.Error("error listing chains", zap.Error(err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	entries := make([]chainStatusEntry, 0, len(chainList.Items))
	for _, chain := range chainList.Items {
		entry := chainStatusEntry{
			Name:        chain.Name,
			CronPattern: chain.Spec.CronPattern,
			Paused:      chain.Spec.Paused,
			Phase:       chain.Status.Phase,
			CurrentStep: chain.Status.CurrentStep,
		}
		if chain.Status.LastRunTime != nil {
			entry.LastRunTime = chain.Status.LastRunTime.UTC().Format(time.RFC3339)
		}
		if chain.Status.LastCompletionTime != nil {
			entry.LastCompletionTime = chain.Status.LastCompletionTime.UTC().Format(time.RFC3339)
		}

		for _, ss := range chain.Status.StepStatuses {
			stepEntry := chainStepStatusEntry{
				Kind:      ss.ResourceRef.Kind,
				Name:      ss.ResourceRef.Name,
				Namespace: ss.ResourceRef.Namespace,
				Phase:     ss.Phase,
				Message:   ss.Message,
			}
			if ss.StartedAt != nil {
				stepEntry.StartedAt = ss.StartedAt.UTC().Format(time.RFC3339)
			}
			if ss.CompletedAt != nil {
				stepEntry.CompletedAt = ss.CompletedAt.UTC().Format(time.RFC3339)
			}
			entry.StepStatuses = append(entry.StepStatuses, stepEntry)
		}
		if entry.StepStatuses == nil {
			entry.StepStatuses = []chainStepStatusEntry{}
		}

		entries = append(entries, entry)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		r.logger.Error("error encoding chain status JSON", zap.Error(err))
	}
}
