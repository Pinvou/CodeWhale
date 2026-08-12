#!/bin/sh
set -eu

psql --set=ON_ERROR_STOP=1 \
  --username "$POSTGRES_USER" \
  --dbname "$POSTGRES_DB" \
  --set=account_password="$ACCOUNT_DB_PASSWORD" \
  --set=billing_password="$BILLING_DB_PASSWORD" \
  --set=token_password="$TOKEN_DB_PASSWORD" \
  --set=payment_sim_password="$PAYMENT_SIM_DB_PASSWORD" \
  --set=keycloak_password="$KEYCLOAK_DB_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE account_service LOGIN PASSWORD %L', :'account_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'account_service') \gexec
SELECT format('CREATE ROLE billing_service LOGIN PASSWORD %L', :'billing_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'billing_service') \gexec
SELECT format('CREATE ROLE token_gateway LOGIN PASSWORD %L', :'token_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'token_gateway') \gexec
SELECT format('CREATE ROLE payment_simulator LOGIN PASSWORD %L', :'payment_sim_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'payment_simulator') \gexec
SELECT format('CREATE ROLE keycloak LOGIN PASSWORD %L', :'keycloak_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'keycloak') \gexec

SELECT 'CREATE DATABASE account_platform_account OWNER account_service'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'account_platform_account') \gexec
SELECT 'CREATE DATABASE account_platform_billing OWNER billing_service'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'account_platform_billing') \gexec
SELECT 'CREATE DATABASE account_platform_token OWNER token_gateway'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'account_platform_token') \gexec
SELECT 'CREATE DATABASE account_platform_payment_sim OWNER payment_simulator'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'account_platform_payment_sim') \gexec
SELECT 'CREATE DATABASE account_platform_keycloak OWNER keycloak'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'account_platform_keycloak') \gexec
SQL
