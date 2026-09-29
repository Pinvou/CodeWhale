#!/usr/bin/env bash
set -euo pipefail
CARD_ID="${LMM_CARD_ID:-minimax-h3-int8-single}"
PKG_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENGINE_DIR="${LMM_ENGINE_DIR:-$HOME/.local/share/lmm/engines/comfyui-minimax-h3-0.35.0-cu130}"
EXPECTED_TORCH="2.13.0+cu130"
EXPECTED_ARCH="aarch64"
CURRENT_PID=""

cleanup() { if [ -n "$CURRENT_PID" ] && kill -0 "$CURRENT_PID" 2>/dev/null; then kill -TERM "$CURRENT_PID" 2>/dev/null || true; fi; }
trap cleanup TERM INT
die() { echo "安装 MiniMax-H3 引擎失败：$*" >&2; exit 1; }
engine_ready() {
  [ -x "$ENGINE_DIR/runtime/bin/python" ] &&
  [ -f "$ENGINE_DIR/ComfyUI/main.py" ] &&
  [ -f "$ENGINE_DIR/ComfyUI/custom_nodes/ComfyUI-MiniMax-H3-Turbo/__init__.py" ] &&
  "$ENGINE_DIR/runtime/bin/python" -c 'import torch; assert torch.__version__ == "2.13.0+cu130"; assert torch.cuda.is_available(); assert torch.cuda.get_device_capability() == (12,1)' >/dev/null 2>&1
}

if [ "${LMM_PROBE_ONLY:-0}" = "1" ]; then
  engine_ready && exit 0
  echo "MiniMax-H3 ComfyUI 引擎尚未安装，或 GB10/CUDA 运行时版本不匹配。" >&2
  exit 1
fi

[ "$(uname -m)" = "$EXPECTED_ARCH" ] || die "需要 ARM64/aarch64，当前为 $(uname -m)。"
command -v nvidia-smi >/dev/null 2>&1 || die "找不到 nvidia-smi，请先安装 MegaCube NVIDIA 驱动。"
nvidia-smi --query-gpu=name --format=csv,noheader | grep -q 'GB10' || die "本卡片只在 NVIDIA GB10 上验证，请确认目标机器型号。"
if engine_ready; then
  echo "引擎已经就绪：$ENGINE_DIR"
  exit 0
fi

BASE_RUNTIME="${LMM_BASE_RUNTIME:-}"
if [ -z "$BASE_RUNTIME" ]; then
  for candidate in "$HOME/minimax-h3-runtime" "/home/$USER/minimax-h3-runtime"; do
    if [ -x "$candidate/bin/python" ]; then BASE_RUNTIME="$candidate"; break; fi
  done
fi
mkdir -p "$ENGINE_DIR"
if [ ! -x "$ENGINE_DIR/runtime/bin/python" ]; then
  if [ -n "$BASE_RUNTIME" ]; then
    [ -x "$BASE_RUNTIME/bin/python" ] || die "LMM_BASE_RUNTIME 无效：$BASE_RUNTIME/bin/python 不存在。"
    "$BASE_RUNTIME/bin/python" -c 'import torch,sys; assert sys.version_info[:2] == (3,12); assert torch.__version__ == "2.13.0+cu130"; assert torch.cuda.is_available(); assert torch.cuda.get_device_capability() == (12,1)' || die "基础运行时必须是 Python 3.12、PyTorch $EXPECTED_TORCH，并能识别 GB10 sm_121。"
    echo "正在复制已验证的 GB10 基础运行时……"
    cp -a --reflink=auto "$BASE_RUNTIME" "$ENGINE_DIR/runtime" & CURRENT_PID=$!
    while kill -0 "$CURRENT_PID" 2>/dev/null; do echo "引擎安装中：正在复制 Python/CUDA 运行时"; sleep 15; done
    wait "$CURRENT_PID" || die "复制基础运行时失败，请检查磁盘空间。"
    CURRENT_PID=""
    OLD_RUNTIME="$BASE_RUNTIME" NEW_RUNTIME="$ENGINE_DIR/runtime" "$ENGINE_DIR/runtime/bin/python" - <<'PY'
import os
from pathlib import Path
old, new = os.environ["OLD_RUNTIME"], os.environ["NEW_RUNTIME"]
for path in (Path(new) / "bin").iterdir():
    if path.is_symlink() or not path.is_file():
        continue
    try: text = path.read_text()
    except UnicodeDecodeError: continue
    if old in text: path.write_text(text.replace(old, new))
PY
  else
    command -v python3.12 >/dev/null 2>&1 || die "未找到基础运行时或 python3.12。请安装 Python 3.12，或用 LMM_BASE_RUNTIME 指定已验证环境。"
    echo "未发现可复用运行时，正在创建 Python 3.12 虚拟环境……"
    python3.12 -m venv "$ENGINE_DIR/runtime" || die "创建虚拟环境失败；请确认系统已安装 python3.12-venv。"
    "$ENGINE_DIR/runtime/bin/python" -m pip install --index-url https://download.pytorch.org/whl/cu130 \
      torch==2.13.0 torchvision==0.28.0 torchaudio==2.11.0 & CURRENT_PID=$!
    while kill -0 "$CURRENT_PID" 2>/dev/null; do echo "引擎安装中：正在下载 PyTorch CUDA 13.0 运行时"; sleep 15; done
    wait "$CURRENT_PID" || die "PyTorch CUDA 13.0 安装失败，请检查网络和磁盘空间。"
    CURRENT_PID=""
  fi
fi
"$ENGINE_DIR/runtime/bin/python" -c 'import torch,sys; assert sys.version_info[:2] == (3,12); assert torch.__version__ == "2.13.0+cu130"; assert torch.cuda.is_available(); assert torch.cuda.get_device_capability() == (12,1)' || die "引擎运行时版本不匹配。若此前安装中断，请删除 $ENGINE_DIR 后重试。"

echo "正在校验并展开锁定的 ComfyUI 源码……"
echo "e42f1265f60cad3e649a3422bda97a462fabbe989db799f6587bb22f781886ba  $PKG_DIR/sources/ComfyUI-source.tar.gz" | sha256sum -c - >/dev/null || die "ComfyUI 源码归档校验失败。"
echo "a1398cac5a703a37073e5f74f852243d99bb716cf82d1e10eaa3b7cedf290a39  $PKG_DIR/sources/ComfyUI-MiniMax-H3-Turbo-source.tar.gz" | sha256sum -c - >/dev/null || die "Turbo 节点源码归档校验失败。"
mkdir -p "$ENGINE_DIR/ComfyUI" "$ENGINE_DIR/ComfyUI/custom_nodes/ComfyUI-MiniMax-H3-Turbo"
tar -xzf "$PKG_DIR/sources/ComfyUI-source.tar.gz" --strip-components=1 -C "$ENGINE_DIR/ComfyUI"
tar -xzf "$PKG_DIR/sources/ComfyUI-MiniMax-H3-Turbo-source.tar.gz" --strip-components=1 -C "$ENGINE_DIR/ComfyUI/custom_nodes/ComfyUI-MiniMax-H3-Turbo"

cat > "$ENGINE_DIR/runtime-constraints.txt" <<'EOF'
torch==2.13.0
torchvision==0.28.0
torchaudio==2.11.0
comfyui-frontend-package==1.51.10
comfyui-workflow-templates==0.11.57
comfyui-embedded-docs==0.5.11
comfy-kitchen==0.2.33
comfy-aimdo==0.5.3
huggingface-hub==1.29.0
EOF
echo "正在安装 ComfyUI 依赖；网络较慢时会持续显示心跳……"
"$ENGINE_DIR/runtime/bin/python" -m pip install -c "$ENGINE_DIR/runtime-constraints.txt" -r "$ENGINE_DIR/ComfyUI/requirements.txt" & CURRENT_PID=$!
while kill -0 "$CURRENT_PID" 2>/dev/null; do echo "引擎安装中：pip 正在处理依赖"; sleep 15; done
wait "$CURRENT_PID" || die "ComfyUI 依赖安装失败，请检查 Python 软件源和网络。"
CURRENT_PID=""
"$ENGINE_DIR/runtime/bin/python" -m pip install -c "$ENGINE_DIR/runtime-constraints.txt" huggingface-hub==1.29.0 || die "huggingface-hub 1.29.0 安装失败。"
"$ENGINE_DIR/runtime/bin/python" -m pip uninstall -y sglang >/dev/null 2>&1 || true

ENGINE_DIR="$ENGINE_DIR" "$ENGINE_DIR/runtime/bin/python" - <<'PY'
import json, os
from pathlib import Path
root = Path(os.environ["ENGINE_DIR"]) / "ComfyUI"
p = root / "comfy_extras/nodes_minimax_h3.py"
s = p.read_text()
old = 'display_name="MiniMax H3 Image to Video"'
new = 'display_name=' + json.dumps('本地 · MiniMax H3 Image to Video', ensure_ascii=False)
if old in s: p.write_text(s.replace(old, new, 1))
elif new not in s: raise SystemExit("无法定位 MiniMax-H3 核心节点标签")
p = root / "custom_nodes/ComfyUI-MiniMax-H3-Turbo/__init__.py"
s = p.read_text()
labels = {'MiniMaxH3TurboLoRA':'本地 · MiniMax-H3 Turbo LoRA','MiniMaxH3TurboSampler':'本地 · MiniMax-H3 Turbo Sampler (4-step)'}
for key, label in labels.items():
    old = json.dumps(key) + ': ' + json.dumps(label.removeprefix('本地 · '))
    new = json.dumps(key) + ': ' + json.dumps(label, ensure_ascii=False)
    if old in s: s = s.replace(old, new)
    elif new not in s: raise SystemExit("无法定位 Turbo 节点标签：" + key)
p.write_text(s)
PY

"$ENGINE_DIR/runtime/bin/python" - <<'PY'
import importlib.metadata as m, torch
assert torch.__version__ == "2.13.0+cu130"
assert torch.cuda.is_available() and torch.cuda.get_device_capability() == (12,1)
for name, version in [('comfyui-frontend-package','1.51.10'),('comfyui-workflow-templates','0.11.57'),('comfy-kitchen','0.2.33')]:
    assert m.version(name) == version, (name, m.version(name))
x=torch.randn(128,128,device='cuda',dtype=torch.bfloat16); _=x@x
print('GB10 CUDA 验证通过：', torch.cuda.get_device_name(0), torch.__version__)
PY
cat > "$ENGINE_DIR/ENGINE_VERSION.json" <<EOF
{"card_id":"$CARD_ID","comfyui":"0.35.0","python":"3.12","pytorch":"2.13.0+cu130","architecture":"aarch64","gpu":"NVIDIA GB10","source_archive_sha256":{"ComfyUI":"e42f1265f60cad3e649a3422bda97a462fabbe989db799f6587bb22f781886ba","TurboNode":"a1398cac5a703a37073e5f74f852243d99bb716cf82d1e10eaa3b7cedf290a39"}}
EOF
engine_ready || die "安装完成后的引擎自检未通过。"
echo "MiniMax-H3 ComfyUI 引擎安装完成：$ENGINE_DIR"
