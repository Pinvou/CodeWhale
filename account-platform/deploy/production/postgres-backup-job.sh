#!/bin/sh
# A dedicated session contains this job only, never the PostgreSQL archiver.
set +x
set -eu
[ "$#" -eq 2 ] || exit 2
case "$1" in ''|*[!0-9]*) exit 2 ;; esac
[ "${#1}" -le 4 ] && [ "$1" -gt 0 ] || exit 2
case "$2" in full|incr) maximum=2700 ;; create) maximum=45 ;; check) maximum=60 ;; info|stats) maximum=15 ;; *) exit 2 ;; esac
[ "$1" -le "$maximum" ] || exit 2
group=''
started=0
members() {
  for entry in /proc/[0-9]*/stat; do
    [ -r "$entry" ] || continue
    line=$(cat "$entry" 2>/dev/null) || continue
    rest=${line##*) }
    set -- $rest
    [ "$#" -ge 3 ] || continue
    [ "$3" = "$group" ] && [ "$1" != Z ] && return 0
  done
  return 1
}
cleanup() {
  [ -n "$group" ] || return 0
  if members; then
    kill -TERM "-$group" 2>/dev/null || true
    count=0
    while members && [ "$count" -lt 5 ]; do sleep 1; count=$((count + 1)); done
    if members; then kill -KILL "-$group" 2>/dev/null || true; fi
    count=0
    while members && [ "$count" -lt 5 ]; do sleep 1; count=$((count + 1)); done
    members && return 1
  fi
  return 0
}
interrupted() {
  trap '' TERM INT HUP
  # A signal can arrive after spawning setsid but before assigning $!.
  # Unknown ownership must leave the host's durable latch, never acknowledge.
  [ -n "$group" ] && [ "$started" -eq 1 ] || exit 125
  cleanup || exit 125
  printf '\nPOSTGRES_BACKUP_JOB_EXIT_CONFIRMED\n'
  exit 124
}
trap interrupted TERM INT HUP
# Non-interactive shell has no job control: setsid child PID is its PGID.
# --foreground keeps timeout and pgBackRest descendants in this session.
setsid /usr/bin/timeout --foreground --kill-after=5 "$1" /bin/sh /opt/pinvou/production/postgres-backup.sh "$2" &
group=$!
# Assigning $! does not prove that setsid(2) has run. Until the session is
# actually observed, interrupted execution remains unconfirmed.
while kill -0 "$group" 2>/dev/null; do
  if members; then started=1; break; fi
  sleep 0.01
done
result=0
wait "$group" || result=$?
cleanup || exit 125
trap - TERM INT HUP
printf '\nPOSTGRES_BACKUP_JOB_EXIT_CONFIRMED\n'
exit "$result"
