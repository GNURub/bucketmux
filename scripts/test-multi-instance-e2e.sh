#!/usr/bin/env sh
set -eu

compose_file="docker-compose.multi-instance-e2e.yml"
compose_project="bucketmux-multi-instance-e2e"
remote_bucket="${MULTI_MINISTACK_BUCKET:-bucketmux-multi-instance}"
replica_bucket="${MULTI_MINISTACK_REPLICA_BUCKET:-bucketmux-multi-instance-replica}"
instance_a_port="${MULTI_A_PORT:-18082}"
instance_b_port="${MULTI_B_PORT:-18083}"
proxy_port="${MULTI_PROXY_PORT:-18084}"
restore_port="${MULTI_RESTORE_PORT:-18085}"
postgres_port="${MULTI_POSTGRES_PORT:-15432}"
repo_root="$(pwd)"
playwright_dir="$repo_root/test/e2e/uppy"
access_key="multi-instance-access"
secret_key="multi-instance-secret"
failover_key="multi-instance/failover.txt"
failover_body="object remains available while either replica is restarted"
multipart_failover_key="multi-instance/multipart-process-failover.txt"
multipart_failover_body="multipart continues on another process with shared staging"
work_dir="$(mktemp -d)"

cleanup() {
  status=$?
  if [ "$status" -ne 0 ]; then
    docker compose --project-name "$compose_project" --file "$compose_file" logs --no-color || true
  fi
  docker compose --project-name "$compose_project" --file "$compose_file" --profile restore-test down --volumes --remove-orphans
  rm -rf "$work_dir"
  exit "$status"
}

header_value() {
  header_file="$1"
  header_name="$2"
  awk -v wanted="$header_name" 'tolower($1) == tolower(wanted ":") { sub(/^[^:]+:[[:space:]]*/, ""); gsub(/\r/, ""); print; exit }' "$header_file"
}

test_multipart_process_failover() {
  curl --fail --silent --show-error \
    --dump-header "$work_dir/create.headers" \
    -X POST "http://127.0.0.1:${proxy_port}/images/${multipart_failover_key}?uploads" \
    -H "Content-Type: text/plain" \
    -H "X-S3LS-Access-Key: ${access_key}" \
    -H "X-S3LS-Secret-Key: ${secret_key}" \
    --output "$work_dir/create.xml"
  upload_id="$(python3 - "$work_dir/create.xml" <<'PY'
import sys
import xml.etree.ElementTree as ET
print(ET.parse(sys.argv[1]).getroot().findtext("UploadId"))
PY
)"
  selected_upstream="$(header_value "$work_dir/create.headers" "X-BucketMux-Upstream")"
  if [ -z "$upload_id" ] || [ -z "$selected_upstream" ]; then
    echo "Multipart failover setup did not return upload ID and upstream" >&2
    return 1
  fi

  curl --fail --silent --show-error \
    --dump-header "$work_dir/part1.headers" \
    -X PUT "http://127.0.0.1:${proxy_port}/images/${multipart_failover_key}?partNumber=1&uploadId=${upload_id}" \
    -H "X-S3LS-Access-Key: ${access_key}" \
    -H "X-S3LS-Secret-Key: ${secret_key}" \
    --data-binary "multipart continues " \
    --output /dev/null
  etag1="$(header_value "$work_dir/part1.headers" "ETag")"

  instance_a_id="$(docker compose --project-name "$compose_project" --file "$compose_file" ps --quiet bucketmux-a)"
  instance_a_ip="$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$instance_a_id")"
  if echo "$selected_upstream" | grep -q "$instance_a_ip"; then
    stopped_service="bucketmux-a"
    stopped_endpoint="http://127.0.0.1:${instance_a_port}"
  else
    stopped_service="bucketmux-b"
    stopped_endpoint="http://127.0.0.1:${instance_b_port}"
  fi
  docker compose --project-name "$compose_project" --file "$compose_file" stop "$stopped_service"

  curl --fail --silent --show-error \
    --dump-header "$work_dir/part2.headers" \
    -X PUT "http://127.0.0.1:${proxy_port}/images/${multipart_failover_key}?partNumber=2&uploadId=${upload_id}" \
    -H "X-S3LS-Access-Key: ${access_key}" \
    -H "X-S3LS-Secret-Key: ${secret_key}" \
    --data-binary "on another process with shared staging" \
    --output /dev/null
  etag2="$(header_value "$work_dir/part2.headers" "ETag")"
  complete_body="<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>${etag1}</ETag></Part><Part><PartNumber>2</PartNumber><ETag>${etag2}</ETag></Part></CompleteMultipartUpload>"
  curl --fail --silent --show-error \
    -X POST "http://127.0.0.1:${proxy_port}/images/${multipart_failover_key}?uploadId=${upload_id}" \
    -H "Content-Type: application/xml" \
    -H "X-S3LS-Access-Key: ${access_key}" \
    -H "X-S3LS-Secret-Key: ${secret_key}" \
    --data-binary "$complete_body" \
    --output /dev/null
  multipart_response="$(curl --fail --silent --show-error \
    "http://127.0.0.1:${proxy_port}/images/${multipart_failover_key}" \
    -H "X-S3LS-Access-Key: ${access_key}" \
    -H "X-S3LS-Secret-Key: ${secret_key}")"
  if [ "$multipart_response" != "$multipart_failover_body" ]; then
    echo "Multipart process failover returned unexpected content" >&2
    return 1
  fi

  docker compose --project-name "$compose_project" --file "$compose_file" start "$stopped_service"
  wait_for_health "$stopped_endpoint"
  curl --fail --silent --show-error \
    -X DELETE "http://127.0.0.1:${proxy_port}/images/${multipart_failover_key}" \
    -H "X-S3LS-Access-Key: ${access_key}" \
    -H "X-S3LS-Secret-Key: ${secret_key}" \
    --output /dev/null
}
trap cleanup EXIT INT TERM

wait_for_health() {
  endpoint="$1"
  attempts=0
  until curl --fail --silent --show-error --max-time 3 "$endpoint/healthz" >/dev/null 2>&1; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 30 ]; then
      echo "Timed out waiting for $endpoint/healthz" >&2
      return 1
    fi
    sleep 1
  done
}

wait_for_proxy_body() {
  attempts=0
  while [ "$attempts" -lt 30 ]; do
    response="$(curl --fail --silent --show-error --max-time 5 \
      "http://127.0.0.1:${proxy_port}/images/${failover_key}" \
      -H "X-S3LS-Access-Key: ${access_key}" \
      -H "X-S3LS-Secret-Key: ${secret_key}" 2>/dev/null || true)"
    if [ "$response" = "$failover_body" ]; then
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 1
  done
  echo "Proxy did not serve the expected object while one replica was unavailable" >&2
  return 1
}

wait_for_replica_provider_read() {
  attempts=0
  while [ "$attempts" -lt 30 ]; do
    if curl --fail --silent --show-error --max-time 15 \
      --dump-header "$work_dir/provider-failover.headers" \
      --output "$work_dir/provider-failover.body" \
      "http://127.0.0.1:${proxy_port}/images/${failover_key}" \
      -H "X-S3LS-Access-Key: ${access_key}" \
      -H "X-S3LS-Secret-Key: ${secret_key}" 2>/dev/null; then
      response="$(cat "$work_dir/provider-failover.body")"
      provider="$(header_value "$work_dir/provider-failover.headers" "X-S3LS-Provider-Account")"
      if [ "$response" = "$failover_body" ] && [ "$provider" = "shared-ministack-replica" ]; then
        return 0
      fi
    fi
    attempts=$((attempts + 1))
    sleep 1
  done
  echo "Proxy did not read the expected object from the replica provider after primary storage failure" >&2
  return 1
}

wait_for_replica() {
  attempts=0
  while [ "$attempts" -lt 30 ]; do
    status="$(docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY postgres \
      psql --username bucketmux --dbname bucketmux --tuples-only --no-align \
      --command "SELECT status FROM object_replicas WHERE bucket='images' AND key='${failover_key}' AND provider_account_id='shared-ministack-replica'" | tr -d '[:space:]')"
    if [ "$status" = "succeeded" ]; then
      return 0
    fi
    attempts=$((attempts + 1))
    sleep 1
  done
  echo "Replica was not completed before the provider failover test" >&2
  return 1
}

npm ci --prefix "$playwright_dir"
if [ "${PLAYWRIGHT_INSTALL_WITH_DEPS:-0}" = "1" ]; then
  "$playwright_dir/node_modules/.bin/playwright" install --with-deps chromium
else
  "$playwright_dir/node_modules/.bin/playwright" install chromium
fi

docker compose --project-name "$compose_project" --file "$compose_file" up --detach --wait ministack ministack-replica postgres redis
docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY ministack awslocal s3api create-bucket --bucket "$remote_bucket"
docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY ministack-replica awslocal s3api create-bucket --bucket "$replica_bucket"
docker compose --project-name "$compose_project" --file "$compose_file" up --build --detach --wait bucketmux-a bucketmux-b proxy

BUCKETMUX_RUN_POSTGRES_INTEGRATION=1 \
BUCKETMUX_EXPECT_PGVECTOR=1 \
POSTGRES_DSN="postgres://bucketmux:multi-instance-postgres@127.0.0.1:${postgres_port}/bucketmux?sslmode=disable" \
GOCACHE=/tmp/go-build \
GOMODCACHE=/tmp/go/pkg/mod \
go test ./internal/store -run '^TestPostgresAtomicQuotaAcrossInstances$' -count=1 -v

BUCKETMUX_RUN_MULTI_INSTANCE_E2E=1 \
MULTI_A_ENDPOINT="http://127.0.0.1:${instance_a_port}" \
MULTI_B_ENDPOINT="http://127.0.0.1:${instance_b_port}" \
MULTI_PROXY_ENDPOINT="http://127.0.0.1:${proxy_port}" \
PLAYWRIGHT_CLI="$playwright_dir/node_modules/.bin/playwright-cli" \
GOCACHE=/tmp/go-build \
GOMODCACHE=/tmp/go/pkg/mod \
go test ./internal/gateway -run '^TestMultiInstanceUserJourneys$' -count=1 -v

test_multipart_process_failover

curl --fail --silent --show-error \
  -X PUT "http://127.0.0.1:${proxy_port}/images/${failover_key}" \
  -H "Content-Type: text/plain" \
  -H "X-S3LS-Access-Key: ${access_key}" \
  -H "X-S3LS-Secret-Key: ${secret_key}" \
  --data-binary "$failover_body" \
  --output /dev/null
wait_for_proxy_body
wait_for_replica

COMPOSE_FILE="$compose_file" COMPOSE_PROJECT="$compose_project" \
  sh scripts/backup-postgres.sh "$work_dir/bucketmux.dump"
COMPOSE_FILE="$compose_file" COMPOSE_PROJECT="$compose_project" \
  sh scripts/restore-postgres.sh "$work_dir/bucketmux.dump" bucketmux_restore
docker compose --project-name "$compose_project" --file "$compose_file" --profile restore-test up --detach --wait bucketmux-restore
restored_body="$(curl --fail --silent --show-error \
  "http://127.0.0.1:${restore_port}/images/${failover_key}" \
  -H "X-S3LS-Access-Key: ${access_key}" \
  -H "X-S3LS-Secret-Key: ${secret_key}")"
if [ "$restored_body" != "$failover_body" ]; then
  echo "Restored database could not retrieve the backed-up object" >&2
  exit 1
fi
docker compose --project-name "$compose_project" --file "$compose_file" stop bucketmux-restore

primary_storage_id="$(docker compose --project-name "$compose_project" --file "$compose_file" ps --quiet ministack)"
docker network disconnect "${compose_project}_default" "$primary_storage_id"
wait_for_replica_provider_read
docker network connect --alias ministack "${compose_project}_default" "$primary_storage_id"
docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY ministack awslocal s3api list-buckets >/dev/null

docker compose --project-name "$compose_project" --file "$compose_file" stop bucketmux-a
wait_for_proxy_body
docker compose --project-name "$compose_project" --file "$compose_file" start bucketmux-a
wait_for_health "http://127.0.0.1:${instance_a_port}"

docker compose --project-name "$compose_project" --file "$compose_file" stop bucketmux-b
wait_for_proxy_body
docker compose --project-name "$compose_project" --file "$compose_file" start bucketmux-b
wait_for_health "http://127.0.0.1:${instance_b_port}"

curl --fail --silent --show-error \
  -X DELETE "http://127.0.0.1:${instance_a_port}/images/${failover_key}" \
  -H "X-S3LS-Access-Key: ${access_key}" \
  -H "X-S3LS-Secret-Key: ${secret_key}" \
  --output /dev/null

echo "Multi-instance certification passed: cross-replica S3, shared multipart failover, admin UI, presigned URL, provider failover, database restore, and single-process failover"
