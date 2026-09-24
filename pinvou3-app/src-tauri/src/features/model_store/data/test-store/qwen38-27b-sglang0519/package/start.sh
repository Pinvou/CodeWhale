#!/usr/bin/env bash
# ============================================================================
# start.sh — Start the Qwen3.8-27B-NVFP4 OpenAI-compatible endpoint (SGLang)
#
# Reads .deploy-env written by deploy.sh (falls back to built-in defaults).
# Any extra CLI arguments are appended to the sglang launch command and
# override the built-in ones, e.g.:
#   ./start.sh --max-running-requests 8
# Env overrides: PORT, MEM_FRACTION_STATIC, MAX_RUNNING_REQUESTS,
#                STARTUP_TIMEOUT (seconds, default 3600)
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Defaults (overridden by .deploy-env when present; env vars win over both)
_ENV_PORT="${PORT:-}"
IMAGE="lmsysorg/sglang:v0.5.19"
CONTAINER_NAME="qwen38-sglang"
PORT="30000"
MODEL_DIR="$SCRIPT_DIR/models/Qwen3.8-27B-NVFP4"
GPU_FLAGS="--gpus all"
[ -f "$SCRIPT_DIR/.deploy-env" ] && . "$SCRIPT_DIR/.deploy-env"
PORT="${_ENV_PORT:-$PORT}"

MEM_FRACTION_STATIC="${MEM_FRACTION_STATIC:-0.60}"
MAX_RUNNING_REQUESTS="${MAX_RUNNING_REQUESTS:-16}"
STARTUP_TIMEOUT="${STARTUP_TIMEOUT:-3600}"
CACHE_DIR="$SCRIPT_DIR/.cache/sglang"

log()  { printf '\033[1;32m[start]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[start] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

DOCK=""
if docker info >/dev/null 2>&1; then
  DOCK="docker"
elif command -v sudo >/dev/null && sudo docker info >/dev/null 2>&1; then
  DOCK="sudo docker"
else
  die "cannot access the docker daemon. Either run in a terminal so sudo can prompt for your password, or: sudo usermod -aG docker \$USER (then re-login)"
fi
dock() { $DOCK "$@"; }

# --------------------------------------------------------------------------
# Preconditions
# --------------------------------------------------------------------------
[ -f "$MODEL_DIR/model.safetensors" ] || die "model not found at $MODEL_DIR — run ./deploy.sh first"
dock image inspect "$IMAGE" >/dev/null 2>&1 || die "image $IMAGE not present — run ./deploy.sh first"

if dock ps --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
  log "container '$CONTAINER_NAME' is already running"
  log "endpoint: http://127.0.0.1:$PORT/v1   (logs: docker logs -f $CONTAINER_NAME)"
  exit 0
fi
# Remove a leftover stopped container with the same name
dock rm "$CONTAINER_NAME" >/dev/null 2>&1 || true

mkdir -p "$CACHE_DIR"

# --------------------------------------------------------------------------
# Launch
# Tuning notes (validated on GB10, see DEPLOYMENT.md for the full table):
#   mem-fraction-static 0.60  : 0.75+ exhausts the 128 GB unified memory
#   speculative-adaptive      : EAGLE MTP w/ adaptive steps (accept ~4.4)
#   bfloat16 SSM + flashinfer linear-attn kernels: ~2x SSM state capacity
#   MAX_JOBS=2                : prevents parallel nvcc from OOM-freezing the
#                               machine during first-boot JIT compilation
# --------------------------------------------------------------------------
log "starting container '$CONTAINER_NAME' (image: $IMAGE)"
# shellcheck disable=SC2086
dock run -d \
  --name "$CONTAINER_NAME" \
  --restart unless-stopped \
  $GPU_FLAGS \
  --ipc=host \
  --shm-size 32g \
  -p "$PORT:30000" \
  -v "$MODEL_DIR:/model:ro" \
  -v "$CACHE_DIR:/root/.cache/sglang" \
  -e MAX_JOBS=2 \
  "$IMAGE" \
  python3 -m sglang.launch_server \
    --model-path /model \
    --served-model-name Qwen3.8-27B-NVFP4 \
    --host 0.0.0.0 \
    --port 30000 \
    --trust-remote-code \
    --mem-fraction-static "$MEM_FRACTION_STATIC" \
    --chunked-prefill-size 2048 \
    --speculative-algorithm EAGLE \
    --speculative-adaptive \
    --enable-linear-replayssm-spec \
    --mamba-radix-cache-strategy extra_buffer_lazy \
    --mamba-ssm-dtype bfloat16 \
    --max-mamba-cache-size 64 \
    --linear-attn-decode-backend flashinfer \
    --linear-attn-verify-backend flashinfer \
    --linear-attn-prefill-backend flashinfer \
    --max-running-requests "$MAX_RUNNING_REQUESTS" \
    --cuda-graph-max-bs-decode "$MAX_RUNNING_REQUESTS" \
    --stream-interval 8 \
    --mm-attention-backend fa4 \
    --reasoning-parser qwen3 \
    --tool-call-parser qwen3_coder \
    "$@"

# --------------------------------------------------------------------------
# Wait for readiness. Startup = weight load (~2.5 min) + kernel warmup/JIT +
# CUDA graph capture ≈ 5 min total on GB10 (measured). The JIT cache persists
# in .cache/sglang, so later starts are at least as fast.
# Note on --mm-attention-backend: fa4 (FlashAttention 4, bundled in the image)
# is the fast ViT path on SM121. Do NOT use flashinfer_cudnn here — the
# image's cuDNN 9.20 is too old (< 9.26) and crashes the vision tower at
# warmup (cudnnGraphNotSupportedError). Fallback if fa4 ever misbehaves:
#   ./start.sh --mm-attention-backend triton_attn
# --------------------------------------------------------------------------
log "waiting for the server to become ready (timeout ${STARTUP_TIMEOUT}s)"
log "follow progress with: docker logs -f $CONTAINER_NAME"
deadline=$(( $(date +%s) + STARTUP_TIMEOUT ))
while true; do
  if ! dock ps --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
    echo
    dock logs --tail 50 "$CONTAINER_NAME" >&2 || true
    die "container exited during startup — see logs above"
  fi
  if curl -sf "http://127.0.0.1:$PORT/health" >/dev/null 2>&1; then
    break
  fi
  [ "$(date +%s)" -lt "$deadline" ] || {
    dock logs --tail 50 "$CONTAINER_NAME" >&2 || true
    die "server did not become ready within ${STARTUP_TIMEOUT}s — see logs above"
  }
  sleep 15
done

cat <<EOF

============================================================================
 Endpoint is ready.

   OpenAI-compatible API : http://127.0.0.1:$PORT/v1
   Model name            : Qwen3.8-27B-NVFP4
   Container             : $CONTAINER_NAME

 Quick test:
   curl http://127.0.0.1:$PORT/v1/chat/completions \\
     -H 'Content-Type: application/json' \\
     -d '{"model":"Qwen3.8-27B-NVFP4","messages":[{"role":"user","content":"Hello!"}]}'

 Manage:
   docker logs -f $CONTAINER_NAME   # logs
   ./stop.sh                        # stop
============================================================================
EOF
