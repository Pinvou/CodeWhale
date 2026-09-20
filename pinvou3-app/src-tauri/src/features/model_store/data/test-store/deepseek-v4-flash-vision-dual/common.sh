#!/bin/bash
# 双机卡片的六个入口脚本共用的一段壳。
#
# 跟单机那张卡一样，研究团队交付的是一个自校验的完整部署包（40 个文件），
# 入口脚本只负责把 LMM 的语义翻译成 megacube.sh 的子命令，不复制它的业务逻辑。
#
# **职责划分（这张卡最关键的一条）**：
#   装引擎 / 下载 —— 只用本机，不碰从机，不需要直连线
#   部署         —— 才开始配直连网络、跟从机配对、把镜像和权重推过去
#
# 下载 168GB 是个几小时的活，把它压在"两台机器都到位且线插好"之后，等于让用户
# 干等；而且权重完全可以从 U 盘拷进来。包本身就提供了 `deploy --offline`
# ——"权重已在本机，只做校验+分发+启动"——正好对上这个分工，不用我们去拆它的逻辑。
#
# **跟 LMM 的分工**：连接从机（管理网地址、用户名、密码、免密登录、sudo、docker
# 组）是每张双机卡都要的通用能力，由 LMM 提供，经 "$LMM_WORKER" 交给我们
# （connect / exec / sudo / push / detect-link / diagnose-link）。只在确实要引导
# 从机时才 connect——已配对的集群用不到管理网。这里只留这张交付包特有的：
# 它要的额外环境、推哪些文件、跑它自己的 init / pair / deploy。
# 需要跟人对话的只剩本机那两项（本机 sudo 密码、多口时选网卡），走 LMM_PROMPT 通道。

set -euo pipefail

CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$CARD_DIR"

[ -x ./megacube.sh ] || { echo "ERROR: 卡片包不完整，缺少 megacube.sh" >&2; exit 1; }

# 这张卡的所有大文件都放在用户选的模型盘上，而不是包默认的
# <卡片目录>/data/cache ——那是 LMM 的隐藏数据目录，157GB 不该落在那儿。
CARD_STORE="$(realpath -m "${LMM_MODELS_DIR:-$HOME/models}/DeepSeek-V4-Flash-Vision-Exp")"
MODEL_DIR="$CARD_STORE/model"

# 把 <卡片目录>/data/cache 整个做成指向模型盘的软链。
#
# **软链必须打在 cache 这一层，不能打在 cache/model 上。** 交付包的 compose 是
# 这么挂的：
#     ${HF_CACHE}:/cache/huggingface        # HF_CACHE = <卡片目录>/data/cache
#     DSPARK_MODEL=/cache/huggingface/model
# 也就是说它挂的是 **cache 这个父目录**。软链打在 cache/model 上时，容器里那个
# 链接指向的宿主路径根本不存在，于是它是**悬空**的，模型一个文件都读不到——真机
# 上推理容器就是这么 FATAL 退出的（编码补丁报 drift:no-image-placeholder），而
# 从机因为是 rsync 出来的真目录反而正常，两边表现不一致查了半天。
#
# 反过来，挂载源**本身**是软链时 docker 会在宿主侧解析，容器里看到的就是真目录。
# 这一点在设备上实测确认过。
#
# purge-data 那条 `require(DATA.resolve() == ROOT/'data' and not DATA.is_symlink())`
# 约束的是 data，不是 data/cache，所以这样改不影响它；它在容器里 rmtree 掉的是
# 那个（容器内已悬空的）cache 链接本身，碰不到模型盘上的真数据，真正删权重仍由
# 本卡的 uninstall.sh 明确去做。
lmm_link_model_dir() {
    local link="$CARD_DIR/data/cache"
    mkdir -p "$MODEL_DIR" "$CARD_DIR/data"

    if [ -L "$link" ]; then
        [ "$(readlink -f "$link")" = "$CARD_STORE" ] || ln -sfn "$CARD_STORE" "$link"
        return 0
    fi

    if [ -d "$link" ]; then
        # 老布局：cache 是真目录，里面的 model 是指向模型盘的软链（就是上面说的
        # 那个会让容器读不到模型的布局）。把权重挪进 CARD_STORE/model，再把软链
        # 上移一层。同一个文件系统内是 rename，不会真的搬 157GB 的数据。
        if [ -L "$link/model" ]; then
            local old
            old="$(readlink -f "$link/model")"
            rm -f "$link/model"
            if [ -n "$old" ] && [ -d "$old" ] && [ "$old" != "$MODEL_DIR" ]; then
                echo "调整权重目录布局：$old -> $MODEL_DIR"
                mv -- "$old" "$old.lmm-migrating"
                mkdir -p "$CARD_STORE"
                mv -- "$old.lmm-migrating" "$MODEL_DIR"
            fi
        fi
        # cache 里剩下的全是运行期缓存（HF 缓存、vLLM 编译缓存、NCCL 的
        # nccl-fr 调试目录……），整体搬到 CARD_STORE 去，不做白名单：
        # 白名单一定会漏——真机上就漏了容器以 root 身份建的 nccl-fr，导致迁移中断。
        # 搬而不是删，所以漏判的代价只是多占点空间，不会丢数据。
        mkdir -p "$CARD_STORE"
        local entry
        for entry in "$link"/* "$link"/.[!.]* "$link"/..?*; do
            [ -e "$entry" ] || continue
            mv -- "$entry" "$CARD_STORE"/ 2>/dev/null && continue
            # 搬不动最常见的原因是容器以 root 建的目录：rename 一个目录需要对**该
            # 目录本身**有写权限（要改它的 ..），sky 对 root 的目录没有。真机上
            # 遇到的是 NCCL 的 nccl-fr 调试目录。
            # 但删掉一个空目录只需要**父目录**的写权限，跟属主无关——所以空的就直接
            # 删掉（本来就是可重建的运行期产物），非空的才需要人来处理。
            rmdir -- "$entry" 2>/dev/null && continue
            echo "ERROR: 无法把 $entry 移到 $CARD_STORE，也删不掉（属主是 root 且非空？）" >&2
            echo "       请手动处理后重试：sudo rm -rf '$entry'" >&2
            return 1
        done
        rmdir "$link" || {
            echo "ERROR: $link 清空后仍无法删除，请手动检查" >&2
            return 1
        }
    fi

    ln -sfn "$CARD_STORE" "$link"
}

# 长时间不出声会被 LMM 判定为卡死。在后台任务旁边打心跳。
# 用法：lmm_heartbeat <后台进程 pid> "正在校验模型"
lmm_heartbeat() {
    local pid="$1" what="$2" start=$SECONDS
    while kill -0 "$pid" 2>/dev/null; do
        sleep 15
        kill -0 "$pid" 2>/dev/null || break
        echo "$what…已用时 $((SECONDS - start))s"
    done
    wait "$pid"
}

# 向用户要一次输入。用法：answer="$(lmm_prompt password '请输入…')"
#
# 标记行必须打到 stderr：stdout 被 $( ) 捕获了，marker 打在 stdout 上 LMM 根本
# 看不到，用户会对着一个不动的进度条等到超时。stderr 和 stdout 接的是同一个
# pty，LMM 两边都读，所以标记照样送得到。
lmm_prompt() {
    local kind="$1" msg="$2" answer
    printf 'LMM_PROMPT=%s|%s\n' "$kind" "$msg" >&2
    IFS= read -r answer || { echo "ERROR: 未收到输入，操作中止" >&2; return 1; }
    printf '%s' "$answer"
}

lmm_state_exists() { [ -f "$CARD_DIR/node.json" ]; }

# 从 node.json 里取字段。直连子网是 /30，主机固定 .1、从机固定 .2 ——从机 IP
# 是由子网推出来的，不是也不该由用户随便填。
lmm_node_get() {
    python3 -c "import json,sys;print(json.load(open('$CARD_DIR/node.json'))['$1'])"
}
lmm_worker_ip() {
    python3 -c "
import ipaddress, json
c = json.load(open('$CARD_DIR/node.json'))
print(ipaddress.ip_address(c['rails'][0]['ip']) + 1)"
}
lmm_worker_target() { printf '%s@%s' "$(lmm_node_get worker_user)" "$(lmm_worker_ip)"; }

# 先把从机**直连地址**的主机密钥收进 known_hosts，包的 pair 用的是
# StrictHostKeyChecking=yes。（管理网那一侧的主机密钥，LMM 连从机时已经收过了。）
# 直连线是一根点对点的专线，这里做 TOFU 是合理的。
lmm_trust_worker_host_key() {
    local ip="$1"
    mkdir -p "$HOME/.ssh" && chmod 700 "$HOME/.ssh"
    touch "$HOME/.ssh/known_hosts" && chmod 600 "$HOME/.ssh/known_hosts"
    if ! ssh-keygen -F "$ip" >/dev/null 2>&1; then
        ssh-keyscan -T 10 "$ip" >>"$HOME/.ssh/known_hosts" 2>/dev/null || {
            lmm_diagnose_link "$(lmm_node_get_iface)" "$ip" >&2
            return 1
        }
    fi
}

lmm_node_get_iface() {
    python3 -c "import json;print(json.load(open('$CARD_DIR/node.json'))['rails'][0]['iface'])"
}

# 连不上从机直连地址时的自检。"线的问题还是对端的问题"是通用的，由 LMM 判断
# （物理链路 / ARP / sshd 分开说清楚）；这里只补一句这张卡特有的修法。
lmm_diagnose_link() {
    local iface="$1" peer="$2"
    if [ -n "${LMM_WORKER:-}" ]; then
        "$LMM_WORKER" diagnose-link "$iface" "$peer" 2>&1 || true
    else
        echo "ERROR: 连不上从机 $peer"
    fi
    echo "  这张卡的修法：在从机上执行 cd $(lmm_worker_dir_default) && bash megacube.sh init --role worker"
}

# 从机是否已经配对成功（能免密登录并且两端配置对得上）。
lmm_pair_ok() {
    lmm_state_exists || return 1
    python3 - <<'PY' >/dev/null 2>&1
import controller
controller.pair_check(controller.config())
PY
}

# 两端各自有没有推理容器在跑、接口健不健康，输出一行 "<主机> <从机> <健康>"，各为 0/1。
#
# "在跑"的判断照抄包里 deploy 拒绝执行时用的那一条（按 compose 项目标签 docker ps），
# 这样我们认为"都停了"和包认为"都停了"永远是同一个结论，不会出现我们放行、包又拒绝。
# 连不上从机时直接报错退出：这时候继续往下走，包的 deploy 也一样会失败，只是报错
# 更难懂。
lmm_ranks_running() {
    python3 - <<'PY'
import shlex, subprocess, sys
import controller
c = controller.config()
q = ['docker', 'ps', '-q', '--filter', 'label=com.docker.compose.project=' + controller.PROJECT]
head = bool(controller.run(q, capture=True).strip())
try:
    worker = bool(controller.run(controller.ssh_base(c) + [shlex.join(q)], capture=True).strip())
except subprocess.CalledProcessError:
    sys.exit('ERROR: 经直连线查询从机服务状态失败，请确认从机已开机、直连线已插好后再点「部署」')
try:
    controller.api(c, '/health', timeout=5)
    healthy = True
except Exception:
    healthy = False
print(int(head), int(worker), int(healthy))
PY
}

# ---------------------------------------------------------------- 表单式输入

# 打出表单标记、读回一行 JSON。跟 lmm_prompt 一样，标记必须走 stderr——
# stdout 被 $( ) 捕获了，标记打在 stdout 上 LMM 根本看不到。
lmm_prompt_form() {
    local spec="$1" answer
    printf 'LMM_PROMPT_FORM=%s\n' "$spec" >&2
    IFS= read -r answer || { echo "ERROR: 未收到输入，操作中止" >&2; return 1; }
    printf '%s' "$answer"
}

# 从表单答案里取一项。走环境变量传值，不拼进 python 源码——答案里有密码，
# 拼字符串既会被引号搞崩，也可能把内容漏进报错信息里。
lmm_answer() {
    ANSWERS_JSON="$1" FIELD="$2" python3 -c '
import json, os
print(json.loads(os.environ["ANSWERS_JSON"]).get(os.environ["FIELD"], ""))'
}

# 拼出这一轮本机还需要问的字段；一项都不需要时什么也不输出（调用方据此不弹表单）。
# 已经能免密 sudo 就不问本机密码；本机网卡候选唯一就不问网卡——能不打扰用户的
# 就别打扰。从机的地址/用户名/密码不在这里问：那是 LMM 的事，部署前就问过了。
lmm_build_form() {
    NEED_HEAD_PW="$1" HEAD_IFACE="$2" HEAD_IFACES="$3" python3 <<'PY'
import json, os
fields = []
if os.environ.get("NEED_HEAD_PW") == "1":
    fields.append({"id": "head_pw", "label": "本机登录密码", "kind": "password",
                   "hint": "配置本机直连网卡需要管理员权限"})
if not os.environ.get("HEAD_IFACE"):
    cands = [x for x in os.environ.get("HEAD_IFACES", "").split() if x]
    fields.append({"id": "head_iface", "label": "本机直连网卡", "kind": "text",
                   "value": cands[0] if cands else "",
                   "hint": "检测到多个已接线的口：" + "、".join(cands)})
if fields:
    print(json.dumps({"title": "双机部署：配置本机直连网络", "fields": fields},
                     ensure_ascii=False))
PY
}

# ---------------------------------------------------------------- 引导从机

lmm_worker_dir_default() {
    if lmm_state_exists; then
        lmm_node_get worker_dir
    else
        printf '%s/megacube-deepseek' "$HOME"
    fi
}

# 候选直连口里**确实插了线**的那些（carrier=1）。包的 candidates() 只看 RDMA
# 端口是不是 ACTIVE，这里再叠一层物理链路，免得把没接线的口推荐给用户。
lmm_link_candidates() {
    python3 - <<'PY'
import pathlib
import controller
for iface, _hca, _port in controller.candidates():
    try:
        if (pathlib.Path('/sys/class/net') / iface / 'carrier').read_text().strip() == '1':
            print(iface)
    except OSError:
        pass
PY
}

# 走管理网把从机引导到"可以配对"的状态：体检 -> 推包 -> 测直连网口 -> 远程 init。
# 调用前要先 "$LMM_WORKER" connect（免密登录、sudo、docker 组由它负责），这里的
# 每一次从机操作都经 "$LMM_WORKER"。每一步都先查状态再动手，中途失败重跑能接着走。
lmm_bootstrap_worker() {
    local head_iface="$1" worker_link_ip="$2"

    echo "[1/3] 检查从机上交付包需要的环境"
    # 通用的（docker、rsync、python3、ip、ping）LMM 已经查过，这里只查这张交付包
    # 自己要的：它用 nmcli 配直连网络，推理走 RDMA。缺什么一次列全。
    local missing
    missing="$("$LMM_WORKER" exec 'miss=""; command -v nmcli >/dev/null 2>&1 || miss="$miss nmcli"; [ -d /dev/infiniband ] || miss="$miss RDMA驱动(/dev/infiniband)"; printf "%s" "$miss"')"
    if [ -n "$missing" ]; then
        echo "ERROR: 从机缺少以下环境，装好之后再重试：$missing" >&2
        return 1
    fi
    echo "      OK"

    echo "[2/3] 把交付包推送到从机 $WORKER_DIR"
    # 推的是主机自己正在用的这一份，所以两端 SHA256SUMS 必然一致——包的
    # pair_check 会核对它，两边各自去商店取会漂移。
    # 只推交付包本身：不推我们的入口脚本（从机不需要），也不推主机的运行期
    # 状态（node.json 是每台机器自己的；data/ 里是 157GB 权重，走直连线分发）
    "$LMM_WORKER" push "$CARD_DIR" "$WORKER_DIR" \
        --exclude 'data/' --exclude 'node.json' --exclude '__pycache__/' \
        --exclude '.operation.lock' --exclude 'logs/' --exclude 'worker-access.json' \
        --exclude 'common.sh' --exclude 'install_engine.sh' --exclude 'download_model.sh' \
        --exclude 'deploy.sh' --exclude 'enable.sh' --exclude 'stop.sh' --exclude 'uninstall.sh'
    "$LMM_WORKER" exec "cd '$WORKER_DIR' && python3 -c 'import controller; controller.verify_bundle()'" || {
        echo "ERROR: 推送到从机的交付包自校验没通过，请重试" >&2
        return 1
    }
    echo "      OK"

    echo "[3/3] 在从机上初始化直连网络"
    if "$LMM_WORKER" exec "[ -f '$WORKER_DIR/node.json' ]"; then
        echo "      从机已初始化过，跳过"
    else
        # 候选口交给包自己挑（只认 ACTIVE 的 RDMA 口），比 LMM 的通用规则（插着线、
        # 不是管理网卡、不是虚拟网卡）更准。
        local cands worker_iface
        cands="$("$LMM_WORKER" exec "cd '$WORKER_DIR' && python3 -c 'import controller; [print(i) for i, _h, _p in controller.candidates()]'")"
        # shellcheck disable=SC2086  # 候选口要拆成多个参数
        worker_iface="$("$LMM_WORKER" detect-link "$head_iface" "$worker_link_ip" $cands)" || {
            echo "       也可以在从机上手动指定后重新点部署：" >&2
            echo "       cd $WORKER_DIR && bash megacube.sh init --role worker --ifaces <网口名>" >&2
            return 1
        }
        echo "      从机直连网卡：$worker_iface（由链路实测确定）"
        # 包内部那几条 sudo nmcli 要密码。LMM 的 sudo 通道会让命令里每一处 sudo
        # 都自动拿到密码，不需要 tty，密码也不会回显进日志。
        "$LMM_WORKER" sudo "cd '$WORKER_DIR' && bash megacube.sh init --role worker --ifaces '$worker_iface'" \
            2>&1 | sed 's/^/      /' || {
            echo "ERROR: 从机初始化直连网络失败（见上方输出）" >&2
            return 1
        }
    fi

    # 确认从机真的拿到了直连地址，别等到 pair 才发现
    local tries=0
    until ping -c 1 -W 2 -I "$head_iface" "$worker_link_ip" >/dev/null 2>&1; do
        tries=$((tries + 1))
        if [ "$tries" -ge 15 ]; then
            lmm_diagnose_link "$head_iface" "$worker_link_ip" >&2
            return 1
        fi
        echo "      等待从机直连地址生效…（$tries/15）"
        sleep 2
    done
    echo "      从机直连地址 $worker_link_ip 已可达"
}
