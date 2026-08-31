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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

func NewScheduler(timezone *time.Location, logger *zap.Logger, workchan <-chan ObjectAndSchedulerAction, clientset kubernetes.Interface, metrics *KairosMetrics, maxJitter time.Duration, lookback time.Duration, chainTimeout time.Duration) *Scheduler {
	scheduler := gocron.NewScheduler(timezone)
	scheduler.TagsUnique()

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	return &Scheduler{
		logger:            logger,
		workchan:          workchan,
		cron:              scheduler,
		clientset:         clientset,
		resourceMap:       &sync.Map{},
		metrics:           metrics,
		maxJitter:         maxJitter,
		lookback:          lookback,
		timezone:          timezone,
		chainTimeout:      chainTimeout,
		chainPollInterval: CHAIN_POLL_INTERVAL,
		chainMap:          &sync.Map{},
		pendingSteps:      &sync.Map{},
		startTime:         time.Now(),
		shutdownCtx:       shutdownCtx,
		shutdownCancel:    shutdownCancel,
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
	Resource         string `json:"resource"`
	CronPattern      string `json:"cronPattern"`
	LastRun          string `json:"lastRun"`
	NextRun          string `json:"nextRun"`
	LastJitter       string `json:"lastJitter"`
	RestartAfter     string `json:"restartAfter,omitempty"`
	RestartAfterMode string `json:"restartAfterMode,omitempty"`
	RestartAfterWait string `json:"restartAfterWait,omitempty"`
}

// chainAnnotationsFrom returns the display strings for the restart-after annotations
// on obj, or empty strings when it has none.
func chainAnnotationsFrom(obj runtime.Object) (after, mode, wait string) {
	om, _ := getObjectMetaAndKind(obj)
	anns := om.GetAnnotations()
	after = strings.TrimSpace(anns[RESTART_AFTER_KEY])
	if after == "" {
		return "", "", ""
	}
	switch strings.TrimSpace(anns[RESTART_AFTER_MODE_KEY]) {
	case CHAIN_MODE_HEALTH_PLUS_WAIT:
		mode = CHAIN_MODE_DISPLAY_PLUS_WAIT
	default:
		mode = CHAIN_MODE_DISPLAY_HEALTH
	}
	wait = strings.TrimSpace(anns[RESTART_AFTER_WAIT_KEY])
	return after, mode, wait
}

type configStatus struct {
	Timezone     string `json:"timezone"`
	Jitter       string `json:"jitter"`
	Lookback     string `json:"lookback"`
	ChainTimeout string `json:"chainTimeout"`
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
		Timezone:     s.timezone.String(),
		Jitter:       jitter,
		Lookback:     lookback,
		ChainTimeout: s.chainTimeout.String(),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cfg); err != nil {
		s.logger.Error("error encoding config JSON", zap.Error(err))
	}
}

func (s *Scheduler) JobStatusJSON(w http.ResponseWriter, r *http.Request) {
	var entries []jobStatusEntry
	emittedRi := map[resourceIdentifier]struct{}{}

	s.resourceMap.Range(func(key, value any) bool {
		ri := key.(resourceIdentifier)
		entry := value.(*resourceMapEntry)
		entry.RLock()
		defer entry.RUnlock()
		lastRunStr := getPodTemplateAnnotation(entry.obj, CRON_LAST_RESTARTED_AT_KEY)
		after, mode, wait := chainAnnotationsFrom(entry.obj)
		for cp, job := range entry.jobs {
			lastJitterStr := ""
			if j, ok := entry.lastJitters[cp]; ok && j > 0 {
				lastJitterStr = j.Round(time.Millisecond).String()
			}
			entries = append(entries, jobStatusEntry{
				Resource:         string(ri),
				CronPattern:      string(cp),
				LastRun:          lastRunStr,
				NextRun:          job.NextRun().UTC().Format(time.RFC3339),
				LastJitter:       lastJitterStr,
				RestartAfter:     after,
				RestartAfterMode: mode,
				RestartAfterWait: wait,
			})
		}
		if len(entry.jobs) > 0 {
			emittedRi[ri] = struct{}{}
		}
		return true
	})

	// pure followers (restart-after with no cron jobs of their own) have no job
	// entries; walk the chain edges so they are still visible on the status page,
	// aggregated per follower across all of its predecessors
	chained := map[resourceIdentifier]*jobStatusEntry{}
	var chainedOrder []resourceIdentifier
	s.chainMap.Range(func(_, value any) bool {
		entry := value.(*chainMapEntry)
		entry.RLock()
		defer entry.RUnlock()
		for _, edge := range entry.edges {
			if _, ok := emittedRi[edge.followerRi]; ok {
				continue
			}
			existing, seen := chained[edge.followerRi]
			if !seen {
				chained[edge.followerRi] = &jobStatusEntry{
					Resource:         string(edge.followerRi),
					LastRun:          getPodTemplateAnnotation(edge.obj, CRON_LAST_RESTARTED_AT_KEY),
					RestartAfter:     edgeDisplay(edge),
					RestartAfterMode: chainModeDisplay(edge.mode),
					RestartAfterWait: chainWaitDisplay(edge),
				}
				chainedOrder = append(chainedOrder, edge.followerRi)
			} else {
				existing.RestartAfter += ", " + edgeDisplay(edge)
			}
		}
		return true
	})
	for _, ri := range chainedOrder {
		entries = append(entries, *chained[ri])
	}

	if entries == nil {
		entries = []jobStatusEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		s.logger.Error("error encoding job status JSON", zap.Error(err))
	}
}

func edgeDisplay(edge *chainEdge) string {
	return edge.predecessor.display
}

func chainModeDisplay(mode chainMode) string {
	if mode == chainModeHealthPlusWait {
		return CHAIN_MODE_DISPLAY_PLUS_WAIT
	}
	return CHAIN_MODE_DISPLAY_HEALTH
}

func chainWaitDisplay(edge *chainEdge) string {
	if edge.mode == chainModeHealthPlusWait && edge.wait > 0 {
		return edge.wait.String()
	}
	return ""
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
	if pattern == "" && !hasChainAnnotations(objm) {
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

	s.reconcileChainEdges(obj)

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
		s.fireRestart(obj)
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

	// a resource that is gone or no longer annotated must not remain in the chain
	// graph: neither as a firing source for others nor as a follower awaiting one
	s.chainMap.Delete(ri)
	s.removeChainEdgesForFollower(ri)

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
			// the job may have been deleted while we slept; do not restart if so
			if !s.jobStillRegistered(missedPattern, ri) {
				s.logger.Info("job removed during jitter sleep, skipping catch-up restart", zap.String("resource", string(ri)), zap.String("cron-pattern", string(missedPattern)))
				return
			}
		}
		s.fireRestart(obj)
	}()
}

// fireRestart patches the pod template annotation on obj and, when the patch
// succeeds, triggers any chained followers registered for it.
func (s *Scheduler) fireRestart(obj runtime.Object) bool {
	if !restartFunc(context.Background(), s.logger, s.clientset, obj, s.metrics) {
		return false
	}
	om, objk := getObjectMetaAndKind(obj)
	s.triggerFollowers(getResourceIdentifier(om, objk))
	return true
}

// triggerFollowers spawns one chain step per follower registered under predRi,
// deduped to a single in-flight step per follower.
func (s *Scheduler) triggerFollowers(predRi resourceIdentifier) {
	raw, ok := s.chainMap.Load(predRi)
	if !ok {
		return
	}
	entry := raw.(*chainMapEntry)
	entry.RLock()
	edges := make([]*chainEdge, 0, len(entry.edges))
	for _, edge := range entry.edges {
		edges = append(edges, edge)
	}
	entry.RUnlock()

	for _, edge := range edges {
		if _, loaded := s.pendingSteps.LoadOrStore(edge.followerRi, struct{}{}); loaded {
			s.logger.Debug("chain step already in flight for follower, skipping trigger",
				zap.String("predecessor", string(predRi)),
				zap.String("follower", string(edge.followerRi)),
			)
			continue
		}
		go s.runChainStep(predRi, edge)
	}
}

// runChainStep waits for the predecessor's restart to settle (its rollout to
// complete again, optionally plus a fixed wait) and then restarts the follower,
// which recursively triggers the follower's own followers.
func (s *Scheduler) runChainStep(predRi resourceIdentifier, edge *chainEdge) {
	defer s.pendingSteps.Delete(edge.followerRi)

	fKind, fNs, fName, ok := parseResourceIdentifier(edge.followerRi)
	if !ok {
		s.logger.Error("chain step has malformed follower identifier, aborting", zap.String("follower", string(edge.followerRi)))
		return
	}
	record := func(outcome string) {
		if s.metrics != nil {
			s.metrics.ChainStepsTotal.WithLabelValues(fKind, fNs, fName, outcome).Inc()
		}
	}
	abort := func(reason string) {
		s.logger.Info("chain step aborted",
			zap.String("predecessor", string(predRi)),
			zap.String("follower", string(edge.followerRi)),
			zap.String("reason", reason),
		)
		record(CHAIN_OUTCOME_ABORTED)
	}

	s.logger.Info("chain step waiting for predecessor to become healthy",
		zap.String("predecessor", string(predRi)),
		zap.String("follower", string(edge.followerRi)),
	)

	deadline := time.Now().Add(s.chainTimeout)
	for {
		select {
		case <-s.shutdownCtx.Done():
			abort("shutdown")
			return
		default:
		}
		if !s.edgeStillRegistered(predRi, edge.followerRi) {
			abort("edge removed")
			return
		}

		predObj, err := s.getWorkload(edge.predecessor)
		switch {
		case apierrors.IsNotFound(err):
			abort("predecessor deleted")
			return
		case err != nil:
			s.logger.Warn("error checking predecessor health, will retry",
				zap.String("predecessor", string(predRi)),
				zap.Error(err),
			)
		case isRolloutComplete(predObj):
			s.logger.Info("predecessor healthy again", zap.String("predecessor", string(predRi)))
			if edge.mode == chainModeHealthPlusWait {
				if !s.chainSettleWait(edge, predRi) {
					return
				}
			}
			freshObj, err := s.getWorkload(workloadRef{kind: fKind, namespace: fNs, name: fName})
			if err != nil {
				if apierrors.IsNotFound(err) {
					abort("follower deleted")
				} else {
					s.logger.Error("error fetching fresh follower object", zap.String("follower", string(edge.followerRi)), zap.Error(err))
					record(CHAIN_OUTCOME_ABORTED)
				}
				return
			}
			if restartFunc(context.Background(), s.logger, s.clientset, freshObj, s.metrics) {
				s.triggerFollowers(edge.followerRi)
				s.refreshChainEdgeObject(predRi, edge.followerRi)
				record(CHAIN_OUTCOME_COMPLETED)
			} else {
				abort("follower restart failed")
			}
			return
		}

		if !time.Now().Before(deadline) {
			s.logger.Error("chain step timed out waiting for predecessor to become healthy, aborting cascade",
				zap.String("predecessor", string(predRi)),
				zap.String("follower", string(edge.followerRi)),
				zap.Duration("timeout", s.chainTimeout),
			)
			record(CHAIN_OUTCOME_TIMEOUT)
			return
		}

		select {
		case <-time.After(s.chainPollInterval):
		case <-s.shutdownCtx.Done():
			abort("shutdown")
			return
		}
	}
}

// chainSettleWait sleeps the configured post-health wait for a health-plus-wait
// edge, re-checking shutdown and edge registration around the sleep like the
// jitter path does. Returns false if the step should abort.
func (s *Scheduler) chainSettleWait(edge *chainEdge, predRi resourceIdentifier) bool {
	select {
	case <-s.shutdownCtx.Done():
		return false
	default:
	}
	if !s.edgeStillRegistered(predRi, edge.followerRi) {
		return false
	}
	s.logger.Info("applying post-health wait before chained restart",
		zap.String("predecessor", string(predRi)),
		zap.String("follower", string(edge.followerRi)),
		zap.Duration("wait", edge.wait),
	)
	select {
	case <-time.After(edge.wait):
	case <-s.shutdownCtx.Done():
		return false
	}
	if !s.edgeStillRegistered(predRi, edge.followerRi) {
		return false
	}
	return true
}

func (s *Scheduler) getWorkload(ref workloadRef) (runtime.Object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), API_CALL_TIMEOUT)
	defer cancel()
	opts := metav1.GetOptions{}
	switch ref.kind {
	case "Deployment":
		return s.clientset.AppsV1().Deployments(ref.namespace).Get(ctx, ref.name, opts)
	case "DaemonSet":
		return s.clientset.AppsV1().DaemonSets(ref.namespace).Get(ctx, ref.name, opts)
	case "StatefulSet":
		return s.clientset.AppsV1().StatefulSets(ref.namespace).Get(ctx, ref.name, opts)
	default:
		return nil, fmt.Errorf("unsupported kind %q in workload ref", ref.kind)
	}
}

func (s *Scheduler) reconcileChainEdges(obj runtime.Object) {
	om, objk := getObjectMetaAndKind(obj)
	ri := getResourceIdentifier(om, objk)

	// rebuild from scratch so removed/invalid annotations drop stale edges; the
	// desired set is re-registered below when valid
	s.removeChainEdgesForFollower(ri)

	if !hasChainAnnotations(om) {
		return
	}

	refs, err := parsePredecessorRefs(om)
	if err != nil {
		s.logger.Error("invalid restart-after annotation, skipping chain edges", zap.String("resource", string(ri)), zap.Error(err))
		return
	}

	modeStr := strings.TrimSpace(om.GetAnnotations()[RESTART_AFTER_MODE_KEY])
	var mode chainMode
	switch modeStr {
	case "", CHAIN_MODE_HEALTH:
		mode = chainModeHealth
	case CHAIN_MODE_HEALTH_PLUS_WAIT:
		mode = chainModeHealthPlusWait
	default:
		s.logger.Error("invalid restart-after-mode, skipping chain edges", zap.String("resource", string(ri)), zap.String("mode", modeStr))
		return
	}

	waitStr := strings.TrimSpace(om.GetAnnotations()[RESTART_AFTER_WAIT_KEY])
	var wait time.Duration
	if waitStr != "" {
		d, err := time.ParseDuration(waitStr)
		if err != nil || d <= 0 {
			s.logger.Error("invalid restart-after-wait, skipping chain edges", zap.String("resource", string(ri)), zap.String("wait", waitStr), zap.Error(err))
			return
		}
		wait = d
	}
	switch mode {
	case chainModeHealth:
		if waitStr != "" {
			s.logger.Error("restart-after-wait is only valid with restart-after-mode health-plus-wait, skipping chain edges", zap.String("resource", string(ri)))
			return
		}
	case chainModeHealthPlusWait:
		if waitStr == "" {
			s.logger.Error("restart-after-wait is required with restart-after-mode health-plus-wait, skipping chain edges", zap.String("resource", string(ri)))
			return
		}
	}

	for _, ref := range refs {
		predRi := ref.identifier()
		if s.wouldCreateCycle(ri, predRi) {
			s.logger.Error("skipping chain edge that would create a cycle",
				zap.String("follower", string(ri)),
				zap.String("predecessor", string(predRi)),
			)
			continue
		}
		entry := s.getOrCreateChainEntry(predRi)
		entry.Lock()
		entry.edges[ri] = &chainEdge{
			predecessor: ref,
			followerRi:  ri,
			obj:         obj,
			mode:        mode,
			wait:        wait,
		}
		entry.Unlock()
		s.logger.Info("registered chain edge",
			zap.String("follower", string(ri)),
			zap.String("predecessor", string(predRi)),
			zap.String("mode", chainModeDisplay(mode)),
		)
	}
}

func (s *Scheduler) getOrCreateChainEntry(predRi resourceIdentifier) *chainMapEntry {
	raw, ok := s.chainMap.Load(predRi)
	if ok {
		return raw.(*chainMapEntry)
	}
	entry := &chainMapEntry{edges: map[resourceIdentifier]*chainEdge{}}
	actual, _ := s.chainMap.LoadOrStore(predRi, entry)
	return actual.(*chainMapEntry)
}

func (s *Scheduler) removeChainEdgesForFollower(followerRi resourceIdentifier) {
	s.chainMap.Range(func(key, value any) bool {
		entry := value.(*chainMapEntry)
		entry.Lock()
		if _, ok := entry.edges[followerRi]; ok {
			delete(entry.edges, followerRi)
			if len(entry.edges) == 0 {
				s.chainMap.Delete(key)
			}
		}
		entry.Unlock()
		return true
	})
}

func (s *Scheduler) edgeStillRegistered(predRi, followerRi resourceIdentifier) bool {
	raw, ok := s.chainMap.Load(predRi)
	if !ok {
		return false
	}
	entry := raw.(*chainMapEntry)
	entry.RLock()
	defer entry.RUnlock()
	_, ok = entry.edges[followerRi]
	return ok
}

// refreshChainEdgeObject re-fetches the follower after a chained restart so the
// status page shows its updated last-restart timestamp.
func (s *Scheduler) refreshChainEdgeObject(predRi, followerRi resourceIdentifier) {
	kind, ns, name, ok := parseResourceIdentifier(followerRi)
	if !ok {
		return
	}
	obj, err := s.getWorkload(workloadRef{kind: kind, namespace: ns, name: name})
	if err != nil {
		return
	}
	raw, ok := s.chainMap.Load(predRi)
	if !ok {
		return
	}
	entry := raw.(*chainMapEntry)
	entry.Lock()
	if edge, exists := entry.edges[followerRi]; exists {
		edge.obj = obj
	}
	entry.Unlock()
}

// wouldCreateCycle reports whether adding the edge predRi→followerRi would close
// a cycle, i.e. whether predRi is reachable from followerRi by following
// existing predecessor→follower edges.
func (s *Scheduler) wouldCreateCycle(followerRi, predRi resourceIdentifier) bool {
	if followerRi == predRi {
		return true
	}
	visited := map[resourceIdentifier]struct{}{followerRi: {}}
	stack := []resourceIdentifier{followerRi}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		raw, ok := s.chainMap.Load(cur)
		if !ok {
			continue
		}
		entry := raw.(*chainMapEntry)
		entry.RLock()
		found := false
		for next := range entry.edges {
			if next == predRi {
				found = true
				break
			}
			if _, seen := visited[next]; !seen {
				visited[next] = struct{}{}
				stack = append(stack, next)
			}
		}
		entry.RUnlock()
		if found {
			return true
		}
	}
	return false
}

// isRolloutComplete reports whether a workload's most recent rollout has fully
// landed: the status controller has observed the current spec and every desired
// replica is updated, ready, and (for Deployments) free of terminating old pods.
func isRolloutComplete(obj runtime.Object) bool {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return deploymentRolloutComplete(o)
	case *appsv1.DaemonSet:
		return daemonSetRolloutComplete(o)
	case *appsv1.StatefulSet:
		return statefulSetRolloutComplete(o)
	default:
		return false
	}
}

func deploymentRolloutComplete(d *appsv1.Deployment) bool {
	if d.Spec.Paused {
		return false
	}
	if d.Status.ObservedGeneration < d.Generation {
		return false
	}
	desired := int32(1)
	if d.Spec.Replicas != nil {
		desired = *d.Spec.Replicas
	}
	if desired == 0 {
		return true
	}
	if d.Status.UpdatedReplicas < desired {
		return false
	}
	// old pods still terminating: replicas exceed the updated set until they drain
	if d.Status.Replicas > d.Status.UpdatedReplicas {
		return false
	}
	if d.Status.AvailableReplicas < d.Status.UpdatedReplicas {
		return false
	}
	return true
}

func statefulSetRolloutComplete(sts *appsv1.StatefulSet) bool {
	if sts.Status.ObservedGeneration < sts.Generation {
		return false
	}
	desired := int32(1)
	if sts.Spec.Replicas != nil {
		desired = *sts.Spec.Replicas
	}
	if desired == 0 {
		return true
	}
	if sts.Status.UpdatedReplicas < desired {
		return false
	}
	if sts.Status.ReadyReplicas < desired {
		return false
	}
	if sts.Status.CurrentRevision != sts.Status.UpdateRevision {
		return false
	}
	return true
}

func daemonSetRolloutComplete(ds *appsv1.DaemonSet) bool {
	if ds.Status.ObservedGeneration < ds.Generation {
		return false
	}
	desired := ds.Status.DesiredNumberScheduled
	if desired == 0 {
		return true
	}
	if ds.Status.UpdatedNumberScheduled < desired {
		return false
	}
	if ds.Status.NumberReady < desired {
		return false
	}
	return true
}

func restartFunc(ctx context.Context, logger *zap.Logger, clientset kubernetes.Interface, incomingObject runtime.Object, metrics *KairosMetrics) bool {
	logger.Debug("entering restartFunc")

	om, _ := getObjectMetaAndKind(incomingObject)
	namespace := om.GetNamespace()
	name := om.GetName()
	kind := kindFromObject(incomingObject)

	logger.Info("firing restartFunc", zap.Time("time", time.Now()), zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name))

	start := time.Now()
	now := start.Format(LAST_RESTARTED_AT_TIME_FORMAT)

	ctx, cancel := context.WithTimeout(ctx, API_CALL_TIMEOUT)
	defer cancel()

	supported, err := patchPodTemplateAnnotation(ctx, logger, clientset, incomingObject, CRON_LAST_RESTARTED_AT_KEY, now)
	if !supported {
		return false
	}

	if metrics != nil {
		metrics.RestartDuration.WithLabelValues(kind, namespace, name).Observe(time.Since(start).Seconds())
	}

	if err != nil {
		logger.Error("error patching object in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name), zap.Error(err))
		if metrics != nil {
			metrics.RestartErrorsTotal.WithLabelValues(kind, namespace, name, "patch").Inc()
		}
		return false
	}

	if metrics != nil {
		metrics.RestartTotal.WithLabelValues(kind, namespace, name).Inc()
	}
	return true
}

// patchPodTemplateAnnotation sets a single annotation on the workload's pod template via a
// JSON merge patch, so concurrent edits to other fields are not clobbered and no
// conflict-retry loop is needed. The bool reports whether the object type is supported.
func patchPodTemplateAnnotation(ctx context.Context, logger *zap.Logger, clientset kubernetes.Interface, obj runtime.Object, key, value string) (bool, error) {
	payload, err := json.Marshal(map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]string{key: value},
				},
			},
		},
	})
	if err != nil {
		return false, fmt.Errorf("error building patch payload: %w", err)
	}

	om, _ := getObjectMetaAndKind(obj)
	namespace := om.GetNamespace()
	name := om.GetName()
	opts := metav1.PatchOptions{}

	switch obj.(type) {
	case *appsv1.Deployment:
		_, err = clientset.AppsV1().Deployments(namespace).Patch(ctx, name, types.MergePatchType, payload, opts)
	case *appsv1.DaemonSet:
		_, err = clientset.AppsV1().DaemonSets(namespace).Patch(ctx, name, types.MergePatchType, payload, opts)
	case *appsv1.StatefulSet:
		_, err = clientset.AppsV1().StatefulSets(namespace).Patch(ctx, name, types.MergePatchType, payload, opts)
	default:
		logger.Error("unsupported type in restartFunc", zap.String("type", fmt.Sprintf("%T", obj)))
		return false, nil
	}
	return true, err
}
