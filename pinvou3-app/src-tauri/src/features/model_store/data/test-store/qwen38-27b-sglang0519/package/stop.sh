#!/usr/bin/env bash
# ============================================================================
# stop.sh — Stop the Qwen3.8-27B-NVFP4 endpoint and free the unified memory
# ============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONTAINER_NAME="qwen38-sglang"
[ -f "$SCRIPT_DIR/.deploy-env" ] && . "$SCRIPT_DIR/.deploy-env"

log()  { printf '\033[1;32m[stop]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[stop]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[stop] ERROR:\033[0m %s\n' "$*" >&2; exit 1; }

DOCK=""
if docker info >/dev/null 2>&1; then
  DOCK="docker"
elif command -v sudo >/dev/null && sudo docker info >/dev/null 2>&1; then
  DOCK="sudo docker"
else
  die "cannot access the docker daemon. Either run in a terminal so sudo can prompt for your password, or: sudo usermod -aG docker \$USER (then re-login)"
fi
dock() { $DOCK "$@"; }

if ! dock ps -a --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
  log "no container named '$CONTAINER_NAME' — nothing to stop"
  exit 0
fi

if dock ps --format '{{.Names}}' | grep -qx "$CONTAINER_NAME"; then
  log "stopping '$CONTAINER_NAME' (graceful, up to 120s)"
  dock stop -t 120 "$CONTAINER_NAME" >/dev/null
fi

# --restart unless-stopped keeps the stopped container around; remove it so
# start.sh always begins from a clean state.
dock rm "$CONTAINER_NAME" >/dev/null 2>&1 || true
log "container stopped and removed"

# Unified memory is reclaimed asynchronously by the driver; give it a moment.
log "waiting for memory to be reclaimed..."
for _ in $(seq 1 12); do
  avail=$(awk '/MemAvailable/ {print int($2/1024/1024)}' /proc/meminfo)
  [ "$avail" -ge 100 ] && break
  sleep 5
done
avail=$(awk '/MemAvailable/ {print int($2/1024/1024)}' /proc/meminfo)
log "done. MemAvailable: ${avail} GB"
[ "$avail" -ge 100 ] || warn "memory still below 100 GB available — wait ~20s before restarting"
