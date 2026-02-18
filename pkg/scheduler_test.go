package pkg

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/go-co-op/gocron"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestScheduler creates a Scheduler suitable for unit tests.
func newTestScheduler(t *testing.T, objects ...runtime.Object) (*Scheduler, *fake.Clientset) {
	t.Helper()
	clientset := fake.NewSimpleClientset(objects...)
	logger := zap.NewNop()
	tz, err := time.LoadLocation("")
	require.NoError(t, err)
	ch := make(chan ObjectAndSchedulerAction, 10)
	s := NewScheduler(tz, logger, ch, clientset)
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
			clientset := fake.NewSimpleClientset(tt.object)
			logger := zap.NewNop()

			// Truncate to second precision since RFC3339 drops sub-second
			startTime := time.Now().Truncate(time.Second)
			restartFunc(context.Background(), logger, clientset, tt.object)

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
	clientset := fake.NewSimpleClientset(unsupported)
	logger := zap.NewNop()

	require.NotPanics(t, func() {
		restartFunc(context.Background(), logger, clientset, unsupported)
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
					m := raw.(*map[cronPattern]*gocron.Job)
					require.Empty(t, *m)
				}
			} else {
				raw, loaded := s.resourceMap.Load(ri)
				require.True(t, loaded)
				m := raw.(*map[cronPattern]*gocron.Job)
				require.Len(t, *m, tt.expectedJobsLen)
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
	m := raw.(*map[cronPattern]*gocron.Job)
	require.Len(t, *m, 2)

	// Modify annotation: remove one, add one
	dep.Annotations[CRON_PATTERN_KEY] = "0 0 * * *;15 18 * * *"
	err = s.reconcileJobsForResource(dep)
	require.NoError(t, err)

	raw, loaded = s.resourceMap.Load(ri)
	require.True(t, loaded)
	m = raw.(*map[cronPattern]*gocron.Job)
	require.Len(t, *m, 2)
	// Should have "0 0 * * *" (unchanged) and "15 18 * * *" (new), but not "30 6 * * *"
	_, has00 := (*m)[cronPattern("0 0 * * *")]
	_, has1518 := (*m)[cronPattern("15 18 * * *")]
	_, has306 := (*m)[cronPattern("30 6 * * *")]
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
				m := raw.(*map[cronPattern]*gocron.Job)
				_, exists := (*m)[tt.pattern]
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
		m := raw.(*map[cronPattern]*gocron.Job)
		require.Len(t, *m, 2)

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
		m := raw.(*map[cronPattern]*gocron.Job)
		require.Len(t, *m, 1)
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

