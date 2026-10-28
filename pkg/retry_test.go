package pkg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

var testRetryBackoff = wait.Backoff{Steps: 3, Duration: time.Millisecond, Factor: 1}

func TestIsTransientAPIError(t *testing.T) {
	t.Parallel()

	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	tests := []struct {
		testName  string
		err       error
		transient bool
	}{
		{"internal error (e.g. etcd leader changed)", apierrors.NewInternalError(errors.New("etcdserver: leader changed")), true},
		{"service unavailable", apierrors.NewServiceUnavailable("apiserver is shutting down"), true},
		{"too many requests", apierrors.NewTooManyRequests("slow down", 1), true},
		{"server timeout", apierrors.NewServerTimeout(gr, "patch", 1), true},
		{"gateway timeout", apierrors.NewTimeoutError("timed out", 1), true},
		{"bad gateway", apierrors.NewGenericServerResponse(502, "patch", gr, "x", "", 0, true), true},
		{"wrapped 5xx", fmt.Errorf("context: %w", apierrors.NewInternalError(errors.New("boom"))), true},
		{"connection reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, true},
		{"connection refused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, true},
		{"unexpected EOF", io.ErrUnexpectedEOF, true},
		{"per-request timeout", fmt.Errorf("Patch: %w", context.DeadlineExceeded), true},
		{"not found", apierrors.NewNotFound(gr, "x"), false},
		{"forbidden", apierrors.NewForbidden(gr, "x", errors.New("rbac")), false},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: "apps", Kind: "Deployment"}, "x", nil), false},
		{"bad request", apierrors.NewBadRequest("nope"), false},
		{"not implemented", apierrors.NewGenericServerResponse(501, "patch", gr, "x", "", 0, true), false},
		{"cancelled", context.Canceled, false},
		{"other", errors.New("something else"), false},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			require.Equal(t, tt.transient, isTransientAPIError(context.Background(), tt.err))
		})
	}

	t.Run("cancelled caller context is never transient", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		require.False(t, isTransientAPIError(ctx, apierrors.NewServiceUnavailable("down")))
	})
}

// patchFailures fails the first n deployment patches with err and records every patch body.
func patchFailures(clientset *fake.Clientset, n int, err error) (attempts func() int, bodies func() []string) {
	var mu sync.Mutex
	var count int
	var sent []string
	clientset.PrependReactor("patch", "deployments", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		count++
		sent = append(sent, string(action.(k8stesting.PatchAction).GetPatch()))
		if count <= n {
			return true, nil, err
		}
		return false, nil, nil
	})
	return func() int {
			mu.Lock()
			defer mu.Unlock()
			return count
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), sent...)
		}
}

func retryTestDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "retry-dep", Namespace: "ns1"},
	}
}

func TestRestartFuncRetriesTransientErrors(t *testing.T) {
	t.Parallel()

	dep := retryTestDeployment()
	clientset := fake.NewClientset(dep)
	attempts, bodies := patchFailures(clientset, 2, apierrors.NewServiceUnavailable("apiserver is shutting down"))

	reg := prometheus.NewRegistry()
	metrics := NewKairosMetrics()
	metrics.Register(reg)

	require.True(t, restartFunc(context.Background(), zap.NewNop(), clientset, testRetryBackoff, dep, metrics))
	require.Equal(t, 3, attempts())
	require.True(t, hasRestarted(clientset, "Deployment", "retry-dep"))

	sent := bodies()
	for _, b := range sent[1:] {
		require.Equal(t, sent[0], b)
	}

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.RestartTotal.WithLabelValues("Deployment", "ns1", "retry-dep")))
	require.Equal(t, float64(0), testutil.ToFloat64(metrics.RestartErrorsTotal.WithLabelValues("Deployment", "ns1", "retry-dep", "patch")))
}

func TestRestartFuncDoesNotRetryPermanentErrors(t *testing.T) {
	t.Parallel()

	dep := retryTestDeployment()
	clientset := fake.NewClientset(dep)
	gr := schema.GroupResource{Group: "apps", Resource: "deployments"}
	attempts, _ := patchFailures(clientset, 100, apierrors.NewForbidden(gr, "retry-dep", errors.New("rbac")))

	require.False(t, restartFunc(context.Background(), zap.NewNop(), clientset, testRetryBackoff, dep, nil))
	require.Equal(t, 1, attempts())
}

func TestRestartFuncGivesUpAfterBackoffSteps(t *testing.T) {
	t.Parallel()

	dep := retryTestDeployment()
	clientset := fake.NewClientset(dep)
	attempts, _ := patchFailures(clientset, 100, apierrors.NewInternalError(errors.New("etcdserver: leader changed")))

	reg := prometheus.NewRegistry()
	metrics := NewKairosMetrics()
	metrics.Register(reg)

	require.False(t, restartFunc(context.Background(), zap.NewNop(), clientset, testRetryBackoff, dep, metrics))
	require.Equal(t, testRetryBackoff.Steps, attempts())
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.RestartErrorsTotal.WithLabelValues("Deployment", "ns1", "retry-dep", "patch")))
}

func TestRestartFuncRetryStopsOnShutdown(t *testing.T) {
	t.Parallel()

	dep := retryTestDeployment()
	clientset := fake.NewClientset(dep)
	attempts, _ := patchFailures(clientset, 100, apierrors.NewServiceUnavailable("down"))

	ctx, cancel := context.WithCancel(context.Background())
	slow := wait.Backoff{Steps: 5, Duration: time.Hour, Factor: 1}
	done := make(chan bool, 1)
	go func() { done <- restartFunc(ctx, zap.NewNop(), clientset, slow, dep, nil) }()

	require.Eventually(t, func() bool { return attempts() == 1 }, 5*time.Second, time.Millisecond)
	cancel()
	select {
	case ok := <-done:
		require.False(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("restartFunc kept waiting out its backoff after shutdown")
	}
	require.Equal(t, 1, attempts())
}

func TestChainStepRetriesTransientFollowerFetch(t *testing.T) {
	t.Parallel()

	head := healthyDeployment("retry-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	mid := healthyStatefulSet("retry-mid", map[string]string{RESTART_AFTER_KEY: "deployment/retry-head"})

	s, clientset := newTestSchedulerWithChain(t, 5*time.Second, head, mid)
	metrics := NewKairosMetrics()
	s.metrics = metrics

	var mu sync.Mutex
	failures := 0
	clientset.PrependReactor("get", "statefulsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		if failures < 2 {
			failures++
			return true, nil, apierrors.NewServiceUnavailable("apiserver is shutting down")
		}
		return false, nil, nil
	})

	s.cron.Start()
	defer shutdownCron(s)
	s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: head})
	s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: mid})
	runAllJobs(t, s)

	require.Eventually(t, func() bool { return hasRestarted(clientset, "StatefulSet", "retry-mid") }, 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.ChainStepsTotal.WithLabelValues("StatefulSet", "ns1", "retry-mid", CHAIN_OUTCOME_COMPLETED)) == 1
	}, 5*time.Second, 10*time.Millisecond)
}

func newHTTPClientset(t *testing.T, srv *httptest.Server, timeout time.Duration, qps float32, burst int) kubernetes.Interface {
	t.Helper()
	clientset, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL, Timeout: timeout, QPS: qps, Burst: burst})
	require.NoError(t, err)
	return clientset
}

func writeDeployment(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(retryTestDeployment())
}

// Regression: rate limiter waits used to count against the timeout, so bursts of restarts failed.
func TestRestartFuncRateLimitWaitExcludedFromTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writeDeployment(w) }))
	defer srv.Close()

	// the last of 10 requests at 10 QPS waits ~900ms, well past the 200ms timeout
	timeout := 200 * time.Millisecond
	clientset := newHTTPClientset(t, srv, timeout, 10, 1)

	start := time.Now()
	var wg sync.WaitGroup
	results := make([]bool, 10)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = restartFunc(context.Background(), zap.NewNop(), clientset, testRetryBackoff, retryTestDeployment(), nil)
		}()
	}
	wg.Wait()

	require.Greater(t, time.Since(start), timeout)
	for i, ok := range results {
		require.True(t, ok, "restart %d failed", i)
	}
}

func TestRestartFuncRetriesTimedOutRequest(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		<-release
	}))
	defer srv.Close()
	defer close(release)

	clientset := newHTTPClientset(t, srv, 200*time.Millisecond, 0, 0)

	done := make(chan bool, 1)
	go func() {
		done <- restartFunc(context.Background(), zap.NewNop(), clientset, testRetryBackoff, retryTestDeployment(), nil)
	}()
	select {
	case ok := <-done:
		require.False(t, ok)
	case <-time.After(5 * time.Second):
		t.Fatal("per-request timeout was not applied")
	}
	require.Equal(t, int32(testRetryBackoff.Steps), requests.Load())
}

// client-go never retries writes that lose their connection.
func TestRestartFuncRetriesDroppedConnection(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) <= 2 {
			conn, _, err := w.(http.Hijacker).Hijack()
			require.NoError(t, err)
			_ = conn.Close()
			return
		}
		writeDeployment(w)
	}))
	defer srv.Close()

	clientset := newHTTPClientset(t, srv, 5*time.Second, 0, 0)

	require.True(t, restartFunc(context.Background(), zap.NewNop(), clientset, testRetryBackoff, retryTestDeployment(), nil))
	require.Equal(t, int32(3), requests.Load())
}
