package pkg

import (
	"context"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// RestartResult holds information about a restart operation.
type RestartResult struct {
	// Generation is the resource's Generation after the restart patch was applied.
	Generation int64
}

// RestartResource patches the PodTemplateSpec of the given workload to trigger a rolling restart.
// It returns the observed generation after the update.
func RestartResource(ctx context.Context, clientset kubernetes.Interface, kind, namespace, name string) (*RestartResult, error) {
	now := time.Now().Format(LAST_RESTARTED_AT_TIME_FORMAT)

	switch kind {
	case "Deployment":
		obj, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("error getting Deployment %s/%s: %w", namespace, name, err)
		}
		if obj.Spec.Template.Annotations == nil {
			obj.Spec.Template.Annotations = make(map[string]string)
		}
		obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY] = now
		updated, err := clientset.AppsV1().Deployments(namespace).Update(ctx, obj, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("error updating Deployment %s/%s: %w", namespace, name, err)
		}
		return &RestartResult{Generation: updated.Generation}, nil

	case "DaemonSet":
		obj, err := clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("error getting DaemonSet %s/%s: %w", namespace, name, err)
		}
		if obj.Spec.Template.Annotations == nil {
			obj.Spec.Template.Annotations = make(map[string]string)
		}
		obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY] = now
		updated, err := clientset.AppsV1().DaemonSets(namespace).Update(ctx, obj, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("error updating DaemonSet %s/%s: %w", namespace, name, err)
		}
		return &RestartResult{Generation: updated.Generation}, nil

	case "StatefulSet":
		obj, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("error getting StatefulSet %s/%s: %w", namespace, name, err)
		}
		if obj.Spec.Template.Annotations == nil {
			obj.Spec.Template.Annotations = make(map[string]string)
		}
		obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY] = now
		updated, err := clientset.AppsV1().StatefulSets(namespace).Update(ctx, obj, metav1.UpdateOptions{})
		if err != nil {
			return nil, fmt.Errorf("error updating StatefulSet %s/%s: %w", namespace, name, err)
		}
		return &RestartResult{Generation: updated.Generation}, nil

	default:
		return nil, fmt.Errorf("unsupported resource kind: %s", kind)
	}
}

