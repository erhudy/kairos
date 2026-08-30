package pkg

import (
	"context"
	"strings"
	"sync"
	"time"

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
	indexer      cache.Store
	queue        workqueue.TypedRateLimitingInterface[string]
	informer     cache.Controller
	typespecimen runtime.Object
	typename     string
	workchan     chan<- ObjectAndSchedulerAction
	objectMap    *sync.Map
	metrics      *KairosMetrics
	// stopCh is set by Run; synchronize uses it to abandon waiting for a scheduler
	// ack during shutdown instead of blocking its worker forever
	stopCh <-chan struct{}
}

type cronPattern string

func (c cronPattern) String() string {
	return string(c)
}

type cronPatterns []cronPattern

func (c cronPatterns) String() string {
	ss := []string{}
	for _, x := range c {
		ss = append(ss, string(x))
	}
	return strings.Join(ss, ", ")
}

type resourceMapEntry struct {
	sync.RWMutex
	obj         runtime.Object
	jobs        map[cronPattern]*gocron.Job
	lastJitters map[cronPattern]time.Duration
}

type resourceIdentifier string

type Scheduler struct {
	logger      *zap.Logger
	workchan    <-chan ObjectAndSchedulerAction
	cron        *gocron.Scheduler
	clientset   kubernetes.Interface
	resourceMap *sync.Map
	metrics     *KairosMetrics
	maxJitter   time.Duration
	lookback    time.Duration
	timezone    *time.Location
	// startTime is when this scheduler instance came up; missed-restart catch-up
	// only applies to firings from before then (i.e. while kairos was not running)
	startTime      time.Time
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

type SchedulerAction int

const (
	RESOURCE_CHANGE SchedulerAction = iota
	RESOURCE_DELETE
)

type ObjectAndSchedulerAction struct {
	action SchedulerAction
	obj    runtime.Object
	// errCh is a buffered (size 1) channel on which the scheduler reports the
	// result of processing this action so the controller can retry failures via
	// its workqueue; nil means no ack is expected
	errCh chan error
}
