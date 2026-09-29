#!/usr/bin/env bash
# ============================================================================
# deploy.sh — One-shot deployment for Qwen3.8-27B-NVFP4 on NVIDIA GB10 (DGX Spark)
#
# What it does:
#   1. Verifies hardware / OS (aarch64 + NVIDIA GPU, Ubuntu 24.04 assumed)
#   2. Installs Docker CE + NVIDIA Container Toolkit if missing (China mirrors)
#   3. Pulls the official image lmsysorg/sglang:v0.5.19 (Docker Hub mirrors,
#      with fallback chain) and re-tags it to the canonical name
#   4. Verifies GPU access inside a container
#   5. Downloads the model from ModelScope (fallback: hf-mirror.com), or reuses
#      an already-downloaded copy found next to this script
#   6. Writes .deploy-env, which start.sh / stop.sh / uninstall.sh read
#
# Everything is idempotent: re-running skips work that is already done.
# Override knobs (env): MODEL_DIR, PORT, MIRROR_LIST, SKIP_DOCKER_INSTALL=1
# ============================================================================
set -euo pipefail

# --------------------------------------------------------------------------
# Configuration
# --------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$SCRIPT_DIR/.deploy-env"

IMAGE="lmsysorg/sglang:v0.5.19"
CONTAINER_NAME="qwen38-sglang"
PORT="${PORT:-30000}"
MODEL_ID="unsloth/Qwen3.8-27B-NVFP4"
MODEL_DIR="${MODEL_DIR:-$SCRIPT_DIR/models/Qwen3.8-27B-NVFP4}"
VENV_DIR="$SCRIPT_DIR/.venv"

# Docker Hub mirror chain (Mainland China). First reachable one wins.
# Override with MIRROR_LIST="prefix1 prefix2 ..." (space-separated).
MIRROR_LIST="${MIRROR_LIST:-m.daocloud.io/docker.io docker.1ms.run docker.m.daocloud.io DIRECT}"

PIP_MIRROR="https://pypi.tuna.tsinghua.edu.cn/simple"
APT_MIRROR="https://mirrors.aliyun.com/ubuntu-ports/"
DOCKER_CE_MIRROR="https://mirrors.aliyun.com/docker-ce"

# Model file verification (bytes, from the official repo)
declare -A MODEL_FILES=(
  [model.safetensors]=22568192096
  [model_mtp.safetensors]=849400392
)
MODEL_EXTRA_FILES=(
  config.json generation_config.json tokenizer.json tokenizer_config.json
  vocab.json chat_template.jinja preprocessor_config.json
  video_preprocessor_config.json model.safetensors.index.json
)

# --------------------------------------------------------------------------
# Helpers
# --------------------------------------------------------------------------
log()  { printf '\033[1;32m[deploy]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[deploy]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[deploy] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

SUDO=""
need_sudo() {
  if [ "$(id -u)" -ne 0 ]; then
    command -v sudo >/dev/null || die "need root privileges but sudo is not installed"
    SUDO="sudo"
  fi
}

docker_ok() { docker info >/dev/null 2>&1; }

DOCK=""
init_dock() {
  if docker_ok; then
    DOCK="docker"
  elif command -v sudo >/dev/null && sudo docker info >/dev/null 2>&1; then
    DOCK="sudo docker"
  else
    die "cannot access the docker daemon. Either run in a terminal so sudo can prompt for your password, or: sudo usermod -aG docker \$USER (then re-login)"
  fi
}
dock() { $DOCK "$@"; }

# --------------------------------------------------------------------------
# Step 1: hardware / OS sanity
# --------------------------------------------------------------------------
log "Step 1/6: checking hardware and OS"
[ "$(uname -m)" = "aarch64" ] || warn "this solution is tuned for aarch64 (GB10); '$(uname -m)' detected — continuing anyway"
command -v nvidia-smi >/dev/null || die "nvidia-smi not found — NVIDIA driver >= 580 (CUDA 13) is required"
nvidia-smi -L || die "nvidia-smi failed — check the NVIDIA driver installation"

# --------------------------------------------------------------------------
# Step 2: Docker CE
# --------------------------------------------------------------------------
log "Step 2/6: checking Docker"
if ! command -v docker >/dev/null; then
  [ "${SKIP_DOCKER_INSTALL:-0}" = "1" ] && die "docker not installed and SKIP_DOCKER_INSTALL=1"
  need_sudo
  log "installing Docker CE from Aliyun mirror ($DOCKER_CE_MIRROR)"
  $SUDO apt-get update
  $SUDO apt-get install -y ca-certificates curl gnupg
  $SUDO install -m 0755 -d /etc/apt/keyrings
  curl -fsSL "$DOCKER_CE_MIRROR/linux/ubuntu/gpg" | $SUDO gpg --dearmor -o /etc/apt/keyrings/docker.gpg
  $SUDO chmod a+r /etc/apt/keyrings/docker.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] $DOCKER_CE_MIRROR/linux/ubuntu $(. /etc/os-release && echo "$VERSION_CODENAME") stable" \
    | $SUDO tee /etc/apt/sources.list.d/docker.list >/dev/null
  $SUDO apt-get update
  $SUDO apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  $SUDO systemctl enable --now docker
fi
init_dock
if [ "$DOCK" != "docker" ]; then
  warn "docker daemon accessed via sudo. To drop sudo: sudo usermod -aG docker \$USER (then re-login)"
fi

# --------------------------------------------------------------------------
# Step 3: NVIDIA Container Toolkit
# --------------------------------------------------------------------------
log "Step 3/6: checking NVIDIA Container Toolkit"
if ! command -v nvidia-ctk >/dev/null; then
  need_sudo
  log "installing nvidia-container-toolkit"
  curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
    | $SUDO gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
  curl -s -L https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
    | $SUDO tee /etc/apt/sources.list.d/nvidia-container-toolkit.list >/dev/null
  $SUDO apt-get update
  $SUDO apt-get install -y nvidia-container-toolkit
  # Generate the CDI spec so both --gpus and --device nvidia.com/gpu=all work
  $SUDO nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml || true
  $SUDO systemctl restart docker
fi

# --------------------------------------------------------------------------
# Step 4: pull the official SGLang image (mirror fallback chain)
# --------------------------------------------------------------------------
log "Step 4/6: pulling $IMAGE"
if dock image inspect "$IMAGE" >/dev/null 2>&1; then
  log "image already present: $IMAGE"
else
  pulled=""
  for prefix in $MIRROR_LIST; do
    if [ "$prefix" = "DIRECT" ]; then
      ref="$IMAGE"
    else
      ref="$prefix/$IMAGE"
    fi
    log "trying: docker pull $ref"
    if dock pull "$ref"; then
      pulled="$ref"
      break
    fi
    warn "pull failed via $prefix — trying next mirror"
  done
  [ -n "$pulled" ] || die "could not pull $IMAGE from any mirror. Check the network, or set MIRROR_LIST to a reachable registry mirror."
  if [ "$pulled" != "$IMAGE" ]; then
    dock tag "$pulled" "$IMAGE"
    dock rmi "$pulled" >/dev/null 2>&1 || true
  fi
  log "image ready: $IMAGE"
fi

# --------------------------------------------------------------------------
# Step 5: verify GPU access inside the container
# --------------------------------------------------------------------------
log "Step 5/6: verifying GPU access inside a container"
GPU_FLAGS=""
for candidate in "--gpus all" "--device nvidia.com/gpu=all"; do
  # shellcheck disable=SC2086
  if dock run --rm $candidate "$IMAGE" nvidia-smi -L >/dev/null 2>&1; then
    GPU_FLAGS="$candidate"
    log "GPU access works with: $candidate"
    break
  fi
done
[ -n "$GPU_FLAGS" ] || die "container cannot see the GPU. Check nvidia-container-toolkit (see DEPLOYMENT.md → Troubleshooting)."

# --------------------------------------------------------------------------
# Step 6: model weights
# --------------------------------------------------------------------------
log "Step 6/6: checking model weights"

model_complete() {
  local dir="$1" f size actual
  [ -d "$dir" ] || return 1
  for f in "${!MODEL_FILES[@]}"; do
    [ -f "$dir/$f" ] || return 1
    size="${MODEL_FILES[$f]}"
    actual=$(stat -c %s "$dir/$f" 2>/dev/null || echo 0)
    [ "$actual" = "$size" ] || return 1
  done
  for f in "${MODEL_EXTRA_FILES[@]}"; do
    [ -f "$dir/$f" ] || return 1
  done
  return 0
}

FOUND_MODEL=""
# Candidate locations, in priority order: explicit MODEL_DIR, default dir,
# ModelScope cache layout (as shipped with this project), flat dir.
for cand in \
  "$MODEL_DIR" \
  "$SCRIPT_DIR/models/Qwen3.8-27B-NVFP4" \
  "$SCRIPT_DIR/unsloth--Qwen3.8-27B-NVFP4/snapshots/master" \
  "$SCRIPT_DIR/unsloth--Qwen3.8-27B-NVFP4" \
  "$SCRIPT_DIR/Qwen3.8-27B-NVFP4"; do
  if model_complete "$cand"; then FOUND_MODEL="$cand"; break; fi
done

if [ -n "$FOUND_MODEL" ]; then
  log "found complete model at: $FOUND_MODEL"
  MODEL_DIR="$FOUND_MODEL"
else
  log "downloading $MODEL_ID (~23.5 GB) from ModelScope"
  mkdir -p "$MODEL_DIR"
  # ModelScope CLI in a local venv (no system-wide pip pollution)
  if [ ! -x "$VENV_DIR/bin/modelscope" ]; then
    log "creating download venv at $VENV_DIR (pip mirror: $PIP_MIRROR)"
    python3 -m venv "$VENV_DIR" || {
      need_sudo; $SUDO apt-get update && $SUDO apt-get install -y python3-venv
      python3 -m venv "$VENV_DIR"
    }
    "$VENV_DIR/bin/pip" install -q --upgrade pip -i "$PIP_MIRROR"
    "$VENV_DIR/bin/pip" install -q modelscope -i "$PIP_MIRROR"
  fi
  if ! "$VENV_DIR/bin/modelscope" download --model "$MODEL_ID" --local_dir "$MODEL_DIR"; then
    warn "ModelScope download failed — falling back to hf-mirror.com"
    "$VENV_DIR/bin/pip" install -q "huggingface_hub[cli,hf_transfer]" -i "$PIP_MIRROR"
    HF_ENDPOINT=https://hf-mirror.com "$VENV_DIR/bin/hf" download "$MODEL_ID" --local-dir "$MODEL_DIR"
  fi
  model_complete "$MODEL_DIR" || die "download finished but the model directory is incomplete: $MODEL_DIR"
  log "model ready: $MODEL_DIR"
fi

# --------------------------------------------------------------------------
# Write .deploy-env for the other scripts
# --------------------------------------------------------------------------
cat > "$ENV_FILE" <<EOF
# Written by deploy.sh on $(date -Is). Sourced by start.sh / stop.sh / uninstall.sh.
IMAGE="$IMAGE"
CONTAINER_NAME="$CONTAINER_NAME"
PORT="$PORT"
MODEL_DIR="$MODEL_DIR"
GPU_FLAGS="$GPU_FLAGS"
EOF
log "wrote $ENV_FILE"

cat <<EOF

============================================================================
 Deployment complete.

   Image : $IMAGE
   Model : $MODEL_DIR
   GPU   : via $GPU_FLAGS
   Port  : $PORT

 Next steps:
   ./start.sh     # start the endpoint (~5 min to ready on GB10)
   ./stop.sh      # stop the endpoint
   ./uninstall.sh # remove container / image / (optionally) model
============================================================================
EOF
