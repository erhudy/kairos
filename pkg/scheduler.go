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

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	return &Scheduler{
		logger:         logger,
		workchan:       workchan,
		cron:           scheduler,
		clientset:      clientset,
		resourceMap:    &sync.Map{},
		metrics:        metrics,
		maxJitter:      maxJitter,
		lookback:       lookback,
		timezone:       timezone,
		startTime:      time.Now(),
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
	}
}

func (s *Scheduler) Run(stopCh chan struct{}) {
	s.cron.StartAsync()

	for {
		select {
		case <-stopCh:
			s.logger.Info("stopping scheduler")
			// wake any goroutines sleeping in a jitter delay so Stop does not block on them
			s.shutdownCancel()
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
		lastRunStr := getPodTemplateAnnotation(entry.obj, CRON_LAST_RESTARTED_AT_KEY)
		for cp, job := range entry.jobs {
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
	var err error
	switch action.action {
	case RESOURCE_DELETE:
		err = s.deleteJobsForResource(action.obj)
		if err != nil {
			s.logger.Error("error removing job from scheduler", zap.Error(err))
		}
	case RESOURCE_CHANGE:
		err = s.reconcileJobsForResource(action.obj)
		if err != nil {
			s.logger.Error("error reconciling jobs", zap.Error(err))
		}
	}
	// ack the action so the controller can retry failures via its workqueue;
	// errCh is buffered (size 1), so this never blocks even if the sender gave up
	if action.errCh != nil {
		action.errCh <- err
	}
}

func (s *Scheduler) reconcileJobsForResource(obj runtime.Object) error {
	objm, objk := getObjectMetaAndKind(obj)
	ri := getResourceIdentifier(objm, objk)

	s.logger.Info("reconciling jobs for resource", zap.String("resource", string(ri)))

	// load the cron patterns on the job
	pattern := getCronPatternString(objm)
	if pattern == "" {
		s.logger.Debug("cron expression was empty, removing any registered jobs", zap.String("resource", string(ri)))
		return s.deleteJobsForResource(obj)
	}

	splitPatternsRaw := strings.Split(strings.TrimSpace(strings.TrimSuffix(string(pattern), ";")), ";")
	cronPatternsFromResource := []cronPattern{}
	for _, p := range splitPatternsRaw {
		cronPatternsFromResource = append(cronPatternsFromResource, cronPattern(strings.TrimSpace(p)))
	}

	// build a comparison list against the keys in the resource map for this resource to figure out what to add/delete/ignore
	cronPatternsFromMap := cronPatterns{}
	entry := s.getOrCreateEntry(ri, obj)
	entry.Lock()
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
	if len(patternsToAdd) > 0 {
		s.checkMissedRestart(patternsToAdd, obj)
	}
	for _, p := range patternsToDelete {
		entry.RLock()
		job := entry.jobs[p]
		entry.RUnlock()
		err := s.deleteJob(p, ri, job, obj)
		if err != nil {
			return fmt.Errorf("error while deleting job during reconcile: %w", err)
		}
	}

	return nil
}

// getOrCreateEntry returns the resource map entry for ri, creating and storing
// a fresh one (and bumping the tracked-resources gauge) if none exists yet.
func (s *Scheduler) getOrCreateEntry(ri resourceIdentifier, obj runtime.Object) *resourceMapEntry {
	registeredJobsForResourceRaw, ok := s.resourceMap.Load(ri)
	if !ok {
		entry := &resourceMapEntry{
			obj:         obj,
			jobs:        make(map[cronPattern]*gocron.Job),
			lastJitters: make(map[cronPattern]time.Duration),
		}
		s.resourceMap.Store(ri, entry)
		if s.metrics != nil {
			s.metrics.TrackedResources.WithLabelValues(kindFromObject(obj)).Inc()
		}
		return entry
	}
	return registeredJobsForResourceRaw.(*resourceMapEntry)
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

	l := len(strings.Fields(cpString))
	switch l {
	case expectedCountForCron:
		cronFunc = s.cron.Cron
	case expectedCountForCronWithSeconds:
		cronFunc = s.cron.CronWithSeconds
	default:
		return fmt.Errorf("got %d fields in cron expression '%s', expected %d or %d", l, cp, expectedCountForCron, expectedCountForCronWithSeconds)
	}

	tag := fmt.Sprintf("%s--%s", ri, cp)

	var err error

	// parsed once here so each firing can clamp jitter against its actual next interval
	schedule, _, err := parseCronExpression(cp, s.timezone)
	if err != nil {
		s.logger.Warn("could not parse cron pattern for jitter clamping, using unclamped jitter", zap.String("cron-pattern", string(cp)), zap.Error(err))
		schedule = nil
	}

	scheduler := cronFunc(cpString)
	job, err = scheduler.Tag(string(tag)).Do(func() {
		if s.maxJitter > 0 {
			if !s.sleepWithJitter(cp, ri, schedule) {
				return
			}
			// the job may have been deleted while we slept; do not restart if so
			if !s.jobStillRegistered(cp, ri) {
				s.logger.Info("job removed during jitter sleep, skipping restart", zap.String("resource", string(ri)), zap.String("cron-pattern", string(cp)))
				return
			}
		}
		restartFunc(ctx, s.logger, s.clientset, obj, s.metrics)
	})
	if err != nil {
		return fmt.Errorf("error in createJob during creation: %w", err)
	}

	registeredEntry := s.getOrCreateEntry(ri, obj)
	registeredEntry.Lock()
	registeredEntry.jobs[cp] = job
	registeredEntry.Unlock()

	if s.metrics != nil {
		s.metrics.ScheduledJobs.WithLabelValues(kindFromObject(obj)).Inc()
	}

	return nil
}

// sleepWithJitter sleeps for a random duration up to maxJitter (clamped to half the
// time until the schedule's next firing) and records it for the status page.
// Returns false if the scheduler shut down during the sleep.
func (s *Scheduler) sleepWithJitter(cp cronPattern, ri resourceIdentifier, schedule cron.Schedule) bool {
	maxJitter := clampJitterToSchedule(s.maxJitter, schedule, time.Now())
	if maxJitter < s.maxJitter {
		s.logger.Info("jitter clamped to 50% of time until next firing",
			zap.String("resource", string(ri)),
			zap.String("cron-pattern", string(cp)),
			zap.Duration("requested", s.maxJitter),
			zap.Duration("effective", maxJitter),
		)
	}
	if maxJitter <= 0 {
		return true
	}
	jitter := time.Duration(rand.Int64N(int64(maxJitter)))
	s.logger.Info("applying jitter before restart", zap.String("resource", string(ri)), zap.String("cron-pattern", string(cp)), zap.Duration("jitter", jitter))
	if entryRaw, ok := s.resourceMap.Load(ri); ok {
		entry := entryRaw.(*resourceMapEntry)
		entry.Lock()
		entry.lastJitters[cp] = jitter
		entry.Unlock()
	}
	select {
	case <-time.After(jitter):
		return true
	case <-s.shutdownCtx.Done():
		return false
	}
}

// jobStillRegistered reports whether the job for cp is still tracked for ri.
func (s *Scheduler) jobStillRegistered(cp cronPattern, ri resourceIdentifier) bool {
	entryRaw, ok := s.resourceMap.Load(ri)
	if !ok {
		return false
	}
	entry := entryRaw.(*resourceMapEntry)
	entry.RLock()
	defer entry.RUnlock()
	_, ok = entry.jobs[cp]
	return ok
}

func (s *Scheduler) deleteJobsForResource(obj runtime.Object) error {
	objm, objk := getObjectMetaAndKind(obj)
	ri := getResourceIdentifier(objm, objk)

	registeredJobsForResourceRaw, ok := s.resourceMap.Load(ri)
	if !ok {
		s.logger.Debug("resource not found in resource map, nothing to delete", zap.String("resource", string(ri)))
		return nil
	}
	entry := registeredJobsForResourceRaw.(*resourceMapEntry)

	s.logger.Info("deleting jobs for resource", zap.String("resource", string(ri)))

	entry.RLock()
	jobsToDelete := make(map[cronPattern]*gocron.Job)
	for cp, job := range entry.jobs {
		jobsToDelete[cp] = job
	}
	entry.RUnlock()

	var errs []error
	for cronPattern, job := range jobsToDelete {
		err := s.deleteJob(cronPattern, ri, job, obj)
		if err != nil {
			s.logger.Error("error deleting job for resource", zap.String("resource", string(ri)), zap.String("cron-pattern", string(cronPattern)), zap.Error(err))
			errs = append(errs, fmt.Errorf("deleting job %s: %w", cronPattern, err))
		}
	}

	// remove the entry and decrement the gauge even if some deletions failed,
	// so a partial failure does not wedge the resource against future re-adds
	s.resourceMap.Delete(ri)
	if s.metrics != nil {
		s.metrics.TrackedResources.WithLabelValues(kindFromObject(obj)).Dec()
	}
	return errors.Join(errs...)
}

func (s *Scheduler) deleteJob(cp cronPattern, ri resourceIdentifier, job *gocron.Job, obj runtime.Object) error {
	notFound := false
	err := s.cron.RemoveByID(job)
	if err != nil && !errors.Is(err, gocron.ErrJobNotFound) {
		return fmt.Errorf("error in deleteJob: %w", err)
	} else if err != nil {
		notFound = true
	}
	// A "not found" result means the desired end state is already reached,
	// so map/gauge cleanup happens regardless of whether removal succeeded.
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
		zap.Bool("job-not-found", notFound),
	)
	if s.metrics != nil {
		s.metrics.ScheduledJobs.WithLabelValues(kindFromObject(obj)).Dec()
	}
	return nil
}

// clampJitterToSchedule returns maxJitter clamped to 50% of the time remaining until
// the schedule's next firing after now, so a jitter sleep can never overshoot the
// following firing. Falls back to maxJitter unchanged if the schedule is unavailable.
func clampJitterToSchedule(maxJitter time.Duration, schedule cron.Schedule, now time.Time) time.Duration {
	if maxJitter <= 0 || schedule == nil {
		return maxJitter
	}
	next := schedule.Next(now)
	if next.IsZero() {
		return maxJitter
	}
	half := next.Sub(now) / 2
	if maxJitter > half {
		return half
	}
	return maxJitter
}

// cronExpressionParser accepts both 5-field (standard) and 6-field (with seconds)
// expressions, matching what gocron accepts via Cron/CronWithSeconds.
var cronExpressionParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// parseCronExpression parses a cron pattern string (with optional TZ= or CRON_TZ= prefix)
// into a robfig/cron Schedule and the effective timezone location. The returned schedule
// evaluates in that location, mirroring how gocron fires the job (gocron prefixes the
// expression with CRON_TZ=<scheduler location> when no TZ prefix is present).
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

	schedule, err := cronExpressionParser.Parse(cpString)
	if err != nil {
		return nil, nil, fmt.Errorf("error parsing cron expression %q: %w", cpString, err)
	}
	// the parser defaults to time.Local since the TZ prefix was stripped above;
	// pin the schedule to the effective location instead
	if specSchedule, ok := schedule.(*cron.SpecSchedule); ok {
		specSchedule.Location = loc
	}
	return schedule, loc, nil
}

// findLastScheduledTimeInWindow returns the most recent scheduled time in [windowStart, now).
func findLastScheduledTimeInWindow(schedule cron.Schedule, windowStart, now time.Time) (time.Time, bool) {
	t := windowStart.Add(-time.Nanosecond)
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

func (s *Scheduler) checkMissedRestart(cps []cronPattern, obj runtime.Object) {
	s.checkMissedRestartAt(cps, obj, time.Now())
}

// checkMissedRestartAt triggers at most one catch-up restart for the resource if any of its
// cron patterns had a firing within the lookback window relative to now that was missed
// because kairos was not running at the time. Firings after the scheduler started are
// handled by the regular jobs and never treated as missed.
func (s *Scheduler) checkMissedRestartAt(cps []cronPattern, obj runtime.Object, now time.Time) {
	if s.lookback == 0 {
		return
	}

	windowStart := now.Add(-s.lookback)

	// find the most recent missed firing across all of the resource's patterns
	var lastScheduled time.Time
	var missedPattern cronPattern
	var missedSchedule cron.Schedule
	for _, cp := range cps {
		schedule, _, err := parseCronExpression(cp, s.timezone)
		if err != nil {
			s.logger.Error("missed restart check: error parsing cron pattern", zap.String("pattern", string(cp)), zap.Error(err))
			continue
		}

		scheduled, found := findLastScheduledTimeInWindow(schedule, windowStart, now)
		if !found || !scheduled.Before(s.startTime) {
			continue
		}
		if scheduled.After(lastScheduled) {
			lastScheduled = scheduled
			missedPattern = cp
			missedSchedule = schedule
		}
	}
	if lastScheduled.IsZero() {
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
		zap.String("cron-pattern", string(missedPattern)),
		zap.Time("lastScheduled", lastScheduled),
	)

	go func() {
		if s.maxJitter > 0 {
			if !s.sleepWithJitter(missedPattern, ri, missedSchedule) {
				return
			}
		}
		restartFunc(context.Background(), s.logger, s.clientset, obj, s.metrics)
	}()
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
