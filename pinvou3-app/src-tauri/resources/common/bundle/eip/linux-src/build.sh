#!/usr/bin/env bash
# 从本 fallback 源码构建 eip CLI，落到 resources/platforms 对应目录。
#
# H3C IT 官方二进制只覆盖 Windows（eip-cli.exe）与 Linux x86_64（eip-cli），
# 原样打包、不经本脚本。以下目标由本源码构建（linux/arm64 存量二进制同出一源，
# 仅在源码变更后按需重建）：
#   ./build.sh                    全部自建目标
#   ./build.sh darwin-arm64 ...   仅指定目标（darwin-arm64 darwin-x64 linux-arm64）
set -euo pipefail

src_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$src_dir/../../../../../../.." && pwd)"
dest_root="$repo_root/pinvou3-app/src-tauri/resources/platforms"

targets="${*:-darwin-arm64 darwin-x64 linux-arm64}"
for target in $targets; do
  case "$target" in
    darwin-arm64)
      goos=darwin; goarch=arm64; out="$dest_root/macos/aarch64/bundle/eip/bin/eip-cli-aarch64" ;;
    darwin-x64)
      goos=darwin; goarch=amd64; out="$dest_root/macos/x86_64/bundle/eip/bin/eip-cli" ;;
    linux-arm64)
      goos=linux; goarch=arm64; out="$dest_root/linux/aarch64/bundle/eip/bin/eip-cli-aarch64" ;;
    *)
      echo "unknown target: $target（官方 windows/linux-x64 二进制不经本脚本构建）" >&2; exit 2 ;;
  esac
  mkdir -p "$(dirname "$out")"
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
    -o "$out" "$src_dir"
  # darwin/arm64 强制校验签名；Go 链接器对 darwin/amd64 不自动 ad-hoc 签名，补一道。
  if [ "$goos" = darwin ] && command -v codesign >/dev/null; then
    codesign --sign - --force "$out"
  fi
  echo "built $target -> $out"
done
