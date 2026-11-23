#!/bin/bash
# 六个入口脚本共用的一段壳。
#
# 厂商交付的是 4 个脚本（deploy / start / stop / uninstall）+ DEPLOYMENT.md，
# 原件一字不改地放在 vendor/ 下，入口脚本只负责把 LMM 的语义翻译成对它们的调用。
#
# **为什么不能直接把 vendor/deploy.sh 当成 LMM 的 deploy**：它是个一条龙脚本，
# 一口气做了「查硬件 → 装 Docker → 装 NVIDIA 容器工具 → 拉镜像 → 验 GPU →
# 下 23GB 权重 → 写 .deploy-env」。而 LMM 把这些拆成三步给用户看进度：
#     装引擎   = 拉镜像 + 验 GPU
#     下载     = 下权重（要断点续传和进度条）
#     部署     = 生成运行配置
# 所以拆法是：拉镜像和下权重由我们自己做（进度、续传、取消都要按 LMM 的约定来），
# 等这两样都齐了再调 vendor/deploy.sh —— 那时它的前五步全部命中「已就绪」直接跳过，
# 第六步也因为权重已完整而跳过下载，只剩下真正属于「部署」的那件事：验一次 GPU
# 直通方式并写出 .deploy-env。一条龙脚本被完整跑过一遍，逻辑没有被我们复制。

set -euo pipefail

CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
VENDOR_DIR="$CARD_DIR/vendor"
cd "$CARD_DIR"

[ -x "$VENDOR_DIR/deploy.sh" ] || {
    echo "ERROR: 卡片包不完整，缺少 vendor/deploy.sh" >&2
    exit 1
}

# 镜像名和容器名以厂商脚本里的常量为准，这里必须跟它们一致（vendor 里是写死的，
# 不接受环境变量覆盖），改了就对不上。
IMAGE="lmsysorg/sglang:v0.5.19"
CONTAINER_NAME="qwen38-sglang"

# 对外端口刻意避开 30000。
#
# 测试商店里已经有一张 qwen38-27b-nvfp4-mtp（SGLang v0.5.16），它占的就是 30000。
# 同一台设备上两张卡都装着是常态（用户要横向对比两个引擎版本），端口撞了的表现
# 是后启动的那个 docker run 直接失败，报错还很难懂。容器内部仍然是 30000，改的
# 只是宿主侧映射（vendor 脚本 `-p "$PORT:30000"`），不影响厂商验证过的任何参数。
export PORT="${PORT:-30001}"

# 权重目录**不能**跟 mtp 那张卡共用。
#
# 两张卡的模型名字看着一样都是 Qwen3.8-27B-NVFP4，但根本不是同一份文件：
#   mtp 卡    RadixArk/Qwen3.8-27B-NVFP4  → model-0000{1,2,3}-of-00003.safetensors（21GB，3 分片）
#   这张卡    unsloth/Qwen3.8-27B-NVFP4   → model.safetensors + model_mtp.safetensors（23.4GB，单文件）
# 落到同一个目录里会互相覆盖不掉、又都通不过各自的完整性检查，用户看到的是
# 「两张卡都下不完」。所以这里带上来源前缀单独存一份。
export MODEL_DIR="${LMM_MODELS_DIR:-$HOME/models}/Qwen3.8-27B-NVFP4-unsloth"

# 镜像拉取的国内镜像站链，跟 vendor/deploy.sh 里的 MIRROR_LIST 保持一致。
#
# 这是全卡唯一一处从 vendor 抄过来的常量：装引擎那步不能调 vendor/deploy.sh
# （见文件开头），但又必须用同一条回退链，否则同一台设备上「装引擎」能拉通而
# 「部署」拉不通（或者反过来）会非常费解。vendor 升级换了镜像站时，这里要跟着改。
MIRROR_LIST="${MIRROR_LIST:-m.daocloud.io/docker.io docker.1ms.run docker.m.daocloud.io DIRECT}"

PIP_MIRROR="${PIP_MIRROR:-https://pypi.tuna.tsinghua.edu.cn/simple}"

# 权重完整性清单（规范 3.3）。取自 hf-mirror 上 unsloth/Qwen3.8-27B-NVFP4 的
# 仓库元数据：13 个文件、合计 23,444,511,852 字节。
#
# 分两层查：
#   便宜的一层（lmm_model_complete）——大文件核对**字节数**，小文件只查存在。
#     每次点操作都会走这层。核字节数而不是只查存在很关键：下载被中断时小文件
#     早就落地了，只有几十 GB 的大文件是半截的，只查存在会让「重新下载」永远
#     误判成「已经有了」，表现就是"怎么点下载都不管用"。
#   贵的一层（lmm_verify_weights）——按 weights.sha256 逐个算哈希，只在下载刚
#     结束时做一次，做完留个记号，之后不再重复算 23GB。
LMM_BIG_FILES=(
    "model.safetensors:22568192096"
    "model_mtp.safetensors:849400392"
)
LMM_SMALL_FILES=(
    config.json generation_config.json model.safetensors.index.json
    tokenizer.json tokenizer_config.json vocab.json
    chat_template.jinja preprocessor_config.json video_preprocessor_config.json
)
LMM_WEIGHTS_MANIFEST="$CARD_DIR/weights.sha256"
LMM_VERIFIED_MARK="$MODEL_DIR/.lmm-verified"
# 贵的那一层（按 weights.sha256 逐个算哈希）写在 download_model.sh 里：只有它一个
# 调用方，而且自检工具只看六个入口脚本，藏在这个共用壳里会被当成"没做校验"。

# 便宜的一层：大小对不对、该有的文件在不在。
lmm_model_complete() {
    local dir="${1:-$MODEL_DIR}" entry name want actual f
    [ -d "$dir" ] || return 1
    for entry in "${LMM_BIG_FILES[@]}"; do
        name="${entry%%:*}"
        want="${entry##*:}"
        actual="$(stat -c %s "$dir/$name" 2>/dev/null || echo 0)"
        [ "$actual" = "$want" ] || return 1
    done
    for f in "${LMM_SMALL_FILES[@]}"; do
        [ -f "$dir/$f" ] || return 1
    done
    return 0
}

# docker 怎么调。设备出厂就把登录用户放进了 docker 组，正常情况下不需要 sudo；
# 退化到 sudo 时只用免密的那种，交互式密码框由 LMM 的提权通道负责，脚本自己不弹。
LMM_DOCKER=""
lmm_init_docker() {
    [ -n "$LMM_DOCKER" ] && return 0
    if docker info >/dev/null 2>&1; then
        LMM_DOCKER="docker"
    elif command -v sudo >/dev/null 2>&1 && sudo -n docker info >/dev/null 2>&1; then
        LMM_DOCKER="sudo -n docker"
    else
        echo "ERROR: 连不上 Docker 服务。请确认设备的容器运行环境正常，并且当前用户在 docker 组里" >&2
        echo "       （检查：docker info；加组：sudo usermod -aG docker \$USER，之后需要重新登录）" >&2
        return 1
    fi
    return 0
}
lmm_docker() { $LMM_DOCKER "$@"; }

# 删一个路径，**容器以 root 身份写进来的东西也要能删掉**。
#
# 为什么需要它
# -----------
# 引擎容器是以 root 跑的，它写进挂载目录的文件属主就是 root——SGLang 的
# flashinfer JIT 产物（vendor/.cache 下面那几百个编译出来的 kernel）是典型。
# 宿主上的普通用户删不掉这些：这不是文件本身权限的事，是**父目录属于 root 且
# 没给别人写位**，unlink 这个动作根本做不了，chmod 也改不动。
#
# 2026-09-18 真机上就是这一下让整个卸载失败的：容器已删、22GB 权重已删，脚本
# 却在清 21MB 缓存时非零退出，LMM 按"卸载失败"处理不动数据库，卡片停在
# 「已部署」指着一个不存在的容器。
#
# 办法：先普通 rm；删不动就把父目录挂进引擎镜像里以 root 身份删——**容器写进来
# 的，让容器删回去**。不用 sudo，也不弹提权框。`--entrypoint rm` 是必须的，
# 镜像自带的 ENTRYPOINT 会把后面的参数当成它自己的参数吃掉。
#
# 返回 0 = 路径已经不在了；返回 1 = 还在。调用方自己决定是告警还是失败。
lmm_rm_rf_force() {
    local p="$1"
    [ -e "$p" ] || return 0
    rm -rf -- "$p" 2>/dev/null || true
    [ -e "$p" ] || return 0

    local parent name
    parent="$(cd -- "$(dirname -- "$p")" 2>/dev/null && pwd)" || return 1
    name="$(basename -- "$p")"
    lmm_init_docker || return 1
    lmm_docker run --rm --entrypoint rm \
        -v "$parent:/lmm-rm" "$IMAGE" -rf -- "/lmm-rm/$name" >/dev/null 2>&1 || true
    [ ! -e "$p" ]
}

# 长时间不出声会被 LMM 判定为卡死并强杀（规范 6.2：装引擎/部署/启用 30 分钟，
# 下载/停用/卸载 5 分钟）。在后台任务旁边打心跳。
# 用法：<命令> & lmm_heartbeat $! "正在做什么"
lmm_heartbeat() {
    local pid="$1" what="$2" start=$SECONDS
    while kill -0 "$pid" 2>/dev/null; do
        sleep 15
        kill -0 "$pid" 2>/dev/null || break
        echo "$what…已用时 $((SECONDS - start))s"
    done
    wait "$pid"
}
