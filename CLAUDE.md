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

Notable flags: `-timezone` (scheduler timezone), `-jitter` (max random delay before each restart, clamped to 50% of the time until the next firing; 0 disables), `-lookback` (window for catch-up restarts missed while kairos was down; 0 disables), `-metrics-addr` (HTTP server for `/metrics`, `/api/jobs`, `/api/config`, and the job-status web UI at `/`).

## Architecture

Kairos is a Kubernetes controller that automatically restarts workloads (Deployments, DaemonSets, StatefulSets) on cron schedules defined via annotations.

**Trigger annotation**: `kairos.erhudy.com/cron-pattern` on a workload resource.
**Restart mechanism**: Rather than deleting pods directly, Kairos patches `PodTemplateSpec` with a `kairos.erhudy.com/cron-last-restarted-at` timestamp annotation, which causes Kubernetes to roll out new pods naturally.

### Data flow

```
Kubernetes Informer (per resource type)
    → WorkQueue
    → synchronize() [pkg/synchronize.go]
        → checks for cron-pattern annotation
        → sends ObjectAndSchedulerAction on channel (with an ack channel)
        → waits for the scheduler's ack; a failed reconcile is returned so the
          workqueue retries it (failed deletes keep the stashed object for retry)
    → Scheduler.run() [pkg/scheduler.go]
        → reconcileJobsForResource(): add/update/remove gocron jobs
        → checkMissedRestart(): with -lookback, one catch-up restart per resource
          for firings missed while kairos was not running (gated on scheduler startTime)
        → restartFunc(): patches PodTemplateSpec on schedule (after optional jitter sleep)
```

### Key files

- `main.go` — parses flags, wires together controllers + scheduler + HTTP server, shuts down gracefully on SIGINT/SIGTERM
- `pkg/controller.go` — generic Kubernetes controller (informer → workqueue → synchronize); three factory functions for each resource type
- `pkg/synchronize.go` — business logic called per queue item: decides RESOURCE_CHANGE vs RESOURCE_DELETE
- `pkg/scheduler.go` — manages gocron jobs per resource; `reconcileJobsForResource()` diffs current vs desired jobs; `restartFunc()` does the actual patch
- `pkg/types.go` — core types: `Controller`, `Scheduler`, `SchedulerAction`, `resourceIdentifier`
- `pkg/constants.go` — annotation key names and time format (RFC3339)
- `pkg/util.go` — helpers for annotation extraction and object metadata

### Cron pattern format

- 5-field (standard) or 6-field (with seconds) cron expressions via gocron
- Multiple patterns: semicolon-separated (`"0 * * * *;30 * * * *"`)
- Timezone: global `-timezone` flag, or per-pattern prefix (`TZ=America/New_York 0 9 * * *`)

### Concurrency model

- One goroutine per controller (3 total: Deployment, DaemonSet, StatefulSet)
- One scheduler goroutine consuming the shared work channel
- `sync.Map` for resource-to-jobs tracking, with a per-entry `sync.RWMutex` (`resourceMapEntry`) guarding each entry's jobs/lastJitters maps; channels for controller→scheduler communication, and a buffered ack channel on each action so the scheduler reports reconcile failures back to the waiting controller worker (the wait also selects on the controller's stopCh so shutdown isn't blocked by unacked actions)
- gocron fires each job in its own goroutine; jitter sleeps select on the scheduler's shutdown context and re-check job registration afterward, so deleted jobs don't restart and shutdown isn't blocked
- Controllers retry failed items up to 5 times with rate limiting
