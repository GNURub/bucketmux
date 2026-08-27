# APP PACKAGE KNOWLEDGE BASE

## OVERVIEW

`internal/app` is the service orchestration layer. It composes storage, provider adapters, placement routing, secret encryption, worker coordination, hooks, migrations, replication, metrics, and multipart operations.

## WHERE TO LOOK

| Task | Location | Notes |
|------|----------|-------|
| Service bootstrap | `service.go` | `NewService`, `Bootstrap`, adapter registration, worker startup. |
| Object lifecycle | `service.go` | `PutObject`, `GetObject`, `HeadObject`, `DeleteObject`. |
| Multipart business flow | `multipart.go` | Create/upload/list/complete/abort multipart records and temp parts. |
| Replication queue | `replication_queue.go` | Replica statuses and worker loop. |
| Migration jobs | `migration.go` | Copy/move job validation and worker loop. |
| HTTP hooks | `hooks.go` | Hook validation, encrypted headers, retries, delivery worker. |
| Provider health | `provider_health.go` | Health list/check operations. |
| Metrics | `metrics.go` | Prometheus text output. |
| Audit | `audit.go` | Admin/destructive action events. |

## CONVENTIONS

- `NewService` owns composition. Add new provider adapters there with `provider.Entry(kind, adapter)`.
- `Bootstrap` upserts configured buckets and providers into the store; provider `SecretKey` is encrypted before persistence.
- Workers are started by `NewService` and stopped through `Service.Close`; run durable work through `runDurableWorker` so claim, recovery, metrics, cancellation, and wake-up behavior stay centralized.
- Database status transitions own work. Redis leases serialize only the short claim operation and must never be held during provider or hook I/O.
- Long-running work must heartbeat its claimed row so another instance cannot recover it while provider I/O is still active.
- Hook delivery is at-least-once. Receivers can deduplicate with `X-BucketMux-Delivery-ID`; never launch hook goroutines outside the tracked worker lifecycle.
- `PutObject` chooses a primary provider through `Router.Choose`, writes to the provider first, then indexes in `Store`.
- Replication is asynchronous: primary object writes enqueue replica rows when bucket replication targets exist.
- Multipart and replicated uploads may spool to temp files under `Config.Server.DataDir`; always clean temp files.
- Copy migrations create `ObjectReplica`; move migrations rewrite the primary `ObjectRecord` and require confirmation.

## ANTI-PATTERNS

- Do not bypass `Service` from HTTP handlers for business operations unless the handler is intentionally doing store-only admin reads.
- Do not store plaintext provider secrets or hook headers; preserve encryption via `Secrets`.
- Do not add background work without cancellation, `workerWG`, and lease semantics.
- Do not make move migrations without exact `MigrationMoveConfirmationPhrase` = `Migrar permanentemente`.
- Do not assume local temp state is shared across replicas.

## TESTING NOTES

- Package tests construct real `Service` instances with `t.TempDir()` and SQLite.
- Run focused app tests with `go test ./internal/app -count=1 -v`.
- Related cross-package behavior may be tested through gateway/admin httptest suites.
