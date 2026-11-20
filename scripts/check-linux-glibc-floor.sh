#!/usr/bin/env bash
# 解包发布 deb 后校验其中全部 ELF 的目标架构和动态符号版本基线;
# 策略本身在 check-linux-elf-policy.sh 中。
set -euo pipefail

usage() {
  echo "usage: $0 <deb> <amd64|arm64|loongarch64> [glibc-floor]" >&2
  exit 2
}

deb_path="${1:-}"
expected_arch="${2:-}"
glibc_floor="${3:-}"
[ -n "$deb_path" ] && [ -f "$deb_path" ] && [ -n "$expected_arch" ] || usage

# Per-arch default floor (same map as check-linux-elf-policy.sh): jammy
# glibc 2.35 for amd64/arm64, 2.38 for loongarch64 (deepin 23/25 + UOS V25).
# Validated here too, so a wrong arch name fails before the deb extraction.
case "$expected_arch" in
  amd64|arm64) glibc_floor="${glibc_floor:-2.35}" ;;
  loongarch64) glibc_floor="${glibc_floor:-2.38}" ;;
  *) usage ;;
esac

command -v dpkg-deb >/dev/null || { echo "missing required command: dpkg-deb" >&2; exit 1; }

extract_dir="$(mktemp -d)"
trap 'rm -rf -- "$extract_dir"' EXIT
package_arch="$(dpkg-deb -f "$deb_path" Architecture)"
# loong 系口径统一：deb control 字段是 loong64（package-deb-loongarch64.sh 的
# DEB_ARCH），而 Rust target/调用方习惯写 loongarch64——两侧任一口径都接受。
normalize_arch() { case "$1" in loongarch64) echo loong64 ;; *) echo "$1" ;; esac; }
[ "$(normalize_arch "$package_arch")" = "$(normalize_arch "$expected_arch")" ] || {
  echo "FAIL: deb architecture $package_arch != expected $expected_arch" >&2
  exit 1
}
dpkg-deb -x "$deb_path" "$extract_dir"
# A desktop ELF alone is insufficient: supported packages must carry the full
# cmsd component at the resource path used by Tauri, with matching metadata.
python3 "$(cd "$(dirname "$0")" && pwd)/check-cmsd-bundled-package.py" \
  "$extract_dir" "$expected_arch"
"$(cd "$(dirname "$0")" && pwd)/check-linux-elf-policy.sh" \
  "$extract_dir" "$expected_arch" "$glibc_floor"
