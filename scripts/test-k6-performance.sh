#!/usr/bin/env sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
work_dir="$(mktemp -d "${TMPDIR:-/tmp}/bucketmux-k6.XXXXXX")"
binary="$work_dir/bucketmux"
server_log="$work_dir/server.log"
target_port="${K6_TARGET_PORT:-18089}"
base_url="${K6_BASE_URL:-http://127.0.0.1:$target_port}"
server_pid=""

cleanup() {
  if [ -n "$server_pid" ]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf -- "$work_dir"
}
trap cleanup EXIT INT TERM

GOTOOLCHAIN=go1.27.0 CGO_ENABLED=0 go build -trimpath -o "$binary" ./cmd/bucketmux
(
  cd "$work_dir"
  CONFIG_PATH="$repo_root/test/performance/config.yaml" \
  ADDR="127.0.0.1:$target_port" \
  DATA_DIR="$work_dir/data" \
  DB_PATH="$work_dir/data/bucketmux.db" \
  exec "$binary"
) >"$server_log" 2>&1 &
server_pid=$!

attempt=0
until curl --fail --silent "$base_url/healthz" >/dev/null 2>&1; do
  attempt=$((attempt + 1))
  if ! kill -0 "$server_pid" 2>/dev/null; then
    echo "BucketMux stopped before becoming healthy" >&2
    sed -n '1,200p' "$server_log" >&2
    exit 1
  fi
  if [ "$attempt" -ge 100 ]; then
    echo "BucketMux did not become healthy" >&2
    sed -n '1,200p' "$server_log" >&2
    exit 1
  fi
  sleep 0.1
done

if command -v k6 >/dev/null 2>&1; then
  k6 run \
    -e BASE_URL="$base_url" \
    -e BUCKETMUX_K6_VUS="${K6_VUS:-20}" \
    -e BUCKETMUX_K6_DURATION="${K6_DURATION:-30s}" \
    -e BUCKETMUX_K6_SEED_OBJECTS="${K6_SEED_OBJECTS:-100}" \
    -e BUCKETMUX_K6_PAYLOAD_BYTES="${K6_PAYLOAD_BYTES:-4096}" \
    "$repo_root/test/performance/s3_workload.js"
else
  docker run --rm --network host \
    -v "$repo_root/test/performance:/performance:ro" \
    -e BASE_URL="$base_url" \
    -e BUCKETMUX_K6_VUS="${K6_VUS:-20}" \
    -e BUCKETMUX_K6_DURATION="${K6_DURATION:-30s}" \
    -e BUCKETMUX_K6_SEED_OBJECTS="${K6_SEED_OBJECTS:-100}" \
    -e BUCKETMUX_K6_PAYLOAD_BYTES="${K6_PAYLOAD_BYTES:-4096}" \
    grafana/k6:2.2.0 run /performance/s3_workload.js
fi
