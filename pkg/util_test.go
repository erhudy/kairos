package pkg

import (
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
		expected   cronPattern
		expectErr  bool
		metaObject metav1.Object
	}{
		{
			testName: "expected empty",
			expected: "",
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
			expected: "* * * * *",
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
			expected: "TZ=America/New_York * * * * *",
			metaObject: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Deployment",
					APIVersion: "apps/v1",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "hello",
					Namespace: "what",
					Annotations: map[string]string{
						CRON_PATTERN_KEY: "TZ=America/New_York * * * * *",
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			cp := getCronPatternString(tt.metaObject)
			require.Equal(t, tt.expected, cronPattern(cp))
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
