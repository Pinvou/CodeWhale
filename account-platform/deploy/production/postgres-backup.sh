#!/bin/sh
# No arbitrary pgBackRest arguments, SQL, restore, expire or deletion entry point.
set +x
set -eu
. /opt/pinvou/production/postgres-common.sh
[ "$#" -eq 1 ] || db_fail
case "$1" in create|check|full|incr|info|stats) ;; *) db_fail ;; esac
db_ready
db_private_directory /run/pinvou/postgres-backup
db_private_file /run/pinvou/postgres-backup/pgbackrest.conf
db_private_file /run/pinvou/postgres-backup/pgpass
db_private_directory /var/lib/pgbackrest
export PGPASSFILE=/run/pinvou/postgres-backup/pgpass
unset PGPASSWORD
backup_fail() { echo 'POSTGRES_BACKUP_FAILED' >&2; exit 1; }
backup() {
  /usr/bin/pgbackrest --config=/run/pinvou/postgres-backup/pgbackrest.conf --stanza=platform "$@"
}
case "$1" in
  create) backup stanza-create >/dev/null 2>&1 || backup_fail ;;
  check) backup check >/dev/null 2>&1 || backup_fail ;;
  full) backup --type=full backup >/dev/null 2>&1 || backup_fail ;;
  incr) backup --type=incr backup >/dev/null 2>&1 || backup_fail ;;
  info) backup --output=json info 2>/dev/null || backup_fail; exit 0 ;;
  stats)
    psql -X -w -At --set=ON_ERROR_STOP=1 -h /var/run/postgresql -U account_platform_admin -d postgres 2>/dev/null <<'SQL' || backup_fail
SELECT json_build_object(
  'system_identifier', (pg_control_system()).system_identifier::text,
  'archive_mode', current_setting('archive_mode'),
  'archive_timeout', current_setting('archive_timeout'),
  'last_archived_wal', last_archived_wal,
  'last_archived_at', last_archived_time,
  'last_failed_at', last_failed_time,
  'failed_count', failed_count,
  'databases', (SELECT json_agg(datname ORDER BY datname) FROM pg_database WHERE datname LIKE 'account_platform_%')
) FROM pg_stat_archiver;
SQL
    exit 0 ;;
esac
echo 'POSTGRES_BACKUP_COMMAND_PASS'
