#!/bin/sh
set +x
set -eu
. /opt/pinvou/production/postgres-common.sh

# The official entrypoint already set PGPASSWORD for this socket-only phase.
[ "$(psql -X -At --set=ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d postgres -c "SELECT count(*) FROM pg_roles WHERE rolname NOT LIKE 'pg_%' AND rolname <> 'account_platform_admin'")" = '0' ] || db_fail
[ "$(psql -X -At --set=ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d postgres -c "SELECT count(*) FROM pg_database WHERE datname NOT IN ('postgres','template0','template1')")" = '0' ] || db_fail
db_secret /run/pinvou/postgres-admin/password
db_seen=" $DB_SECRET "
for db_name in ACCOUNT_DB_PASSWORD BILLING_DB_PASSWORD TOKEN_DB_PASSWORD ACCOUNT_MIGRATION_DB_PASSWORD BILLING_MIGRATION_DB_PASSWORD TOKEN_MIGRATION_DB_PASSWORD TOKEN_POLICY_DB_PASSWORD NEWAPI_DB_PASSWORD KEYCLOAK_DB_PASSWORD; do
  db_secret "/run/pinvou/postgres-bootstrap/$db_name"
  case "$db_seen" in *" $DB_SECRET "*) db_fail ;; esac
  db_seen="$db_seen$DB_SECRET "
  export "$db_name=$DB_SECRET"
done
unset DB_SECRET db_seen db_name
export ENABLE_PAYMENT_SIMULATOR_DB=false
. /opt/pinvou/init-databases.sh
unset ACCOUNT_DB_PASSWORD BILLING_DB_PASSWORD TOKEN_DB_PASSWORD ACCOUNT_MIGRATION_DB_PASSWORD BILLING_MIGRATION_DB_PASSWORD TOKEN_MIGRATION_DB_PASSWORD TOKEN_POLICY_DB_PASSWORD NEWAPI_DB_PASSWORD KEYCLOAK_DB_PASSWORD
db_utf8 "$POSTGRES_USER"

# Final durable marker: no marker on failed/partial initialization, no overwrite.
db_control_id
(set -C; printf 'pinvou-production-postgres-v2-utf8 %s\n' "$DB_CONTROL_ID" > "$PGDATA/.pinvou-bootstrap-complete")
sync "$PGDATA/.pinvou-bootstrap-complete"
sync "$PGDATA"
db_ready
