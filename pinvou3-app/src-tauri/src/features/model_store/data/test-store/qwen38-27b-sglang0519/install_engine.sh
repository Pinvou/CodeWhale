#!/bin/bash
# 装引擎 = 把官方 SGLang 镜像拉到本机 + 验一次容器能不能看见 GPU。不下权重。
#
# 刻意不调 vendor/deploy.sh：那个脚本会一路做到下载 23GB 权重为止（原因见
# common.sh 开头）。这一步只做「拉镜像」，权重交给 download_model.sh。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

# 只读探针：LMM 每次下载前先以普通用户身份跑一次，命中就整段跳过安装。
#
# 不打 LMM_NEED_ROOT：设备出厂就装好了 Docker 和 NVIDIA 容器运行时，登录用户也
# 已经在 docker 组里（真机实测 docker info 免 sudo 可用、runtime 列表里有 nvidia），
# 拉镜像不需要任何提权。真碰上环境不全，用 pkexec 也修不好（usermod 要重新登录
# 才生效），不如直接把话说清楚让用户处理。
#
# 顺带：用户如果是拿 U 盘 docker load 把镜像导进来的，这里同样命中，装引擎整步跳过。
if [ "${LMM_PROBE_ONLY:-0}" = "1" ]; then
    command -v docker >/dev/null 2>&1 || {
        echo "ERROR: 没找到 docker。这张卡需要设备出厂自带的 NVIDIA 容器运行环境" >&2
        exit 1
    }
    if docker image inspect "$IMAGE" >/dev/null 2>&1; then
        echo "probe: 引擎镜像已就绪（$IMAGE）"
        exit 0
    fi
    echo "probe: 引擎镜像尚未准备（$IMAGE）" >&2
    exit 1
fi

lmm_init_docker

echo "检查硬件…"
command -v nvidia-smi >/dev/null 2>&1 || {
    echo "ERROR: 没找到 nvidia-smi。这张卡需要 NVIDIA 驱动 580 及以上（CUDA 13）" >&2
    exit 1
}
nvidia-smi -L || {
    echo "ERROR: nvidia-smi 执行失败，请检查 NVIDIA 驱动是否正常" >&2
    exit 1
}
[ "$(uname -m)" = "aarch64" ] || echo "注意：这套方案是按 GB10（aarch64）调的，当前是 $(uname -m)，继续执行"

# ---------------------------------------------------------------------------
# 拉镜像（约 15GB 压缩体积），逐个镜像站试，第一个通的算数
# ---------------------------------------------------------------------------
if lmm_docker image inspect "$IMAGE" >/dev/null 2>&1; then
    echo "引擎镜像已在本机，跳过拉取：$IMAGE"
else
    echo "拉取引擎镜像 $IMAGE（约 15GB，视网络可能要十几分钟）…"
    pulled=""
    for prefix in $MIRROR_LIST; do
        if [ "$prefix" = "DIRECT" ]; then
            ref="$IMAGE"
            echo "尝试直连 Docker Hub…"
        else
            ref="$prefix/$IMAGE"
            echo "尝试镜像站 $prefix…"
        fi
        if lmm_docker pull "$ref"; then
            pulled="$ref"
            break
        fi
        echo "这个源没拉通，换下一个" >&2
    done
    [ -n "$pulled" ] || {
        echo "ERROR: 所有镜像源都没能拉下 $IMAGE" >&2
        echo "       请检查设备的网络（能不能上外网／镜像站是否可达）后重试；" >&2
        echo "       也可以在别的机器上拉好后用 docker save/load 拷进来，本步骤会自动跳过。" >&2
        exit 1
    }
    # 从镜像站拉的带着镜像站前缀，改回官方名字，后面的 vendor 脚本认的是官方名
    if [ "$pulled" != "$IMAGE" ]; then
        lmm_docker tag "$pulled" "$IMAGE"
        lmm_docker rmi "$pulled" >/dev/null 2>&1 || true
    fi
    echo "镜像就绪：$IMAGE"
fi

# ---------------------------------------------------------------------------
# 验 GPU 直通。容器看不见 GPU 的话，后面启用那一步会以很难懂的方式失败，
# 在这里就拦下来。
# ---------------------------------------------------------------------------
echo "验证容器能否访问 GPU…"
gpu_ok=""
for candidate in "--gpus all" "--device nvidia.com/gpu=all"; do
    # shellcheck disable=SC2086
    if lmm_docker run --rm $candidate "$IMAGE" nvidia-smi -L >/dev/null 2>&1; then
        gpu_ok="$candidate"
        echo "GPU 直通可用（$candidate）"
        break
    fi
done
[ -n "$gpu_ok" ] || {
    echo "ERROR: 容器里看不到 GPU。" >&2
    echo "       多半是 NVIDIA Container Toolkit 没注册进 Docker。可以让管理员执行：" >&2
    echo "         sudo nvidia-ctk runtime configure --runtime=docker && sudo systemctl restart docker" >&2
    exit 1
}

echo "LMM_ENGINE_IMAGE=$IMAGE"
echo "引擎准备完成。接下来可以下载权重。"
