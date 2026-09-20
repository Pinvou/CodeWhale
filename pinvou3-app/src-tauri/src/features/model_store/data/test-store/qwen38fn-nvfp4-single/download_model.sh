#!/bin/bash
# 下载权重（约 123.57GiB / 132.7GB），并做官方摘要的全量校验。
#
# 直接用包的 `megacube.sh download`：它内部是
#   preflight → ensure_stopped → 磁盘余量检查 → image_prepare（已被
#   install_engine 锁好，这里秒过）→ gpu_probe → 在运行时镜像里跑
#   model_assets.py download（snapshot_download + 逐文件 sha256 校验）
# 全程可断点续传，所以 LMM 的"超时重试"直接重跑这个脚本就行。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

lmm_sync_config

if [ -n "${LMM_HF_TOKEN:-}" ]; then
    export HF_TOKEN="$LMM_HF_TOKEN"
fi
# 说明：这张卡片的下载是在包锁定的运行时镜像里跑的，包只把 HF_TOKEN 透传进
# 容器，没有透传 HF_ENDPOINT。也就是说设置里的镜像站对本卡片不生效，权重固定
# 从官方 Hub 拉。这是交付包刻意的选择（它要按官方 commit SHA 锁定 revision），
# 我们不去改包，改了 SHA256SUMS 自校验就过不了。
if [ -n "${LMM_HF_ENDPOINT:-}" ] && [ "$LMM_HF_ENDPOINT" != "https://huggingface.co" ]; then
    echo "提示：本卡片的权重由部署包在容器内按官方 commit SHA 下载，设置里的镜像站（$LMM_HF_ENDPOINT）对它不生效。"
fi

# 已下载字节数 = 落地成品 + 还在传的临时块。
#
# 不能直接 du 整个目录：huggingface_hub 把在传的数据放在
# <目录>/.cache/huggingface/download/ 下，传完再移到最终路径。同一个文件如果
# 既有成品、又残留了一个没人认领的临时块（下载被中断时会这样），du 就会把它
# 算两遍——真机上出现过"51.0GB / 30.6GB，进度 100% 还在跑"的画面。
_lmm_downloaded_bytes() {
    local dir="$1" total="$2" done_bytes=0 partial=0 sum=0
    done_bytes="$(find "$dir" -type f -not -path "*/.cache/*" -printf '%s\n' 2>/dev/null | awk '{s+=$1} END{printf "%d", s+0}')"
    partial="$(find "$dir" -name '*.incomplete' -printf '%s\n' 2>/dev/null | awk '{s+=$1} END{printf "%d", s+0}')"
    sum=$((done_bytes + partial))
    if [ "$total" -gt 0 ] && [ "$sum" -gt "$total" ]; then
        sum="$total"
    fi
    printf '%d' "$sum"
}

# 下载成功返回之后，还剩下的 *.incomplete 按定义就是垃圾——所有文件都已经下完
# 并移到最终路径了。这些残块是被中断的下载留下的，不清掉会一直白占磁盘。
_lmm_cleanup_stale_partials() {
    find "$1" -name '*.incomplete' -delete 2>/dev/null || true
}

# 取消下载时 LMM 杀的是脚本所在的进程组。`docker run`（非 -d）被杀掉的只是
# 客户端，容器会继续在后台把 133GB 拉完——用户点了取消，磁盘却还在涨。
# 所以退出时主动收尾：按"用锁定镜像跑 model_assets.py"这个特征找出下载容器
# 并停掉（推理服务容器用的是同一个镜像但命令不同，不会被误伤）。
_lmm_kill_download_containers() {
    local ids
    ids="$(docker ps --no-trunc --format '{{.ID}} {{.Command}}' 2>/dev/null | grep 'model_assets.py' | awk '{print $1}')" || true
    if [ -n "$ids" ]; then
        echo "正在停止后台下载容器…"
        # shellcheck disable=SC2086
        docker stop -t 5 $ids >/dev/null 2>&1 || true
    fi
}
trap _lmm_kill_download_containers EXIT INT TERM

TOTAL="${LMM_MODEL_SIZE_BYTES:-0}"

# 互斥交给包自己：megacube.sh 内部会 flock .operation.lock，别人在跑时它会
# 直接报错退出，我们不用再套一层（套了反而会跟它自己抢同一把锁）。
#
# 后台跑真正的下载，前台按 LMM_PROGRESS_BYTES=已下载/总量 的约定汇报进度。
./megacube.sh download &
dl_pid=$!
while kill -0 "$dl_pid" 2>/dev/null; do
    echo "LMM_PROGRESS_BYTES=$(_lmm_downloaded_bytes "$MODEL_DIR" "$TOTAL")/$TOTAL"
    sleep 5
done
wait "$dl_pid"

_lmm_cleanup_stale_partials "$MODEL_DIR"
echo "LMM_PROGRESS_BYTES=$(_lmm_downloaded_bytes "$MODEL_DIR" "$TOTAL")/$TOTAL"
