#!/bin/sh
set -eu

# Never print secret values. Also run before migrations on existing volumes.
: "${BILLING_MIGRATION_DB_PASSWORD:?set BILLING_MIGRATION_DB_PASSWORD}"
: "${TOKEN_MIGRATION_DB_PASSWORD:?set TOKEN_MIGRATION_DB_PASSWORD}"
: "${TOKEN_POLICY_DB_PASSWORD:?set TOKEN_POLICY_DB_PASSWORD}"
if [ "${ENABLE_PAYMENT_SIMULATOR_DB:-true}" != "false" ]; then
  : "${PAYMENT_SIM_DB_PASSWORD:?set PAYMENT_SIM_DB_PASSWORD}"
fi
for deployment_secret in "$BILLING_MIGRATION_DB_PASSWORD" "$TOKEN_MIGRATION_DB_PASSWORD" "$TOKEN_POLICY_DB_PASSWORD"; do
  for runtime_secret in "$ACCOUNT_DB_PASSWORD" "$BILLING_DB_PASSWORD" "$TOKEN_DB_PASSWORD" "${PAYMENT_SIM_DB_PASSWORD:-}" "$NEWAPI_DB_PASSWORD" "$KEYCLOAK_DB_PASSWORD"; do
    if [ "$deployment_secret" = "$runtime_secret" ]; then
      echo 'Deployment database credentials must differ from all runtime credentials' >&2
      exit 1
    fi
  done
done
if [ "$BILLING_MIGRATION_DB_PASSWORD" = "$TOKEN_MIGRATION_DB_PASSWORD" ] ||
   [ "$BILLING_MIGRATION_DB_PASSWORD" = "$TOKEN_POLICY_DB_PASSWORD" ] ||
   [ "$TOKEN_MIGRATION_DB_PASSWORD" = "$TOKEN_POLICY_DB_PASSWORD" ]; then
  echo 'Migration and policy operator credentials must be independent' >&2
  exit 1
fi
unset deployment_secret runtime_secret
