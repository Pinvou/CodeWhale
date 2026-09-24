#!/bin/bash
# 启用 = 把服务跑起来，并确认它真的能用之后才返回 0。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

[ -f "$VENDOR_DIR/.deploy-env" ] || {
    echo "ERROR: 还没有部署过（缺 .deploy-env），请先完成「部署」" >&2
    exit 1
}

# vendor/start.sh 自己会等到 /health 通过为止，但**等待期间一声不吭**（sleep 15
# 的循环里没有任何输出）。LMM 对「启用」的静默上限是 30 分钟，而它默认能等 3600
# 秒——真等久了就会先被 LMM 当成卡死强杀。所以两件事都要做：
#   1. 放后台跑，前台每 15 秒打一次心跳；
#   2. 把它自己的超时压到 25 分钟，让它的报错（会带上容器日志尾巴）来得及输出，
#      而不是被 LMM 一刀切掉。
# GB10 上实测约 5 分钟就绪（加载权重 + CUDA graph 捕获），25 分钟是很宽的余量。
echo "启动 $CONTAINER_NAME（首次启动约 5 分钟：加载权重 + CUDA graph 捕获）…"
STARTUP_TIMEOUT=1500 PORT="$PORT" "$VENDOR_DIR/start.sh" &
lmm_heartbeat $! "正在等待服务就绪"

# 容器 Up 不等于模型可用，再问一次 /v1/models 才算数。顺便把引擎真正认的模型名
# 拿到手——这个名字必须跟服务返回的 id 一字不差，网关转发时会把请求里的 model
# 换成它，差一个字符所有接入的应用（品悟等）第一句话就收到 404。所以问，不写死。
model_id=""
for _ in $(seq 1 10); do
    model_id="$(curl -fsS --max-time 10 "http://127.0.0.1:$PORT/v1/models" 2>/dev/null |
        python3 -c 'import json,sys;print(json.load(sys.stdin)["data"][0]["id"])' 2>/dev/null || true)"
    [ -n "$model_id" ] && break
    echo "服务已就绪，正在确认模型标识…"
    sleep 5
done
[ -n "$model_id" ] || {
    echo "ERROR: 服务起来了，但 /v1/models 问不到模型标识。" >&2
    echo "       可以看看容器日志：docker logs --tail 100 $CONTAINER_NAME" >&2
    exit 1
}

echo "LMM_ENDPOINT_URL=http://127.0.0.1:$PORT/v1/chat/completions"
echo "LMM_ENDPOINT_MODEL=$model_id"
echo "LMM_CONTAINER_NAME=$CONTAINER_NAME"
echo "服务已就绪：http://127.0.0.1:$PORT/v1（模型标识 $model_id）"
