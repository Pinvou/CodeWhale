#!/bin/bash
# 卸载 = 停服务 + 删部署产物（配置、工作流副本、日志、输入输出目录）。
#
# 厂商的 package/uninstall.sh 原样用：它先调自己的 stop.sh，再删部署目录，而且
# 删之前要求该目录必须在 $HOME 底下，否则拒绝——被人 export 了一个奇怪的
# LMM_DEPLOY_DIR 也不会 rm -rf 到别处去。
#
# **权重删不删由用户在界面上选**，LMM 用 LMM_PURGE_WEIGHTS 把这个选择传进来
# （规范 4.6）：
#
#   LMM_PURGE_WEIGHTS=1  彻底删除：连 40.34GiB 权重一起删，不可撤销
#   LMM_PURGE_WEIGHTS=0  仅卸载：权重留在盘上（厂商脚本的原生行为）
#
# 变量没有时按 0（保留）算，跟规范 4.6 的默认值一致。
#
# 厂商脚本自己**没有**删权重的开关（它按规范恒保留，最后还会打一句"模型权重按
# 规范保留"）。所以"彻底删除"这一步只能由我们这层在它跑完之后补做——原件一个
# 字节都不改。
#
# **引擎保留**（ComfyUI + Python/CUDA 运行时，约 10GiB），两种选择都一样。在 LMM
# 里引擎属于「已安装引擎」，由那边统一管理和删除——卸载一个模型不该顺手把可能还有
# 别的卡片在用的引擎清掉。厂商留了 LMM_REMOVE_ENGINE=1 这个开关，我们不设置它。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

purge_weights=0
[ "${LMM_PURGE_WEIGHTS:-0}" = "1" ] && purge_weights=1

echo "正在卸载：停掉服务并删除部署目录 $LMM_DEPLOY_DIR"
if [ "$purge_weights" = "1" ]; then
    echo "按你的选择，权重（$MODEL_DIR）也会一起删除；引擎（$LMM_ENGINE_DIR）保留。"
else
    echo "权重（$MODEL_DIR）和引擎（$LMM_ENGINE_DIR）会保留。"
fi

# 原来这里是 exec，权重删除那一步就永远没机会跑了。换成普通调用。
bash "$PKG_DIR/uninstall.sh"

if [ "$purge_weights" = "1" ]; then
    # 跟厂商删部署目录一样的安全边界：只删 LMM_MODELS_DIR 底下的子目录。
    # MODEL_DIR 是 "$LMM_MODELS_DIR/$LMM_MODEL_NAME" 拼出来的，LMM_MODEL_NAME
    # 万一是空串，拼出来就是模型根目录本身——那一下会把用户**所有**模型删光。
    models_root="${LMM_MODELS_DIR:-$HOME/.local/share/lmm/models}"
    case "$MODEL_DIR" in
        "$models_root"/?*)
            if [ -d "$MODEL_DIR" ]; then
                echo "正在删除模型权重：$MODEL_DIR"
                rm -rf -- "$MODEL_DIR"
                echo "卸载完成，权重已删除。引擎保留（可在「已安装引擎」中单独删除）。"
            else
                echo "卸载完成。权重目录不存在，没有可删除的权重：$MODEL_DIR"
            fi
            ;;
        *)
            echo "ERROR: 拒绝删除权重，目标不在模型目录内：$MODEL_DIR" >&2
            echo "       部署产物已经删除，权重请自行确认后手动清理。" >&2
            exit 1
            ;;
    esac
else
    echo "卸载完成。按你的选择保留了模型权重：$MODEL_DIR（重新下载这张卡时可直接复用）。"
fi
