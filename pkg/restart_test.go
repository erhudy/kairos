package pkg

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRestartResource(t *testing.T) {
	t.Parallel()

	t.Run("deployment", func(t *testing.T) {
		dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "dep1", Namespace: "ns1"}}
		clientset := fake.NewClientset(dep)
		result, err := RestartResource(context.Background(), clientset, "Deployment", "ns1", "dep1")
		require.NoError(t, err)
		require.NotNil(t, result)
	})

	t.Run("daemonset", func(t *testing.T) {
		ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "ds1", Namespace: "ns1"}}
		clientset := fake.NewClientset(ds)
		result, err := RestartResource(context.Background(), clientset, "DaemonSet", "ns1", "ds1")
		require.NoError(t, err)
		require.NotNil(t, result)
	})

	t.Run("statefulset", func(t *testing.T) {
		ss := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "ss1", Namespace: "ns1"}}
		clientset := fake.NewClientset(ss)
		result, err := RestartResource(context.Background(), clientset, "StatefulSet", "ns1", "ss1")
		require.NoError(t, err)
		require.NotNil(t, result)
	})
}

func TestRestartResourceUnsupportedKind(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	_, err := RestartResource(context.Background(), clientset, "ReplicaSet", "ns1", "rs1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported resource kind")
}

func TestRestartResourceNotFound(t *testing.T) {
	t.Parallel()

	clientset := fake.NewClientset()
	_, err := RestartResource(context.Background(), clientset, "Deployment", "ns1", "nonexistent")
	require.Error(t, err)
}
