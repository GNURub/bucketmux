# BucketMux

![BucketMux architecture diagram](./image.png)

**BucketMux** is a self-hosted, S3-compatible storage gateway that lets you route objects across multiple storage providers from one API.

It is designed for small teams, side projects, prototypes and cost-conscious deployments that want to combine free-tier or low-cost storage accounts without exposing that complexity to the client application.

BucketMux is **not a SaaS**. You run it as your own Docker container, configure your own provider credentials, and keep the object index and encrypted secrets under your control.

> Status: active MVP. BucketMux implements a practical Core S3 surface, not every AWS S3 feature.

---

## Why BucketMux?

Object storage providers are cheap, but real projects often end up with several accounts or services:

- AWS S3
- Cloudflare R2
- MinIO
- SeaweedFS S3
- S3-compatible hosting providers
- Cloudinary
- Vercel Blob
- local disk storage

BucketMux gives applications a single S3-like endpoint and handles placement, routing, admin operations, provider health, webhooks, replication targets and presigned access behind the scenes.

```text
Application / SDK / Browser Upload
            |
            v
      BucketMux S3 API
            |
   ---------------------
   |        |          |
 AWS S3    R2       MinIO
   |        |          |
 local index + routing metadata
```

---

## Features

### S3-compatible gateway

BucketMux exposes a path-style S3-compatible API:

- `PUT /{bucket}/{key}`
- `GET /{bucket}/{key}`
- `HEAD /{bucket}/{key}`
- `DELETE /{bucket}/{key}`
- `GET /{bucket}?list-type=2&prefix=...`
- multipart upload flow
- SigV4 header authentication
- SigV4 presigned URLs
- range reads
- CORS support for browser uploads

### Multiple providers

Supported provider types:

| Provider kind | Use case |
| --- | --- |
| `local` | Store objects on disk inside or mounted into the Docker container. |
| `s3-compatible` | AWS S3, Cloudflare R2, MinIO, SeaweedFS, Backblaze B2, GCS XML/HMAC-style endpoints and similar services. |
| `cloudinary` | Media storage through Cloudinary's API adapter. |
| `vercel-blob` | Vercel Blob storage through the Vercel Blob API adapter. |

Multiple accounts of the same provider type are supported.

### Routing and capacity-aware placement

On upload, BucketMux:

1. lists enabled providers,
2. removes providers without enough configured remaining capacity,
3. chooses the lowest priority value,
4. breaks ties by available free capacity,
5. stores the object location in the local index.

Reads are transparent because BucketMux knows where every object landed.

### Embedded admin

The optional embedded admin can be disabled completely. When enabled, it provides:

- provider CRUD
- encrypted provider credentials
- bucket CRUD
- per-bucket replication provider selection
- upload object dialog
- object browser
- public presigned URL generation
- safe object deletion requiring the exact confirmation phrase `Eliminar permanentemente`
- provider health checks
- usage by provider and bucket
- migration jobs by bucket/prefix with progress history
- audit log for destructive admin operations
- HTTP hooks/webhooks
- secret webhook headers
- delivery history and retry visibility

### Presigned public access

From the admin object browser you can generate public, expiring URLs that go through the BucketMux S3 gateway:

```text
https://storage.example.com/images/photo.jpg?X-Amz-Algorithm=AWS4-HMAC-SHA256&...
```

These URLs do **not** expose admin credentials. They are normal SigV4 presigned GET URLs and expire automatically.

### Uppy and browser upload support

BucketMux includes helper endpoints compatible with Uppy AWS S3-style flows:

- direct upload params
- multipart create/sign/list/complete/abort
- CORS headers
- exposed `ETag`

### Bun compatibility

BucketMux works with Bun's built-in S3 client using a custom endpoint in path-style mode.

### Optional Postgres

SQLite is the default and works well for local or single-instance deployments.

Postgres is supported as an optional state backend for higher write concurrency and multi-instance scenarios.

---

## Architecture

```mermaid
flowchart LR
  client[Client app\nS3 SDK / Bun / Uppy / curl]
  proxy[BucketMux\nS3-compatible gateway]
  admin[Embedded admin\noptional]
  db[(SQLite or Postgres\nobject index + metadata)]
  local[Local disk provider]
  s3[AWS S3]
  r2[Cloudflare R2]
  minio[MinIO / SeaweedFS]
  cloudinary[Cloudinary]
  hooks[HTTP hooks]

  client --> proxy
  admin --> proxy
  proxy --> db
  proxy --> local
  proxy --> s3
  proxy --> r2
  proxy --> minio
  proxy --> cloudinary
  proxy --> hooks
```

BucketMux is intentionally simple:

- one Go binary,
- one HTTP server,
- pluggable provider adapters,
- local object index,
- encrypted secrets,
- optional admin.

---

## Quick start

### 1. Copy the example config

```bash
cp config.example.yaml config.yaml
```

Edit `config.yaml` and set at least:

- `server.master_key` or `MASTER_KEY`
- `s3.access_key`
- `s3.secret_key`
- admin credentials if admin is enabled
- providers

### 2. Run locally

```bash
CONFIG_PATH=config.local.yaml \
MASTER_KEY="replace-with-a-long-random-secret" \
go run ./cmd/bucketmux
```

Or use the Makefile:

```bash
make run-local
```

The server listens on `http://localhost:8080` by default.

### 3. Run with Docker

```bash
docker build -t bucketmux .

docker run --rm -p 8080:8080 \
  -v "$PWD/data:/data" \
  -v "$PWD/config.yaml:/config/config.yaml:ro" \
  -e CONFIG_PATH=/config/config.yaml \
  -e MASTER_KEY="replace-with-a-long-random-secret" \
  bucketmux
```

### 4. Run with Docker Compose

BucketMux ships with two compose examples.

Single instance with SQLite and a local-disk provider:

```bash
BUCKETMUX_MASTER_KEY="replace-with-a-long-random-secret" \
BUCKETMUX_ADMIN_PASSWORD="change-me" \
docker compose -f docker-compose.single.yml up --build
```

Open:

```text
http://localhost:8080/admin
```

Multi-instance example with two BucketMux replicas, shared Postgres state and an Nginx reverse proxy:

```bash
BUCKETMUX_MASTER_KEY="replace-with-a-long-random-secret" \
BUCKETMUX_POSTGRES_PASSWORD="bucketmux" \
BUCKETMUX_ADMIN_PASSWORD="change-me" \
docker compose -f docker-compose.multiple.yml up --build
```

Files:

| File | Purpose |
| --- | --- |
| [`docker-compose.single.yml`](./docker-compose.single.yml) | One BucketMux instance, SQLite, local Docker volume provider. |
| [`docker-compose.multiple.yml`](./docker-compose.multiple.yml) | Two BucketMux instances, shared Postgres, Nginx proxy. |
| [`examples/config.single.yaml`](./examples/config.single.yaml) | Compose config for single-instance mode. |
| [`examples/config.multiple.yaml`](./examples/config.multiple.yaml) | Compose config for multi-instance mode. |
| [`examples/nginx.bucketmux.conf`](./examples/nginx.bucketmux.conf) | Nginx proxy config with sticky routing. |

Important multi-instance note: multipart uploads currently stage parts on the instance local `/data` volume before completion. The example Nginx config uses `ip_hash` sticky routing to keep a client on the same backend. For production-grade horizontal scaling, prefer shared object providers and consider shared staging storage or a future distributed multipart coordinator.

---

## Configuration

See [`config.example.yaml`](./config.example.yaml) for a complete example.

Minimal local configuration:

```yaml
server:
  addr: ":8080"
  data_dir: "/data"
  master_key: "change-me-use-a-long-random-secret"

store:
  kind: "sqlite"
  sqlite:
    path: "/data/switcher.db"

s3:
  access_key: "local-access-key"
  secret_key: "local-secret-key"
  region: "auto"

admin:
  enabled: true
  username: "admin"
  password: "change-me"

buckets:
  - name: "images"

providers:
  - id: "local-default"
    name: "Local hard-drive storage"
    kind: "local"
    bucket: "images"
    capacity_bytes: 10737418240
    priority: 100
    enabled: true
    settings:
      path: "/data/local-provider"
      cost_per_gb_month: "0"
      max_object_size_bytes: "0"
      min_free_bytes: "0"
```

### Environment overrides

Common environment variables:

| Variable | Description |
| --- | --- |
| `CONFIG_PATH` | Path to YAML config. |
| `ADDR` | HTTP listen address. |
| `DATA_DIR` | Runtime data directory. |
| `PUBLIC_BASE_URL` | External base URL used when the admin generates public presigned links behind a reverse proxy. |
| `MASTER_KEY` | Secret used to encrypt stored provider credentials. |
| `S3_ACCESS_KEY` | Gateway access key. |
| `S3_SECRET_KEY` | Gateway secret key. |
| `S3_REGION` | SigV4 region. Defaults to `auto` when empty. |
| `ADMIN_ENABLED` | Enable embedded admin. |
| `ADMIN_USER` | Admin Basic Auth user. |
| `ADMIN_PASSWORD` | Admin Basic Auth password. |
| `STORE_KIND` | `sqlite` or `postgres`. |
| `SQLITE_PATH` | SQLite database path. |
| `POSTGRES_DSN` | Postgres connection string. |
| `POSTGRES_MAX_OPEN_CONNS` | Max open Postgres connections. |
| `POSTGRES_MAX_IDLE_CONNS` | Max idle Postgres connections. |
| `COORDINATION_KIND` | `database` or `redis`. Defaults to database-backed job claims. |
| `REDIS_ADDR` | Redis address for optional distributed worker leases. |
| `REDIS_PASSWORD` | Redis password. |
| `REDIS_DB` | Redis database number. |
| `REDIS_KEY_PREFIX` | Prefix for Redis worker lease keys. |
| `REDIS_LEASE_TTL_SECONDS` | Lease TTL for Redis worker coordination. |

---

## Providers

### Local disk

```yaml
providers:
  - id: "local-default"
    kind: "local"
    bucket: "images"
    capacity_bytes: 10737418240
    priority: 100
    enabled: true
    settings:
      path: "/data/local-provider"
```

Local objects are stored under:

```text
{settings.path}/{bucket}/{object-key}
```

Object keys are validated to reject path traversal such as `../secret`.

### Provider routing policies

Provider `settings` can define optional routing policies:

| Setting | Effect |
| --- | --- |
| `cost_per_gb_month` | Lower cost wins when provider priority is tied. |
| `max_object_size_bytes` | Skip the provider for larger objects. |
| `min_free_bytes` | Keep this amount of capacity free after routing an upload. |

### Cloudflare R2 / AWS S3 / MinIO / SeaweedFS

```yaml
providers:
  - id: "r2-main"
    name: "Cloudflare R2 main"
    kind: "s3-compatible"
    endpoint: "https://ACCOUNT_ID.r2.cloudflarestorage.com"
    region: "auto"
    bucket: "images"
    access_key: "..."
    secret_key: "..."
    capacity_bytes: 10737418240
    priority: 10
    enabled: true
```

For MinIO:

```yaml
endpoint: "http://localhost:9000"
region: "us-east-1"
```

For SeaweedFS S3:

```yaml
endpoint: "http://localhost:8333"
region: "us-east-1"
```

### Cloudinary

```yaml
providers:
  - id: "cloudinary-free"
    name: "Cloudinary free"
    kind: "cloudinary"
    bucket: "CLOUD_NAME"
    access_key: "API_KEY"
    secret_key: "API_SECRET"
    capacity_bytes: 1073741824
    priority: 200
    enabled: true
    settings:
      cloud_name: "CLOUD_NAME"
```

Cloudinary is not S3-native. BucketMux uses a provider adapter so clients still talk to BucketMux through the same API.

### Vercel Blob

```yaml
providers:
  - id: "vercel-blob-main"
    name: "Vercel Blob main"
    kind: "vercel-blob"
    bucket: "vercel"
    access_key: ""
    secret_key: "vercel_blob_rw_STOREID_SECRET"
    capacity_bytes: 10737418240
    priority: 50
    enabled: true
    settings:
      access: "public" # or "private"
      # Optional if the token already contains the store id.
      store_id: "STOREID"
```

Vercel Blob is not S3-native. BucketMux stores and retrieves objects through the Vercel Blob API while exposing the same BucketMux S3-compatible surface to clients.

Notes:

- Use a Vercel Blob read-write token as `secret_key`.
- `settings.access` defaults to `public` and may be set to `private`.
- BucketMux sends `x-api-version: 12`, `x-vercel-blob-access`, `x-allow-overwrite: 1`, and `x-content-length` for uploads.
- Vercel Blob pathnames use `/` as folder delimiters and are limited to safe, non-traversing paths.

---

## S3 usage

BucketMux uses path-style URLs.

Upload an object:

```bash
curl -X PUT "http://localhost:8080/images/demo.txt" \
  -H "X-S3LS-Access-Key: local-access-key" \
  -H "X-S3LS-Secret-Key: local-secret-key" \
  -H "Content-Type: text/plain" \
  --data "hello from BucketMux"
```

Read it back:

```bash
curl "http://localhost:8080/images/demo.txt" \
  -H "X-S3LS-Access-Key: local-access-key" \
  -H "X-S3LS-Secret-Key: local-secret-key"
```

List objects:

```bash
curl "http://localhost:8080/images?list-type=2&prefix=demo" \
  -H "X-S3LS-Access-Key: local-access-key" \
  -H "X-S3LS-Secret-Key: local-secret-key"
```

Delete an object:

```bash
curl -X DELETE "http://localhost:8080/images/demo.txt" \
  -H "X-S3LS-Access-Key: local-access-key" \
  -H "X-S3LS-Secret-Key: local-secret-key"
```

---

## Bun example

```ts
import { S3Client } from "bun";

const client = new S3Client({
  endpoint: "http://localhost:8080",
  bucket: "images",
  accessKeyId: "local-access-key",
  secretAccessKey: "local-secret-key",
  region: "auto",
});

await client.write("hello.txt", "hello from Bun");

const file = client.file("hello.txt");
console.log(await file.text());
console.log(await file.exists());

await client.delete("hello.txt");
```

Do not enable virtual-hosted style unless you also configure DNS for bucket subdomains. Use path-style access for local BucketMux deployments.

---

## Uppy integration

BucketMux exposes helper endpoints under:

```text
/uppy/s3/*
```

Supported flows:

- direct upload parameters,
- multipart create,
- multipart part signing,
- multipart list,
- multipart complete,
- multipart abort.

The gateway also returns browser-friendly CORS headers and exposes `ETag`.

---

## Multipart uploads

BucketMux supports S3-style multipart uploads:

1. `POST /{bucket}/{key}?uploads`
2. `PUT /{bucket}/{key}?partNumber=N&uploadId=...`
3. `POST /{bucket}/{key}?uploadId=...`
4. `DELETE /{bucket}/{key}?uploadId=...`
5. `GET /{bucket}/{key}?uploadId=...`

Multipart parts are staged under `server.data_dir` before completion. Size your Docker volume accordingly.

---

## Replication

Buckets can define zero or more replication provider targets:

```yaml
buckets:
  - name: "images"
    replication_enabled: true
    replication_provider_ids:
      - "r2-main"
      - "minio-backup"
```

When an object is uploaded, BucketMux writes the primary object according to placement rules and enqueues replica jobs for the selected providers. Background workers claim pending replicas, copy the object to the target providers and update replica status in the object index.

This keeps uploads fast and makes replication safe for multi-instance deployments.

---

## Coordination and metrics

Background work uses database-backed job claims by default. For high-load multi-instance deployments, enable Redis leases around worker polling:

```yaml
coordination:
  kind: "redis"
  redis:
    addr: "redis:6379"
    key_prefix: "bucketmux"
    lease_ttl_seconds: 5
```

Prometheus-compatible metrics are exposed at:

```text
GET /metrics
```

The endpoint includes provider usage, capacity, bucket/provider usage, migration job counts and hook delivery counts.

---

## Admin API

All admin API endpoints require Basic Auth.

```bash
curl -u admin:change-me http://localhost:8080/admin/api/providers
```

Useful endpoints:

| Endpoint | Description |
| --- | --- |
| `GET /admin/api/providers` | List providers. |
| `POST /admin/api/providers` | Create or update provider. |
| `DELETE /admin/api/providers/{id}` | Delete provider. |
| `GET /admin/api/buckets` | List buckets. |
| `POST /admin/api/buckets` | Create or update bucket. |
| `GET /admin/api/usage` | Storage usage by provider and bucket. |
| `GET /admin/api/provider-health` | Provider health checks. |
| `GET /admin/api/hooks` | List hooks. |
| `POST /admin/api/hooks` | Create or update hook. |
| `GET /admin/api/hook-deliveries` | Hook delivery history. |
| `GET /admin/api/migrations` | List migration jobs. |
| `POST /admin/api/migrations` | Create a copy/move migration job for a bucket/prefix. |
| `GET /admin/api/objects` | Browse indexed objects. |
| `GET /admin/api/objects/presign` | Generate public presigned GET URL. |
| `DELETE /admin/api/objects` | Delete object after confirmation phrase. |

Delete object safely:

```bash
curl -u admin:change-me -X DELETE http://localhost:8080/admin/api/objects \
  -H "Content-Type: application/json" \
  -d '{
    "bucket": "images",
    "key": "demo.txt",
    "confirm": "Eliminar permanentemente"
  }'
```

---

## Hooks

BucketMux can call HTTP hooks after object events.

Supported events:

- `object.created`
- `object.deleted`

Hooks support:

- custom HTTP method,
- encrypted secret headers,
- delivery history,
- retries,
- admin visibility.

---

## Postgres backend

SQLite is the default:

```yaml
store:
  kind: "sqlite"
  sqlite:
    path: "/data/switcher.db"
```

Use Postgres when you need stronger concurrency or plan to scale multiple BucketMux instances:

```yaml
store:
  kind: "postgres"
  postgres:
    dsn: "postgres://bucketmux:bucketmux@postgres:5432/bucketmux?sslmode=disable"
    max_open_conns: 25
    max_idle_conns: 10
```

Run the optional Postgres integration test with an existing database:

```bash
BUCKETMUX_RUN_POSTGRES_INTEGRATION=1 \
POSTGRES_DSN="postgres://bucketmux:bucketmux@localhost:5432/bucketmux?sslmode=disable" \
go test ./internal/store -run TestPostgresStoreIntegration -count=1 -v
```

---

## GitHub Actions Docker image

The repository includes a workflow that builds the Docker image and publishes it to GitHub Container Registry:

```text
.github/workflows/docker-image.yml
```

Behavior:

- pull requests build the image but do not push it,
- pushes to `main` or `master` build and push the image,
- version tags like `v1.2.3` publish semver tags,
- the default branch also publishes `latest`,
- images are pushed to:

```text
ghcr.io/<owner>/<repo>
```

Example pull:

```bash
docker pull ghcr.io/<owner>/<repo>:latest
```

The workflow uses GitHub's `GITHUB_TOKEN` with `packages: write`. If the package is not visible after the first push, check the repository/package permissions in GitHub Container Registry.

---

## Testing

Run the full test suite:

```bash
make test
```

Or directly:

```bash
GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./...
```

Optional integration tests:

```bash
make test-bun
make test-seaweedfs
```

---

## Security notes

BucketMux is a storage gateway. Treat it like infrastructure.

Recommended baseline:

- run behind TLS,
- use a strong `MASTER_KEY`,
- use strong S3 gateway credentials,
- keep admin enabled only on trusted networks,
- put admin behind a reverse proxy/VPN if exposed remotely,
- mount persistent data volumes,
- back up the database,
- rotate provider credentials when needed.

BucketMux encrypts provider secrets at rest using `MASTER_KEY`, but anyone with admin access can configure storage behavior. Protect admin credentials accordingly.

---

## Compatibility scope

BucketMux targets practical Core S3 compatibility. It does not aim to be a full AWS S3 clone.

Implemented or partially implemented:

- path-style buckets,
- object put/get/head/delete,
- list objects v2,
- multipart upload,
- SigV4 header auth,
- SigV4 presigned URLs,
- CORS for browser uploads,
- range reads.

Not currently implemented:

- ACLs,
- bucket policies,
- lifecycle rules,
- object versioning,
- S3 event notifications API,
- all AWS-specific error edge cases.

---

## License

BucketMux is licensed under the **GNU Affero General Public License v3.0**.

See [`LICENSE`](LICENSE) for the full license text.
