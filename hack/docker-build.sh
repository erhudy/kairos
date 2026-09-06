#!/usr/bin/env bash
# Builds the kairos image with the Go version taken from go.mod.
#
# Usage: hack/docker-build.sh [extra docker build args...]
#   IMAGE=ghcr.io/erhudy/kairos:dev hack/docker-build.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GO_VERSION="$("$REPO_ROOT/hack/go-version.sh")"
VERSION="$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"

exec docker build \
  --build-arg "GO_VERSION=${GO_VERSION}" \
  --build-arg "VERSION=${VERSION}" \
  -t "${IMAGE:-kairos:dev}" \
  "$@" \
  "$REPO_ROOT"
