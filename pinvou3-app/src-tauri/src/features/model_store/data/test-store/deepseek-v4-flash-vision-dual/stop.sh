#!/bin/bash
# 停止两端的推理容器。权重、镜像、直连网络配置和 SSH 互信都保留。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

./megacube.sh stop
