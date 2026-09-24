#!/bin/bash
# 启用：把两端的推理容器拉起来，等到 /health 通过并做一次真实生成冒烟。
#
# `start` 是幂等的（底层是 compose up -d），服务已经在跑时它就是一次快速的
# 健康确认——部署那一步结束时服务其实已经起来了，这里正好接得上。
#
# 首次加载 168GB 权重要十几分钟，包内部每 15 秒打印一行"模型加载/预热中"，
# 这就是心跳，LMM 的空闲超时（1800 秒）不会误杀。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

lmm_state_exists || {
    echo "ERROR: 尚未组建双机集群，请先点「部署」——那一步会配置直连网络、与从机配对并分发模型" >&2
    exit 1
}

./megacube.sh start --timeout 3600

BIND="$(lmm_node_get bind)"
PORT="$(lmm_node_get port)"
# 端点要给 LMM 的 API 网关和"供 Agent 应用接入"面板用，必须是本机可直连的地址：
# bind 成 0.0.0.0 时对外写 127.0.0.1 才有意义。
[ "$BIND" = "0.0.0.0" ] && BIND="127.0.0.1"
# 报上本机（主机侧）容器名，LMM 用它判断这个容器是不是本应用起的。
# 名字问 compose 自己要，不猜——服务名和项目名拼出来的规则以后可能变。
CONTAINER="$(python3 -c "
import controller
print(controller.compose(['ps', '--format', '{{.Name}}'], capture=True).strip().splitlines()[0])
" 2>/dev/null || true)"
# 用 if 而不是 `[ -n ... ] && echo`：后者在 CONTAINER 为空时整条命令返回非 0，
# set -e 会就地把脚本掐死——服务其实已经起来了，却报成启用失败。
if [ -n "$CONTAINER" ]; then
    echo "LMM_CONTAINER_NAME=$CONTAINER"
fi
# 模型名问正在跑的服务要，不写死。LMM 的 API 网关转发时会把请求里的 model 换成
# 这个值，差一个字符对方就回 404 "model ... does not exist"。之前这里写的是 HF 仓库
# 名 deepseek-ai/DeepSeek-V4-Flash-Vision-Exp，而服务认的是 --served-model-name 给的
# deepseek-v4-flash-vision-exp——品悟接进来第一句话就 404。
MODEL_ID="$(python3 -c "
import json, controller
print(json.loads(controller.api(controller.config(), '/v1/models'))['data'][0]['id'])
" 2>/dev/null || true)"
echo "LMM_ENDPOINT_URL=http://$BIND:$PORT/v1/chat/completions"
echo "LMM_ENDPOINT_MODEL=${MODEL_ID:-deepseek-v4-flash-vision-exp}"
