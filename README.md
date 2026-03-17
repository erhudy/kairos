# Kairos

Kairos is a small Kubernetes controller that exists solely to restart pods based on a cron pattern annotation applied to the controlling resource (`Deployment`, `DaemonSet` or `StatefulSet`). It exists because of the long and storied tradition of restarting services on a regular basis because it's easier than fixing memory leaks, and who wants to wait around for pods to get OOMKilled?

## Using

To use, add the annotation `kairos.erhudy.com/cron-pattern` to your `apps/v1` resource. Kairos accepts either 5- or 6-element patterns (with seconds), but if you really need to specify things down to the second, what are you even doing?

Kairos works in a similar manner to [Reloader](https://github.com/stakater/reloader) by adding or updating the annotation `kairos.erhudy.com/cron-last-restarted-at` inside the contained PodTemplateSpec, which will cause Kubernetes to generate a new `ReplicaSet` and turn all the pods. Kairos itself does not do anything with the pods directly. The `kairos.erhudy.com/cron-last-restarted-at` annotation is in RFC 3339 format and may be inspected to determine the last time the pod was restarted via Kairos's machinations.

Bear in mind that as with all pod cycles in Kubernetes, the restarts will not happen instantly, so ensure that you do not set a cron pattern so aggressive that you end up in unending `ReplicaSet` churn.

Kairos also accepts multiple cron patterns in a single annotation, separated by semicolons. Multiple independent restart jobs will be registered for that resource. Kairos does not check whether any of the specified cron patterns overlap or conflict.

### Restart chains

For cases where you need to restart multiple resources in a specific order — waiting for each to become healthy before proceeding to the next — Kairos supports a `RestartChain` custom resource. RestartChain is cluster-scoped and can orchestrate restarts across namespaces.

```yaml
apiVersion: kairos.erhudy.com/v1alpha1
kind: RestartChain
metadata:
  name: nightly-chain
spec:
  cronPattern: "0 2 * * *"
  steps:
    - resourceRef:
        kind: Deployment
        name: frontend
        namespace: production
      healthCheck:
        strategy: RolloutComplete
        timeoutSeconds: 300
        pollIntervalSeconds: 5
    - resourceRef:
        kind: StatefulSet
        name: cache
        namespace: production
      healthCheck:
        strategy: RolloutWithTimeout
        timeoutSeconds: 600
    - resourceRef:
        kind: Deployment
        name: backend
        namespace: production
      healthCheck:
        strategy: FixedTimeout
        timeoutSeconds: 120
```

Each step restarts the referenced resource, then waits for it to become healthy before moving on to the next step. Three health check strategies are available:

- **RolloutComplete** (default): Polls until all replicas are updated and available. Fails the chain if the timeout is exceeded.
- **FixedTimeout**: Waits for the specified duration, then proceeds regardless of rollout status.
- **RolloutWithTimeout**: Polls for rollout completion, but proceeds after the timeout even if the rollout is incomplete (the step is marked with a warning).

Set `spec.paused: true` to suspend future executions without deleting the resource. If a chain's cron fires while a previous execution is still running, the new execution is skipped.

### Timezone handling

Kairos starts up with the scheduler running on local time by default, determined through whatever mechanism Go uses to figure out the local timezone. To override that and specify a particular timezone, indicate it with the `-timezone` flag.

Timezones for particular jobs may be set by prefixing each cron pattern with `TZ=` or `CRON_TZ=`, e.g. `TZ=America/New_York 5 12 * * *`.

## Web UI

Kairos serves a status page on the metrics address (default `:9090`) showing all scheduled jobs and restart chains. JSON endpoints are available at `/api/jobs` and `/api/chains`.

## Installing

Use the Kustomize directory at `deploy`:

```
➜  kairos git:(main) kubectl apply -k deploy/install
serviceaccount/kairos created
clusterrole.rbac.authorization.k8s.io/kairos created
clusterrolebinding.rbac.authorization.k8s.io/kairos created
deployment.apps/kairos created
customresourcedefinition.apiextensions.k8s.io/restartchains.kairos.erhudy.com created
```
