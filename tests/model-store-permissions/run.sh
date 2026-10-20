#!/bin/bash
set -euo pipefail
BASE="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
export CARGO_TARGET_DIR="$BASE/../target"
cargo build --locked --offline --manifest-path "$BASE/Cargo.toml"
for mask in 0002 0022 0077; do
    (
        umask "$mask"
        "$CARGO_TARGET_DIR/debug/model-store-permissions-check" "$mask"
    )
done
