#!/usr/bin/env bash
# Prints the Go version this project builds with, read from the `go` directive in
# go.mod — the single source of truth. Consumed by hack/docker-build.sh and CI so
# that bumping go.mod is the only edit needed to move the toolchain.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="$(awk '$1 == "go" { print $2; exit }' "$REPO_ROOT/go.mod")"
[[ -n "$version" ]] || { echo "no go directive found in go.mod" >&2; exit 1; }
echo "$version"
