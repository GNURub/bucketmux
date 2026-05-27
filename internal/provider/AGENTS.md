# PROVIDER PACKAGE KNOWLEDGE BASE

## OVERVIEW

`internal/provider` contains the storage adapter boundary and concrete backends. Adapters hide local disk, S3-compatible APIs, Cloudinary, and Vercel Blob behind one object contract used by `app.Service`.

## WHERE TO LOOK

| Task | Location | Notes |
|------|----------|-------|
| Adapter contract | `provider.go` | `Adapter`, `Registry`, `Entry`, `Get`. |
| Local disk | `local.go` | Filesystem storage under configured provider path. |
| S3-compatible | `s3compat.go` | AWS/R2/MinIO/SeaweedFS-style backend. |
| Cloudinary | `cloudinary.go` | Non-S3 media API adapter. |
| Vercel Blob | `vercel_blob.go` | Vercel Blob REST API adapter and path/header rules. |
| Health checks | `health.go` | Provider health helpers. |

## CONVENTIONS

- Every adapter implements `Put`, `Get`, `Head`, `Delete`, and `Health`.
- Register new adapters in `internal/app/service.go` via `provider.Entry(domain.ProviderKindX, provider.NewXAdapter())`.
- Provider kinds are defined in `internal/domain/types.go`.
- Provider settings are string maps; common routing keys are consumed by `internal/router`, backend-specific keys stay in adapters.
- Local provider rejects path traversal; object keys must never escape the configured root.
- Vercel Blob pathnames reject unsafe paths and use API-specific headers such as `X-Api-Version`, `X-Vercel-Blob-Access`, and `X-Allow-Overwrite`.
- `StoredObject` must return the remote bucket/key, size, content type, ETag, and checksum when available.

## ANTI-PATTERNS

- Do not add provider-specific behavior to `app.Service` if it belongs inside an adapter.
- Do not assume all providers are S3-native; Cloudinary and Vercel Blob are API adapters behind the same gateway.
- Do not persist plaintext secrets in provider settings; secrets come through `ProviderAccount.SecretKey` after decryption.
- Do not trust object keys for filesystem or URL paths without adapter-level normalization/validation.
- Do not add a provider kind without config examples, domain constant, registry registration, and tests.

## TESTING NOTES

- Run provider tests with `go test ./internal/provider -count=1 -v`.
- SeaweedFS integration is gated: `BUCKETMUX_RUN_SEAWEEDFS_INTEGRATION=1 make test-seaweedfs`.
- Important tests: `local_test.go`, `vercel_blob_test.go`, `seaweedfs_integration_test.go`.
