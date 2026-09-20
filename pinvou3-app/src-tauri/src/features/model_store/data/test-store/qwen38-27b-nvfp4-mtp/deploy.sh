#!/bin/bash
set -euo pipefail
VENDOR_DIR="$HOME/.local/share/local-model-manager/vendor/qwen38-nvfp4-mtp"
[ -d "$VENDOR_DIR" ] || { echo "ERROR: 未找到部署包，请先完成下载" >&2; exit 1; }
cd "$VENDOR_DIR"

PROFILE="${LMM_DEPLOY_PROFILE:-benchmark}"
PROFILE_DIR="$VENDOR_DIR/profiles/$PROFILE"
[ -d "$PROFILE_DIR" ] || { echo "ERROR: 未知的部署方案: $PROFILE" >&2; exit 1; }

PREV_PROFILE=""
[ -f "$VENDOR_DIR/.active_profile" ] && PREV_PROFILE="$(cat "$VENDOR_DIR/.active_profile")"

cp "$PROFILE_DIR/compose.yaml" compose.yaml
cp "$PROFILE_DIR/smoke-test.sh" scripts/smoke-test.sh
chmod +x scripts/smoke-test.sh
cp "$PROFILE_DIR/.env.example" .env.example

# 换方案时旧 .env 里残留的上一套方案取值（比如 KV_CACHE_DTYPE/HOST_BIND）不能
# 留着，必须按新方案的 .env.example 整份重建；同一方案重新部署/修复则保留用户
# 可能手改过的 .env，不覆盖。
if [ "$PROFILE" != "$PREV_PROFILE" ] || [ ! -f .env ]; then
    cp .env.example .env
    [ -n "$PREV_PROFILE" ] && echo "切换部署方案：$PREV_PROFILE -> $PROFILE，已重新生成 .env"
fi
echo "$PROFILE" > "$VENDOR_DIR/.active_profile"

# 上下文长度是基准方案里唯一特意改离研究团队 Benchmark 默认值（8192）的参数：
# 实测调用方经常一次请求就超过 8192（输入+期望输出），默认值下直接被 SGLang
# 拒绝。生产采样方案自带的 262144 已经是研究团队为生产场景调好的，不应该被
# 这个覆盖顶掉。
if [ "$PROFILE" = "benchmark" ]; then
    sed -i "s/^CONTEXT_LENGTH=.*/CONTEXT_LENGTH=65536/" .env
fi

# 权重/缓存路径每次部署都强制对齐一次：老版本下载时写进 .env 的是 /opt/...
# 或 LMM 隐藏目录，只靠下载步骤里改的话，这种存量 .env 会在部署时直接因 /opt
# 权限不够而失败；两套方案共用同一份模型权重，不因为切方案就重新下载。
MODELS_DIR="${LMM_MODELS_DIR:-$HOME/models}/Qwen3.8-27B-NVFP4"
mkdir -p "$MODELS_DIR"
sed -i \
    -e "s#^MODEL_PATH=.*#MODEL_PATH=$MODELS_DIR#" \
    -e "s#^CACHE_PATH=.*#CACHE_PATH=$VENDOR_DIR/cache#" \
    .env

./scripts/deploy.sh
