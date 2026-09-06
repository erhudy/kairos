# Kairos

Kairos is a small Kubernetes controller that exists solely to restart pods based on a cron pattern annotation applied to the controlling resource (`Deployment`, `DaemonSet` or `StatefulSet`). It exists because of the long and storied tradition of restarting services on a regular basis because it's easier than fixing memory leaks, and who wants to wait around for pods to get OOMKilled?

## Using

To use, add the annotation `kairos.erhudy.com/cron-pattern` to your `apps/v1` resource. Kairos accepts either 5- or 6-element patterns (with seconds), but if you really need to specify things down to the second, what are you even doing?

Kairos works in a similar manner to [Reloader](https://github.com/stakater/reloader) by adding or updating the annotation `kairos.erhudy.com/cron-last-restarted-at` inside the contained PodTemplateSpec, which will cause Kubernetes to generate a new `ReplicaSet` and turn all the pods. Kairos itself does not do anything with the pods directly. The `kairos.erhudy.com/cron-last-restarted-at` annotation is in RFC 3339 format and may be inspected to determine the last time the pod was restarted via Kairos's machinations.

Bear in mind that as with all pod cycles in Kubernetes, the restarts will not happen instantly, so ensure that you do not set a cron pattern so aggressive that you end up in unending `ReplicaSet` churn.

Kairos also accepts multiple cron patterns in a single annotation, separated by semicolons. Multiple independent restart jobs will be registered for that resource. Kairos does not check whether any of the specified cron patterns overlap or conflict.

### Timezone handling

Kairos starts up with the scheduler running on local time by default, determined through whatever mechanism Go uses to figure out the local timezone. To override that and specify a particular timezone, indicate it with the `-timezone` flag.

Timezones for particular jobs may be set by prefixing each cron pattern with `TZ=` or `CRON_TZ=`, e.g. `TZ=America/New_York 5 12 * * *`.

### Jitter

With `-jitter` set (e.g. `-jitter 15m`), each firing sleeps for a random duration up to that value before patching, so a fleet of workloads sharing the same pattern does not all roll at the same instant. The jitter is clamped to half the time until the pattern's next firing, so it can never overshoot into the following one. The last jitter applied to each job is shown on the status page.

### Catching up missed restarts

With `-lookback` set (e.g. `-lookback 30m`), Kairos checks each pattern on startup for a firing that fell inside the lookback window while Kairos was not running, and performs at most one catch-up restart per resource. The `kairos.erhudy.com/cron-last-restarted-at` annotation is consulted first, so a workload already restarted after the missed firing is left alone. Catch-up restarts propagate through chains like scheduled ones.

### Chained restarts

Some things only make sense to restart in order: the cache before the app, the app before the worker that drains it. Kairos can chain restarts so that a resource comes back after something else's restart has landed, rather than on its own schedule.

Add `kairos.erhudy.com/restart-after` to a resource naming its predecessor(s). When Kairos successfully restarts a resource, each follower waits for the predecessor's rollout to complete again and is then restarted itself — which in turn triggers that follower's own followers (X → Y → Z). Catch-up restarts from `-lookback` propagate through chains just like scheduled ones.

By default (`health` mode) the follower fires as soon as the predecessor reports a completed rollout; if the predecessor does not become healthy within `-chain-timeout` (default 10m), that step is aborted and the cascade stops there rather than restarting onto an unhealthy dependency. `health-plus-wait` adds a fixed settling delay after health is reached before firing.

```yaml
metadata:
  annotations:
    kairos.erhudy.com/restart-after: deployment/redis-cache            # same namespace
    kairos.erhudy.com/restart-after-mode: health-plus-wait             # default: health
    kairos.erhudy.com/restart-after-wait: 30s                          # required for health-plus-wait
```

Chains compose entirely from per-resource annotations; cycles are rejected at registration with a logged error. A rejected edge is re-evaluated on the next informer resync (`-resync`), so it comes back on its own once the cycle is broken elsewhere. Chained step outcomes are counted by the `kairos_chain_steps_total{kind,namespace,name,outcome}` metric (`completed`, `timeout`, or `aborted`), and pure followers appear on the job-status page without a cron pattern.

A follower may name several predecessors, but the semantics are **after whichever predecessor restarts first**, not "after all of them": at most one chain step is in flight per follower, so when predecessor A's restart lands the follower restarts as soon as A is healthy, and a restart of predecessor B while that step is pending is ignored rather than queued. If two predecessors must both be settled before a follower rolls, chain them in series instead (A → B → follower).

### Annotation reference

| Annotation | Meaning |
| --- | --- |
| `kairos.erhudy.com/cron-pattern` | Restart schedule: 5- or 6-field cron, semicolon-separated multiples, optional per-pattern `TZ=`/`CRON_TZ=` prefix |
| `kairos.erhudy.com/restart-after` | Predecessor(s) to follow: comma/semicolon-separated `kind/name` (same namespace) or `kind/namespace/name`, kind one of `deployment`, `daemonset`, `statefulset` |
| `kairos.erhudy.com/restart-after-mode` | `health` (default) or `health-plus-wait` |
| `kairos.erhudy.com/restart-after-wait` | Post-health settling delay; required for `health-plus-wait`, invalid with `health` |

## Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `-kubeconfig` | in-cluster | Path to a kubeconfig; leave unset when running inside the cluster |
| `-master` | | API server URL override |
| `-namespace` | all | Restrict watching to a single namespace |
| `-timezone` | `Local` | Timezone the scheduler evaluates patterns in (unless a pattern carries its own `TZ=` prefix) |
| `-jitter` | `0` (off) | Maximum random delay before each restart, clamped to 50% of the time until the next firing |
| `-lookback` | `0` (off) | Window to check on startup for firings missed while Kairos was down |
| `-chain-timeout` | `10m` | How long a chained restart waits for its predecessor to become healthy before the cascade is aborted |
| `-resync` | `10m` | Informer resync period; every watched resource is re-reconciled on this interval so rejected chain edges and dropped reconciles recover. `0` disables |
| `-metrics-addr` | `:9090` | Listen address for the metrics endpoint, JSON API, and web UI |
| `-debug` | `false` | Development-style logging at debug level |

## Observability

Everything is served on `-metrics-addr`:

- `/` is a self-refreshing status page listing every tracked resource with its cron pattern, chain predecessors, last restart, last jitter, and next scheduled run, with a filter box.
- `/api/jobs` is the JSON behind that page; `/api/config` reports the effective timezone, jitter, lookback, and chain timeout.
- `/metrics` is a Prometheus endpoint.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `kairos_restart_total` | kind, namespace, name | Successful restarts |
| `kairos_restart_errors_total` | kind, namespace, name, error_phase | Failed restart patches |
| `kairos_restart_duration_seconds` | kind, namespace, name | Histogram of successful patch latency |
| `kairos_chain_steps_total` | kind, namespace, name, outcome | Chained steps by outcome: `completed`, `timeout`, `aborted` |
| `kairos_tracked_resources` | kind | Resources currently carrying a Kairos annotation |
| `kairos_scheduled_jobs` | kind | Cron jobs currently registered |
| `kairos_queue_depth` | kind | Controller workqueue depth |
| `kairos_sync_errors_total` | kind | Reconciles dropped after exhausting retries |

The deploy manifests expose port 9090 through a `kairos` Service and use `/api/config` for liveness and readiness.

## Installing

Use the Kustomize directory at `deploy`:

```
➜  kairos git:(main) kubectl apply -k deploy/install
serviceaccount/kairos created
clusterrole.rbac.authorization.k8s.io/kairos created
clusterrolebinding.rbac.authorization.k8s.io/kairos created
service/kairos created
deployment.apps/kairos created
```

This installs into `kube-system`, pulls the release image from `ghcr.io/erhudy/kairos` at the tag pinned in `deploy/install/kustomization.yaml`, and creates a `kairos` Service on port 9090 fronting the metrics endpoint and web UI.

## Development

```bash
go build ./...
go test -race ./...
golangci-lint run
hack/docker-build.sh            # container image, Go version taken from go.mod
hack/run-integration.sh         # end-to-end suite against a local kind cluster (~25 min)
```

## Todos

* de-duplicate various code paths through unhealthy `reflect` witchcraft
