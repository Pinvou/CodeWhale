#!/bin/bash
# 部署 = 生成运行配置，不启动服务。
#
# 厂商的 package/deploy.sh 做的正是规范 4.3 要的那些：校验 LMM_DEPLOY_PROFILE、
# 确认引擎和权重都在、发现已有实例在跑就先调自己的 stop.sh（重复部署安全）、
# 把两个工作流拷进 user/default/workflows/ 并按 profile 选出默认那份、写
# extra_model_paths.yaml / deployment.env / run.sh / MODEL_VERSION.json。
# 原样用。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

echo "部署方案：${LMM_DEPLOY_PROFILE:-quality-current}（quality-current = 243 帧 10 步 + 1080p 超分；balanced-fast = 124 帧 6 步）"
echo "引擎：$LMM_ENGINE_DIR"
echo "权重：$MODEL_DIR"
echo "部署目录：$LMM_DEPLOY_DIR"

# 厂商脚本在写配置之前会再跑一遍 sha256sum -c（40.34GiB，确认权重没被动过），
# 这一段同样是静默的。部署的静默上限是 30 分钟，撞上的概率比下载那步小得多，
# 但用户对着一个五分钟不动的界面同样会以为卡死了，所以照样放后台打心跳。
lmm_vendor_bg deploy.sh "正在校验权重并生成运行配置"

[ -f "$LMM_DEPLOY_DIR/deployment.env" ] || {
    echo "ERROR: 部署跑完了却没生成 deployment.env（$LMM_DEPLOY_DIR），请把上面的输出反馈给我们" >&2
    exit 1
}
echo "部署完成，可以点「启用」把 ComfyUI 跑起来。"
