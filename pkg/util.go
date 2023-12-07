package pkg

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func careAboutThisObject(om metav1.Object) bool {
	_, ok := om.GetAnnotations()[CRON_PATTERN_KEY]
	return ok
}

func getCronPattern(om metav1.Object) cronPattern {
	v, ok := om.GetAnnotations()[CRON_PATTERN_KEY]
	if ok {
		return cronPattern(v)
	} else {
		return cronPattern("")
	}
}

func getObjectMetaAndKind(o runtime.Object) (metav1.Object, schema.ObjectKind) {
	return o.(metav1.ObjectMetaAccessor).GetObjectMeta(), o.GetObjectKind()
}

func getResourceIdentifier(om metav1.Object, ok schema.ObjectKind) resourceIdentifier {
	return resourceIdentifier(fmt.Sprintf("%s/%s/%s", ok.GroupVersionKind(), om.GetNamespace(), om.GetName()))
}
