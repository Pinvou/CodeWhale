#!/usr/bin/env bash
# 从本 fallback 源码构建 eip CLI，落到 resources/platforms 对应目录。
#
# **五个平台全部由本源码构建**（Windows x64 / Linux x64 / Linux arm64 /
# macOS arm64 / macOS x64）。iClaw 官方只发 Windows 与 Linux x86_64 两份，
# 且已不可用：SSO 服务端把登录响应 `data.pollInterval` 由 string 改为 number 后，
# 其 Go struct 反序列化必失败（auth login 与 AIT 重登录全挂，报文误导读检查网络）；
# 本实现按 `json.RawMessage` 宽容解析，对字段类型变化天然兼容，故不再打包官方二进制。
#
# 源码变更后须整批重建（漏一个平台就会让该端停在旧行为）：
#   ./build.sh                    全部目标
#   ./build.sh linux-x64 ...      仅指定目标
#   目标名：windows-x64 linux-x64 linux-arm64 darwin-arm64 darwin-x64
#
# 注：并行编译（go 默认 -p=<CPU 数>）在低内存/高提交占用的机器上会让编译器
# 子进程 OOM（"fatal error: out of memory allocating heap arena map"），
# 此时用 `./build.sh ... ` 前加 `GOFLAGS=-p=1` 串行构建。
set -euo pipefail

src_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$src_dir/../../../../../../.." && pwd)"
dest_root="$repo_root/pinvou3-app/src-tauri/resources/platforms"

targets="${*:-windows-x64 linux-x64 linux-arm64 darwin-arm64 darwin-x64}"
for target in $targets; do
  case "$target" in
    windows-x64)
      goos=windows; goarch=amd64; out="$dest_root/windows/x86_64/bundle/eip/bin/eip-cli.exe" ;;
    linux-x64)
      goos=linux; goarch=amd64; out="$dest_root/linux/x86_64/bundle/eip/bin/eip-cli" ;;
    darwin-arm64)
      goos=darwin; goarch=arm64; out="$dest_root/macos/aarch64/bundle/eip/bin/eip-cli-aarch64" ;;
    darwin-x64)
      goos=darwin; goarch=amd64; out="$dest_root/macos/x86_64/bundle/eip/bin/eip-cli" ;;
    linux-arm64)
      goos=linux; goarch=arm64; out="$dest_root/linux/aarch64/bundle/eip/bin/eip-cli-aarch64" ;;
    *)
      echo "unknown target: $target（目标名：windows-x64 linux-x64 linux-arm64 darwin-arm64 darwin-x64）" >&2; exit 2 ;;
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
