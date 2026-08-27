#!/usr/bin/env sh
set -eu

compose_file="docker-compose.e2e.yml"
compose_project="bucketmux-fetch-browser-e2e"
bucket="${MINISTACK_BUCKET:-bucketmux-fetch-e2e}"
port="${MINISTACK_PORT:-4566}"
repo_root="$(pwd)"
fixture_dir="$repo_root/test/e2e/uppy"

cleanup() {
  docker compose --project-name "$compose_project" --file "$compose_file" down --volumes --remove-orphans
}
trap cleanup EXIT INT TERM

npm ci --prefix "$fixture_dir"
npm run build --prefix "$fixture_dir"
if [ "${PLAYWRIGHT_INSTALL_WITH_DEPS:-0}" = "1" ]; then
  "$fixture_dir/node_modules/.bin/playwright" install --with-deps chromium
else
  "$fixture_dir/node_modules/.bin/playwright" install chromium
fi

docker compose --project-name "$compose_project" --file "$compose_file" up --detach --wait
docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY ministack awslocal s3api create-bucket --bucket "$bucket"

BUCKETMUX_RUN_FETCH_BROWSER_E2E=1 \
MINISTACK_ENDPOINT="${MINISTACK_ENDPOINT:-http://127.0.0.1:$port}" \
MINISTACK_BUCKET="$bucket" \
PLAYWRIGHT_CLI="$fixture_dir/node_modules/.bin/playwright-cli" \
BROWSER_FIXTURE_DIR="$fixture_dir" \
go test ./internal/gateway -run '^TestFetchPresignedMultipartBrowserCompatibility$' -count=1 -v
