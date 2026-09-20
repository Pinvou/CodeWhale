#!/bin/bash
# 下载权重（约 23.4GB）到 $LMM_MODELS_DIR/Qwen3.8-27B-NVFP4-unsloth。
#
# 断点续传、进度上报、完整性校验都在这里做——vendor/deploy.sh 里那段下载是一次性
# 的（下不完就整个失败重来，也没有进度可看），不适合 LMM 的下载步骤，所以这一步
# 自己实现，vendor/deploy.sh 到了「部署」那步会发现权重已完整而直接跳过下载。
set -euo pipefail
CARD_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
. "$CARD_DIR/common.sh"

REPO_ID="${LMM_SOURCE_REPO_ID:-unsloth/Qwen3.8-27B-NVFP4}"
TOTAL="${LMM_MODEL_SIZE_BYTES:-23444511852}"
VENV_DIR="$CARD_DIR/.venv"

mkdir -p "$MODEL_DIR"

# 逐个文件按官方 sha256 校验（规范 3.3）。清单 weights.sha256 里只有三个文件，
# 是因为 HuggingFace 只对走 LFS 的文件公布 sha256——正好就是这三个（22.6GB 的
# model.safetensors、849MB 的 model_mtp.safetensors、20MB 的 tokenizer.json），
# 合计 23,437,581,813 字节，占全部权重的 99.97%。剩下十个是几 KB 的配置文件，
# 官方只给 git blob 的 sha1，没有可比对的 sha256，由 lmm_model_complete 查存在。
#
# 算完在权重目录里留个记号，记的是清单自己的哈希：清单变了（换了模型版本）记号
# 自然失效会重新校验；没变就不必每点一次下载都再啃一遍 23GB。
verify_weights() {
    local dir="$1" want_mark
    [ -f "$LMM_WEIGHTS_MANIFEST" ] || {
        echo "ERROR: 卡片包不完整，缺少权重清单 weights.sha256" >&2
        return 1
    }
    want_mark="$(sha256sum "$LMM_WEIGHTS_MANIFEST" | cut -d' ' -f1)"
    if [ -f "$LMM_VERIFIED_MARK" ] && [ "$(cat "$LMM_VERIFIED_MARK" 2>/dev/null)" = "$want_mark" ]; then
        return 0
    fi
    echo "校验权重摘要（约 23.4GB，需要几分钟）…"
    ( cd "$dir" && sha256sum -c "$LMM_WEIGHTS_MANIFEST" ) &
    lmm_heartbeat $! "正在校验权重摘要" || return 1
    printf '%s' "$want_mark" > "$LMM_VERIFIED_MARK"
    echo "权重摘要校验通过。"
}

if lmm_model_complete "$MODEL_DIR"; then
    verify_weights "$MODEL_DIR" || {
        echo "ERROR: 本机已有的权重没通过摘要校验，文件可能损坏了。" >&2
        echo "       请删掉 $MODEL_DIR 后重新下载。" >&2
        exit 1
    }
    echo "权重已就绪，跳过下载：$MODEL_DIR"
    echo "LMM_PROGRESS_BYTES=$TOTAL/$TOTAL"
    exit 0
fi

# ---------------------------------------------------------------------------
# 用户可能已经把权重拷到 U 盘或某个暂存目录里了，先找一圈，找到就不走网络。
# 单个目录和总耗时都设上限：某个挂载点很慢（网络共享）时不能把整个下载拖住。
# ---------------------------------------------------------------------------
found=""
search_start=$SECONDS
for dir in \
    "$HOME/LMM/model" "$HOME/LMM" "$HOME/lmm-deploy" "$HOME/Downloads" \
    "$HOME/桌面" "$HOME/Desktop" \
    /media/*/* /media/* /mnt/* /run/media/*/*; do
    [ -d "$dir" ] || continue
    if [ $((SECONDS - search_start)) -ge 15 ]; then
        echo "本地查找超过 15 秒，不再继续找，改走网络下载"
        break
    fi
    while IFS= read -r candidate; do
        [ -n "$candidate" ] || continue
        candidate_dir="$(dirname "$candidate")"
        if lmm_model_complete "$candidate_dir"; then
            found="$candidate_dir"
            break
        fi
    done < <(timeout 3 find "$dir" -maxdepth 4 -type f -name "model_mtp.safetensors" 2>/dev/null)
    [ -n "$found" ] && break
done

if [ -n "$found" ]; then
    echo "在 $found 找到一份完整的权重，正在转移到 $MODEL_DIR …"
    case "$found" in
        /media/*|/mnt/*|/run/media/*)
            # 移动介质：原件留着，用户可能还要在别的设备上用
            cp -r "$found"/. "$MODEL_DIR"/ &
            ;;
        *)
            # 本机磁盘：直接挪，不留一份重复占 23GB 的副本
            mv "$found"/* "$MODEL_DIR"/ &
            ;;
    esac
    lmm_heartbeat $! "正在转移权重"
    lmm_model_complete "$MODEL_DIR" || {
        echo "ERROR: 转移完成后权重仍不完整，请检查 $MODEL_DIR" >&2
        exit 1
    }
    # 从 U 盘/暂存目录拷进来的东西来路不明，摘要必须真算一遍
    verify_weights "$MODEL_DIR" || {
        echo "ERROR: 转移过来的权重没通过摘要校验，文件可能不完整或不是这个版本。" >&2
        echo "       请删掉 $MODEL_DIR 后改走网络下载。" >&2
        exit 1
    }
    echo "LMM_PROGRESS_BYTES=$TOTAL/$TOTAL"
    echo "权重就绪：$MODEL_DIR"
    exit 0
fi

# ---------------------------------------------------------------------------
# 下载工具：设备上没有 hf / modelscope（真机实测），装进卡片目录下的 venv，
# 不污染系统 python。
# ---------------------------------------------------------------------------
hf_bin() {
    if [ -x "$VENV_DIR/bin/hf" ]; then echo "$VENV_DIR/bin/hf"
    elif [ -x "$VENV_DIR/bin/huggingface-cli" ]; then echo "$VENV_DIR/bin/huggingface-cli"
    else echo ""
    fi
}

if [ -z "$(hf_bin)" ]; then
    echo "准备下载工具（首次需要几分钟，pip 源：$PIP_MIRROR）…"
    if [ ! -x "$VENV_DIR/bin/pip" ]; then
        python3 -m venv "$VENV_DIR" || {
            echo "ERROR: 创建 python 虚拟环境失败。请让管理员安装 python3-venv：" >&2
            echo "         sudo apt-get install -y python3-venv" >&2
            exit 1
        }
    fi
    "$VENV_DIR/bin/pip" install --upgrade "huggingface_hub[cli]" -i "$PIP_MIRROR" &
    lmm_heartbeat $! "正在安装下载工具"
    [ -n "$(hf_bin)" ] || {
        echo "ERROR: 下载工具装好了却找不到 hf 命令，请检查 $VENV_DIR/bin" >&2
        exit 1
    }
fi
HF="$(hf_bin)"

export HF_ENDPOINT="${LMM_HF_ENDPOINT:-https://hf-mirror.com}"
[ -n "${LMM_HF_TOKEN:-}" ] && export HF_TOKEN="$LMM_HF_TOKEN"
# 新版 huggingface_hub 会自动给大文件启用 Xet 分块通道，请求直接打到
# cas-server.xethub.hf.co——那条通道不走 HF_ENDPOINT 镜像，未授权会 401，实测
# 大文件下到八成多必然失败。关掉后退回标准 resolve 接口，走的就是上面的镜像。
export HF_HUB_DISABLE_XET=1

# 已下载字节 = 落地成品 + 还在传的临时块，并夹到总量以内。
#
# 不能直接 du 整个目录：huggingface_hub 把在传数据放在 .cache/huggingface/download/
# 下，传完再移到最终路径。同一个文件既有成品又残留临时块时（并发下载被中断就会
# 这样）du 会算两遍——真机上出现过「51.0GB / 30.6GB，进度 100% 还在跑」。
lmm_downloaded_bytes() {
    local done_bytes partial sum
    done_bytes="$(find "$MODEL_DIR" -type f -not -path "*/.cache/*" -printf '%s\n' 2>/dev/null | awk '{s+=$1} END{printf "%d", s+0}')"
    partial="$(find "$MODEL_DIR" -name '*.incomplete' -printf '%s\n' 2>/dev/null | awk '{s+=$1} END{printf "%d", s+0}')"
    sum=$((done_bytes + partial))
    if [ "$TOTAL" -gt 0 ] && [ "$sum" -gt "$TOTAL" ]; then sum="$TOTAL"; fi
    printf '%d' "$sum"
}

# 后台跑下载，前台按 LMM 约定的标记行报进度。总量用商店卡片元数据里的真实体积，
# 不自己估算。
run_with_progress() {
    "$@" &
    local pid=$!
    while kill -0 "$pid" 2>/dev/null; do
        echo "LMM_PROGRESS_BYTES=$(lmm_downloaded_bytes)/$TOTAL"
        sleep 2
    done
    wait "$pid"
}

echo "开始下载 $REPO_ID（约 23.4GB）→ $MODEL_DIR"
echo "镜像地址：$HF_ENDPOINT"
# --max-workers 4 是实测出来的：8 并发在断线时会丢弃在途分片导致进度倒退，
# 1 并发只有 4 并发四分之一的速度。
# hf 自己支持续传和跳过已完整的文件，对着同一个 --local-dir 重跑即可，不用先清空。
ok=0
if run_with_progress "$HF" download "$REPO_ID" --local-dir "$MODEL_DIR" --max-workers 4; then
    ok=1
else
    echo "从 $HF_ENDPOINT 下载失败，改用 ModelScope 重试（已下好的部分会保留）…" >&2
    if [ ! -x "$VENV_DIR/bin/modelscope" ]; then
        "$VENV_DIR/bin/pip" install modelscope -i "$PIP_MIRROR" &
        lmm_heartbeat $! "正在安装 ModelScope 下载工具"
    fi
    if [ -x "$VENV_DIR/bin/modelscope" ] &&
        run_with_progress "$VENV_DIR/bin/modelscope" download --model "$REPO_ID" --local_dir "$MODEL_DIR"; then
        ok=1
    fi
fi

if [ "$ok" != "1" ]; then
    echo "ERROR: 权重没能下完。" >&2
    echo "       已经下好的部分保留在 $MODEL_DIR，再点一次下载会从断点接着传。" >&2
    echo "       如果反复失败，请检查设备网络，或在设置里换一个 HF 镜像地址。" >&2
    exit 1
fi

# 下载成功返回后还剩的 *.incomplete 按定义就是垃圾（所有文件都已下完并移走），
# 不清掉会一直白占磁盘——真机上一次攒了 23GB。
find "$MODEL_DIR" -name '*.incomplete' -delete 2>/dev/null || true

lmm_model_complete "$MODEL_DIR" || {
    echo "ERROR: 下载结束了，但权重不完整（文件缺失或大小不对）。" >&2
    echo "       目录：$MODEL_DIR。再点一次下载会补齐缺的部分。" >&2
    exit 1
}

# 大小对了不等于内容对，逐个文件比官方 sha256。校验不过绝不静默放行——带着一份
# 坏权重往下走，报出来的会是启用那一步某个看不懂的 CUDA 错误。
verify_weights "$MODEL_DIR" || {
    echo "ERROR: 权重摘要校验没通过，下载到的内容跟官方对不上。" >&2
    echo "       请删掉 $MODEL_DIR 后重新下载；反复出现的话多半是镜像源上的副本有问题，" >&2
    echo "       可以在设置里换一个 HF 镜像地址再试。" >&2
    exit 1
}

echo "LMM_PROGRESS_BYTES=$TOTAL/$TOTAL"
echo "权重下载完成并校验通过：$MODEL_DIR"
