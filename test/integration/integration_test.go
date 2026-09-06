//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	ns1 = "namespace-1"
	ns2 = "namespace-2"

	portDefault   = 19090
	portNamespace = 19091
	portJitter    = 19092
	portLookback  = 19093
	portChain     = 19094
)

// TestIntegration runs the full suite against a kind cluster with test.yaml
// applied. Phases run sequentially; each phase manages its own kairos process
// (different flags per phase). Total runtime is ~20 minutes because cron
// schedules are minute-granularity and kairos fires on wall-clock boundaries,
// and PhaseE adds chained-cascade waits on top of that.
// Host sleep during a boundary window yields SKIPs (not false failures); keep
// the machine awake for a fully meaningful run.
func TestIntegration(t *testing.T) {
	t.Run("PhaseA_Default", phaseADefault)
	t.Run("PhaseB_NamespaceScoping", phaseBNamespaceScoping)
	t.Run("PhaseC_Jitter", phaseCJitter)
	t.Run("PhaseD_Lookback", phaseDLookback)
	t.Run("PhaseE_ChainedRestarts", phaseEChainedRestarts)
}

var allAnnotated = [][2]string{
	{ns1, "erhudy-test-every-minute"},
	{ns1, "erhudy-test-daemonset-every-minute"},
	{ns1, "erhudy-test-every-three-minutes"},
	{ns1, "erhudy-test-every-four-minutes"},
	{ns1, "erhudy-test-semicolon-every-odd-minute"},
	{ns2, "erhudy-test-every-even-minute"},
	{ns2, "erhudy-test-every-fifth-minute"},
	{ns2, "erhudy-test-every-minute"},
	{ns2, "erhudy-test-semicolon-every-odd-minute"},
	{ns2, "erhudy-test-semicolon-every-odd-minute-with-timezone"},
	{ns2, "erhudy-test-more-timezones"},
}

var ns1Annotated = allAnnotated[:5]

func phaseADefault(t *testing.T) {
	applyFixtures(t)
	p := startKairos(t, portDefault)

	t.Run("ConfigAndRegistration", func(t *testing.T) {
		cfg, err := fetchConfig(p.port)
		if err != nil {
			t.Fatalf("fetching /api/config: %v", err)
		}
		if cfg.Jitter != "disabled" || cfg.Lookback != "disabled" {
			t.Errorf("expected jitter/lookback disabled, got %+v", cfg)
		}
		if cfg.ChainTimeout != "10m0s" {
			t.Errorf("expected chain timeout 10m0s by default, got %q", cfg.ChainTimeout)
		}

		entries := waitForJobsRegistered(t, p.port, allAnnotated)

		if un := jobsFor(entries, ns1, "erhudy-test-unannotated"); len(un) != 0 {
			t.Errorf("unannotated deployment must not be tracked, got %d entries", len(un))
		}
	})

	t.Run("EveryMinuteRestarts", func(t *testing.T) {
		targets := []struct{ kind, ns, name string }{
			{"Deployment", ns1, "erhudy-test-every-minute"},
			{"StatefulSet", ns2, "erhudy-test-every-minute"},
			{"DaemonSet", ns1, "erhudy-test-daemonset-every-minute"},
		}
		baselines := map[string]string{}
		for _, tg := range targets {
			baselines[tg.ns+"/"+tg.name] = getRestartAnnotation(t, tg.kind, tg.ns, tg.name)
		}
		g := newClockGuard(10 * time.Second)

		boundary := nextMinuteBoundary()
		waitUntil(boundary.Add(time.Second))
		g.tick()
		for _, tg := range targets {
			ts := waitForRestart(t, g, tg.kind, tg.ns, tg.name, baselines[tg.ns+"/"+tg.name], 75*time.Second)
			assertWindow(t, g, ts, boundary, -2*time.Second, 40*time.Second)
		}
	})

	t.Run("TimezoneNextRuns", func(t *testing.T) {
		entries := waitForJobsRegistered(t, p.port, [][2]string{{ns2, "erhudy-test-more-timezones"}})
		now := time.Now()
		for _, e := range jobsFor(entries, ns2, "erhudy-test-more-timezones") {
			want, err := expectedNextRun(e.CronPattern, now)
			if err != nil {
				t.Fatalf("computing expected next run for %q: %v", e.CronPattern, err)
			}
			got, err := time.Parse(time.RFC3339, e.NextRun)
			if err != nil {
				t.Fatalf("parsing NextRun %q for %q: %v", e.NextRun, e.CronPattern, err)
			}
			d := want.UTC().Sub(got.UTC())
			if d < -5*time.Second || d > 5*time.Second {
				t.Errorf("pattern %q: NextRun %s differs from expected %s by %v", e.CronPattern, got, want.UTC(), d)
			}
		}

		tzEntries := jobsFor(entries, ns2, "erhudy-test-semicolon-every-odd-minute-with-timezone")
		if len(tzEntries) != 6 {
			t.Errorf("expected 6 job entries for the with-timezone deployment, got %d", len(tzEntries))
		}
	})

	t.Run("ThreeMinuteStepPattern", func(t *testing.T) {
		const name = "erhudy-test-every-three-minutes"
		baseline := getRestartAnnotation(t, "Deployment", ns1, name)
		g := newClockGuard(10 * time.Second)

		boundary := nextMinuteBoundary()
		if boundary.Minute()%3 != 0 {
			waitUntil(boundary.Add(time.Second))
			assertNoRestart(t, g, "Deployment", ns1, name, baseline, 20*time.Second)
			boundary = boundary.Add(time.Minute)
			for boundary.Minute()%3 != 0 {
				boundary = boundary.Add(time.Minute)
			}
		}

		waitUntil(boundary.Add(time.Second))
		g.tick()
		ts := waitForRestart(t, g, "Deployment", ns1, name, baseline, 75*time.Second)
		assertWindow(t, g, ts, boundary, -2*time.Second, 40*time.Second)
	})

	t.Run("SemicolonOddEvenMinutes", func(t *testing.T) {
		targets := []struct{ kind, ns, name string }{
			{"Deployment", ns1, "erhudy-test-semicolon-every-odd-minute"},
			{"Deployment", ns2, "erhudy-test-semicolon-every-odd-minute"},
		}
		baselines := map[string]string{}
		for _, tg := range targets {
			baselines[tg.ns+"/"+tg.name] = getRestartAnnotation(t, tg.kind, tg.ns, tg.name)
		}
		g := newClockGuard(10 * time.Second)

		// the pattern fires on odd minutes only (1,3,5,...,59)
		boundary := nextMinuteBoundary()
		if boundary.Minute()%2 == 0 {
			waitUntil(boundary.Add(time.Second))
			g.tick()
			for _, tg := range targets {
				assertNoRestart(t, g, tg.kind, tg.ns, tg.name, baselines[tg.ns+"/"+tg.name], 20*time.Second)
			}
			boundary = boundary.Add(time.Minute)
		}

		waitUntil(boundary.Add(time.Second))
		g.tick()
		for _, tg := range targets {
			ts := waitForRestart(t, g, tg.kind, tg.ns, tg.name, baselines[tg.ns+"/"+tg.name], 75*time.Second)
			assertWindow(t, g, ts, boundary, -2*time.Second, 40*time.Second)
		}

		evenBoundary := boundary.Add(time.Minute)
		waitUntil(evenBoundary.Add(time.Second))
		g.tick()
		for _, tg := range targets {
			current := getRestartAnnotation(t, tg.kind, tg.ns, tg.name)
			assertNoRestart(t, g, tg.kind, tg.ns, tg.name, current, 20*time.Second)
		}
	})

	t.Run("MetricsSmoke", func(t *testing.T) {
		text, err := fetchMetrics(p.port)
		if err != nil {
			t.Fatalf("fetching /metrics: %v", err)
		}
		v, ok := counterValue(text, "kairos_restart_total", map[string]string{
			"kind":      "Deployment",
			"namespace": ns1,
			"name":      "erhudy-test-every-minute",
		})
		if !ok || v < 1 {
			t.Errorf("expected kairos_restart_total >= 1 for %s/%s every-minute, got %v (found=%v)", ns1, "erhudy-test-every-minute", v, ok)
		}
		if _, ok := counterValue(text, "kairos_scheduled_jobs", map[string]string{"kind": "Deployment"}); !ok {
			t.Errorf("expected kairos_scheduled_jobs gauge for kind=Deployment")
		}
	})

	t.Run("AnnotationRemovalCancelsJobs", func(t *testing.T) {
		const name = "erhudy-test-every-minute"
		baseline := getRestartAnnotation(t, "Deployment", ns1, name)

		removeWorkloadAnnotation(t, "Deployment", ns1, name, cronPatternKey)

		err := pollUntil(20*time.Second, func() error {
			entries, err := fetchJobs(p.port)
			if err != nil {
				return err
			}
			if n := len(jobsFor(entries, ns1, name)); n != 0 {
				return fmt.Errorf("%d job entries still registered for %s/%s", n, ns1, name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("jobs not cancelled after annotation removal: %v", err)
		}

		// the resource restarted every minute before; it must stay quiet across a boundary now
		g := newClockGuard(10 * time.Second)
		waitUntil(nextMinuteBoundary().Add(70 * time.Second))
		g.tick()
		assertNoRestart(t, g, "Deployment", ns1, name, baseline, 5*time.Second)
	})

	t.Run("DeletionRemovesJobs", func(t *testing.T) {
		const name = "erhudy-test-every-three-minutes"
		deleteWorkload(t, "Deployment", ns1, name)

		err := pollUntil(30*time.Second, func() error {
			entries, err := fetchJobs(p.port)
			if err != nil {
				return err
			}
			if n := len(jobsFor(entries, ns1, name)); n != 0 {
				return fmt.Errorf("%d job entries still registered for deleted %s/%s", n, ns1, name)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("jobs not removed after deletion: %v", err)
		}
	})

	p.stop()
}

func phaseBNamespaceScoping(t *testing.T) {
	applyFixtures(t)
	p := startKairos(t, portNamespace, "-namespace", ns1)

	entries := waitForJobsRegistered(t, p.port, ns1Annotated)
	for _, e := range entries {
		if strings.Contains(e.Resource, "/"+ns2+"/") {
			t.Errorf("namespace-scoped kairos tracked a %s resource: %s", ns2, e.Resource)
		}
	}

	dsBaseline := getRestartAnnotation(t, "DaemonSet", ns1, "erhudy-test-daemonset-every-minute")
	ssBaseline := getRestartAnnotation(t, "StatefulSet", ns2, "erhudy-test-every-minute")
	g := newClockGuard(10 * time.Second)

	boundary := nextMinuteBoundary()
	waitUntil(boundary.Add(time.Second))
	g.tick()
	ts := waitForRestart(t, g, "DaemonSet", ns1, "erhudy-test-daemonset-every-minute", dsBaseline, 75*time.Second)
	assertWindow(t, g, ts, boundary, -2*time.Second, 40*time.Second)

	// out-of-scope resources must not restart across the same boundary window
	assertNoRestart(t, g, "StatefulSet", ns2, "erhudy-test-every-minute", ssBaseline, time.Until(boundary.Add(80*time.Second)))

	p.stop()
}

func phaseCJitter(t *testing.T) {
	applyFixtures(t)
	p := startKairos(t, portJitter, "-jitter", "45s")

	cfg, err := fetchConfig(p.port)
	if err != nil {
		t.Fatalf("fetching /api/config: %v", err)
	}
	if cfg.Jitter != "45s" {
		t.Errorf("expected jitter 45s in config, got %q", cfg.Jitter)
	}

	const name = "erhudy-test-every-minute" // StatefulSet in ns2
	baseline := getRestartAnnotation(t, "StatefulSet", ns2, name)
	g := newClockGuard(10 * time.Second)

	boundary := nextMinuteBoundary()
	waitUntil(boundary.Add(time.Second))
	g.tick()
	ts := waitForRestart(t, g, "StatefulSet", ns2, name, baseline, 45*time.Second)
	// jitter delays the restart past the boundary; clamp keeps it under half the interval (30s)
	assertWindow(t, g, ts, boundary, -1*time.Second, 32*time.Second)

	var jitter time.Duration
	err = pollUntil(15*time.Second, func() error {
		entries, err := fetchJobs(p.port)
		if err != nil {
			return err
		}
		for _, e := range jobsFor(entries, ns2, name) {
			if e.LastJitter == "" {
				continue
			}
			jitter, err = time.ParseDuration(e.LastJitter)
			return err
		}
		return fmt.Errorf("no lastJitter recorded yet for %s/%s", ns2, name)
	})
	if err != nil {
		t.Fatalf("waiting for jitter to be recorded: %v", err)
	}
	if jitter >= 31*time.Second {
		t.Errorf("jitter %v exceeds clamp of half the minute interval", jitter)
	}

	p.stop()
}

func phaseDLookback(t *testing.T) {
	const name = "erhudy-test-every-minute" // StatefulSet in ns2
	baseline := getRestartAnnotation(t, "StatefulSet", ns2, name)

	// kairos is down here; wait through a full firing boundary so one firing is missed
	boundary := nextMinuteBoundary()
	waitUntil(boundary.Add(5 * time.Second))

	// the catch-up premise requires the most recent missed firing to still be inside
	// the 5m lookback window; if far more than a minute passed (e.g. host sleep), skip
	if stale := time.Since(boundary); stale > 4*time.Minute {
		t.Skipf("%v elapsed since the missed firing at %v, outside the 5m lookback window (host slept?)", stale.Round(time.Second), boundary)
	}
	g := newClockGuard(10 * time.Second)

	p := startKairos(t, portLookback, "-lookback", "5m")
	startTime := time.Now()

	cfg, err := fetchConfig(p.port)
	if err != nil {
		t.Fatalf("fetching /api/config: %v", err)
	}
	if cfg.Lookback != "5m0s" {
		t.Errorf("expected lookback 5m0s in config, got %q", cfg.Lookback)
	}

	ts := waitForRestart(t, g, "StatefulSet", ns2, name, baseline, 20*time.Second)
	if ts.Before(startTime.Add(-5 * time.Second)) {
		t.Errorf("catch-up restart timestamp %v predates kairos start %v (not a catch-up?)", ts, startTime)
	}
	if !ts.After(boundary) {
		t.Errorf("expected catch-up restart after missed boundary %v, got %v", boundary, ts)
	}

	p.stop()
}

func phaseEChainedRestarts(t *testing.T) {
	applyFixtures(t)
	p := startKairos(t, portChain, "-chain-timeout", "30s")

	const (
		head    = "erhudy-test-chain-head"
		mid     = "erhudy-test-chain-mid"
		tail    = "erhudy-test-chain-tail"
		stuck   = "erhudy-test-chain-stuck"
		blocked = "erhudy-test-chain-blocked"
	)

	baselines := map[string]string{
		head:    getRestartAnnotation(t, "Deployment", ns1, head),
		mid:     getRestartAnnotation(t, "StatefulSet", ns1, mid),
		tail:    getRestartAnnotation(t, "Deployment", ns1, tail),
		stuck:   getRestartAnnotation(t, "Deployment", ns1, stuck),
		blocked: getRestartAnnotation(t, "Deployment", ns1, blocked),
	}

	t.Run("ConfigAndChainedRegistration", func(t *testing.T) {
		cfg, err := fetchConfig(p.port)
		if err != nil {
			t.Fatalf("fetching /api/config: %v", err)
		}
		if cfg.ChainTimeout != "30s" {
			t.Errorf("expected chain timeout 30s in config, got %q", cfg.ChainTimeout)
		}

		waitForJobsRegistered(t, p.port, [][2]string{{ns1, head}})

		// pure followers have no cron jobs but must show up as chained entries
		err = pollUntil(registrationTimeout, func() error {
			entries, err := fetchJobs(p.port)
			if err != nil {
				return err
			}
			for _, w := range []struct{ name, after string }{
				{mid, "deployment/" + head},
				{tail, "statefulset/" + mid},
				{stuck, "deployment/" + head},
				{blocked, "deployment/" + stuck},
			} {
				var found *jobEntry
				for i := range entries {
					if strings.HasSuffix(entries[i].Resource, "/"+ns1+"/"+w.name) {
						found = &entries[i]
						break
					}
				}
				if found == nil {
					return fmt.Errorf("no chained entry for %s/%s yet", ns1, w.name)
				}
				if found.CronPattern != "" {
					return fmt.Errorf("%s/%s should be a pure follower, got cron pattern %q", ns1, w.name, found.CronPattern)
				}
				if found.RestartAfter != w.after {
					return fmt.Errorf("chained entry for %s/%s has restart-after %q, want %q", ns1, w.name, found.RestartAfter, w.after)
				}
			}
			for _, e := range jobsFor(entries, ns1, tail) {
				if e.RestartAfterMode != "health+wait" || e.RestartAfterWait != "30s" {
					return fmt.Errorf("tail chained entry has mode %q wait %q, want health+wait 30s", e.RestartAfterMode, e.RestartAfterWait)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("chained entries not visible on /api/jobs: %v", err)
		}
	})

	t.Run("CascadeOrdering", func(t *testing.T) {
		g := newClockGuard(10 * time.Second)
		boundary := nextMinuteBoundary()
		waitUntil(boundary.Add(time.Second))
		g.tick()

		headTs := waitForRestart(t, g, "Deployment", ns1, head, baselines[head], 75*time.Second)
		assertWindow(t, g, headTs, boundary, -2*time.Second, 40*time.Second)

		midTs := waitForRestart(t, g, "StatefulSet", ns1, mid, baselines[mid], 90*time.Second)
		if midTs.Before(headTs) {
			t.Errorf("mid restarted at %v before head at %v; cascade must follow the predecessor", midTs, headTs)
		}

		stuckTs := waitForRestart(t, g, "Deployment", ns1, stuck, baselines[stuck], 90*time.Second)
		if stuckTs.Before(headTs) {
			t.Errorf("stuck restarted at %v before head at %v; cascade must follow the predecessor", stuckTs, headTs)
		}

		tailTs := waitForRestart(t, g, "Deployment", ns1, tail, baselines[tail], 3*time.Minute)
		if tailTs.Before(midTs) {
			t.Errorf("tail restarted at %v before mid at %v; cascade must follow the predecessor", tailTs, midTs)
		}
		if gap := tailTs.Sub(midTs); gap < 28*time.Second {
			t.Errorf("tail fired only %v after mid; expected >= the 30s health-plus-wait (minus second-truncation slack)", gap)
		}
		assertWindow(t, g, tailTs, boundary, -2*time.Second, 4*time.Minute)
	})

	t.Run("TimeoutAbortsCascade", func(t *testing.T) {
		g := newClockGuard(10 * time.Second)

		// blocked waits on stuck, whose rollout never lands: its step must time out
		var text string
		err := pollUntil(3*time.Minute, func() error {
			var err error
			text, err = fetchMetrics(p.port)
			if err != nil {
				return err
			}
			v, ok := counterValue(text, "kairos_chain_steps_total", map[string]string{
				"kind":      "Deployment",
				"namespace": ns1,
				"name":      blocked,
				"outcome":   "timeout",
			})
			if !ok || v < 1 {
				return fmt.Errorf("no timeout outcome recorded yet for %s/%s", ns1, blocked)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("waiting for chain timeout metric: %v", err)
		}

		// the abort must not cascade: blocked stays quiet across further head firings
		assertNoRestart(t, g, "Deployment", ns1, blocked, baselines[blocked], 2*time.Minute)

		text, err = fetchMetrics(p.port)
		if err != nil {
			t.Fatalf("fetching /metrics: %v", err)
		}
		for _, w := range []struct{ kind, name string }{
			{"StatefulSet", mid},
			{"Deployment", tail},
			{"Deployment", stuck},
		} {
			v, ok := counterValue(text, "kairos_chain_steps_total", map[string]string{
				"kind":      w.kind,
				"namespace": ns1,
				"name":      w.name,
				"outcome":   "completed",
			})
			if !ok || v < 1 {
				t.Errorf("expected completed chain step for %s/%s, got %v (found=%v)", ns1, w.name, v, ok)
			}
		}
		vBlockedCompleted, _ := counterValue(text, "kairos_chain_steps_total", map[string]string{
			"kind":      "Deployment",
			"namespace": ns1,
			"name":      blocked,
			"outcome":   "completed",
		})
		if vBlockedCompleted != 0 {
			t.Errorf("blocked must never complete a chain step, got %v completions", vBlockedCompleted)
		}
	})

	t.Run("AnnotationRemovalStopsChain", func(t *testing.T) {
		tailBaseline := getRestartAnnotation(t, "Deployment", ns1, tail)
		midBaseline := getRestartAnnotation(t, "StatefulSet", ns1, mid)

		removeWorkloadAnnotation(t, "Deployment", ns1, tail, restartAfterKey)

		err := pollUntil(30*time.Second, func() error {
			entries, err := fetchJobs(p.port)
			if err != nil {
				return err
			}
			if n := len(jobsFor(entries, ns1, tail)); n != 0 {
				return fmt.Errorf("%d entries still registered for %s/%s", n, ns1, tail)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("tail not untracked after restart-after removal: %v", err)
		}

		g := newClockGuard(10 * time.Second)
		boundary := nextMinuteBoundary()
		waitUntil(boundary.Add(time.Second))
		g.tick()

		// the rest of the chain keeps firing...
		midTs := waitForRestart(t, g, "StatefulSet", ns1, mid, midBaseline, 90*time.Second)
		assertWindow(t, g, midTs, boundary, -2*time.Second, 95*time.Second)

		// ...while the detached tail stays quiet well past when it used to fire (~mid+45s)
		assertNoRestart(t, g, "Deployment", ns1, tail, tailBaseline, 75*time.Second)
	})

	p.stop()
}

func assertWindow(t *testing.T, g *clockGuard, ts time.Time, center time.Time, lo, hi time.Duration) {
	t.Helper()
	d := ts.Sub(center)
	if d < lo || d > hi {
		if g != nil && g.Slept {
			t.Skipf("timestamp %v outside window around boundary %v, but host sleep was detected; overdue firings on wake make this inconclusive", ts, center)
		}
		t.Errorf("timestamp %v is outside expected window [%v, %v] around boundary %v", ts, center.Add(lo), center.Add(hi), center)
	}
}
