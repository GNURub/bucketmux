# GATEWAY PACKAGE KNOWLEDGE BASE

## OVERVIEW

`internal/gateway` implements the public S3-compatible HTTP surface plus Uppy browser-upload helpers. It translates HTTP requests into `app.Service` calls and preserves S3-like XML, SigV4, range, CORS, and multipart behavior.

## WHERE TO LOOK

| Task | Location | Notes |
|------|----------|-------|
| S3 route dispatch | `handler.go` | `ServeHTTP`, bucket/object operations, list, range, CORS. |
| SigV4/auth | `auth.go` | `Authenticator`, direct headers, header SigV4, presigned URL validation. |
| Presigning | `presign.go` | URL signing helpers used by admin/Uppy flows. |
| Multipart S3 API | `multipart.go` | Initiate/upload/list/complete/abort multipart endpoints. |
| Uppy API | `uppy.go` | `/uppy/s3/*` JSON helpers. |

## CONVENTIONS

- Public bucket/object access is path-style: `/{bucket}/{key}`.
- Simple auth headers are `X-S3LS-Access-Key` and `X-S3LS-Secret-Key`.
- SigV4 supports header auth and presigned URLs; accepted clock skew for header auth is 15 minutes.
- Presigned URL expiry must be `1..604800` seconds.
- Region defaults to `auto` when config region is empty.
- CORS exposes browser-relevant headers including `ETag` and BucketMux metadata headers.
- Range support is single-range only; multi-range headers are rejected by parser behavior.
- S3 XML response shapes live beside handlers; tests assert compatibility details.

## ANTI-PATTERNS

- Do not add virtual-hosted-style assumptions; README says local deployments should use path-style URLs.
- Do not change auth header names without updating README, tests, and client examples.
- Do not skip canonical request ordering or lowercase signature comparisons in SigV4 paths.
- Do not implement multipart behavior only in Uppy helpers; keep S3 multipart routes working too.
- Do not forget CORS/exposed headers when adding browser-facing upload flows.

## TESTING NOTES

- Run gateway tests with `go test ./internal/gateway -count=1 -v`.
- Bun compatibility is gated: `BUCKETMUX_RUN_BUN_INTEGRATION=1 make test-bun` and requires a local `bun` binary.
- Important test files: `auth_test.go`, `object_lifecycle_test.go`, `multipart_test.go`, `uppy_test.go`, `bucket_test.go`, `range_test.go`.
