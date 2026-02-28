package pkg

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
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
		return strings.TrimSpace(strings.TrimSuffix(v, ";"))
	} else {
		return ""
	}
}

func getObjectMetaAndKind(o runtime.Object) (metav1.Object, schema.ObjectKind) {
	return o.(metav1.ObjectMetaAccessor).GetObjectMeta(), o.GetObjectKind()
}

func getResourceIdentifier(om metav1.Object, ok schema.ObjectKind) resourceIdentifier {
	return resourceIdentifier(fmt.Sprintf("%s/%s/%s", ok.GroupVersionKind(), om.GetNamespace(), om.GetName()))
}

func kindFromObject(obj runtime.Object) string {
	switch obj.(type) {
	case *appsv1.Deployment:
		return "Deployment"
	case *appsv1.DaemonSet:
		return "DaemonSet"
	case *appsv1.StatefulSet:
		return "StatefulSet"
	default:
		return "Unknown"
	}
}
