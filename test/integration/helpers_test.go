//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// kairosProc manages one kairos subprocess.

type kairosProc struct {
	cmd     *exec.Cmd
	port    int
	logPath string
	stopped bool
	mu      sync.Mutex
}

func startKairos(t *testing.T, port int, extraArgs ...string) *kairosProc {
	t.Helper()

	// fail fast if a stale kairos is still holding the port
	if conn, err := netDial(port); err == nil {
		_ = conn.Close()
		t.Fatalf("port %d already in use; a stale kairos process may be running (see logs in %s)", port, os.TempDir())
	}

	logPath := filepath.Join(os.TempDir(), fmt.Sprintf("kairos-integration-%d.log", port))
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating log file: %v", err)
	}

	args := []string{
		"-kubeconfig", kubeconfigPath,
		"-metrics-addr", fmt.Sprintf(":%d", port),
		"-debug",
	}
	args = append(args, extraArgs...)

	t.Logf("starting kairos on :%d with args %v (log: %s)", port, extraArgs, logPath)
	cmd := exec.Command(kairosBin, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting kairos: %v", err)
	}

	p := &kairosProc{cmd: cmd, port: port, logPath: logPath}
	t.Cleanup(p.stop)
	p.waitReady(t)
	return p
}

func (p *kairosProc) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	if p.cmd.ProcessState != nil && p.cmd.ProcessState.Exited() {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- p.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = p.cmd.Process.Kill()
		<-done
	}
}

func (p *kairosProc) waitReady(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := fetchConfig(p.port); err == nil {
			return
		}
		time.Sleep(time.Second)
	}
	logs, _ := os.ReadFile(p.logPath)
	t.Fatalf("kairos did not become ready on :%d\nlast logs:\n%s", p.port, tail(string(logs), 3000))
}

// --- fixtures ---

// applyFixtures reconverges cluster state to test.yaml before each phase, so a
// phase that mutates resources (annotation removal, deletion) does not leak into
// later phases.
func applyFixtures(t *testing.T) {
	t.Helper()
	cmd := exec.Command("kubectl", "apply", "-f", filepath.Join(repoRoot(), "hack", "test.yaml"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("re-applying test.yaml: %s: %v", out, err)
	}
}

// --- HTTP API ---

type jobEntry struct {
	Resource    string `json:"resource"`
	CronPattern string `json:"cronPattern"`
	LastRun     string `json:"lastRun"`
	NextRun     string `json:"nextRun"`
	LastJitter  string `json:"lastJitter"`
}

type configEntry struct {
	Timezone string `json:"timezone"`
	Jitter   string `json:"jitter"`
	Lookback string `json:"lookback"`
}

func fetchJobs(port int) ([]jobEntry, error) {
	body, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d/api/jobs", port))
	if err != nil {
		return nil, err
	}
	var entries []jobEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("decoding /api/jobs: %w", err)
	}
	return entries, nil
}

func fetchConfig(port int) (*configEntry, error) {
	body, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d/api/config", port))
	if err != nil {
		return nil, err
	}
	var cfg configEntry
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, fmt.Errorf("decoding /api/config: %w", err)
	}
	return &cfg, nil
}

func fetchMetrics(port int) (string, error) {
	body, err := httpGet(fmt.Sprintf("http://127.0.0.1:%d/metrics", port))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func httpGet(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// jobsFor returns all job entries whose resource identifier ends in /ns/name.
func jobsFor(entries []jobEntry, ns, name string) []jobEntry {
	suffix := "/" + ns + "/" + name
	var out []jobEntry
	for _, e := range entries {
		if strings.HasSuffix(e.Resource, suffix) {
			out = append(out, e)
		}
	}
	return out
}

// waitForJobsRegistered blocks until every (ns,name) pair has at least one job.
func waitForJobsRegistered(t *testing.T, port int, want [][2]string) []jobEntry {
	t.Helper()
	var entries []jobEntry
	err := pollUntil(registrationTimeout, func() error {
		var err error
		entries, err = fetchJobs(port)
		if err != nil {
			return err
		}
		for _, w := range want {
			if len(jobsFor(entries, w[0], w[1])) == 0 {
				return fmt.Errorf("no jobs registered yet for %s/%s (have %d entries)", w[0], w[1], len(entries))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("timed out waiting for job registration: %v", err)
	}
	return entries
}

// --- Kubernetes accessors ---

func getRestartAnnotation(t *testing.T, kind, ns, name string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch kind {
	case "Deployment":
		obj, err := clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting Deployment %s/%s: %v", ns, name, err)
		}
		return obj.Spec.Template.Annotations[restartedAtKey]
	case "DaemonSet":
		obj, err := clientset.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting DaemonSet %s/%s: %v", ns, name, err)
		}
		return obj.Spec.Template.Annotations[restartedAtKey]
	case "StatefulSet":
		obj, err := clientset.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting StatefulSet %s/%s: %v", ns, name, err)
		}
		return obj.Spec.Template.Annotations[restartedAtKey]
	default:
		t.Fatalf("unknown kind %q", kind)
		return ""
	}
}

func removeCronAnnotation(t *testing.T, kind, ns, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch kind {
	case "Deployment":
		obj, err := clientset.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("getting Deployment: %v", err)
		}
		delete(obj.Annotations, cronPatternKey)
		if _, err := clientset.AppsV1().Deployments(ns).Update(ctx, obj, metav1.UpdateOptions{}); err != nil {
			t.Fatalf("removing annotation: %v", err)
		}
	default:
		t.Fatalf("removeCronAnnotation not implemented for kind %q", kind)
	}
}

func deleteWorkload(t *testing.T, kind, ns, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	policy := metav1.DeletePropagationForeground
	opts := metav1.DeleteOptions{PropagationPolicy: &policy}
	var err error
	switch kind {
	case "Deployment":
		err = clientset.AppsV1().Deployments(ns).Delete(ctx, name, opts)
	default:
		t.Fatalf("deleteWorkload not implemented for kind %q", kind)
	}
	if err != nil {
		t.Fatalf("deleting %s %s/%s: %v", kind, ns, name, err)
	}
}

// waitForRestart waits for the restart annotation to change from baseline and
// returns the parsed timestamp. If g is non-nil it tracks host-sleep detection;
// a timeout or late observation after detected sleep skips instead of failing.
func waitForRestart(t *testing.T, g *clockGuard, kind, ns, name, baseline string, timeout time.Duration) time.Time {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if g != nil {
			g.tick()
		}
		v := getRestartAnnotation(t, kind, ns, name)
		if v != "" && v != baseline {
			parsed, err := time.Parse(timeFormat, v)
			if err == nil {
				return parsed
			}
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(pollInterval)
	}
	if g != nil && g.Slept {
		t.Skipf("host sleep detected while waiting for restart of %s %s/%s; window invalid", kind, ns, name)
	}
	t.Fatalf("timed out after %v waiting for restart of %s %s/%s (still %q)", timeout, kind, ns, name, getRestartAnnotation(t, kind, ns, name))
	return time.Time{}
}

// assertNoRestart asserts the annotation stays at baseline for the given duration.
func assertNoRestart(t *testing.T, g *clockGuard, kind, ns, name, baseline string, dur time.Duration) {
	t.Helper()
	deadline := time.Now().Add(dur)
	for time.Now().Before(deadline) {
		if g != nil {
			g.tick()
		}
		v := getRestartAnnotation(t, kind, ns, name)
		if v != baseline {
			if g != nil && g.Slept {
				t.Skipf("host sleep detected during no-restart window for %s %s/%s; overdue firings on wake make this inconclusive", kind, ns, name)
			}
			t.Fatalf("expected no restart for %s %s/%s but annotation changed from %q to %q", kind, ns, name, baseline, v)
		}
		time.Sleep(3 * time.Second)
	}
	if g != nil && g.Slept {
		t.Logf("warning: host sleep detected during no-restart window for %s %s/%s (no restart observed anyway)", kind, ns, name)
	}
}

// --- timing helpers (kairos fires on wall-clock minute boundaries) ---

// clockGuard detects host sleep during a polling window. Go's monotonic clock
// (used by Sub) excludes macOS system sleep while the wall clock does not, so a
// host suspend shows up as the wall gap running ahead of the monotonic gap. An
// intentional long wait (waiting for a minute boundary) advances both equally and
// is therefore never mistaken for sleep. Detected sleeps turn affected boundary
// assertions into skips instead of false failures.
type clockGuard struct {
	last      time.Time
	threshold time.Duration
	Slept     bool
}

func newClockGuard(threshold time.Duration) *clockGuard {
	return &clockGuard{last: time.Now(), threshold: threshold}
}

// wallGap returns the duration between two time.Now() readings using only their
// wall clock readings (monotonic stripped), i.e. including any host sleep.
func wallGap(a, b time.Time) time.Duration {
	return b.Round(0).Sub(a.Round(0))
}

// tick records one observation and flags divergence between wall and monotonic
// clocks larger than threshold as host sleep. Safe to call across intentional
// waits; see clockGuard docs.
func (c *clockGuard) tick() {
	now := time.Now()
	if wallGap(c.last, now)-now.Sub(c.last) > c.threshold {
		c.Slept = true
	}
	c.last = now
}

// nextMinuteBoundary returns the next local wall-clock boundary strictly after now.
func nextMinuteBoundary() time.Time {
	now := time.Now()
	return now.Truncate(time.Minute).Add(time.Minute)
}

// waitUntil sleeps until tm (returns immediately if already past).
func waitUntil(tm time.Time) {
	if d := time.Until(tm); d > 0 {
		time.Sleep(d)
	}
}

func pollUntil(timeout time.Duration, cond func() error) error {
	deadline := time.Now().Add(timeout)
	prev := time.Now()
	var last error
	for {
		last = cond()
		if last == nil {
			return nil
		}
		now := time.Now()
		if gap := wallGap(prev, now); gap > 10*time.Second {
			// host likely slept; do not penalize the deadline for wall-clock gaps
			deadline = deadline.Add(gap)
		}
		prev = now
		if now.After(deadline) {
			return last
		}
		time.Sleep(pollInterval)
	}
}

// --- metrics parsing (kept dependency-free; label order is not assumed) ---

func counterValue(metricsText, metric string, labels map[string]string) (float64, bool) {
	for _, line := range strings.Split(metricsText, "\n") {
		if !strings.HasPrefix(line, metric+"{") {
			continue
		}
		open := strings.Index(line, "{")
		closing := strings.LastIndex(line, "}")
		if closing < open {
			continue
		}
		labelPart := line[open+1 : closing]
		valuePart := strings.TrimSpace(line[closing+1:])
		ok := true
		for k, v := range labels {
			if !strings.Contains(labelPart, fmt.Sprintf("%s=%q", k, v)) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		f, err := strconv.ParseFloat(valuePart, 64)
		if err != nil {
			continue
		}
		return f, true
	}
	return 0, false
}

// --- cron expectations (mirrors pkg.parseCronExpression for TZ-prefixed patterns) ---

var cronParser = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

func expectedNextRun(pattern string, now time.Time) (time.Time, error) {
	loc := time.Local
	spec := pattern
	if strings.HasPrefix(spec, "TZ=") || strings.HasPrefix(spec, "CRON_TZ=") {
		eqIdx := strings.Index(spec, "=")
		rest := spec[eqIdx+1:]
		spaceIdx := strings.Index(rest, " ")
		if spaceIdx < 0 {
			return time.Time{}, fmt.Errorf("invalid TZ-prefixed pattern %q", pattern)
		}
		var err error
		loc, err = time.LoadLocation(rest[:spaceIdx])
		if err != nil {
			return time.Time{}, err
		}
		spec = rest[spaceIdx+1:]
	}
	schedule, err := cronParser.Parse(spec)
	if err != nil {
		return time.Time{}, err
	}
	if s, ok := schedule.(*cron.SpecSchedule); ok {
		s.Location = loc
	}
	return schedule.Next(now), nil
}

// --- misc ---

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func netDial(port int) (net.Conn, error) {
	return net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second)
}
