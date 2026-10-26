#!/bin/sh
# pinvou3 .deb 装后处理:
#   1) 刷新 desktop 数据库 + 图标缓存 → 应用菜单/启动器立即出现带图标的 pinvou3(免重新登录)
#   2) 在每个真实用户桌面放一个快捷方式(Windows 习惯;Linux .deb 默认只进菜单不上桌面)
# 全程 best-effort,任何失败都不阻塞安装(故意不开 set -e)。

APP_DESKTOP=/usr/share/applications/pinvou3.desktop

# ── 1) 缓存刷新 ──────────────────────────────────────────────
command -v update-desktop-database >/dev/null 2>&1 && \
  update-desktop-database -q /usr/share/applications 2>/dev/null || true
command -v gtk-update-icon-cache >/dev/null 2>&1 && \
  gtk-update-icon-cache -q -t -f /usr/share/icons/hicolor 2>/dev/null || true

# ── 2) 桌面快捷方式 ──────────────────────────────────────────
[ -f "$APP_DESKTOP" ] || exit 0

# 模型商店开机自启（2026-10-23）：systemd user 服务要在用户未登录时也能跑，
# 需要 loginctl enable-linger。best-effort：失败不阻塞安装，只影响自启时机
# （用户登录后 GUI 启动时仍会托管拉起，见 service.rs autostart_pending）。
# r6 P1-3：只对**用过品悟**的用户开（~/.pinvou3 存在）——enable-linger 是持久
# 的登录态变更，给从未用过品悟的账号开等于无谓扩大后台常驻面。
enable_linger() {
  uhome="$1"; uowner="$2"
  [ -d "$uhome/.pinvou3" ] || return 0
  command -v loginctl >/dev/null 2>&1 || return 0
  loginctl enable-linger "$uowner" 2>/dev/null || true
}

# 服务单元随包落在 /usr/lib/systemd/user/ 后还要「enable」才进开机链。装机时
# 用户的 session dbus 未必在线（systemctl --user 需要），直接做等价的文件操作：
# default.target.wants 软链——与 systemctl --user enable 落盘结果一致。
# 与 enable_linger 同判据：只给用过品悟的用户挂（r6 P1-3）。
link_autostart() {
  uhome="$1"; uowner="$2"
  [ -d "$uhome/.pinvou3" ] || return 0
  UNIT=/usr/lib/systemd/user/pinvou3-model-store-autostart.service
  [ -f "$UNIT" ] || return 0
  wants="$uhome/.config/systemd/user/default.target.wants"
  mkdir -p "$wants" 2>/dev/null || return 0
  chown -R "$uowner" "$uhome/.config/systemd" 2>/dev/null || true
  ln -sfn "$UNIT" "$wants/pinvou3-model-store-autostart.service" 2>/dev/null || true
}

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
  enable_linger "$uhome" "$uowner"
  link_autostart "$uhome" "$uowner"
done

exit 0
