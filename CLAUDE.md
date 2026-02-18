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

# Run locally against a cluster (kubeconfig required)
go run main.go -kubeconfig ~/.kube/config
```

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
        → sends ObjectAndSchedulerAction on channel
    → Scheduler.run() [pkg/scheduler.go]
        → reconcileJobsForResource(): add/update/remove gocron jobs
        → restartFunc(): patches PodTemplateSpec on schedule
```

### Key files

- `main.go` — parses flags, wires together controllers + scheduler, handles SIGUSR1 to dump job status
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
- `sync.Map` for resource-to-jobs tracking; channels for controller→scheduler communication
- Controllers retry failed items up to 5 times with rate limiting
