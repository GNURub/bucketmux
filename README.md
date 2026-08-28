# BucketMux

![BucketMux architecture diagram](./image.png)

**BucketMux** is a self-hosted, S3-compatible storage gateway that lets you route objects across multiple storage providers from one API.

It is designed for small teams, side projects, prototypes and cost-conscious deployments that want to combine free-tier or low-cost storage accounts without exposing that complexity to the client application.

BucketMux is **not a SaaS**. You run it as your own Docker container, configure your own provider credentials, and keep the object index and encrypted secrets under your control.

> Status: production-ready baseline for the documented Core S3 surface. BucketMux is not a complete AWS S3 implementation; deploy only against the compatibility scope and operational requirements documented below.

---

## Why BucketMux?

Object storage providers are cheap, but real projects often end up with several accounts or services:

- AWS S3
- Cloudflare R2
- MinIO
- SeaweedFS S3
- IDrive e2, DigitalOcean Spaces and Hetzner Object Storage
- Scaleway, OVHcloud, Akamai and OCI Object Storage
- Microsoft Azure Blob Storage
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
- browser-compatible presigned POST policies
- SigV4 header authentication
- SigV4 presigned URLs
- copy object and multi-object delete
- object metadata and tags
- conditional reads/writes
- versioning, delete markers, retention and legal hold
- bucket notification configuration backed by durable HTTP deliveries
- range reads
- CORS support for browser uploads

### Multiple providers

Supported provider types:

| Provider kind | Use case |
| --- | --- |
| `local` | Store objects on disk inside or mounted into the Docker container. |
| `s3-compatible` | AWS S3, Cloudflare R2, MinIO, SeaweedFS, Backblaze B2, GCS XML/HMAC, IDrive e2, OCI, DigitalOcean Spaces, Hetzner, Scaleway, OVHcloud, Akamai and similar services. |
| `azure-blob` | Microsoft Azure Blob Storage through native Shared Key authentication. |
| `cloudinary` | Media storage through Cloudinary's API adapter. |
| `vercel-blob` | Vercel Blob storage through the Vercel Blob API adapter. |

Multiple accounts of the same provider type are supported.

### Routing and capacity-aware placement

On upload, BucketMux:

1. lists enabled providers,
2. removes providers that are degraded or lack configured, remotely measured or monthly capacity,
3. chooses the lowest priority value,
4. breaks ties by available free capacity,
5. atomically reserves capacity in Turso or Postgres,
6. retries the complete body on another account when the provider reports exhausted quota, throttling, invalid credentials or an outage,
7. commits measured usage and stores the object location in the index.

Reads are transparent because BucketMux knows where every object landed.

### Sandboxed WASM processing

Object-created events can enqueue durable `bucketmux.wasm.v1` pipelines for transforms, derived files, metadata extraction, classification, and typed embeddings (including one vector per detected face). Guests written in Go 1.27, TinyGo, Rust, or Bun 1.4/AssemblyScript run asynchronously with WASI filesystem isolation, import allowlisting, memory/time/stdio/output limits, atomic multi-instance claims, heartbeats, retries, generation deduplication, alerts, and normal BucketMux placement for every derived object. Embeddings are atomically persisted with model provenance and overwrite invalidation. Turso performs native exact vector search for single-instance deployments; production Postgres deployments can require pgvector 0.8+ and receive transactionally synchronized native vectors, migration backfill, partial HNSW indexes per model profile, iterative scans, and cross-instance index coordination. See [the ABI and security guide](docs/wasm-plugins.md) and examples under `examples/wasm`.

Configured capacity is a hard BucketMux limit. Remote usage is reconciled from
provider inventory; adapters with a native quota signal can additionally report
remote capacity. Reservations include in-flight uploads, can retain a safety
margin, expire after interrupted uploads, and enforce an optional UTC monthly
upload allowance. This keeps the displayed available quota honest about its
source instead of presenting an estimate as a provider guarantee.

### Embedded admin

The optional embedded admin can be disabled completely. When enabled, it provides:

- provider CRUD
- searchable provider onboarding with credential and capability validation before enablement
- encrypted provider credentials
- bucket CRUD
- per-bucket replication provider selection
- upload object dialog
- object browser
- public presigned URL generation
- safe object deletion requiring the exact confirmation phrase `Delete permanently`
- provider health checks
- configured/remote quota, in-flight reservations and monthly allowance visibility
- alerts for near/exhausted quota, rejected credentials, degraded providers and exhausted replica retries
- usage by provider and bucket
- migration jobs by bucket/prefix with progress history
- audit log for destructive admin operations
- HTTP hooks/webhooks
- secret webhook headers
- delivery history and retry visibility
- provider connection tests, remote bucket discovery and inventory import/reconciliation
- scoped and expiring S3 access keys with rotation
- OIDC Authorization Code + PKCE login with optional group allowlist
- recoverable trash, lifecycle rules, Object Lock controls and object versions
- durable integrity scans that restore damaged primaries from replicas
- placement simulation and storage cost recommendations
- declarative admin API, OpenAPI document and `bucketmux admin` CLI

Inventory jobs keep the logical BucketMux bucket separate from the remote provider bucket. This allows an existing bucket such as `company-production-assets` to be imported into a concise logical bucket such as `assets` without renaming provider data.

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

### SQLite single-instance and optional Postgres

`store.kind: sqlite` is the default local or single-instance backend. Internally it uses Turso Database as the SQLite-compatible engine, opens existing SQLite files in place, and does not link the former SQLite driver.

Do not open the same embedded Turso file from multiple BucketMux processes. Use Postgres for multiple instances; Turso is intentionally the single-process backend.

Postgres is supported as an optional state backend for higher write concurrency and multi-instance scenarios.

---

## Architecture

```mermaid
flowchart LR
  client[Client app\nS3 SDK / Bun / Uppy / curl]
  proxy[BucketMux\nS3-compatible gateway]
  admin[Embedded admin\noptional]
  db[(Turso or Postgres\nobject index + metadata)]
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
  -e S3_ACCESS_KEY="replace-with-a-unique-access-key" \
  -e S3_SECRET_KEY="replace-with-a-long-random-secret-key" \
  bucketmux
```

The container image does not embed an active configuration or default credentials. It exits immediately unless `MASTER_KEY`, `S3_ACCESS_KEY`, and `S3_SECRET_KEY` are supplied directly or by a mounted configuration file.
Its runtime is a minimal pinned Alpine image required by Turso's embedded native
library, runs as UID/GID 10001, and exposes a built-in healthcheck. If the service listens somewhere other than
the default `:8080`, set `HEALTHCHECK_URL` to its internal `/readyz` URL.

### 4. Run with Docker Compose

BucketMux ships with two compose examples.

Single instance with the SQLite backend (powered by Turso) and a local-disk provider:

```bash
BUCKETMUX_MASTER_KEY="replace-with-a-long-random-secret" \
BUCKETMUX_S3_ACCESS_KEY="replace-with-a-unique-access-key" \
BUCKETMUX_S3_SECRET_KEY="replace-with-a-long-random-secret-key" \
BUCKETMUX_ADMIN_USER="admin" \
BUCKETMUX_ADMIN_PASSWORD="replace-with-a-long-random-password" \
docker compose -f docker-compose.single.yml up --build
```

Open:

```text
http://localhost:8080/admin
```

Multi-instance example with two BucketMux replicas, shared Postgres state and an Nginx reverse proxy:

```bash
BUCKETMUX_MASTER_KEY="replace-with-a-long-random-secret" \
BUCKETMUX_S3_ACCESS_KEY="replace-with-a-unique-access-key" \
BUCKETMUX_S3_SECRET_KEY="replace-with-a-long-random-secret-key" \
BUCKETMUX_ADMIN_USER="admin" \
BUCKETMUX_ADMIN_PASSWORD="replace-with-a-long-random-password" \
BUCKETMUX_POSTGRES_PASSWORD="replace-with-a-long-random-password" \
BUCKETMUX_REDIS_PASSWORD="replace-with-a-long-random-password" \
docker compose -f docker-compose.multiple.yml up --build
```

Files:

| File | Purpose |
| --- | --- |
| [`docker-compose.single.yml`](./docker-compose.single.yml) | One BucketMux instance, SQLite backend powered by Turso, local Docker volume provider. |
| [`docker-compose.multiple.yml`](./docker-compose.multiple.yml) | Two BucketMux instances, shared Postgres, Nginx proxy. |
| [`examples/config.single.yaml`](./examples/config.single.yaml) | Compose config for single-instance mode. |
| [`examples/config.multiple.yaml`](./examples/config.multiple.yaml) | Compose config for multi-instance mode. |
| [`examples/nginx.bucketmux.conf`](./examples/nginx.bucketmux.conf) | Nginx proxy config with sticky routing. |

Important multi-instance note: multipart uploads stage parts in `server.multipart_staging_dir`. The Compose example mounts a shared volume at `/multipart`, so an upload can continue after one BucketMux process stops. On multiple hosts, replace that Docker volume with a shared filesystem such as NFS or a ReadWriteMany persistent volume. Completed objects must use shared remote providers; do not configure instance-local providers unless their data path is also shared.

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
    max_open_conns: 10
    max_idle_conns: 10

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
    versioning_enabled: true
    trash_enabled: true
    trash_retention_days: 30

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
| `MULTIPART_STAGING_DIR` | Multipart staging directory. Defaults to `${DATA_DIR}/multipart`; it must be shared by all replicas. |
| `MAX_UPLOAD_BYTES` | Maximum complete object size. Defaults to 5 GiB. |
| `MAX_MULTIPART_PART_BYTES` | Maximum staged multipart part size. Defaults to 512 MiB. |
| `MAX_ADMIN_BODY_BYTES` | Maximum non-upload admin mutation body. Defaults to 1 MiB. |
| `PUBLIC_BASE_URL` | External base URL used when the admin generates public presigned links behind a reverse proxy. |
| `MASTER_KEY` | Secret used to encrypt stored provider credentials. |
| `S3_ACCESS_KEY` | Gateway access key. |
| `S3_SECRET_KEY` | Gateway secret key. |
| `S3_REGION` | SigV4 region. Defaults to `auto` when empty. |
| `ADMIN_ENABLED` | Enable embedded admin. |
| `ADMIN_USER` | Admin Basic Auth user. |
| `ADMIN_PASSWORD` | Admin Basic Auth password. |
| `ADMIN_OIDC_ENABLED` | Enable OpenID Connect admin login. |
| `ADMIN_OIDC_ISSUER_URL` | OIDC issuer URL used for discovery. |
| `ADMIN_OIDC_CLIENT_ID` | OIDC client ID. |
| `ADMIN_OIDC_CLIENT_SECRET` | OIDC client secret. |
| `ADMIN_OIDC_REDIRECT_URL` | Absolute `/admin/oidc/callback` URL registered with the identity provider. |
| `ADMIN_OIDC_ALLOWED_GROUPS` | Optional comma-separated group allowlist. |
| `ADMIN_OIDC_SESSION_HOURS` | Encrypted admin session duration. Defaults to 8 hours. |
| `STORE_KIND` | `sqlite` or `postgres`. The `sqlite` backend is executed by Turso Database. |
| `SQLITE_PATH` | Embedded SQLite-compatible database path. Existing SQLite files are supported. |
| `SQLITE_MAX_OPEN_CONNS` | Maximum open connections for the embedded backend. Defaults to `10`. |
| `SQLITE_MAX_IDLE_CONNS` | Maximum idle connections for the embedded backend. Defaults to `10`. |
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

BucketMux is compatibility-tested with `@uppy/core` 6.0.0 and
`@uppy/aws-s3` 6.0.0 in a real Chromium browser. It exposes signing helpers
under:

```text
/uppy/s3/*
```

Supported flows:

- the Uppy 6 `signRequest` contract through `POST /uppy/s3/sign`,
- direct upload parameters,
- multipart create,
- multipart part signing,
- multipart list,
- multipart complete,
- multipart abort.

The gateway also returns browser-friendly CORS headers and exposes `ETag`.

Configure Uppy 6 with a signing callback:

```js
import Uppy from '@uppy/core'
import AwsS3 from '@uppy/aws-s3'

const uppy = new Uppy().use(AwsS3, {
  shouldUseMultipart: (file) => file.size > 100 * 1024 * 1024,
  generateObjectKey: (file) => `uploads/${crypto.randomUUID()}-${file.name}`,
  async signRequest(request) {
    const response = await fetch('/api/uploads/sign', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...request, bucket: 'images', expiresIn: 300 }),
    })
    if (!response.ok) throw new Error(`signing failed: ${response.status}`)
    return response.json()
  },
})
```

`/api/uploads/sign` should be an authenticated application endpoint or reverse
proxy that forwards the JSON request to BucketMux `/uppy/s3/sign` and adds the
`X-S3LS-Access-Key` and `X-S3LS-Secret-Key` headers server-side. Never embed
permanent BucketMux credentials in a public browser bundle.

The certification test installs the pinned official npm packages, runs direct
and two-part multipart uploads through BucketMux into MiniStack, downloads both
objects, and compares every byte. Run it with `make test-uppy`.

### imgproxy compatibility

BucketMux is compatibility-certified with `imgproxy` 4.0.14 using imgproxy's
[native S3 image source](https://docs.imgproxy.net/image_sources/amazon_s3). The automated test runs both services in containers and
verifies:

1. upload and byte-identical retrieval of a generated PNG through BucketMux,
2. an AWS SigV4 `GetObject` issued by imgproxy against BucketMux's path-style endpoint,
3. a signed imgproxy transformation URL,
4. a real resize from 8x6 to 4x3 pixels with valid `image/png` output.

The test pins the official multi-architecture imgproxy image by version and
digest. Run the same certification used by CI with `make test-imgproxy`.

Production imgproxy configuration should set `IMGPROXY_USE_S3=true`, point
`IMGPROXY_S3_ENDPOINT` to BucketMux, keep path-style endpoints enabled, restrict
`IMGPROXY_S3_ALLOWED_BUCKETS`, and use strong `IMGPROXY_KEY` and
`IMGPROXY_SALT` values for signed transformation URLs.

### Native Fetch and presigned multipart

BucketMux also runs an independent Chromium E2E using only the browser's native
`window.fetch()` API. It requests a separate presigned URL for every data-plane
operation:

1. create the multipart upload,
2. upload two parts and read their exposed `ETag` headers,
3. list and validate the staged parts,
4. complete the upload,
5. download and compare every byte,
6. delete the object and confirm that a subsequent presigned GET returns `404`.

All those requests use SigV4 query-string signatures; permanent credentials are
sent only to the test signer. Run this flow with `make test-fetch`.

### Provider migration E2E

The migration certification starts two independent MiniStack instances plus a
real temporary local-disk provider. It uploads six source objects through the
gateway, then exercises every direction through both the admin API and the real
admin UI in Chromium:

- S3-like to S3-like,
- local disk to S3-like,
- S3-like to local disk.

Every job uses `move` mode. The test waits for `completed`, validates the job
counters, downloads and byte-compares the migrated object, checks the metadata
now points at the target provider, confirms the physical target exists, and
confirms the physical source was deleted. Run it with `make test-migration`.

### Multi-instance user-journey E2E

`make test-multi-instance` starts two real BucketMux processes backed by shared
Postgres, Redis coordination, shared multipart staging and two independent
MiniStack S3 providers, with Nginx in front. The browser/API certification verifies:

- upload on one replica followed by GET, HEAD, range and list on the other,
- bucket creation shared between replicas and a presigned GET through the proxy,
- multipart creation and first part on one process, followed by process shutdown, second part and completion on the other process,
- admin UI upload, object browsing, public URL access and confirmed deletion,
- asynchronous replication followed by a forced primary-provider outage and a verified read from the replica provider,
- a custom-format `pg_dump`, restore into a new database and object retrieval through a fresh BucketMux process,
- continued reads while each BucketMux process is stopped and restarted in turn.

The replicas deliberately use separate local `/data` volumes and one shared
multipart volume. Ordinary completed objects remain portable through shared
Postgres and remote storage, while in-progress multipart uploads survive process
failover through the shared staging directory.

### k6 performance regression

The pinned Grafana k6 workload exercises BucketMux over real HTTP with a local
disk provider and Turso. Its steady-state mix is 50% GET, 30% HEAD, 10% object
listing, and 10% complete PUT/GET/DELETE lifecycles using 4 KiB objects. It
checks response codes and downloaded byte counts and fails on more than 1%
errors or when these local-gateway latency budgets are exceeded:

- GET and HEAD: p95 below 50 ms and p99 below 100 ms,
- PUT and DELETE: p95 below 100 ms and p99 below 200 ms,
- LIST: p95 below 100 ms and p99 below 200 ms.

Run the default 20-VU, 30-second workload with `make test-k6`. Override it with,
for example, `K6_VUS=50 K6_DURATION=2m make test-k6`. The runner uses an
installed `k6` binary when available and otherwise runs the pinned
`grafana/k6:2.2.0` image. These thresholds detect regressions in the gateway;
they are not a substitute for capacity testing with production hardware and
remote providers.

---

## Multipart uploads

BucketMux supports S3-style multipart uploads:

1. `POST /{bucket}/{key}?uploads`
2. `PUT /{bucket}/{key}?partNumber=N&uploadId=...`
3. `POST /{bucket}/{key}?uploadId=...`
4. `DELETE /{bucket}/{key}?uploadId=...`
5. `GET /{bucket}/{key}?uploadId=...`

Multipart parts are staged under `server.multipart_staging_dir` before completion. It defaults to `${server.data_dir}/multipart`. Size the filesystem for concurrent uploads and mount the same directory on every replica.

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

When an object is uploaded, BucketMux writes the primary object according to placement rules and enqueues replica jobs for the selected providers. Background workers claim pending replicas, copy the object to the target providers, verify size and SHA-256 (downloading the replica when the provider does not return checksum metadata), and update replica status in the object index.

This keeps uploads fast. Database claims ensure that one replica job is owned by one worker at a time; transient failures use exponential backoff, exhausted retries raise a durable alert, and interrupted `running` jobs are returned to the queue after the stale-work timeout.

GET and HEAD first use the indexed primary provider. If that provider is unavailable, BucketMux tries completed, enabled replicas and returns the provider actually used in `X-S3LS-Provider-Account`.

---

## Coordination and metrics

Background work uses atomic database-backed claims for ownership. Active jobs heartbeat their database row, and interrupted hook deliveries, migrations, inventories, repairs and replica jobs are recovered from stale `running` state. For high-load multi-instance deployments, Redis can serialize the short claim operation without holding a lease for the duration of provider I/O:

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

The endpoint includes provider usage, capacity, bucket/provider usage, migration, inventory, repair and WASM job counts, hook delivery counts, recoverable-trash count, and durable-worker failure/success metrics.

HTTP hooks use at-least-once delivery. Every request includes `X-BucketMux-Delivery-ID`; receivers should persist that value and ignore duplicates if processing must be idempotent.

---

## Admin API

All admin API endpoints require an authenticated Basic or OIDC admin session.

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
| `GET /admin/api/provider-quotas` | Configured, remote, reserved, available and monthly quota values. |
| `POST /admin/api/providers/{id}/quota/reconcile` | Reconcile one provider's remote quota and inventory usage now. |
| `GET /admin/api/alerts` | List provider, quota and replication alerts. |
| `GET, POST /admin/api/wasm-plugins` | List or validate and install a sandboxed WASI plugin. Module bytes are never returned by list operations. |
| `POST /admin/api/wasm-plugins/validate` | Compile and validate a plugin without installing it. |
| `DELETE /admin/api/wasm-plugins/{id}` | Delete a plugin and its job history. |
| `GET /admin/api/wasm-plugin-jobs` | Inspect durable plugin execution history. |
| `GET /admin/api/embeddings?bucket={bucket}&key={key}` | List embedding provenance for an object without exposing vector values. |
| `POST /admin/api/embeddings/search` | Run cosine, dot-product, or L2 search through pgvector/HNSW or the bounded exact fallback. |
| `GET /admin/api/embeddings/capabilities` | Report exact/pgvector backend, ANN status and active HNSW profiles. |
| `POST /admin/api/providers/{id}/test` | Test provider credentials and connectivity. |
| `GET /admin/api/providers/{id}/buckets` | Discover remote buckets. |
| `GET, POST /admin/api/inventory-jobs` | Import or reconcile an existing remote inventory. |
| `GET, POST /admin/api/repair-jobs` | Run durable integrity and replica repair scans. |
| `GET, POST /admin/api/access-credentials` | List or create scoped S3 access keys. |
| `POST /admin/api/access-credentials/{id}/rotate` | Atomically rotate a scoped key. |
| `GET /admin/api/trash` | List recoverable deleted objects. |
| `POST /admin/api/trash/{id}/restore` | Restore a recoverable object. |
| `POST /admin/api/lifecycle/run` | Run lifecycle expiration and trash purge now. |
| `GET /admin/api/placement-plan` | Simulate placement and projected storage cost. |
| `GET /admin/api/cost-optimizations` | List estimated provider savings. |
| `GET /admin/api/hooks` | List hooks. |
| `POST /admin/api/hooks` | Create or update hook. |
| `GET /admin/api/hook-deliveries` | Hook delivery history. |
| `GET /admin/api/migrations` | List migration jobs. |
| `POST /admin/api/migrations` | Create a copy/move migration job for a bucket/prefix. |
| `GET /admin/api/objects` | Browse indexed objects. |
| `GET /admin/api/objects/presign` | Generate public presigned GET URL. |
| `DELETE /admin/api/objects` | Delete object after confirmation phrase. |
| `POST /admin/api/declarative/apply` | Apply providers, buckets and hooks from JSON/YAML through the CLI. |
| `GET /admin/openapi.json` | Fetch the OpenAPI 3.1 admin contract. |

Delete object safely:

```bash
curl -u admin:change-me -X DELETE http://localhost:8080/admin/api/objects \
  -H "Content-Type: application/json" \
  -d '{
    "bucket": "images",
    "key": "demo.txt",
    "confirm": "Delete permanently"
  }'
```

The same read APIs and declarative apply workflow are available from the binary:

```bash
export BUCKETMUX_ADMIN_URL=http://localhost:8080
export ADMIN_USER=admin ADMIN_PASSWORD=change-me
bucketmux admin providers
bucketmux admin inventory
bucketmux admin repairs
bucketmux admin openapi
bucketmux admin apply desired-state.yaml
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

## SQLite and Postgres backends

SQLite is the public single-instance backend name and uses Turso as its embedded engine:

```yaml
store:
  kind: "sqlite"
  sqlite:
    path: "/data/switcher.db"
    max_open_conns: 10
    max_idle_conns: 10
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

## Production operations

### Liveness and readiness

- `GET /healthz` is process liveness only. It stays healthy when a dependency fails, so an orchestrator does not create a restart loop.
- `GET /readyz` checks the database, coordination backend and writable multipart staging. It returns `503` while the instance must not receive traffic.
- Provider reachability is visible through `/admin/api/provider-health`; it is not a readiness dependency, because replication and read failover can keep the gateway useful during a provider outage.

The container healthcheck uses `/readyz`. Configure the reverse proxy or load balancer to use the same endpoint.

### PostgreSQL backup and restore

Create a compressed custom-format backup without stopping BucketMux:

```bash
BACKUP_FILE="./backups/bucketmux-$(date +%Y%m%d-%H%M%S).dump" \
make backup-postgres
```

Restore into a new database for validation or disaster recovery:

```bash
BACKUP_FILE="./backups/bucketmux-20260827-130000.dump" \
TARGET_DATABASE="bucketmux_restore" \
make restore-postgres
```

The restore script refuses to overwrite the configured live database. Database backups contain BucketMux metadata and encrypted credentials, not provider object bytes; enable provider-side versioning/replication or independent backups for the underlying storage. Test restores regularly and protect both the dump and `MASTER_KEY`.

### Deployment checklist

- terminate TLS at a trusted proxy and pass the original `Host` and scheme,
- generate unique high-entropy master, gateway, admin, database and Redis secrets,
- use Postgres plus Redis and shared multipart staging for multiple instances,
- use shared remote object providers in multi-instance mode,
- keep upload limits aligned between BucketMux and the reverse proxy,
- send traffic only to `/readyz`-healthy replicas and collect `/metrics`,
- retain rotated application/proxy logs outside ephemeral containers,
- schedule PostgreSQL backups and provider-data protection, then rehearse restore,
- protect the admin route with TLS and preferably a VPN or trusted network,
- pin and scan the exact image digest promoted to production.

---

## GitHub Actions Docker image

The repository includes a workflow that gates and publishes the Docker image to GitHub Container Registry:

```text
.github/workflows/docker-image.yml
```

Behavior:

- pull requests run tests, the race detector, vet, static analysis, vulnerability scans, MiniStack S3 end-to-end, Uppy 6 in Chromium, native Fetch presigned multipart, provider migrations through the API and UI, multi-instance process/provider failover plus database restore, the k6 performance regression, and an image build without pushing,
- pushes to `main` or `master` publish only after all quality and security jobs pass,
- version tags like `v1.2.3` publish semver tags,
- the default branch also publishes `latest`,
- published images include provenance and an SBOM attestation,
- every published image digest is keyless-signed with Sigstore Cosign,
- Dependabot checks GitHub Actions, Go modules and container images weekly,
- images are pushed to:

```text
ghcr.io/<owner>/<repo>
```

Example pull:

```bash
docker pull ghcr.io/<owner>/<repo>:latest
```

The workflow uses GitHub's `GITHUB_TOKEN` with `packages: write`. If the package is not visible after the first push, check the repository/package permissions in GitHub Container Registry.

To prevent merges when CI fails, protect `main` in the GitHub repository ruleset and require these status checks: `Test, race, vet and build`, `Static analysis`, `Vulnerability and image scan`, `MiniStack S3 end-to-end`, `Uppy 6 browser compatibility`, `Fetch presigned multipart`, `imgproxy 4 S3 compatibility`, `Migration API and UI end-to-end`, `Multi-instance HA and restore`, and `k6 performance regression`.

---

## Testing

Source builds and quality gates use Go 1.27.0. The version is pinned in
`.go-version`, `go.mod`, Docker, and GitHub Actions.

Run the full test suite:

```bash
make test
```

Run the same local quality gates used by CI after installing `staticcheck`, `golangci-lint`, and `govulncheck`:

```bash
make ci
```

Or directly:

```bash
GOCACHE=/tmp/go-build GOMODCACHE=/tmp/go/pkg/mod go test ./...
```

Optional integration tests:

```bash
make test-bun
make test-fetch
make test-imgproxy
make test-k6
make test-migration
make test-ministack
make test-multi-instance
make test-uppy
make test-seaweedfs
REDIS_ADDR=127.0.0.1:6379 make test-redis
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

BucketMux limits gateway uploads, multipart parts and admin mutation bodies; rejects cross-site admin mutations; uses constant-time Basic Auth comparison; and emits restrictive browser security headers. The production Nginx example also applies request and connection limits. These controls do not replace TLS, network isolation or external abuse monitoring.

BucketMux encrypts provider secrets at rest using `MASTER_KEY`, but anyone with admin access can configure storage behavior. Protect admin credentials accordingly.

---

## Compatibility scope

BucketMux targets practical Core S3 compatibility. It does not aim to be a full AWS S3 clone.

Implemented and covered by compatibility tests:

- path-style buckets,
- object put/get/head/delete, copy and multi-delete,
- ListObjectsV2 with prefix, delimiter and continuation tokens,
- multipart upload and browser presigned POST,
- SigV4 header auth,
- SigV4 presigned URLs,
- CORS for browser uploads,
- range and conditional reads,
- metadata and object tags,
- versioning and delete markers,
- retention and legal hold,
- S3-shaped bucket event notification configuration.

Not currently implemented:

- ACLs,
- AWS IAM policy documents (BucketMux provides scoped keys and roles instead),
- provider-native S3 replication configuration,
- all AWS-specific error edge cases.

---

## License

BucketMux is licensed under the **GNU Affero General Public License v3.0**.

See [`LICENSE`](LICENSE) for the full license text.
