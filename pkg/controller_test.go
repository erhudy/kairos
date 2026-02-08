package pkg

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// --- TestSynchronize ---

func TestSynchronize(t *testing.T) {
	t.Parallel()

	t.Run("object exists with cron annotation sends RESOURCE_CHANGE", func(t *testing.T) {
		dep := &appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-dep",
				Namespace: "default",
				Annotations: map[string]string{
					CRON_PATTERN_KEY: "0 0 * * *",
				},
			},
		}

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		err := indexer.Add(dep)
		require.NoError(t, err)

		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan)

		err = c.synchronize("default/my-dep")
		require.NoError(t, err)

		require.Len(t, workchan, 1)
		oasa := <-workchan
		require.Equal(t, RESOURCE_CHANGE, oasa.action)

		// Verify the object was stored in objectMap
		_, loaded := c.objectMap.Load("default/my-dep")
		require.True(t, loaded)
	})

	t.Run("object exists without cron annotation does not send to workchan", func(t *testing.T) {
		dep := &appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "no-cron",
				Namespace: "default",
			},
		}

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		err := indexer.Add(dep)
		require.NoError(t, err)

		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan)

		err = c.synchronize("default/no-cron")
		require.NoError(t, err)
		require.Len(t, workchan, 0)
	})

	t.Run("object deleted and found in objectMap sends RESOURCE_DELETE", func(t *testing.T) {
		dep := &appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "deleted-dep",
				Namespace: "default",
				Annotations: map[string]string{
					CRON_PATTERN_KEY: "0 0 * * *",
				},
			},
		}

		// Empty indexer (object doesn't exist)
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan)

		// Pre-populate objectMap to simulate that we previously saw this object
		c.objectMap.Store("default/deleted-dep", dep)

		err := c.synchronize("default/deleted-dep")
		require.NoError(t, err)

		require.Len(t, workchan, 1)
		oasa := <-workchan
		require.Equal(t, RESOURCE_DELETE, oasa.action)

		// Verify the object was removed from objectMap
		_, loaded := c.objectMap.Load("default/deleted-dep")
		require.False(t, loaded)
	})

	t.Run("object deleted and not in objectMap returns error", func(t *testing.T) {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan)

		err := c.synchronize("default/nonexistent")
		require.Error(t, err)
		require.Contains(t, err.Error(), "did not exist")
		require.Len(t, workchan, 0)
	})

	t.Run("indexer returns error", func(t *testing.T) {
		workchan := make(chan ObjectAndSchedulerAction, 10)
		fi := &fakeErrorIndexer{err: fmt.Errorf("indexer failure")}
		c := NewController(zap.NewNop(), workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()), fi, nil, &appsv1.Deployment{}, "deployments", workchan)

		err := c.synchronize("default/anything")
		require.Error(t, err)
		require.Contains(t, err.Error(), "indexer failure")
		require.Len(t, workchan, 0)
	})

	t.Run("GVK is set correctly for DaemonSet", func(t *testing.T) {
		ds := &appsv1.DaemonSet{
			TypeMeta: metav1.TypeMeta{Kind: "DaemonSet", APIVersion: "apps/v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-ds",
				Namespace: "kube-system",
				Annotations: map[string]string{
					CRON_PATTERN_KEY: "0 0 * * *",
				},
			},
		}

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		err := indexer.Add(ds)
		require.NoError(t, err)

		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()), indexer, nil, &appsv1.DaemonSet{}, "daemonsets", workchan)

		err = c.synchronize("kube-system/my-ds")
		require.NoError(t, err)

		require.Len(t, workchan, 1)
		oasa := <-workchan
		require.Equal(t, RESOURCE_CHANGE, oasa.action)
		require.Equal(t, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"}, oasa.obj.GetObjectKind().GroupVersionKind())
	})

	t.Run("GVK is set correctly for StatefulSet", func(t *testing.T) {
		ss := &appsv1.StatefulSet{
			TypeMeta: metav1.TypeMeta{Kind: "StatefulSet", APIVersion: "apps/v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "my-ss",
				Namespace: "prod",
				Annotations: map[string]string{
					CRON_PATTERN_KEY: "0 6 * * *",
				},
			},
		}

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		err := indexer.Add(ss)
		require.NoError(t, err)

		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter()), indexer, nil, &appsv1.StatefulSet{}, "statefulsets", workchan)

		err = c.synchronize("prod/my-ss")
		require.NoError(t, err)

		require.Len(t, workchan, 1)
		oasa := <-workchan
		require.Equal(t, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}, oasa.obj.GetObjectKind().GroupVersionKind())
	})
}

// --- TestHandleErr ---

func TestHandleErr(t *testing.T) {
	t.Parallel()

	t.Run("no error forgets the key", func(t *testing.T) {
		queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
		c := &Controller{
			logger:    zap.NewNop(),
			queue:     queue,
			objectMap: &sync.Map{},
		}

		// Simulate some prior requeues
		queue.AddRateLimited("test-key")
		queue.AddRateLimited("test-key")

		c.handleErr(nil, "test-key")

		// After Forget, NumRequeues should be 0
		require.Equal(t, 0, queue.NumRequeues("test-key"))
	})

	t.Run("error with requeues < 5 re-enqueues", func(t *testing.T) {
		queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
		c := &Controller{
			logger:    zap.NewNop(),
			queue:     queue,
			objectMap: &sync.Map{},
		}

		c.handleErr(fmt.Errorf("test error"), "retry-key")

		// Should have been re-enqueued
		require.Equal(t, 1, queue.NumRequeues("retry-key"))
	})

	t.Run("error with requeues >= 5 drops the item", func(t *testing.T) {
		queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
		c := &Controller{
			logger:    zap.NewNop(),
			queue:     queue,
			objectMap: &sync.Map{},
		}

		// Simulate 5 prior requeues
		for i := 0; i < 5; i++ {
			queue.AddRateLimited("drop-key")
		}
		require.Equal(t, 5, queue.NumRequeues("drop-key"))

		c.handleErr(fmt.Errorf("persistent error"), "drop-key")

		// After exceeding the retry limit, the key should be forgotten (NumRequeues resets)
		require.Equal(t, 0, queue.NumRequeues("drop-key"))
	})
}

// fakeErrorIndexer implements cache.Indexer but returns an error from GetByKey.
type fakeErrorIndexer struct {
	cache.Indexer
	err error
}

func (f *fakeErrorIndexer) GetByKey(key string) (interface{}, bool, error) {
	return nil, false, f.err
}
