# Performance and tuning

BucketMux favors predictable correctness over bypassing its placement and durability layers. The optimizations in this document remove redundant local work while preserving quota reservations, write failover, checksums, replication, hooks, object protection, and the persistent object index.

## Single-instance baseline

The public single-instance backend remains:

```yaml
store:
  kind: sqlite
  sqlite:
    path: /data/switcher.db
    max_open_conns: 4
    max_idle_conns: 4
```

`store.kind: sqlite` uses the embedded Turso Database engine. Existing SQLite-compatible files continue to open in place; `turso` is deliberately not exposed as a configuration kind.

BucketMux initializes Turso in WAL mode and applies `synchronous=NORMAL`, foreign-key enforcement, and the busy timeout to every pooled connection. This retains crash-safe WAL semantics while avoiding full-sync serialization on every metadata commit.

The default connection pool is four open and four idle connections. Turso can serve concurrent reads, but conflicting writes are serialized by the database. A larger pool can therefore add scheduling and lock contention without improving a mixed S3 workload. Four is a conservative default, not a hard limit: measure with the production workload before changing it.

Use Postgres rather than opening one embedded database file from multiple BucketMux processes. Postgres is the supported backend for multi-instance deployments and higher metadata-write concurrency.

## Upload data path

Every upload is first streamed into BucketMux's durable spool while its size and SHA-256 checksum are calculated. This spool is still required because placement can fail over between providers and every retry must read identical bytes.

The local-disk adapter implements the optional `PreparedPutAdapter` capability. When the spool and destination are on the same filesystem, it commits the prepared file with this sequence:

1. synchronize the spool file;
2. atomically rename it into the provider's object namespace;
3. reuse the already calculated size and SHA-256 as stored-object metadata.

This removes the former second userspace copy and second checksum pass for local uploads. It does not weaken durability: the file is synchronized before the rename, quota errors remain classified for placement failover, and all normal post-write indexing and replication steps still run.

If the rename returns `EXDEV` because the spool and destination are on different filesystems, BucketMux rewinds the spool and falls back to the normal streaming adapter contract. Remote and third-party adapters are unchanged unless they explicitly implement `PreparedPutAdapter`.

For the prepared path, place `server.data_dir` and the local provider's `settings.path` on the same filesystem. Separate mounts remain supported but use the fallback copy path.

## Reduced metadata writes

The object index avoids writes that cannot change state:

- a brand-new object does not attempt to delete embeddings from a previous generation;
- a brand-new object with no metadata, tags, version, retention, or legal hold does not create an empty attributes row;
- replica cleanup returns immediately when an object has no replicas.

Overwrites retain the full correctness path: stale embeddings are invalidated, explicit empty metadata/tags clear previous values, versions and protection are persisted, and replaced provider objects and replicas are cleaned up.

## Tuning guidance

| Workload | Recommendation |
|---|---|
| One process, local or modest remote object traffic | Keep `store.kind: sqlite` and the default `4/4` Turso pool. |
| Read-heavy single process | Increase the pool gradually only after measuring; watch tail latency and database lock time. |
| Write-heavy metadata traffic | Prefer Postgres if increasing the Turso pool does not improve throughput. |
| More than one BucketMux process | Use Postgres; do not share an embedded Turso file between processes. |
| Large local uploads | Keep spool and local-provider storage on the same filesystem to enable atomic rename. |
| Remote-provider uploads | Size `server.data_dir` for concurrent spools; failover may rewind and resend the file. |
| Vector search at ANN scale | Use Postgres with pgvector. Turso's native vector search is exact and bounded, not ANN. |

Connection overrides are available through `SQLITE_MAX_OPEN_CONNS` and `SQLITE_MAX_IDLE_CONNS`. The idle count must not exceed the open count. Changing either value should be treated as a benchmarked deployment setting rather than a universal optimization.

## Measuring regressions

Run the pinned local HTTP workload:

```bash
make test-k6
```

It uses Turso, a local-disk provider, 4 KiB objects, and a steady-state mix of 50% GET, 30% HEAD, 10% LIST, and 10% complete PUT/GET/DELETE lifecycles. Override concurrency and duration when exploring a deployment:

```bash
K6_VUS=50 K6_DURATION=2m make test-k6
```

The checked-in thresholds are regression guards for a local gateway, not production capacity claims. Test again with the intended disks, object sizes, network providers, replication policy, plugins, and encryption settings.

GitHub Actions places only the k6 working directory on `/dev/shm`. This removes hosted-runner disk and `fsync` variance from the strict gateway regression gate; the local-provider test suite still exercises synchronized disk commits. Running `make test-k6` normally continues to use `${TMPDIR:-/tmp}` unless `K6_WORK_ROOT` is explicitly set.

The correctness paths behind these optimizations are covered by:

```bash
make test
make test-race
```

Specific tests verify prepared local commits, cross-feature object attributes, overwrite clearing, pool defaults, and the existing single/multiple-instance suites.

## Operational caveats

- An atomic rename is atomic only within one filesystem. The cross-device fallback is expected behavior, not an error.
- The local adapter synchronizes file contents before rename. Operators remain responsible for the durability guarantees of the underlying filesystem, volume, and host.
- Fewer empty metadata writes reduce database work but do not change the hydrated API result: missing attributes still read as empty/default attributes.
- Pool sizing cannot turn the embedded backend into a multi-instance database. Use Postgres for that boundary.
- WASM-derived objects and declarative copies use the same optimized upload path when placement selects a compatible local adapter.
