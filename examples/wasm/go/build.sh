#!/usr/bin/env bash
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
build_dir="$root_dir/examples/wasm/go/build"
mkdir -p "$build_dir"
export GOCACHE="${GOCACHE:-/tmp/go-build}"
export GOMODCACHE="${GOMODCACHE:-/tmp/go/pkg/mod}"

for plugin in metadata-tagger image-dimensions embedding-generator; do
  GOOS=wasip1 GOARCH=wasm go build -trimpath -ldflags="-s -w" -o "$build_dir/$plugin.wasm" "$root_dir/examples/wasm/go/$plugin"
done
