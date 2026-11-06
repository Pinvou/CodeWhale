#!/bin/sh
# Only sourced by the fixed production entrypoint, initialization and healthcheck.
# Do not enable tracing: temporary shell variables contain bootstrap passwords.
set +x
set -eu
umask 077
export LC_ALL=C

db_fail() { echo 'PRODUCTION_DATABASE_CONFIGURATION_REJECTED' >&2; exit 1; }

db_private_directory() {
  [ -d "$1" ] && [ ! -L "$1" ] || db_fail
  [ "$(stat -c '%u:%g:%a' "$1")" = '999:999:700' ] || db_fail
}

db_private_file() {
  [ -f "$1" ] && [ ! -L "$1" ] || db_fail
  [ "$(stat -c '%u:%g:%a' "$1")" = '999:999:600' ] || db_fail
}

db_secret() {
  db_private_directory "$(dirname "$1")"
  db_private_file "$1"
  [ "$(wc -c < "$1")" -le 65 ] || db_fail
  DB_SECRET=$(cat "$1")
  [ "${#DB_SECRET}" -eq 64 ] || db_fail
  case "$DB_SECRET" in *[!0-9a-f]*) db_fail ;; esac
}

db_control_id() {
  DB_CONTROL_ID=$(pg_controldata -D "$PGDATA" | awk -F ': *' '/^Database system identifier:/ { print $2 }')
  case "$DB_CONTROL_ID" in ''|*[!0-9]*) db_fail ;; esac
}

db_utf8() {
  [ "$#" -eq 1 ] || db_fail
  [ -n "$1" ] || db_fail
  [ "$(psql -X -At --set=ON_ERROR_STOP=1 -h /var/run/postgresql -U "$1" -d postgres -c \
    "SELECT count(*)=8 AND bool_and(datname=ANY(ARRAY['postgres','template0','template1','account_platform_account','account_platform_billing','account_platform_token','account_platform_newapi','account_platform_keycloak'])) AND bool_and(pg_encoding_to_char(encoding)='UTF8' AND datcollate='C' AND datctype='C') FROM pg_database" 2>/dev/null)" = 't' ] || db_fail
}

db_ready() {
  db_private_file "$PGDATA/.pinvou-bootstrap-complete"
  [ "$(wc -c < "$PGDATA/.pinvou-bootstrap-complete")" -le 80 ] || db_fail
  db_control_id
  [ "$(cat "$PGDATA/.pinvou-bootstrap-complete")" = "pinvou-production-postgres-v2-utf8 $DB_CONTROL_ID" ] || db_fail
}

[ "$(id -u):$(id -g)" = '999:999' ] || db_fail
[ "${PGDATA:-}" = '/var/lib/postgresql/data' ] || db_fail
db_private_directory "$PGDATA"
