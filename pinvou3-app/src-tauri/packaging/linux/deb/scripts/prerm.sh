#!/bin/sh
# pinvou3 .deb 卸载前清理：删除超级权限 sudoers 文件。
# 否则用户不先在 UI 关 toggle 直接 apt remove pinvou3,会留下 NOPASSWD: ALL 授权 —— 安全黑洞。
# 失败不阻塞卸载(set -e 不开)。
rm -f /etc/sudoers.d/pinvou3 2>/dev/null || true

# ── docker 组对等回收（r33 P1-2：postinst 自动加入的授权随包走） ──
# 只回收 postinst 登记过的用户（/var/lib/pinvou3/docker-group.added）；
# 管理员手工加入的不在名单上，不动。仅在 remove/purge 时执行——
# upgrade 时新包 postinst 会重新加入，先删后加只会白闪一次组员身份。
if [ "$1" = "remove" ] || [ "$1" = "purge" ]; then
  DOCKER_GROUP_STATE=/var/lib/pinvou3/docker-group.added
  if [ -f "$DOCKER_GROUP_STATE" ]; then
    while IFS= read -r u; do
      [ -n "$u" ] || continue
      if command -v gpasswd >/dev/null 2>&1; then
        gpasswd -d "$u" docker >/dev/null 2>&1 || true
      elif command -v deluser >/dev/null 2>&1; then
        deluser "$u" docker >/dev/null 2>&1 || true
      fi
      echo "[pinvou3] 已将 $u 移出 docker 组（该授权随包卸载回收）" >&2
    done < "$DOCKER_GROUP_STATE"
    rm -f "$DOCKER_GROUP_STATE" 2>/dev/null || true
  fi
fi

# 清理 postinst 放到各用户桌面的快捷方式(本地化目录名 + XDG 解析,与 postinst 对称)。
for uhome in /home/*; do
  [ -d "$uhome" ] || continue
  for c in "$uhome/桌面/pinvou3.desktop" "$uhome/Desktop/pinvou3.desktop"; do
    rm -f "$c" 2>/dev/null || true
  done
  if [ -r "$uhome/.config/user-dirs.dirs" ]; then
    ddir=$(. "$uhome/.config/user-dirs.dirs" 2>/dev/null; printf '%s' "$XDG_DESKTOP_DIR")
    [ -n "$ddir" ] && rm -f "$ddir/pinvou3.desktop" 2>/dev/null || true
  fi
done
exit 0
