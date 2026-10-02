#!/usr/bin/env bash
# 部署 PINVOU 完整 WebUI v2 与 Relay 到生产端点。
# 流程：公网基线 → 构建/测试共享 WebUI → 远端暂存/完整备份/整体替换
#      → 服务与公网验证 → 失败自动回滚。脚本不修改 Nginx 配置。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [[ "${PINVOU_CONFIRM_PRODUCTION_DEPLOY:-0}" != "1" ]]; then
  echo "拒绝修改生产 Relay：请在确认发布窗口和回滚负责人后设置 PINVOU_CONFIRM_PRODUCTION_DEPLOY=1。" >&2
  exit 64
fi

RELAY_DIR="$ROOT/remote-control-relay"
SERVER="${PINVOU_REMOTE_DEPLOY_SERVER:-root@47.120.8.237}"
# 与测试端点部署脚本保持同一约定：默认使用本机部署私钥，且全程 BatchMode。
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_ed25519_pinvou8}"
REMOTE_DIR="${PINVOU_REMOTE_DEPLOY_DIR:-/opt/pinvou-remote-relay}"
SERVICE="${PINVOU_REMOTE_DEPLOY_SERVICE:-pinvou-remote-relay.service}"
PUBLIC_URL="${PINVOU_REMOTE_PUBLIC_URL:-https://pinvou.com/pinvou3/remote}"
DIRECT_URL="${PINVOU_REMOTE_DIRECT_URL:-http://47.120.8.237:8787/pinvou3/remote}"
BASE_PATH="${PINVOU_REMOTE_PUBLIC_BASE_PATH:-/pinvou3/remote}"
STAMP="$(date +%Y%m%d-%H%M%S)"
REMOTE_CODE_TMP="/tmp/pinvou-remote-code-$STAMP.tar.gz"
REMOTE_WEB_TMP="/tmp/pinvou-remote-web-$STAMP.tar.gz"
REMOTE_STATS_TMP="/tmp/pinvou-stats-$STAMP.html"
REMOTE_HARDENING_TMP="/tmp/pinvou-remote-hardening-$STAMP.conf"
REMOTE_PACKAGE_TMP="/tmp/pinvou-remote-package-$STAMP.json"
REMOTE_LOCK_TMP="/tmp/pinvou-remote-package-lock-$STAMP.json"
REMOTE_IPV4_DB_TMP="/tmp/pinvou-ip2region-v4-$STAMP.xdb"
REMOTE_IPV6_DB_TMP="/tmp/pinvou-ip2region-v6-$STAMP.xdb"
IP_DB_CACHE_DIR="${XDG_CACHE_HOME:-$HOME/.cache}/pinvou-deploy"
IPV4_DB_CACHE="$IP_DB_CACHE_DIR/ip2region-v4-v3.17.0.xdb"
IPV6_DB_CACHE="$IP_DB_CACHE_DIR/ip2region-v6-v3.17.0.xdb"
IPV4_DB_URL="https://raw.githubusercontent.com/lionsoul2014/ip2region/v3.17.0/data/ip2region_v4.xdb"
IPV6_DB_URL="https://raw.githubusercontent.com/lionsoul2014/ip2region/v3.17.0/data/ip2region_v6.xdb"
IPV4_DB_SHA256="6307a9696f5711f84bcb8b25f07894de68a64a0ed4a1cc7e990562dd3084f210"
IPV6_DB_SHA256="5b93da35ac28bc316dccc54a758381f7a874ae0461dd51ff5df5e34815586f11"
VERIFY_ERROR=""
LAST_HEALTH=""

if [[ "$BASE_PATH" != "/pinvou3/remote" ]]; then
  echo "拒绝执行：生产部署 BASE_PATH 必须是 /pinvou3/remote，当前为 $BASE_PATH" >&2
  exit 64
fi

ensure_cached_db() {
  local path="$1"
  local url="$2"
  local expected="$3"
  local temporary="${path}.download"
  if [[ -f "$path" ]] && echo "$expected  $path" | sha256sum -c --status; then
    return
  fi
  mkdir -p "$IP_DB_CACHE_DIR"
  rm -f "$temporary"
  curl -fL --retry 3 --connect-timeout 10 --max-time 180 -o "$temporary" "$url"
  echo "$expected  $temporary" | sha256sum -c --status
  mv "$temporary" "$path"
}

verify_public() {
  local expected="$1"
  local health page
  if ! health="$(curl -fsS "$PUBLIC_URL/healthz")"; then
    VERIFY_ERROR="公网健康检查失败：$PUBLIC_URL/healthz"
    return 1
  fi
  if ! node -e 'const h=JSON.parse(process.argv[1]); if(!h.ok || !("room_count" in h) || "rooms" in h) process.exit(1)' "$health"; then
    VERIFY_ERROR="公网健康检查返回格式不符合预期"
    return 1
  fi
  if ! page="$(curl -fsSL "$PUBLIC_URL/r/deploy-check")"; then
    VERIFY_ERROR="手机页面检查失败：$PUBLIC_URL/r/deploy-check"
    return 1
  fi
  if [[ "$expected" == "release" ]]; then
    if [[ "$page" != *'<title>PINVOU 智能助手'* \
      || "$page" != *'<base href="/pinvou3/remote/">'* \
      || "$page" != *'/pinvou3/remote/assets/'* ]]; then
      VERIFY_ERROR="公网页面未命中完整 WebUI v2 或生产 base path"
      return 1
    fi
  elif [[ -z "$page" ]]; then
    VERIFY_ERROR="部署前公网页面为空"
    return 1
  fi
  if curl --noproxy '*' -fsS --max-time 3 "$DIRECT_URL/healthz" >/dev/null 2>&1; then
    VERIFY_ERROR="Relay 仍可通过公网端口直接访问：$DIRECT_URL"
    return 1
  fi
  LAST_HEALTH="$health"
  VERIFY_ERROR=""
}

if ! verify_public baseline; then
  echo "部署前检查失败，未修改线上版本：$VERIFY_ERROR" >&2
  exit 1
fi

node --check "$RELAY_DIR/server.js"
node --check "$RELAY_DIR/telemetry-service.js"
node --check "$RELAY_DIR/behavior-telemetry-schema.js"
if [[ "${SKIP_WEB_BUILD:-0}" != "1" ]]; then
  echo "构建生产 base path 的共享 WebUI"
  (cd "$ROOT/pinvou3-app" && PINVOU_REMOTE_PUBLIC_BASE_PATH="$BASE_PATH" npm run build:web)
else
  echo "使用已构建并验证的共享 WebUI 产物"
fi
test -f "$RELAY_DIR/web/dist/index.html"
test -f "$RELAY_DIR/web/dist/platform/tauri/bridge.js"
grep -Fq '<base href="/pinvou3/remote/">' "$RELAY_DIR/web/dist/index.html"
# 当前 WebUI 由入口 JS 动态加载 bridge，index.html 不再内联该路径；改为校验
# 「入口引用生产 base path 且 bridge 资源随包存在」，避免把构建方式变化误判成部署失败。
grep -Fq '/pinvou3/remote/assets/' "$RELAY_DIR/web/dist/index.html"
if [[ "${SKIP_LOCAL_TESTS:-0}" != "1" ]]; then
  (cd "$RELAY_DIR" && npm test)
fi
ensure_cached_db "$IPV4_DB_CACHE" "$IPV4_DB_URL" "$IPV4_DB_SHA256"
ensure_cached_db "$IPV6_DB_CACHE" "$IPV6_DB_URL" "$IPV6_DB_SHA256"

mapfile -t relay_modules < <(cd "$RELAY_DIR" && printf '%s\n' ./*.js)
tar -czf - -C "$RELAY_DIR" \
  "${relay_modules[@]}" \
  -C "$ROOT" \
  docs/schemas/behavior-telemetry-v1.schema.json \
  docs/schemas/behavior-telemetry-registries.json \
  | ssh -i "$SSH_KEY" -o BatchMode=yes "$SERVER" "cat > '$REMOTE_CODE_TMP'"
# dist 内包含带哈希的 JS/CSS、桥接脚本和本地 vendor，必须作为一个版本整体上传。
tar -czf - -C "$RELAY_DIR/web/dist" . | ssh -i "$SSH_KEY" -o BatchMode=yes "$SERVER" "cat > '$REMOTE_WEB_TMP'"
scp -i "$SSH_KEY" -o BatchMode=yes "$RELAY_DIR/web/stats.html" "$SERVER:$REMOTE_STATS_TMP"
scp -i "$SSH_KEY" -o BatchMode=yes "$RELAY_DIR/10-hardening.conf" "$SERVER:$REMOTE_HARDENING_TMP"
scp -i "$SSH_KEY" -o BatchMode=yes "$RELAY_DIR/package.json" "$SERVER:$REMOTE_PACKAGE_TMP"
scp -i "$SSH_KEY" -o BatchMode=yes "$RELAY_DIR/package-lock.json" "$SERVER:$REMOTE_LOCK_TMP"
scp -i "$SSH_KEY" -o BatchMode=yes "$IPV4_DB_CACHE" "$SERVER:$REMOTE_IPV4_DB_TMP"
scp -i "$SSH_KEY" -o BatchMode=yes "$IPV6_DB_CACHE" "$SERVER:$REMOTE_IPV6_DB_TMP"

deploy_output="$(ssh -i "$SSH_KEY" -o BatchMode=yes "$SERVER" bash -s -- "$REMOTE_DIR" "$SERVICE" "$STAMP" "$REMOTE_CODE_TMP" "$REMOTE_WEB_TMP" "$REMOTE_STATS_TMP" "$REMOTE_HARDENING_TMP" "$REMOTE_PACKAGE_TMP" "$REMOTE_LOCK_TMP" "$REMOTE_IPV4_DB_TMP" "$REMOTE_IPV6_DB_TMP" <<'REMOTE'
set -Eeuo pipefail
remote_dir="$1"
service="$2"
stamp="$3"
code_tmp="$4"
web_tmp="$5"
stats_tmp="$6"
hardening_tmp="$7"
package_tmp="$8"
lock_tmp="$9"
ipv4_tmp="${10}"
ipv6_tmp="${11}"
backup="$remote_dir/backups/$stamp"
code_stage="$remote_dir/code.next-$stamp"
web_stage="$remote_dir/web/dist.next-$stamp"
dropin_dir="/etc/systemd/system/${service}.d"
dropin="$dropin_dir/10-hardening.conf"
telemetry_data_dir="/var/lib/pinvou-telemetry"
ipv4_db="$telemetry_data_dir/ip2region_v4.xdb"
ipv6_db="$telemetry_data_dir/ip2region_v6.xdb"
ipv4_sha256="6307a9696f5711f84bcb8b25f07894de68a64a0ed4a1cc7e990562dd3084f210"
ipv6_sha256="5b93da35ac28bc316dccc54a758381f7a874ae0461dd51ff5df5e34815586f11"

rm -rf "$code_stage"
mkdir -p "$code_stage"
tar --no-same-owner --no-same-permissions -xzf "$code_tmp" -C "$code_stage"
rm -f "$code_tmp"
node --check "$code_stage/server.js"
node --check "$code_stage/telemetry-service.js"
node --check "$code_stage/behavior-telemetry-schema.js"
test -f "$code_stage/docs/schemas/behavior-telemetry-v1.schema.json"
test -f "$code_stage/docs/schemas/behavior-telemetry-registries.json"
mkdir -p "$remote_dir/web"
rm -rf "$web_stage"
mkdir -p "$web_stage"
tar --no-same-owner --no-same-permissions -xzf "$web_tmp" -C "$web_stage"
chown -R root:root "$web_stage"
find "$web_stage" -type d -exec chmod 755 {} +
find "$web_stage" -type f -exec chmod 644 {} +
rm -f "$web_tmp"
test -f "$web_stage/index.html"
test -f "$web_stage/platform/tauri/bridge.js"
grep -Fq '<base href="/pinvou3/remote/">' "$web_stage/index.html"
# 当前 WebUI 由入口 JS 动态加载 bridge，index.html 不再内联该路径（2026-09-19 实测）；
# 校验「入口引用生产 base path 且 bridge 资源随包存在」即可保持本意。
grep -Fq '/pinvou3/remote/assets/' "$web_stage/index.html"
mkdir -p "$backup"
if [[ -f "$remote_dir/server.js" ]]; then cp -a "$remote_dir/server.js" "$backup/server.js"; else touch "$backup/no-server"; fi
if [[ -d "$remote_dir/web/dist" ]]; then cp -a "$remote_dir/web/dist" "$backup/web-dist"; else touch "$backup/no-web-dist"; fi
if [[ -f "$remote_dir/package.json" ]]; then cp -a "$remote_dir/package.json" "$backup/package.json"; else touch "$backup/no-package"; fi
if [[ -f "$remote_dir/package-lock.json" ]]; then cp -a "$remote_dir/package-lock.json" "$backup/package-lock.json"; else touch "$backup/no-package-lock"; fi
if [[ -f "$remote_dir/telemetry-service.js" ]]; then
  cp -a "$remote_dir/telemetry-service.js" "$backup/telemetry-service.js"
else
  touch "$backup/no-telemetry-service"
fi
if [[ -f "$remote_dir/behavior-telemetry-schema.js" ]]; then
  cp -a "$remote_dir/behavior-telemetry-schema.js" "$backup/behavior-telemetry-schema.js"
else
  touch "$backup/no-behavior-telemetry-schema"
fi
mkdir -p "$backup/code-js"
for module in "$remote_dir"/*.js; do
  [[ -f "$module" ]] || continue
  cp -a "$module" "$backup/code-js/"
done
if [[ -d "$remote_dir/docs/schemas" ]]; then
  cp -a "$remote_dir/docs/schemas" "$backup/telemetry-schemas"
else
  touch "$backup/no-telemetry-schemas"
fi
if [[ -f "$remote_dir/web/stats.html" ]]; then
  cp -a "$remote_dir/web/stats.html" "$backup/stats.html"
else
  touch "$backup/no-stats-html"
fi
mkdir -p "$dropin_dir"
if [[ -f "$dropin" ]]; then
  cp -a "$dropin" "$backup/10-hardening.conf"
else
  touch "$backup/no-hardening-dropin"
fi

rollback() {
  if [[ -f "$backup/no-server" ]]; then rm -f "$remote_dir/server.js"; else cp -a "$backup/server.js" "$remote_dir/server.js"; fi
  rm -rf "$remote_dir/web/dist"
  if [[ ! -f "$backup/no-web-dist" ]]; then cp -a "$backup/web-dist" "$remote_dir/web/dist"; fi
  if [[ -f "$backup/no-package" ]]; then rm -f "$remote_dir/package.json"; else cp -a "$backup/package.json" "$remote_dir/package.json"; fi
  if [[ -f "$backup/no-package-lock" ]]; then rm -f "$remote_dir/package-lock.json"; else cp -a "$backup/package-lock.json" "$remote_dir/package-lock.json"; fi
  if [[ -f "$remote_dir/package.json" ]]; then (cd "$remote_dir" && npm ci --omit=dev); fi
  if [[ -f "$backup/no-telemetry-service" ]]; then rm -f "$remote_dir/telemetry-service.js"; else cp -a "$backup/telemetry-service.js" "$remote_dir/telemetry-service.js"; fi
  if [[ -f "$backup/no-behavior-telemetry-schema" ]]; then rm -f "$remote_dir/behavior-telemetry-schema.js"; else cp -a "$backup/behavior-telemetry-schema.js" "$remote_dir/behavior-telemetry-schema.js"; fi
  rm -f "$remote_dir"/*.js
  for module in "$backup/code-js"/*.js; do
    [[ -f "$module" ]] || continue
    cp -a "$module" "$remote_dir/"
  done
  rm -rf "$remote_dir/docs/schemas"
  if [[ ! -f "$backup/no-telemetry-schemas" ]]; then mkdir -p "$remote_dir/docs"; cp -a "$backup/telemetry-schemas" "$remote_dir/docs/schemas"; fi
  if [[ -f "$backup/no-stats-html" ]]; then rm -f "$remote_dir/web/stats.html"; else cp -a "$backup/stats.html" "$remote_dir/web/stats.html"; fi
  if [[ -f "$backup/no-hardening-dropin" ]]; then
    rm -f "$dropin"
  else
    cp -a "$backup/10-hardening.conf" "$dropin"
  fi
  systemctl daemon-reload
  systemctl restart "$service"
}

rollback_on_error() {
  local status=$?
  trap - ERR
  echo "部署步骤失败，开始从 $backup 回滚" >&2
  if ! rollback; then
    echo "严重：自动回滚失败，请立即检查 $service（备份：$backup）" >&2
  fi
  exit "$status"
}
trap rollback_on_error ERR

install_db() {
  local path="$1"
  local supplied="$2"
  local expected="$3"
  if [[ -f "$path" ]] && echo "$expected  $path" | sha256sum -c --status; then
    rm -f "$supplied"
    return
  fi
  echo "$expected  $supplied" | sha256sum -c --status
  chown root:root "$supplied"
  chmod 600 "$supplied"
  mv "$supplied" "$path"
}

mkdir -p "$telemetry_data_dir"
chown root:root "$telemetry_data_dir"
chmod 700 "$telemetry_data_dir"
install_db "$ipv4_db" "$ipv4_tmp" "$ipv4_sha256"
install_db "$ipv6_db" "$ipv6_tmp" "$ipv6_sha256"

chown -R root:root "$code_stage"
find "$code_stage" -type d -exec chmod 755 {} +
find "$code_stage" -type f -exec chmod 644 {} +
chown root:root "$stats_tmp" "$hardening_tmp" "$package_tmp" "$lock_tmp"
chmod 644 "$stats_tmp" "$hardening_tmp" "$package_tmp" "$lock_tmp"
mv "$package_tmp" "$remote_dir/package.json"
mv "$lock_tmp" "$remote_dir/package-lock.json"
(cd "$remote_dir" && npm ci --omit=dev)
mv "$code_stage/server.js" "$remote_dir/server.js"
mv "$code_stage/telemetry-service.js" "$remote_dir/telemetry-service.js"
mv "$code_stage/behavior-telemetry-schema.js" "$remote_dir/behavior-telemetry-schema.js"
for module in "$code_stage"/*.js; do
  [[ -f "$module" ]] || continue
  mv "$module" "$remote_dir/"
done
mkdir -p "$remote_dir/docs"
rm -rf "$remote_dir/docs/schemas"
mv "$code_stage/docs/schemas" "$remote_dir/docs/schemas"
rm -rf "$code_stage"
(cd "$remote_dir" && node --input-type=module -e "await import('./telemetry-service.js')")
rm -rf "$remote_dir/web/dist"
mv "$web_stage" "$remote_dir/web/dist"
mv "$stats_tmp" "$remote_dir/web/stats.html"
mv "$hardening_tmp" "$dropin"
systemctl daemon-reload

if ! systemctl restart "$service" || ! systemctl is-active --quiet "$service"; then
  trap - ERR
  rollback
  echo "部署失败，已从 $backup 回滚" >&2
  exit 1
fi

# 有界重试：新版启动（含 controller runtime 初始化）可能超过 1 秒，固定 sleep 会把
# 「启动稍慢」误判成失败并触发回滚（2026-09-19 实测踩坑）。
healthy=0
for _ in $(seq 1 30); do
  if curl -fsS http://127.0.0.1:8787/pinvou3/remote/healthz >/dev/null \
    && curl -fsS http://127.0.0.1:8787/pinvou3/telemetry/healthz >/dev/null; then
    healthy=1
    break
  fi
  sleep 1
done
if [ "$healthy" != "1" ]; then
  trap - ERR
  rollback
  echo "健康检查失败，已从 $backup 回滚" >&2
  exit 1
fi
trap - ERR
echo "backup=$backup"
REMOTE
) " || {
  # 远程步骤失败时把它的 stdout 一并回显，否则 set -e 会在赋值处静默退出，
  # 现场只剩一个 exit code（2026-09-19 实测踩坑）。
  printf '%s\n' "$deploy_output" >&2
  echo "远程部署步骤失败，请检查上方输出" >&2
  exit 1
}
echo "$deploy_output"

backup="$REMOTE_DIR/backups/$STAMP"

rollback_remote() {
  ssh -i "$SSH_KEY" -o BatchMode=yes "$SERVER" bash -s -- "$REMOTE_DIR" "$SERVICE" "$backup" <<'REMOTE'
set -euo pipefail
remote_dir="$1"
service="$2"
backup="$3"

case "$backup" in
  "$remote_dir"/backups/*) ;;
  *) echo "拒绝未知备份目录：$backup" >&2; exit 1 ;;
esac
test -d "$backup"
if [[ -f "$backup/no-server" ]]; then rm -f "$remote_dir/server.js"; else test -f "$backup/server.js"; cp -a "$backup/server.js" "$remote_dir/server.js"; fi
rm -rf "$remote_dir/web/dist"
if [[ ! -f "$backup/no-web-dist" ]]; then test -d "$backup/web-dist"; cp -a "$backup/web-dist" "$remote_dir/web/dist"; fi
if [[ -f "$backup/no-package" ]]; then rm -f "$remote_dir/package.json"; else test -f "$backup/package.json"; cp -a "$backup/package.json" "$remote_dir/package.json"; fi
if [[ -f "$backup/no-package-lock" ]]; then rm -f "$remote_dir/package-lock.json"; else test -f "$backup/package-lock.json"; cp -a "$backup/package-lock.json" "$remote_dir/package-lock.json"; fi
if [[ -f "$remote_dir/package.json" ]]; then (cd "$remote_dir" && npm ci --omit=dev); fi
if [[ -f "$backup/no-telemetry-service" ]]; then rm -f "$remote_dir/telemetry-service.js"; else cp -a "$backup/telemetry-service.js" "$remote_dir/telemetry-service.js"; fi
if [[ -f "$backup/no-behavior-telemetry-schema" ]]; then rm -f "$remote_dir/behavior-telemetry-schema.js"; else test -f "$backup/behavior-telemetry-schema.js"; cp -a "$backup/behavior-telemetry-schema.js" "$remote_dir/behavior-telemetry-schema.js"; fi
test -d "$backup/code-js"
rm -f "$remote_dir"/*.js
for module in "$backup/code-js"/*.js; do
  [[ -f "$module" ]] || continue
  cp -a "$module" "$remote_dir/"
done
rm -rf "$remote_dir/docs/schemas"
if [[ ! -f "$backup/no-telemetry-schemas" ]]; then test -d "$backup/telemetry-schemas"; mkdir -p "$remote_dir/docs"; cp -a "$backup/telemetry-schemas" "$remote_dir/docs/schemas"; fi
if [[ -f "$backup/no-stats-html" ]]; then rm -f "$remote_dir/web/stats.html"; else cp -a "$backup/stats.html" "$remote_dir/web/stats.html"; fi
if [[ -f "$backup/no-hardening-dropin" ]]; then
  rm -f "/etc/systemd/system/${service}.d/10-hardening.conf"
else
  test -f "$backup/10-hardening.conf"
  cp -a "$backup/10-hardening.conf" "/etc/systemd/system/${service}.d/10-hardening.conf"
fi
systemctl daemon-reload
systemctl restart "$service"
systemctl is-active --quiet "$service"
curl -fsS http://127.0.0.1:8787/pinvou3/remote/healthz >/dev/null
echo "已恢复备份：$backup"
REMOTE
}

if ! verify_public release; then
  deploy_error="$VERIFY_ERROR"
  echo "部署后检查失败，开始回滚：$deploy_error" >&2
  if ! rollback_remote; then
    echo "严重：自动回滚失败，请立即检查 $SERVER 上的 $SERVICE" >&2
    exit 1
  fi
  if ! verify_public baseline; then
    echo "严重：备份已恢复，但回滚后的公网复检失败：$VERIFY_ERROR" >&2
    exit 1
  fi
  echo "部署失败，已恢复并验证上一线上版本：$deploy_error" >&2
  exit 1
fi

echo "部署完成：$PUBLIC_URL"
echo "$LAST_HEALTH"
