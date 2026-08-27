#!/usr/bin/env sh
set -eu
set -C

output_file="${1:?usage: scripts/backup-postgres.sh OUTPUT_FILE}"
compose_file="${COMPOSE_FILE:-docker-compose.multiple.yml}"
compose_project="${COMPOSE_PROJECT:-bucketmux}"
postgres_service="${POSTGRES_SERVICE:-postgres}"
postgres_database="${POSTGRES_DATABASE:-bucketmux}"
postgres_user="${POSTGRES_USER:-bucketmux}"

umask 077
docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY "$postgres_service" \
  pg_dump --username "$postgres_user" --dbname "$postgres_database" --format=custom --compress=9 > "$output_file"

if [ ! -s "$output_file" ]; then
  echo "Postgres backup is empty: $output_file" >&2
  exit 1
fi
echo "Postgres backup written to $output_file"
