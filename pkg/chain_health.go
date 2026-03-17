package pkg

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// RolloutStatus represents the result of checking a resource's rollout status.
type RolloutStatus struct {
	// Done is true if the rollout is complete.
	Done bool
	// Message describes the current status.
	Message string
}

// CheckRolloutStatus checks whether a workload's rollout is complete by verifying
// that ObservedGeneration matches the expected generation and all replicas are updated and available.
func CheckRolloutStatus(ctx context.Context, clientset kubernetes.Interface, kind, namespace, name string, expectedGeneration int64) (*RolloutStatus, error) {
	switch kind {
	case "Deployment":
		obj, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("error getting Deployment %s/%s: %w", namespace, name, err)
		}

		if obj.Status.ObservedGeneration < expectedGeneration {
			return &RolloutStatus{
				Done:    false,
				Message: fmt.Sprintf("waiting for observed generation to catch up (current: %d, expected: %d)", obj.Status.ObservedGeneration, expectedGeneration),
			}, nil
		}

		desired := int32(1)
		if obj.Spec.Replicas != nil {
			desired = *obj.Spec.Replicas
		}
		if obj.Status.UpdatedReplicas < desired {
			return &RolloutStatus{
				Done:    false,
				Message: fmt.Sprintf("waiting for updated replicas (%d/%d)", obj.Status.UpdatedReplicas, desired),
			}, nil
		}
		if obj.Status.AvailableReplicas < desired {
			return &RolloutStatus{
				Done:    false,
				Message: fmt.Sprintf("waiting for available replicas (%d/%d)", obj.Status.AvailableReplicas, desired),
			}, nil
		}
		return &RolloutStatus{Done: true, Message: "rollout complete"}, nil

	case "StatefulSet":
		obj, err := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("error getting StatefulSet %s/%s: %w", namespace, name, err)
		}

		if obj.Status.ObservedGeneration < expectedGeneration {
			return &RolloutStatus{
				Done:    false,
				Message: fmt.Sprintf("waiting for observed generation to catch up (current: %d, expected: %d)", obj.Status.ObservedGeneration, expectedGeneration),
			}, nil
		}

		desired := int32(1)
		if obj.Spec.Replicas != nil {
			desired = *obj.Spec.Replicas
		}
		if obj.Status.UpdatedReplicas < desired {
			return &RolloutStatus{
				Done:    false,
				Message: fmt.Sprintf("waiting for updated replicas (%d/%d)", obj.Status.UpdatedReplicas, desired),
			}, nil
		}
		if obj.Status.AvailableReplicas < desired {
			return &RolloutStatus{
				Done:    false,
				Message: fmt.Sprintf("waiting for available replicas (%d/%d)", obj.Status.AvailableReplicas, desired),
			}, nil
		}
		return &RolloutStatus{Done: true, Message: "rollout complete"}, nil

	case "DaemonSet":
		obj, err := clientset.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("error getting DaemonSet %s/%s: %w", namespace, name, err)
		}

		if obj.Status.ObservedGeneration < expectedGeneration {
			return &RolloutStatus{
				Done:    false,
				Message: fmt.Sprintf("waiting for observed generation to catch up (current: %d, expected: %d)", obj.Status.ObservedGeneration, expectedGeneration),
			}, nil
		}

		if obj.Status.UpdatedNumberScheduled < obj.Status.DesiredNumberScheduled {
			return &RolloutStatus{
				Done:    false,
				Message: fmt.Sprintf("waiting for updated nodes (%d/%d)", obj.Status.UpdatedNumberScheduled, obj.Status.DesiredNumberScheduled),
			}, nil
		}
		if obj.Status.NumberAvailable < obj.Status.DesiredNumberScheduled {
			return &RolloutStatus{
				Done:    false,
				Message: fmt.Sprintf("waiting for available nodes (%d/%d)", obj.Status.NumberAvailable, obj.Status.DesiredNumberScheduled),
			}, nil
		}
		return &RolloutStatus{Done: true, Message: "rollout complete"}, nil

	default:
		return nil, fmt.Errorf("unsupported resource kind: %s", kind)
	}
}
