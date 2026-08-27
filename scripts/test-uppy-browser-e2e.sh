#!/usr/bin/env sh
set -eu

compose_file="docker-compose.e2e.yml"
compose_project="bucketmux-uppy-browser-e2e"
bucket="${MINISTACK_BUCKET:-bucketmux-uppy-e2e}"
port="${MINISTACK_PORT:-4566}"
repo_root="$(pwd)"
uppy_dir="$repo_root/test/e2e/uppy"

cleanup() {
  docker compose --project-name "$compose_project" --file "$compose_file" down --volumes --remove-orphans
}
trap cleanup EXIT INT TERM

npm ci --prefix "$uppy_dir"
npm run build --prefix "$uppy_dir"
if [ "${PLAYWRIGHT_INSTALL_WITH_DEPS:-0}" = "1" ]; then
  "$uppy_dir/node_modules/.bin/playwright" install --with-deps chromium
else
  "$uppy_dir/node_modules/.bin/playwright" install chromium
fi

docker compose --project-name "$compose_project" --file "$compose_file" up --detach --wait
docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY ministack awslocal s3api create-bucket --bucket "$bucket"

BUCKETMUX_RUN_UPPY_BROWSER_E2E=1 \
MINISTACK_ENDPOINT="${MINISTACK_ENDPOINT:-http://127.0.0.1:$port}" \
MINISTACK_BUCKET="$bucket" \
PLAYWRIGHT_CLI="$uppy_dir/node_modules/.bin/playwright-cli" \
BROWSER_FIXTURE_DIR="$uppy_dir" \
go test ./internal/gateway -run '^TestUppyV6BrowserCompatibility$' -count=1 -v
