#!/bin/sh
# pinvou3 .deb 装后处理:
#   1) 把真实用户加入 docker 组（模型商店引擎安装/部署的前置条件）
#   2) 刷新 desktop 数据库 + 图标缓存
#   3) 在每个真实用户桌面放一个快捷方式
# 全程 best-effort,任何失败都不阻塞安装(故意不开 set -e)。

APP_DESKTOP=/usr/share/applications/pinvou3.desktop

# ── 1) docker 组（最先——这是模型商店能用的前提） ────────────
# postinst 以 root 运行,直接 usermod。幂等：已在组里的不加。
# 此前每个客户首次下载模型都撞 "permission denied while trying to
# connect to the docker API" → pkexec 弹窗，违背「一键部署」承诺。
if command -v docker >/dev/null 2>&1 && getent group docker >/dev/null 2>&1; then
  for uhome in /home/*; do
    [ -d "$uhome" ] || continue
    uowner=$(stat -c %U "$uhome" 2>/dev/null) || continue
    [ -n "$uowner" ] && [ "$uowner" != "UNKNOWN" ] || continue
    # 跳过系统用户（uid < 1000）
    uuid=$(id -u "$uowner" 2>/dev/null) || continue
    [ "$uuid" -ge 1000 ] 2>/dev/null || continue
    # 幂等：不在 docker 组才加
    if ! id -nG "$uowner" 2>/dev/null | tr ' ' '\n' | grep -qx docker; then
      usermod -aG docker "$uowner" 2>/dev/null || true
      echo "[pinvou3] 已将 $uowner 加入 docker 组（重新登录后模型商店可直接使用）" >&2
    fi
  done
else
  echo "[pinvou3] 注意：未检测到 docker——模型商店的引擎安装需要 docker。" >&2
  echo "[pinvou3] 请先安装 docker 并运行：sudo usermod -aG docker <用户名>" >&2
fi

# ── 2) 缓存刷新 ──────────────────────────────────────────────
command -v update-desktop-database >/dev/null 2>&1 && \
  update-desktop-database -q /usr/share/applications 2>/dev/null || true
command -v gtk-update-icon-cache >/dev/null 2>&1 && \
  gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor 2>/dev/null || true

# ── 2) 桌面快捷方式 ──────────────────────────────────────────
[ -f "$APP_DESKTOP" ] || exit 0

drop_to_desktop() {
  uhome="$1"; uowner="$2"
  [ -d "$uhome" ] || return 0
  # 解析本地化桌面目录(中文系统 = ~/桌面),按 XDG user-dirs → 桌面 → Desktop 依次兜底
  ddir=""
  if [ -r "$uhome/.config/user-dirs.dirs" ]; then
    ddir=$(. "$uhome/.config/user-dirs.dirs" 2>/dev/null; printf '%s' "$XDG_DESKTOP_DIR")
  fi
  if [ -z "$ddir" ] || [ ! -d "$ddir" ]; then
    for c in "$uhome/桌面" "$uhome/Desktop"; do
      [ -d "$c" ] && ddir="$c" && break
    done
  fi
  [ -n "$ddir" ] && [ -d "$ddir" ] || return 0   # 无桌面目录就不强造,避免污染家目录

  tgt="$ddir/pinvou3.desktop"
  cp -f "$APP_DESKTOP" "$tgt" 2>/dev/null || return 0
  chmod 755 "$tgt" 2>/dev/null || true
  [ -n "$uowner" ] || return 0
  chown "$uowner" "$tgt" 2>/dev/null || true
  # GNOME(Ubuntu ding 扩展)要求 metadata::trusted=true,否则图标灰/首启需右键「允许启动」。
  # 需用户 session 的 dbus,装机时其未必在线 → best-effort;失败则首次右键「允许启动」一次即可。
  uid=$(id -u "$uowner" 2>/dev/null) || return 0
  [ -n "$uid" ] && command -v gio >/dev/null 2>&1 && \
    sudo -u "$uowner" DBUS_SESSION_BUS_ADDRESS="unix:path=/run/user/$uid/bus" \
      gio set "$tgt" metadata::trusted true 2>/dev/null || true
}

for uhome in /home/*; do
  [ -d "$uhome" ] || continue
  uowner=$(stat -c %U "$uhome" 2>/dev/null) || continue
  [ -n "$uowner" ] && [ "$uowner" != "UNKNOWN" ] || continue
  drop_to_desktop "$uhome" "$uowner"
done

exit 0
