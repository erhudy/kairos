package pkg

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-co-op/gocron"
	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
)

func NewScheduler(timezone *time.Location, logger *zap.Logger, workchan <-chan ObjectAndSchedulerAction, clientset kubernetes.Interface, metrics *KairosMetrics, maxJitter time.Duration, lookback time.Duration) *Scheduler {
	scheduler := gocron.NewScheduler(timezone)
	scheduler.TagsUnique()

	return &Scheduler{
		logger:      logger,
		workchan:    workchan,
		cron:        scheduler,
		clientset:   clientset,
		resourceMap: &sync.Map{},
		metrics:     metrics,
		maxJitter:   maxJitter,
		lookback:    lookback,
		timezone:    timezone,
	}
}

func (s *Scheduler) Run(stopCh chan struct{}) {
	s.cron.StartAsync()

	for {
		select {
		case <-stopCh:
			s.logger.Info("stopping scheduler")
			s.cron.Stop()
			return
		case i := <-s.workchan:
			s.processSchedulerBundle(i)
		}
	}
}

type jobStatusEntry struct {
	Resource    string `json:"resource"`
	CronPattern string `json:"cronPattern"`
	LastRun     string `json:"lastRun"`
	NextRun     string `json:"nextRun"`
	LastJitter  string `json:"lastJitter"`
}

type configStatus struct {
	Timezone string `json:"timezone"`
	Jitter   string `json:"jitter"`
	Lookback string `json:"lookback"`
}

func (s *Scheduler) ConfigJSON(w http.ResponseWriter, r *http.Request) {
	jitter := "disabled"
	if s.maxJitter > 0 {
		jitter = s.maxJitter.String()
	}
	lookback := "disabled"
	if s.lookback > 0 {
		lookback = s.lookback.String()
	}
	cfg := configStatus{
		Timezone: s.timezone.String(),
		Jitter:   jitter,
		Lookback: lookback,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cfg); err != nil {
		s.logger.Error("error encoding config JSON", zap.Error(err))
	}
}

func (s *Scheduler) JobStatusJSON(w http.ResponseWriter, r *http.Request) {
	var entries []jobStatusEntry

	s.resourceMap.Range(func(key, value any) bool {
		ri := key.(resourceIdentifier)
		entry := value.(*resourceMapEntry)
		entry.RLock()
		defer entry.RUnlock()
		for cp, job := range entry.jobs {
			lastRunStr := getPodTemplateAnnotation(entry.obj, CRON_LAST_RESTARTED_AT_KEY)
			lastJitterStr := ""
			if j, ok := entry.lastJitters[cp]; ok && j > 0 {
				lastJitterStr = j.Round(time.Millisecond).String()
			}
			entries = append(entries, jobStatusEntry{
				Resource:    string(ri),
				CronPattern: string(cp),
				LastRun:     lastRunStr,
				NextRun:     job.NextRun().UTC().Format(time.RFC3339),
				LastJitter:  lastJitterStr,
			})
		}
		return true
	})

	if entries == nil {
		entries = []jobStatusEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		s.logger.Error("error encoding job status JSON", zap.Error(err))
	}
}

//go:embed job_status.html
var jobStatusPageHTML string

func (s *Scheduler) JobStatusPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if _, err := w.Write([]byte(jobStatusPageHTML)); err != nil {
		s.logger.Error("error writing job status page", zap.Error(err))
	}
}

func (s *Scheduler) processSchedulerBundle(action ObjectAndSchedulerAction) {
	// action here refers to what is happening to the owning Deployment/DaemonSet/StatefulSet, not what is happening with the cron jobs
	switch action.action {
	case RESOURCE_DELETE:
		err := s.deleteJobsForResource(action.obj)
		if err != nil {
			s.logger.Error("error removing job from scheduler", zap.Error(err))
		}
	case RESOURCE_CHANGE:
		err := s.reconcileJobsForResource(action.obj)
		if err != nil {
			s.logger.Error("error reconciling jobs", zap.Error(err))
		}
	}
}

func (s *Scheduler) reconcileJobsForResource(obj runtime.Object) error {
	objm, objk := getObjectMetaAndKind(obj)
	ri := getResourceIdentifier(objm, objk)

	s.logger.Info("reconciling jobs for resource", zap.String("resource", string(ri)))

	// load the cron patterns on the job
	pattern := getCronPatternString(objm)
	if pattern == "" {
		s.logger.Debug("cron expression was empty", zap.String("resource", string(ri)))
		return nil
	}

	splitPatternsRaw := strings.Split(strings.TrimSpace(strings.TrimSuffix(string(pattern), ";")), ";")
	cronPatternsFromResource := []cronPattern{}
	for _, p := range splitPatternsRaw {
		cronPatternsFromResource = append(cronPatternsFromResource, cronPattern(strings.TrimSpace(p)))
	}

	// build a comparison list against the keys in the resource map for this resource to figure out what to add/delete/ignore
	cronPatternsFromMap := cronPatterns{}
	registeredJobsForResourceRaw, ok := s.resourceMap.Load(ri)
	if !ok {
		entry := &resourceMapEntry{
			obj:         obj,
			jobs:        make(map[cronPattern]*gocron.Job),
			lastJitters: make(map[cronPattern]time.Duration),
		}
		s.resourceMap.Store(ri, entry)
		registeredJobsForResourceRaw = entry
	}
	entry := registeredJobsForResourceRaw.(*resourceMapEntry)
	entry.Lock()
	if entry.lastJitters == nil {
		entry.lastJitters = make(map[cronPattern]time.Duration)
	}
	entry.obj = obj
	for pattern := range entry.jobs {
		cronPatternsFromMap = append(cronPatternsFromMap, pattern)
	}
	entry.Unlock()

	// strings and not cronPatterns
	patternsToAdd := []cronPattern{}
	patternsToDelete := []cronPattern{}
	patternsThatDidNotChangeMap := make(map[cronPattern]struct{})

	s.logger.Debug("patterns already registered", zap.String("resource", string(ri)), zap.Stringers("patterns", cronPatternsFromMap))

	// if the pattern is in our map, but is not on the resource, it has been removed and so we delete the restart job
	for _, i := range cronPatternsFromMap {
		if slices.Contains(cronPatternsFromResource, i) {
			patternsThatDidNotChangeMap[i] = struct{}{}
		} else {
			if i.String() != "" {
				patternsToDelete = append(patternsToDelete, i)
			}
		}
	}

	// if the pattern is on the resource, but is not in our map, it is a new pattern and so we need to make a restart job
	for _, i := range cronPatternsFromResource {
		if slices.Contains(cronPatternsFromMap, i) {
			patternsThatDidNotChangeMap[i] = struct{}{}
		} else {
			if i.String() != "" {
				patternsToAdd = append(patternsToAdd, i)
			}
		}
	}

	patternsThatDidNotChange := cronPatterns{}
	for k := range patternsThatDidNotChangeMap {
		patternsThatDidNotChange = append(patternsThatDidNotChange, k)
	}

	if len(patternsToAdd) > 0 {
		s.logger.Debug("patterns to add", zap.Stringers("patterns", patternsToAdd))
	}
	if len(patternsToDelete) > 0 {
		s.logger.Debug("patterns to delete", zap.Stringers("patterns", patternsToDelete))
	}
	if len(patternsThatDidNotChange) > 0 {
		s.logger.Debug("patterns that did not change", zap.Stringers("patterns", patternsThatDidNotChange))
	}

	for _, p := range patternsToAdd {
		err := s.createJob(p, ri, obj)
		if err != nil {
			return fmt.Errorf("error while adding job during reconcile: %w", err)
		}
	}
	for _, p := range patternsToDelete {
		job := entry.jobs[p]
		err := s.deleteJob(p, ri, job, obj)
		if err != nil {
			return fmt.Errorf("error while deleting job during reconcile: %w", err)
		}
	}

	return nil
}

// creates/updates the job (by deleting/recreating) and returns it for inspection
func (s *Scheduler) createJob(cp cronPattern, ri resourceIdentifier, obj runtime.Object) error {
	ctx := context.Background()

	cpString := string(cp)

	var job *gocron.Job

	// if 5 fields, regular cron, if 6 fields, cron with seconds, otherwise freak out
	var cronFunc func(string) *gocron.Scheduler

	s.logger.Debug("working on cp", zap.String("cp", cp.String()))

	expectedCountForCron := 5
	expectedCountForCronWithSeconds := 6
	// if TZ/CRON_TZ specification is present, expect an extra field when we naively split string

	if strings.HasPrefix(cpString, "TZ=") || strings.HasPrefix(cpString, "CRON_TZ=") {
		expectedCountForCron += 1
		expectedCountForCronWithSeconds += 1
	}

	l := len(strings.Split(cpString, " "))
	switch l {
	case expectedCountForCron:
		cronFunc = s.cron.Cron
	case expectedCountForCronWithSeconds:
		cronFunc = s.cron.CronWithSeconds
	default:
		return fmt.Errorf("got %d fields splitting cron expression '%s', expected 5 or 6", l, cp)
	}

	tag := fmt.Sprintf("%s--%s", ri, cp)

	var err error

	maxJitter := clampedMaxJitter(cp, s.maxJitter, s.timezone)
	if s.maxJitter > 0 && maxJitter < s.maxJitter {
		s.logger.Info("jitter clamped to 50% of schedule interval",
			zap.String("resource", string(ri)),
			zap.String("cron-pattern", string(cp)),
			zap.Duration("requested", s.maxJitter),
			zap.Duration("effective", maxJitter),
		)
	}
	logger := s.logger
	clientset := s.clientset
	metrics := s.metrics
	resourceMap := s.resourceMap

	scheduler := cronFunc(cpString)
	job, err = scheduler.Tag(string(tag)).Do(func() {
		if maxJitter > 0 {
			jitter := time.Duration(rand.Int64N(int64(maxJitter)))
			logger.Info("applying jitter before restart", zap.String("resource", string(ri)), zap.String("cron-pattern", string(cp)), zap.Duration("jitter", jitter))
			if entryRaw, ok := resourceMap.Load(ri); ok {
				entry := entryRaw.(*resourceMapEntry)
				entry.Lock()
				entry.lastJitters[cp] = jitter
				entry.Unlock()
			}
			time.Sleep(jitter)
		}
		restartFunc(ctx, logger, clientset, obj, metrics)
	})
	if err != nil {
		return fmt.Errorf("error in createJob during creation: %w", err)
	}

	registeredJobsForResourceRaw, ok := s.resourceMap.Load(ri)
	if !ok {
		entry := &resourceMapEntry{
			obj:         obj,
			jobs:        make(map[cronPattern]*gocron.Job),
			lastJitters: make(map[cronPattern]time.Duration),
		}
		s.resourceMap.Store(ri, entry)
		registeredJobsForResourceRaw = entry
		if s.metrics != nil {
			s.metrics.TrackedResources.WithLabelValues(kindFromObject(obj)).Inc()
		}
	}
	registeredEntry := registeredJobsForResourceRaw.(*resourceMapEntry)
	registeredEntry.Lock()
	registeredEntry.jobs[cp] = job
	registeredEntry.Unlock()


	if s.metrics != nil {
		s.metrics.ScheduledJobs.WithLabelValues(kindFromObject(obj)).Inc()
	}

	s.checkMissedRestart(cp, obj)

	return nil
}

func (s *Scheduler) deleteJobsForResource(obj runtime.Object) error {
	objm, objk := getObjectMetaAndKind(obj)
	ri := getResourceIdentifier(objm, objk)

	s.logger.Info("deleting jobs for resource", zap.String("resource", string(ri)))

	registeredJobsForResourceRaw, ok := s.resourceMap.Load(ri)
	if !ok {
		return fmt.Errorf("resource %s not found in resource map", ri)
	}
	entry := registeredJobsForResourceRaw.(*resourceMapEntry)

	entry.RLock()
	jobsToDelete := make(map[cronPattern]*gocron.Job)
	for cp, job := range entry.jobs {
		jobsToDelete[cp] = job
	}
	entry.RUnlock()

	for cronPattern, job := range jobsToDelete {
		err := s.deleteJob(cronPattern, ri, job, obj)
		if err != nil {
			return err
		}
	}

	s.resourceMap.Delete(ri)
	if s.metrics != nil {
		s.metrics.TrackedResources.WithLabelValues(kindFromObject(obj)).Dec()
	}
	return nil
}

func (s *Scheduler) deleteJob(cp cronPattern, ri resourceIdentifier, job *gocron.Job, obj runtime.Object) error {
	err := s.cron.RemoveByID(job)
	if err != nil {
		if !errors.Is(err, gocron.ErrJobNotFound) {
			return fmt.Errorf("error in deleteJob: %w", err)
		}
	} else {
		registeredJobsForResourceRaw, ok := s.resourceMap.Load(ri)
		if !ok {
			return fmt.Errorf("resource %s not found in resource map", ri)
		}
		entry := registeredJobsForResourceRaw.(*resourceMapEntry)
		entry.Lock()
		delete(entry.jobs, cp)
		delete(entry.lastJitters, cp)
		entry.Unlock()
		s.logger.Info(
			"deleted job",
			zap.String("resource", string(ri)),
			zap.String("cron-pattern", string(cp)),
		)
		if s.metrics != nil {
			s.metrics.ScheduledJobs.WithLabelValues(kindFromObject(obj)).Dec()
		}
	}
	return nil
}

// clampedMaxJitter returns maxJitter clamped to 50% of the schedule interval for cp.
// Falls back to maxJitter unchanged if the interval cannot be determined.
func clampedMaxJitter(cp cronPattern, maxJitter time.Duration, timezone *time.Location) time.Duration {
	if maxJitter == 0 {
		return 0
	}
	schedule, _, err := parseCronExpression(cp, timezone)
	if err != nil {
		return maxJitter
	}
	now := time.Now()
	next1 := schedule.Next(now)
	if next1.IsZero() {
		return maxJitter
	}
	next2 := schedule.Next(next1)
	if next2.IsZero() {
		return maxJitter
	}
	half := next2.Sub(next1) / 2
	if maxJitter > half {
		return half
	}
	return maxJitter
}

// parseCronExpression parses a cron pattern string (with optional TZ= or CRON_TZ= prefix)
// into a robfig/cron Schedule and the effective timezone location.
func parseCronExpression(cp cronPattern, defaultLoc *time.Location) (cron.Schedule, *time.Location, error) {
	cpString := string(cp)
	loc := defaultLoc

	if strings.HasPrefix(cpString, "TZ=") || strings.HasPrefix(cpString, "CRON_TZ=") {
		eqIdx := strings.Index(cpString, "=")
		rest := cpString[eqIdx+1:]
		spaceIdx := strings.Index(rest, " ")
		if spaceIdx < 0 {
			return nil, nil, fmt.Errorf("invalid TZ-prefixed pattern: %s", cp)
		}
		tzName := rest[:spaceIdx]
		cpString = rest[spaceIdx+1:]
		var err error
		loc, err = time.LoadLocation(tzName)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid timezone %q in pattern %s: %w", tzName, cp, err)
		}
	}

	fields := strings.Fields(cpString)
	var parser cron.Parser
	switch len(fields) {
	case 5:
		parser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	case 6:
		parser = cron.NewParser(cron.Second | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	default:
		return nil, nil, fmt.Errorf("invalid cron expression field count %d in %q", len(fields), cpString)
	}

	schedule, err := parser.Parse(cpString)
	if err != nil {
		return nil, nil, fmt.Errorf("error parsing cron expression %q: %w", cpString, err)
	}
	return schedule, loc, nil
}

// findLastScheduledTimeInWindow returns the most recent scheduled time in [windowStart, now).
func findLastScheduledTimeInWindow(schedule cron.Schedule, loc *time.Location, windowStart, now time.Time) (time.Time, bool) {
	t := windowStart.In(loc).Add(-time.Nanosecond)
	var lastTime time.Time
	for {
		next := schedule.Next(t)
		if next.IsZero() || !next.Before(now) {
			break
		}
		lastTime = next
		t = next
	}
	if lastTime.IsZero() {
		return time.Time{}, false
	}
	return lastTime, true
}

func (s *Scheduler) checkMissedRestart(cp cronPattern, obj runtime.Object) {
	s.checkMissedRestartAt(cp, obj, time.Now())
}

// checkMissedRestartAt triggers an immediate catch-up restart if the resource missed a scheduled
// restart within the lookback window relative to now.
func (s *Scheduler) checkMissedRestartAt(cp cronPattern, obj runtime.Object, now time.Time) {
	if s.lookback == 0 {
		return
	}

	windowStart := now.Add(-s.lookback)

	schedule, loc, err := parseCronExpression(cp, s.timezone)
	if err != nil {
		s.logger.Error("missed restart check: error parsing cron pattern", zap.String("pattern", string(cp)), zap.Error(err))
		return
	}

	lastScheduled, found := findLastScheduledTimeInWindow(schedule, loc, windowStart, now)
	if !found {
		return
	}

	lastRestartedStr := getPodTemplateAnnotation(obj, CRON_LAST_RESTARTED_AT_KEY)
	if lastRestartedStr != "" {
		lastRestarted, err := time.Parse(LAST_RESTARTED_AT_TIME_FORMAT, lastRestartedStr)
		if err == nil && !lastRestarted.Before(lastScheduled) {
			return
		}
	}

	om, objk := getObjectMetaAndKind(obj)
	ri := getResourceIdentifier(om, objk)
	s.logger.Info("missed restart detected, triggering catch-up restart",
		zap.String("resource", string(ri)),
		zap.String("cron-pattern", string(cp)),
		zap.Time("lastScheduled", lastScheduled),
	)

	ctx := context.Background()
	go restartFunc(ctx, s.logger, s.clientset, obj, s.metrics)
}

func restartFunc(ctx context.Context, logger *zap.Logger, clientset kubernetes.Interface, incomingObject runtime.Object, metrics *KairosMetrics) {
	logger.Debug("entering restartFunc")

	om, _ := getObjectMetaAndKind(incomingObject)
	namespace := om.GetNamespace()
	name := om.GetName()
	kind := kindFromObject(incomingObject)

	logger.Info("firing restartFunc", zap.Time("time", time.Now()), zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name))

	start := time.Now()
	now := start.Format(LAST_RESTARTED_AT_TIME_FORMAT)
	var err error

	const maxRetries = 5

	switch incomingObject.(type) {
	case *appsv1.Deployment:
		for range maxRetries {
			var obj *appsv1.Deployment
			obj, err = clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				logger.Error("error getting object in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name), zap.Error(err))
				if metrics != nil {
					metrics.RestartErrorsTotal.WithLabelValues(kind, namespace, name, "get").Inc()
				}
				return
			}
			if obj.Spec.Template.Annotations == nil {
				obj.Spec.Template.Annotations = make(map[string]string)
			}
			obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY] = now
			_, err = clientset.AppsV1().Deployments(namespace).Update(ctx, obj, metav1.UpdateOptions{})
			if err == nil || !k8serrors.IsConflict(err) {
				break
			}
			logger.Warn("conflict updating object in restartFunc, retrying", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name))
		}
	case *appsv1.DaemonSet:
		for range maxRetries {
			var obj *appsv1.DaemonSet
			obj, err = clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				logger.Error("error getting object in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name), zap.Error(err))
				if metrics != nil {
					metrics.RestartErrorsTotal.WithLabelValues(kind, namespace, name, "get").Inc()
				}
				return
			}
			if obj.Spec.Template.Annotations == nil {
				obj.Spec.Template.Annotations = make(map[string]string)
			}
			obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY] = now
			_, err = clientset.AppsV1().DaemonSets(namespace).Update(ctx, obj, metav1.UpdateOptions{})
			if err == nil || !k8serrors.IsConflict(err) {
				break
			}
			logger.Warn("conflict updating object in restartFunc, retrying", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name))
		}
	case *appsv1.StatefulSet:
		for range maxRetries {
			var obj *appsv1.StatefulSet
			obj, err = clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				logger.Error("error getting object in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name), zap.Error(err))
				if metrics != nil {
					metrics.RestartErrorsTotal.WithLabelValues(kind, namespace, name, "get").Inc()
				}
				return
			}
			if obj.Spec.Template.Annotations == nil {
				obj.Spec.Template.Annotations = make(map[string]string)
			}
			obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY] = now
			_, err = clientset.AppsV1().StatefulSets(namespace).Update(ctx, obj, metav1.UpdateOptions{})
			if err == nil || !k8serrors.IsConflict(err) {
				break
			}
			logger.Warn("conflict updating object in restartFunc, retrying", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name))
		}
	default:
		logger.Error("unsupported type in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)))
		return
	}

	if metrics != nil {
		metrics.RestartDuration.WithLabelValues(kind, namespace, name).Observe(time.Since(start).Seconds())
	}

	if err != nil {
		logger.Error("error updating object in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name), zap.Error(err))
		if metrics != nil {
			metrics.RestartErrorsTotal.WithLabelValues(kind, namespace, name, "update").Inc()
		}
	} else {
		if metrics != nil {
			metrics.RestartTotal.WithLabelValues(kind, namespace, name).Inc()
		}
	}
}
