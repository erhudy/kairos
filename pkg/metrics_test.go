package pkg

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRestartFuncIncrementsMetrics(t *testing.T) {
	t.Parallel()

	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "metric-dep", Namespace: "ns1"},
	}

	clientset := fake.NewClientset(dep)
	logger := zap.NewNop()

	reg := prometheus.NewRegistry()
	metrics := NewKairosMetrics()
	metrics.Register(reg)

	restartFunc(context.Background(), logger, clientset, dep, metrics)

	// Verify restart counter was incremented
	count := testutil.ToFloat64(metrics.RestartTotal.WithLabelValues("Deployment", "ns1", "metric-dep"))
	require.Equal(t, float64(1), count)

	// Verify duration histogram was observed (check sample count via Gather)
	families, err := reg.Gather()
	require.NoError(t, err)
	var durationFound bool
	for _, f := range families {
		if f.GetName() == "kairos_restart_duration_seconds" {
			for _, m := range f.GetMetric() {
				if m.GetHistogram().GetSampleCount() > 0 {
					durationFound = true
				}
			}
		}
	}
	require.True(t, durationFound, "expected restart duration histogram to have observations")

	// Verify no error counters were incremented
	errCount := testutil.ToFloat64(metrics.RestartErrorsTotal.WithLabelValues("Deployment", "ns1", "metric-dep", "get"))
	require.Equal(t, float64(0), errCount)
	errCount = testutil.ToFloat64(metrics.RestartErrorsTotal.WithLabelValues("Deployment", "ns1", "metric-dep", "update"))
	require.Equal(t, float64(0), errCount)
}

func TestRestartFuncGetErrorIncrementsErrorMetric(t *testing.T) {
	t.Parallel()

	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "nonexistent", Namespace: "ns1"},
	}

	// Empty clientset — Get will fail with not found
	clientset := fake.NewClientset()
	logger := zap.NewNop()

	reg := prometheus.NewRegistry()
	metrics := NewKairosMetrics()
	metrics.Register(reg)

	restartFunc(context.Background(), logger, clientset, dep, metrics)

	// Verify get error counter was incremented
	errCount := testutil.ToFloat64(metrics.RestartErrorsTotal.WithLabelValues("Deployment", "ns1", "nonexistent", "get"))
	require.Equal(t, float64(1), errCount)

	// Verify restart counter was NOT incremented
	count := testutil.ToFloat64(metrics.RestartTotal.WithLabelValues("Deployment", "ns1", "nonexistent"))
	require.Equal(t, float64(0), count)
}

func TestMetricsRegistration(t *testing.T) {
	t.Parallel()

	reg := prometheus.NewRegistry()
	metrics := NewKairosMetrics()
	metrics.Register(reg)

	// Verify all metrics are registered by gathering them
	families, err := reg.Gather()
	require.NoError(t, err)

	names := make(map[string]bool)
	for _, f := range families {
		names[f.GetName()] = true
	}

	// No families should be gathered yet (no observations), but registration should not panic
	// Let's make some observations and verify
	metrics.RestartTotal.WithLabelValues("Deployment", "ns1", "test").Inc()
	metrics.TrackedResources.WithLabelValues("Deployment").Set(1)
	metrics.ScheduledJobs.WithLabelValues("Deployment").Set(2)
	metrics.QueueDepth.WithLabelValues("deployments").Set(0)
	metrics.SyncErrorsTotal.WithLabelValues("deployments").Inc()

	families, err = reg.Gather()
	require.NoError(t, err)

	names = make(map[string]bool)
	for _, f := range families {
		names[f.GetName()] = true
	}

	require.True(t, names["kairos_restart_total"])
	require.True(t, names["kairos_tracked_resources"])
	require.True(t, names["kairos_scheduled_jobs"])
	require.True(t, names["kairos_queue_depth"])
	require.True(t, names["kairos_sync_errors_total"])
}
