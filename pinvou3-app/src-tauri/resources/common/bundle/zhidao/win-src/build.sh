#!/usr/bin/env bash
# 从本清洁室源码构建 zhidao CLI 的全部自建目标，落到 resources/platforms 对应目录。
#
# H3C IT 只发布 Linux x86_64 ELF，原样打包、不经本脚本；以下目标均由本源码
# 构建，源码变更后须随本脚本重建：
#   ./build.sh                    全部自建目标
#   ./build.sh darwin-arm64 ...   仅指定目标（darwin-arm64 darwin-x64 windows-amd64 linux-arm64）
set -euo pipefail

src_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$src_dir/../../../../../../.." && pwd)"
dest_root="$repo_root/pinvou3-app/src-tauri/resources/platforms"

targets="${*:-darwin-arm64 darwin-x64 windows-amd64 linux-arm64}"
for target in $targets; do
  case "$target" in
    darwin-arm64)
      goos=darwin; goarch=arm64; out="$dest_root/macos/aarch64/bundle/zhidao/bin/zhidao-cli-aarch64" ;;
    darwin-x64)
      goos=darwin; goarch=amd64; out="$dest_root/macos/x86_64/bundle/zhidao/bin/zhidao-cli" ;;
    windows-amd64)
      goos=windows; goarch=amd64; out="$dest_root/windows/x86_64/bundle/zhidao/bin/zhidao-cli.exe" ;;
    linux-arm64)
      goos=linux; goarch=arm64; out="$dest_root/linux/aarch64/bundle/zhidao/bin/zhidao-cli-aarch64" ;;
    *)
      echo "unknown target: $target" >&2; exit 2 ;;
  esac
  mkdir -p "$(dirname "$out")"
  # -buildvcs=false：不嵌入 vcs 指纹，避免脏工作区构建产生误导性的
  # vcs.revision/vcs.modified 标记；二进制溯源以提交记录为准。
  GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
    go build -trimpath -buildvcs=false -ldflags "-s -w" \
    -o "$out" "$src_dir"
  # darwin 两个架构统一补 ad-hoc 签名（Go 链接器只自动签 darwin/arm64，--force 幂等）。
  if [ "$goos" = darwin ] && command -v codesign >/dev/null; then
    codesign --sign - --force "$out"
  fi
  echo "built $target -> $out"
done
