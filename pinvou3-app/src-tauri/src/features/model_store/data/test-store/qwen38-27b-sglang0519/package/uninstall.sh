#!/usr/bin/env bash
# ============================================================================
# uninstall.sh — Remove the Qwen3.8-27B-NVFP4 deployment
#
# Default (interactive): stops and removes the container, asks about the
# docker image, model weights, and local caches.
# Flags:
#   --all          remove everything, including the model weights (~23.5 GB)
#   --keep-model   keep the model weights, remove everything else
#   --yes / -y     do not ask; use defaults (remove container+image+caches,
#                  keep the model unless --all was given)
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="lmsysorg/sglang:v0.5.19"
CONTAINER_NAME="qwen38-sglang"
MODEL_DIR="$SCRIPT_DIR/models/Qwen3.8-27B-NVFP4"
[ -f "$SCRIPT_DIR/.deploy-env" ] && . "$SCRIPT_DIR/.deploy-env"

REMOVE_ALL=0 ASSUME_YES=0 KEEP_MODEL=0
for arg in "$@"; do
  case "$arg" in
    --all) REMOVE_ALL=1 ;;
    --keep-model) KEEP_MODEL=1 ;;
    --yes|-y) ASSUME_YES=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 1 ;;
  esac
done

log()  { printf '\033[1;32m[uninstall]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[uninstall] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

DOCK=""
if docker info >/dev/null 2>&1; then
  DOCK="docker"
elif command -v sudo >/dev/null && sudo docker info >/dev/null 2>&1; then
  DOCK="sudo docker"
else
  die "cannot access the docker daemon. Either run in a terminal so sudo can prompt for your password, or: sudo usermod -aG docker \$USER (then re-login)"
fi
dock() { $DOCK "$@"; }

confirm() { # confirm <question> <default:y|n>
  local q="$1" def="${2:-n}" ans
  [ "$ASSUME_YES" = "1" ] && { [ "$def" = "y" ]; return; }
  read -r -p "$q [$def] " ans
  ans="${ans:-$def}"
  [ "$ans" = "y" ] || [ "$ans" = "Y" ]
}

# 1. container ----------------------------------------------------------------
if dock ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$CONTAINER_NAME"; then
  log "stopping and removing container '$CONTAINER_NAME'"
  dock stop -t 60 "$CONTAINER_NAME" >/dev/null 2>&1 || true
  dock rm "$CONTAINER_NAME" >/dev/null 2>&1 || true
fi

# 2. image --------------------------------------------------------------------
if dock image inspect "$IMAGE" >/dev/null 2>&1; then
  if [ "$ASSUME_YES" = "1" ] || confirm "Remove docker image $IMAGE (~15 GB)?" "y"; then
    dock rmi "$IMAGE" && log "image removed"
  fi
fi

# 3. model weights -------------------------------------------------------------
if [ "$REMOVE_ALL" = "1" ] && [ "$KEEP_MODEL" = "0" ]; then
  log "removing model weights: $MODEL_DIR"
  rm -rf "$MODEL_DIR"
  # also remove the pre-shipped ModelScope cache layout if present
  rm -rf "$SCRIPT_DIR/unsloth--Qwen3.8-27B-NVFP4"
elif [ -d "$MODEL_DIR" ] && [ "$KEEP_MODEL" = "0" ]; then
  if confirm "Remove model weights at $MODEL_DIR (~23.5 GB)?" "n"; then
    rm -rf "$MODEL_DIR"
    log "model removed"
  else
    log "model kept: $MODEL_DIR"
  fi
fi

# 4. local state (caches, venv, env file) --------------------------------------
log "removing local state: .cache .venv .deploy-env"
rm -rf "$SCRIPT_DIR/.cache" "$SCRIPT_DIR/.venv" "$SCRIPT_DIR/.deploy-env"

cat <<EOF

============================================================================
 Uninstall complete.

 Not touched (remove manually if desired):
   - Docker CE and nvidia-container-toolkit system packages
   - apt/pip mirror configuration on the host
$( [ "$KEEP_MODEL" = "1" ] || [ "$REMOVE_ALL" = "0" ] && echo "   - model weights (if you chose to keep them)" )
============================================================================
EOF
