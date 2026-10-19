#!/usr/bin/env bash
# 从固定 SenseVoice.cpp commit 为当前 Linux 主机构建可发布的 ASR 引擎。
# 产物链接构建宿主 glibc,构建后校验会拒绝超出基线的符号:x86_64/aarch64 的
# 基线是 22.04 (glibc 2.35),发布 CI 在 ubuntu-22.04 runner 上构建。
#
# --arch loongarch64(deepin 23/25 + UOS V25,glibc 2.38 基线)是环境变量驱动的
# 交叉构建:不做 host==requested 检查,CMake 由指向 loongarch64 交叉编译器的
# CC/CXX 或 CMAKE_TOOLCHAIN_FILE 驱动,STRIP 指向目标 strip(宿主 strip 无法
# 处理异架构 ELF)。引擎静态链接 libstdc++/libgcc(deb 中唯一的 C++ 产物,
# 因此不引入 GLIBCXX_/CXXABI_ 要求),构建后在宿主上按 loongarch64 策略
# (GLIBC_ <= 2.38)校验。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=sensevoice-source.env
source "$SCRIPT_DIR/sensevoice-source.env"

usage() {
  echo "usage: $0 --output <path> --arch <x86_64|aarch64|loongarch64>" >&2
  exit 2
}

output_path=""
requested_arch=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --output)
      [ "$#" -ge 2 ] || usage
      output_path="$2"
      shift 2
      ;;
    --arch)
      [ "$#" -ge 2 ] || usage
      requested_arch="$2"
      shift 2
      ;;
    *) usage ;;
  esac
done
[ -n "$output_path" ] && [ -n "$requested_arch" ] || usage

case "$(uname -m)" in
  x86_64|amd64) host_arch="x86_64" ;;
  aarch64|arm64) host_arch="aarch64" ;;
  loongarch64) host_arch="loongarch64" ;;
  *) echo "unsupported Linux build architecture: $(uname -m)" >&2; exit 1 ;;
esac

case "$requested_arch" in
  x86_64) target_arch="x86_64"; policy_arch="amd64"; cross_mode=0 ;;
  aarch64) target_arch="aarch64"; policy_arch="arm64"; cross_mode=0 ;;
  # loongarch64: release CI cross-compiles on an x86_64/arm64 runner, so the
  # host==requested assertion below is skipped and the cross toolchain comes
  # from the environment instead (see the dependency pre-check and cmake
  # invocation).
  loongarch64) target_arch="loongarch64"; policy_arch="loongarch64"; cross_mode=1 ;;
  *) echo "unsupported requested SenseVoice architecture: $requested_arch" >&2; exit 1 ;;
esac

if [ "$cross_mode" -eq 0 ]; then
  [ "$requested_arch" = "$host_arch" ] || {
    echo "requested SenseVoice architecture $requested_arch does not match build host $host_arch" >&2
    exit 1
  }
fi

output_path="$(realpath -m "$output_path")"
output_dir="$(dirname "$output_path")"
# Build tree per target architecture: a cross loongarch64 build must not
# clobber or reuse a native x86_64/aarch64 tree (the values coincide for
# native builds).
build_root="${PINVOU3_SENSEVOICE_BUILD_ROOT:-$REPO_ROOT/pinvou3-app/src-tauri/target/sensevoice-build/$target_arch}"
source_dir="$build_root/source"
build_dir="$build_root/build"
build_info="$output_path.build-info"
mkdir -p "$output_dir" "$build_root"

validate_runtime() {
  "$REPO_ROOT/scripts/check-linux-elf-policy.sh" "$output_path" "$policy_arch"
}

cached_runtime_matches() {
  local expected_sha actual_sha
  expected_sha="$(sed -n 's/^sha256=//p' "$build_info")"
  [ -n "$expected_sha" ] || return 1
  actual_sha="$(sha256sum "$output_path" | awk '{print $1}')"
  [ "$actual_sha" = "$expected_sha" ]
}

if [ -x "$output_path" ] && [ -f "$build_info" ] \
  && grep -Fxq "commit=$SENSEVOICE_SOURCE_COMMIT" "$build_info" \
  && grep -Fxq "architecture=$target_arch" "$build_info" \
  && cached_runtime_matches \
  && validate_runtime; then
  echo "[sensevoice] 复用 $target_arch 运行时: $output_path"
  exit 0
fi

# 预检校验依赖:无法运行 check-linux-elf-policy.sh 的环境应在完整编译前
# 失败,而不是编译完才在 validate_runtime 处失败。原生构建需要宿主 g++;
# 交叉构建的编译器来自环境,宿主 strip 无法处理异架构 ELF,所以 STRIP
# 指向目标 strip(默认按钉死交叉工具链的 binutils 命名,Debian 交叉
# binutils 的命名可通过 STRIP 覆盖继续使用)。
if [ "$cross_mode" -eq 1 ]; then
  for command_name in cmake git make sha256sum dpkg file objdump readelf; do
    command -v "$command_name" >/dev/null || {
      echo "missing SenseVoice build dependency: $command_name" >&2
      exit 1
    }
  done
  # 环境驱动的工具链只在真正交叉构建时强制;loongarch64 宿主上原生编译
  # 工具链就够。
  if [ "$requested_arch" != "$host_arch" ] \
    && [ -z "${CMAKE_TOOLCHAIN_FILE:-}" ] && [ -z "${CC:-}" ] && [ -z "${CXX:-}" ]; then
    echo "loongarch64 cross build requires CC/CXX (loongarch64 cross compilers) or CMAKE_TOOLCHAIN_FILE in the environment" >&2
    exit 1
  fi
  # A native loongarch64 host uses its own strip; a real cross build needs
  # the target's strip (host strip cannot handle foreign-arch ELFs).
  # Default spelling matches the pinned cross toolchain's binutils
  # (loongarch64-unknown-linux-gnu-*); the Debian cross-binutils spelling
  # keeps working via STRIP override.
  if [ "$requested_arch" = "$host_arch" ]; then
    strip_bin="${STRIP:-strip}"
  else
    strip_bin="${STRIP:-loongarch64-unknown-linux-gnu-strip}"
  fi
else
  for command_name in cmake g++ git make sha256sum dpkg file objdump readelf; do
    command -v "$command_name" >/dev/null || {
      echo "missing SenseVoice build dependency: $command_name" >&2
      exit 1
    }
  done
  strip_bin="strip"
fi
command -v "$strip_bin" >/dev/null || {
  echo "missing SenseVoice build dependency: $strip_bin" >&2
  exit 1
}

source_tmp="$build_root/source.tmp.$$"
rm -rf -- "$source_tmp"
trap 'rm -rf -- "${source_tmp:-}" "${output_tmp:-}"' EXIT
git init --quiet "$source_tmp"
git -C "$source_tmp" remote add origin "$SENSEVOICE_SOURCE_URL"
git -C "$source_tmp" fetch --quiet --depth 1 origin "$SENSEVOICE_SOURCE_COMMIT"
git -C "$source_tmp" checkout --quiet --detach FETCH_HEAD
# 复核浅克隆检出的正是 pinned commit（与 setup-sensevoice.sh 同一道 tripwire）。
test "$(git -C "$source_tmp" rev-parse HEAD)" = "$SENSEVOICE_SOURCE_COMMIT" \
  || { echo "pinned commit checkout mismatch: expected $SENSEVOICE_SOURCE_COMMIT, got $(git -C "$source_tmp" rev-parse HEAD)" >&2; exit 1; }
git -C "$source_tmp" submodule update --init --recursive --depth 1
rm -rf -- "$source_dir" "$build_dir"
mv "$source_tmp" "$source_dir"

# GGML_NATIVE=OFF 时钉死的 ggml 仍默认 AVX2/FMA 为 ON,x86_64 引擎因此带
# Haswell+ (AVX2) 下限:更老的 CPU 会在推理时 SIGILL。作为发布基线接受。
#
# loongarch64:钉死的 ggml(SenseVoice.cpp submodule, b2a092a7)原生支持
# loongarch64 —— ggml_get_system_arch 按 CMAKE_SYSTEM_PROCESSOR 匹配
# "loongarch64" 推导 GGML_SYSTEM_ARCH,进而加入 ggml-cpu/arch/loongarch/quants.c、
# -march=loongarch64,并在 GGML_LASX/GGML_LSX 开启(均默认 ON;新世界
# LoongArch64 硬件是 3A5000+,实现 LSX/LASX)时加 -mlasx/-mlsx。普通 CC/CXX
# 交叉配置会让 CMAKE_SYSTEM_PROCESSOR 停留在构建宿主的值,静默丢掉 loongarch
# 后端,所以交叉模式显式传入它(外加 CMAKE_SYSTEM_NAME=Linux);调用方提供的
# toolchain file 则由其自行负责设置这两项。没有单独的 GGML_LOONGARCH64 开关
# 可关。
cmake_args=(
  -S "$source_dir" -B "$build_dir"
  -DCMAKE_BUILD_TYPE=Release
  -DBUILD_SHARED_LIBS=OFF
  -DSENSE_VOICE_BUILD_EXAMPLES=OFF
  -DSENSE_VOICE_BUILD_TESTS=OFF
  -DSENSE_VOICE_CCACHE=OFF
  -DGGML_NATIVE=OFF
  -DGGML_CUDA=OFF
  -DGGML_VULKAN=OFF
  -DGGML_KOMPUTE=OFF
)
if [ "$cross_mode" -eq 1 ]; then
  # The engine is the deb's only C++ artifact: link libstdc++/libgcc
  # statically so it carries no GLIBCXX_/CXXABI_ version requirements and the
  # jammy GLIBCXX_/CXXABI_ floors hold for every architecture.
  cmake_args+=(-DCMAKE_EXE_LINKER_FLAGS="-static-libstdc++ -static-libgcc")
  if [ -n "${CMAKE_TOOLCHAIN_FILE:-}" ]; then
    cmake_args+=(-DCMAKE_TOOLCHAIN_FILE="$CMAKE_TOOLCHAIN_FILE")
  else
    cmake_args+=(
      -DCMAKE_SYSTEM_NAME=Linux
      -DCMAKE_SYSTEM_PROCESSOR="$requested_arch"
    )
  fi
fi
cmake "${cmake_args[@]}"
cmake --build "$build_dir" --target sense-voice-main \
  --parallel "${PINVOU3_SENSEVOICE_BUILD_JOBS:-4}"

output_tmp="$output_path.tmp.$$"
cp "$build_dir/bin/sense-voice-main" "$output_tmp"
"$strip_bin" --strip-unneeded "$output_tmp"
chmod 0755 "$output_tmp"
mv "$output_tmp" "$output_path"
validate_runtime

sha256="$(sha256sum "$output_path" | awk '{print $1}')"
printf 'source=%s\ncommit=%s\narchitecture=%s\nsha256=%s\n' \
  "$SENSEVOICE_SOURCE_URL" "$SENSEVOICE_SOURCE_COMMIT" "$target_arch" "$sha256" \
  > "$build_info"
echo "[sensevoice] 已构建 $target_arch 运行时: $output_path ($sha256)"
