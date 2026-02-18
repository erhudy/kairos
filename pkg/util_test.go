package pkg

import (
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// quick and dirty implementation of the runtime.Object interface that does not
// include the schema.ObjectKind interface (to cause a panic on the type assertion)
type GetObjectMetaAndKindPanicType struct{}

func (p GetObjectMetaAndKindPanicType) GetObjectKind() schema.ObjectKind {
	return schema.EmptyObjectKind
}

func (p GetObjectMetaAndKindPanicType) DeepCopyObject() runtime.Object {
	return &runtime.Unknown{}
}

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
			object:      &GetObjectMetaAndKindPanicType{},
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

func TestGetResourceIdentifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		testName string
		object   runtime.Object
		expected resourceIdentifier
	}{
		{
			testName: "deployment",
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-deploy",
					Namespace: "default",
				},
			},
			expected: resourceIdentifier("apps/v1, Kind=Deployment/default/my-deploy"),
		},
		{
			testName: "daemonset in kube-system",
			object: &appsv1.DaemonSet{
				TypeMeta: metav1.TypeMeta{Kind: "DaemonSet", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "node-agent",
					Namespace: "kube-system",
				},
			},
			expected: resourceIdentifier("apps/v1, Kind=DaemonSet/kube-system/node-agent"),
		},
		{
			testName: "statefulset",
			object: &appsv1.StatefulSet{
				TypeMeta: metav1.TypeMeta{Kind: "StatefulSet", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "db",
					Namespace: "prod",
				},
			},
			expected: resourceIdentifier("apps/v1, Kind=StatefulSet/prod/db"),
		},
		{
			testName: "custom GVK",
			object: &appsv1.Deployment{
				TypeMeta: metav1.TypeMeta{Kind: "Deployment", APIVersion: "apps/v1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test",
					Namespace: "ns",
				},
			},
			expected: resourceIdentifier("apps/v1, Kind=Deployment/ns/test"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			om, ok := getObjectMetaAndKind(tt.object)
			ri := getResourceIdentifier(om, ok)
			require.Equal(t, tt.expected, ri)
		})
	}
}

func TestCronPatternString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		testName string
		pattern  cronPattern
		expected string
	}{
		{
			testName: "simple pattern",
			pattern:  cronPattern("* * * * *"),
			expected: "* * * * *",
		},
		{
			testName: "pattern with TZ",
			pattern:  cronPattern("TZ=America/New_York 0 9 * * 1-5"),
			expected: "TZ=America/New_York 0 9 * * 1-5",
		},
		{
			testName: "empty pattern",
			pattern:  cronPattern(""),
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			require.Equal(t, tt.expected, tt.pattern.String())
		})
	}
}

func TestCronPatternsString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		testName string
		patterns cronPatterns
		expected string
	}{
		{
			testName: "zero patterns",
			patterns: cronPatterns{},
			expected: "",
		},
		{
			testName: "single pattern",
			patterns: cronPatterns{cronPattern("* * * * *")},
			expected: "* * * * *",
		},
		{
			testName: "multiple patterns",
			patterns: cronPatterns{
				cronPattern("0 0 * * *"),
				cronPattern("30 12 * * 1-5"),
				cronPattern("TZ=UTC 0 6 * * *"),
			},
			expected: "0 0 * * *, 30 12 * * 1-5, TZ=UTC 0 6 * * *",
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			require.Equal(t, tt.expected, tt.patterns.String())
		})
	}
}
