#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
APP_DIR="$(cd -- "$SCRIPT_DIR/.." && pwd)"

OS_NAME="$(uname -s)"
case "$OS_NAME" in
  Linux)
    PLATFORM_DIR="linux"
    ;;
  Darwin)
    PLATFORM_DIR="macos"
    ;;
  *)
    echo "当前 Bridge 构建脚本仅支持 Linux/macOS" >&2
    exit 1
    ;;
esac
OUT_DIR="$APP_DIR/src-tauri/resources/platforms/$PLATFORM_DIR/codex-bridge"

# BSD 工具(macOS)不支持 GNU 风格的 `--` 参数分隔符;脚本内路径无空格风险可控,
# 按 OS 决定是否携带。
if [ "$OS_NAME" = "Darwin" ]; then
  DD=""
else
  DD="--"
fi

NODE_VERSION="24.20.0"
CODEX_ACP_VERSION="1.6.2"
CODEX_ACP_PACKAGE="@agentclientprotocol/codex-acp"
CLAUDE_ACP_VERSION="0.70.0"
CLAUDE_ACP_PACKAGE="@agentclientprotocol/claude-agent-acp"
CLAUDE_SDK_VERSION="0.3.232"
BRIDGE_PACKAGE_DIR="$SCRIPT_DIR/codex-bridge-runtime"

# 从 manifest.json 里读一个字符串字段，把值打到标准输出。
#
# 退出码只区分「文件不存在或为空」（返回 1，输出空串）这一种；文件在、但里面没有这个
# 键时 sed 找不到匹配，照样返回 **0** 并输出空串。所以调用方一律比对输出串，不要拿
# 退出码当「读到了没有」用——下面几处 `[ "$(manifest_field …)" = "$X" ]` 就是这么写的。
#
# 两段式，不用反向引用：先砍掉 `"<key>": "` 之前的一切，再砍掉右引号之后的。
# sed 的反向引用穿过多层引号太容易被吃掉，而且吃掉之后是**静默**取到空串——
# 那会让调用方的比对永远判失败，退化成每次都重建，且没人看得出来。
manifest_field() {
  local manifest="$1"
  local key="$2"
  [ -s "$manifest" ] || return 1
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"//p" "$manifest" \
    | head -n 1 \
    | sed 's/".*//'
}

bridge_runtime_valid() {
  local root="$1"
  local node
  # 本 PR 前生成的旧 staging 可能仍残留 Claude 平台原生二进制；发现即视为无效，
  # 强制重打包，避免本地复用旧产物时把约 245MB 的二进制重新打进安装包。
  if find "$root/acp/node_modules/@anthropic-ai" -maxdepth 1 -mindepth 1 \
    -type d -name 'claude-agent-sdk-*' -print -quit 2>/dev/null | grep -q .; then
    return 1
  fi
  local entry="$root/acp/node_modules/@agentclientprotocol/codex-acp/dist/index.js"
  local claude_entry="$root/acp/node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js"
  local package_json="$root/acp/node_modules/@agentclientprotocol/codex-acp/package.json"
  local claude_package_json="$root/acp/node_modules/@agentclientprotocol/claude-agent-acp/package.json"
  local npm_cli="$root/node/lib/node_modules/npm/bin/npm-cli.js"
  local version_output

  # 已有产物必须是**目标架构**的。
  #
  # 没有这一条就有个很隐蔽的坑：先做一次宿主构建（staging 里是 x64 node），
  # 再做交叉构建，下面的执行检查会**通过**（x64 node 在 x64 宿主上跑得起来），
  # 脚本打印「already ready」直接退出，于是 arm64 包里装进了 x64 的 node——
  # 正是本次要修的那个 bug 换条路径重现。
  #
  # 用 manifest.json 里已有的 arch 字段，不另造标记文件：它本来就记着这个值
  # （见文件末尾生成 manifest 那段），多一个标记就多一处会不同步的地方。
  # darwin 打的是 universal（两个架构都在包里），不参与这个比对。
  #
  # manifest 读不出来（缺失或字段对不上）一律判无效：宁可多重建一次，也不能拿
  # 一份来路不明的产物当 ready。
  if [ -n "${NODE_CPU:-}" ] && [ "$OS_NAME" != "Darwin" ]; then
    [ "$(manifest_field "$root/manifest.json" arch)" = "$NODE_CPU" ] || return 1
  fi
  case "$OS_NAME-$(uname -m)" in
    Linux-x86_64|Linux-aarch64|Linux-arm64)
      node="$root/node/bin/node"
      ;;
    Darwin-arm64)
      node="$root/node/darwin-arm64/bin/node"
      [ -x "$root/node/darwin-x64/bin/node" ] \
        && /usr/bin/lipo "$node" -verify_arch arm64 \
        && /usr/bin/lipo "$root/node/darwin-x64/bin/node" -verify_arch x86_64 || return 1
      ;;
    Darwin-x86_64)
      node="$root/node/darwin-x64/bin/node"
      [ -x "$root/node/darwin-arm64/bin/node" ] \
        && /usr/bin/lipo "$root/node/darwin-arm64/bin/node" -verify_arch arm64 \
        && /usr/bin/lipo "$node" -verify_arch x86_64 || return 1
      ;;
    *)
      return 1
      ;;
  esac

  [ -x "$node" ] && [ -s "$npm_cli" ] && [ -s "$entry" ] && [ -s "$package_json" ] \
    && [ -s "$claude_entry" ] && [ -s "$claude_package_json" ] || return 1

  # 下面两处检查跑的是**纯 JS**（ACP 包的 --version、读 package.json），拿哪个
  # 解释器跑都一样，但解释器本身得能在**这台机器**上执行：
  # - 宿主那份 node 已就位（下载之后的两次调用）：用它，最稳。
  # - 还没就位但不交叉：包里那份就是宿主架构的，跑得起来。macOS 打的是
  #   universal，上面的 case 已按 uname -m 挑了宿主那一份，同样跑得起来。
  # - 还没就位且是交叉（脚本开头那次快路径调用）：包里那份是目标架构的，一跑
  #   就 ENOEXEC。这里留空，改走下面不执行二进制的那条。
  local runner=""
  if [ -n "${NPM_NODE_ROOT:-}" ] && [ -x "$NPM_NODE_ROOT/bin/node" ]; then
    runner="$NPM_NODE_ROOT/bin/node"
  elif [ "$OS_NAME" = "Darwin" ] || [ "${NODE_TARGET:-}" = "${HOST_NODE_TARGET:-}" ]; then
    runner="$node"
  fi

  if [ -z "$runner" ]; then
    # 交叉构建的快路径：改成拿 manifest 里记着的版本跟脚本常量比对，不执行任何
    # 二进制。原先这里必然回落到包里那份目标架构的 node，`--version` 直接
    # ENOEXEC，于是「already ready」在交叉模式下是条**死分支**——本地第二次起的
    # 每次交叉构建都白付一趟「下两份 tarball + 校 sha256 + npm ci」。
    #
    # 这样不会把坏产物误判成 ready：manifest.json 是整个构建最后才落盘的——先
    # npm ci、剪掉平台原生二进制，再写 manifest，再跑一遍**会执行**的完整校验，
    # 最后才把 node / acp / manifest 依次 mv 进 OUT_DIR（manifest 是三次 mv 里的
    # 最后一次）。所以 OUT_DIR 里躺着一份版本对得上的 manifest，本身就是「这份
    # 产物当时通过了执行校验，且三次 mv 都做完了」的凭证。
    #
    # 三个版本都比对（Node、两个 ACP 包）：manifest 里既然都记着，少比一个就留一个
    # 「单 bump 该版本时旧 staging 被判 ready」的口子。这个凭证只覆盖构建时序——
    # 两次构建之间 OUT_DIR 里的文件被外部改动（内容损坏但 [ -s ] 仍过）是不设防的；
    # 执行校验只在「本次构建刚备完料」那一次全量跑过。
    [ "$(manifest_field "$root/manifest.json" node_version)" = "$NODE_VERSION" ] \
      || return 1
    [ "$(manifest_field "$root/manifest.json" codex_acp_version)" = "$CODEX_ACP_VERSION" ] \
      || return 1
    [ "$(manifest_field "$root/manifest.json" claude_acp_version)" = "$CLAUDE_ACP_VERSION" ] \
      || return 1
    return 0
  fi

  version_output="$(
    env CODEX_PATH="$(command -v codex || true)" \
      "$runner" "$entry" --version 2>/dev/null
  )" || return 1
  [ "$version_output" = "$CODEX_ACP_PACKAGE $CODEX_ACP_VERSION" ] || return 1
  local claude_version
  claude_version="$(
    "$runner" -e 'process.stdout.write(require(process.argv[1]).version)' "$claude_package_json"
  )" || return 1
  [ "$claude_version" = "$CLAUDE_ACP_VERSION" ]
}

# ---- 目标架构 vs 宿主架构 ----
#
# 交叉构建时这两者不同，而 Bridge 里要打进包的是**目标架构**的 Node。原先整段
# 只看 `uname -m`，于是 x64 宿主交叉出的 arm64 包里躺着一个 x86_64 的 node，
# 装上去一用就 ENOEXEC，而构建过程一句警告都没有。
#
# 跟 SenseVoice 不同，Bridge 是**可以**为另一个架构备料的：它不编译任何东西，
# 只是下一个 Node 官方 tarball、校验 sha256、解压。唯一的障碍是 `npm ci` 要用
# 一个能在**宿主**上执行的 node 去跑——所以交叉时下两份：宿主那份用来跑 npm，
# 目标那份打进包。npm 本来就收 `--os/--cpu/--libc`（下面 npm_ci_for_target），
# 按目标平台解依赖这件事它自己支持。
#
# macOS 那条分支早就在下两份（darwin-arm64 + darwin-x64）并都打进包，所以
# 「下载多份、挑一份跑」这个形态本脚本里是现成的，不是新发明。
HOST_MACHINE="$(uname -m)"
TARGET_MACHINE="${PINVOU3_BRIDGE_TARGET_ARCH:-$HOST_MACHINE}"

# machine 名 -> Node 官方发行包的 target 名
node_target_for() {
  case "$OS_NAME-$1" in
    Linux-x86_64) echo "linux-x64" ;;
    Linux-aarch64|Linux-arm64) echo "linux-arm64" ;;
    Darwin-x86_64) echo "darwin-x64" ;;
    Darwin-arm64) echo "darwin-arm64" ;;
    *) return 1 ;;
  esac
}

HOST_NODE_TARGET="$(node_target_for "$HOST_MACHINE")" || {
  echo "Bridge 备料不认识这个宿主：$OS_NAME-$HOST_MACHINE（只认 x86_64、aarch64/arm64）" >&2
  exit 1
}

# 目标 machine 名的词表要比 `uname -m` 宽：PINVOU3_BRIDGE_TARGET_ARCH 是从 JS 侧
# 按 Rust target 三元组注入的，三元组里 macOS 的 arm64 写作 **aarch64**
#（aarch64-apple-darwin，run-mac-verify.sh 和 mac-build.yml 装的就是它），而
# macOS 自己的 `uname -m` 返回 arm64。两个词都得认，否则 darwin 宿主上手工单架构
# 构建会落到 `*)` 硬失败——跟 Linux 那条本来就写成双词表是同一个道理。
case "$OS_NAME-$TARGET_MACHINE" in
  Linux-x86_64)
    NODE_OS="linux"
    NODE_CPU="x64"
    NODE_TARGET="linux-x64"
    NODE_TARGETS=("linux-x64")
    ;;
  Linux-aarch64|Linux-arm64)
    NODE_OS="linux"
    NODE_CPU="arm64"
    NODE_TARGET="linux-arm64"
    NODE_TARGETS=("linux-arm64")
    ;;
  Darwin-x86_64)
    NODE_OS="darwin"
    NODE_CPU="x64"
    NODE_TARGET="darwin-x64"
    NODE_TARGETS=("darwin-arm64" "darwin-x64")
    ;;
  Darwin-aarch64|Darwin-arm64)
    NODE_OS="darwin"
    NODE_CPU="arm64"
    NODE_TARGET="darwin-arm64"
    NODE_TARGETS=("darwin-arm64" "darwin-x64")
    ;;
  *)
    echo "Bridge 备料不认识这个目标：$OS_NAME-$TARGET_MACHINE（只认 x86_64、aarch64/arm64）" >&2
    exit 1
    ;;
esac

# 交叉时把宿主那份也加进下载清单——它只用来跑 npm，不进包。
if [ "$HOST_NODE_TARGET" != "$NODE_TARGET" ]; then
  case " ${NODE_TARGETS[*]} " in
    *" $HOST_NODE_TARGET "*) ;;
    *) NODE_TARGETS+=("$HOST_NODE_TARGET") ;;
  esac
  echo "[codex-bridge] 交叉构建：目标 $NODE_TARGET，用宿主 $HOST_NODE_TARGET 的 node 跑 npm"
fi

if bridge_runtime_valid "$OUT_DIR"; then
  echo "Codex ACP Bridge already ready: $OUT_DIR"
  exit 0
fi

node_archive_ext() {
  case "$1" in
    linux-*) echo "tar.xz" ;;
    darwin-*) echo "tar.gz" ;;
    *) return 1 ;;
  esac
}

node_sha256() {
  case "$1" in
    linux-x64) echo "2f2c0da162318f0de47665410c7c8c2ed3d36c8f3105de4bbc61176c70a7cbf2" ;;
    linux-arm64) echo "5f4ddab610c1ab2016b3c227cebdbf6d9495161487e4739c7b90090595f465f7" ;;
    darwin-x64) echo "9e5b2644cf107befb6aefca676b96d3296bc10138096f022ed378d6233ed81f4" ;;
    darwin-arm64) echo "40e5607e5ecb3db9192723776da2d75d966260fc74a7a9e731c1bd67dda96bc8" ;;
    *) return 1 ;;
  esac
}

for command_name in curl tar; do
  command -v "$command_name" >/dev/null 2>&1 || {
    echo "缺少构建命令: $command_name" >&2
    exit 1
  }
done

# macOS 没有 sha256sum,回退 shasum;两种工具的 --check 输入格式相同。
if command -v sha256sum >/dev/null 2>&1; then
  SHA256_CHECK=(sha256sum --check -)
elif command -v shasum >/dev/null 2>&1; then
  SHA256_CHECK=(shasum -a 256 --check -)
else
  echo "缺少 SHA256 校验工具(sha256sum 或 shasum)" >&2
  exit 1
fi

# Node 解压与 npm 安装需要数百 MB。把 staging 放到资源目录同一文件系统，
# 避免容量较小的 /tmp 留下半成品，也让最终目录切换只做同盘 rename。
RESOURCE_PARENT="$(dirname "$OUT_DIR")"
mkdir -p "$RESOURCE_PARENT"
BUILD_DIR="$(mktemp -d "$RESOURCE_PARENT/.codex-bridge-build.XXXXXX")"
trap 'rm -rf $DD "$BUILD_DIR"' EXIT

# 要打进包的那份（目标架构）
NODE_DIST_ROOT="$BUILD_DIR/node-v${NODE_VERSION}-${NODE_TARGET}"
# 用来跑 npm 的那份（宿主架构）。不交叉时两者是同一个目录。
NPM_NODE_ROOT="$BUILD_DIR/node-v${NODE_VERSION}-${HOST_NODE_TARGET}"
for node_target in "${NODE_TARGETS[@]}"; do
  node_archive_ext="$(node_archive_ext "$node_target")"
  node_archive="node-v${NODE_VERSION}-${node_target}.${node_archive_ext}"
  node_archive_path="$BUILD_DIR/$node_archive"
  downloaded=false
  for base_url in "https://nodejs.org/dist/v${NODE_VERSION}" "https://npmmirror.com/mirrors/node/v${NODE_VERSION}"; do
    if curl --fail --location --retry 2 --connect-timeout 15 \
      "$base_url/$node_archive" --output "$node_archive_path"; then
      downloaded=true
      break
    fi
  done
  if [ "$downloaded" != true ]; then
    echo "下载 Node.js Runtime 失败: $node_target" >&2
    exit 1
  fi
  printf '%s  %s\n' "$(node_sha256 "$node_target")" "$node_archive_path" \
    | "${SHA256_CHECK[@]}"
  case "$node_archive_ext" in
    tar.xz) tar -xJf "$node_archive_path" -C "$BUILD_DIR" ;;
    tar.gz) tar -xzf "$node_archive_path" -C "$BUILD_DIR" ;;
  esac
done

ACP_ROOT="$BUILD_DIR/acp"
mkdir -p "$ACP_ROOT"
cp $DD "$BRIDGE_PACKAGE_DIR/package.json" "$BRIDGE_PACKAGE_DIR/package-lock.json" "$ACP_ROOT/"

npm_ci_for_target() {
  local prefix="$1"
  local target_os="$2"
  local target_cpu="$3"
  local npm_args=(
    ci
    --prefix "$prefix"
    --os="$target_os"
    --cpu="$target_cpu"
    --no-audit
    --no-fund
    --omit=dev
  )
  if [ "$target_os" = "linux" ]; then
    npm_args+=(--libc=glibc)
  fi
  # 用**宿主**架构的 node 跑 npm：交叉时目标架构的 node 在这台机器上执行不了
  # （ENOEXEC）。依赖解析的正确性由上面的 --os/--cpu/--libc 保证，跟拿哪个 node
  # 解释器跑无关。非交叉时 NPM_NODE_ROOT 就等于 NODE_DIST_ROOT，行为不变。
  PATH="$NPM_NODE_ROOT/bin:$PATH" "$NPM_NODE_ROOT/bin/npm" "${npm_args[@]}"
}

npm_ci_for_target "$ACP_ROOT" "$NODE_OS" "$NODE_CPU"

# Bridge 通过 CODEX_PATH 启动系统 Codex，通过 CLAUDE_CODE_EXECUTABLE / PATH
# 中的 claude 启动系统 Claude Code（与 Kimi 一致），均不随包携带平台原生二进制
#（单个 claude 二进制解压后约 245MB，universal 双架构会让 dmg 多出约 140MB）。
rm -rf $DD "$ACP_ROOT/node_modules/@openai"/codex-*
rm -rf $DD "$ACP_ROOT"/node_modules/@anthropic-ai/claude-agent-sdk-{darwin,linux,win32}-*
if find "$ACP_ROOT/node_modules/@openai" -maxdepth 1 -mindepth 1 \
  -type d -name 'codex-*' -print -quit | grep -q .; then
  echo "Bridge 中仍残留 Codex 平台二进制，拒绝打包" >&2
  exit 1
fi
if find "$ACP_ROOT/node_modules/@anthropic-ai" -maxdepth 1 -mindepth 1 \
  -type d -name 'claude-agent-sdk-*' -print -quit | grep -q .; then
  echo "Bridge 中仍残留 Claude 平台二进制，拒绝打包" >&2
  exit 1
fi

READY_DIR="$BUILD_DIR/ready"
mkdir -p "$READY_DIR/node" "$READY_DIR/acp"
if [ "$OS_NAME" = "Darwin" ]; then
  for node_target in "${NODE_TARGETS[@]}"; do
    mkdir -p "$READY_DIR/node/$node_target/bin"
    install -m 0755 \
      "$BUILD_DIR/node-v${NODE_VERSION}-${node_target}/bin/node" \
      "$READY_DIR/node/$node_target/bin/node"
  done
  install -m 0644 "$NODE_DIST_ROOT/LICENSE" "$READY_DIR/node/LICENSE"
else
  mkdir -p "$READY_DIR/node/bin"
  install -m 0755 "$NODE_DIST_ROOT/bin/node" "$READY_DIR/node/bin/node"
  install -m 0644 "$NODE_DIST_ROOT/LICENSE" "$READY_DIR/node/LICENSE"
fi
# 连接器首次使用时由随包 Node 运行 npm；保留 Node 官方发行包自带的纯 JS npm，
# Linux/macOS 均不再依赖用户机器预装 npm。
mkdir -p "$READY_DIR/node/lib/node_modules"
cp -R $DD "$NODE_DIST_ROOT/lib/node_modules/npm" "$READY_DIR/node/lib/node_modules/npm"
mv $DD "$ACP_ROOT/node_modules" "$READY_DIR/acp/node_modules"

# 生成 manifest 用宿主那份 node（`$NPM_NODE_ROOT`），不能用刚装进 `$READY_DIR/node`
# 的那份：那是要打进包的**目标**架构二进制，交叉构建时在这台机器上根本执行不了。
# 生成 manifest 是纯 JS，跟解释器架构无关，用宿主那份没有任何副作用。
"${NPM_NODE_ROOT}/bin/node" -e '
const fs = require("fs");
const path = require("path");
const out = process.argv[1];
const platform = process.argv[5];
const runtimeArch = process.argv[6];
const nodes = platform === "darwin"
  ? {
      arm64: "node/darwin-arm64/bin/node",
      x64: "node/darwin-x64/bin/node"
    }
  : { [runtimeArch]: "node/bin/node" };
const manifest = {
  schema_version: 3,
  node_version: process.argv[2],
  codex_acp_version: process.argv[3],
  claude_acp_version: process.argv[4],
  claude_sdk_version: process.argv[7],
  platform,
  arch: platform === "darwin" ? "universal" : runtimeArch,
  node: nodes[runtimeArch],
  nodes,
  npm: "node/lib/node_modules/npm/bin/npm-cli.js",
  entrypoints: {
    codex: "acp/node_modules/@agentclientprotocol/codex-acp/dist/index.js",
    claude: "acp/node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js"
  },
  requires_codex_path: true
};
fs.writeFileSync(path.join(out, "manifest.json"), JSON.stringify(manifest, null, 2) + "\n");
' "$READY_DIR" "$NODE_VERSION" "$CODEX_ACP_VERSION" "$CLAUDE_ACP_VERSION" \
  "$NODE_OS" "$NODE_CPU" "$CLAUDE_SDK_VERSION"

bridge_runtime_valid "$READY_DIR" || {
  echo "生成的 Codex ACP Bridge 未通过完整性检查" >&2
  exit 1
}

mkdir -p "$OUT_DIR"
rm -rf $DD "$OUT_DIR/node.next" "$OUT_DIR/acp.next"
rm -f $DD "$OUT_DIR/manifest.json.next"
mv $DD "$READY_DIR/node" "$OUT_DIR/node.next"
mv $DD "$READY_DIR/acp" "$OUT_DIR/acp.next"
mv $DD "$READY_DIR/manifest.json" "$OUT_DIR/manifest.json.next"
rm -rf $DD "$OUT_DIR/node" "$OUT_DIR/acp"
rm -f $DD "$OUT_DIR/manifest.json"
mv $DD "$OUT_DIR/node.next" "$OUT_DIR/node"
mv $DD "$OUT_DIR/acp.next" "$OUT_DIR/acp"
mv $DD "$OUT_DIR/manifest.json.next" "$OUT_DIR/manifest.json"

bridge_runtime_valid "$OUT_DIR" || {
  echo "Codex ACP Bridge 安装后完整性检查失败" >&2
  exit 1
}
echo "Codex ACP Bridge ready: $OUT_DIR"
