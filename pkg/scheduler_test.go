package pkg

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func TestScheduler(t *testing.T) {
	tests := []struct {
		testName                                string
		customTimer                             func(d time.Duration, f func()) *time.Timer
		tzstring                                string
		testObject                              runtime.Object
		oasaChan                                chan ObjectAndSchedulerAction
		specShouldHaveLastRestartedAtAnnotation bool
		timeToWaitBeforeCheckingClientset       time.Duration
	}{
		{
			customTimer: func(d time.Duration, f func()) *time.Timer {
				if d == time.Minute {
					d = time.Second
				}
				return time.AfterFunc(d, f)
			},
			testObject: &appsv1.Deployment{
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
			oasaChan:                          make(chan ObjectAndSchedulerAction),
			timeToWaitBeforeCheckingClientset: time.Second * 2,
		},
		{
			customTimer: func(d time.Duration, f func()) *time.Timer {
				if d == time.Minute {
					d = time.Second
				}
				return time.AfterFunc(d, f)
			},
			testObject: &appsv1.DaemonSet{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Daemonset",
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
			oasaChan:                          make(chan ObjectAndSchedulerAction),
			timeToWaitBeforeCheckingClientset: time.Second * 2,
		},
		{
			customTimer: func(d time.Duration, f func()) *time.Timer {
				if d == time.Minute {
					d = time.Second
				}
				return time.AfterFunc(d, f)
			},
			testObject: &appsv1.StatefulSet{
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
			oasaChan:                          make(chan ObjectAndSchedulerAction),
			timeToWaitBeforeCheckingClientset: time.Second * 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.testName, func(t *testing.T) {
			clientset := fake.NewSimpleClientset(tt.testObject)
			logger := zap.NewNop()

			tz, err := time.LoadLocation(tt.tzstring)
			require.NoError(t, err)

			startTime := time.Now()
			s := NewScheduler(tz, logger, tt.oasaChan, clientset)
			s.cron.CustomTimer(tt.customTimer)
			stopCh := make(chan struct{})
			go s.Run(stopCh)
			time.Sleep(tt.timeToWaitBeforeCheckingClientset)

			var obj runtime.Object
			switch tt.testObject.(type) {
			case *appsv1.DaemonSet:
				obj, err = clientset.AppsV1().DaemonSets("what").Get(context.TODO(), "hello", metav1.GetOptions{})
			case *appsv1.Deployment:
				obj, err = clientset.AppsV1().Deployments("what").Get(context.TODO(), "hello", metav1.GetOptions{})
			case *appsv1.StatefulSet:
				obj, err = clientset.AppsV1().StatefulSets("what").Get(context.TODO(), "hello", metav1.GetOptions{})
			}
			require.NoError(t, err)

			anns := reflect.Indirect(reflect.ValueOf(obj)).FieldByName("Spec").FieldByName("Template").FieldByName("Annotations").Interface().(map[string]string)
			ann, ok := anns[CRON_LAST_RESTARTED_AT_KEY]
			require.Equal(t, tt.specShouldHaveLastRestartedAtAnnotation, ok)
			if tt.specShouldHaveLastRestartedAtAnnotation {
				parsed, err := time.Parse(LAST_RESTARTED_AT_TIME_FORMAT, ann)
				require.NoError(t, err)
				require.WithinRange(t, parsed, startTime, time.Now())
			}
			stopCh <- struct{}{}
		})
	}
}
