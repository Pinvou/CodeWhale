#!/usr/bin/env bash
set -euo pipefail
DEPLOY_DIR="${LMM_DEPLOY_DIR:-$HOME/.local/share/lmm/deployments/minimax-h3-int8-single}"
[ -f "$DEPLOY_DIR/deployment.env" ] || { echo "启用失败：部署配置不存在，请先执行 deploy.sh。" >&2; exit 1; }
source "$DEPLOY_DIR/deployment.env"
healthy() { "$ENGINE_DIR/runtime/bin/python" - "$PORT" <<'PY' >/dev/null 2>&1
import json,sys,urllib.request
with urllib.request.urlopen('http://127.0.0.1:%s/queue' % sys.argv[1], timeout=5) as r:
    q=json.load(r)
assert 'queue_running' in q and 'queue_pending' in q
PY
}
PID=""
if [ -f "$DEPLOY_DIR/server.pid" ]; then
  PID="$(cat "$DEPLOY_DIR/server.pid" 2>/dev/null || true)"
  case "$PID" in ''|*[!0-9]*) PID="";; esac
  if [ -n "$PID" ] && ! kill -0 "$PID" 2>/dev/null; then PID=""; fi
fi
if [ -n "$PID" ] && healthy; then
  echo "LMM_ENDPOINT_URL=http://127.0.0.1:$PORT/"
  echo "LMM_ENDPOINT_MODEL=MiniMax-H3-INT8-Turbo"
  exit 0
fi
if [ -z "$PID" ] && healthy; then echo "启用失败：端口 $PORT 已被其他健康的 ComfyUI 服务占用。" >&2; exit 1; fi
if [ -z "$PID" ]; then
  nohup "$DEPLOY_DIR/run.sh" > "$DEPLOY_DIR/logs/server.log" 2>&1 </dev/null &
  PID=$!; echo "$PID" > "$DEPLOY_DIR/server.pid"
fi
cleanup() { if kill -0 "$PID" 2>/dev/null; then kill -TERM "$PID" 2>/dev/null || true; fi; }
trap cleanup TERM INT
for attempt in $(seq 1 90); do
  if healthy; then
    trap - TERM INT
    echo "LMM_ENDPOINT_URL=http://127.0.0.1:$PORT/"
    echo "LMM_ENDPOINT_MODEL=MiniMax-H3-INT8-Turbo"
    exit 0
  fi
  if ! kill -0 "$PID" 2>/dev/null; then tail -n 60 "$DEPLOY_DIR/logs/server.log" >&2 || true; echo "启用失败：ComfyUI 进程提前退出。" >&2; exit 1; fi
  echo "MiniMax-H3 ComfyUI 启动中：等待健康检查（$attempt/90）"
  sleep 2
done
cleanup
tail -n 60 "$DEPLOY_DIR/logs/server.log" >&2 || true
echo "启用失败：180 秒内未通过 /queue 健康检查。" >&2
exit 1
