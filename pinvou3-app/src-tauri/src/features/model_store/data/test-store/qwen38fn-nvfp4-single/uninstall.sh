#!/bin/bash
# 卸载模型：删容器和部署产物。**权重删不删由用户在界面上选**，LMM 用
# LMM_PURGE_WEIGHTS 把这个选择传进来（规范 4.6）：
#
#   LMM_PURGE_WEIGHTS=1  彻底删除：约 133GB 权重一起删，不可撤销
#   LMM_PURGE_WEIGHTS=0  仅卸载：权重留在盘上，重装时不用再下 133GB
#
# 变量没有时按 0（保留）算，跟规范 4.6 的默认值一致——也正好是包自己的行为：
# 它的 uninstall 只删容器、刻意保留权重（面向"工程师手工运维"的场景）。所以
# "彻底删除"这一步由我们这层在它跑完之后补做，厂商原件一个字节都不改。
#
# 引擎镜像不在这里删：它由"已安装引擎"那边单独管理。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

./megacube.sh uninstall

# 从 config.json 读实际路径，而不是用 common.sh 算出来的：用户可能在设置里
# 改过模型盘，那样算出来的是新路径，真正占着盘的却是旧的那个。
# config.json 可能压根没生成过（下载还没开始就被卸载），那样读不到路径也不该
# 让卸载整个失败——退回按当前设置算出来的默认路径。
# 保留权重时同样要读：报给用户的那句"权重在哪儿"必须是真正占着盘的那个目录。
INSTALLED_MODEL_DIR="$(lmm_config_get model_dir 2>/dev/null || echo "$MODEL_DIR")"

if [ "${LMM_PURGE_WEIGHTS:-0}" = "1" ]; then
    if [ -n "$INSTALLED_MODEL_DIR" ] && [ -d "$INSTALLED_MODEL_DIR" ]; then
        echo "正在删除模型权重：$INSTALLED_MODEL_DIR"
        rm -rf -- "$INSTALLED_MODEL_DIR"
    fi
    # 校验记录/来源锁跟权重是一套的，权重删了还留着，下次重新下载时
    # model_assets.py 会拿旧的 model-source.json 去比对，凭空多出一次冲突。
    rm -f -- "$CARD_DIR/state/model-verified.json" "$CARD_DIR/state/model-source.json"
    echo "卸载完成，权重已删除。引擎镜像保留（可在「已安装引擎」中单独删除）。"
elif [ -n "$INSTALLED_MODEL_DIR" ] && [ -d "$INSTALLED_MODEL_DIR" ]; then
    # 权重留着，校验记录和来源锁就也得留着——它们记的是"这份权重校验过、来自
    # 哪个源"。删了记录留下权重，下次重新下载会把这 133GB 从头再校验一遍。
    echo "卸载完成。按你的选择保留了模型权重：$INSTALLED_MODEL_DIR（重新下载这张卡时可直接复用）。"
    echo "引擎镜像同样保留，可在「已安装引擎」中单独删除。"
else
    # 不能顺口说成"权重已保留"：那个目录根本不在。
    echo "卸载完成。按你的选择不删除权重，但没有找到权重目录：$INSTALLED_MODEL_DIR"
fi
