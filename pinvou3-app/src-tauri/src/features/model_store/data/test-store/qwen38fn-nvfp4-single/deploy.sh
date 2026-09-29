#!/bin/bash
# 部署 = 把配置定下来并把能提前发现的问题都发现掉，但不启动服务。
#
# 对应包的 `megacube.sh check`：preflight（aarch64 / 128GB 内存 / docker）
# + gpu_probe（在锁定镜像里确认只有 1 张 GB10、算力 12.1、九个运行时补丁的
# 挂载目标都存在）。这些正是"启动前一定会卡住用户"的那几项，放在部署阶段
# 暴露，比等到 12~14 分钟的首次加载失败之后再报要好。
#
# 不做 verify：那会把 133GB 权重整个重新算一遍 sha256，要跑几个小时；
# download 结束时已经做过一次全量校验，start 时包还会做一次基于大小+mtime
# 的 quick_verify。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

lmm_sync_config
./megacube.sh check

echo "部署检查通过。模型目录：$MODEL_DIR"
echo "启用后 API 将监听 $(lmm_config_get bind):$(lmm_config_get port)"
