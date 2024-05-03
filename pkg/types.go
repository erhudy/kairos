package pkg

import (
	"fmt"
	"sync"

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
	objectMap    *sync.Map
}

type cronPatternWithTimezone struct {
	cronPattern    string
	locationString string // not using *time.Location here because it causes problems when comparing two patterns
}
type resourceIdentifier string

func (c cronPatternWithTimezone) String() string {
	return fmt.Sprintf("%s_%s", c.cronPattern, c.locationString)
}

func (c cronPatternWithTimezone) Equals(d cronPatternWithTimezone) bool {
	return c.cronPattern == d.cronPattern && c.locationString == d.locationString
}

type Scheduler struct {
	logger      *zap.Logger
	workchan    <-chan ObjectAndSchedulerAction
	cron        *gocron.Scheduler
	clientset   kubernetes.Interface
	resourceMap *sync.Map
}

type SchedulerAction int

const (
	RESOURCE_CHANGE SchedulerAction = iota
	RESOURCE_DELETE
)

type ObjectAndSchedulerAction struct {
	action SchedulerAction
	obj    runtime.Object
}
