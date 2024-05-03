package pkg

import (
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"testing"

	"github.com/erhudy/kairos/kairostest"
	"github.com/stretchr/testify/require"
)

func TestCareAboutThisObject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		testName   string
		expected   bool
		metaObject metav1.Object
	}{
		{
			testName: "expected fail",
			expected: false,
			metaObject: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hello",
					Namespace: "what",
				},
			},
		},
		{
			testName: "expected pass",
			expected: true,
			metaObject: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hello",
					Namespace: "what",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "* * * * *",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			require.Equal(t, tt.expected, careAboutThisObject(tt.metaObject))
		})
	}
}

func TestGetCronPattern(t *testing.T) {
	tests := []struct {
		testName   string
		expected   cronPatternWithTimezone
		expectErr  bool
		location   func() (*time.Location, error)
		metaObject metav1.Object
	}{
		{
			testName: "expected empty",
			expected: cronPatternWithTimezone{
				cronPattern:    "",
				locationString: time.UTC.String(),
			},
			location: nil,
			metaObject: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hello",
					Namespace: "what",
				},
			},
		},
		{
			testName: "expected pattern",
			expected: cronPatternWithTimezone{
				cronPattern:    "* * * * *",
				locationString: time.UTC.String(),
			},
			metaObject: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hello",
					Namespace: "what",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "* * * * *",
					},
				},
			},
		},
		{
			testName: "expected pattern with America/New_York",
			expected: cronPatternWithTimezone{
				cronPattern: "* * * * *",
				// location will be computed via the location struct field
			},
			location: func() (*time.Location, error) {
				return time.LoadLocation("America/New_York")
			},
			metaObject: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hello",
					Namespace: "what",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "* * * * *",
						TIME_ZONE_KEY:    "America/New_York",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			cronPattern := getCronPatternString(tt.metaObject)

			var location *time.Location
			var err error

			location, err = getTimeLocation(tt.metaObject)
			if tt.expectErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			if tt.location != nil {
				location, err = tt.location()
				require.NoError(t, err)
				tt.expected.locationString = location.String()
			}

			require.Equal(t, tt.expected, cronPatternWithTimezone{cronPattern: cronPattern, locationString: location.String()})
		})
	}
}

func TestGetObjectMetaAndKind(t *testing.T) {
	tests := []struct {
		testName    string
		expectPanic bool
		object      runtime.Object
		// we don't expect objectMeta or objectKind object here because we will call
		// methods on the returned object and check them against the test object
	}{
		{
			testName:    "deployment object",
			expectPanic: false,
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hello",
					Namespace: "what",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "* * * * *",
					},
				},
			},
		},
		{
			testName:    "daemonset object",
			expectPanic: false,
			object: &appsv1.DaemonSet{
				TypeMeta: metav1.TypeMeta{
					Kind:       "DaemonSet",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hello",
					Namespace: "what",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "* * * * *",
					},
				},
			},
		},
		{
			testName:    "statefulset object",
			expectPanic: false,
			object: &appsv1.StatefulSet{
				TypeMeta: metav1.TypeMeta{
					Kind:       "StatefulSet",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hello",
					Namespace: "what",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "* * * * *",
					},
				},
			},
		},
		{
			testName:    "expect panic",
			expectPanic: true,
			object:      &kairostest.GetObjectMetaAndKindPanicType{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			if tt.expectPanic {
				require.Panics(t, func() {
					getObjectMetaAndKind(tt.object)
				})
			} else {
				om, ok := getObjectMetaAndKind(tt.object)

				eom := tt.object.(metav1.ObjectMetaAccessor).GetObjectMeta()
				eok := tt.object.GetObjectKind()

				require.Equal(t, om.GetName(), eom.GetName())
				require.Equal(t, om.GetNamespace(), eom.GetNamespace())
				require.Equal(t, om.GetAnnotations(), eom.GetAnnotations())
				require.Equal(t, om.GetLabels(), eom.GetLabels())

				require.Equal(t, ok.GroupVersionKind(), eok.GroupVersionKind())
			}
		})
	}
}
