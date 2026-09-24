#!/bin/bash
set -euo pipefail
VENDOR_DIR="$HOME/.local/share/local-model-manager/vendor/ornith15-bf16"
[ -d "$VENDOR_DIR" ] || { echo "ERROR: 未找到部署包" >&2; exit 1; }
cd "$VENDOR_DIR"
# 卸载模型：停容器、删部署产物（scripts/uninstall.sh 会 compose down 并删掉
# runtime/）。**权重删不删由用户在界面上选**，LMM 用 LMM_PURGE_WEIGHTS 把这个
# 选择传进来（规范 4.6）：
#
#   LMM_PURGE_WEIGHTS=1  彻底删除：约 67GB 权重一起删，不可撤销
#   LMM_PURGE_WEIGHTS=0  仅卸载：权重留在盘上，重装时不用再下 67GB
#
# 变量没有时按 0（保留）算，跟规范 4.6 的默认值一致。包自己的 uninstall 也是
# 这个默认：不加 --purge-model 它什么权重都不碰。
#
# 两种选择都不动引擎（docker 镜像）——引擎跟 Z-Image 共用同一个基础镜像，交给
# "已安装引擎"那边单独管理和删除。缓存和 .env 也都保留（不是模型权重本身，留着
# 对下次重新部署更省事）。
if [ "${LMM_PURGE_WEIGHTS:-0}" = "1" ] && [ -f .env ]; then
    set -a
    . .env
    set +a
    if [ -n "${MODEL_PATH:-}" ]; then
        # CONFIRM_DELETE_MODEL 是包里那道"确认删的就是这个目录"的闸门，值必须跟
        # MODEL_PATH 一字不差，否则它会 die。
        CONFIRM_DELETE_MODEL="$MODEL_PATH" ./scripts/uninstall.sh --purge-model
        echo "卸载完成，权重已删除（$MODEL_PATH）。引擎镜像保留，可在「已安装引擎」里单独删除。"
    else
        # .env 里没有 MODEL_PATH：不知道该删哪儿，只能把服务卸掉，并且**说清楚
        # 权重没删**——用户是冲着"把盘收回来"点的，不能让他以为成了。
        ./scripts/uninstall.sh
        echo "警告：.env 里没有 MODEL_PATH，无法定位权重目录，权重未删除，请自行清理。" >&2
    fi
else
    ./scripts/uninstall.sh
    if [ "${LMM_PURGE_WEIGHTS:-0}" = "1" ]; then
        echo "警告：没有 .env（可能还没部署过），无法定位权重目录，权重未删除，请自行清理。" >&2
    else
        echo "卸载完成。按你的选择保留了模型权重，重新下载这张卡时可直接复用。"
    fi
fi
