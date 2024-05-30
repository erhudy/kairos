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
	c.logger.Debug("synchronize loading key", zap.String("key", key))
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		c.logger.Error("failed fetching object from store", zap.String("key", key), zap.Error(err))
		return fmt.Errorf("error in synchronize: %w", err)
	}

	if !exists {
		c.logger.Debug("object does not exist anymore", zap.String("key", key))

		mapObj, ok := c.objectMap.LoadAndDelete(key)
		if ok {
			c.workchan <- ObjectAndSchedulerAction{action: RESOURCE_DELETE, obj: mapObj.(runtime.Object)}
		} else {
			return fmt.Errorf("synchronize was asked to delete object for key %s that did not exist", key)
		}
	} else {
		// because of https://github.com/kubernetes/kubernetes/issues/80609 the GVK is purged on decode,
		// so I re-add it here so that it's accessible for the rest of this section
		objk := obj.(runtime.Object).GetObjectKind()
		switch obj.(type) {
		case *appsv1.DaemonSet:
			objk.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "DaemonSet"})
		case *appsv1.Deployment:
			objk.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"})
		case *appsv1.StatefulSet:
			objk.SetGroupVersionKind(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"})
		default:
			panic(fmt.Errorf("got object of type %t in synchronize which should be impossible", obj))
		}

		// stash the runtime object in the object map so that we can pull it back out at delete time,
		// because the object passed into the queue when deleted is nil and we can no longer retrieve the
		// required information from it
		c.objectMap.Store(key, obj)

		// Note that you also have to check the uid if you have a local controlled resource, which
		// is dependent on the actual instance, to detect that a Pod was recreated with the same name
		// if err != nil {
		// 	return err
		// }
		objm := obj.(metav1.ObjectMetaAccessor).GetObjectMeta()

		if !careAboutThisObject(objm) {
			c.logger.Debug("don't care about object", zap.String("namespace", objm.GetNamespace()), zap.String("name", objm.GetName()))
			return nil
		}

		c.logger.Info("observed change for object", zap.String("key", key), zap.String("gvk", objk.GroupVersionKind().String()))
		c.workchan <- ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: obj.(runtime.Object)}
	}
	return nil
}
