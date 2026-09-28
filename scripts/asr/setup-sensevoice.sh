#!/usr/bin/env bash
# 在 Linux 上搭建 pinvou3 本地语音识别引擎(SenseVoice.cpp)+ 模型到 ~/.pinvou3/asr/。
#
# 这是 POC 验证过的搭建流程，也是将来「按需下载器」的基础：把同样的
# 引擎二进制 + gguf 模型 + shim 落到 ~/.pinvou3/asr/，app 设 PINVOU3_ASR_CMD
# 指向 shim 即可用本地语音输入。
#
# 用法:
#   scripts/asr/setup-sensevoice.sh [q4_k|q8_0]      # 量化档,默认 q4_k(174MB)
#   GGML_CUDA=ON scripts/asr/setup-sensevoice.sh     # GB10 等带 GPU 的机器开 CUDA 加速
#
# 依赖: git / gcc / g++ / make / sha256sum（apt install build-essential git; sha256sum 属 coreutils）; ffmpeg(转码,建议)。
#       cmake 缺失会自动下预编译(免 root);复用系统 cmake 需 ≥ 3.12(构建走 `cmake --build --parallel`)。
set -euo pipefail

QUANT="${1:-q4_k}"
CMAKE_VERSION="4.4.2"
ASR_DIR="$HOME/.pinvou3/asr"
MODEL_FILE="sense-voice-small-${QUANT}.gguf"
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=sensevoice-source.env
source "$HERE/sensevoice-source.env"
WORK="$(mktemp -d)"
# 退出时清理 WORK 与已创建的落位临时文件;engine_tmp 等变量在后续步骤才
# 赋值,单引号使 trap 在退出时才展开,${var:-} 保证中途失败时 set -u 不炸。
trap 'rm -rf "$WORK"
  [ -z "${engine_tmp:-}" ] || rm -f "$engine_tmp"
  [ -z "${model_tmp:-}" ] || rm -f "$model_tmp"
  [ -z "${shim_tmp:-}" ] || rm -f "$shim_tmp"' EXIT

case "$QUANT" in
  q4_k) MODEL_SHA256="c8e7bf77acd860c5b83d2106da44aa7b985026ef4e7dbf5236c7f0f4001d9e9b" ;;
  q8_0) MODEL_SHA256="f92beb119d07e42a96e3fbe6fbbb172910026f26b724c2b10fd75654c23d6912" ;;
  *) echo "❌ 不支持的量化档: $QUANT（仅支持 q4_k / q8_0）" >&2; exit 2 ;;
esac

echo "[1/5] 检查依赖…"
for t in git gcc g++ make curl sha256sum; do
  command -v "$t" >/dev/null || { echo "❌ 缺 $t — 请先: sudo apt install build-essential git curl" >&2; exit 1; }
done
command -v ffmpeg >/dev/null || echo "⚠️  缺 ffmpeg(浏览器录音转码用) — 建议: sudo apt install ffmpeg"
if command -v cmake >/dev/null; then
  CMAKE=cmake
else
  echo "    cmake 缺失，下载预编译(免 root)…"
  case "$(uname -m)" in
    aarch64) CMAKE_SHA256="9ca1aadb4451c5dcbdc67f9b4aff42dab52abbaebd8db9e2900026502dbed671" ;;
    x86_64) CMAKE_SHA256="3ada9a3f5d8a85413579bdd0ea6aa8e8da86efdd6d15c91a1afa517f2021956c" ;;
    *) echo "❌ cmake 自动下载暂不支持架构: $(uname -m)" >&2; exit 1 ;;
  esac
  curl --retry 4 --retry-all-errors -fsSL -o "$WORK/cmake.tgz" \
    "https://github.com/Kitware/CMake/releases/download/v${CMAKE_VERSION}/cmake-${CMAKE_VERSION}-linux-$(uname -m).tar.gz"
  printf '%s  %s\n' "$CMAKE_SHA256" "$WORK/cmake.tgz" | sha256sum --check --status \
    || { echo "❌ CMake 预编译包 sha256 校验失败（下载损坏或上游产物已变更）: 期望 $CMAKE_SHA256，实际 $(sha256sum "$WORK/cmake.tgz" | cut -d' ' -f1)" >&2; exit 1; }
  tar xzf "$WORK/cmake.tgz" -C "$WORK"
  # head -n 1 防多匹配。解压结果不能写成裸赋值:grep 无匹配时其状态码会在
  # 任何护栏执行前就触发 set -e/pipefail 退出,守卫形同虚设,所以先用
  # `|| true` 接住再显式检查。
  cmake_dir="$(ls "$WORK" | grep '^cmake-' | head -n 1 || true)"
  if [[ -z "$cmake_dir" || ! -x "$WORK/$cmake_dir/bin/cmake" ]]; then
    echo "❌ CMake 预编译包解压后未找到可执行的 bin/cmake（上游资产布局可能已变化）: $WORK/${cmake_dir:-<no cmake-* directory>}/bin/cmake" >&2
    exit 1
  fi
  CMAKE="$WORK/$cmake_dir/bin/cmake"
fi

echo "[2/5] 构建 SenseVoice.cpp@$SENSEVOICE_SOURCE_COMMIT（CUDA=${GGML_CUDA:-OFF}）…"
git init --quiet "$WORK/sv"
git -C "$WORK/sv" remote add origin "$SENSEVOICE_SOURCE_URL"
git -C "$WORK/sv" fetch --quiet --depth 1 origin "$SENSEVOICE_SOURCE_COMMIT"
git -C "$WORK/sv" checkout --quiet --detach FETCH_HEAD
test "$(git -C "$WORK/sv" rev-parse HEAD)" = "$SENSEVOICE_SOURCE_COMMIT" \
  || { echo "❌ pinned commit 复核失败：检出 $(git -C "$WORK/sv" rev-parse HEAD)，期望 $SENSEVOICE_SOURCE_COMMIT" >&2; exit 1; }
git -C "$WORK/sv" submodule update --init --recursive --depth 1
mkdir -p "$WORK/sv/build"
# 静态链接(BUILD_SHARED_LIBS=OFF):WORK 退出即删,只拷出 sense-voice-main
# 一个文件,共享构建会让已装引擎指向随后消失的 so。与
# build-sensevoice-runtime.sh 同参,但 GGML_NATIVE 默认 ON——本构建只跑在
# 使用它的那台机器上。
( cd "$WORK/sv/build" \
  && "$CMAKE" -DCMAKE_BUILD_TYPE=Release -DBUILD_SHARED_LIBS=OFF \
       -DSENSE_VOICE_BUILD_EXAMPLES=OFF -DSENSE_VOICE_BUILD_TESTS=OFF \
       -DGGML_NATIVE="${GGML_NATIVE:-ON}" -DGGML_KOMPUTE=OFF -DGGML_VULKAN=OFF \
       -DGGML_CUDA="${GGML_CUDA:-OFF}" .. \
  && "$CMAKE" --build . --target sense-voice-main --parallel "$(nproc)" )

echo "[3/5] 安装引擎 → $ASR_DIR …"
mkdir -p "$ASR_DIR"
# 上次被 SIGKILL(OOM、手动 kill -9)的运行不会触发 EXIT trap,会在安装目录
# 留下同目录临时文件;shim 会忽略它们,但 174-292MB 的模型临时文件是实打实
# 的磁盘浪费,落位前先清扫。
rm -f "$ASR_DIR"/.*.tmp.*
# 同目录临时文件 + rename 落位：拷贝中途被打断不能在最终路径留下截断文件
# （shim 只检查文件存在，截断的引擎/模型/shim 要到运行期才会暴露）。
engine_tmp="$ASR_DIR/.sense-voice-main.tmp.$$"
cp "$WORK/sv/build/bin/sense-voice-main" "$engine_tmp" || { rm -f "$engine_tmp"; exit 1; }
mv -f "$engine_tmp" "$ASR_DIR/sense-voice-main"

echo "[4/5] 下载模型 $MODEL_FILE（modelscope，国内可达）…"
curl --retry 4 --retry-all-errors -fsSL -o "$WORK/$MODEL_FILE" \
  "https://www.modelscope.cn/models/lovemefan/SenseVoiceGGUF/resolve/master/$MODEL_FILE"
printf '%s  %s\n' "$MODEL_SHA256" "$WORK/$MODEL_FILE" | sha256sum --check --status \
  || { echo "❌ 模型 sha256 校验失败（下载损坏或上游产物已变更）: $MODEL_FILE（期望 $MODEL_SHA256，实际 $(sha256sum "$WORK/$MODEL_FILE" | cut -d' ' -f1)）" >&2; exit 1; }
model_tmp="$ASR_DIR/.$MODEL_FILE.tmp.$$"
install -m 0644 "$WORK/$MODEL_FILE" "$model_tmp" || { rm -f "$model_tmp"; exit 1; }
mv -f "$model_tmp" "$ASR_DIR/$MODEL_FILE"

echo "[5/5] 安装 shim …"
shim_tmp="$ASR_DIR/.pinvou3-asr-shim.py.tmp.$$"
cp "$HERE/pinvou3-asr-shim.py" "$shim_tmp" || { rm -f "$shim_tmp"; exit 1; }
chmod +x "$shim_tmp"
mv -f "$shim_tmp" "$ASR_DIR/pinvou3-asr-shim.py"

echo
echo "✅ 完成。引擎/模型/shim 已装到 $ASR_DIR"
echo
echo "接入 pinvou3（启动 app 时带环境变量）:"
echo "   PINVOU3_ASR_CMD=$ASR_DIR/pinvou3-asr-shim.py \\"
[ "$QUANT" = "q4_k" ] || echo "   SV_MODEL=$ASR_DIR/$MODEL_FILE \\"
echo "   ./pinvou3-app/run-dev.sh"
echo
echo "然后点麦克风录音即可。诊断日志: $ASR_DIR/shim.log"
