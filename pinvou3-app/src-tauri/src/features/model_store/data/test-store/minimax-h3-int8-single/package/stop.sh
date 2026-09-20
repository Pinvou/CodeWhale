#!/usr/bin/env bash
set -euo pipefail
DEPLOY_DIR="${LMM_DEPLOY_DIR:-$HOME/.local/share/lmm/deployments/minimax-h3-int8-single}"
PID_FILE="$DEPLOY_DIR/server.pid"
[ -f "$PID_FILE" ] || { echo "MiniMax-H3 ComfyUI 已停止。"; exit 0; }
PID="$(cat "$PID_FILE" 2>/dev/null || true)"
case "$PID" in ''|*[!0-9]*) rm -f "$PID_FILE"; echo "PID 文件无效，已清理。"; exit 0;; esac
if [ ! -d "/proc/$PID" ]; then rm -f "$PID_FILE"; echo "MiniMax-H3 ComfyUI 已停止。"; exit 0; fi
CMDLINE="$(tr '\0' ' ' < "/proc/$PID/cmdline" 2>/dev/null || true)"
case "$CMDLINE" in *"ComfyUI"*"--user-directory"*"$DEPLOY_DIR/user"*) ;; *) echo "停止失败：PID $PID 不属于本卡片，拒绝终止。" >&2; exit 1;; esac
kill -TERM "$PID"
for attempt in $(seq 1 30); do
  if [ ! -d "/proc/$PID" ]; then rm -f "$PID_FILE"; echo "MiniMax-H3 ComfyUI 已停止。"; exit 0; fi
  echo "正在停止 MiniMax-H3 ComfyUI（$attempt/30）"; sleep 1
done
echo "停止失败：进程在 30 秒内未退出，请检查 $DEPLOY_DIR/logs/server.log。" >&2
exit 1
