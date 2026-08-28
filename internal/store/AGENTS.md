# STORE PACKAGE KNOWLEDGE BASE

## OVERVIEW

`internal/store` is the persistent metadata layer for providers, buckets, objects, replicas, multipart uploads, hooks, migrations, audit events, and usage. The public single-instance backend is named SQLite and is executed by embedded Turso Database; Postgres is used for multi-instance deployments. The old SQLite driver is not linked.

## WHERE TO LOOK

| Task | Location | Notes |
|------|----------|-------|
| Open DB | `store.go` | `OpenConfig`, public `OpenSQLite`, internal-engine `OpenTurso`, `OpenPostgres`. |
| Schema migrations | `store.go:migrate` | Creates tables/indexes and adds legacy columns. |
| SQL dialect handling | `store.go:rebind` | `?` placeholders become `$N` for Postgres. |
| Provider/bucket/object CRUD | `store.go` | `UpsertProvider`, `UpsertBucket`, `PutObject`; `GetObjectWithProvider` is the joined gateway hot path. |
| Worker claims | `store.go` | Atomic hook/replica/migration claims plus stale-running recovery. |
| Multipart records | `store.go` | `CreateMultipartUpload`, `UpsertMultipartPart`, cleanup. |
| Hooks/audit/usage | `store.go` | Hook deliveries, audit log, provider usage. |

## CONVENTIONS

- Write SQL with `?` placeholders only; call `s.exec`, `s.query`, or `s.queryRow` so Postgres rebinding happens.
- Schema migration runs at startup; additions currently use `CREATE TABLE IF NOT EXISTS` and `addColumnIfMissing`.
- Upserts use `ON CONFLICT ... DO UPDATE` patterns that must remain Turso/Postgres compatible.
- Timestamps are stored as RFC3339Nano strings.
- Booleans are stored as integer `0/1` via `boolToInt`.
- JSON/array/map fields are TEXT columns ending in `_json`.
- Encrypted secrets use `secret_encrypted` or `headers_encrypted`; encryption happens in `app`, not store.
- Not-found reads return exported sentinel `ErrNotFound`.
- Turso defaults to a bounded 10-open/10-idle pool; keep it configurable.
- Store-side caps matter: object listing caps at 1000; hook deliveries cap at 200; migration lists cap at 100.
- Durable workers claim with conditional `pending -> running` updates. Every new running state needs a conditional heartbeat and a stale recovery query.

## ANTI-PATTERNS

- Do not use Postgres-only SQL unless Turso compatibility is intentionally changed everywhere.
- Do not add raw `$1` placeholders; `rebind` is the portability layer.
- Do not silently add columns without considering both dialects and startup migration behavior.
- Do not parse timestamps as local time; keep UTC/RFC3339Nano semantics.
- Do not introduce a second persistence path for object index state.

## TESTING NOTES

- Run store tests with `go test ./internal/store -count=1 -v`.
- Postgres integration is gated: `BUCKETMUX_RUN_POSTGRES_INTEGRATION=1 POSTGRES_DSN="postgres://..." go test ./internal/store -run TestPostgresStoreIntegration -count=1 -v`.
- `postgres_test.go` validates placeholder rebinding; keep it updated when query conventions change.
