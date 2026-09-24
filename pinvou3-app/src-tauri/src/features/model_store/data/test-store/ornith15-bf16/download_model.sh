#!/bin/bash
set -euo pipefail
VENDOR_DIR="$HOME/.local/share/local-model-manager/vendor/ornith15-bf16"
[ -d "$VENDOR_DIR" ] || { echo "ERROR: 未找到部署包，请先完成引擎安装" >&2; exit 1; }
cd "$VENDOR_DIR"

[ -f .env ] || cp .env.example .env

# 权重统一放到用户可见的 ~/models/<HF 仓库名> 下：跟部署包默认的复用检测目录
# （common.sh 里的 reuse_local_model_if_present）是同一个约定，用户自己手动
# 下载放过去的模型和 app 下载的模型天然互通；缓存仍放 LMM 数据目录。
MODELS_DIR="${LMM_MODELS_DIR:-$HOME/models}/Ornith-1.5-35B-A3B"
CACHE_DIR="$VENDOR_DIR/cache"
mkdir -p "$MODELS_DIR" "$CACHE_DIR"
sed -i \
    -e "s#^MODEL_PATH=.*#MODEL_PATH=$MODELS_DIR#" \
    -e "s#^CACHE_PATH=.*#CACHE_PATH=$CACHE_DIR#" \
    .env

if [ -n "${LMM_HF_TOKEN:-}" ]; then
    export HF_TOKEN="$LMM_HF_TOKEN"
fi
if [ -n "${LMM_HF_ENDPOINT:-}" ]; then
    export HF_ENDPOINT="$LMM_HF_ENDPOINT"
fi
# 跟 Qwen38 同一个坑：新版 huggingface_hub 会给大文件自动启用不走镜像的
# "Xet" 分块存储通道，未授权访问报 401 或者干脆连不上超时，导致大文件下到
# 一半必然失败。这次直接一开始就禁用，不用等它在这张卡片上再踩一遍。
export HF_HUB_DISABLE_XET=1

# 标准单一分片索引结构（跟 Qwen38 一样），完整性校验同样的思路：只看
# config.json 存不存在不够，要把 model.safetensors.index.json 里列出的每个
# 分片文件都核对一遍。
_ornith15_model_complete() {
    local dir="$1"
    [ -f "$dir/config.json" ] || return 1
    [ -f "$dir/model.safetensors.index.json" ] || return 1
    python3 -c "
import json, os, sys
try:
    idx = json.load(open('$dir/model.safetensors.index.json'))
except Exception:
    sys.exit(1)
for f in set(idx.get('weight_map', {}).values()):
    if not os.path.isfile(os.path.join('$dir', f)):
        sys.exit(1)
sys.exit(0)
" 2>/dev/null
}

# 先看看用户是不是已经手动把模型权重放到常见暂存目录/U 盘里了，找到就直接挪过
# 来，不用再走一遍网络下载。
if ! _ornith15_model_complete "$MODELS_DIR"; then
    FOUND_MODEL_DIR=""
    SEARCH_START=$SECONDS
    for dir in \
        "$HOME/LMM/model" "$HOME/LMM" "$HOME/lmm-deploy" "$HOME/Downloads" \
        "$HOME/桌面" "$HOME/Desktop" \
        /media/*/* /media/* /mnt/* /run/media/*/*; do
        [ -d "$dir" ] || continue
        if [ $((SECONDS - SEARCH_START)) -ge 15 ]; then
            echo "本地查找模型文件超过 15 秒，不再继续找，改走网络下载" >&2
            break
        fi
        candidate="$(timeout 3 find "$dir" -maxdepth 4 -type f -name "model.safetensors.index.json" 2>/dev/null | head -1)" || true
        if [ -n "$candidate" ]; then
            candidate_dir="$(dirname "$candidate")"
            _ornith15_model_complete "$candidate_dir" || continue
            FOUND_MODEL_DIR="$candidate_dir"
            break
        fi
    done
    if [ -n "$FOUND_MODEL_DIR" ]; then
        echo "本地未部署过模型，在 $FOUND_MODEL_DIR 找到已经放好的模型文件，正在转移到 $MODELS_DIR…"
        case "$FOUND_MODEL_DIR" in
            /media/*|/mnt/*|/run/media/*)
                cp -r "$FOUND_MODEL_DIR"/. "$MODELS_DIR"/
                ;;
            *)
                mv "$FOUND_MODEL_DIR"/* "$MODELS_DIR"/
                ;;
        esac
    fi
fi

# 已下载字节数 = 落地成品 + 还在传的临时块。
#
# 不能直接 du 整个目录：huggingface_hub 把在传的数据放在
# <目录>/.cache/huggingface/download/ 下，传完再移到最终路径。同一个文件如果
# 既有成品、又残留了一个没人认领的临时块（并发下载被中断时会这样），du 就会
# 把它算两遍——真机上出现过「51.0GB / 30.6GB，进度 100% 还在跑」的画面。
#
# 所以分开算：成品只统计 .cache 之外的文件，在传的只统计 *.incomplete，
# 最后再夹到总量以内，无论如何不让进度条冲过 100%。
_lmm_downloaded_bytes() {
    local dir="$1" total="$2" done_bytes=0 partial=0 sum=0
    done_bytes="$(find "$dir" -type f -not -path "*/.cache/*" -printf '%s
' 2>/dev/null         | awk '{s+=$1} END{printf "%d", s+0}')"
    partial="$(find "$dir" -name '*.incomplete' -printf '%s
' 2>/dev/null         | awk '{s+=$1} END{printf "%d", s+0}')"
    sum=$((done_bytes + partial))
    if [ "$total" -gt 0 ] && [ "$sum" -gt "$total" ]; then
        sum="$total"
    fi
    printf '%d' "$sum"
}

# 下载成功返回之后，还剩下的 *.incomplete 按定义就是垃圾——所有文件都已经下完
# 并移到最终路径了，没有任何东西还需要续传。这些残块是被中断的下载留下的，不清
# 掉会一直白占磁盘（真机上一次就攒了 23GB）。
_lmm_cleanup_stale_partials() {
    local dir="$1"
    find "$dir" -name '*.incomplete' -delete 2>/dev/null || true
}

# 后台跑实际下载，前台轮询目标目录实际占用字节数，用
# LMM_PROGRESS_BYTES=已下载/总量 这个约定告诉界面真实进度——总量用商店卡片
# 元数据里的真实体积（LMM_MODEL_SIZE_BYTES），不是本地临时估算的。
_ornith15_run_download_with_progress() {
    local total="${LMM_MODEL_SIZE_BYTES:-0}"
    ./scripts/download-model.sh &
    local dl_pid=$!
    while kill -0 "$dl_pid" 2>/dev/null; do
        echo "LMM_PROGRESS_BYTES=$(_lmm_downloaded_bytes "$MODELS_DIR" "$total")/$total"
        sleep 2
    done
    wait "$dl_pid"
    _lmm_cleanup_stale_partials "$MODELS_DIR"
    echo "LMM_PROGRESS_BYTES=$(_lmm_downloaded_bytes "$MODELS_DIR" "$total")/$total"
}

if ! _ornith15_model_complete "$MODELS_DIR"; then
    if ! _ornith15_run_download_with_progress; then
        # 跟 Z-Image/Qwen38 同一个坑：第一次失败后，把这次下载生成的 hf 工具
        # venv 里的 hf_xet 包直接卸载掉（不只是设环境变量），重跑一遍。
        tool_venv="$VENDOR_DIR/.tools/huggingface"
        if [ -x "$tool_venv/bin/pip" ]; then
            echo "下载失败，尝试卸载 hf_xet 后重试（很可能是 Xet 相关基础设施连不上）…"
            "$tool_venv/bin/pip" uninstall -y hf_xet >/dev/null 2>&1 || true
            _ornith15_run_download_with_progress
        else
            exit 1
        fi
    fi
else
    echo "模型已就绪：$MODELS_DIR（本地找到，跳过下载）"
fi
