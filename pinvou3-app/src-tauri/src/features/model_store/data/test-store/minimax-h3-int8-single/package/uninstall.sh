#!/usr/bin/env bash
set -euo pipefail
PKG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_DIR="${LMM_DEPLOY_DIR:-$HOME/.local/share/lmm/deployments/minimax-h3-int8-single}"
ENGINE_DIR="${LMM_ENGINE_DIR:-$HOME/.local/share/lmm/engines/comfyui-minimax-h3-0.35.0-cu130}"
LMM_DEPLOY_DIR="$DEPLOY_DIR" bash "$PKG_DIR/stop.sh"
case "$DEPLOY_DIR" in "$HOME"/*) rm -rf -- "$DEPLOY_DIR";; *) echo "卸载失败：部署目录必须位于当前用户主目录内：$DEPLOY_DIR" >&2; exit 1;; esac
if [ "${LMM_REMOVE_ENGINE:-0}" = "1" ]; then
  case "$ENGINE_DIR" in "$HOME"/*) rm -rf -- "$ENGINE_DIR";; *) echo "引擎目录必须位于当前用户主目录内，未删除：$ENGINE_DIR" >&2; exit 1;; esac
fi
echo "部署产物已删除；模型权重按规范保留。"
