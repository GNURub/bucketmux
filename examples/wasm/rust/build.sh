#!/bin/sh
set -eu

cargo build --manifest-path "$(dirname "$0")/Cargo.toml" --release --target wasm32-wasip1
