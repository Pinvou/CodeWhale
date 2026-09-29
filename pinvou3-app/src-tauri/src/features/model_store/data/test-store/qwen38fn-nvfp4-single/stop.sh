#!/bin/bash
# 停止服务。容器、权重、缓存都保留，再次启用时不用重新下载。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

./megacube.sh stop
