#!/bin/bash
# 部署 = 生成运行配置，不启动服务。
#
# 这一步才真正把 vendor/deploy.sh 跑一遍。前面两步已经把镜像和权重都备齐了，
# 所以它的前五步（查硬件／装 Docker／装容器工具／拉镜像）全部命中「已就绪」直接
# 跳过，第六步也因为权重完整而跳过下载，实际只剩下属于「部署」的两件事：
# 探出这台机器能用的 GPU 直通方式，并写出 start/stop/uninstall 都要读的 .deploy-env。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

lmm_init_docker

lmm_docker image inspect "$IMAGE" >/dev/null 2>&1 || {
    echo "ERROR: 引擎镜像不在本机（$IMAGE），请先完成「安装引擎」" >&2
    exit 1
}
lmm_model_complete "$MODEL_DIR" || {
    echo "ERROR: 权重不完整（$MODEL_DIR），请先完成「下载」" >&2
    exit 1
}

# 重复部署必须安全：已经在跑的实例先停掉，否则待会儿 start 起来的还是旧配置的容器。
if lmm_docker ps --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
    echo "检测到 $CONTAINER_NAME 正在运行，先停掉再重新部署…"
    "$VENDOR_DIR/stop.sh"
fi

# SKIP_DOCKER_INSTALL=1：装系统包是要 root 的事，这张卡的定位是「设备出厂就带好了
# 容器环境」。真缺的话在「安装引擎」那步就已经报过更清楚的错了，不该等到这里才
# 悄悄弹一个 apt 出来。
echo "生成运行配置（端口 $PORT，权重 $MODEL_DIR）…"
SKIP_DOCKER_INSTALL=1 MODEL_DIR="$MODEL_DIR" PORT="$PORT" "$VENDOR_DIR/deploy.sh"

[ -f "$VENDOR_DIR/.deploy-env" ] || {
    echo "ERROR: 部署跑完了却没有生成 .deploy-env，请把上面的输出反馈给我们" >&2
    exit 1
}
echo "部署完成，可以点「启用」把服务跑起来。"
