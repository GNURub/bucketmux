# BucketMux WASM plugin examples

BucketMux runs untrusted plugins as asynchronous WASI Preview 1 commands. A plugin receives one JSON invocation on standard input, can read only `/input/object`, can write only declared files below `/output`, and must emit one `bucketmux.wasm.v1` JSON result on standard output.

## Go 1.27 guests

Build all examples with the reusable `sdk/go/bucketmuxplugin` package:

```sh
./examples/wasm/go/build.sh
```

- `metadata-tagger` streams the object through SHA-256 and emits metadata/tags.
- `image-dimensions` decodes real PNG/JPEG/GIF headers.
- `embedding-generator` emits a normalized 16-dimensional fixture vector, allowing the complete Turso persistence and native similarity-search path to be tested. It is deliberately not a biometric model.

The same SDK is compatible with a `wasip1` TinyGo command. Native Go `plugin.so` files are intentionally unsupported because they are platform-specific and run with full host privileges.

## Rust guests

Install the official target and build all three examples:

```sh
rustup target add wasm32-wasip1
./examples/wasm/rust/build.sh
```

- `uppercase-transform` creates a derived object and changes source metadata/tags.
- `image-classifier` demonstrates rule/model classification metadata.
- `embedding-matcher` compares **precomputed** embeddings with cosine similarity. It demonstrates the matching seam; it does not detect faces itself.

## Bun 1.4 + AssemblyScript guest

Bun is the pinned package manager and conformance runner; AssemblyScript compiles TypeScript-like source to a real WebAssembly guest:

```sh
cd examples/wasm/bun-assemblyscript
bun install --frozen-lockfile
bun test
```

The production server does not depend on Bun's partially implemented `node:wasi`. Its guest is executed by BucketMux through the same WASI sandbox as Rust modules.

## ABI result

```json
{
  "abi_version": "bucketmux.wasm.v1",
  "metadata": {"classifier": "model-v1"},
  "tags": {"category": "image"},
  "embeddings": [
    {
      "kind": "face",
      "model": "arcface",
      "model_version": "2026-01",
      "metric": "cosine",
      "dimensions": 3,
      "values": [0.012, -0.034, 0.998]
    }
  ],
  "derived_objects": [
    {
      "path": "thumbnail.webp",
      "key_suffix": ".thumbnail.webp",
      "content_type": "image/webp"
    }
  ]
}
```

`path` must be relative to `/output`. A derived object cannot overwrite its source and does not recursively invoke pipelines.

Embedding values are stored separately as compact vectors and are redacted from job history and admin responses. `dimensions` must exactly match the complete array.
