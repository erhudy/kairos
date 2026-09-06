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

Chains compose entirely from per-resource annotations; cycles are rejected at registration with a logged error. Chained step outcomes are counted by the `kairos_chain_steps_total{kind,namespace,name,outcome}` metric (`completed`, `timeout`, or `aborted`), and pure followers appear on the job-status page without a cron pattern.

### Annotation reference

| Annotation | Meaning |
| --- | --- |
| `kairos.erhudy.com/cron-pattern` | Restart schedule: 5- or 6-field cron, semicolon-separated multiples, optional per-pattern `TZ=`/`CRON_TZ=` prefix |
| `kairos.erhudy.com/restart-after` | Predecessor(s) to follow: comma/semicolon-separated `kind/name` (same namespace) or `kind/namespace/name`, kind one of `deployment`, `daemonset`, `statefulset` |
| `kairos.erhudy.com/restart-after-mode` | `health` (default) or `health-plus-wait` |
| `kairos.erhudy.com/restart-after-wait` | Post-health settling delay; required for `health-plus-wait`, invalid with `health` |

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

## Todos

* proper release versioning and not just `:latest`
* more comprehensive test suite
* de-duplicate various code paths through unhealthy `reflect` witchcraft
* add a feature to allow backfilling restarts if the controller was down or not running during a time when a cron pattern matched the current time, and the last time the pods were restarted can be determined by inspecting the `kairos.erhudy.com/cron-last-restarted-at` annotation
* Prometheus metrics
* better logging