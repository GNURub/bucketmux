# ADMIN PACKAGE KNOWLEDGE BASE

## OVERVIEW

`internal/admin` serves the optional embedded admin UI and admin REST API. It owns Basic Auth checks, the inline HTML template, provider/bucket/hook/migration/object APIs, presign generation, and destructive-action confirmations.

## WHERE TO LOOK

| Task | Location | Notes |
|------|----------|-------|
| Route dispatch | `handler.go:ServeHTTP` | Manual switch over `/admin` and `/admin/api/*`. |
| Auth | `handler.go:authorized` | HTTP Basic Auth from `svc.Config.Admin`. |
| Provider forms/API | `handler.go` | `createProviderFromForm`, `providers`, `providerByID`. |
| Hook forms/API | `handler.go` | `createHookFromForm`, `hooks`, `hookDeliveries`. |
| Object browser/API | `handler.go` | `listAdminObjects`, `presignObjectURL`, `deleteAdminObject`. |
| Migration API | `handler.go:migrations` | Creates copy/move migration jobs. |
| Template | `handler.go:indexTemplate` | Embedded UI, JS, dialog IDs, labels. |

## ROUTES

| Method | Path | Handler |
|--------|------|---------|
| `GET` | `/admin` | Admin page. |
| `POST` | `/admin/providers` | Provider form create/update. |
| `POST` | `/admin/providers/{id}/delete` | Provider form delete. |
| `POST` | `/admin/hooks` | Hook form create/update. |
| `POST` | `/admin/hooks/{id}/delete` | Hook form delete. |
| `POST` | `/admin/buckets` | Bucket form create/update. |
| `POST` | `/admin/upload` | Multipart form object upload. |
| `GET,POST` | `/admin/api/providers` | Provider JSON list/create/update. |
| `DELETE` | `/admin/api/providers/{id}` | Provider JSON delete. |
| `GET,POST` | `/admin/api/hooks` | Hook JSON list/create/update. |
| `DELETE` | `/admin/api/hooks/{id}` | Hook JSON delete. |
| `GET` | `/admin/api/hook-deliveries` | Delivery history. |
| `GET,POST` | `/admin/api/buckets` | Bucket list/create/update. |
| `GET` | `/admin/api/usage` | Provider/bucket usage. |
| `GET` | `/admin/api/provider-health` | Provider health. |
| `GET` | `/admin/api/objects` | Object browser JSON. |
| `GET` | `/admin/api/objects/presign` | Public GET presign. |
| `DELETE` | `/admin/api/objects` | Delete object with confirmation. |
| `GET,POST` | `/admin/api/migrations` | List/create migration jobs. |

## CONVENTIONS

- Admin only mounts when `svc.Config.Admin.Enabled` is true in `internal/httpserver/http.go`.
- Basic Auth is mandatory for every admin route.
- Some form handlers return JSON only when `Accept` contains `application/json`; otherwise they redirect/render HTML flows.
- Object delete confirmation phrase is exactly `Eliminar permanentemente`.
- Move migration confirmation phrase is exactly `Migrar permanentemente` from `internal/app`.
- Presign default expiry is 900 seconds; accepted range is `1..604800`.
- Public presign base URL prefers `server.public_base_url`, then forwarded headers, then request host.
- Empty provider secret/hook header inputs preserve existing encrypted values.

## ANTI-PATTERNS

- Do not change dialog IDs, input names, or confirmation phrases without updating `handler_test.go`.
- Do not expose stored secrets in HTML or JSON; UI copy says existing secrets are never shown.
- Do not add admin endpoints outside the Basic Auth gate.
- Do not assume JSON error responses unless the client asks for JSON.
- Do not bypass audit events for destructive admin operations.

## TESTING NOTES

- Run admin tests with `go test ./internal/admin -count=1 -v`.
- Tests use `app.NewService` with temp SQLite DBs and call handlers through `httptest`.
- `handler_test.go` asserts UI strings, dialog IDs, API shapes, presign behavior, deletion confirmations, and side effects in `svc.Store`.
