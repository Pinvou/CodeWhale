#!/bin/bash
# 卸载模型：停掉并移除两端容器、删掉部署产物。权重删不删**由用户在界面上选**，
# LMM 用 LMM_PURGE_WEIGHTS 把这个选择传进来（规范 4.6）：
#
#   LMM_PURGE_WEIGHTS=1  彻底删除：两端权重（各约 168GB）一起删，不可撤销
#   LMM_PURGE_WEIGHTS=0  仅卸载：权重留在盘上，重装时不用再下 168GB
#
# 变量没有时按 0（保留）算，跟规范 4.6 的默认值一致——手工跑这个脚本的人不该
# 凭空少掉 168GB。
#
# 不动的东西（两种选择都一样）：直连网络配置、SSH 互信、运行镜像。前两样是设备级
# 的一次性配置，重装模型还要用；镜像由"已安装引擎"那边单独管理和删除。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

purge_weights=0
[ "${LMM_PURGE_WEIGHTS:-0}" = "1" ] && purge_weights=1

# 还没部署过（没有 node.json）时，包的命令全都会因为读不到配置而失败。这种情况
# 下"卸载"要做的只是按用户的选择处理本机已下好的权重，不该报错。
if lmm_state_exists; then
    # --purge-data 是包里"连两台机器的 data/ 一起删"的开关。用户选保留时就不能加
    # 它——加了从机那 168GB 也没了，而从机上的权重是重新部署时最贵的一份。
    if [ "$purge_weights" = "1" ]; then
        ./megacube.sh uninstall --purge-data
    else
        ./megacube.sh uninstall
    fi
else
    echo "尚未部署过双机集群，跳过集群卸载。"
fi

if [ "$purge_weights" = "1" ]; then
    # 本机的权重和缓存要自己删。包的 purge-data 是在容器里对 data/ 做清理，而我们把
    # data/cache 整个做成了指向模型盘的软链——在容器里那个链接是悬空的，被删掉的只是
    # 链接本身，模型盘上的真数据不会被动。这是刻意的（免得容器里的清理逻辑误伤模型
    # 盘），代价就是真删这一步必须在这里显式做。
    if [ -d "$CARD_STORE" ]; then
        echo "正在删除本机权重与缓存：$CARD_STORE"
        rm -rf -- "$CARD_STORE"
    fi
    # 校验记录和那条软链跟权重是一套的：权重删了还留着，下次重新下载时会拿旧记录
    # 去比对，凭空多出一次冲突；软链也会变成悬空的。
    rm -f -- "$CARD_DIR/data/model-manifest.json" "$CARD_DIR/data/verified"
    rm -f -- "$CARD_DIR/data/cache"
    echo "卸载完成，权重已删除。直连网络、SSH 互信和运行镜像保留。"
else
    # 保留分支不碰 CARD_STORE，也不碰 model-manifest.json / verified / data/cache
    # 那条软链——它们正是"下次重新部署可以直接复用这份权重"所依赖的东西。
    if [ -d "$CARD_STORE" ]; then
        echo "卸载完成。按你的选择保留了本机权重：$CARD_STORE"
        echo "从机上的那一份也保留了（没有用 --purge-data）。直连网络、SSH 互信和运行镜像同样保留。"
    else
        # 不能顺口说成"权重已保留"：本机压根没有这个目录（还没下载过，或者用户
        # 自己删了）。说清楚现状，免得他以为重装时能白捡一份。
        echo "卸载完成。按你的选择不删除权重，但本机没有找到权重目录：$CARD_STORE"
    fi
fi
