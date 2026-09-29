#!/usr/bin/env bash
set -euo pipefail
PKG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODEL_KEY="${LMM_MODEL_NAME:-minimax-h3-int8-single}"
MODELS_ROOT="${LMM_MODELS_DIR:-$HOME/.local/share/lmm/models}"
MODEL_DIR="$MODELS_ROOT/$MODEL_KEY"
ENGINE_DIR="${LMM_ENGINE_DIR:-$HOME/.local/share/lmm/engines/comfyui-minimax-h3-0.35.0-cu130}"
TOTAL="${LMM_MODEL_SIZE_BYTES:-43317497012}"
SOURCE_KIND="${LMM_SOURCE_KIND:-hf_endpoint}"
SOURCE_BASE="${LMM_SOURCE_BASE_URL:-${LMM_HF_ENDPOINT:-}}"
CURRENT_PID=""

case "$MODEL_KEY" in ''|*/*|*'..'*) echo "下载 MiniMax-H3 权重失败：LMM_MODEL_NAME 只能是单个安全目录名。" >&2; exit 1;; esac

cleanup() { if [ -n "$CURRENT_PID" ] && kill -0 "$CURRENT_PID" 2>/dev/null; then kill -TERM "$CURRENT_PID" 2>/dev/null || true; fi; }
trap cleanup TERM INT
die() { echo "下载 MiniMax-H3 权重失败：$*" >&2; exit 1; }
progress() {
  local done_bytes partial sum
  done_bytes=$(find "$MODEL_DIR" -type f -not -path "*/.cache/*" -printf '%s\n' 2>/dev/null | awk '{s+=$1} END{printf "%.0f", s+0}')
  partial=$(find "$MODEL_DIR" -name '*.incomplete' -printf '%s\n' 2>/dev/null | awk '{s+=$1} END{printf "%.0f", s+0}')
  sum=$((done_bytes + partial)); [ "$TOTAL" -gt 0 ] && [ "$sum" -gt "$TOTAL" ] && sum="$TOTAL"
  echo "LMM_PROGRESS_BYTES=$sum/$TOTAL"
}
verify() { (cd "$MODEL_DIR" && sha256sum -c "$PKG_DIR/weights.sha256"); }
run_with_progress() {
  "$@" & CURRENT_PID=$!
  while kill -0 "$CURRENT_PID" 2>/dev/null; do progress; sleep 15; done
  wait "$CURRENT_PID"; local rc=$?; CURRENT_PID=""; return "$rc"
}

[ "$TOTAL" = "43317497012" ] || die "LMM_MODEL_SIZE_BYTES 应为 43317497012，当前为 $TOTAL。"
[ -n "$SOURCE_BASE" ] || die "LMM 未提供 LMM_SOURCE_BASE_URL；请选择下载源后重试。"
mkdir -p "$MODEL_DIR"
if verify >/dev/null 2>&1; then
  find "$MODEL_DIR" -name '*.incomplete' -type f -delete
  progress
  echo "全部权重已经存在且校验通过。"
  exit 0
fi

case "$SOURCE_KIND" in
  local_dir)
    command -v rsync >/dev/null 2>&1 || die "离线复制需要 rsync。"
    [ -d "$SOURCE_BASE" ] || die "离线模型目录不存在：$SOURCE_BASE"
    run_with_progress rsync -a --partial --info=progress2 "$SOURCE_BASE/" "$MODEL_DIR/" || die "离线复制中断；保留的部分可在下次继续。"
    ;;
  hf_endpoint)
    HF="$ENGINE_DIR/runtime/bin/hf"
    [ -x "$HF" ] || die "找不到 hf 命令，请先执行 install_engine.sh。"
    export HF_ENDPOINT="$SOURCE_BASE" HF_TOKEN="${LMM_HF_TOKEN:-}"
    run_with_progress "$HF" download Comfy-Org/MiniMax-H3 \
      diffusion_models/minimax_h3_fl2va_pruned_int8_convrot.safetensors \
      text_encoders/qwen3vl_32b_minimax_h3_nvfp4_awq.safetensors \
      vae/minimax_h3_video_vae_fp16.safetensors \
      vae/minimax_h3_audio_vae_fp32.safetensors \
      --revision a98869194787969724c7425d95d0ed73ce9202af --local-dir "$MODEL_DIR" --max-workers 4 || die "MiniMax-H3 主权重下载中断，LMM 可换源后续传。"
    run_with_progress "$HF" download larryvrh/MiniMax-H3-Turbo-Lora \
      minimax_h3_turbo_v4_step600_ema.safetensors \
      --revision 43a74557ac3f6539db8e0f2a959d03feb7a81480 --local-dir "$MODEL_DIR/loras" --max-workers 4 || die "Turbo LoRA 下载中断，LMM 可换源后续传。"
    run_with_progress "$HF" download rockapaper/realesrgan RealESRGAN_x2plus.pth \
      --revision main --local-dir "$MODEL_DIR/upscale_models" --max-workers 4 || die "1080p 超分模型下载中断，LMM 可换源后续传。"
    ;;
  *) die "不支持的下载源类型：$SOURCE_KIND（仅支持 hf_endpoint 或 local_dir）。" ;;
esac

if ! verify; then
  while read -r expected rel; do
    [ -z "${expected:-}" ] && continue
    actual=$(sha256sum "$MODEL_DIR/$rel" 2>/dev/null | awk '{print $1}')
    if [ "$actual" != "$expected" ]; then rm -f "$MODEL_DIR/$rel"; echo "已删除校验失败的文件：$rel" >&2; fi
  done < "$PKG_DIR/weights.sha256"
  die "至少一个权重校验失败；坏文件已删除，请重试或切换下载源。"
fi
find "$MODEL_DIR" -name '*.incomplete' -type f -delete
progress
echo "MiniMax-H3 权重下载及 SHA-256 校验完成：$MODEL_DIR"
