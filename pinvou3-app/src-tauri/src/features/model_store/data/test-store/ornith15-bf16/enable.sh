#!/bin/bash
set -euo pipefail
VENDOR_DIR="$HOME/.local/share/local-model-manager/vendor/ornith15-bf16"
[ -d "$VENDOR_DIR" ] || { echo "ERROR: 未找到部署包，请先完成部署" >&2; exit 1; }
cd "$VENDOR_DIR"
./scripts/start.sh
# 标准 OpenAI 风格接口，跟网关现有的转发逻辑直接兼容。
PORT="$(grep '^PORT=' .env | cut -d= -f2)"
SERVED_MODEL_NAME="$(grep '^SERVED_MODEL_NAME=' .env | cut -d= -f2 || true)"
echo "LMM_ENDPOINT_URL=http://127.0.0.1:${PORT}/v1/chat/completions"
echo "LMM_ENDPOINT_MODEL=${SERVED_MODEL_NAME:-Ornith-1.5-35B-A3B}"
