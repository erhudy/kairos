package pkg

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// syncWithAck runs c.synchronize(key), replying with ackErr on the scheduler ack channel for
// the single action it sends. It returns the error from synchronize and the action sent.
func syncWithAck(t *testing.T, workchan <-chan ObjectAndSchedulerAction, c *Controller, key string, ackErr error) (ObjectAndSchedulerAction, error) {
	t.Helper()

	type result struct{ err error }
	done := make(chan result, 1)
	go func() { done <- result{c.synchronize(key)} }()

	var oasa ObjectAndSchedulerAction
	select {
	case oasa = <-workchan:
		oasa.errCh <- ackErr
	case <-time.After(2 * time.Second):
		t.Fatal("synchronize did not send an action")
	}

	select {
	case r := <-done:
		return oasa, r.err
	case <-time.After(2 * time.Second):
		t.Fatal("synchronize did not return after ack")
	}
	return ObjectAndSchedulerAction{}, nil
}

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
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan, nil)

		oasa, err := syncWithAck(t, workchan, c, "default/my-dep", nil)
		require.NoError(t, err)

		require.Equal(t, RESOURCE_CHANGE, oasa.action)

		// Verify the object was stored in objectMap
		_, loaded := c.objectMap.Load("default/my-dep")
		require.True(t, loaded)
	})
	t.Run("object exists without cron annotation sends RESOURCE_DELETE", func(t *testing.T) {
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
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan, nil)

		oasa, err := syncWithAck(t, workchan, c, "default/no-cron", nil)
		require.NoError(t, err)
		require.Equal(t, RESOURCE_DELETE, oasa.action)

		// Verify the unannotated object was not stored in objectMap
		_, loaded := c.objectMap.Load("default/no-cron")
		require.False(t, loaded)
	})

	t.Run("annotation removal drops stashed object and sends RESOURCE_DELETE", func(t *testing.T) {
		dep := &appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "losing-cron",
				Namespace: "default",
			},
		}

		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		err := indexer.Add(dep)
		require.NoError(t, err)

		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan, nil)

		// Pre-populate objectMap to simulate that the object was previously annotated
		c.objectMap.Store("default/losing-cron", dep)

		oasa, err := syncWithAck(t, workchan, c, "default/losing-cron", nil)
		require.NoError(t, err)
		require.Equal(t, RESOURCE_DELETE, oasa.action)

		// Verify the stale entry was removed from objectMap
		_, loaded := c.objectMap.Load("default/losing-cron")
		require.False(t, loaded)
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
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan, nil)

		// Pre-populate objectMap to simulate that we previously saw this object
		c.objectMap.Store("default/deleted-dep", dep)

		oasa, err := syncWithAck(t, workchan, c, "default/deleted-dep", nil)
		require.NoError(t, err)

		require.Equal(t, RESOURCE_DELETE, oasa.action)

		// Verify the object was removed from objectMap
		_, loaded := c.objectMap.Load("default/deleted-dep")
		require.False(t, loaded)
	})

	t.Run("object deleted and not in objectMap is a no-op", func(t *testing.T) {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})

		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan, nil)

		err := c.synchronize("default/nonexistent")
		require.NoError(t, err)
		require.Len(t, workchan, 0)
	})

	t.Run("indexer returns error", func(t *testing.T) {
		workchan := make(chan ObjectAndSchedulerAction, 10)
		fi := &fakeErrorIndexer{err: fmt.Errorf("indexer failure")}
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), fi, nil, &appsv1.Deployment{}, "deployments", workchan, nil)

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
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), indexer, nil, &appsv1.DaemonSet{}, "daemonsets", workchan, nil)

		oasa, err := syncWithAck(t, workchan, c, "kube-system/my-ds", nil)
		require.NoError(t, err)

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
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), indexer, nil, &appsv1.StatefulSet{}, "statefulsets", workchan, nil)

		oasa, err := syncWithAck(t, workchan, c, "prod/my-ss", nil)
		require.NoError(t, err)

		require.Equal(t, schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}, oasa.obj.GetObjectKind().GroupVersionKind())
	})
}

// --- TestSynchronizeAckPropagation ---

func TestSynchronizeAckPropagation(t *testing.T) {
	t.Parallel()

	annotatedDep := func(name string) *appsv1.Deployment {
		return &appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   "default",
				Annotations: map[string]string{CRON_PATTERN_KEY: "0 0 * * *"},
			},
		}
	}

	t.Run("scheduler ack error propagates from a change reconcile", func(t *testing.T) {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		require.NoError(t, indexer.Add(annotatedDep("flaky")))

		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan, nil)

		oasa, err := syncWithAck(t, workchan, c, "default/flaky", fmt.Errorf("gocron exploded"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "gocron exploded")
		require.Equal(t, RESOURCE_CHANGE, oasa.action)
	})

	t.Run("failed delete keeps the stashed object for retry", func(t *testing.T) {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan, nil)

		c.objectMap.Store("default/gone", annotatedDep("gone"))

		oasa, err := syncWithAck(t, workchan, c, "default/gone", fmt.Errorf("delete failed"))
		require.Error(t, err)
		require.Contains(t, err.Error(), "delete failed")
		require.Equal(t, RESOURCE_DELETE, oasa.action)

		// the entry must survive so a retry can re-attempt the delete
		_, loaded := c.objectMap.Load("default/gone")
		require.True(t, loaded)
	})

	t.Run("successful delete removes the stashed object", func(t *testing.T) {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan, nil)

		c.objectMap.Store("default/gone2", annotatedDep("gone2"))

		_, err := syncWithAck(t, workchan, c, "default/gone2", nil)
		require.NoError(t, err)

		_, loaded := c.objectMap.Load("default/gone2")
		require.False(t, loaded)
	})

	t.Run("shutdown abandons waiting for an ack", func(t *testing.T) {
		indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
		require.NoError(t, indexer.Add(annotatedDep("hang")))

		workchan := make(chan ObjectAndSchedulerAction, 10)
		c := NewController(zap.NewNop(), workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]()), indexer, nil, &appsv1.Deployment{}, "deployments", workchan, nil)

		stopCh := make(chan struct{})
		close(stopCh)
		c.stopCh = stopCh

		// nobody replies on the ack channel; synchronize must return via stopCh
		done := make(chan error, 1)
		go func() { done <- c.synchronize("default/hang") }()

		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			t.Fatal("synchronize did not return after shutdown")
		}
	})
}

// --- TestHandleErr ---

func TestHandleErr(t *testing.T) {
	t.Parallel()

	t.Run("no error forgets the key", func(t *testing.T) {
		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
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
		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
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
		queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())
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
