#!/bin/bash
set -euo pipefail
VENDOR_DIR="$HOME/.local/share/local-model-manager/vendor/qwen38-nvfp4-mtp"
[ -d "$VENDOR_DIR" ] || { echo "ERROR: 未找到部署包，请先完成部署" >&2; exit 1; }
cd "$VENDOR_DIR"
./scripts/start.sh
# 启动成功后告诉 LMM 这个模型对外怎么调用（给 API 网关转发用）：网关跟引擎在
# 同一台机器上，不管 HOST_BIND 配的是 127.0.0.1 还是 0.0.0.0，网关这边直接走
# 127.0.0.1 都能连到。基准方案没有显式设置 --served-model-name，SGLang 默认
# 认 /models/target 这个模型名；生产采样方案会在 .env 里写 SERVED_MODEL_NAME，
# 两种情况都要覆盖到。
PORT="$(grep '^PORT=' .env | cut -d= -f2)"
SERVED_MODEL_NAME="$(grep '^SERVED_MODEL_NAME=' .env | cut -d= -f2 || true)"
echo "LMM_ENDPOINT_URL=http://127.0.0.1:${PORT}/v1/chat/completions"
echo "LMM_ENDPOINT_MODEL=${SERVED_MODEL_NAME:-/models/target}"
