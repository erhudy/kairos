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
	anns := om.GetAnnotations()
	if _, ok := anns[CRON_PATTERN_KEY]; ok {
		return true
	}
	v, ok := anns[RESTART_AFTER_KEY]
	return ok && strings.TrimSpace(v) != ""
}

// canonicalKindForRef maps a lowercase kind string from a restart-after annotation
// to the canonical Kind used in resource identifiers.
func canonicalKindForRef(kind string) (string, bool) {
	switch strings.ToLower(kind) {
	case "deployment":
		return "Deployment", true
	case "daemonset":
		return "DaemonSet", true
	case "statefulset":
		return "StatefulSet", true
	default:
		return "", false
	}
}

// hasChainAnnotations reports whether the object declares any chain predecessors.
func hasChainAnnotations(om metav1.Object) bool {
	return strings.TrimSpace(om.GetAnnotations()[RESTART_AFTER_KEY]) != ""
}

// parsePredecessorRefs parses the restart-after annotation into workload refs.
// Entries are comma/semicolon-separated, each kind/name (same namespace as om)
// or kind/namespace/name with kind in deployment|daemonset|statefulset.
func parsePredecessorRefs(om metav1.Object) ([]workloadRef, error) {
	v := strings.TrimSpace(om.GetAnnotations()[RESTART_AFTER_KEY])
	if v == "" {
		return nil, nil
	}

	parts := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ';' })
	refs := []workloadRef{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		segs := strings.Split(p, "/")
		var kind, namespace, name string
		switch len(segs) {
		case 2:
			kind, namespace, name = segs[0], om.GetNamespace(), segs[1]
		case 3:
			kind, namespace, name = segs[0], segs[1], segs[2]
		default:
			return nil, fmt.Errorf("invalid predecessor %q: expected kind/name or kind/namespace/name", p)
		}
		canonical, ok := canonicalKindForRef(kind)
		if !ok {
			return nil, fmt.Errorf("invalid predecessor %q: kind must be deployment, daemonset, or statefulset", p)
		}
		if namespace == "" || name == "" {
			return nil, fmt.Errorf("invalid predecessor %q: namespace and name must not be empty", p)
		}

		display := strings.ToLower(canonical) + "/" + name
		if namespace != om.GetNamespace() {
			display = strings.ToLower(canonical) + "/" + namespace + "/" + name
		}
		refs = append(refs, workloadRef{kind: canonical, namespace: namespace, name: name, display: display})
	}
	return refs, nil
}

// parseResourceIdentifier decomposes a resource identifier of the form
// "apps/v1, Kind=Deployment/ns/name" back into its parts.
func parseResourceIdentifier(ri resourceIdentifier) (kind, namespace, name string, ok bool) {
	s := string(ri)
	i := strings.Index(s, ", Kind=")
	if i < 0 {
		return "", "", "", false
	}
	parts := strings.Split(s[i+len(", Kind="):], "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", "", "", false
	}
	return parts[0], parts[1], parts[2], true
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

func getPodTemplateAnnotation(obj runtime.Object, key string) string {
	switch o := obj.(type) {
	case *appsv1.Deployment:
		return o.Spec.Template.Annotations[key]
	case *appsv1.DaemonSet:
		return o.Spec.Template.Annotations[key]
	case *appsv1.StatefulSet:
		return o.Spec.Template.Annotations[key]
	default:
		return ""
	}
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
