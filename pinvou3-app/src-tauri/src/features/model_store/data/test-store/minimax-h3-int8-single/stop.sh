#!/bin/bash
# 停用 = 停掉 ComfyUI 进程，保留部署配置、权重、引擎和缓存。
#
# 厂商的 package/stop.sh 直接用，一个字都不用包：
#   - 幂等（没有 pid 文件、pid 文件是脏的、进程已经不在，三种情况都打一行然后退 0）
#   - kill 之前核对 /proc/<pid>/cmdline 里带不带本卡片的 --user-directory，不是自己
#     起的进程就拒绝终止并非 0 退出。这一条很关键：pid 会被系统回收复用，照着一个
#     过期的 pid 文件下手，杀掉的可能是别人的进程
#   - 30 秒内没退出就非 0 退出并指向 server.log，不假装成功
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

exec bash "$PKG_DIR/stop.sh"
