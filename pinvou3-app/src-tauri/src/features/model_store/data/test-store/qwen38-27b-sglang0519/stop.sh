#!/bin/bash
# 停用 = 停掉服务，保留容器配置、权重、镜像和缓存。已经停了再停一次也返回 0。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

# vendor/stop.sh 本身就是幂等的（没有这个容器时打一行「nothing to stop」后退 0），
# 而且它会多等一会儿让驱动把统一内存回收掉——GB10 上刚停完马上再启会因为内存
# 还没还回来而失败，这段等待是必要的。
"$VENDOR_DIR/stop.sh"
