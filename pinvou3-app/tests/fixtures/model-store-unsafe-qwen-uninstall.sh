#!/bin/bash
# 卸载模型：停掉服务、真删权重（约 22GB）、清掉运行状态；保留引擎。
#
# 权重是真删：LMM 里「卸载模型」对用户的意思就是把这份磁盘收回去，其它卡都是
# 这个行为，不对齐的话用户卸载完会发现磁盘一点没少。
#
# 引擎保留：它是内容寻址的公共目录（~/.local/share/.../engines/tachyon-serve），
# 其他驭光卡片可能复用同一个包，删了下次又要重下 215MB。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

"$CARD_DIR/stop.sh" >/dev/null 2>&1 ||
    echo "注意：停服务那步没完全成功，继续清理权重；端口若仍被占用请按上面的提示检查。" >&2

# 权重目录从 .deploy-env 里再读一遍：用户可能在设置里改过模型存放目录，
# 以实际部署过的为准
target="$MODEL_DIR"
if [ -f "$DEPLOY_ENV" ]; then
    deployed="$(sed -n 's/^MODEL_DIR="\(.*\)"$/\1/p' "$DEPLOY_ENV" | tail -1)"
    [ -n "$deployed" ] && target="$deployed"
fi

if [ -d "$target" ]; then
    echo "正在删除权重：$target"
    rm -rf -- "$target"
fi

# 卡片自己在设备上产生的运行期状态：日志、pid、部署配置，都可重建
rm -rf -- "$RUNTIME_DIR" "$DEPLOY_ENV"

echo "卸载完成，权重已删除。"
echo "引擎保留在 $ENGINE_DIR（约 215MB），确定不再使用任何驭光卡片时，"
echo "可手动删除整个 $ENGINE_ROOT 收回空间。"
