#!/bin/sh
set -eu

. /opt/pinvou/validate-database-secrets.sh

psql --set=ON_ERROR_STOP=1 \
  --username "$POSTGRES_USER" \
  --dbname "$POSTGRES_DB" \
  --set=account_password="$ACCOUNT_DB_PASSWORD" \
  --set=billing_password="$BILLING_DB_PASSWORD" \
  --set=token_password="$TOKEN_DB_PASSWORD" \
  --set=billing_migration_password="$BILLING_MIGRATION_DB_PASSWORD" \
  --set=token_migration_password="$TOKEN_MIGRATION_DB_PASSWORD" \
  --set=token_policy_password="$TOKEN_POLICY_DB_PASSWORD" \
  --set=newapi_password="$NEWAPI_DB_PASSWORD" \
  --set=keycloak_password="$KEYCLOAK_DB_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE account_service LOGIN PASSWORD %L', :'account_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'account_service') \gexec
SELECT format('CREATE ROLE billing_service LOGIN PASSWORD %L', :'billing_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'billing_service') \gexec
SELECT format('CREATE ROLE token_gateway LOGIN PASSWORD %L', :'token_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'token_gateway') \gexec
SELECT 'CREATE ROLE token_cap_policy_deployer NOLOGIN'
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'token_cap_policy_deployer') \gexec
SELECT 'CREATE ROLE token_schema_owner NOLOGIN'
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'token_schema_owner') \gexec
SELECT format('CREATE ROLE token_migrator LOGIN NOINHERIT PASSWORD %L', :'token_migration_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'token_migrator') \gexec
SELECT format('CREATE ROLE token_policy_operator LOGIN NOINHERIT PASSWORD %L', :'token_policy_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'token_policy_operator') \gexec
GRANT token_gateway, token_cap_policy_deployer, token_schema_owner TO token_migrator;
SELECT 'CREATE ROLE billing_wallet_receipt_deployer NOLOGIN'
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'billing_wallet_receipt_deployer') \gexec
SELECT 'CREATE ROLE billing_schema_owner NOLOGIN'
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'billing_schema_owner') \gexec
SELECT format('CREATE ROLE billing_migrator LOGIN NOINHERIT PASSWORD %L', :'billing_migration_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'billing_migrator') \gexec
GRANT billing_service, billing_wallet_receipt_deployer, billing_schema_owner TO billing_migrator;
SELECT format('CREATE ROLE new_api LOGIN PASSWORD %L', :'newapi_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'new_api') \gexec
SELECT format('CREATE ROLE keycloak LOGIN PASSWORD %L', :'keycloak_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'keycloak') \gexec

SELECT 'CREATE DATABASE account_platform_account OWNER account_service'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'account_platform_account') \gexec
SELECT 'CREATE DATABASE account_platform_billing OWNER billing_schema_owner'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'account_platform_billing') \gexec
SELECT 'CREATE DATABASE account_platform_token OWNER token_schema_owner'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'account_platform_token') \gexec
SELECT 'CREATE DATABASE account_platform_newapi OWNER new_api'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'account_platform_newapi') \gexec
SELECT 'CREATE DATABASE account_platform_keycloak OWNER keycloak'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'account_platform_keycloak') \gexec
\connect account_platform_billing
ALTER SCHEMA public OWNER TO billing_schema_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE,CREATE ON SCHEMA public TO billing_service;
\connect account_platform_token
CREATE EXTENSION IF NOT EXISTS pgcrypto;
ALTER SCHEMA public OWNER TO token_schema_owner;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE,CREATE ON SCHEMA public TO token_gateway;
SQL

# Dedicated migration identities can SET ROLE into the existing domain owner;
# runtime services are deliberately not members of deployment-owner roles.

if [ "${ENABLE_PAYMENT_SIMULATOR_DB:-false}" = "true" ]; then
  psql --set=ON_ERROR_STOP=1 \
    --username "$POSTGRES_USER" \
    --dbname "$POSTGRES_DB" \
    --set=payment_sim_password="$PAYMENT_SIM_DB_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE payment_simulator LOGIN PASSWORD %L', :'payment_sim_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'payment_simulator') \gexec
SELECT 'CREATE DATABASE account_platform_payment_sim OWNER payment_simulator'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'account_platform_payment_sim') \gexec
SQL
fi
