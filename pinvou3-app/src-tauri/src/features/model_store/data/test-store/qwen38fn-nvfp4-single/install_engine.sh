#!/bin/bash
# 引擎 = 部署包锁定的 vLLM 运行时镜像。
#
# 包（1.0.0-rc3 起）专门为这一步提供了 `megacube.sh engine`：拉取/复用固定摘要
# 镜像、校验镜像身份、跑 GPU 与九个补丁目标路径检查，不下载权重、不启动推理
# ——正好就是 LMM 语义下的"装引擎"。rc1/rc2 时代没有这个子命令，只能自己拼
# controller 的模块函数（image_prepare + gpu_probe）；现在有了官方入口就必须走
# 它：rc3 的镜像准备多了旧镜像锁迁移和按平台校验 RepoDigests 这些动作，自己拼
# 就会漏掉，而且包的 README 明确要求"调 engine 并读退出码，不要改 controller
# 内部常量或跳过校验"。
#
# 之后 download_model 跑 `megacube.sh download` 时，image_prepare() 会命中
# 这把锁直接返回，不会重复拉镜像。就算用户跳过这一步（或者锁被删了），
# download 也会自己补上，两边不会互相依赖出错。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

# LMM_PROBE_ONLY=1：普通用户身份的只读探针，LMM 每次下载前都会先跑一次。
# 命中就整段跳过安装，同时负责声明这次安装要不要管理员权限。
if [ "${LMM_PROBE_ONLY:-0}" = "1" ]; then
    command -v docker >/dev/null 2>&1 || { echo "ERROR: 未找到 docker，请先安装出厂 NVIDIA 容器运行环境" >&2; exit 1; }
    # 唯一真正需要 root 的事：NVIDIA Container Toolkit 还没注册成 docker
    # runtime。包里的 gpu_probe 要 `docker run --gpus all`，没注册就必然失败，
    # 而修它得改 /etc/docker 并重启 docker 服务。GB10 出厂环境通常已经配好，
    # 所以正常路径下这里不会打印，用户也就不用输密码。
    if ! docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q 'nvidia'; then
        echo "LMM_NEED_ROOT=1"
    fi
    # 镜像在不在、算不算"已就绪"，交给包自己判断：locked_image() 就是包里那个
    # 只读地核对"镜像锁 + 本机镜像都在、且跟本配方匹配"的函数，不拉取、不落盘。
    #
    # 不再自己解析 state/image.json 的 id 字段：rc1/rc2 的旧锁里也有 id，照旧
    # 解析会把"旧锁 + 旧镜像"判成已就绪从而跳过安装，随后 deploy 必定停在
    # "镜像锁属于旧版或其他配方；请先 stop，再运行 engine 迁移"——用户就卡死了。
    # 让包来判，旧锁会被判成未就绪，正好触发下面的 engine 迁移。
    if image="$(cd -- "$CARD_DIR" && python3 -c 'import controller; print(controller.locked_image())' 2>/dev/null)"; then
        echo "probe: 运行时镜像已就绪并已锁定（$image）"
        exit 0
    fi
    echo "probe: 运行时镜像尚未准备或镜像锁需要迁移" >&2
    exit 1
fi

# 被 pkexec 提权进来了：只做那一件必须 root 的事，然后立刻降权回原用户。
#
# 不能就这么以 root 一路走下去——包的 preflight() 明确
# `require(os.geteuid() != 0)`，而且 root 跑出来的 state/、cache/ 会变成 root
# 属主，后面普通用户身份的下载/启动全都会踩权限错误。
if [ "$(id -u)" = "0" ] && [ -z "${LMM_QWEN38FN_DROPPED:-}" ]; then
    TARGET_UID="${PKEXEC_UID:-}"
    [ -n "$TARGET_UID" ] || { echo "ERROR: 以 root 运行但拿不到 PKEXEC_UID，无法降权" >&2; exit 1; }
    TARGET_USER="$(getent passwd "$TARGET_UID" | cut -d: -f1)"
    TARGET_HOME="$(getent passwd "$TARGET_UID" | cut -d: -f6)"
    [ -n "$TARGET_USER" ] && [ -n "$TARGET_HOME" ] || { echo "ERROR: 无法解析目标用户" >&2; exit 1; }

    if ! docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q 'nvidia'; then
        if ! command -v nvidia-ctk >/dev/null 2>&1; then
            echo "ERROR: 未检测到 nvidia-ctk，请先安装 NVIDIA Container Toolkit（apt install nvidia-container-toolkit）后重试" >&2
            exit 1
        fi
        echo "检测到 NVIDIA Container Toolkit 未注册为 Docker runtime，正在自动配置…"
        nvidia-ctk runtime configure --runtime=docker
        systemctl restart docker
        # 重启后 daemon 要几秒才重新接受连接，直接往下跑会撞上 "Cannot connect"
        for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
    fi

    echo "系统级配置完成，降回普通用户 $TARGET_USER 继续安装引擎…"
    exec runuser -u "$TARGET_USER" -m -- \
        env HOME="$TARGET_HOME" LMM_QWEN38FN_DROPPED=1 bash "$0"
fi

. "$CARD_DIR/common.sh"

lmm_sync_config

# 只拉镜像、不碰权重。engine 内部顺带把 aarch64 / 128GB 内存 / docker 可用性
# 这些硬门槛先卡掉，免得用户白等一轮 133GB 下载才发现设备不对。
#
# 互斥交给包自己：megacube.sh 会 flock .operation.lock，我们这边再套一层就是
# 自己跟自己抢同一把锁，engine 必然报"另一个部署/启停操作正在执行"。
./megacube.sh engine

# 把引擎身份报给 LMM：rc3 起是固定摘要引用（vllm/vllm-openai@sha256:…），
# docker image inspect / docker rmi 都认这个形式，LMM 的"已安装引擎"按它算
# 体积、按它删除。直接读包里的常量，不在这边写第二份，免得包换摘要时对不上。
echo "LMM_ENGINE_IMAGE=$(python3 -c 'import controller; print(controller.IMAGE)')"
