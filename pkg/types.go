package pkg

import (
	"context"
	"fmt"
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
	logger    *zap.Logger
	indexer   cache.Store
	queue     workqueue.TypedRateLimitingInterface[string]
	informer  cache.Controller
	typename  string
	workchan  chan<- ObjectAndSchedulerAction
	objectMap *sync.Map
	metrics   *KairosMetrics
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

// workloadRef identifies a workload by canonical kind, namespace, and name.
type workloadRef struct {
	kind      string
	namespace string
	name      string
	display   string
}

func (w workloadRef) identifier() resourceIdentifier {
	return resourceIdentifier(fmt.Sprintf("apps/v1, Kind=%s/%s/%s", w.kind, w.namespace, w.name))
}

type chainMode int

const (
	chainModeHealth chainMode = iota
	chainModeHealthPlusWait
)

// chainEdge is a single predecessor→follower link: when the predecessor's restart
// completes, the follower is restarted once the predecessor is healthy again.
type chainEdge struct {
	predecessor workloadRef
	followerRi  resourceIdentifier
	obj         runtime.Object
	mode        chainMode
	wait        time.Duration
}

// chainMapEntry holds all followers of one predecessor, guarded by its own mutex.
type chainMapEntry struct {
	sync.RWMutex
	edges map[resourceIdentifier]*chainEdge
}

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
	// chainTimeout caps how long a chain step waits for its predecessor to become
	// healthy again; when exceeded the cascade is aborted, not fired onto an
	// unhealthy dependency
	chainTimeout time.Duration
	// chainPollInterval is how often a chain step re-checks predecessor health;
	// a field (not just CHAIN_POLL_INTERVAL) so tests can shrink it
	chainPollInterval time.Duration
	// chainMap maps predecessor resourceIdentifier -> *chainMapEntry of followers
	chainMap *sync.Map
	// pendingSteps dedupes to one in-flight chain step per follower, keyed by
	// follower resourceIdentifier
	pendingSteps *sync.Map
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
