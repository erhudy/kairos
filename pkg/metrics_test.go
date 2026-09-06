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
	errCount := testutil.ToFloat64(metrics.RestartErrorsTotal.WithLabelValues("Deployment", "ns1", "metric-dep", "patch"))
	require.Equal(t, float64(0), errCount)
}

func TestRestartFuncPatchErrorIncrementsErrorMetric(t *testing.T) {
	t.Parallel()

	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "nonexistent", Namespace: "ns1"},
	}

	// Empty clientset — Patch will fail with not found
	clientset := fake.NewClientset()
	logger := zap.NewNop()

	reg := prometheus.NewRegistry()
	metrics := NewKairosMetrics()
	metrics.Register(reg)

	restartFunc(context.Background(), logger, clientset, dep, metrics)

	// Verify patch error counter was incremented
	errCount := testutil.ToFloat64(metrics.RestartErrorsTotal.WithLabelValues("Deployment", "ns1", "nonexistent", "patch"))
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

// histogramSampleCount returns the total observations across all series of a
// histogram in reg, or 0 when the family was never gathered.
func histogramSampleCount(t *testing.T, reg *prometheus.Registry, name string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	require.NoError(t, err)
	var total uint64
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		for _, m := range f.GetMetric() {
			total += m.GetHistogram().GetSampleCount()
		}
	}
	return total
}

// TestRestartDurationOnlyObservedOnSuccess covers that the restart-latency
// histogram is not polluted by failures. It previously observed before checking
// the error, so every failed patch -- including API_CALL_TIMEOUT expiries, which
// land at the top of the range -- was folded into what reads as success latency
// (refs #41).
func TestRestartDurationOnlyObservedOnSuccess(t *testing.T) {
	t.Parallel()

	dep := &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "duration-dep", Namespace: "ns1"},
	}
	logger := zap.NewNop()

	t.Run("failed patch observes nothing", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		metrics := NewKairosMetrics()
		metrics.Register(reg)

		// empty clientset: the patch fails with not found
		require.False(t, restartFunc(context.Background(), logger, fake.NewClientset(), dep, metrics))

		require.Zero(t, histogramSampleCount(t, reg, "kairos_restart_duration_seconds"),
			"a failed patch must not be recorded as restart latency")
		require.Equal(t, float64(1),
			testutil.ToFloat64(metrics.RestartErrorsTotal.WithLabelValues("Deployment", "ns1", "duration-dep", "patch")))
	})

	t.Run("successful patch observes once", func(t *testing.T) {
		reg := prometheus.NewRegistry()
		metrics := NewKairosMetrics()
		metrics.Register(reg)

		require.True(t, restartFunc(context.Background(), logger, fake.NewClientset(dep), dep, metrics))

		require.Equal(t, uint64(1), histogramSampleCount(t, reg, "kairos_restart_duration_seconds"))
		require.Equal(t, float64(1),
			testutil.ToFloat64(metrics.RestartTotal.WithLabelValues("Deployment", "ns1", "duration-dep")))
	})
}
