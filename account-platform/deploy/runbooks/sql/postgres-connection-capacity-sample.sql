\set ON_ERROR_STOP on
BEGIN TRANSACTION READ ONLY;
SET LOCAL search_path = pg_catalog, pg_temp;
SET LOCAL statement_timeout = '5s';
SET LOCAL lock_timeout = '2s';
SET LOCAL TIME ZONE 'UTC';
SET LOCAL DateStyle = 'ISO, YMD';

SELECT 1 / CASE WHEN current_database()::text = :'expected_database' THEN 1 ELSE 0 END
    AS connection_sample_target_guard \gset

SELECT statement_timestamp() AS captured_at_utc,
    pg_postmaster_start_time() AS server_started_at_utc,
    current_setting('server_version_num')::integer AS server_version_num,
    current_setting('max_connections')::integer AS max_connections,
    current_setting('superuser_reserved_connections')::integer AS superuser_reserved_connections,
    COALESCE(current_setting('reserved_connections', true)::integer, 0) AS reserved_connections,
    sum(numbackends) - 1 AS server_database_backends,
    sum(numbackends) FILTER (WHERE datname = current_database()) - 1 AS target_database_backends
FROM pg_stat_database
HAVING sum(numbackends) FILTER (WHERE datname = current_database()) > 0 \gset

SELECT :'captured_at_utc'::timestamptz AS captured_at_utc,
    :'server_started_at_utc'::timestamptz AS server_started_at_utc,
    :'server_version_num'::integer AS server_version_num,
    :'max_connections'::integer AS max_connections,
    :'superuser_reserved_connections'::integer AS superuser_reserved_connections,
    :'reserved_connections'::integer AS reserved_connections,
    :'server_database_backends'::bigint AS server_database_backends,
    :'target_database_backends'::bigint AS target_database_backends;

ROLLBACK;
