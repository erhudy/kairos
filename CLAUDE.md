# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Build
go build ./...

# Run all tests
go test ./...

# Run tests with verbose output
go test -v ./...

# Run a single test
go test -v ./pkg -run TestRestartFunc

# Lint
golangci-lint run

# Run locally against a cluster (kubeconfig required)
go run main.go -kubeconfig ~/.kube/config
```

Notable flags: `-timezone` (scheduler timezone), `-jitter` (max random delay before each restart, clamped to 50% of the time until the next firing; 0 disables), `-lookback` (window for catch-up restarts missed while kairos was down; 0 disables), `-chain-timeout` (how long a chained restart waits for its predecessor to become healthy again before aborting the cascade; default 10m), `-metrics-addr` (HTTP server for `/metrics`, `/api/jobs`, `/api/config`, and the job-status web UI at `/`).

## Architecture

Kairos is a Kubernetes controller that automatically restarts workloads (Deployments, DaemonSets, StatefulSets) on cron schedules defined via annotations.

**Trigger annotation**: `kairos.erhudy.com/cron-pattern` on a workload resource.
**Chain annotations**: `kairos.erhudy.com/restart-after` (predecessors, comma/semicolon-separated `kind/name` or `kind/namespace/name`), `-restart-after-mode` (`health` default, or `health-plus-wait`), and `-restart-after-wait` (post-health settle duration; required for `health-plus-wait`, invalid with `health`). A follower restarts after each predecessor's restart lands and its rollout completes again; chains compose per-resource and recurse (X → Y → Z). Cycles are rejected at edge registration.
**Restart mechanism**: Rather than deleting pods directly, Kairos patches `PodTemplateSpec` with a `kairos.erhudy.com/cron-last-restarted-at` timestamp annotation, which causes Kubernetes to roll out new pods naturally.

### Data flow

```
Kubernetes Informer (per resource type)
    → WorkQueue
    → synchronize() [pkg/synchronize.go]
        → checks for cron-pattern / restart-after annotations
        → sends ObjectAndSchedulerAction on channel (with an ack channel)
        → waits for the scheduler's ack; a failed reconcile is returned so the
          workqueue retries it (failed deletes keep the stashed object for retry)
    → Scheduler.run() [pkg/scheduler.go]
        → reconcileJobsForResource(): add/update/remove gocron jobs; every phase runs
          even if an earlier one failed (errors are joined, not returned early), so a
          bad pattern cannot block other patterns, stale-job deletion, or chain edges
        → reconcileChainEdges(): rebuild this resource's follower edges (validation,
          cycle detection). An edge is owned by the follower that declared
          restart-after: only the follower's own reconcile/delete drops it. A
          predecessor's delete leaves chainMap[pred] intact (the edges are inert while
          it cannot fire), so churn on the predecessor — losing its cron-pattern, being
          recreated by a redeploy — does not silently orphan its followers
        → checkMissedRestart(): with -lookback, one catch-up restart per resource
          for firings missed while kairos was not running (gated on scheduler startTime)
        → fireRestart() = restartFunc() + triggerFollowers(): patches PodTemplateSpec
          on schedule (after optional jitter sleep); a successful patch spawns one
          chain step goroutine per follower (deduped to one in-flight step per follower)
        → runChainStep(): poll predecessor rollout status until healthy (capped by
          -chain-timeout; timeout aborts the cascade), optionally settle-wait, then
          restart the follower — which recursively triggers its own followers
```

### Key files

- `main.go` — parses flags, wires together controllers + scheduler + HTTP server, shuts down gracefully on SIGINT/SIGTERM
- `pkg/controller.go` — generic Kubernetes controller (informer → workqueue → synchronize); three factory functions for each resource type
- `pkg/synchronize.go` — business logic called per queue item: decides RESOURCE_CHANGE vs RESOURCE_DELETE
- `pkg/scheduler.go` — manages gocron jobs and chain edges per resource; `reconcileJobsForResource()` diffs current vs desired jobs; `restartFunc()` does the actual patch; `runChainStep()`/`isRolloutComplete()` implement the health-gated cascade
- `pkg/types.go` — core types: `Controller`, `Scheduler`, `SchedulerAction`, `resourceIdentifier`, `chainEdge`/`chainMapEntry`
- `pkg/constants.go` — annotation key names, chain mode/outcome strings, and time format (RFC3339)
- `pkg/util.go` — helpers for annotation extraction, object metadata, and predecessor-ref parsing (`parsePredecessorRefs`)

### Cron pattern format

- 5-field (standard) or 6-field (with seconds) cron expressions via gocron
- Multiple patterns: semicolon-separated (`"0 * * * *;30 * * * *"`)
- Timezone: global `-timezone` flag, or per-pattern prefix (`TZ=America/New_York 0 9 * * *`)

### Concurrency model

- One goroutine per controller (3 total: Deployment, DaemonSet, StatefulSet)
- One scheduler goroutine consuming the shared work channel
- `sync.Map` for resource-to-jobs tracking, with a per-entry `sync.RWMutex` (`resourceMapEntry`) guarding each entry's jobs/lastJitters maps; channels for controller→scheduler communication, and a buffered ack channel on each action so the scheduler reports reconcile failures back to the waiting controller worker (the wait also selects on the controller's stopCh so shutdown isn't blocked by unacked actions)
- gocron fires each job in its own goroutine; jitter sleeps select on the scheduler's shutdown context and re-check job registration afterward, so deleted jobs don't restart and shutdown isn't blocked
- `chainMap` (predecessor → follower edges) uses the same per-entry mutex pattern; a `pendingSteps` sync.Map dedupes to one in-flight chain step per follower; chain-step health polls and settle waits select on the scheduler's shutdown context so shutdown isn't blocked by waiting cascades
- Controllers retry failed items up to 5 times with rate limiting
