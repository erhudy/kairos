package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/go-co-op/gocron"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

func NewScheduler(oasaChannel <-chan ObjectAndSchedulerAction, clientset kubernetes.Interface) *Scheduler {
	scheduler := gocron.NewScheduler(time.UTC)
	scheduler.TagsUnique()

	return &Scheduler{
		workchan:  oasaChannel,
		cron:      scheduler,
		clientset: clientset,
		jobMap:    make(map[jobTag]JobAndCronPattern),
	}
}

func (s *Scheduler) Run(stopCh chan struct{}) {
	s.cron.StartAsync()

	select {
	case i := <-s.workchan:
		s.processSchedulerBundle(i)
	case <-stopCh:
		klog.Info("Stopping scheduler")
	}
}

func (s *Scheduler) processSchedulerBundle(action ObjectAndSchedulerAction) {
	om, ok := getObjectMetaAndKind(action.obj)
	tag := getJobTag(om, ok)

	switch action.action {
	case SCHEDULER_DELETE:
		err := s.deleteJobByTag(tag)
		if err != nil {
			klog.Errorf("error removing job from scheduler: %w", err)
		}
	case SCHEDULER_UPSERT:
		jobs, err := s.cron.FindJobsByTag(string(tag))
		if err != nil && !errors.Is(err, gocron.ErrJobNotFoundWithTag) {
			klog.Errorf("error fetching jobs by tag from scheduler: %w", err)
			break
		}

		if len(jobs) > 1 {
			klog.Errorf("got %d jobs back for tag '%s', only expected 0 or 1", len(jobs), tag)
			break
		}

		cronPattern := getCronPattern(om)
		if cronPattern == "" {
			klog.Errorf("cron expression for tag %s was empty", tag)
			break
		}
		_, err = s.createOrUpdateJobByTag(tag, cronPattern, action.obj)
		if err != nil {
			klog.Errorf("error upserting job: %w", err)
		}
		klog.Infof("created/updated job for tag '%s' with cron expression '%s'", tag, cronPattern)
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
			// no change, can exit now
			return job, nil
		}
	}

	// in all other cases the job is either new or the cron pattern has changed -
	// attempt to remove the job by tag, ignoring not found errors, and then recreate
	err := s.deleteJobByTag(tag)
	if err != nil {
		return job, fmt.Errorf("error in createOrUpdateJobByTag during deletion: %w", err)
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

	job, err = cronFunc(cronPattern).Do(restartFunc, ctx, s.clientset, obj)
	if err != nil {
		return job, fmt.Errorf("error in createOrUpdateJobByTag during creation: %w", err)
	}
	return job, nil
}

func (s *Scheduler) deleteJobByTag(tag jobTag) error {
	err := s.cron.RemoveByTag(string(tag))
	if err != nil && !errors.Is(err, gocron.ErrJobNotFoundWithTag) {
		return fmt.Errorf("error in deleteJob: %w", err)
	}
	return nil
}

func restartFunc(ctx context.Context, clientset kubernetes.Interface, incomingObject runtime.Object) {
	klog.Infof("firing restartFunc at %s", time.Now())
	om, _ := getObjectMetaAndKind(incomingObject)

	namespace := om.GetNamespace()
	name := om.GetName()

	var obj runtime.Object
	var err error
	switch incomingObject.(type) {
	case *appsv1.DaemonSet:
		obj, err = clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, v1.GetOptions{})
		if err != nil {
			klog.Errorf("got error looking up DaemonSet %s/%s", namespace, name)
			return
		}
		klog.Infof("got daemonset %s", name)
	case *appsv1.Deployment:
		obj, err = clientset.AppsV1().Deployments(namespace).Get(ctx, name, v1.GetOptions{})
		if err != nil {
			klog.Errorf("got error looking up Deployment %s/%s", namespace, name)
			return
		}
		klog.Infof("got deployment %s", name)
	case *appsv1.StatefulSet:
		obj, err = clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, v1.GetOptions{})
		if err != nil {
			klog.Errorf("got error looking up StatefulSet %s/%s", namespace, name)
			return
		}
		klog.Infof("got statefulset %s", name)

	default:
		panic(fmt.Errorf("panicked in restartFunc because got type %t", incomingObject))
	}

	template := reflect.Indirect(reflect.ValueOf(obj)).FieldByName("Spec").FieldByName("Template")
	annotations := template.FieldByName("Annotations").Interface().(map[string]string)

	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[CRON_LAST_RESTARTED_AT_KEY] = time.Now().Format(LAST_RESTARTED_AT_TIME_FORMAT)

	annotationsReflectValue := reflect.ValueOf(annotations)
	template.MethodByName("SetAnnotations").Call([]reflect.Value{annotationsReflectValue})

	switch incomingObject.(type) {
	case *appsv1.DaemonSet:
		_, err = clientset.AppsV1().DaemonSets(namespace).Update(ctx, obj.(*appsv1.DaemonSet), v1.UpdateOptions{})
		if err != nil {
			klog.Errorf("got error updating DaemonSet %s/%s", namespace, name)
			return
		}
	case *appsv1.Deployment:
		_, err = clientset.AppsV1().Deployments(namespace).Update(ctx, obj.(*appsv1.Deployment), v1.UpdateOptions{})
		if err != nil {
			klog.Errorf("got error updating Deployment %s/%s", namespace, name)
			return
		}
	case *appsv1.StatefulSet:
		_, err = clientset.AppsV1().StatefulSets(namespace).Update(ctx, obj.(*appsv1.StatefulSet), v1.UpdateOptions{})
		if err != nil {
			klog.Errorf("got error updating StatefulSet %s/%s", namespace, name)
			return
		}
	default:
		panic(fmt.Errorf("panicked in restartFunc because got type %t", incomingObject))
	}
}
