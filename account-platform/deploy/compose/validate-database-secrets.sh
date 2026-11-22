#!/bin/sh
set -eu

# Never print secret values. Also run before migrations on existing volumes.
: "${ACCOUNT_MIGRATION_DB_PASSWORD:?set ACCOUNT_MIGRATION_DB_PASSWORD}"
: "${ACCOUNT_APPROVAL_WRITER_DB_PASSWORD:?set ACCOUNT_APPROVAL_WRITER_DB_PASSWORD}"
: "${ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD:?set ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD}"
: "${ACCOUNT_APPROVAL_RELAY_DB_PASSWORD:?set ACCOUNT_APPROVAL_RELAY_DB_PASSWORD}"
: "${BILLING_MIGRATION_DB_PASSWORD:?set BILLING_MIGRATION_DB_PASSWORD}"
: "${TOKEN_MIGRATION_DB_PASSWORD:?set TOKEN_MIGRATION_DB_PASSWORD}"
: "${TOKEN_POLICY_DB_PASSWORD:?set TOKEN_POLICY_DB_PASSWORD}"
if [ "${ENABLE_PAYMENT_SIMULATOR_DB:-true}" != "false" ]; then
  : "${PAYMENT_SIM_DB_PASSWORD:?set PAYMENT_SIM_DB_PASSWORD}"
fi
for deployment_secret in "$ACCOUNT_MIGRATION_DB_PASSWORD" "$ACCOUNT_APPROVAL_WRITER_DB_PASSWORD" "$ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD" "$ACCOUNT_APPROVAL_RELAY_DB_PASSWORD" "$BILLING_MIGRATION_DB_PASSWORD" "$TOKEN_MIGRATION_DB_PASSWORD" "$TOKEN_POLICY_DB_PASSWORD"; do
  for runtime_secret in "$ACCOUNT_DB_PASSWORD" "$BILLING_DB_PASSWORD" "$TOKEN_DB_PASSWORD" "${PAYMENT_SIM_DB_PASSWORD:-}" "$NEWAPI_DB_PASSWORD" "$KEYCLOAK_DB_PASSWORD"; do
    if [ "$deployment_secret" = "$runtime_secret" ]; then
      echo 'Deployment database credentials must differ from all runtime credentials' >&2
      exit 1
    fi
  done
done
deployment_secret_set=''
for deployment_secret in "$ACCOUNT_MIGRATION_DB_PASSWORD" "$ACCOUNT_APPROVAL_WRITER_DB_PASSWORD" "$ACCOUNT_APPROVAL_BACKFILL_DB_PASSWORD" "$ACCOUNT_APPROVAL_RELAY_DB_PASSWORD" "$BILLING_MIGRATION_DB_PASSWORD" "$TOKEN_MIGRATION_DB_PASSWORD" "$TOKEN_POLICY_DB_PASSWORD"; do
  case "|$deployment_secret_set|" in
    *"|$deployment_secret|"*)
      echo 'Migration, approval writer/backfill/relay, and policy credentials must be independent' >&2
      exit 1
      ;;
  esac
  deployment_secret_set="${deployment_secret_set:+$deployment_secret_set|}$deployment_secret"
done
unset deployment_secret deployment_secret_set runtime_secret
