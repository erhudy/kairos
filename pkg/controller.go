package pkg

import (
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// NewController creates a new Controller.
func NewController(logger *zap.Logger, queue workqueue.RateLimitingInterface, indexer cache.Indexer, informer cache.Controller, typespecimen runtime.Object, typename string, workchan chan<- ObjectAndSchedulerAction) *Controller {
	return &Controller{
		logger:       logger,
		informer:     informer,
		indexer:      indexer,
		queue:        queue,
		typespecimen: typespecimen,
		typename:     typename,
		workchan:     workchan,
		objectMap:    &sync.Map{},
	}
}

func (c *Controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.queue.Done(key)

	// Invoke the method containing the business logic
	err := c.synchronize(key.(string))
	// Handle the error if something went wrong during the execution of the business logic
	c.handleErr(err, key)
	return true
}

// handleErr checks if an error happened and makes sure we will retry later.
func (c *Controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		c.logger.Error("error syncing item", zap.Any("key", key), zap.Error(err))

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	utilruntime.HandleError(err)
	c.logger.Info("dropping item out of the queue", zap.Any("key", key), zap.Error(err))
}

// Run begins watching and syncing.
func (c *Controller) Run(workers int, stopCh chan struct{}) {
	defer utilruntime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	c.logger.Info("starting controller", zap.String("type", c.typename))

	go c.informer.Run(stopCh)

	// Wait for all involved caches to be synced, before processing items from the queue is started
	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		utilruntime.HandleError(fmt.Errorf("timed out waiting for caches to sync"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	c.logger.Info("stopping controller", zap.String("type", c.typename))
}

func (c *Controller) runWorker() {
	for c.processNextItem() {
	}
}

func GenerateDeploymentController(logger *zap.Logger, clientset kubernetes.Interface, namespace string, workchan chan<- ObjectAndSchedulerAction) *Controller {
	return generateGenericController(logger, clientset.AppsV1().RESTClient(), namespace, "deployments", &appsv1.Deployment{}, workchan)
}

func GenerateDaemonSetController(logger *zap.Logger, clientset kubernetes.Interface, namespace string, workchan chan<- ObjectAndSchedulerAction) *Controller {
	return generateGenericController(logger, clientset.AppsV1().RESTClient(), namespace, "daemonsets", &appsv1.DaemonSet{}, workchan)
}

func GenerateStatefulSetController(logger *zap.Logger, clientset kubernetes.Interface, namespace string, workchan chan<- ObjectAndSchedulerAction) *Controller {
	return generateGenericController(logger, clientset.AppsV1().RESTClient(), namespace, "statefulsets", &appsv1.StatefulSet{}, workchan)
}

func generateGenericController(logger *zap.Logger, restclient rest.Interface, namespace string, typename string, typespecimen runtime.Object, workchan chan<- ObjectAndSchedulerAction) *Controller {
	watcher := cache.NewListWatchFromClient(restclient, typename, namespace, fields.Everything())
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

	indexer, informer := cache.NewIndexerInformer(watcher, typespecimen, 0, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
				logger.Debug("added object to add queue", zap.String("key", key))
			} else {
				logger.Error("error adding object to add queue", zap.Any("obj", obj), zap.String("key", key), zap.Error(err))
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(key)
				logger.Debug("added object to update queue", zap.String("key", key))
			} else {
				logger.Error("error adding object to update queue", zap.Any("obj", new), zap.String("key", key), zap.Error(err))
			}

		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
				logger.Debug("added object to delete queue", zap.String("key", key))
			} else {
				logger.Error("error adding object to delete queue", zap.Any("obj", obj), zap.String("key", key), zap.Error(err))
			}

		},
	}, cache.Indexers{})

	return NewController(logger, queue, indexer, informer, typespecimen, typename, workchan)
}
