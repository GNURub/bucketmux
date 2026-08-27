# PROJECT KNOWLEDGE BASE

**Generated:** 2026-05-22
**Commit:** 63b841c
**Branch:** main

## OVERVIEW

BucketMux is a single-binary Go 1.27 service exposing a practical S3-compatible gateway over multiple storage providers. It owns provider routing, encrypted credentials, object index state, admin operations, hooks, migrations, replication, and optional multi-instance worker coordination.

## STRUCTURE

```text
s3-like-switcher/
├── cmd/bucketmux/        # only executable entrypoint
├── internal/app/         # service orchestration + workers
├── internal/gateway/     # public S3/Uppy HTTP surface
├── internal/admin/       # embedded admin UI + admin API
├── internal/provider/    # pluggable storage adapters
├── internal/store/       # SQLite/Postgres object index + jobs
├── internal/router/      # placement choice rules
├── internal/config/      # YAML/env loading and validation
├── examples/             # compose-mounted configs and nginx sticky proxy
└── docker-compose.*.yml  # single-node and multi-node examples
```

## WHERE TO LOOK

| Task | Location | Notes |
|------|----------|-------|
| Startup flow | `cmd/bucketmux/main.go` | `CONFIG_PATH` -> `config.Load` -> `app.NewService` -> HTTP server. |
| HTTP mount graph | `internal/httpserver/http.go` | `/healthz`, `/metrics`, `/uppy/s3/`, optional `/admin`, root gateway. |
| Core object lifecycle | `internal/app/service.go` | `PutObject`, `GetObject`, `DeleteObject`, bootstrap, worker startup. |
| Public S3 behavior | `internal/gateway/handler.go`, `internal/gateway/auth.go` | Path-style buckets, SigV4, custom `X-S3LS-*` headers, range/CORS. |
| Browser upload helpers | `internal/gateway/uppy.go` | Uppy direct and multipart helper endpoints. |
| Admin UI/API | `internal/admin/handler.go` | Basic Auth, embedded template, REST APIs, destructive confirmations. |
| DB schema and SQL | `internal/store/store.go` | Startup migrations, SQLite/Postgres dialects, job claiming. |
| Provider backends | `internal/provider/` | Adapter interface plus local, S3-compatible, Cloudinary, Vercel Blob. |
| Placement policy | `internal/router/placement.go` | Priority, cost, capacity, `max_object_size_bytes`, `min_free_bytes`. |
| Runtime config | `internal/config/config.go`, `config.example.yaml` | YAML first, then env overrides. |
| Docker/CI | `Dockerfile`, `.github/workflows/docker-image.yml` | Multi-arch GHCR image only; no test workflow. |
| Local/multi-node examples | `docker-compose.single.yml`, `docker-compose.multiple.yml`, `examples/` | Multi-node uses Postgres, Redis, nginx `ip_hash`. |

## CODE MAP

| Symbol | Type | Location | Role |
|--------|------|----------|------|
| `main` | func | `cmd/bucketmux/main.go` | Loads config, starts HTTP server, handles shutdown. |
| `NewHTTPHandler` | func | `internal/httpserver/http.go` | Wires health, metrics, admin, Uppy, and S3 gateway routes. |
| `Service` | struct | `internal/app/service.go` | Runtime composition: store, secrets, providers, router, coordinator, workers. |
| `NewService` | func | `internal/app/service.go` | Opens store, registers adapters, bootstraps config, starts workers. |
| `Store` | struct | `internal/store/store.go` | Persistent metadata/index API over SQLite or Postgres. |
| `Adapter` | interface | `internal/provider/provider.go` | Provider contract: `Put`, `Get`, `Head`, `Delete`, `Health`. |
| `PlacementRouter` | struct | `internal/router/placement.go` | Chooses upload provider from enabled accounts and policies. |
| `Authenticator` | struct | `internal/gateway/auth.go` | Custom header auth, SigV4 headers, presigned URL validation/signing. |
| `Handler` | struct | `internal/gateway/handler.go` | Public S3-compatible API handler. |
| `Handler` | struct | `internal/admin/handler.go` | Admin UI/API handler. |
| `Config` | struct | `internal/config/config.go` | Runtime YAML/env configuration model. |

## CONVENTIONS

- Module path is `github.com/gnurub/bucketmux`; checkout directory may still be `s3-like-switcher`.
- This is an application, not a public Go library: packages live under `internal/`.
- YAML config loads first; env vars override in `internal/config/config.go`.
- Required startup secrets: `MASTER_KEY`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`; admin also needs `ADMIN_USER`/`ADMIN_PASSWORD` when enabled.
- Prefer `store.sqlite.path`; `server.db_path` is deprecated but still supported for SQLite.
- Provider secrets and hook headers are encrypted through `internal/crypto.SecretBox` before persistence.
- Background workers start in `app.NewService`: hooks, migrations, replication.
- Multi-instance mode should use Postgres plus Redis leases; compose mounts separate `/data` volumes per replica.

## ANTI-PATTERNS (THIS PROJECT)

- Do not enable virtual-hosted-style S3 URLs for local deployments unless DNS routes bucket subdomains; use path-style access.
- Do not add instance-local providers in multi-replica deployments unless `/data` is shared across replicas.
- Do not rely on compose defaults for production secrets (`BUCKETMUX_MASTER_KEY`, admin password, Postgres password).
- Do not change destructive confirmation phrases without updating tests and admin copy: `Eliminar permanentemente`, `Migrar permanentemente`.
- Do not write Postgres `$1` placeholders directly in store queries; write `?` and let `Store.rebind` convert.

## UNIQUE STYLES

- Admin UI copy includes Spanish destructive confirmations while code/docs are otherwise English.
- Admin/API code intentionally mixes form flows and JSON APIs in one handler; keep exact route and content-type behavior tested.
- Store timestamps are RFC3339Nano strings; booleans are integer `0/1`; JSON-ish fields are stored as TEXT `*_json`.
- Provider routing policy is data-driven through provider `settings` keys: `cost_per_gb_month`, `max_object_size_bytes`, `min_free_bytes`.

## COMMANDS

```bash
make run-local
make test
GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./...
BUCKETMUX_RUN_BUN_INTEGRATION=1 make test-bun
BUCKETMUX_RUN_SEAWEEDFS_INTEGRATION=1 make test-seaweedfs
BUCKETMUX_RUN_POSTGRES_INTEGRATION=1 POSTGRES_DSN="postgres://..." go test ./internal/store -run TestPostgresStoreIntegration -count=1 -v
docker build -t bucketmux .
docker compose -f docker-compose.single.yml up --build
docker compose -f docker-compose.multiple.yml up --build
```

## NOTES

- `Makefile` test target pins `GOCACHE=/tmp/go-build` and `GOMODCACHE=/tmp/go/pkg/mod`; reuse that in CI-like environments.
- Existing GitHub Actions only builds/pushes the Docker image to GHCR; it does not run `go test ./...`.
- Multipart parts stage under `server.data_dir`; in multi-node mode sticky routing or shared staging storage is required.
- `examples/config.multiple.yaml` intentionally leaves `providers: []`; configure shared remote providers through YAML or admin before production use.
- Child knowledge bases exist for high-risk packages: `internal/app`, `internal/gateway`, `internal/provider`, `internal/store`, `internal/admin`.
