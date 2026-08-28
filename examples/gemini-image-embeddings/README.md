# Gemini multimodal image embeddings

This runnable Bun 1.4 example implements the complete asynchronous flow:

1. a client uploads a PNG or JPEG to BucketMux;
2. BucketMux commits the object and enqueues an `object.created` HTTP hook;
3. this worker durably queues the hook and immediately returns `202`;
4. the worker downloads the object from BucketMux;
5. Gemini `gemini-embedding-2` converts the image bytes directly into a visual-semantic vector;
6. the worker imports the vector through BucketMux's authenticated embedding endpoint;
7. BucketMux rejects the result if the object was overwritten while inference was running.

The WASM sandbox remains offline and never receives Gemini or BucketMux credentials. The external worker owns network access and secrets. BucketMux owns durable object events, generation checks, vector persistence, Turso/pgvector indexing, and search.

## Why Gemini Embedding 2

`gemini-embedding-2` maps images and text into one vector space, so a text query can find related images without first generating a caption. This example requests 768 dimensions: Google recommends 768, 1536, or 3072, and automatically normalizes the reduced Gemini Embedding 2 vectors for cosine similarity.

The model currently accepts PNG and JPEG image inputs. This example rejects other `image/*` formats instead of silently sending unsupported data. See Google's [multimodal embeddings guide](https://ai.google.dev/gemini-api/docs/embeddings#multimodal-embeddings) and [`gemini-embedding-2` model card](https://ai.google.dev/gemini-api/docs/models/gemini-embedding-2).

This is a general visual-semantic embedding. It is not a face embedding, biometric identifier, or guaranteed near-duplicate image hash.

## Configure the worker

```bash
cd examples/gemini-image-embeddings

export BUCKETMUX_S3_URL=http://127.0.0.1:8080
export BUCKETMUX_ADMIN_URL=http://127.0.0.1:8080
export BUCKETMUX_S3_ACCESS_KEY=local-access-key
export BUCKETMUX_S3_SECRET_KEY=local-secret-key
export BUCKETMUX_ADMIN_USER=admin
export BUCKETMUX_ADMIN_PASSWORD=change-me
export BUCKETMUX_WEBHOOK_SECRET='replace-with-a-random-webhook-secret'
export GEMINI_API_KEY='...'

# Optional. Pin these explicitly in production.
export GEMINI_EMBEDDING_MODEL=gemini-embedding-2
export GEMINI_OUTPUT_DIMENSIONS=768
export QUEUE_DIR=./data/queue

bun run start
```

The worker listens on port `8091` by default. Set `PORT` to change it. `GEMINI_BASE_URL` exists for controlled gateways and local tests; it defaults to `https://generativelanguage.googleapis.com/v1beta`.

## Register the BucketMux hook

The hook endpoint must be reachable from BucketMux. Use HTTPS and a random secret outside local development:

```bash
curl -u admin:change-me http://127.0.0.1:8080/admin/api/hooks \
  -H 'Content-Type: application/json' \
  -d '{
    "id": "gemini-image-embeddings",
    "name": "Gemini multimodal image embeddings",
    "kind": "http",
    "url": "http://127.0.0.1:8091/hooks/object-created",
    "method": "POST",
    "events": ["object.created"],
    "headers": {"X-Webhook-Secret": "replace-with-a-random-webhook-secret"},
    "enabled": true
  }'
```

Use the ordinary BucketMux hook payload for this worker, not an S3-compatible bucket-notification payload. The ordinary payload includes `checksumSHA256` and `objectUpdatedAt`, which bind the imported embedding to one object generation.

Upload an image normally. The response does not wait for Gemini:

```bash
curl -X PUT http://127.0.0.1:8080/images/photo.jpg \
  -H 'X-S3LS-Access-Key: local-access-key' \
  -H 'X-S3LS-Secret-Key: local-secret-key' \
  -H 'Content-Type: image/jpeg' \
  --data-binary @photo.jpg
```

Inspect the stored provenance. Vector values are deliberately redacted:

```bash
curl -u admin:change-me \
  'http://127.0.0.1:8080/admin/api/embeddings?bucket=images&key=photo.jpg'
```

## Cross-modal search

The query helper embeds text with the same model and dimensionality as the image:

```bash
bun run query -- "a red bicycle in a city street"
```

It then submits the vector to BucketMux's existing vector-search API. Turso performs native exact vector search for a single instance; Postgres with pgvector is the scalable ANN path.

Never compare vectors from different models or dimensionality profiles. If either setting changes, re-embed the corpus and query with the new `model_version` profile.

## Failure and privacy behavior

- Hook reception is fast: the worker atomically writes a deduplicated file-backed job before returning `202`.
- Jobs retry with exponential backoff and survive restarts. After eight failures they become `*.failed.json` for inspection.
- A job identity contains bucket, key, checksum, and object timestamp, so duplicate hook deliveries do not duplicate inference.
- BucketMux returns `409 embedding-source-superseded` if a later upload replaced the source generation. The worker discards that obsolete job without retrying; the later upload has its own job.
- Gemini and BucketMux credentials stay in environment variables and are never written to queue files or embedding metadata.
- The image bytes leave your deployment and are sent to Google. Review current Gemini data controls, consent, residency, retention, and your applicable privacy obligations.
- Do not use this pipeline as face recognition. Biometric matching needs a purpose-built consent-aware detector/model and a stricter security and retention design.

## Tests

Tests mock every HTTP interaction and never call Gemini:

```bash
bun test
```

They verify object download, Gemini inline-image request shape, direct visual vector import, cross-modal text query shape, stale-generation handling, and durable queue deduplication.
