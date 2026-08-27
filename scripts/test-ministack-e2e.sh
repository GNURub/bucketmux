#!/usr/bin/env sh
set -eu

compose_file="docker-compose.e2e.yml"
compose_project="bucketmux-ministack-e2e"
bucket="${MINISTACK_BUCKET:-bucketmux-e2e}"
port="${MINISTACK_PORT:-4566}"

cleanup() {
  docker compose --project-name "$compose_project" --file "$compose_file" down --volumes --remove-orphans
}
trap cleanup EXIT INT TERM

docker compose --project-name "$compose_project" --file "$compose_file" up --detach --wait
docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY ministack awslocal s3api create-bucket --bucket "$bucket"

BUCKETMUX_RUN_MINISTACK_E2E=1 \
MINISTACK_ENDPOINT="${MINISTACK_ENDPOINT:-http://127.0.0.1:$port}" \
MINISTACK_BUCKET="$bucket" \
go test ./internal/gateway -run '^TestMiniStackS3EndToEnd$' -count=1 -v
