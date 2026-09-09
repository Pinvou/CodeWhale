#!/bin/sh
set +x
set -eu
. /opt/pinvou/production/postgres-common.sh
db_ready
db_secret /run/pinvou/postgres-admin/password
export PGPASSWORD="$DB_SECRET"
unset DB_SECRET
[ "$(psql -X -At --set=ON_ERROR_STOP=1 -h /var/run/postgresql -U account_platform_admin -d postgres -c 'SELECT 1' 2>/dev/null)" = '1' ] || db_fail
db_utf8 account_platform_admin
