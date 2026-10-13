#!/bin/bash
# pinvou3 发布脚本：构建 deb → 生成 latest.json → rsync 上传到更新源。
# 用法: ./scripts/release-deb.sh "本次更新说明"
#
# 发版三步:
#   1. bump 版本号(tauri.conf.json / Cargo.toml / package.json 三处,本脚本会校验一致)
#   2. ./scripts/release-deb.sh "修了 xxx"
#   3. 客户端 App 启动/手动检查即可看到新版
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
APP_DIR="$REPO_ROOT/pinvou3-app"
SERVER="admin@8.218.49.20"
REMOTE_DIR="/var/www/pinvou3"
BASE_URL="https://pinvou.com/pinvou3"

NOTES="${1:-}"
if [ -z "$NOTES" ]; then
  echo "用法: $0 \"本次更新说明\"" >&2
  exit 1
fi

# ── 1. 版本号以根目录 VERSION 为单一事实来源(社区 #604 回流)──────
# 旧三处交叉比对在打包文件同时过时时照样通过;改由 sync-version.mjs --check
# 把登记的全部版本文件逐一与 VERSION 比对(社区 #604),版本号直接读 VERSION。
node "$REPO_ROOT/scripts/sync-version.mjs" --check
VERSION="$(tr -d '[:space:]' < "$REPO_ROOT/VERSION")"
echo "=== 发布 pinvou3 v$VERSION ==="

# ── 1.5 embedding 模型不再随 deb 打包（deb 瘦 ~559MB）──────
# 模型(bge-m3)改由客户端在知识库页**按需下载**,见 scripts/release-kb-model.sh(一次性发布到
# OTA 源 kb-model/bge-m3.tar.gz,变更模型才跑)。这里不再拷模型进 resources/bge-m3。

# ── 2. 准备 Codex ACP Bridge，按 lockfile 安装前端工具链并构建 deb ──
# Vite/React 已进入正式构建链；每次发布先 npm ci，避免旧 worktree 只有 Tauri CLI、
# 缺少 vite/react 等新增依赖时生成坏包或直接在 beforeBuildCommand 失败。
"$APP_DIR/scripts/prepare-codex-bridge-runtime.sh"
(cd "$APP_DIR" && npm ci --prefer-offline --no-audit && npm run build)

# 架构取实际构建出的产物(社区 #604 回流):构建 target 来自 host 的 node/Rust 三元组,
# 与 dpkg 无关;dpkg 缺失时旧的 `2>/dev/null || echo amd64` 会在 arm64 非 Debian host 上
# 静默猜成 amd64,随后以误导性的"产物不存在"指向一个从未构建过的文件名。
DEB_DIR="$APP_DIR/src-tauri/target/release/bundle/deb"
shopt -s nullglob
BUILT=("$DEB_DIR/pinvou3_${VERSION}_"*.deb)
shopt -u nullglob
if [ "${#BUILT[@]}" -ne 1 ]; then
  echo "Expected exactly one $DEB_DIR/pinvou3_${VERSION}_*.deb, found ${#BUILT[@]}" >&2
  if [ -d "$DEB_DIR" ]; then
    ls -la "$DEB_DIR" >&2
  fi
  exit 1
fi
DEB="${BUILT[0]}"
ARCH="$(basename "$DEB" .deb)"
ARCH="${ARCH#pinvou3_"${VERSION}"_}"

# 与 release-packages.yml 同款最大压缩重打包(data.tar gzip-6 → xz -9);
# 先重打包再算 sha256/latest.json,校验和与清单覆盖最终产物。
"$REPO_ROOT/scripts/repack-deb-xz.sh" "$DEB"

# ── 3. 合并 latest.json(保留其它平台条目,只更新顶层 Linux 字段) ──────
# 修复「Linux 发版抹掉 platforms.macos-arm64」:此前用 `jq -n` 从零生成 latest.json,
# 会清掉 platforms 下所有非 Linux 条目 → Mac 客户端回退到顶层(此时是 Linux deb)→
# 把 .deb 当 dmg 下。改为:拉远端 latest.json → 只覆盖顶层 version/url/sha256/size →
# 推回。platforms.macos-arm64 / windows-x86_64 等条目原样保留。
SHA256=$(sha256sum "$DEB" | awk '{print $1}')
SIZE=$(stat -c%s "$DEB")
PUB_DATE=$(date -u +%FT%TZ)
DEB_URL="$BASE_URL/pinvou3_${VERSION}_${ARCH}.deb"

TMP_REMOTE=$(mktemp)
TMP_JSON_NEW=$(mktemp)
TMP_ERR=$(mktemp)
trap 'rm -f "$TMP_REMOTE" "$TMP_JSON_NEW" "$TMP_ERR"' EXIT

# 拉远端 latest.json(与 release-macos.sh 同款加固):SSH 探测本身失败必须中止,
# 只有远端明确返回 missing 才能按首发场景使用 {}。不能用 `|| true`,否则网络/
# 权限故障会被伪装成文件不存在,随后覆盖掉其它平台条目。
if ! REMOTE_STATE=$(ssh "$SERVER" \
  "if [ -f '$REMOTE_DIR/latest.json' ]; then printf '%s\\n' exists; else printf '%s\\n' missing; fi" \
  2>"$TMP_ERR"); then
  echo "❌ 无法探测远端 latest.json(SSH/权限/网络异常),中止发布:" >&2
  cat "$TMP_ERR" >&2
  exit 1
fi
if [ "$REMOTE_STATE" = "exists" ]; then
  if ! ssh "$SERVER" "cat $REMOTE_DIR/latest.json" >"$TMP_REMOTE" 2>"$TMP_ERR"; then
    echo "❌ 远端 latest.json 存在但读取失败(权限不足/网络中断?),中止以免破坏清单:" >&2
    cat "$TMP_ERR" >&2
    exit 1
  fi
  if ! jq -e . "$TMP_REMOTE" >/dev/null 2>&1; then
    echo "❌ 远端 latest.json 非合法 JSON,中止以免破坏清单:" >&2
    head -c 200 "$TMP_REMOTE" >&2
    exit 1
  fi
elif [ "$REMOTE_STATE" = "missing" ]; then
  echo "⚠️  远端 latest.json 不存在(首发场景),用空对象 {} 起步" >&2
  echo '{}' > "$TMP_REMOTE"
else
  echo "❌ 远端 latest.json 探测返回异常结果: $REMOTE_STATE" >&2
  exit 1
fi
jq --arg ver "$VERSION" --arg url "$DEB_URL" --arg sha "$SHA256" --arg size "$SIZE" \
   --arg date "$PUB_DATE" --arg notes "$NOTES" '
  .version = $ver |
  .pub_date = $date |
  .notes = $notes |
  .url = $url |
  .sha256 = $sha |
  .size = ($size | tonumber) |
  .platforms = (.platforms // {})
' "$TMP_REMOTE" > "$TMP_JSON_NEW"

echo "--- latest.json (顶层 + 各平台节) ---"
jq '{version, url, sha256, size, macos_arm64: .platforms["macos-arm64"] | {version,url,sha256} , linux_arm64: .platforms["linux-arm64"]}' "$TMP_JSON_NEW"

# ── 4. 上传：先 deb 后 latest.json ────────────────────────────────
# 顺序关键:清单最后传,避免清单已指向新版而 deb 还没传完,客户端 404。
# 原子上传清单(与 release-macos.sh 一致):先传临时文件名 → 远端 mv 原子重命名,
# 避免网络中断导致远端 latest.json 被截断成半份坏 JSON。
rsync -avz --progress "$DEB" "$SERVER:$REMOTE_DIR/"
rsync -avz "$TMP_JSON_NEW" "$SERVER:$REMOTE_DIR/latest.json.new"
ssh "$SERVER" "mv '$REMOTE_DIR/latest.json.new' '$REMOTE_DIR/latest.json' && chmod 644 '$REMOTE_DIR/latest.json'"

echo "=== 发布完成 ==="
echo "清单: $BASE_URL/latest.json"
curl -fsS "$BASE_URL/latest.json" | jq .version || echo "(线上验证失败,检查 nginx)"
