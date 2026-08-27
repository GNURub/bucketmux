#!/usr/bin/env sh
set -eu

backup_file="${1:?usage: scripts/restore-postgres.sh BACKUP_FILE TARGET_DATABASE}"
target_database="${2:?usage: scripts/restore-postgres.sh BACKUP_FILE TARGET_DATABASE}"
compose_file="${COMPOSE_FILE:-docker-compose.multiple.yml}"
compose_project="${COMPOSE_PROJECT:-bucketmux}"
postgres_service="${POSTGRES_SERVICE:-postgres}"
postgres_user="${POSTGRES_USER:-bucketmux}"
source_database="${POSTGRES_DATABASE:-bucketmux}"

if [ ! -s "$backup_file" ]; then
  echo "Backup does not exist or is empty: $backup_file" >&2
  exit 1
fi
if [ "$target_database" = "$source_database" ]; then
  echo "Refusing to restore over the live database. Restore into a new database first." >&2
  exit 1
fi
case "$target_database" in
  *[!a-zA-Z0-9_-]*|'')
    echo "Target database contains unsupported characters" >&2
    exit 1
    ;;
esac

docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY "$postgres_service" \
  createdb --username "$postgres_user" "$target_database"
docker compose --project-name "$compose_project" --file "$compose_file" exec --no-TTY "$postgres_service" \
  pg_restore --username "$postgres_user" --dbname "$target_database" --no-owner --no-privileges < "$backup_file"

echo "Postgres backup restored into new database $target_database"
