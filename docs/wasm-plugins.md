# Sandboxed WASM processing pipelines

BucketMux can run asynchronous object transforms and classifiers after a successful upload. The upload path only commits the source object and a durable job. One BucketMux instance later claims that job atomically, heartbeats it while running, retries transient failures with backoff, and records the result for every instance to inspect.

This design is suitable for thumbnails, manifests, metadata extraction, deterministic format conversion, calls into WASM-packaged codecs, and classification with compact models. The bundled embedding matcher shows the seam for face similarity, but expects embeddings produced by a separate detector/model. Large FFmpeg builds and GPU inference are better implemented later as another executor adapter behind the same job and result model.

## Security model

- wazero runs in-process without CGO and interrupts execution when the configured context deadline expires.
- Every module receives a fixed linear-memory ceiling.
- No host environment variables, network sockets, subprocesses, host clocks, or host filesystem are inherited.
- `/input` is a read-only preopen containing only `object`.
- `/output` is a fresh per-job directory. Results must declare every regular file they wrote; traversal, absolute paths, symlinks, undeclared files, excess aggregate size, and more than 32 outputs are rejected.
- Standard output is capped at 1 MiB and must be exactly one ABI result. Standard error is capped at 64 KiB.
- Imports are restricted to `wasi_snapshot_preview1` and AssemblyScript's inert `env.abort`, `env.trace`, and `env.seed` helpers.
- Derived objects cannot overwrite their source and have WASM recursion disabled. They still use normal provider placement, quota reservations, replication, checksums, hooks, and write failover.
- Bucket operations are declarative commands, not host calls. The guest receives only its capability grant; BucketMux validates the complete manifest after exit and never exposes provider credentials, sockets, or an unrestricted storage API.
- Operations are denied by default, limited to 32 per invocation, scoped by bucket patterns and key prefixes, and recorded in the audit log. Object deletion still honors versioning, trash, retention, and legal holds.

The output byte limit is verified after the guest exits. Operators should also size and monitor `server.data_dir`, because a malicious module can consume temporary disk up to the filesystem's own limit before that verification runs.

## ABI v1

A plugin is a WASI Preview 1 command exporting `_start`. BucketMux writes one JSON object to stdin:

```json
{
  "abi_version": "bucketmux.wasm.v1",
  "event": "object.created",
  "job_id": "wasm-job-...",
  "object": {
    "bucket": "images",
    "key": "photo.jpg",
    "size": 1234,
    "content_type": "image/jpeg",
    "checksum_sha256": "...",
    "metadata": {},
    "tags": {},
    "input_path": "/input/object"
  },
  "workspace": {"output_dir": "/output"},
  "config": {},
  "capabilities": {
    "operations": {
      "allowed_operations": ["metadata.patch", "tags.patch", "object.copy"],
      "bucket_patterns": ["archive-*"],
      "key_prefixes": ["incoming/", "processed/"],
      "max_operations": 8
    }
  }
}
```

The guest writes one JSON result to stdout:

```json
{
  "abi_version": "bucketmux.wasm.v1",
  "metadata": {"model": "classifier-v3"},
  "tags": {"category": "portrait"},
  "embeddings": [
    {
      "kind": "face",
      "model": "arcface",
      "model_version": "2026-01",
      "metric": "cosine",
      "dimensions": 3,
      "values": [0.012, -0.034, 0.998],
      "metadata": {"box": "120,80,220,220"}
    }
  ],
  "derived_objects": [
    {
      "path": "preview.webp",
      "key_suffix": ".preview.webp",
      "content_type": "image/webp",
      "metadata": {},
      "tags": {}
    }
  ],
  "operations": [
    {
      "id": "mark-classified",
      "type": "metadata.patch",
      "metadata": {"classification-state": "complete"},
      "remove_metadata": ["classification-pending"]
    },
    {
      "id": "archive-original",
      "type": "object.copy",
      "bucket": "archive-eu",
      "key": "processed/photo.jpg",
      "metadata": {"archived-by": "classifier-v3"}
    }
  ]
}
```

`path` is relative to `/output`. A result may specify an explicit object `key`, a `key_suffix`, or neither; the last form uses BucketMux's deterministic derived-object namespace.

### Lambda-like bucket operations

The result manifest supports four operations:

| Type | Effect |
|---|---|
| `metadata.patch` | Adds/replaces `metadata` and removes names in `remove_metadata`. The source object is the default target. |
| `tags.patch` | Adds/replaces `tags` and removes names in `remove_tags`. The source object is the default target. |
| `object.copy` | Streams `source_bucket`/`source_key` (the invocation object by default) through normal BucketMux reads and writes it to `bucket`/`key`. Metadata/tag patches are optional. |
| `object.delete` | Deletes `bucket`/`key` (the invocation object by default) through the protected object lifecycle. |

Plugins cannot create buckets or bypass placement, quota reservations, write failover, replication, checksums, object locks, trash, or versioning. Copies and derived files set the internal recursion guard, so they do not create an accidental infinite pipeline. Cross-bucket access requires a matching `bucket_patterns` grant; when `key_prefixes` is present, every non-source object must match one of those prefixes. Empty `allowed_operations` means no bucket-operation access.

Install grants use this schema:

```json
{
  "operation_policy": {
    "allowed_operations": [
      "metadata.patch",
      "tags.patch",
      "object.copy",
      "object.delete"
    ],
    "bucket_patterns": ["archive-*", "review"],
    "key_prefixes": ["incoming/", "processed/"],
    "max_operations": 8
  }
}
```

Policy fields have these meanings:

| Field | Meaning |
|---|---|
| `allowed_operations` | Exact operation types the guest may return. Empty or omitted denies all operations. |
| `bucket_patterns` | Glob patterns authorizing buckets other than the invocation bucket. The invocation bucket remains in scope. |
| `key_prefixes` | Optional prefixes applied to every non-source key, including copy sources and targets. Empty allows any key inside an authorized bucket. |
| `max_operations` | Per-result limit. Defaults to 16 and cannot exceed the hard limit of 32. |

The invocation source object is always an eligible target once its operation type is granted. For patches and deletes, omitted `bucket` and `key` select that source. For copies, omitted `source_bucket` and `source_key` select the source; the target `key` is mandatory and `bucket` defaults to the source bucket.

Operations are executed sequentially in result order. Additions are applied before removals, so a key listed in both a patch map and its remove list is removed. A copy observes changes made by earlier operations in the same result. Each successful operation creates an audit event with actor `wasm:<plugin-id>`, its stable operation ID, type, bucket, and key.

Before any result side effect, BucketMux validates the complete operation manifest: operation count, unique IDs, allowed types, paths, policy scope, and patch sizes. The entire stdout result remains subject to the 1 MiB limit. Each metadata/tag patch accepts at most 128 entries; keys are capped at 256 bytes and values at 4096 bytes. Unsafe absolute or parent-traversing object keys are rejected.

After validation, BucketMux persists embeddings and the legacy top-level metadata/tags, writes declared derived objects, and finally runs the ordered operations. A failure in any stage leaves the durable job retryable and eventually raises the normal WASM failure alert when attempts are exhausted. Successful operations are visible both in job result JSON and the audit log.

Jobs and external providers make the execution model **at least once**, like most event-driven systems: a worker can fail after a remote effect but before recording job success. The operations are therefore designed to converge safely—patches converge, copies overwrite the same current target, and deleting an already absent object succeeds. A retry against a versioned target can retain an additional historical version, even though the current object converges. Use stable operation IDs, deterministic target keys, and order a move as `object.copy` followed by `object.delete`. BucketMux validates all commands before applying the first one, but a later provider failure can still leave earlier commands applied until the retry completes.

An `object.copy` never creates its destination bucket. It reads through BucketMux's normal primary/replica path and writes through normal placement, quota reservation, failover, checksum, hook, and replication paths. A copy cannot replace a protected current object in a non-versioned bucket. An `object.delete` treats an already absent object as success for safe retries, but otherwise uses the standard protected deletion path.

This is intentionally Lambda-like rather than an AWS Lambda compatibility layer: triggers and durable retries are managed by BucketMux, while storage effects use a portable result manifest instead of an AWS SDK or provider credentials inside the guest.

### Networked inference workers

WASM guests intentionally cannot call OpenAI or another remote inference API. Use an authenticated HTTP hook and an external worker when inference requires network access, provider SDKs, or secrets. `POST /admin/api/embeddings` accepts vectors from that worker together with `source_checksum` and `source_updated_at`; BucketMux returns `409` instead of persisting the result when the object generation is no longer current. Imported vector values are write-only through the admin API and remain redacted from list and search responses.

The runnable [Gemini multimodal image example](../examples/gemini-image-embeddings/README.md) uses this architecture. It embeds PNG/JPEG bytes directly with `gemini-embedding-2`, maintains a restart-safe file queue, and performs cross-modal text-to-image search. It is a general visual-semantic vector, not a face or biometric embedding pipeline.

Embedding vectors are a typed result, not S3 metadata. BucketMux accepts at most 128 vectors per invocation and 4096 finite `float32` dimensions per vector. `model`, `model_version`, dimensions and one of `cosine`, `dot` or `l2` are validated before any result is committed. Vectors are written as compact portable little-endian BLOBs plus a native Turso `vector32` value in single-instance mode (and `BYTEA` plus pgvector in Postgres), atomically replaced per object/plugin generation, and removed through the object's foreign-key cascade. A source overwrite invalidates old embeddings immediately.

The admin API never returns stored vector values in list or search results, and plugin job history redacts them. Use `GET /admin/api/embeddings?bucket=...&key=...` for provenance summaries and `POST /admin/api/embeddings/search` with a query vector. Scores are ordered high-to-low; L2 is represented as negative Euclidean distance. `GET /admin/api/embeddings/capabilities` reports whether the active backend is exact or ANN.

### Scalable vector search

Turso performs bounded exact search inside its native vector engine with `vector_distance_cos`, `vector_distance_dot`, and `vector_distance_l2`. Candidate scans default to 10,000 rows and are capped at 100,000. The portable BLOB remains authoritative for API reads, migration and repair; startup backfills any missing native Turso vector rows, including databases created before this migration. Turso's current native similarity search is linear, so Postgres+pgvector remains the required backend when ANN scale is needed.

Run `make test-turso-vector-scale` for the opt-in 10,000 × 512-dimensional native Turso load/search test.

Postgres can use pgvector 0.8+ as the production ANN backend:

```yaml
store:
  kind: postgres
  vector_search:
    backend: pgvector
    hnsw_m: 16
    ef_construction: 64
    ef_search: 100
    max_scan_tuples: 20000
    max_profiles: 64
```

`backend: pgvector` is strict: startup fails if the extension cannot be enabled. `auto` uses pgvector when available and otherwise retains exact search; `exact` disables ANN explicitly. Environment overrides use the `VECTOR_SEARCH_*` prefix.

BucketMux dual-writes the authoritative portable BLOB and pgvector's native `vector` value in the same transaction. Existing BLOB rows are backfilled under a cross-instance advisory lock. It creates a partial HNSW index for each `kind/model/model_version/metric/dimensions` profile, protected by another advisory lock so two replicas cannot build the same index concurrently. Startup removes stale profile indexes. Profiles up to 2,000 dimensions receive HNSW; larger supported embeddings remain searchable through pgvector's database-side exact operator.

Searches set `hnsw.iterative_scan=strict_order`, `hnsw.ef_search`, and `hnsw.max_scan_tuples` per transaction. Always provide `kind` and `model_version` to select the profile index; omitting either remains correct but cannot use a profile-specific HNSW index.

For facial recognition, one embedding should represent one detected/aligned face. Preserve the detector box and quality score in `metadata`, use an immutable model version, compare only vectors from the same model/version, and apply identity thresholds outside the plugin. Production ArcFace variants commonly emit more dimensions than the shortened three-value contract example. The bundled histogram embedding is deliberately a deterministic integration fixture, not a biometric model.

## Install through the admin API

First compile the examples with `make test-wasm-plugins`. Then base64-encode a module and install it:

```bash
MODULE=$(base64 -w0 examples/wasm/rust/target/wasm32-wasip1/release/image-classifier.wasm)
curl -u admin:change-me http://localhost:8080/admin/api/wasm-plugins \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"image-classifier\",\"name\":\"Image classifier\",\"module_base64\":\"$MODULE\",\"events\":[\"object.created\"],\"bucket_pattern\":\"images\",\"key_prefix\":\"uploads/\",\"content_types\":[\"image/*\"],\"operation_policy\":{\"allowed_operations\":[\"metadata.patch\"],\"max_operations\":4},\"enabled\":true}"
```

The module SHA-256 is computed server-side. Installation compiles the binary and rejects invalid ABI/imports before it can be enabled. In a multi-instance deployment, executable bytes and jobs live in Postgres, so all instances observe the same plugin generation without a shared plugin directory.

## Examples and tests

- Go 1.27 `metadata-tagger`: reads the mounted object and emits verified metadata/tags.
- Go 1.27 `image-dimensions`: parses real PNG/JPEG/GIF headers with the standard library.
- Go 1.27 `embedding-generator`: emits a deterministic, normalized vector to exercise durable Turso storage and native similarity search. It is not a recognition model.
- Go 1.27 `bucket-operator`: checks its granted capabilities, patches source metadata, and optionally requests a scoped cross-bucket copy.
- Rust `uppercase-transform`: writes a real derived object.
- Rust `image-classifier`: emits classification metadata/tags plus a declarative metadata patch.
- Rust `embedding-matcher`: computes cosine similarity over precomputed embeddings.
- Bun 1.4 + AssemblyScript `image-classifier`: emits a declarative tag patch; Bun builds a real `.wasm` guest and executes a conformance test with the WebAssembly API.

Run all language and single/multiple-instance tests:

```bash
make test-wasm-plugins
```

Go guests use the reusable package at `sdk/go/bucketmuxplugin` and compile without CGO:

```bash
GOOS=wasip1 GOARCH=wasm go build -o plugin.wasm ./cmd/plugin
```

TinyGo can use the same SDK and ABI with `tinygo build -target=wasip1`; it is optional and is not required by the BucketMux server.
