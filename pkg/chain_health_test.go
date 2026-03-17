package pkg

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestCheckRolloutStatusDeployment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name               string
		deployment         *appsv1.Deployment
		expectedGeneration int64
		wantDone           bool
		wantMessageContain string
	}{
		{
			name: "rollout complete",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "dep1", Namespace: "ns1"},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  2,
					UpdatedReplicas:     3,
					AvailableReplicas:   3,
				},
			},
			expectedGeneration: 2,
			wantDone:           true,
		},
		{
			name: "generation not caught up",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "dep1", Namespace: "ns1"},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  1,
					UpdatedReplicas:     3,
					AvailableReplicas:   3,
				},
			},
			expectedGeneration: 2,
			wantDone:           false,
			wantMessageContain: "observed generation",
		},
		{
			name: "updated replicas not ready",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "dep1", Namespace: "ns1"},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  2,
					UpdatedReplicas:     1,
					AvailableReplicas:   3,
				},
			},
			expectedGeneration: 2,
			wantDone:           false,
			wantMessageContain: "updated replicas",
		},
		{
			name: "available replicas not ready",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "dep1", Namespace: "ns1"},
				Spec:       appsv1.DeploymentSpec{Replicas: ptr.To(int32(3))},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  2,
					UpdatedReplicas:     3,
					AvailableReplicas:   1,
				},
			},
			expectedGeneration: 2,
			wantDone:           false,
			wantMessageContain: "available replicas",
		},
		{
			name: "nil replicas defaults to 1",
			deployment: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Name: "dep1", Namespace: "ns1"},
				Spec:       appsv1.DeploymentSpec{},
				Status: appsv1.DeploymentStatus{
					ObservedGeneration:  1,
					UpdatedReplicas:     1,
					AvailableReplicas:   1,
				},
			},
			expectedGeneration: 1,
			wantDone:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientset := fake.NewClientset(tt.deployment)
			status, err := CheckRolloutStatus(context.Background(), clientset, "Deployment", "ns1", "dep1", tt.expectedGeneration)
			require.NoError(t, err)
			require.Equal(t, tt.wantDone, status.Done)
			if tt.wantMessageContain != "" {
				require.Contains(t, status.Message, tt.wantMessageContain)
			}
		})
	}
}

func TestCheckRolloutStatusStatefulSet(t *testing.T) {
	t.Parallel()

	ss := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ss1", Namespace: "ns1"},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(int32(3))},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2,
			UpdatedReplicas:    3,
			AvailableReplicas:  3,
		},
	}

	clientset := fake.NewClientset(ss)
	status, err := CheckRolloutStatus(context.Background(), clientset, "StatefulSet", "ns1", "ss1", 2)
	require.NoError(t, err)
	require.True(t, status.Done)
}

func TestCheckRolloutStatusDaemonSet(t *testing.T) {
	t.Parallel()

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ds1", Namespace: "ns1"},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     2,
			DesiredNumberScheduled: 5,
			UpdatedNumberScheduled: 5,
			NumberAvailable:        5,
		},
	}

	clientset := fake.NewClientset(ds)
	status, err := CheckRolloutStatus(context.Background(), clientset, "DaemonSet", "ns1", "ds1", 2)
	require.NoError(t, err)
	require.True(t, status.Done)
}

func TestCheckRolloutStatusDaemonSetNotReady(t *testing.T) {
	t.Parallel()

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ds1", Namespace: "ns1"},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     2,
			DesiredNumberScheduled: 5,
			UpdatedNumberScheduled: 3,
			NumberAvailable:        5,
		},
	}

	clientset := fake.NewClientset(ds)
	status, err := CheckRolloutStatus(context.Background(), clientset, "DaemonSet", "ns1", "ds1", 2)
	require.NoError(t, err)
	require.False(t, status.Done)
	require.Contains(t, status.Message, "updated nodes")
}

func TestCheckRolloutStatusUnsupportedKind(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	_, err := CheckRolloutStatus(context.Background(), clientset, "ReplicaSet", "ns1", "rs1", 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported resource kind")
}

func TestCheckRolloutStatusNotFound(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	_, err := CheckRolloutStatus(context.Background(), clientset, "Deployment", "ns1", "nonexistent", 1)
	require.Error(t, err)
}
