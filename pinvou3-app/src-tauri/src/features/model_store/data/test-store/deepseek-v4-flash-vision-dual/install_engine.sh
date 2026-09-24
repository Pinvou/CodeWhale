#!/bin/bash
# 装引擎 = 把运行镜像拉到**本机**并做一次 GPU 自检。
#
# 刻意不碰从机、不配直连网络：那些是"部署"那一步的事。装引擎和下载都只需要一台
# 机器，用户可以先把这两步做完，等第二台设备和直连线到位之后再点部署。
#
# 包里有个 `_node pull` 能干这件事，但 node() 一进门就 `c = config()`，而 config()
# 要求 node.json 已存在——那是 init 配完直连网络才会生成的。所以这里直接用包里的
# 常量和函数拼出等价动作（IMAGE / RUNTIME_IMAGE / gpu_check 都是 import 进来的，
# 不抄字面量，包升级了跟着变）。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

RUNTIME_IMAGE="megacube/deepseek-direct:0.1.1-a83948492cf1"

# 只读探针：LMM 每次下载前先跑一次，命中就整段跳过安装。
#
# 这张卡不打 LMM_NEED_ROOT：交付包的 preflight 明确 `require(os.geteuid() != 0)`，
# 整个脚本以 root 跑会被它当场拒绝。部署时需要提权的几条 nmcli 由包自己用 sudo 调。
#
# 顺带一提：用户如果是从 U 盘 `docker load` 把镜像导进来的，这里同样会命中，
# 整个装引擎步骤自动跳过——离线装机不需要额外做什么。
if [ "${LMM_PROBE_ONLY:-0}" = "1" ]; then
    command -v docker >/dev/null 2>&1 || { echo "ERROR: 未找到 docker，请先安装出厂 NVIDIA 容器运行环境" >&2; exit 1; }
    if docker image inspect "$RUNTIME_IMAGE" >/dev/null 2>&1; then
        echo "probe: 运行镜像已就绪（$RUNTIME_IMAGE）"
        exit 0
    fi
    echo "probe: 运行镜像尚未准备（$RUNTIME_IMAGE）" >&2
    exit 1
fi

. "$CARD_DIR/common.sh"

echo "拉取运行镜像并做 GPU 自检（只用本机，不需要直连线和从机）…"
python3 - <<'PY'
import controller

# 交付包完整性：包被改过一个字节，后面每一步都会拒绝执行，不如现在就说清楚
controller.verify_bundle()
controller.run(['docker', 'pull', controller.IMAGE])
controller.run(['docker', 'tag', controller.IMAGE, controller.RUNTIME_IMAGE])
controller.gpu_check()
print('LMM_ENGINE_IMAGE=' + controller.RUNTIME_IMAGE)
PY

echo "引擎准备完成。接下来可以直接下载权重；两台设备和直连线到位后再点「部署」。"
