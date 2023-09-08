# Kairos

Kairos is a small Kubernetes controller that exists solely to restart pods based on a cron pattern annotation applied to the controlling resource (`Deployment`, `DaemonSet` or `StatefulSet`). It exists because of the long and storied tradition of restarting services on a regular basis because it's easier than fixing memory leaks, and who wants to wait around for pods to get OOMKilled?

## Using

To use, add the annotation `kairos.erhudy.com/cron-pattern` to your `apps/v1` resource. Kairos accepts either 5- or 6-element patterns (with seconds), but if you really need to specify things down to the second, what are you even doing?

Kairos works in a similar manner to [Reloader](https://github.com/stakater/reloader) by adding or updating the annotation `kairos.erhudy.com/cron-last-restarted-at` inside the contained PodTemplateSpec, which will cause Kubernetes to generate a new `ReplicaSet` and turn all the pods. Kairos itself does not do anything with the pods directly. The `kairos.erhudy.com/cron-last-restarted-at` annotation is in RFC 3339 format and may be inspected to determine the last time the pod was restarted via Kairos's machinations.

Bear in mind that as with all pod cycles in Kubernetes, the restarts will not happen instantly, so ensure that you do not set a cron pattern so aggressive that you end up in unending `ReplicaSet` churn.

## Installing

TK - for now get the image from [Docker Hub](https://hub.docker.com/r/erhudy/kairos), put it in the cluster with an appropriate Role+RoleBinding or ClusterRole+ClusterRoleBinding and let 'er rip.

## Todos

* proper release versioning and not just `:latest`
* more comprehensive test suite
* de-duplicate various code paths through unhealthy `reflect` witchcraft
* add a feature to allow backfilling restarts if the controller was down or not running during a time when a cron pattern matched the current time, and the last time the pods were restarted can be determined by inspecting the `kairos.erhudy.com/cron-last-restarted-at` annotation