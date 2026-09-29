#!/bin/bash
# 部署 = 把两台机器组成集群并把模型推过去跑起来。**这一步才需要直连线和从机。**
#
# 目标是"填一次表就不用管了"。**连接从机**（管理网地址/用户名/密码、投递公钥、
# 核对 sudo、补 docker 组）是每张双机卡都要的通用能力，由 LMM 提供
# （"$LMM_WORKER" connect，见 LMM 的 core/worker.rs）。这里只做这张卡特有的事：
#
#   1. 还没配对时：让 LMM 连上从机 → 本机配直连网卡（init --role head）
#      → 借 LMM 的从机通道推送交付包、测出从机直连网口、远程 init --role worker
#      → 走直连线 pair
#   2. 走**直连线** deploy --offline（推镜像和权重 → 双机校验 → 启动）
#
# 已经配对好的集群不碰管理网：真机上遇到过主机的有线管理网掉了、只剩一个连不到
# 从机的访客 WiFi，这时部署照样该零输入地走完。
#
# 为什么引导必须走管理网：主机跟从机说的每一句话都是
# `ssh <直连IP> python3 $worker_dir/controller.py _node ...`——从机上没有这个包
# 就一句话都说不出口；而包要靠 init --role worker 才能把直连网络配起来。先有鸡
# 还是先有蛋，只能借一条已经通的路（管理网）把包送过去。
#
# 第二次部署这张卡是**零输入**的：从机信息 LMM 记着，两端网络已配好，什么都不用再问。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

if [ -z "${LMM_WORKER:-}" ]; then
    echo "ERROR: 这张卡需要由新版 LMM 部署（由 LMM 负责连接从机），请升级 LMM 后重试" >&2
    exit 1
fi

lmm_link_model_dir

# 权重和清单是 --offline 路径的前提，先检查再折腾网络——不然用户填完表才被告知
# "你还没下载"。
[ -f "$CARD_DIR/data/model-manifest.json" ] || {
    echo "ERROR: 还没有模型校验清单，请先完成「下载」（或把权重拷进 $MODEL_DIR 后重新下载以生成清单）" >&2
    exit 1
}

WORKER_DIR="$(lmm_worker_dir_default)"

###############################################################################
# 第 1 步：本机这边还需要问什么
###############################################################################
# 本机密码只在"确实要配网卡"且"sudo 不免密"时才问。
# 出厂镜像如果配了免密 sudo，这一栏就不出现——先试再问，别无脑弹框。
need_head_pw=0
if ! lmm_state_exists && ! sudo -n -v 2>/dev/null; then
    need_head_pw=1
fi

# 本机直连网卡：只有一个候选就直接用，不打扰用户
head_ifaces="$(lmm_link_candidates)"
head_iface=""
if lmm_state_exists; then
    head_iface="$(lmm_node_get_iface)"
    echo "本机直连网络已初始化（$head_iface），跳过配置。"
elif [ -z "$head_ifaces" ]; then
    echo "ERROR: 没有检测到处于 ACTIVE 状态的直连接口。请确认 QSFP 线缆两端都已插好、从机已开机；引擎和权重都已就绪，接好线再点一次「部署」即可" >&2
    exit 1
elif [ "$(printf '%s\n' "$head_ifaces" | wc -l)" = 1 ]; then
    head_iface="$head_ifaces"
    echo "本机直连网卡：$head_iface（唯一候选，自动选定）"
fi

if lmm_pair_ok; then
    echo "与从机的配对已就绪，跳过引导。"
else
    # 第一次会弹一张表问从机信息；之后用 LMM 保存的，零输入。连不上 / 密码失效时
    # LMM 自己会带着原因请用户重填，这里不用管。
    echo "=== 连接从机（管理网）==="
    "$LMM_WORKER" connect
    worker_target="$("$LMM_WORKER" info)"
    worker_user="${worker_target%@*}"
    worker_host="${worker_target#*@}"

    head_pw=""
    form="$(lmm_build_form "$need_head_pw" "$head_iface" "$head_ifaces")"
    if [ -n "$form" ]; then
        answers="$(lmm_prompt_form "$form")"
        head_pw="$(lmm_answer "$answers" head_pw)"
        [ -n "$head_iface" ] || head_iface="$(lmm_answer "$answers" head_iface)"
    fi

    ###########################################################################
    # 第 2 步：配置本机直连网卡
    ###########################################################################
    if ! lmm_state_exists; then
        echo
        echo "=== 配置本机直连网卡（$head_iface）==="
        if [ "$need_head_pw" = 1 ]; then
            # 交付包内部直接调 `sudo nmcli`（不是 sudo -A），没法用 SUDO_ASKPASS
            # 递密码。改成先把 sudo 时间戳热一遍（默认 15 分钟、按 tty 记，我们
            # 和 controller.py 共用同一个 pty），后面它自己调 sudo 就不会再问。
            printf '%s\n' "$head_pw" | sudo -S -v 2>/dev/null || {
                echo "ERROR: 本机管理员密码不正确，或当前用户没有 sudo 权限" >&2
                exit 1
            }
        fi
        ./megacube.sh init --role head --ifaces "$head_iface" \
            --worker-user "$worker_user" --worker-dir "$WORKER_DIR"
    fi
    unset head_pw
    worker_link_ip="$(lmm_worker_ip)"

    ###########################################################################
    # 第 3 步：走管理网把从机引导起来
    ###########################################################################
    echo
    echo "=== 通过管理网 $worker_host 引导从机 ==="
    lmm_bootstrap_worker "$head_iface" "$worker_link_ip"

    ###########################################################################
    # 第 4 步：走直连线完成配对
    ###########################################################################
    echo
    echo "=== 与从机建立直连链路上的互信 ==="
    lmm_trust_worker_host_key "$worker_link_ip"
    ./megacube.sh pair
fi

###############################################################################
# 第 5 步之前：两端是不是已经有服务在跑
###############################################################################
# 包的 deploy 只要发现任意一端有容器在跑就拒绝执行（"服务正在运行，请先 stop 再
# deploy"），用户点「部署」就卡死在这。走到这里通常是下面两种情况，都要能接住：
#   - 两端都在跑且接口健康：集群本来就是好的（上次部署成功后 LMM 的状态没跟上、
#     或者在 LMM 之外起过）。没必要停服务、再把 168GB 重新校验分发一遍，直接算
#     部署完成，接下来的「启用」是一次快速确认。
#   - 只有一端在跑：上次停止只停掉了一半。包的 stop 两端互不中断——从机那一步
#     失败了，主机照样会停，于是剩下半截集群。这种状态没法用，先两端都停干净。
ranks="$(lmm_ranks_running)"
read -r head_up worker_up healthy <<<"$ranks"
if [ "$head_up" = 1 ] && [ "$worker_up" = 1 ] && [ "$healthy" = 1 ]; then
    echo "两端服务都在运行且接口健康，无需重新分发，部署完成。"
    echo "API 监听 $(lmm_node_get bind):$(lmm_node_get port)"
    exit 0
fi
if [ "$head_up" = 1 ] || [ "$worker_up" = 1 ]; then
    echo
    echo "=== 检测到上次的服务没有完整停止（主机：$([ "$head_up" = 1 ] && echo 运行中 || echo 已停)，" \
         "从机：$([ "$worker_up" = 1 ] && echo 运行中 || echo 已停)），先把两端都停掉再部署 ==="
    ./megacube.sh stop
fi

###############################################################################
# 第 5 步：双机体检、分发镜像与权重、启动
###############################################################################
echo
echo "=== 双机体检、分发镜像与权重、启动服务 ==="
echo "注意：包的这一步在分发校验通过后会直接把服务拉起来，所以之后的「启用」是一次快速确认。"
./megacube.sh deploy --offline --timeout 3600

echo "双机部署完成。主机：本机；从机：$(lmm_worker_target)。"
echo "API 监听 $(lmm_node_get bind):$(lmm_node_get port)"
