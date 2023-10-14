package pkg

import (
	"github.com/go-co-op/gocron"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// Controller demonstrates how to implement a controller with client-go.
type Controller struct {
	logger       *zap.Logger
	indexer      cache.Indexer
	queue        workqueue.RateLimitingInterface
	informer     cache.Controller
	typespecimen runtime.Object
	typename     string
	workchan     chan<- ObjectAndSchedulerAction
}

type jobTag string

type Scheduler struct {
	logger    *zap.Logger
	workchan  <-chan ObjectAndSchedulerAction
	cron      *gocron.Scheduler
	clientset kubernetes.Interface
	jobMap    map[jobTag]JobAndCronPattern
}

type SchedulerAction int

const (
	SCHEDULER_DELETE SchedulerAction = iota
	SCHEDULER_UPSERT
)

type ObjectAndSchedulerAction struct {
	action SchedulerAction
	obj    runtime.Object
}

type JobAndCronPattern struct {
	cronPattern string
	job         *gocron.Job
}
