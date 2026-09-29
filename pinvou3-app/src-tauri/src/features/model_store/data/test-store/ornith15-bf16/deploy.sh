#!/bin/bash
set -euo pipefail
VENDOR_DIR="$HOME/.local/share/local-model-manager/vendor/ornith15-bf16"
[ -d "$VENDOR_DIR" ] || { echo "ERROR: 未找到部署包，请先完成下载" >&2; exit 1; }
cd "$VENDOR_DIR"

[ -f .env ] || cp .env.example .env

# 权重/缓存路径在部署这一步也强制对齐一次：老版本下载时写进 .env 的是 LMM
# 隐藏数据目录，只靠下载步骤里改的话，这种存量 .env 在部署时会跟下载到的
# 实际位置对不上。
MODELS_DIR="${LMM_MODELS_DIR:-$HOME/models}/Ornith-1.5-35B-A3B"
mkdir -p "$MODELS_DIR"
sed -i \
    -e "s#^MODEL_PATH=.*#MODEL_PATH=$MODELS_DIR#" \
    -e "s#^CACHE_PATH=.*#CACHE_PATH=$VENDOR_DIR/cache#" \
    .env

./scripts/deploy.sh
