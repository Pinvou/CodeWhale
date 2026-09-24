#!/bin/bash
set -euo pipefail
VENDOR_DIR="$HOME/.local/share/local-model-manager/vendor/qwen38-nvfp4-mtp"
[ -d "$VENDOR_DIR" ] || { echo "ERROR: 未找到部署包" >&2; exit 1; }
cd "$VENDOR_DIR"
./scripts/stop.sh
