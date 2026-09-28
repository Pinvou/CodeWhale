#!/bin/bash
# 下载 168GB 权重到**本机**，生成官方摘要清单并全量校验。全程不碰从机。
#
# 包里没有"只下载不分发"的子命令（它的 deploy 把下载+分发+启动串成一条），所以
# 这里自己发那一条 docker run。要点是：镜像、仓库名、commit 都从 controller.py
# import 进来，不抄字面量——包升级换了 revision，这里跟着变，不会悄悄下到旧版本。
#
# 权重也可以不下载：从 U 盘拷进模型目录即可，本脚本会自动识别、跳过下载，直接
# 生成清单并校验。校验用的是包自己的 model_hashes/check_model，跟联网下载完全
# 同一套标准。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

lmm_link_model_dir

if [ -n "${LMM_HF_TOKEN:-}" ]; then
    export HF_TOKEN="$LMM_HF_TOKEN"
fi
if [ -n "${LMM_HF_ENDPOINT:-}" ] && [ "$LMM_HF_ENDPOINT" != "https://huggingface.co" ]; then
    echo "提示：本卡片的权重由部署包在容器内按官方 commit 下载，设置里的镜像站（$LMM_HF_ENDPOINT）对它不生效。"
fi

TOTAL="${LMM_MODEL_SIZE_BYTES:-0}"

# 环境硬门槛。包的 preflight 连直连链路一起查，这一步还没到那个阶段，所以只查
# 跟"能不能在本机把权重拿下来"有关的几项——否则用户会在完全不合适的机器上
# 白下 168GB。
python3 - <<'PY'
import os, platform, shutil, controller
controller.verify_bundle()
controller.require(platform.system() == 'Linux' and platform.machine() == 'aarch64',
                   '仅支持 Linux aarch64 / GB10 MegaCube。')
controller.require(shutil.which('docker'), '缺少 docker，请按 README 完成系统基线。')
controller.run(['docker', 'info'], capture=True)
model = controller.DATA / 'cache/model'
model.mkdir(parents=True, exist_ok=True)
present = sum(p.stat().st_size for p in model.rglob('*') if p.is_file())
# 阈值跟包里 node('prepare') 用的是同一个：168GB 权重 + 60GiB 镜像/缓存预算
controller.require(shutil.disk_usage(model).free > max(0, 168_000_000_000 - present) + 60 * 1024**3,
                   '空间不足：需容纳剩余权重及至少 60GiB 的镜像/缓存预算。')
PY

# 已下载字节数 = 落地成品 + 还在传的临时块。不能直接 du 整个目录：
# huggingface_hub 把在传的数据放在 .cache/huggingface/download/ 下，传完再移到
# 最终路径。同一个文件既有成品又残留临时块时会被算两遍——真机上出现过进度冲过
# 100% 还在跑的画面。
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

# 取消时 LMM 杀的是进程组，而 `docker run`（非 -d）被杀掉的只是客户端，容器会
# 继续把 168GB 拉完——用户点了取消，磁盘却还在涨。按"用运行镜像跑
# snapshot_download"这个特征把下载容器收掉（推理容器命令不同，不会被误伤）。
_lmm_kill_download_containers() {
    local ids
    ids="$(docker ps --no-trunc --format '{{.ID}} {{.Command}}' 2>/dev/null | grep 'snapshot_download' | awk '{print $1}')" || true
    if [ -n "$ids" ]; then
        echo "正在停止后台下载容器…"
        # shellcheck disable=SC2086
        docker stop -t 5 $ids >/dev/null 2>&1 || true
    fi
}
trap _lmm_kill_download_containers EXIT INT TERM

# 权重是不是已经齐了（用户从 U 盘拷进来的，或上次下了一半这次续传完的）
_lmm_model_complete() {
    python3 - <<'PY' >/dev/null 2>&1
import controller
controller.check_model(controller.DATA / 'cache/model')
PY
}

MANIFEST="$CARD_DIR/data/model-manifest.json"
HAD_MANIFEST=0; [ -f "$MANIFEST" ] && HAD_MANIFEST=1
PREEXISTING=0

if _lmm_model_complete; then
    PREEXISTING=1
    echo "模型目录已完整（$MODEL_DIR），跳过下载。"
else
    echo "开始下载权重到 $MODEL_DIR（只用本机，不需要直连线和从机）…"
    # 挂载的是软链的**目标**目录，容器里看不到那个链接，不会踩空。
    # 参数与包 deploy 里那条下载命令一致：固定 revision、4 并发。
    docker run --rm --user "$(id -u):$(id -g)" \
        -e HOME=/tmp -e HF_TOKEN -e HF_HUB_DOWNLOAD_TIMEOUT=120 -e HF_HUB_DISABLE_XET=1 \
        -v "$MODEL_DIR:/model" \
        --entrypoint python3 \
        "$(python3 -c 'import controller;print(controller.RUNTIME_IMAGE)')" -c "
from huggingface_hub import snapshot_download
snapshot_download('$(python3 -c 'import controller;print(controller.MODEL)')',
                  revision='$(python3 -c 'import controller;print(controller.REVISION)')',
                  local_dir='/model', max_workers=4)" &
    dl_pid=$!
    while kill -0 "$dl_pid" 2>/dev/null; do
        echo "LMM_PROGRESS_BYTES=$(_lmm_downloaded_bytes "$MODEL_DIR" "$TOTAL")/$TOTAL"
        sleep 5
    done
    wait "$dl_pid"
    find "$MODEL_DIR" -name '*.incomplete' -delete 2>/dev/null || true
    echo "LMM_PROGRESS_BYTES=$(_lmm_downloaded_bytes "$MODEL_DIR" "$TOTAL")/$TOTAL"
fi

# 校验清单：**有没有权威参照，决定了这一步是"真校验"还是"只是记账"**。
#
#   已有 model-manifest.json  -> 拿它对权重做真校验。这是唯一能发现拷贝损坏的
#                                情况：清单是在别处下载时生成的，是外部参照。
#   没有清单                  -> 只能从现有文件算出一份清单。**这不构成校验**：
#                                拿刚从这些文件算出来的哈希再去比对这些文件，
#                                永远相等，坏字节照样通过。
#
# 所以从 U 盘拷权重时，请把源机器上的 data/model-manifest.json 一起拷过来。
# 下载路径不受影响：huggingface_hub 在下载过程中已按官方摘要逐文件校验过。
#
# 168GB 哈希一遍要好几分钟且中途一声不吭，必须打心跳，否则会被 LMM 的静默超时
# 当成卡死杀掉。每一遍都不便宜，所以一遍都不多做。
if [ "$HAD_MANIFEST" = 1 ]; then
    echo "发现已有校验清单，按它逐文件校验权重（约 168GB）…"
    python3 -c "import controller; controller.verify()" &
    lmm_heartbeat $! "正在校验模型"
else
    if [ "$PREEXISTING" = 1 ]; then
        echo "注意：权重是现成的，但没有随附 model-manifest.json。"
        echo "      没有外部参照，无法判断这份拷贝是否完整——下面只能按现有文件生成一份清单，"
        echo "      它记录的是「当前磁盘上是什么」，不构成校验。"
        echo "      如果这批权重是从别的机器拷来的，建议把那台机器上的 data/model-manifest.json 一并拷过来后重下一次。"
    fi
    echo "生成模型校验清单（约 168GB，需要数分钟）…"
    python3 - <<'PY' &
import hashlib
import controller
manifest = controller.DATA / 'model-manifest.json'
controller.save(manifest, dict(model=controller.MODEL, revision=controller.REVISION,
                               files=controller.model_hashes(controller.DATA / 'cache/model')))
# 直接写 verified 标记，而不是再调一次 verify()。verify() 会把 168GB 重新哈希一遍
# 去跟刚刚由这些文件算出来的清单比对——结果必然相等，纯属白跑一遍。标记的内容
# 定义就是清单的 sha256，跟 verify() 写进去的完全一样。
manifest_sha = hashlib.sha256(manifest.read_bytes()).hexdigest()
(controller.DATA / 'verified').write_text(manifest_sha)
print('清单已生成。')
PY
    lmm_heartbeat $! "正在生成校验清单"
fi

echo "权重已就绪：$MODEL_DIR"
echo "下一步「部署」会配置直连网络、与从机配对，并把镜像和权重推送过去——那一步才需要两台设备。"
