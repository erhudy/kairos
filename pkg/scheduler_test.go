package pkg

import (
	"context"
	"reflect"
	"testing"
	"time"

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
	s := NewScheduler(tz, logger, ch, clientset, nil, 0, lookback)
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

	t.Run("delete for nonexistent resource returns error", func(t *testing.T) {
		s, _ := newTestScheduler(t)
		s.cron.StartAsync()
		defer s.cron.Stop()

		err := s.deleteJobsForResource(dep)
		require.Error(t, err)
		require.Contains(t, err.Error(), "not found in resource map")
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
			if action.GetVerb() == "update" && action.GetResource().Resource == "deployments" {
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
