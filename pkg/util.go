package pkg

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func careAboutThisObject(om metav1.Object) bool {
	for k := range om.GetAnnotations() {
		if k == CRON_PATTERN_KEY {
			return true
		}
	}
	return false
}

func getCronPattern(om metav1.Object) string {
	for k, v := range om.GetAnnotations() {
		if k == CRON_PATTERN_KEY {
			return v
		}
	}
	return ""
}

func getObjectMetaAndKind(o runtime.Object) (metav1.Object, schema.ObjectKind) {
	return o.(metav1.ObjectMetaAccessor).GetObjectMeta(), o.GetObjectKind()
}

func getJobTag(om metav1.Object, ok schema.ObjectKind) jobTag {
	return jobTag(fmt.Sprintf("%s_%s/%s", ok.GroupVersionKind(), om.GetNamespace(), om.GetName()))
}
