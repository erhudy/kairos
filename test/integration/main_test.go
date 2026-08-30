//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	cronPatternKey      = "kairos.erhudy.com/cron-pattern"
	restartedAtKey      = "kairos.erhudy.com/cron-last-restarted-at"
	timeFormat          = time.RFC3339
	pollInterval        = 2 * time.Second
	registrationTimeout = 90 * time.Second
)

var (
	kubeconfigPath string
	kairosBin      string
	clientset      *kubernetes.Clientset
)

func TestMain(m *testing.M) {
	code, err := runMain(m)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration setup failed: %v\n(hint: run hack/run-integration.sh)\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func runMain(m *testing.M) (int, error) {
	kubeconfigPath = os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		home, _ := os.UserHomeDir()
		kubeconfigPath = filepath.Join(home, ".kube", "config")
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return 0, fmt.Errorf("building kubeconfig from %s: %w", kubeconfigPath, err)
	}
	clientset, err = kubernetes.NewForConfig(config)
	if err != nil {
		return 0, fmt.Errorf("building clientset: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{}); err != nil {
		return 0, fmt.Errorf("cluster unreachable via %s: %w", kubeconfigPath, err)
	}

	kairosBin = os.Getenv("KAIROS_BIN")
	if kairosBin == "" {
		built, err := buildBinary()
		if err != nil {
			return 0, err
		}
		kairosBin = built
	} else if _, err := os.Stat(kairosBin); err != nil {
		return 0, fmt.Errorf("KAIROS_BIN not usable: %w", err)
	}

	return m.Run(), nil
}

func buildBinary() (string, error) {
	dir, err := os.MkdirTemp("", "kairos-integration-bin")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "kairos")
	cmd := exec.Command("go", "build", "-o", bin, "../..")
	cmd.Dir = filepath.Join(repoRoot(), "test", "integration")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building kairos binary: %s: %w", out, err)
	}
	return bin, nil
}

func repoRoot() string {
	wd, _ := os.Getwd()
	return filepath.Dir(filepath.Dir(wd))
}
