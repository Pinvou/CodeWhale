#!/bin/bash
# 卸载模型：停掉并移除容器、清掉部署配置和运行期缓存。**权重删不删由用户在界面
# 上选**，LMM 用 LMM_PURGE_WEIGHTS 把这个选择传进来（规范 4.6）：
#
#   LMM_PURGE_WEIGHTS=1  彻底删除：约 23.4GB 权重一起删，不可撤销
#   LMM_PURGE_WEIGHTS=0  仅卸载：权重留在盘上，重装时不用再下 23.4GB
#
# 变量没有时按 0（保留）算，跟规范 4.6 的默认值一致。
#
# **为什么不直接调 vendor/uninstall.sh**：它非交互跑（--yes）时会无条件把
# lmsysorg/sglang:v0.5.19 这个镜像也 rmi 掉，而它又没有「保留镜像」的开关。
# 在 LMM 里镜像属于「已安装引擎」，由那边统一管理和删除——同一个镜像可能还有
# 别的卡片/方案在用，卸载一个模型不该顺手把它清了。它剩下的动作（停容器、删
# 目录）就是几行 rm，与其去跟它的交互式确认较劲，不如在这里明确写出来。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

purge_weights=0
[ "${LMM_PURGE_WEIGHTS:-0}" = "1" ] && purge_weights=1

if lmm_init_docker; then
    if lmm_docker ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER_NAME"; then
        echo "停止并移除容器 $CONTAINER_NAME …"
        lmm_docker stop -t 60 "$CONTAINER_NAME" >/dev/null 2>&1 || true
        lmm_docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
    fi
else
    # 连不上 docker 也要把该做的做完，不能因为这个就整个卸载失败
    echo "连不上 Docker，跳过容器清理，继续卸载" >&2
fi

# 权重目录从 .deploy-env 里读一遍：用户可能在设置里改过模型目录，那时候
# common.sh 算出来的和当初部署时用的会不一样，以实际部署过的为准。
# 保留权重时同样要读——这句要报给用户的路径必须是真正占着盘的那个。
target="$MODEL_DIR"
if [ -f "$VENDOR_DIR/.deploy-env" ]; then
    deployed="$(sed -n 's/^MODEL_DIR="\(.*\)"$/\1/p' "$VENDOR_DIR/.deploy-env" | tail -1)"
    [ -n "$deployed" ] && target="$deployed"
fi

if [ "$purge_weights" = "1" ]; then
    if [ -d "$target" ]; then
        echo "正在删除权重：$target"
        rm -rf -- "$target"
    fi
fi

# 卡片自己在设备上产生的运行期状态：SGLang 的 JIT 缓存、下载工具的 venv、
# 部署配置。都是可重建的，**两种选择下都清掉**——用户选的是"保留权重"，不是
# "保留这套部署"，而且这些目录留着反而会让下次重新部署读到过期配置。
#
# 走 lmm_rm_rf_force 而不是直接 rm：.cache 里是容器以 root 写进来的 JIT 产物，
# 普通用户删不掉（原因见 common.sh 里那个函数的注释）。
#
# **删不掉也不能让整个卸载失败。** 这里清的全是可重建的缓存，实测 21MB；而此时
# 容器和权重都已经删完了。为这点东西把一次已经做完的卸载判成失败，LMM 就不会
# 更新数据库，卡片会停在「已部署」指着一个不存在的容器——2026-09-18 真机上踩的
# 正是这个。所以失败只告警，并把没删掉的路径如实列出来，不替它把话圆过去。
leftover=()
for runtime_path in "$VENDOR_DIR/.cache" "$VENDOR_DIR/.venv" "$VENDOR_DIR/.deploy-env" "$CARD_DIR/.venv"; do
    lmm_rm_rf_force "$runtime_path" || leftover+=("$runtime_path")
done
if [ "${#leftover[@]}" -gt 0 ]; then
    echo "警告：以下运行期缓存没能删除，已跳过：${leftover[*]}" >&2
    echo "      它们是容器以 root 身份写入的可重建缓存，不影响本次卸载的结果；" >&2
    echo "      需要时可在设备上执行：sudo rm -rf ${leftover[*]}" >&2
fi

if [ "$purge_weights" = "1" ]; then
    echo "卸载完成，权重已删除。引擎镜像（$IMAGE）保留，可在「已安装引擎」里单独删除。"
elif [ -d "$target" ]; then
    echo "卸载完成。按你的选择保留了模型权重：$target（重新下载这张卡时可直接复用）。"
    echo "引擎镜像（$IMAGE）同样保留，可在「已安装引擎」里单独删除。"
else
    # 不能顺口说成"权重已保留"：那个目录根本不在。
    echo "卸载完成。按你的选择不删除权重，但没有找到权重目录：$target"
fi
