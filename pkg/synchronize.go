package pkg

import (
	"fmt"

	"go.uber.org/zap"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// synchronize is the business logic of the controller. In this controller it simply prints
// information about the pod to stdout. In case an error happened, it has to simply return the error.
// The retry logic should not be part of the business logic.
func (c *Controller) synchronize(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		c.logger.Error("failed fetching object from store", zap.String("key", key), zap.Error(err))
		return err
	}

	// because of https://github.com/kubernetes/kubernetes/issues/80609 the GVK is purged on decode,
	// so I re-add it here so that it's accessible for the rest of this section
	ok := obj.(runtime.Object).GetObjectKind()
	switch obj.(type) {
	case *appsv1.DaemonSet:
		ok.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"})
	case *appsv1.Deployment:
		ok.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
	case *appsv1.StatefulSet:
		ok.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"})
	default:
		panic(fmt.Errorf("got object of type %t in synchronize which should be impossible", obj))
	}

	if !exists {
		c.logger.Debug("object does not exist anymore", zap.String("key", key))
		c.workchan <- ObjectAndSchedulerAction{action: SCHEDULER_DELETE, obj: obj.(runtime.Object)}
	} else {
		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		if err != nil {
			return err
		}
		om := obj.(metav1.ObjectMetaAccessor).GetObjectMeta()

		if !careAboutThisObject(om) {
			c.logger.Debug("don't care about object", zap.String("namespace", om.GetNamespace()), zap.String("name", om.GetName()))
			return nil
		}

		c.logger.Info("observed change for object", zap.String("key", key), zap.String("gvk", ok.GroupVersionKind().String()))
		c.workchan <- ObjectAndSchedulerAction{action: SCHEDULER_UPSERT, obj: obj.(runtime.Object)}
	}
	return nil
}
