#!/bin/bash
# 启用 = 把 ComfyUI 跑起来，等 /queue 健康检查通过之后才返回 0。
#
# 厂商的 package/enable.sh 该做的都做了：已经在跑且健康就直接成功（幂等）、
# 端口被别的健康 ComfyUI 占着会明确报错而不是假装成功、nohup 起进程并记 server.pid、
# 等待期间每 2 秒打一行（心跳没问题）、最长 180 秒、失败时把 server.log 末尾 60 行
# 打出来。原样用。
#
# ⚠️ 这个端点不是 OpenAI 对话接口。
#    MiniMax-H3 是 ComfyUI 的视频生成工作流，不是对话模型：提交任务是
#    POST /prompt + 一整段工作流 JSON，没有 /v1/chat/completions，也没有 /v1/models
#    （所以规范 4.4 里"问服务要 LMM_ENDPOINT_MODEL"那套在这儿不适用，只能报一个
#    固定标识）。跟 z-image-turbo 一样，它接不进品悟的对话模型列表——品悟侧按
#    endpoint_url 是不是 OpenAI 对话接口做拦截，这张卡会被正确挡在外面。这两行
#    标记目前只用于在详情页展示调用信息。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

# 不放后台：厂商脚本每 2 秒就打一行等待进度，本来就不会触发静默超时；而且它给
# nohup 起的子进程装了 trap，前台跑着信号传递最直接。
bash "$PKG_DIR/enable.sh"

[ -f "$LMM_DEPLOY_DIR/deployment.env" ] || {
    echo "ERROR: 启用返回成功，却找不到 $LMM_DEPLOY_DIR/deployment.env，无法确定服务端口" >&2
    exit 1
}
# shellcheck disable=SC1091
. "$LMM_DEPLOY_DIR/deployment.env"

# 厂商脚本自己也打了这两行，值完全一样（端口就是从它写的 deployment.env 读的），
# LMM 取最后一条，重复无副作用。这里再打一遍是因为「对外调用信息」必须由入口
# 脚本这一层负责——它才是 LMM 和自检工具唯一会看的那一层，厂商脚本换一版就可能
# 不打了，那时界面上的调用信息会悄无声息地变空。
echo "LMM_ENDPOINT_URL=http://127.0.0.1:$PORT/"
echo "LMM_ENDPOINT_MODEL=MiniMax-H3-INT8-Turbo"
echo "ComfyUI 已就绪：在浏览器打开 http://<设备地址>:$PORT/ ，工作流 $PROFILE 已经预置在界面里。"
