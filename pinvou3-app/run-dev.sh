#!/bin/bash
set -euo pipefail

cd "$(dirname "$0")"

OS_NAME="$(uname -s)"

# 注:源 workflows/ → bundle 嵌入快照的同步已移入 build.rs(任何 cargo build/打包都同步,
# 不再只覆盖 dev 启动,改完直接 build 也不漂移)。

# ── 工作流预检开关 ───────────────────────────────────────────────
# warmup_check 已对齐 app 配置(endpoint 由 harness 注入 / REQUIRED_ENVS 精简为 base_url),
# 默认启用预检、与客户机一致,尽早暴露 warmup 类问题。要跳过(省 vLLM 冷启动探活 ~30s)
# export PINVOU3_SKIP_WARMUP=1。
export PINVOU3_SKIP_WARMUP="${PINVOU3_SKIP_WARMUP:-0}"

# ── 远程 vLLM 连接(明文 HTTP,仅 Linux 内网机)─────────────────────
# 底座默认拒绝对非 loopback 的明文 http:// 发请求(client.rs validate_base_url_security),
# 且 reqwest 默认协议协商在某些网关下会 502。开发机连内网 GB10(http://10.214.74.113:8000)
# 必须显式放行明文 HTTP + 钉死 HTTP/1.1。可在外部 export 覆盖。
# macOS 走远程 HTTPS API,不需要这两项,跳过。
if [ "$OS_NAME" = "Linux" ]; then
  export DEEPSEEK_ALLOW_INSECURE_HTTP="${DEEPSEEK_ALLOW_INSECURE_HTTP:-1}"
  export DEEPSEEK_FORCE_HTTP1="${DEEPSEEK_FORCE_HTTP1:-1}"
fi

# ── L1 知识库语义检索：默认与正式版共用应用托管目录 ──────────────────
# 本地知识库和共享知识库宿主复用 ~/.pinvou3/knowledge/models/bge-m3。
# 仅需要外部只读模型夹具时，才在启动前显式设置
# PINVOU3_KB_EMBED_MODEL_DIR；开发入口不再覆盖应用的托管目录选择。

# ── 三省六部「网页类」预置模板 seed 源(dev)──────────────────────────
# 工部角色 `cp -r ~/.pinvou3/web-template ...` 的母版,首次启动从此处复制(prod 走随 deb 的
# resource_dir)。目录需含 package.json + 预装 node_modules(离线可 npm run build 出单文件)。
export PINVOU3_WEB_TEMPLATE_DIR="${PINVOU3_WEB_TEMPLATE_DIR:-$HOME/models/web-template}"

# ── 完整 WebUI v2 relay ──────────────────────────────────────────
# 仅本地开发启动默认走组内 remote-test 中继，避免调试版本误连生产 8787。
# 正式安装包不经过本脚本，使用 Rust 内置的 /pinvou3/remote；外部覆盖时 public
# 页面和 WebSocket 都必须保留 Relay 的公开 base path：
#   PINVOU_REMOTE_PUBLIC_URL=http://10.x.x.x:8787/pinvou3/remote
#   PINVOU_REMOTE_RELAY_WS_URL=ws://10.x.x.x:8787/pinvou3/remote/ws
export PINVOU_REMOTE_PUBLIC_URL="${PINVOU_REMOTE_PUBLIC_URL:-https://pinvou.com/pinvou3/remote-test}"
export PINVOU_REMOTE_RELAY_WS_URL="${PINVOU_REMOTE_RELAY_WS_URL:-wss://pinvou.com/pinvou3/remote-test/ws}"

# ── 编译器自身线程栈注入(仅编译期 rustc)────────────────────────
# v0.9.5 升级后 dev 对依赖开 O2 + 256 codegen-units 时,rustc/LLVM 编译
# codewhale-tui 会在 MachineLateinstrsCleanup 递归中栈溢出(SIGBUS,
# rustc 1.96/1.97 stable 稳定复现;std 线程默认栈三端均为 2 MiB,
# macOS 已实测触发,Linux 同源风险)。
# 经 RUSTC_WRAPPER 环境变量(优先级高于 .cargo/config.toml)注入
# scripts/rustc-stack-wrapper(带 shebang 的 sh,Unix 可执行),只在编译期
# rustc 进程注入 RUST_MIN_STACK=16MiB;cargo run / cargo test 启动的
# 应用与测试进程不经过 wrapper,不会继承该变量,默认线程栈语义不变。
# 平台选择由 scripts/rustc-stack-wrapper-select.sh 统一决定(run-dev.sh
# 与 CI smoke 共用同一 selector,避免入口与验证漂移):
#   - Darwin/Linux:注入 sh 版 wrapper(macOS 有 SIGBUS 实测,Linux 同源);
#   - Windows (MINGW*/MSYS*/CYGWIN*):selector 幂等编译并注入 .exe 版
#     wrapper(栈溢出根因三端同源,Windows 本地 dev 同样需要 16 MiB 栈;
#     .exe 经 CreateProcess 直启规避 cmd /C 的 8191 字符命令行上限,
#     无扩展名 sh 会 os error 193)。
# 契约测试 tests/compiler_stack_contract.rs 守护"运行时进程不继承"。
RUSTC_WRAPPER_VALUE="$(src-tauri/scripts/rustc-stack-wrapper-select.sh)"
if [ -n "$RUSTC_WRAPPER_VALUE" ]; then
  export RUSTC_WRAPPER="${RUSTC_WRAPPER:-$RUSTC_WRAPPER_VALUE}"
fi

# ── macOS 提示 ───────────────────────────────────────────────────
# Mac 不需要 webkit/fcitx/X11 相关 env(那些在 lib.rs RELEASE_ENV_DEFAULTS Linux 段)。
# 此处无需额外 Mac 专属 export,直接落到 tauri dev 即可。
# macOS dev 同样套用平台 overlay(原生红绿灯顶栏 titleBarStyle=Overlay)，
# Linux dev 动态生成隐藏启动 overlay；两者统一通过 build.js 注入，避免配置分叉。
if [ "$OS_NAME" = "Darwin" ]; then
  echo "✓ macOS dev 模式(跳过 Linux 内网 vLLM/WebKit env)"
fi

exec npm run dev -- "$@"
