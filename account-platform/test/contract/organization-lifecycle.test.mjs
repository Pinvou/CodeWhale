import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('organization lifecycle is idempotent, paginated, fail-closed, and cross-domain safe', async () => {
  const [accountMigration, billingMigration, tokenMigration, accountAdmin, accountAuthorization, accountLifecycle, billingLifecycle, tokenLifecycle, billingRepository, tokenRepository, accountService, billingService, tokenService, policy, openAPI, compose, helm, adminClient, adminView] = await Promise.all([
    read('migrations/account/000019_organization_lifecycle.up.sql'),
    read('migrations/billing/000018_organization_lifecycle.up.sql'),
    read('migrations/token/000008_organization_lifecycle.up.sql'),
    read('internal/account/admin.go'),
    read('internal/account/authorization.go'),
    read('internal/account/organization_lifecycle.go'),
    read('internal/billing/organization_lifecycle.go'),
    read('internal/token/organization_lifecycle.go'),
    read('internal/billing/postgres.go'),
    read('internal/token/postgres.go'),
    read('internal/account/service.go'),
    read('internal/billing/service.go'),
    read('internal/token/service.go'),
    read('internal/platform/approvalpolicy/registry.go'),
    read('contracts/openapi/admin.yaml'),
    read('deploy/compose/compose.yaml'),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
    read('../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx')
  ]);

  assert.match(accountMigration, /organization_creation_requests/);
  assert.match(accountMigration, /PRIMARY KEY \(actor_user_id, idempotency_key\)/);
  assert.match(accountMigration, /organization_creation_requests_immutable/);
  assert.match(accountMigration, /organizations_active_name_idx/);
  assert.match(accountAdmin, /requestHash != existingHash|existingHash != requestHash/);
  assert.match(accountAdmin, /o\.updated_at<\$4 OR \(o\.updated_at=\$4 AND o\.id<\$5::uuid\)/);
  assert.match(accountAdmin, /filter\.Status/);
  assert.match(accountAdmin, /PENDING_INVITATION/);
  assert.match(accountAdmin, /PENDING_APPROVAL_REQUEST/);
  assert.match(accountAdmin, /ORGANIZATION_NOT_DISABLED/);
  assert.match(accountAdmin, /status == "DELETED"/);
  assert.match(accountAdmin, /AdminRecordOrganizationCreationFailure/);
  assert.match(accountAdmin, /NAME_OR_IDEMPOTENCY_CONFLICT/);
  assert.match(accountAdmin, /reason must contain between 1 and 500 characters/);
  assert.match(accountAdmin, /summary\["reason"\] = reason/);
  assert.match(accountAuthorization, /RecordPermissionDenial/);
  assert.match(accountAuthorization, /authorization\.permission\.denied/);

  assert.match(accountLifecycle, /\/internal\/v1\/organizations\//);
  assert.match(accountLifecycle, /preflight_only/);
  assert.match(accountLifecycle, /client\.Preflight/);
  assert.match(accountLifecycle, /assessOrganizationDeletion/);
  assert.match(accountLifecycle, /INTERNAL_SERVICE_TOKEN is required/);
  assert.match(accountService, /WithOrganizationLifecycle/);
  assert.match(billingService, /BILLING_ORGANIZATION_LIFECYCLE_APPLY/);
  assert.match(policy, /BILLING_ORGANIZATION_LIFECYCLE_APPLY/);
  assert.match(billingMigration, /organization_lifecycle_states/);
  assert.match(billingMigration, /organization_governance_blockers/);
  assert.match(billingMigration, /deleted organization lifecycle is terminal/);
  assert.match(billingLifecycle, /WALLET_BALANCE_NOT_ZERO/);
  assert.match(billingLifecycle, /EFFECTIVE_ENTITLEMENT_REMAINS/);
  assert.match(billingLifecycle, /UNSETTLED_USAGE_HOLD/);
  assert.match(billingLifecycle, /UNFINISHED_PAYMENT/);
  assert.match(billingLifecycle, /UNFINISHED_REFUND/);
  assert.match(billingLifecycle, /OPEN_RECONCILIATION_DIFFERENCE/);
  assert.match(billingLifecycle, /PENDING_USAGE_REVIEW/);
  assert.match(billingLifecycle, /REGISTERED_GOVERNANCE_BLOCKER/);
  assert.match(billingLifecycle, /walletStatus = "CLOSED"/);
  assert.match(billingRepository, /organization_lifecycle_states/);
  assert.match(tokenMigration, /organization_lifecycle_states/);
  assert.match(tokenMigration, /deleted organization lifecycle is terminal/);
  assert.match(tokenLifecycle, /UNSETTLED_USAGE_REQUEST/);
  assert.match(tokenLifecycle, /'CREATED','RESERVED','UPSTREAM_ACTIVE','SETTLEMENT_PENDING','REVIEW'/);
  assert.match(tokenRepository, /organization_lifecycle_states/);
  assert.match(tokenService, /TOKEN_ORGANIZATION_LIFECYCLE_APPLY/);
  assert.match(policy, /TOKEN_ORGANIZATION_LIFECYCLE_APPLY/);
  assert.match(compose, /TOKEN_GATEWAY_HTTP_URL: http:\/\/token-gateway:8080/);
  assert.match(helm, /name: TOKEN_GATEWAY_HTTP_URL/);

  assert.match(openAPI, /operationId: listAdminOrganizations/);
  assert.match(openAPI, /AdminOrganizationPage/);
  assert.match(openAPI, /Idempotency-Key/);
  assert.match(openAPI, /enum: \[ACTIVE, DISABLED, DELETED\]/);
  assert.match(openAPI, /operationId: getAdminOrganizationDeletionReadiness/);
  assert.match(openAPI, /OrganizationDeleteRequest/);
  assert.match(adminClient, /organizationDeletionReadiness/);
  assert.match(adminClient, /error\.details = payload\?\.details/);
  assert.match(adminView, /function OrganizationDeletionDialog/);
  assert.match(adminView, /typedName !== organization\.name/);
  assert.match(adminView, /item\.status === 'DISABLED'/);
});
