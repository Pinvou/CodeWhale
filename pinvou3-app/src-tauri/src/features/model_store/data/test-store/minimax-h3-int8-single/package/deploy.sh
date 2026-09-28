#!/usr/bin/env bash
set -euo pipefail
PKG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MODEL_KEY="${LMM_MODEL_NAME:-minimax-h3-int8-single}"
MODEL_DIR="${LMM_MODELS_DIR:-$HOME/.local/share/lmm/models}/$MODEL_KEY"
ENGINE_DIR="${LMM_ENGINE_DIR:-$HOME/.local/share/lmm/engines/comfyui-minimax-h3-0.35.0-cu130}"
DEPLOY_DIR="${LMM_DEPLOY_DIR:-$HOME/.local/share/lmm/deployments/minimax-h3-int8-single}"
PROFILE="${LMM_DEPLOY_PROFILE:-quality-current}"
PORT="${LMM_SERVICE_PORT:-8188}"
die() { echo "部署 MiniMax-H3 失败：$*" >&2; exit 1; }
case "$MODEL_KEY" in ''|*/*|*'..'*) die "LMM_MODEL_NAME 只能是单个安全目录名。";; esac
case "$PROFILE" in quality-current|balanced-fast) ;; *) die "未知部署方案：$PROFILE" ;; esac
[ -x "$ENGINE_DIR/runtime/bin/python" ] && [ -f "$ENGINE_DIR/ComfyUI/main.py" ] || die "引擎未就绪，请先执行 install_engine.sh。"
[ -d "$MODEL_DIR" ] || die "模型目录不存在，请先执行 download_model.sh。"
(cd "$MODEL_DIR" && sha256sum -c "$PKG_DIR/weights.sha256") || die "模型权重校验失败。"
if [ -f "$DEPLOY_DIR/server.pid" ]; then
  LMM_DEPLOY_DIR="$DEPLOY_DIR" bash "$PKG_DIR/stop.sh"
fi
mkdir -p "$DEPLOY_DIR/user/default/workflows" "$DEPLOY_DIR/input" "$DEPLOY_DIR/output" "$DEPLOY_DIR/logs"
cp "$PKG_DIR/workflows/MiniMaxH3_QUALITY_CURRENT_1080P.json" "$DEPLOY_DIR/user/default/workflows/"
cp "$PKG_DIR/workflows/MiniMaxH3_BALANCED_FAST_1080P.json" "$DEPLOY_DIR/user/default/workflows/"
if [ "$PROFILE" = "quality-current" ]; then DEFAULT_WORKFLOW="MiniMaxH3_QUALITY_CURRENT_1080P.json"; else DEFAULT_WORKFLOW="MiniMaxH3_BALANCED_FAST_1080P.json"; fi
cp "$DEPLOY_DIR/user/default/workflows/$DEFAULT_WORKFLOW" "$DEPLOY_DIR/user/default/workflows/MiniMaxH3_DEFAULT.json"
cat > "$DEPLOY_DIR/extra_model_paths.yaml" <<EOF
minimax_h3_single:
  base_path: $MODEL_DIR
  diffusion_models: diffusion_models
  text_encoders: text_encoders
  vae: vae
  loras: loras
  upscale_models: upscale_models
EOF
printf 'ENGINE_DIR=%q\nDEPLOY_DIR=%q\nMODEL_DIR=%q\nPORT=%q\nPROFILE=%q\n' "$ENGINE_DIR" "$DEPLOY_DIR" "$MODEL_DIR" "$PORT" "$PROFILE" > "$DEPLOY_DIR/deployment.env"
cat > "$DEPLOY_DIR/run.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/deployment.env"
cd "$ENGINE_DIR/ComfyUI"
exec "$ENGINE_DIR/runtime/bin/python" main.py --listen 0.0.0.0 --port "$PORT" --fast autotune \
  --extra-model-paths-config "$DEPLOY_DIR/extra_model_paths.yaml" \
  --user-directory "$DEPLOY_DIR/user" --input-directory "$DEPLOY_DIR/input" --output-directory "$DEPLOY_DIR/output"
EOF
chmod +x "$DEPLOY_DIR/run.sh"
cat > "$DEPLOY_DIR/MODEL_VERSION.json" <<EOF
{"card_version":"1.0.0","profile":"$PROFILE","model_revision":"a98869194787969724c7425d95d0ed73ce9202af","turbo_revision":"43a74557ac3f6539db8e0f2a959d03feb7a81480","comfyui":"0.35.0","pytorch":"2.13.0+cu130","default_workflow":"$DEFAULT_WORKFLOW"}
EOF
echo "部署配置已生成（尚未启动）：$DEPLOY_DIR"
echo "默认工作流：$DEFAULT_WORKFLOW"
