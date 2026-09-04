#!/usr/bin/env bash
# Runs the integration test suite against a local kind (docker) cluster.
# Not part of CI: tests are gated behind the `integration` build tag and this
# script is the only thing that passes it.
#
# Usage: hack/run-integration.sh [--keep-cluster]
set -euo pipefail

CLUSTER_NAME="${KAIROS_IT_CLUSTER:-kairos-it}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KEEP_CLUSTER=0
[[ "${1:-}" == "--keep-cluster" ]] && KEEP_CLUSTER=1

for dep in kind docker kubectl go; do
  command -v "$dep" >/dev/null || { echo "missing dependency: $dep" >&2; exit 1; }
done
docker info >/dev/null 2>&1 || { echo "docker daemon is not running" >&2; exit 1; }

CREATED_CLUSTER=0
cleanup() {
  local rc=$?
  if [[ "$KEEP_CLUSTER" == "1" ]]; then
    echo "keeping cluster $CLUSTER_NAME (use: kubectl --context kind-$CLUSTER_NAME ...)"
  elif [[ "$CREATED_CLUSTER" == "1" ]]; then
    echo "deleting kind cluster $CLUSTER_NAME"
    kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
  fi
  exit $rc
}
trap cleanup EXIT

if ! kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  echo "creating kind cluster $CLUSTER_NAME"
  kind create cluster --name "$CLUSTER_NAME"
  CREATED_CLUSTER=1
else
  echo "reusing existing kind cluster $CLUSTER_NAME"
fi

KUBECONFIG_FILE="$(mktemp)"
kind get kubeconfig --name "$CLUSTER_NAME" >"$KUBECONFIG_FILE"
export KUBECONFIG="$KUBECONFIG_FILE"

echo "building kairos binary"
BIN="$REPO_ROOT/bin/kairos-integration"
mkdir -p "$REPO_ROOT/bin"
( cd "$REPO_ROOT" && go build -o "$BIN" . )
export KAIROS_BIN="$BIN"

echo "applying test.yaml"
kubectl apply -f "$REPO_ROOT/hack/test.yaml" >/dev/null

echo "running integration tests (~20 min)"
# on hosts with aggressive sleep settings (e.g. `pmset -c sleep 1`) boundary windows
# may still be interrupted; consider `sudo pmset -c sleep 0` for a reliable run.
cd "$REPO_ROOT"
# caffeinate (macOS) prevents idle system sleep mid-run; a sleeping machine makes
# gocron fire overdue jobs in bursts on wake and breaks minute-boundary assertions.
if command -v caffeinate >/dev/null 2>&1; then
  caffeinate -dis go test -tags=integration -v -timeout 30m ./test/integration/...
else
  go test -tags=integration -v -timeout 30m ./test/integration/...
fi
