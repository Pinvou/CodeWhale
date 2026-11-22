#!/bin/sh
set +x
set -eu
. /opt/pinvou/production/postgres-common.sh
[ "$#" -eq 0 ] || db_fail
db_private_directory /run/pinvou/postgres-tls
db_private_file /run/pinvou/postgres-tls/server.key
db_secret /run/pinvou/postgres-admin/password
unset DB_SECRET
if [ -e "$PGDATA/PG_VERSION" ]; then
  db_ready
else
  [ -z "$(find "$PGDATA" -mindepth 1 -maxdepth 1 -print -quit)" ] || db_fail
fi
exec /usr/local/bin/docker-entrypoint.sh postgres -c config_file=/etc/pinvou/postgres/postgres.conf
