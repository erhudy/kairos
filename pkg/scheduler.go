package pkg

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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

func NewScheduler(logger *zap.Logger, workchan <-chan ObjectAndSchedulerAction, clientset kubernetes.Interface) *Scheduler {
	scheduler := gocron.NewScheduler(time.UTC)
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
		case i := <-s.workchan:
			s.processSchedulerBundle(i)
		}
	}
}

func (s *Scheduler) ShowJobStatus() {
	s.logger.Info("got signal to dump job status")
	s.resourceMap.Range(func(key, value any) bool {
		valueAsserted := value.(*map[cronPattern]*gocron.Job)
		cronStrings := []string{}
		for cp := range *valueAsserted {
			cronStrings = append(cronStrings, fmt.Sprintf("'%s'", cp))
		}
		s.logger.Info(fmt.Sprintf("JOB STATUS FOR '%s': %s", key, strings.Join(cronStrings, " ")))
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
	pattern := getCronPattern(objm)
	if pattern == "" {
		s.logger.Debug("cron expression was empty", zap.String("resource", string(ri)))
		return nil
	}

	splitPatternsRaw := strings.Split(string(pattern), ";")
	cronPatternsFromResource := []cronPattern{}
	for _, p := range splitPatternsRaw {
		cronPatternsFromResource = append(cronPatternsFromResource, cronPattern(strings.TrimSpace(p)))
	}

	// build a comparison list against the keys in the resource map for this resource to figure out what to add/delete/ignore
	cronPatternsFromMap := []cronPattern{}
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
			if i != "" {
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
			if i != "" {
				patternsToAdd = append(patternsToAdd, i)
			}
		}
	}

	patternsThatDidNotChange := []cronPattern{}
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

	var job *gocron.Job

	// if 5 fields, regular cron, if 6 fields, cron with seconds, otherwise freak out
	var cronFunc func(string) *gocron.Scheduler

	switch l := len(strings.Split(string(cp), " ")); {
	case l == 5:
		cronFunc = s.cron.Cron
	case l == 6:
		cronFunc = s.cron.CronWithSeconds
	default:
		return fmt.Errorf("got %d fields splitting cron expression '%s', expected 5 or 6", l, cp)
	}

	tag := fmt.Sprintf("%s--%s", ri, cp)

	var err error
	job, err = cronFunc(string(cp)).Tag(string(tag)).Do(restartFunc, ctx, s.logger, s.clientset, obj)
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
	var obj runtime.Object
	var err error

	switch incomingObject.(type) {
	case *appsv1.DaemonSet:
		obj, err = clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
	case *appsv1.Deployment:
		obj, err = clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	case *appsv1.StatefulSet:
		obj, err = clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	default:
		panic(fmt.Errorf("panicked in restartFunc because got type %T", incomingObject))
	}

	logger.Info("firing restartFunc", zap.Time("time", time.Now()), zap.String("type", fmt.Sprintf("%T", obj)), zap.String("namespace", namespace), zap.String("name", name))

	if err != nil {
		logger.Error("error getting object in restartFunc", zap.String("type", fmt.Sprintf("%T", obj)), zap.String("namespace", namespace), zap.String("name", name))
		return
	}

	template := reflect.Indirect(reflect.ValueOf(obj)).FieldByName("Spec").FieldByName("Template")
	annotations := template.FieldByName("Annotations").Interface().(map[string]string)

	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[CRON_LAST_RESTARTED_AT_KEY] = time.Now().Format(LAST_RESTARTED_AT_TIME_FORMAT)
	template.FieldByName("Annotations").Set(reflect.ValueOf(annotations))

	switch incomingObject.(type) {
	case *appsv1.DaemonSet:
		_, err = clientset.AppsV1().DaemonSets(namespace).Update(ctx, obj.(*appsv1.DaemonSet), metav1.UpdateOptions{})
	case *appsv1.Deployment:
		_, err = clientset.AppsV1().Deployments(namespace).Update(ctx, obj.(*appsv1.Deployment), metav1.UpdateOptions{})
	case *appsv1.StatefulSet:
		_, err = clientset.AppsV1().StatefulSets(namespace).Update(ctx, obj.(*appsv1.StatefulSet), metav1.UpdateOptions{})
	default:
		panic(fmt.Errorf("panicked in restartFunc because got type %T", incomingObject))
	}

	if err != nil {
		logger.Error("error updating object in restartFunc", zap.String("type", fmt.Sprintf("%T", incomingObject)), zap.String("namespace", namespace), zap.String("name", name))
		return
	}
}
