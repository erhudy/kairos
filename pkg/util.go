package pkg

import (
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func careAboutThisObject(om metav1.Object) bool {
	_, ok := om.GetAnnotations()[CRON_PATTERN_KEY]
	return ok
}

func getCronPatternString(om metav1.Object) string {
	v, ok := om.GetAnnotations()[CRON_PATTERN_KEY]
	if ok {
		return v
	} else {
		return ""
	}
}

func getTimeLocation(om metav1.Object) (*time.Location, error) {
	v, ok := om.GetAnnotations()[TIME_ZONE_KEY]
	timezone := "UTC"
	if ok {
		timezone = v
	}
	location, err := time.LoadLocation(timezone)
	if err != nil {
		return nil, err
	}
	return location, nil
}

func getObjectMetaAndKind(o runtime.Object) (metav1.Object, schema.ObjectKind) {
	return o.(metav1.ObjectMetaAccessor).GetObjectMeta(), o.GetObjectKind()
}

func getResourceIdentifier(om metav1.Object, ok schema.ObjectKind) resourceIdentifier {
	return resourceIdentifier(fmt.Sprintf("%s/%s/%s", ok.GroupVersionKind(), om.GetNamespace(), om.GetName()))
}
