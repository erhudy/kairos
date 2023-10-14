package pkg

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
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
		logger:    logger,
		workchan:  workchan,
		cron:      scheduler,
		clientset: clientset,
		jobMap:    make(map[jobTag]JobAndCronPattern),
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

func (s *Scheduler) processSchedulerBundle(action ObjectAndSchedulerAction) {
	om, ok := getObjectMetaAndKind(action.obj)
	tag := getJobTag(om, ok)

	switch action.action {
	case SCHEDULER_DELETE:
		err := s.deleteJobByTag(tag)
		if err != nil {
			s.logger.Error("error removing job from scheduler", zap.Error(err))
		}
	case SCHEDULER_UPSERT:
		jobs, err := s.cron.FindJobsByTag(string(tag))
		if err != nil && !errors.Is(err, gocron.ErrJobNotFoundWithTag) {
			s.logger.Error("error fetching jobs by tag from scheduler", zap.String("tag", string(tag)), zap.Error(err))
			break
		}

		if len(jobs) > 1 {
			s.logger.Error("got unexpected count of jobs back for tag, only expected 0 or 1", zap.Int("count", len(jobs)), zap.String("tag", string(tag)))
			break
		}

		cronPattern := getCronPattern(om)
		if cronPattern == "" {
			s.logger.Error("cron expression was empty", zap.String("tag", string(tag)))
			break
		}
		job, err := s.createOrUpdateJobByTag(tag, cronPattern, action.obj)
		if err != nil {
			s.logger.Error("error upserting job", zap.String("tag", string(tag)), zap.Error(err))
		}
		s.jobMap[tag] = JobAndCronPattern{
			cronPattern: cronPattern,
			job:         job,
		}
		s.logger.Info("upserted job", zap.String("tag", string(tag)), zap.String("cron-pattern", cronPattern))
	}
}

// creates/updates the job (by deleting/recreating) and returns it for inspection
func (s *Scheduler) createOrUpdateJobByTag(tag jobTag, cronPattern string, obj runtime.Object) (*gocron.Job, error) {
	ctx := context.Background()

	jcp, ok := s.jobMap[tag]

	var job *gocron.Job
	if ok {
		job = jcp.job
		if jcp.cronPattern == cronPattern {
			s.logger.Debug("cron pattern matches previous, no change", zap.String("tag", string(tag)), zap.String("cron-pattern", cronPattern))
			return job, nil
		} else {
			// in all other cases the job is either new or the cron pattern has changed -
			// attempt to remove the job by tag and then recreate
			err := s.deleteJobByTag(tag)
			if err != nil {
				return job, fmt.Errorf("error in createOrUpdateJobByTag during deletion: %w", err)
			}
		}
	}

	// if 5 fields, regular cron, if 6 fields, cron with seconds, otherwise freak out
	var cronFunc func(string) *gocron.Scheduler

	switch l := len(strings.Split(cronPattern, " ")); {
	case l == 5:
		cronFunc = s.cron.Cron
	case l == 6:
		cronFunc = s.cron.CronWithSeconds
	default:
		return job, fmt.Errorf("got %d fields splitting cron expression '%s', expected 5 or 6", l, cronPattern)
	}

	var err error
	job, err = cronFunc(cronPattern).Tag(string(tag)).Do(restartFunc, ctx, s.logger, s.clientset, obj)
	if err != nil {
		return job, fmt.Errorf("error in createOrUpdateJobByTag during creation: %w", err)
	}
	return job, nil
}

func (s *Scheduler) deleteJobByTag(tag jobTag) error {
	s.logger.Info("deleting job by tag", zap.String("tag", string(tag)))
	err := s.cron.RemoveByTag(string(tag))
	if err != nil {
		if errors.Is(err, gocron.ErrJobNotFoundWithTag) {
			// nonexistent tag is not considered fatal, just some sloppy bookkeeping somewhere that shouldn't happen
			s.logger.Error("requested to delete nonexistent tag", zap.String("tag", string(tag)))
		} else {
			return fmt.Errorf("error in deleteJob: %w", err)
		}
	} else {
		s.logger.Info("deleted tag", zap.String("tag", string(tag)))
	}
	delete(s.jobMap, tag)
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
