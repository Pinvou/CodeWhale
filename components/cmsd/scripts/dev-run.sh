#!/bin/bash
# cmsd 开发启动脚本：先配置独立 state/socket/unit/cards，前台运行。
# 用法：CMSD_CONFIG=/absolute/dev/cmsd.toml scripts/dev-run.sh
set -euo pipefail
cd "$(dirname "$0")/.."
export CMSD_CONFIG="${CMSD_CONFIG:-$PWD/.dev/cmsd.toml}"
if [[ ! -f "$CMSD_CONFIG" ]]; then
    echo '请先创建独立开发配置，并通过 CMSD_CONFIG 指定；不会回退用户服务默认配置。' >&2
    exit 2
fi
# GB10 shares system/GPU memory; keep an explicit operator override available.
CARGO_BUILD_JOBS="${CARGO_BUILD_JOBS:-1}" cargo build
./target/debug/cmsd --check-config
exec ./target/debug/cmsd
