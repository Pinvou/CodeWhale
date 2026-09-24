#!/bin/bash
# 启用 = 创建并启动推理容器，等到 /health 通过 + 一次真实生成冒烟通过才算成功。
#
# 首次加载通常要 12~14 分钟（PLE 按需读 NVMe），包内部的 wait_ready 上限是
# config.json 里的 startup_timeout（默认 3600 秒），期间每 15 秒打印一行
# "加载/预热中: …"。这行输出就是心跳，LMM 的空闲超时（1800 秒）不会误杀。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

lmm_sync_config
./megacube.sh start

BIND="$(lmm_config_get bind)"
PORT="$(lmm_config_get port)"
# 端点给 LMM 的 API 网关和"供 Agent 应用接入"面板用，必须是本机可直连的地址：
# bind 成 0.0.0.0 时对外写 127.0.0.1 才有意义。
[ "$BIND" = "0.0.0.0" ] && BIND="127.0.0.1"
# 报上容器名。LMM 用它判断"这个容器是不是本应用起的"——镜像不是身份，
# 别人用同一个镜像起的容器跟我们的长得一模一样。名字取自包里的 NAME 常量，
# 不写死字面量，包改了名字这里跟着变。
echo "LMM_CONTAINER_NAME=$(python3 -c 'import controller;print(controller.NAME)')"
echo "LMM_ENDPOINT_URL=http://$BIND:$PORT/v1/chat/completions"
echo "LMM_ENDPOINT_MODEL=qwen3.8-flash-next"
