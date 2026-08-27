#!/usr/bin/env sh
set -eu

compose_file="docker-compose.migration-e2e.yml"
compose_project="bucketmux-migration-e2e"
source_bucket="${MINISTACK_SOURCE_BUCKET:-bucketmux-migration-source}"
target_bucket="${MINISTACK_TARGET_BUCKET:-bucketmux-migration-target}"
source_port="${MINISTACK_SOURCE_PORT:-4566}"
target_port="${MINISTACK_TARGET_PORT:-4567}"
repo_root="$(pwd)"
playwright_dir="$repo_root/test/e2e/uppy"

cleanup() {
  docker compose --project-name "$compose_project" --file "$compose_file" down --volumes --remove-orphans
}
trap cleanup EXIT INT TERM

npm ci --prefix "$playwright_dir"
if [ "${PLAYWRIGHT_INSTALL_WITH_DEPS:-0}" = "1" ]; then
  "$playwright_dir/node_modules/.bin/playwright" install --with-deps chromium
else
  "$playwright_dir/node_modules/.bin/playwright" install chromium
fi

docker compose --project-name "$compose_project" --file "$compose_file" up --detach --wait
docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY ministack-source awslocal s3api create-bucket --bucket "$source_bucket"
docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY ministack-target awslocal s3api create-bucket --bucket "$target_bucket"

BUCKETMUX_RUN_MIGRATION_E2E=1 \
MINISTACK_SOURCE_ENDPOINT="${MINISTACK_SOURCE_ENDPOINT:-http://127.0.0.1:$source_port}" \
MINISTACK_TARGET_ENDPOINT="${MINISTACK_TARGET_ENDPOINT:-http://127.0.0.1:$target_port}" \
MINISTACK_SOURCE_BUCKET="$source_bucket" \
MINISTACK_TARGET_BUCKET="$target_bucket" \
PLAYWRIGHT_CLI="$playwright_dir/node_modules/.bin/playwright-cli" \
GOCACHE=/tmp/go-build \
GOMODCACHE=/tmp/go/pkg/mod \
go test ./internal/admin -run '^TestMigrationAPIAndUIAcrossS3LikeAndLocalProviders$' -count=1 -v
