package pkg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-co-op/gocron"
	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
)

func NewScheduler(timezone *time.Location, logger *zap.Logger, workchan <-chan ObjectAndSchedulerAction, clientset kubernetes.Interface) *Scheduler {
	scheduler := gocron.NewScheduler(timezone)
	scheduler.TagsUnique()

	return &Scheduler{
		logger:      logger,
		workchan:    workchan,
		cron:        scheduler,
		clientset:   clientset,
		resourceMap: &sync.Map{},
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

func (s *Scheduler) ShowJobStatus() {
	s.logger.Info("got signal to dump job status")
	s.resourceMap.Range(func(key, value any) bool {
		valueAsserted := value.(*map[cronPattern]*gocron.Job)
		jobs := []struct {
			cronStrings cronPattern
			job         *gocron.Job
		}{}
		for cp, job := range *valueAsserted {
			jobs = append(jobs, struct {
				cronStrings cronPattern
				job         *gocron.Job
			}{
				cronStrings: cp,
				job:         job,
			})
		}
		s.logger.Info("job status for resource", zap.Any("resource", key))
		for _, j := range jobs {
			nextRun := j.job.NextRun()
			s.logger.Info("job detail",
				zap.String("tags", strings.Join(j.job.Tags(), ",")),
				zap.String("cron-pattern", j.cronStrings.String()),
				zap.Time("last-run", j.job.LastRun()),
				zap.Time("next-run", nextRun),
				zap.Duration("next-run-in", time.Until(nextRun)),
			)
		}
		s.logger.Info("----------------------------------")
		return true
	})
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
		tempMap := make(map[cronPattern]*gocron.Job)
		s.resourceMap.Store(ri, &tempMap)
		registeredJobsForResourceRaw = &tempMap
	}
	registeredJobsForResource := registeredJobsForResourceRaw.(*map[cronPattern]*gocron.Job)
	for pattern := range *registeredJobsForResource {
		cronPatternsFromMap = append(cronPatternsFromMap, pattern)
	}

	// strings and not cronPatterns
	patternsToAdd := []cronPattern{}
	patternsToDelete := []cronPattern{}
	patternsThatDidNotChangeMap := make(map[cronPattern]struct{})

	s.logger.Debug("patterns already registered", zap.String("resource", string(ri)), zap.Stringers("patterns", cronPatternsFromMap))

	// if the pattern is in our map, but is not on the resource, it has been removed and so we delete the restart job
	for _, i := range cronPatternsFromMap {
		found := false
		for _, j := range cronPatternsFromResource {
			if i == j {
				found = true
				break
			}
		}
		if found {
			patternsThatDidNotChangeMap[i] = struct{}{}
		} else {
			if i.String() != "" {
				patternsToDelete = append(patternsToDelete, i)
			}
		}
	}

	// if the pattern is on the resource, but is not in our map, it is a new pattern and so we need to make a restart job
	for _, i := range cronPatternsFromResource {
		found := false
		for _, j := range cronPatternsFromMap {
			if i == j {
				found = true
				break
			}
		}
		if found {
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
		job := (*registeredJobsForResource)[p]
		err := s.deleteJob(p, ri, job)
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

	scheduler := cronFunc(cpString)
	job, err = scheduler.Tag(string(tag)).Do(restartFunc, ctx, s.logger, s.clientset, obj)
	if err != nil {
		return fmt.Errorf("error in createJob during creation: %w", err)
	}

	registeredJobsForResourceRaw, ok := s.resourceMap.Load(ri)
	if !ok {
		tempMap := make(map[cronPattern]*gocron.Job)
		s.resourceMap.Store(ri, &tempMap)
		registeredJobsForResourceRaw = &tempMap
	}
	registeredJobsForResource := registeredJobsForResourceRaw.(*map[cronPattern]*gocron.Job)

	(*registeredJobsForResource)[cp] = job

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
	registeredJobsForResource := registeredJobsForResourceRaw.(*map[cronPattern]*gocron.Job)

	for cronPattern, job := range *registeredJobsForResource {
		err := s.deleteJob(cronPattern, ri, job)
		if err != nil {
			return err
		}
	}

	s.resourceMap.Delete(ri)
	return nil
}

func (s *Scheduler) deleteJob(cp cronPattern, ri resourceIdentifier, job *gocron.Job) error {
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
		registeredJobsForResource := registeredJobsForResourceRaw.(*map[cronPattern]*gocron.Job)
		delete(*registeredJobsForResource, cp)
		s.logger.Info(
			"deleted job",
			zap.String("resource", string(ri)),
			zap.String("cron-pattern", string(cp)),
		)
	}
	return nil
}

func restartFunc(ctx context.Context, logger *zap.Logger, clientset kubernetes.Interface, incomingObject runtime.Object) {
	logger.Debug("entering restartFunc")

	om, _ := getObjectMetaAndKind(incomingObject)
	namespace := om.GetNamespace()
	name := om.GetName()

	logger.Info("firing restartFunc", zap.Time("time", time.Now()), zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name))

	now := time.Now().Format(LAST_RESTARTED_AT_TIME_FORMAT)
	var err error

	switch incomingObject.(type) {
	case *appsv1.Deployment:
		obj, getErr := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			logger.Error("error getting object in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name), zap.Error(getErr))
			return
		}
		if obj.Spec.Template.Annotations == nil {
			obj.Spec.Template.Annotations = make(map[string]string)
		}
		obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY] = now
		_, err = clientset.AppsV1().Deployments(namespace).Update(ctx, obj, metav1.UpdateOptions{})
	case *appsv1.DaemonSet:
		obj, getErr := clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			logger.Error("error getting object in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name), zap.Error(getErr))
			return
		}
		if obj.Spec.Template.Annotations == nil {
			obj.Spec.Template.Annotations = make(map[string]string)
		}
		obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY] = now
		_, err = clientset.AppsV1().DaemonSets(namespace).Update(ctx, obj, metav1.UpdateOptions{})
	case *appsv1.StatefulSet:
		obj, getErr := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			logger.Error("error getting object in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name), zap.Error(getErr))
			return
		}
		if obj.Spec.Template.Annotations == nil {
			obj.Spec.Template.Annotations = make(map[string]string)
		}
		obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY] = now
		_, err = clientset.AppsV1().StatefulSets(namespace).Update(ctx, obj, metav1.UpdateOptions{})
	default:
		logger.Error("unsupported type in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)))
		return
	}

	if err != nil {
		logger.Error("error updating object in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name), zap.Error(err))
	}
}
