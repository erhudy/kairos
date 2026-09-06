package pkg

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-co-op/gocron"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/robfig/cron/v3"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestScheduler creates a Scheduler suitable for unit tests.
func newTestScheduler(t *testing.T, objects ...runtime.Object) (*Scheduler, *fake.Clientset) {
	t.Helper()
	return newTestSchedulerWithLookback(t, 0, objects...)
}

func newTestSchedulerWithLookback(t *testing.T, lookback time.Duration, objects ...runtime.Object) (*Scheduler, *fake.Clientset) {
	t.Helper()
	clientset := fake.NewClientset(objects...)
	logger := zap.NewNop()
	tz, err := time.LoadLocation("")
	require.NoError(t, err)
	ch := make(chan ObjectAndSchedulerAction, 10)
	s := NewScheduler(tz, logger, ch, clientset, nil, 0, lookback, 10*time.Minute)
	return s, clientset
}

// newTestSchedulerWithChain creates a Scheduler with chain support tuned for
// fast unit tests: short timeout and health-poll interval.
func newTestSchedulerWithChain(t *testing.T, chainTimeout time.Duration, objects ...runtime.Object) (*Scheduler, *fake.Clientset) {
	t.Helper()
	s, clientset := newTestSchedulerWithLookback(t, 0, objects...)
	s.chainTimeout = chainTimeout
	s.chainPollInterval = 10 * time.Millisecond
	return s, clientset
}

// --- TestRestartFunc ---

func TestRestartFunc(t *testing.T) {
	t.Parallel()

	tests := []struct {
		testName string
		object   runtime.Object
	}{
		{
			testName: "deployment",
			object: &appsv1.Deployment{
				TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{Name: "dep1", Namespace: "ns1"},
			},
		},
		{
			testName: "daemonset",
			object: &appsv1.DaemonSet{
				TypeMeta:   metav1.TypeMeta{Kind: "DaemonSet", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{Name: "ds1", Namespace: "ns1"},
			},
		},
		{
			testName: "statefulset",
			object: &appsv1.StatefulSet{
				TypeMeta:   metav1.TypeMeta{Kind: "StatefulSet", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{Name: "ss1", Namespace: "ns1"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			clientset := fake.NewClientset(tt.object)
			logger := zap.NewNop()

			// Truncate to second precision since RFC3339 drops sub-second
			startTime := time.Now().Truncate(time.Second)
			restartFunc(context.Background(), logger, clientset, tt.object, nil)

			// Retrieve the object and verify the annotation was set
			var obj runtime.Object
			var err error
			om, _ := getObjectMetaAndKind(tt.object)
			ns := om.GetNamespace()
			name := om.GetName()

			switch tt.object.(type) {
			case *appsv1.Deployment:
				obj, err = clientset.AppsV1().Deployments(ns).Get(context.TODO(), name, metav1.GetOptions{})
			case *appsv1.DaemonSet:
				obj, err = clientset.AppsV1().DaemonSets(ns).Get(context.TODO(), name, metav1.GetOptions{})
			case *appsv1.StatefulSet:
				obj, err = clientset.AppsV1().StatefulSets(ns).Get(context.TODO(), name, metav1.GetOptions{})
			}
			require.NoError(t, err)

			anns := reflect.Indirect(reflect.ValueOf(obj)).FieldByName("Spec").FieldByName("Template").FieldByName("Annotations").Interface().(map[string]string)
			ann, ok := anns[CRON_LAST_RESTARTED_AT_KEY]
			require.True(t, ok, "expected annotation %s to be set", CRON_LAST_RESTARTED_AT_KEY)
			parsed, err := time.Parse(LAST_RESTARTED_AT_TIME_FORMAT, ann)
			require.NoError(t, err)
			require.WithinRange(t, parsed, startTime, time.Now().Add(time.Second))
		})
	}
}

func TestRestartFuncHandlesUnsupportedType(t *testing.T) {
	t.Parallel()

	unsupported := &appsv1.ReplicaSet{
		TypeMeta:   metav1.TypeMeta{Kind: "ReplicaSet", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: "rs1", Namespace: "ns1"},
	}
	clientset := fake.NewClientset(unsupported)
	logger := zap.NewNop()

	require.NotPanics(t, func() {
		restartFunc(context.Background(), logger, clientset, unsupported, nil)
	})
}

// --- TestReconcileJobsForResource ---

func TestReconcileJobsForResource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		testName        string
		object          runtime.Object
		expectedJobsLen int
		expectErr       bool
	}{
		{
			testName: "new resource with single pattern",
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dep1",
					Namespace: "ns1",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "0 0 * * *",
					},
				},
			},
			expectedJobsLen: 1,
		},
		{
			testName: "new resource with multiple semicolon-separated patterns",
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dep2",
					Namespace: "ns1",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "0 0 * * *;30 12 * * 1-5",
					},
				},
			},
			expectedJobsLen: 2,
		},
		{
			testName: "empty pattern returns no jobs",
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dep3",
					Namespace: "ns1",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "",
					},
				},
			},
			expectedJobsLen: 0,
		},
		{
			testName: "TZ-prefixed pattern",
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dep4",
					Namespace: "ns1",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "TZ=America/New_York 0 9 * * 1-5",
					},
				},
			},
			expectedJobsLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			s, _ := newTestScheduler(t, tt.object)
			s.cron.StartAsync()
			defer s.cron.Stop()

			err := s.reconcileJobsForResource(tt.object)
			if tt.expectErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)

			om, ok := getObjectMetaAndKind(tt.object)
			ri := getResourceIdentifier(om, ok)

			if tt.expectedJobsLen == 0 {
				// Either no entry or an empty map
				raw, loaded := s.resourceMap.Load(ri)
				if loaded {
					m := raw.(*resourceMapEntry)
					require.Empty(t, m.jobs)
				}
			} else {
				raw, loaded := s.resourceMap.Load(ri)
				require.True(t, loaded)
				m := raw.(*resourceMapEntry)
				require.Len(t, m.jobs, tt.expectedJobsLen)
			}
		})
	}
}

func TestReconcileJobsPatternChange(t *testing.T) {
	t.Parallel()

	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dep-change",
			Namespace: "ns1",
			Annotations: map[string]string{
				CRON_PATTERN_KEY: "0 0 * * *;30 6 * * *",
			},
		},
	}
	s, _ := newTestScheduler(t, dep)
	s.cron.StartAsync()
	defer s.cron.Stop()

	// First reconcile: adds two patterns
	err := s.reconcileJobsForResource(dep)
	require.NoError(t, err)

	om, ok := getObjectMetaAndKind(dep)
	ri := getResourceIdentifier(om, ok)
	raw, loaded := s.resourceMap.Load(ri)
	require.True(t, loaded)
	m := raw.(*resourceMapEntry)
	require.Len(t, m.jobs, 2)

	// Modify annotation: remove one, add one
	dep.Annotations[CRON_PATTERN_KEY] = "0 0 * * *;15 18 * * *"
	err = s.reconcileJobsForResource(dep)
	require.NoError(t, err)

	raw, loaded = s.resourceMap.Load(ri)
	require.True(t, loaded)
	m = raw.(*resourceMapEntry)
	require.Len(t, m.jobs, 2)
	// Should have "0 0 * * *" (unchanged) and "15 18 * * *" (new), but not "30 6 * * *"
	_, has00 := m.jobs[cronPattern("0 0 * * *")]
	_, has1518 := m.jobs[cronPattern("15 18 * * *")]
	_, has306 := m.jobs[cronPattern("30 6 * * *")]
	require.True(t, has00, "expected pattern '0 0 * * *' to still exist")
	require.True(t, has1518, "expected pattern '15 18 * * *' to be added")
	require.False(t, has306, "expected pattern '30 6 * * *' to be removed")
}

// TestReconcileJobsContinuesPastBadPattern covers that a permanently-invalid cron
// pattern does not stop the rest of the reconcile. The workqueue drops the key
// after five retries, so anything skipped here would never happen at all.
func TestReconcileJobsContinuesPastBadPattern(t *testing.T) {
	t.Parallel()

	t.Run("bad pattern does not block chain edges on the same object", func(t *testing.T) {
		head := healthyDeployment("bad-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
		mid := healthyStatefulSet("bad-mid", map[string]string{
			CRON_PATTERN_KEY:  "not a cron",
			RESTART_AFTER_KEY: "deployment/bad-head",
		})
		s, _ := newTestSchedulerWithChain(t, time.Minute, head, mid)
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(head))

		err := s.reconcileJobsForResource(mid)
		require.Error(t, err, "the invalid pattern must still be reported")
		require.NotNil(t, edgeFor(s, riOf(head), riOf(mid)),
			"a valid restart-after must be registered even when cron-pattern is invalid")
	})

	t.Run("bad pattern does not block deletion of a removed pattern", func(t *testing.T) {
		dep := &appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:        "bad-delete",
				Namespace:   "ns1",
				Annotations: map[string]string{CRON_PATTERN_KEY: "30 6 * * *"},
			},
		}
		s, _ := newTestScheduler(t, dep)
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(dep))
		ri := riOf(dep)
		raw, _ := s.resourceMap.Load(ri)
		require.Len(t, raw.(*resourceMapEntry).jobs, 1)

		// swap the old pattern for an invalid one: the old job must still go away,
		// otherwise the workload keeps restarting on a schedule the user deleted
		dep.Annotations[CRON_PATTERN_KEY] = "garbage"
		require.Error(t, s.reconcileJobsForResource(dep))

		raw, _ = s.resourceMap.Load(ri)
		entry := raw.(*resourceMapEntry)
		entry.RLock()
		defer entry.RUnlock()
		require.NotContains(t, entry.jobs, cronPattern("30 6 * * *"),
			"removed pattern must be deleted even though the new one is invalid")
		require.Empty(t, entry.jobs)
	})

	t.Run("valid patterns are added alongside an invalid one", func(t *testing.T) {
		dep := &appsv1.Deployment{
			TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bad-mixed",
				Namespace: "ns1",
				// the invalid pattern sorts first so it is hit before the valid ones
				Annotations: map[string]string{CRON_PATTERN_KEY: "garbage;0 0 * * *;15 18 * * *"},
			},
		}
		s, _ := newTestScheduler(t, dep)
		s.cron.StartAsync()
		defer s.cron.Stop()

		err := s.reconcileJobsForResource(dep)
		require.Error(t, err)
		require.Contains(t, err.Error(), "garbage")

		raw, _ := s.resourceMap.Load(riOf(dep))
		entry := raw.(*resourceMapEntry)
		entry.RLock()
		defer entry.RUnlock()
		require.Contains(t, entry.jobs, cronPattern("0 0 * * *"))
		require.Contains(t, entry.jobs, cronPattern("15 18 * * *"))
		require.Len(t, entry.jobs, 2)
	})
}

func TestReconcileJobsAnnotationRemoved(t *testing.T) {
	t.Parallel()

	tests := []struct {
		testName    string
		mutateAnnot func(dep *appsv1.Deployment)
	}{
		{
			testName: "annotation removed entirely",
			mutateAnnot: func(dep *appsv1.Deployment) {
				delete(dep.Annotations, CRON_PATTERN_KEY)
			},
		},
		{
			testName: "annotation set to empty string",
			mutateAnnot: func(dep *appsv1.Deployment) {
				dep.Annotations[CRON_PATTERN_KEY] = ""
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			dep := &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "dep-remove",
					Namespace: "ns1",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "0 0 * * *;30 12 * * *",
					},
				},
			}
			s, _ := newTestScheduler(t, dep)
			s.cron.StartAsync()
			defer s.cron.Stop()

			err := s.reconcileJobsForResource(dep)
			require.NoError(t, err)

			om, ok := getObjectMetaAndKind(dep)
			ri := getResourceIdentifier(om, ok)
			raw, loaded := s.resourceMap.Load(ri)
			require.True(t, loaded)
			m := raw.(*resourceMapEntry)
			require.Len(t, m.jobs, 2)

			tt.mutateAnnot(dep)
			err = s.reconcileJobsForResource(dep)
			require.NoError(t, err)

			_, loaded = s.resourceMap.Load(ri)
			require.False(t, loaded, "expected all jobs removed when annotation is gone")
		})
	}
}

// --- TestCreateJob ---

func TestCreateJob(t *testing.T) {
	t.Parallel()

	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cj-dep",
			Namespace: "ns1",
			Annotations: map[string]string{
				CRON_PATTERN_KEY: "* * * * *",
			},
		},
	}

	tests := []struct {
		testName  string
		pattern   cronPattern
		expectErr bool
	}{
		{
			testName: "5-field cron",
			pattern:  cronPattern("0 0 * * *"),
		},
		{
			testName: "6-field cron with seconds",
			pattern:  cronPattern("0 0 0 * * *"),
		},
		{
			testName: "TZ prefix 5-field",
			pattern:  cronPattern("TZ=UTC 0 0 * * *"),
		},
		{
			testName: "CRON_TZ prefix 5-field",
			pattern:  cronPattern("CRON_TZ=UTC 0 0 * * *"),
		},
		{
			testName: "TZ prefix 6-field",
			pattern:  cronPattern("TZ=UTC 0 0 0 * * *"),
		},
		{
			testName: "double spaces between fields",
			pattern:  cronPattern("0  0 *  * *"),
		},
		{
			testName: "tabs between fields",
			pattern:  cronPattern("0\t0\t* * *"),
		},
		{
			testName:  "invalid field count (4 fields)",
			pattern:   cronPattern("0 0 * *"),
			expectErr: true,
		},
		{
			testName:  "invalid field count (7 fields no TZ)",
			pattern:   cronPattern("0 0 0 * * * extra"),
			expectErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			s, _ := newTestScheduler(t, dep)
			s.cron.StartAsync()
			defer s.cron.Stop()

			om, ok := getObjectMetaAndKind(dep)
			ri := getResourceIdentifier(om, ok)

			err := s.createJob(tt.pattern, ri, dep)
			if tt.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				raw, loaded := s.resourceMap.Load(ri)
				require.True(t, loaded)
				m := raw.(*resourceMapEntry)
				_, exists := m.jobs[tt.pattern]
				require.True(t, exists, "expected job to be stored for pattern %s", tt.pattern)
			}
		})
	}
}

// --- TestDeleteJobsForResource ---

func TestDeleteJobsForResource(t *testing.T) {
	t.Parallel()

	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "del-dep",
			Namespace: "ns1",
			Annotations: map[string]string{
				CRON_PATTERN_KEY: "0 0 * * *;30 12 * * *",
			},
		},
	}

	t.Run("delete all jobs for resource", func(t *testing.T) {
		s, _ := newTestScheduler(t, dep)
		s.cron.StartAsync()
		defer s.cron.Stop()

		// First add jobs via reconcile
		err := s.reconcileJobsForResource(dep)
		require.NoError(t, err)

		om, ok := getObjectMetaAndKind(dep)
		ri := getResourceIdentifier(om, ok)

		raw, loaded := s.resourceMap.Load(ri)
		require.True(t, loaded)
		m := raw.(*resourceMapEntry)
		require.Len(t, m.jobs, 2)

		// Now delete all jobs
		err = s.deleteJobsForResource(dep)
		require.NoError(t, err)

		// Resource should be removed from the map
		_, loaded = s.resourceMap.Load(ri)
		require.False(t, loaded, "expected resource to be removed from resourceMap after delete")
	})

	t.Run("delete for nonexistent resource is a no-op", func(t *testing.T) {
		s, _ := newTestScheduler(t)
		s.cron.StartAsync()
		defer s.cron.Stop()

		err := s.deleteJobsForResource(dep)
		require.NoError(t, err)
	})

	t.Run("deleteJob cleans up map entry and gauge on ErrJobNotFound", func(t *testing.T) {
		clientset := fake.NewClientset(dep)
		tz, err := time.LoadLocation("")
		require.NoError(t, err)
		metrics := NewKairosMetrics()
		s := NewScheduler(tz, zap.NewNop(), make(chan ObjectAndSchedulerAction, 10), clientset, metrics, 0, 0, 10*time.Minute)
		s.cron.StartAsync()
		defer s.cron.Stop()

		om, ok := getObjectMetaAndKind(dep)
		ri := getResourceIdentifier(om, ok)

		// Simulate a stale entry: job tracked in the map but already gone from gocron.
		cp := cronPattern("0 0 * * *")
		entry := &resourceMapEntry{
			obj:         dep,
			jobs:        map[cronPattern]*gocron.Job{cp: {}},
			lastJitters: map[cronPattern]time.Duration{cp: time.Second},
		}
		s.resourceMap.Store(ri, entry)
		metrics.ScheduledJobs.WithLabelValues("Deployment").Inc()

		err = s.deleteJob(cp, ri, &gocron.Job{}, dep)
		require.NoError(t, err)

		entry.RLock()
		_, jobExists := entry.jobs[cp]
		_, jitterExists := entry.lastJitters[cp]
		entry.RUnlock()
		require.False(t, jobExists, "expected stale job entry to be removed on ErrJobNotFound")
		require.False(t, jitterExists, "expected stale jitter entry to be removed on ErrJobNotFound")

		require.Zero(t, testutil.ToFloat64(metrics.ScheduledJobs.WithLabelValues("Deployment")),
			"expected ScheduledJobs gauge not to drift on ErrJobNotFound")
	})
}

// --- TestProcessSchedulerBundle ---

func TestProcessSchedulerBundle(t *testing.T) {
	t.Parallel()

	dep := &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "psb-dep",
			Namespace: "ns1",
			Annotations: map[string]string{
				CRON_PATTERN_KEY: "0 0 * * *",
			},
		},
	}

	t.Run("RESOURCE_CHANGE dispatches to reconcile", func(t *testing.T) {
		s, _ := newTestScheduler(t, dep)
		s.cron.StartAsync()
		defer s.cron.Stop()

		s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: dep})

		om, ok := getObjectMetaAndKind(dep)
		ri := getResourceIdentifier(om, ok)
		raw, loaded := s.resourceMap.Load(ri)
		require.True(t, loaded)
		m := raw.(*resourceMapEntry)
		require.Len(t, m.jobs, 1)
	})

	t.Run("RESOURCE_DELETE dispatches to delete", func(t *testing.T) {
		s, _ := newTestScheduler(t, dep)
		s.cron.StartAsync()
		defer s.cron.Stop()

		// First add, then delete
		s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: dep})
		s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_DELETE, obj: dep})

		om, ok := getObjectMetaAndKind(dep)
		ri := getResourceIdentifier(om, ok)
		_, loaded := s.resourceMap.Load(ri)
		require.False(t, loaded)
	})
}

// --- TestParseCronExpression ---

func TestParseCronExpression(t *testing.T) {
	t.Parallel()
	utc := time.UTC

	tests := []struct {
		name      string
		pattern   cronPattern
		expectErr bool
		wantLoc   string
	}{
		{name: "5-field", pattern: "0 0 * * *", wantLoc: "UTC"},
		{name: "6-field with seconds", pattern: "0 0 0 * * *", wantLoc: "UTC"},
		{name: "TZ prefix", pattern: "TZ=America/New_York 0 9 * * 1-5", wantLoc: "America/New_York"},
		{name: "CRON_TZ prefix", pattern: "CRON_TZ=Asia/Tokyo 30 8 * * *", wantLoc: "Asia/Tokyo"},
		{name: "invalid field count", pattern: "0 0 *", expectErr: true},
		{name: "invalid TZ prefix no space", pattern: "TZ=Bad", expectErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched, loc, err := parseCronExpression(tt.pattern, utc)
			if tt.expectErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, sched)
			require.Equal(t, tt.wantLoc, loc.String())
		})
	}
}

// --- TestFindLastScheduledTimeInWindow ---

func TestFindLastScheduledTimeInWindow(t *testing.T) {
	t.Parallel()

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

	t.Run("hourly cron with firing in window", func(t *testing.T) {
		// "0 * * * *" fires at the top of every hour
		sched, err := parser.Parse("0 * * * *")
		require.NoError(t, err)

		now := time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC)
		windowStart := now.Add(-2 * time.Hour)

		last, found := findLastScheduledTimeInWindow(sched, windowStart, now)
		require.True(t, found)
		require.Equal(t, time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC), last)
	})

	t.Run("no firing in window", func(t *testing.T) {
		// "0 0 * * *" fires once per day at midnight
		sched, err := parser.Parse("0 0 * * *")
		require.NoError(t, err)

		now := time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC)
		windowStart := now.Add(-30 * time.Minute)

		_, found := findLastScheduledTimeInWindow(sched, windowStart, now)
		require.False(t, found)
	})

	t.Run("multiple firings in window returns most recent", func(t *testing.T) {
		// "*/10 * * * *" fires every 10 minutes
		sched, err := parser.Parse("*/10 * * * *")
		require.NoError(t, err)

		now := time.Date(2024, 1, 1, 10, 35, 0, 0, time.UTC)
		windowStart := now.Add(-30 * time.Minute)

		last, found := findLastScheduledTimeInWindow(sched, windowStart, now)
		require.True(t, found)
		require.Equal(t, time.Date(2024, 1, 1, 10, 30, 0, 0, time.UTC), last)
	})
}

// --- TestCheckMissedRestart ---

func TestCheckMissedRestart(t *testing.T) {
	t.Parallel()

	makeDeployment := func(lastRestartedAt string) *appsv1.Deployment {
		templateAnnotations := map[string]string{}
		if lastRestartedAt != "" {
			templateAnnotations[CRON_LAST_RESTARTED_AT_KEY] = lastRestartedAt
		}
		return &appsv1.Deployment{
			TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
			ObjectMeta: metav1.ObjectMeta{Name: "test-dep", Namespace: "ns1"},
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Annotations: templateAnnotations},
				},
			},
		}
	}

	// "0 * * * *" fires hourly; set now such that the last firing was 20 min ago (within a 30m window)
	now := time.Date(2024, 1, 1, 10, 20, 0, 0, time.UTC)
	lastScheduledTime := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	beforeLastScheduled := lastScheduledTime.Add(-5 * time.Minute).Format(LAST_RESTARTED_AT_TIME_FORMAT)
	afterLastScheduled := lastScheduledTime.Add(5 * time.Minute).Format(LAST_RESTARTED_AT_TIME_FORMAT)

	t.Run("restart triggered when no annotation", func(t *testing.T) {
		dep := makeDeployment("")
		s, clientset := newTestSchedulerWithLookback(t, 30*time.Minute, dep)

		s.checkMissedRestartAt([]cronPattern{"0 * * * *"}, dep, now)

		require.Eventually(t, func() bool {
			obj, err := clientset.AppsV1().Deployments("ns1").Get(context.TODO(), "test-dep", metav1.GetOptions{})
			if err != nil {
				return false
			}
			_, ok := obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY]
			return ok
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("restart triggered when annotation is older than last scheduled", func(t *testing.T) {
		dep := makeDeployment(beforeLastScheduled)
		s, clientset := newTestSchedulerWithLookback(t, 30*time.Minute, dep)

		s.checkMissedRestartAt([]cronPattern{"0 * * * *"}, dep, now)

		require.Eventually(t, func() bool {
			obj, err := clientset.AppsV1().Deployments("ns1").Get(context.TODO(), "test-dep", metav1.GetOptions{})
			if err != nil {
				return false
			}
			ann := obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY]
			parsed, err := time.Parse(LAST_RESTARTED_AT_TIME_FORMAT, ann)
			if err != nil {
				return false
			}
			// The restart should have updated the annotation to a time after lastScheduledTime
			return !parsed.Before(lastScheduledTime)
		}, 5*time.Second, 10*time.Millisecond)
	})

	t.Run("no restart when annotation is newer than last scheduled", func(t *testing.T) {
		dep := makeDeployment(afterLastScheduled)
		s, clientset := newTestSchedulerWithLookback(t, 30*time.Minute, dep)

		s.checkMissedRestartAt([]cronPattern{"0 * * * *"}, dep, now)

		// Give the goroutine a moment to fire if it were going to
		time.Sleep(100 * time.Millisecond)

		obj, err := clientset.AppsV1().Deployments("ns1").Get(context.TODO(), "test-dep", metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, afterLastScheduled, obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY])
	})

	t.Run("no restart when lookback is zero", func(t *testing.T) {
		dep := makeDeployment("")
		s, clientset := newTestScheduler(t, dep) // lookback=0

		s.checkMissedRestartAt([]cronPattern{"0 * * * *"}, dep, now)

		time.Sleep(100 * time.Millisecond)

		obj, err := clientset.AppsV1().Deployments("ns1").Get(context.TODO(), "test-dep", metav1.GetOptions{})
		require.NoError(t, err)
		_, hasAnn := obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY]
		require.False(t, hasAnn)
	})

	t.Run("no restart when no firing in window", func(t *testing.T) {
		dep := makeDeployment("")
		s, clientset := newTestSchedulerWithLookback(t, 5*time.Minute, dep) // only 5m window; last hourly was 20m ago

		s.checkMissedRestartAt([]cronPattern{"0 * * * *"}, dep, now)

		time.Sleep(100 * time.Millisecond)

		obj, err := clientset.AppsV1().Deployments("ns1").Get(context.TODO(), "test-dep", metav1.GetOptions{})
		require.NoError(t, err)
		_, hasAnn := obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY]
		require.False(t, hasAnn)
	})

	t.Run("no restart when firing occurred after scheduler start", func(t *testing.T) {
		dep := makeDeployment("")
		s, clientset := newTestSchedulerWithLookback(t, 30*time.Minute, dep)
		// scheduler was already running before the 10:00 firing, so the regular job owns it
		s.startTime = lastScheduledTime.Add(-5 * time.Minute)

		s.checkMissedRestartAt([]cronPattern{"0 * * * *"}, dep, now)

		time.Sleep(100 * time.Millisecond)

		obj, err := clientset.AppsV1().Deployments("ns1").Get(context.TODO(), "test-dep", metav1.GetOptions{})
		require.NoError(t, err)
		_, hasAnn := obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY]
		require.False(t, hasAnn)
	})

	t.Run("no catch-up restart when job is deleted during jitter sleep", func(t *testing.T) {
		dep := makeDeployment("")
		clientset := fake.NewClientset(dep)
		tz, err := time.LoadLocation("")
		require.NoError(t, err)
		s := NewScheduler(tz, zap.NewNop(), make(chan ObjectAndSchedulerAction, 10), clientset, nil, 500*time.Millisecond, 30*time.Minute, 10*time.Minute)
		s.cron.StartAsync()
		defer s.cron.Stop()

		om, kind := getObjectMetaAndKind(dep)
		ri := getResourceIdentifier(om, kind)
		require.NoError(t, s.createJob("0 * * * *", ri, dep))

		s.checkMissedRestartAt([]cronPattern{"0 * * * *"}, dep, now)
		// deregister immediately; the catch-up goroutine is still inside its jitter sleep
		require.NoError(t, s.deleteJobsForResource(dep))

		time.Sleep(1500 * time.Millisecond)

		obj, err := clientset.AppsV1().Deployments("ns1").Get(context.TODO(), "test-dep", metav1.GetOptions{})
		require.NoError(t, err)
		_, hasAnn := obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY]
		require.False(t, hasAnn, "expected no restart after job was deregistered during jitter sleep")
	})

	t.Run("multiple missed patterns trigger only one restart", func(t *testing.T) {
		dep := makeDeployment("")
		s, clientset := newTestSchedulerWithLookback(t, 30*time.Minute, dep)

		// both patterns fired in the window (10:00 and 10:10); expect a single catch-up
		s.checkMissedRestartAt([]cronPattern{"0 * * * *", "10 * * * *"}, dep, now)

		require.Eventually(t, func() bool {
			obj, err := clientset.AppsV1().Deployments("ns1").Get(context.TODO(), "test-dep", metav1.GetOptions{})
			if err != nil {
				return false
			}
			_, ok := obj.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY]
			return ok
		}, 5*time.Second, 10*time.Millisecond)

		// allow any (buggy) second restart goroutine time to land, then count updates
		time.Sleep(200 * time.Millisecond)
		updates := 0
		for _, action := range clientset.Actions() {
			if action.GetVerb() == "patch" && action.GetResource().Resource == "deployments" {
				updates++
			}
		}
		require.Equal(t, 1, updates, "expected exactly one catch-up restart across patterns")
	})
}

// --- TestClampJitterToSchedule ---

func TestClampJitterToSchedule(t *testing.T) {
	t.Parallel()

	mustParse := func(t *testing.T, pattern cronPattern) cron.Schedule {
		t.Helper()
		sched, _, err := parseCronExpression(pattern, time.UTC)
		require.NoError(t, err)
		return sched
	}

	// with "* * * * *" the next firing after now is 10:01:00 → 60s away → half = 30s
	now := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		pattern cronPattern
		jitter  time.Duration
		wantMax time.Duration
	}{
		{
			name:    "zero jitter returns zero",
			pattern: "* * * * *",
			jitter:  0,
			wantMax: 0,
		},
		{
			name:    "jitter below half remaining returned unchanged",
			pattern: "* * * * *",
			jitter:  10 * time.Second,
			wantMax: 10 * time.Second,
		},
		{
			name:    "jitter equal to half remaining returned unchanged",
			pattern: "* * * * *",
			jitter:  30 * time.Second,
			wantMax: 30 * time.Second,
		},
		{
			name:    "jitter above half remaining clamped to half",
			pattern: "* * * * *",
			jitter:  time.Minute,
			wantMax: 30 * time.Second,
		},
		{
			name:    "hourly schedule clamps large jitter",
			pattern: "0 * * * *", // next firing 11:00 → 3600s away → half = 1800s
			jitter:  time.Hour,
			wantMax: 30 * time.Minute,
		},
		{
			name:    "hourly schedule leaves small jitter alone",
			pattern: "0 * * * *",
			jitter:  5 * time.Minute,
			wantMax: 5 * time.Minute,
		},
		{
			name:    "TZ-prefixed pattern respects half remaining",
			pattern: "TZ=UTC * * * * *",
			jitter:  45 * time.Second,
			wantMax: 30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := clampJitterToSchedule(tt.jitter, mustParse(t, tt.pattern), now)
			require.Equal(t, tt.wantMax, result)
		})
	}

	t.Run("nil schedule falls back to original jitter", func(t *testing.T) {
		require.Equal(t, time.Minute, clampJitterToSchedule(time.Minute, nil, now))
	})

	t.Run("clamp varies with time until next firing", func(t *testing.T) {
		// at 10:00:30 the next "* * * * *" firing is 30s away → half = 15s
		lateNow := time.Date(2024, 1, 1, 10, 0, 30, 0, time.UTC)
		require.Equal(t, 15*time.Second, clampJitterToSchedule(time.Minute, mustParse(t, "* * * * *"), lateNow))
	})
}

// --- TestParseCronExpressionLocation ---

func TestParseCronExpressionLocation(t *testing.T) {
	t.Parallel()

	t.Run("TZ prefix determines firing instants", func(t *testing.T) {
		sched, _, err := parseCronExpression("TZ=America/New_York 0 9 * * *", time.UTC)
		require.NoError(t, err)
		// 2024-01-15 12:00 UTC = 07:00 in New York (EST); next 9am NY = 14:00 UTC
		from := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
		next := sched.Next(from)
		require.Equal(t, time.Date(2024, 1, 15, 14, 0, 0, 0, time.UTC), next.UTC())
	})

	t.Run("default location determines firing instants", func(t *testing.T) {
		tokyo, err := time.LoadLocation("Asia/Tokyo")
		require.NoError(t, err)
		sched, _, err := parseCronExpression("0 9 * * *", tokyo)
		require.NoError(t, err)
		// next 9am Tokyo (UTC+9) after 2024-01-15 12:00 UTC is 2024-01-16 00:00 UTC
		from := time.Date(2024, 1, 15, 12, 0, 0, 0, time.UTC)
		next := sched.Next(from)
		require.Equal(t, time.Date(2024, 1, 16, 0, 0, 0, 0, time.UTC), next.UTC())
	})
}

// --- TestSchedulerEndToEnd ---
// Tests the full flow: processSchedulerBundle creates a cron job, RunAll fires it,
// and the restart annotation is set on the object in the fake clientset.

func TestSchedulerEndToEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		testName  string
		object    runtime.Object
		namespace string
		name      string
	}{
		{
			testName: "deployment",
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name: "e2e-dep", Namespace: "ns1",
					Annotations: map[string]string{CRON_PATTERN_KEY: "* * * * *"},
				},
			},
			namespace: "ns1", name: "e2e-dep",
		},
		{
			testName: "daemonset",
			object: &appsv1.DaemonSet{
				TypeMeta: metav1.TypeMeta{Kind: "DaemonSet", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name: "e2e-ds", Namespace: "ns1",
					Annotations: map[string]string{CRON_PATTERN_KEY: "* * * * *"},
				},
			},
			namespace: "ns1", name: "e2e-ds",
		},
		{
			testName: "statefulset",
			object: &appsv1.StatefulSet{
				TypeMeta: metav1.TypeMeta{Kind: "StatefulSet", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name: "e2e-ss", Namespace: "ns1",
					Annotations: map[string]string{CRON_PATTERN_KEY: "* * * * *"},
				},
			},
			namespace: "ns1", name: "e2e-ss",
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			s, clientset := newTestScheduler(t, tt.object)
			s.cron.StartAsync()
			defer s.cron.Stop()

			startTime := time.Now().Truncate(time.Second)

			// Process the bundle (schedules the cron job)
			s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: tt.object})

			// Force-fire all scheduled jobs immediately
			s.cron.RunAll()

			// Poll until the restart annotation appears rather than using a fixed sleep
			var obj runtime.Object
			var fetchErr error
			require.Eventually(t, func() bool {
				switch tt.object.(type) {
				case *appsv1.Deployment:
					obj, fetchErr = clientset.AppsV1().Deployments(tt.namespace).Get(context.TODO(), tt.name, metav1.GetOptions{})
				case *appsv1.DaemonSet:
					obj, fetchErr = clientset.AppsV1().DaemonSets(tt.namespace).Get(context.TODO(), tt.name, metav1.GetOptions{})
				case *appsv1.StatefulSet:
					obj, fetchErr = clientset.AppsV1().StatefulSets(tt.namespace).Get(context.TODO(), tt.name, metav1.GetOptions{})
				}
				if fetchErr != nil {
					return false
				}
				anns := reflect.Indirect(reflect.ValueOf(obj)).FieldByName("Spec").FieldByName("Template").FieldByName("Annotations").Interface().(map[string]string)
				_, ok := anns[CRON_LAST_RESTARTED_AT_KEY]
				return ok
			}, 5*time.Second, 10*time.Millisecond, "expected annotation %s to be set", CRON_LAST_RESTARTED_AT_KEY)

			require.NoError(t, fetchErr)
			anns := reflect.Indirect(reflect.ValueOf(obj)).FieldByName("Spec").FieldByName("Template").FieldByName("Annotations").Interface().(map[string]string)
			ann, ok := anns[CRON_LAST_RESTARTED_AT_KEY]
			require.True(t, ok, "expected annotation %s to be set", CRON_LAST_RESTARTED_AT_KEY)

			parsed, err := time.Parse(LAST_RESTARTED_AT_TIME_FORMAT, ann)
			require.NoError(t, err)
			require.WithinRange(t, parsed, startTime, time.Now().Add(time.Second))
		})
	}
}

// --- Chain restarts ---

func riOf(obj runtime.Object) resourceIdentifier {
	om, ok := getObjectMetaAndKind(obj)
	return getResourceIdentifier(om, ok)
}

// healthyDeployment returns a Deployment whose status reports a fully landed
// rollout, so isRolloutComplete passes for it. All chain fixtures live in ns1.
func healthyDeployment(name string, anns map[string]string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1", Annotations: anns},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{UpdatedReplicas: 1, Replicas: 1, AvailableReplicas: 1},
	}
}

func healthyStatefulSet(name string, anns map[string]string) *appsv1.StatefulSet {
	replicas := int32(1)
	return &appsv1.StatefulSet{
		TypeMeta:   metav1.TypeMeta{Kind: "StatefulSet", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1", Annotations: anns},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
		Status:     appsv1.StatefulSetStatus{UpdatedReplicas: 1, ReadyReplicas: 1, CurrentRevision: "r1", UpdateRevision: "r1"},
	}
}

// unhealthyDeployment reports a rollout that never lands (no updated replicas).
func unhealthyDeployment(name string, anns map[string]string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		TypeMeta:   metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns1", Annotations: anns},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{},
	}
}

func setDeploymentHealthy(t *testing.T, clientset *fake.Clientset, ns, name string) {
	t.Helper()
	dep, err := clientset.AppsV1().Deployments(ns).Get(context.TODO(), name, metav1.GetOptions{})
	require.NoError(t, err)
	replicas := int32(1)
	if dep.Spec.Replicas != nil {
		replicas = *dep.Spec.Replicas
	}
	dep.Status = appsv1.DeploymentStatus{
		ObservedGeneration: dep.Generation,
		UpdatedReplicas:    replicas,
		Replicas:           replicas,
		AvailableReplicas:  replicas,
	}
	_, err = clientset.AppsV1().Deployments(ns).UpdateStatus(context.TODO(), dep, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func hasRestarted(clientset *fake.Clientset, kind, name string) bool {
	var anns map[string]string
	switch kind {
	case "Deployment":
		obj, err := clientset.AppsV1().Deployments("ns1").Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		anns = obj.Spec.Template.Annotations
	case "StatefulSet":
		obj, err := clientset.AppsV1().StatefulSets("ns1").Get(context.TODO(), name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		anns = obj.Spec.Template.Annotations
	}
	_, ok := anns[CRON_LAST_RESTARTED_AT_KEY]
	return ok
}

func edgeFor(s *Scheduler, predRi, followerRi resourceIdentifier) *chainEdge {
	raw, ok := s.chainMap.Load(predRi)
	if !ok {
		return nil
	}
	entry := raw.(*chainMapEntry)
	entry.RLock()
	defer entry.RUnlock()
	return entry.edges[followerRi]
}

func TestReconcileChainEdges(t *testing.T) {
	t.Parallel()

	newHeadMid := func() (*appsv1.Deployment, *appsv1.StatefulSet) {
		return healthyDeployment("chain-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"}),
			healthyStatefulSet("chain-mid", map[string]string{RESTART_AFTER_KEY: "deployment/chain-head"})
	}

	t.Run("registers edge for pure follower", func(t *testing.T) {
		head, mid := newHeadMid()
		s, _ := newTestSchedulerWithChain(t, time.Minute, head, mid)
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(head))
		require.NoError(t, s.reconcileJobsForResource(mid))

		edge := edgeFor(s, riOf(head), riOf(mid))
		require.NotNil(t, edge, "expected chain edge deployment/head -> statefulset/mid")
		require.Equal(t, chainModeHealth, edge.mode)
		require.Zero(t, edge.wait)

		raw, loaded := s.resourceMap.Load(riOf(mid))
		require.True(t, loaded, "pure follower should be tracked")
		entry := raw.(*resourceMapEntry)
		require.Empty(t, entry.jobs, "pure follower must not get cron jobs")
	})

	t.Run("stale edge removed when annotation removed", func(t *testing.T) {
		head, mid := newHeadMid()
		s, _ := newTestSchedulerWithChain(t, time.Minute, head, mid)
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(head))
		require.NoError(t, s.reconcileJobsForResource(mid))
		require.NotNil(t, edgeFor(s, riOf(head), riOf(mid)))

		delete(mid.Annotations, RESTART_AFTER_KEY)
		require.NoError(t, s.reconcileJobsForResource(mid))

		require.Nil(t, edgeFor(s, riOf(head), riOf(mid)), "edge must be removed when annotation is gone")
		_, loaded := s.resourceMap.Load(riOf(mid))
		require.False(t, loaded, "unannotated follower must be untracked entirely")
	})

	t.Run("follower delete removes its edges, predecessor delete does not", func(t *testing.T) {
		head, mid := newHeadMid()
		s, _ := newTestSchedulerWithChain(t, time.Minute, head, mid)
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(head))
		require.NoError(t, s.reconcileJobsForResource(mid))

		require.NoError(t, s.deleteJobsForResource(mid))
		require.Nil(t, edgeFor(s, riOf(head), riOf(mid)), "follower delete must drop the edge")

		require.NoError(t, s.reconcileJobsForResource(mid))
		require.NotNil(t, edgeFor(s, riOf(head), riOf(mid)))

		// the edge belongs to the follower that declared restart-after, so deleting
		// the predecessor must leave it in place; it is inert while head cannot fire
		require.NoError(t, s.deleteJobsForResource(head))
		require.NotNil(t, edgeFor(s, riOf(head), riOf(mid)),
			"predecessor delete must not orphan the follower's edge")
	})
}

// TestReconcileChainEdgesKeepsUnchangedEdgeVisible is the regression test for
// the remove-then-readd window: reconcile runs on every informer update for the
// follower, and triggerFollowers reads the map exactly once at firing time, so
// an unchanged edge that is briefly absent during reconcile silently drops the
// cascade. A reader hammers the map while the follower is reconciled in a loop
// and must never see the edge, or the predecessor's whole entry, go missing.
func TestReconcileChainEdgesKeepsUnchangedEdgeVisible(t *testing.T) {
	t.Parallel()

	head := healthyDeployment("chain-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	mid := healthyStatefulSet("chain-mid", map[string]string{RESTART_AFTER_KEY: "deployment/chain-head"})
	s, _ := newTestSchedulerWithChain(t, time.Minute, head, mid)
	require.NoError(t, s.reconcileJobsForResource(head))
	require.NoError(t, s.reconcileJobsForResource(mid))
	require.NotNil(t, edgeFor(s, riOf(head), riOf(mid)))

	var checks, edgeMissing, entryMissing atomic.Int64
	done := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-done:
				return
			default:
			}
			checks.Add(1)
			if _, ok := s.chainMap.Load(riOf(head)); !ok {
				entryMissing.Add(1)
			}
			if !s.edgeStillRegistered(riOf(head), riOf(mid)) {
				edgeMissing.Add(1)
			}
		}
	}()

	for i := 0; i < 10000; i++ {
		s.reconcileChainEdges(mid)
	}
	close(done)
	<-readerDone

	require.Positive(t, checks.Load(), "reader never ran")
	require.Zero(t, edgeMissing.Load(), "unchanged edge was observed missing during reconcile")
	require.Zero(t, entryMissing.Load(), "predecessor chainMap entry was observed missing during reconcile")
	require.NotNil(t, edgeFor(s, riOf(head), riOf(mid)))
}

// TestReconcileChainEdgesInPlaceDiff pins the rest of the in-place contract:
// a reconcile replaces the edge pointer (so in-flight steps keep a consistent
// snapshot) but keeps the edge for a predecessor that is still listed, prunes
// only the predecessors that were dropped, and drops everything when the
// annotation set becomes invalid.
func TestReconcileChainEdgesInPlaceDiff(t *testing.T) {
	t.Parallel()

	a := healthyDeployment("chain-a", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	b := healthyDeployment("chain-b", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	c := healthyDeployment("chain-c", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	mid := healthyStatefulSet("chain-mid", map[string]string{RESTART_AFTER_KEY: "deployment/chain-a, deployment/chain-b"})
	s, _ := newTestSchedulerWithChain(t, time.Minute, a, b, c, mid)
	for _, o := range []runtime.Object{a, b, c, mid} {
		require.NoError(t, s.reconcileJobsForResource(o))
	}
	before := edgeFor(s, riOf(a), riOf(mid))
	require.NotNil(t, before)
	require.NotNil(t, edgeFor(s, riOf(b), riOf(mid)))

	// swap b for c: a's edge stays (new pointer, same key), b's is pruned, c's added
	mid.Annotations[RESTART_AFTER_KEY] = "deployment/chain-a; deployment/chain-c"
	mid.Annotations[RESTART_AFTER_MODE_KEY] = CHAIN_MODE_HEALTH_PLUS_WAIT
	mid.Annotations[RESTART_AFTER_WAIT_KEY] = "30s"
	require.NoError(t, s.reconcileJobsForResource(mid))

	after := edgeFor(s, riOf(a), riOf(mid))
	require.NotNil(t, after)
	require.NotSame(t, before, after, "reconcile must replace the edge pointer, not mutate it in place")
	require.Equal(t, chainModeHealth, before.mode, "the old snapshot must be untouched")
	require.Equal(t, chainModeHealthPlusWait, after.mode)
	require.Equal(t, 30*time.Second, after.wait)
	require.Nil(t, edgeFor(s, riOf(b), riOf(mid)), "dropped predecessor's edge must be pruned")
	_, bEntry := s.chainMap.Load(riOf(b))
	require.False(t, bEntry, "emptied predecessor entry must be deleted")
	require.NotNil(t, edgeFor(s, riOf(c), riOf(mid)))

	// an invalid annotation set drops every edge, including the previously valid ones
	mid.Annotations[RESTART_AFTER_WAIT_KEY] = "not-a-duration"
	require.NoError(t, s.reconcileJobsForResource(mid))
	require.Nil(t, edgeFor(s, riOf(a), riOf(mid)))
	require.Nil(t, edgeFor(s, riOf(c), riOf(mid)))
}

// TestChainEdgeSurvivesPredecessorChurn covers the ways synchronize routes a
// predecessor through deleteJobsForResource without the follower ever changing:
// losing its annotations, and being deleted and recreated. In every case the
// follower's edge must still be there once the predecessor can fire again,
// because only the follower's own reconcile ever registers it.
func TestChainEdgeSurvivesPredecessorChurn(t *testing.T) {
	t.Parallel()

	t.Run("predecessor loses and regains cron-pattern", func(t *testing.T) {
		head := healthyDeployment("churn-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
		mid := healthyStatefulSet("churn-mid", map[string]string{RESTART_AFTER_KEY: "deployment/churn-head"})
		s, _ := newTestSchedulerWithChain(t, time.Minute, head, mid)
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(head))
		require.NoError(t, s.reconcileJobsForResource(mid))
		require.NotNil(t, edgeFor(s, riOf(head), riOf(mid)))

		// operator removes the cron-pattern; synchronize turns this into a delete
		delete(head.Annotations, CRON_PATTERN_KEY)
		require.NoError(t, s.reconcileJobsForResource(head))
		require.NotNil(t, edgeFor(s, riOf(head), riOf(mid)),
			"edge must survive the predecessor being unannotated")

		// ...and puts it back
		head.Annotations[CRON_PATTERN_KEY] = "* * * * *"
		require.NoError(t, s.reconcileJobsForResource(head))
		require.NotNil(t, edgeFor(s, riOf(head), riOf(mid)),
			"edge must still be registered once the predecessor can fire again")
	})

	t.Run("predecessor deleted and recreated", func(t *testing.T) {
		head := healthyDeployment("recreate-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
		mid := healthyStatefulSet("recreate-mid", map[string]string{RESTART_AFTER_KEY: "deployment/recreate-head"})
		s, _ := newTestSchedulerWithChain(t, time.Minute, head, mid)
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(head))
		require.NoError(t, s.reconcileJobsForResource(mid))

		require.NoError(t, s.deleteJobsForResource(head))
		recreated := healthyDeployment("recreate-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
		require.NoError(t, s.reconcileJobsForResource(recreated))

		require.NotNil(t, edgeFor(s, riOf(recreated), riOf(mid)),
			"edge must survive a delete/recreate of the predecessor")
	})

	t.Run("predecessor that is not itself annotated", func(t *testing.T) {
		// a follower may name a predecessor carrying no kairos annotations at all;
		// every informer update for it arrives here as a delete
		mid := healthyStatefulSet("bare-mid", map[string]string{RESTART_AFTER_KEY: "deployment/bare-head"})
		bare := healthyDeployment("bare-head", nil)
		s, _ := newTestSchedulerWithChain(t, time.Minute, bare, mid)
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(mid))
		require.NotNil(t, edgeFor(s, riOf(bare), riOf(mid)))

		require.NoError(t, s.deleteJobsForResource(bare))
		require.NotNil(t, edgeFor(s, riOf(bare), riOf(mid)),
			"an unannotated predecessor's sync must not wipe the follower's edge")
	})

	t.Run("edge still fires after predecessor churn", func(t *testing.T) {
		head := healthyDeployment("fire-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
		mid := healthyStatefulSet("fire-mid", map[string]string{RESTART_AFTER_KEY: "deployment/fire-head"})
		s, clientset := newTestSchedulerWithChain(t, 5*time.Second, head, mid)
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(head))
		require.NoError(t, s.reconcileJobsForResource(mid))

		delete(head.Annotations, CRON_PATTERN_KEY)
		require.NoError(t, s.reconcileJobsForResource(head))
		head.Annotations[CRON_PATTERN_KEY] = "* * * * *"
		require.NoError(t, s.reconcileJobsForResource(head))

		// head restarting must still cascade to mid
		s.fireRestart(head)
		require.Eventually(t, func() bool {
			sts, err := clientset.AppsV1().StatefulSets("ns1").Get(context.TODO(), "fire-mid", metav1.GetOptions{})
			require.NoError(t, err)
			return sts.Spec.Template.Annotations[CRON_LAST_RESTARTED_AT_KEY] != ""
		}, 5*time.Second, 10*time.Millisecond, "follower was never restarted after predecessor churn")
	})
}

func TestReconcileChainEdgeValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		testName string
		anns     map[string]string
		wantEdge bool
		wantMode chainMode
		wantWait time.Duration
	}{
		{
			testName: "default mode health",
			anns:     map[string]string{RESTART_AFTER_KEY: "deployment/head"},
			wantEdge: true, wantMode: chainModeHealth,
		},
		{
			testName: "explicit health",
			anns:     map[string]string{RESTART_AFTER_KEY: "deployment/head", RESTART_AFTER_MODE_KEY: CHAIN_MODE_HEALTH},
			wantEdge: true, wantMode: chainModeHealth,
		},
		{
			testName: "health-plus-wait with wait",
			anns:     map[string]string{RESTART_AFTER_KEY: "deployment/head", RESTART_AFTER_MODE_KEY: CHAIN_MODE_HEALTH_PLUS_WAIT, RESTART_AFTER_WAIT_KEY: "30s"},
			wantEdge: true, wantMode: chainModeHealthPlusWait, wantWait: 30 * time.Second,
		},
		{
			testName: "invalid mode",
			anns:     map[string]string{RESTART_AFTER_KEY: "deployment/head", RESTART_AFTER_MODE_KEY: "eventually"},
		},
		{
			testName: "wait without health-plus-wait",
			anns:     map[string]string{RESTART_AFTER_KEY: "deployment/head", RESTART_AFTER_WAIT_KEY: "30s"},
		},
		{
			testName: "health-plus-wait without wait",
			anns:     map[string]string{RESTART_AFTER_KEY: "deployment/head", RESTART_AFTER_MODE_KEY: CHAIN_MODE_HEALTH_PLUS_WAIT},
		},
		{
			testName: "unparsable wait",
			anns:     map[string]string{RESTART_AFTER_KEY: "deployment/head", RESTART_AFTER_MODE_KEY: CHAIN_MODE_HEALTH_PLUS_WAIT, RESTART_AFTER_WAIT_KEY: "banana"},
		},
		{
			testName: "non-positive wait",
			anns:     map[string]string{RESTART_AFTER_KEY: "deployment/head", RESTART_AFTER_MODE_KEY: CHAIN_MODE_HEALTH_PLUS_WAIT, RESTART_AFTER_WAIT_KEY: "-5m"},
		},
		{
			testName: "invalid predecessor ref",
			anns:     map[string]string{RESTART_AFTER_KEY: "pod/head"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			head := healthyDeployment("head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
			follower := healthyDeployment("follower", tt.anns)

			s, _ := newTestSchedulerWithChain(t, time.Minute, head, follower)
			s.cron.StartAsync()
			defer s.cron.Stop()

			require.NoError(t, s.reconcileJobsForResource(head))
			require.NoError(t, s.reconcileJobsForResource(follower))

			edge := edgeFor(s, riOf(head), riOf(follower))
			if !tt.wantEdge {
				require.Nil(t, edge, "invalid configuration must not register an edge")
				return
			}
			require.NotNil(t, edge)
			require.Equal(t, tt.wantMode, edge.mode)
			require.Equal(t, tt.wantWait, edge.wait)
		})
	}
}

func TestChainCycleDetection(t *testing.T) {
	t.Parallel()

	t.Run("self-reference rejected", func(t *testing.T) {
		dep := healthyDeployment("loop", map[string]string{
			CRON_PATTERN_KEY:  "* * * * *",
			RESTART_AFTER_KEY: "deployment/loop",
		})
		s, _ := newTestSchedulerWithChain(t, time.Minute, dep)
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(dep))
		require.Nil(t, edgeFor(s, riOf(dep), riOf(dep)))
	})

	t.Run("two-node cycle rejected", func(t *testing.T) {
		a := healthyDeployment("a", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
		b := healthyStatefulSet("b", map[string]string{RESTART_AFTER_KEY: "deployment/a"})
		s, _ := newTestSchedulerWithChain(t, time.Minute, a, b)
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(a))
		require.NoError(t, s.reconcileJobsForResource(b))
		require.NotNil(t, edgeFor(s, riOf(a), riOf(b)))

		// now make a follow b: a->b->a would close the cycle and must be skipped
		a.Annotations[RESTART_AFTER_KEY] = "statefulset/b"
		require.NoError(t, s.reconcileJobsForResource(a))
		require.Nil(t, edgeFor(s, riOf(b), riOf(a)), "cycle-creating edge must be rejected")
	})

	t.Run("diamond allowed", func(t *testing.T) {
		a := healthyDeployment("d-a", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
		b := healthyStatefulSet("d-b", map[string]string{RESTART_AFTER_KEY: "deployment/d-a"})
		c := healthyDeployment("d-c", map[string]string{RESTART_AFTER_KEY: "deployment/d-a"})
		d := healthyDeployment("d-d", map[string]string{RESTART_AFTER_KEY: "statefulset/d-b,deployment/d-c"})
		s, _ := newTestSchedulerWithChain(t, time.Minute, a, b, c, d)
		s.cron.StartAsync()
		defer s.cron.Stop()

		for _, obj := range []runtime.Object{a, b, c, d} {
			require.NoError(t, s.reconcileJobsForResource(obj))
		}
		require.NotNil(t, edgeFor(s, riOf(b), riOf(d)))
		require.NotNil(t, edgeFor(s, riOf(c), riOf(d)))
	})
}

func TestIsRolloutComplete(t *testing.T) {
	t.Parallel()

	replicas := func(n int32) *int32 { return &n }

	tests := []struct {
		testName string
		obj      runtime.Object
		expected bool
	}{
		{
			testName: "deployment healthy",
			obj: &appsv1.Deployment{
				Spec:   appsv1.DeploymentSpec{Replicas: replicas(2)},
				Status: appsv1.DeploymentStatus{UpdatedReplicas: 2, Replicas: 2, AvailableReplicas: 2},
			},
			expected: true,
		},
		{
			testName: "deployment paused",
			obj: &appsv1.Deployment{
				Spec:   appsv1.DeploymentSpec{Replicas: replicas(1), Paused: true},
				Status: appsv1.DeploymentStatus{UpdatedReplicas: 1, Replicas: 1, AvailableReplicas: 1},
			},
			expected: false,
		},
		{
			testName: "deployment observedGeneration lag",
			obj: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{Generation: 3},
				Spec:       appsv1.DeploymentSpec{Replicas: replicas(1)},
				Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, UpdatedReplicas: 1, Replicas: 1, AvailableReplicas: 1},
			},
			expected: false,
		},
		{
			testName: "deployment scaled to zero",
			obj: &appsv1.Deployment{
				Spec:   appsv1.DeploymentSpec{Replicas: replicas(0)},
				Status: appsv1.DeploymentStatus{},
			},
			expected: true,
		},
		{
			testName: "deployment not fully updated",
			obj: &appsv1.Deployment{
				Spec:   appsv1.DeploymentSpec{Replicas: replicas(2)},
				Status: appsv1.DeploymentStatus{UpdatedReplicas: 1, Replicas: 2, AvailableReplicas: 2},
			},
			expected: false,
		},
		{
			testName: "deployment old pods still terminating",
			obj: &appsv1.Deployment{
				Spec:   appsv1.DeploymentSpec{Replicas: replicas(1)},
				Status: appsv1.DeploymentStatus{UpdatedReplicas: 1, Replicas: 2, AvailableReplicas: 1},
			},
			expected: false,
		},
		{
			testName: "deployment available below updated",
			obj: &appsv1.Deployment{
				Spec:   appsv1.DeploymentSpec{Replicas: replicas(2)},
				Status: appsv1.DeploymentStatus{UpdatedReplicas: 2, Replicas: 2, AvailableReplicas: 1},
			},
			expected: false,
		},
		{
			testName: "deployment default replicas healthy",
			obj: &appsv1.Deployment{
				Spec:   appsv1.DeploymentSpec{},
				Status: appsv1.DeploymentStatus{UpdatedReplicas: 1, Replicas: 1, AvailableReplicas: 1},
			},
			expected: true,
		},
		{
			testName: "statefulset healthy",
			obj: &appsv1.StatefulSet{
				Spec:   appsv1.StatefulSetSpec{Replicas: replicas(2)},
				Status: appsv1.StatefulSetStatus{UpdatedReplicas: 2, ReadyReplicas: 2, CurrentRevision: "r2", UpdateRevision: "r2"},
			},
			expected: true,
		},
		{
			testName: "statefulset revision rollout pending",
			obj: &appsv1.StatefulSet{
				Spec:   appsv1.StatefulSetSpec{Replicas: replicas(1)},
				Status: appsv1.StatefulSetStatus{UpdatedReplicas: 1, ReadyReplicas: 1, CurrentRevision: "r1", UpdateRevision: "r2"},
			},
			expected: false,
		},
		{
			testName: "statefulset not ready",
			obj: &appsv1.StatefulSet{
				Spec:   appsv1.StatefulSetSpec{Replicas: replicas(2)},
				Status: appsv1.StatefulSetStatus{UpdatedReplicas: 2, ReadyReplicas: 1, CurrentRevision: "r1", UpdateRevision: "r1"},
			},
			expected: false,
		},
		{
			testName: "statefulset scaled to zero",
			obj: &appsv1.StatefulSet{
				Spec:   appsv1.StatefulSetSpec{Replicas: replicas(0)},
				Status: appsv1.StatefulSetStatus{},
			},
			expected: true,
		},
		{
			testName: "daemonset healthy",
			obj: &appsv1.DaemonSet{
				Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, UpdatedNumberScheduled: 3, NumberReady: 3},
			},
			expected: true,
		},
		{
			testName: "daemonset no nodes",
			obj: &appsv1.DaemonSet{
				Status: appsv1.DaemonSetStatus{},
			},
			expected: true,
		},
		{
			testName: "daemonset not fully updated",
			obj: &appsv1.DaemonSet{
				Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, UpdatedNumberScheduled: 2, NumberReady: 3},
			},
			expected: false,
		},
		{
			testName: "daemonset not ready",
			obj: &appsv1.DaemonSet{
				Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 2, UpdatedNumberScheduled: 2, NumberReady: 1},
			},
			expected: false,
		},
		{
			testName: "unsupported type",
			obj:      &appsv1.ReplicaSet{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			require.Equal(t, tt.expected, isRolloutComplete(tt.obj))
		})
	}
}

func TestChainHealthEndToEnd(t *testing.T) {
	t.Parallel()

	head := healthyDeployment("e2e-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	mid := healthyStatefulSet("e2e-mid", map[string]string{RESTART_AFTER_KEY: "deployment/e2e-head"})
	tail := healthyDeployment("e2e-tail", map[string]string{RESTART_AFTER_KEY: "statefulset/e2e-mid"})

	s, clientset := newTestSchedulerWithChain(t, 5*time.Second, head, mid, tail)
	metrics := NewKairosMetrics()
	s.metrics = metrics
	s.cron.StartAsync()
	defer s.cron.Stop()

	for _, obj := range []runtime.Object{head, mid, tail} {
		s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: obj})
	}

	s.cron.RunAll()

	require.Eventually(t, func() bool { return hasRestarted(clientset, "Deployment", "e2e-head") }, 5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { return hasRestarted(clientset, "StatefulSet", "e2e-mid") }, 5*time.Second, 10*time.Millisecond, "expected mid to restart after head")
	require.Eventually(t, func() bool { return hasRestarted(clientset, "Deployment", "e2e-tail") }, 5*time.Second, 10*time.Millisecond, "expected tail to restart after mid (X->Y->Z)")

	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ChainStepsTotal.WithLabelValues("StatefulSet", "ns1", "e2e-mid", CHAIN_OUTCOME_COMPLETED)))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ChainStepsTotal.WithLabelValues("Deployment", "ns1", "e2e-tail", CHAIN_OUTCOME_COMPLETED)))
}

func TestChainWaitsForPredecessorHealth(t *testing.T) {
	t.Parallel()

	head := unhealthyDeployment("gate-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	mid := healthyStatefulSet("gate-mid", map[string]string{RESTART_AFTER_KEY: "deployment/gate-head"})

	s, clientset := newTestSchedulerWithChain(t, 5*time.Second, head, mid)
	s.cron.StartAsync()
	defer s.cron.Stop()

	s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: head})
	s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: mid})
	s.cron.RunAll()

	require.Eventually(t, func() bool { return hasRestarted(clientset, "Deployment", "gate-head") }, 5*time.Second, 10*time.Millisecond)

	// while the predecessor's rollout has not landed, the follower must wait
	time.Sleep(300 * time.Millisecond)
	require.False(t, hasRestarted(clientset, "StatefulSet", "gate-mid"), "follower must not restart while predecessor is unhealthy")

	setDeploymentHealthy(t, clientset, "ns1", "gate-head")

	require.Eventually(t, func() bool { return hasRestarted(clientset, "StatefulSet", "gate-mid") }, 5*time.Second, 10*time.Millisecond, "follower must restart once predecessor is healthy")
}

func TestChainHealthPlusWaitTiming(t *testing.T) {
	t.Parallel()

	head := healthyDeployment("wait-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	follower := healthyDeployment("wait-follower", map[string]string{
		RESTART_AFTER_KEY:      "deployment/wait-head",
		RESTART_AFTER_MODE_KEY: CHAIN_MODE_HEALTH_PLUS_WAIT,
		RESTART_AFTER_WAIT_KEY: "250ms",
	})

	s, clientset := newTestSchedulerWithChain(t, 10*time.Second, head, follower)
	s.cron.StartAsync()
	defer s.cron.Stop()

	s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: head})
	s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: follower})
	s.cron.RunAll()

	require.Eventually(t, func() bool { return hasRestarted(clientset, "Deployment", "wait-head") }, 5*time.Second, 5*time.Millisecond)
	headAt := time.Now()
	require.Eventually(t, func() bool { return hasRestarted(clientset, "Deployment", "wait-follower") }, 5*time.Second, 5*time.Millisecond)

	gap := time.Since(headAt)
	require.GreaterOrEqual(t, gap, 180*time.Millisecond, "follower must wait out the post-health settle delay")
	require.Less(t, gap, 3*time.Second, "follower fired implausibly late after the settle delay")
}

func TestChainTimeoutAbortsCascade(t *testing.T) {
	t.Parallel()

	head := unhealthyDeployment("to-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	mid := healthyStatefulSet("to-mid", map[string]string{RESTART_AFTER_KEY: "deployment/to-head"})
	blocked := healthyDeployment("to-blocked", map[string]string{RESTART_AFTER_KEY: "statefulset/to-mid"})

	s, clientset := newTestSchedulerWithChain(t, 300*time.Millisecond, head, mid, blocked)
	metrics := NewKairosMetrics()
	s.metrics = metrics
	s.cron.StartAsync()
	defer s.cron.Stop()

	for _, obj := range []runtime.Object{head, mid, blocked} {
		s.processSchedulerBundle(ObjectAndSchedulerAction{action: RESOURCE_CHANGE, obj: obj})
	}
	s.cron.RunAll()

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(metrics.ChainStepsTotal.WithLabelValues("StatefulSet", "ns1", "to-mid", CHAIN_OUTCOME_TIMEOUT)) == 1
	}, 5*time.Second, 10*time.Millisecond, "expected a timeout outcome for the direct follower")

	time.Sleep(300 * time.Millisecond)
	require.False(t, hasRestarted(clientset, "StatefulSet", "to-mid"), "timed-out follower must not restart onto an unhealthy predecessor")
	require.False(t, hasRestarted(clientset, "Deployment", "to-blocked"), "cascade must not propagate past the timed-out step")
	require.Zero(t, testutil.ToFloat64(metrics.ChainStepsTotal.WithLabelValues("Deployment", "ns1", "to-blocked", CHAIN_OUTCOME_TIMEOUT)))
}

func TestPendingStepDedupe(t *testing.T) {
	t.Parallel()

	head := unhealthyDeployment("dedupe-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	mid := healthyStatefulSet("dedupe-mid", map[string]string{RESTART_AFTER_KEY: "deployment/dedupe-head"})

	s, clientset := newTestSchedulerWithChain(t, 5*time.Second, head, mid)
	s.cron.StartAsync()
	defer s.cron.Stop()

	require.NoError(t, s.reconcileJobsForResource(head))
	require.NoError(t, s.reconcileJobsForResource(mid))

	// two triggers while the first step is still waiting on health must dedupe to one
	s.triggerFollowers(riOf(head))
	s.triggerFollowers(riOf(head))

	pending := 0
	s.pendingSteps.Range(func(_, _ any) bool { pending++; return true })
	require.Equal(t, 1, pending, "expected exactly one in-flight step for the follower")

	setDeploymentHealthy(t, clientset, "ns1", "dedupe-head")
	require.Eventually(t, func() bool { return hasRestarted(clientset, "StatefulSet", "dedupe-mid") }, 5*time.Second, 10*time.Millisecond)

	time.Sleep(200 * time.Millisecond)
	patches := 0
	for _, action := range clientset.Actions() {
		if action.GetVerb() == "patch" && action.GetResource().Resource == "statefulsets" {
			patches++
		}
	}
	require.Equal(t, 1, patches, "deduped triggers must produce exactly one follower restart")
}

func TestJobStatusJSONChainedEntries(t *testing.T) {
	t.Parallel()

	head := healthyDeployment("js-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	mid := healthyStatefulSet("js-mid", map[string]string{RESTART_AFTER_KEY: "deployment/js-head"})
	tail := healthyDeployment("js-tail", map[string]string{
		CRON_PATTERN_KEY:       "0 0 * * *",
		RESTART_AFTER_KEY:      "statefulset/js-mid",
		RESTART_AFTER_MODE_KEY: CHAIN_MODE_HEALTH_PLUS_WAIT,
		RESTART_AFTER_WAIT_KEY: "30s",
	})

	s, _ := newTestSchedulerWithChain(t, time.Minute, head, mid, tail)
	s.cron.StartAsync()
	defer s.cron.Stop()

	for _, obj := range []runtime.Object{head, mid, tail} {
		require.NoError(t, s.reconcileJobsForResource(obj))
	}

	rec := httptest.NewRecorder()
	s.JobStatusJSON(rec, httptest.NewRequest("GET", "/api/jobs", nil))

	var entries []jobStatusEntry
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &entries))

	byName := map[string][]jobStatusEntry{}
	for _, e := range entries {
		byName[e.Resource] = append(byName[e.Resource], e)
	}

	midEntries := byName[string(riOf(mid))]
	require.Len(t, midEntries, 1, "pure follower must appear exactly once")
	require.Equal(t, "", midEntries[0].CronPattern, "chained entry has no cron pattern")
	require.Equal(t, "deployment/js-head", midEntries[0].RestartAfter)
	require.Equal(t, CHAIN_MODE_DISPLAY_HEALTH, midEntries[0].RestartAfterMode)

	tailEntries := byName[string(riOf(tail))]
	require.Len(t, tailEntries, 1, "follower with its own cron job appears as its cron entry, not additionally chained")
	require.Equal(t, cronPattern("0 0 * * *").String(), tailEntries[0].CronPattern)
	require.Equal(t, "statefulset/js-mid", tailEntries[0].RestartAfter)
	require.Equal(t, CHAIN_MODE_DISPLAY_PLUS_WAIT, tailEntries[0].RestartAfterMode)
	require.Equal(t, "30s", tailEntries[0].RestartAfterWait)

	headEntries := byName[string(riOf(head))]
	require.Len(t, headEntries, 1)
	require.Empty(t, headEntries[0].RestartAfter)
}

// TestChainSettleWaitAbortIsObserved covers that a cascade dropped during the
// health-plus-wait settle is logged and counted like every other abort. Before
// the fix for #41 the settle-wait branch returned bare, so the step vanished
// with no outcome metric -- exactly the long window where an operator needs to
// know the follower never fired.
func TestChainSettleWaitAbortIsObserved(t *testing.T) {
	t.Parallel()

	newWaitChain := func(t *testing.T, prefix string) (*Scheduler, *fake.Clientset, *KairosMetrics, *appsv1.Deployment, *appsv1.Deployment) {
		t.Helper()
		head := healthyDeployment(prefix+"-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
		follower := healthyDeployment(prefix+"-follower", map[string]string{
			RESTART_AFTER_KEY:      "deployment/" + prefix + "-head",
			RESTART_AFTER_MODE_KEY: CHAIN_MODE_HEALTH_PLUS_WAIT,
			// long enough that the test can interrupt the settle deterministically,
			// short enough that the edge-removal case is not gated on the full sleep
			RESTART_AFTER_WAIT_KEY: "1s",
		})
		s, clientset := newTestSchedulerWithChain(t, 10*time.Second, head, follower)
		metrics := NewKairosMetrics()
		s.metrics = metrics
		s.cron.StartAsync()
		t.Cleanup(s.cron.Stop)

		require.NoError(t, s.reconcileJobsForResource(head))
		require.NoError(t, s.reconcileJobsForResource(follower))
		return s, clientset, metrics, head, follower
	}

	abortedCount := func(m *KairosMetrics, name string) float64 {
		return testutil.ToFloat64(m.ChainStepsTotal.WithLabelValues("Deployment", "ns1", name, CHAIN_OUTCOME_ABORTED))
	}

	t.Run("edge removed during settle", func(t *testing.T) {
		s, clientset, metrics, head, follower := newWaitChain(t, "settle-edge")
		edge := edgeFor(s, riOf(head), riOf(follower))
		require.NotNil(t, edge)

		go s.runChainStep(riOf(head), edge)

		// let the step clear the health check and enter the settle wait
		time.Sleep(200 * time.Millisecond)
		s.removeChainEdgesForFollower(riOf(follower))

		require.Eventually(t, func() bool {
			return abortedCount(metrics, "settle-edge-follower") == 1
		}, 10*time.Second, 10*time.Millisecond, "settle-wait abort must record an aborted outcome")
		require.False(t, hasRestarted(clientset, "Deployment", "settle-edge-follower"),
			"follower must not restart after its edge was removed mid-settle")
	})

	t.Run("shutdown during settle", func(t *testing.T) {
		s, clientset, metrics, head, follower := newWaitChain(t, "settle-shutdown")
		edge := edgeFor(s, riOf(head), riOf(follower))
		require.NotNil(t, edge)

		go s.runChainStep(riOf(head), edge)

		time.Sleep(200 * time.Millisecond)
		s.shutdownCancel()

		require.Eventually(t, func() bool {
			return abortedCount(metrics, "settle-shutdown-follower") == 1
		}, 10*time.Second, 10*time.Millisecond, "shutdown during settle must record an aborted outcome")
		require.False(t, hasRestarted(clientset, "Deployment", "settle-shutdown-follower"),
			"follower must not restart when the scheduler is shutting down")
	})

	t.Run("settle that completes still restarts the follower", func(t *testing.T) {
		// guards against "fixing" the abort path by aborting too eagerly
		head := healthyDeployment("settle-ok-head", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
		follower := healthyDeployment("settle-ok-follower", map[string]string{
			RESTART_AFTER_KEY:      "deployment/settle-ok-head",
			RESTART_AFTER_MODE_KEY: CHAIN_MODE_HEALTH_PLUS_WAIT,
			RESTART_AFTER_WAIT_KEY: "150ms",
		})
		s, clientset := newTestSchedulerWithChain(t, 10*time.Second, head, follower)
		metrics := NewKairosMetrics()
		s.metrics = metrics
		s.cron.StartAsync()
		defer s.cron.Stop()

		require.NoError(t, s.reconcileJobsForResource(head))
		require.NoError(t, s.reconcileJobsForResource(follower))

		s.runChainStep(riOf(head), edgeFor(s, riOf(head), riOf(follower)))

		require.True(t, hasRestarted(clientset, "Deployment", "settle-ok-follower"))
		require.Equal(t, float64(1), testutil.ToFloat64(
			metrics.ChainStepsTotal.WithLabelValues("Deployment", "ns1", "settle-ok-follower", CHAIN_OUTCOME_COMPLETED)))
		require.Zero(t, abortedCount(metrics, "settle-ok-follower"))
	})
}

// TestDeleteJobGaugeNotLeakedWhenEntryMissing covers that the scheduled-jobs gauge
// tracks gocron, not the resource map: the job is gone once RemoveByID returns, so
// a later map-lookup failure must not strand the count (refs #41).
func TestDeleteJobGaugeNotLeakedWhenEntryMissing(t *testing.T) {
	t.Parallel()

	dep := healthyDeployment("gauge-dep", map[string]string{CRON_PATTERN_KEY: "* * * * *"})
	s, _ := newTestScheduler(t, dep)
	metrics := NewKairosMetrics()
	s.metrics = metrics
	s.cron.StartAsync()
	defer s.cron.Stop()

	require.NoError(t, s.reconcileJobsForResource(dep))
	require.Equal(t, float64(1), testutil.ToFloat64(metrics.ScheduledJobs.WithLabelValues("Deployment")))

	cp := cronPattern("* * * * *")
	ri := riOf(dep)
	raw, loaded := s.resourceMap.Load(ri)
	require.True(t, loaded)
	job := raw.(*resourceMapEntry).jobs[cp]

	// drop the entry so deleteJob's map lookup fails after the job is already gone
	s.resourceMap.Delete(ri)
	require.Error(t, s.deleteJob(cp, ri, job, dep))
	require.Zero(t, testutil.ToFloat64(metrics.ScheduledJobs.WithLabelValues("Deployment")),
		"gauge must not leak a count when the resource map entry is already gone")
}
