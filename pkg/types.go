package pkg

import (
	"strings"
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

type resourceIdentifier string

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
