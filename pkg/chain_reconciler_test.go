package pkg

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kairosv1alpha1 "github.com/erhudy/kairos/api/v1alpha1"
)

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = kairosv1alpha1.AddToScheme(s)
	_ = appsv1.AddToScheme(s)
	return s
}

func newTestChainReconciler(t *testing.T, crObjs []ctrlclient.Object, k8sObjs ...runtime.Object) (*ChainReconciler, ctrlclient.Client) {
	t.Helper()
	scheme := newTestScheme()
	fakeClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(crObjs...).WithStatusSubresource(&kairosv1alpha1.RestartChain{}).Build()
	k8sClientset := fake.NewClientset(k8sObjs...)
	logger := zap.NewNop()
	tz, _ := time.LoadLocation("")

	reconciler := NewChainReconciler(fakeClient, k8sClientset, logger, tz, nil)
	return reconciler, fakeClient
}

func chainKey(name string) types.NamespacedName {
	return types.NamespacedName{Name: name}
}

func TestReconcileIdleChain(t *testing.T) {
	t.Parallel()

	chain := &kairosv1alpha1.RestartChain{
		ObjectMeta: metav1.ObjectMeta{Name: "test-chain"},
		Spec: kairosv1alpha1.RestartChainSpec{
			CronPattern: "0 2 * * *",
			Steps: []kairosv1alpha1.RestartChainStep{
				{
					ResourceRef: kairosv1alpha1.ResourceRef{Kind: "Deployment", Name: "app", Namespace: "default"},
					HealthCheck: kairosv1alpha1.HealthCheck{Strategy: kairosv1alpha1.HealthCheckRolloutComplete, TimeoutSeconds: 300},
				},
			},
		},
		Status: kairosv1alpha1.RestartChainStatus{
			Phase:       kairosv1alpha1.ChainPhaseIdle,
			CurrentStep: -1,
		},
	}

	reconciler, _ := newTestChainReconciler(t, []ctrlclient.Object{chain})
	reconciler.cron.StartAsync()
	defer reconciler.cron.Stop()

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: chainKey("test-chain")})
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
}

func TestReconcileDeletedChain(t *testing.T) {
	t.Parallel()

	reconciler, _ := newTestChainReconciler(t, nil)
	reconciler.cron.StartAsync()
	defer reconciler.cron.Stop()

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: chainKey("nonexistent")})
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
}

func TestReconcilePausedChain(t *testing.T) {
	t.Parallel()

	chain := &kairosv1alpha1.RestartChain{
		ObjectMeta: metav1.ObjectMeta{Name: "paused-chain"},
		Spec: kairosv1alpha1.RestartChainSpec{
			CronPattern: "0 2 * * *",
			Paused:      true,
			Steps: []kairosv1alpha1.RestartChainStep{
				{
					ResourceRef: kairosv1alpha1.ResourceRef{Kind: "Deployment", Name: "app", Namespace: "default"},
				},
			},
		},
	}

	reconciler, _ := newTestChainReconciler(t, []ctrlclient.Object{chain})
	reconciler.cron.StartAsync()
	defer reconciler.cron.Stop()

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: chainKey("paused-chain")})
	require.NoError(t, err)
	require.Zero(t, result.RequeueAfter)
}

func TestReconcileRunningChainPendingStep(t *testing.T) {
	t.Parallel()

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
	}

	chain := &kairosv1alpha1.RestartChain{
		ObjectMeta: metav1.ObjectMeta{Name: "running-chain"},
		Spec: kairosv1alpha1.RestartChainSpec{
			CronPattern: "0 2 * * *",
			Steps: []kairosv1alpha1.RestartChainStep{
				{
					ResourceRef: kairosv1alpha1.ResourceRef{Kind: "Deployment", Name: "app", Namespace: "default"},
					HealthCheck: kairosv1alpha1.HealthCheck{Strategy: kairosv1alpha1.HealthCheckRolloutComplete, TimeoutSeconds: 300, PollIntervalSeconds: 5},
				},
			},
		},
		Status: kairosv1alpha1.RestartChainStatus{
			Phase:       kairosv1alpha1.ChainPhaseRunning,
			CurrentStep: 0,
		},
	}

	reconciler, fakeClient := newTestChainReconciler(t, []ctrlclient.Object{chain}, dep)
	reconciler.cron.StartAsync()
	defer reconciler.cron.Stop()

	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: chainKey("running-chain")})
	require.NoError(t, err)
	require.NotZero(t, result.RequeueAfter)

	var updated kairosv1alpha1.RestartChain
	err = fakeClient.Get(context.Background(), chainKey("running-chain"), &updated)
	require.NoError(t, err)
	require.Len(t, updated.Status.StepStatuses, 1)
	require.Equal(t, kairosv1alpha1.StepPhaseRestarting, updated.Status.StepStatuses[0].Phase)
}

func TestReconcileRunningChainCompletesAllSteps(t *testing.T) {
	t.Parallel()

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "default"},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(1))},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 1,
			UpdatedReplicas:    1,
			AvailableReplicas:  1,
		},
	}

	pastTime := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	chain := &kairosv1alpha1.RestartChain{
		ObjectMeta: metav1.ObjectMeta{Name: "complete-chain"},
		Spec: kairosv1alpha1.RestartChainSpec{
			CronPattern: "0 2 * * *",
			Steps: []kairosv1alpha1.RestartChainStep{
				{
					ResourceRef: kairosv1alpha1.ResourceRef{Kind: "Deployment", Name: "app", Namespace: "default"},
					HealthCheck: kairosv1alpha1.HealthCheck{Strategy: kairosv1alpha1.HealthCheckFixedTimeout, TimeoutSeconds: 1, PollIntervalSeconds: 1},
				},
			},
		},
		Status: kairosv1alpha1.RestartChainStatus{
			Phase:       kairosv1alpha1.ChainPhaseRunning,
			CurrentStep: 0,
			StepStatuses: []kairosv1alpha1.StepStatus{
				{
					ResourceRef:        kairosv1alpha1.ResourceRef{Kind: "Deployment", Name: "app", Namespace: "default"},
					Phase:              kairosv1alpha1.StepPhaseWaitingForHealthy,
					StartedAt:          &pastTime,
					ObservedGeneration: 1,
				},
			},
		},
	}

	reconciler, fakeClient := newTestChainReconciler(t, []ctrlclient.Object{chain}, dep)
	reconciler.cron.StartAsync()
	defer reconciler.cron.Stop()

	req := ctrl.Request{NamespacedName: chainKey("complete-chain")}
	for range 10 {
		_, err := reconciler.Reconcile(context.Background(), req)
		require.NoError(t, err)

		var current kairosv1alpha1.RestartChain
		err = fakeClient.Get(context.Background(), req.NamespacedName, &current)
		require.NoError(t, err)
		if current.Status.Phase == kairosv1alpha1.ChainPhaseCompleted {
			return
		}
	}

	var updated kairosv1alpha1.RestartChain
	err := fakeClient.Get(context.Background(), req.NamespacedName, &updated)
	require.NoError(t, err)
	require.Equal(t, kairosv1alpha1.ChainPhaseCompleted, updated.Status.Phase)
}

func TestTriggerChainSkipsIfRunning(t *testing.T) {
	t.Parallel()

	chain := &kairosv1alpha1.RestartChain{
		ObjectMeta: metav1.ObjectMeta{Name: "running-chain"},
		Spec: kairosv1alpha1.RestartChainSpec{
			CronPattern: "0 2 * * *",
			Steps: []kairosv1alpha1.RestartChainStep{
				{
					ResourceRef: kairosv1alpha1.ResourceRef{Kind: "Deployment", Name: "app", Namespace: "default"},
				},
			},
		},
		Status: kairosv1alpha1.RestartChainStatus{
			Phase:       kairosv1alpha1.ChainPhaseRunning,
			CurrentStep: 0,
		},
	}

	reconciler, fakeClient := newTestChainReconciler(t, []ctrlclient.Object{chain})
	reconciler.cron.StartAsync()
	defer reconciler.cron.Stop()

	reconciler.triggerChain("running-chain")

	var updated kairosv1alpha1.RestartChain
	err := fakeClient.Get(context.Background(), chainKey("running-chain"), &updated)
	require.NoError(t, err)
	require.Equal(t, kairosv1alpha1.ChainPhaseRunning, updated.Status.Phase)
}

func TestTriggerChainStartsExecution(t *testing.T) {
	t.Parallel()

	chain := &kairosv1alpha1.RestartChain{
		ObjectMeta: metav1.ObjectMeta{Name: "idle-chain"},
		Spec: kairosv1alpha1.RestartChainSpec{
			CronPattern: "0 2 * * *",
			Steps: []kairosv1alpha1.RestartChainStep{
				{
					ResourceRef: kairosv1alpha1.ResourceRef{Kind: "Deployment", Name: "app", Namespace: "default"},
				},
			},
		},
		Status: kairosv1alpha1.RestartChainStatus{
			Phase: kairosv1alpha1.ChainPhaseIdle,
		},
	}

	reconciler, fakeClient := newTestChainReconciler(t, []ctrlclient.Object{chain})
	reconciler.cron.StartAsync()
	defer reconciler.cron.Stop()

	reconciler.triggerChain("idle-chain")

	var updated kairosv1alpha1.RestartChain
	err := fakeClient.Get(context.Background(), chainKey("idle-chain"), &updated)
	require.NoError(t, err)
	require.Equal(t, kairosv1alpha1.ChainPhaseRunning, updated.Status.Phase)
	require.Equal(t, int32(0), updated.Status.CurrentStep)
	require.NotNil(t, updated.Status.LastRunTime)
}

func TestEnsureCronJob(t *testing.T) {
	t.Parallel()

	chain := &kairosv1alpha1.RestartChain{
		ObjectMeta: metav1.ObjectMeta{Name: "cron-chain"},
		Spec: kairosv1alpha1.RestartChainSpec{
			CronPattern: "0 2 * * *",
			Steps: []kairosv1alpha1.RestartChainStep{
				{
					ResourceRef: kairosv1alpha1.ResourceRef{Kind: "Deployment", Name: "app", Namespace: "default"},
				},
			},
		},
	}

	reconciler, _ := newTestChainReconciler(t, []ctrlclient.Object{chain})
	reconciler.cron.StartAsync()
	defer reconciler.cron.Stop()

	err := reconciler.ensureCronJob(chain)
	require.NoError(t, err)

	_, ok := reconciler.cronJobs.Load("cron-chain")
	require.True(t, ok)

	// Calling again with same pattern should be idempotent
	err = reconciler.ensureCronJob(chain)
	require.NoError(t, err)
}

func TestEnsureCronJobPaused(t *testing.T) {
	t.Parallel()

	chain := &kairosv1alpha1.RestartChain{
		ObjectMeta: metav1.ObjectMeta{Name: "paused-chain"},
		Spec: kairosv1alpha1.RestartChainSpec{
			CronPattern: "0 2 * * *",
			Paused:      true,
			Steps: []kairosv1alpha1.RestartChainStep{
				{
					ResourceRef: kairosv1alpha1.ResourceRef{Kind: "Deployment", Name: "app", Namespace: "default"},
				},
			},
		},
	}

	reconciler, _ := newTestChainReconciler(t, []ctrlclient.Object{chain})
	reconciler.cron.StartAsync()
	defer reconciler.cron.Stop()

	err := reconciler.ensureCronJob(chain)
	require.NoError(t, err)

	_, ok := reconciler.cronJobs.Load("paused-chain")
	require.False(t, ok)
}

func TestReconcileFailsOnRestartError(t *testing.T) {
	t.Parallel()

	// No deployment exists in the clientset, so RestartResource will fail
	chain := &kairosv1alpha1.RestartChain{
		ObjectMeta: metav1.ObjectMeta{Name: "fail-chain"},
		Spec: kairosv1alpha1.RestartChainSpec{
			CronPattern: "0 2 * * *",
			Steps: []kairosv1alpha1.RestartChainStep{
				{
					ResourceRef: kairosv1alpha1.ResourceRef{Kind: "Deployment", Name: "nonexistent", Namespace: "default"},
					HealthCheck: kairosv1alpha1.HealthCheck{Strategy: kairosv1alpha1.HealthCheckRolloutComplete, TimeoutSeconds: 300},
				},
			},
		},
		Status: kairosv1alpha1.RestartChainStatus{
			Phase:       kairosv1alpha1.ChainPhaseRunning,
			CurrentStep: 0,
		},
	}

	reconciler, fakeClient := newTestChainReconciler(t, []ctrlclient.Object{chain})
	reconciler.cron.StartAsync()
	defer reconciler.cron.Stop()

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: chainKey("fail-chain")})
	require.NoError(t, err)

	var updated kairosv1alpha1.RestartChain
	err = fakeClient.Get(context.Background(), chainKey("fail-chain"), &updated)
	require.NoError(t, err)
	require.Equal(t, kairosv1alpha1.ChainPhaseFailed, updated.Status.Phase)
}
