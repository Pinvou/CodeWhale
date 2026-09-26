import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('versioned metering and pricing persist deterministic immutable facts', async () => {
  const [billingMigration, tokenMigration, resolution, lifecycle, metering] = await Promise.all([
    read('migrations/billing/000022_versioned_metering_pricing.up.sql'),
    read('migrations/token/000010_protocol_formula_versions.up.sql'),
    read('internal/billing/pricing_resolution.go'),
    read('internal/billing/pricing_admin_v2.go'),
    read('internal/token/metering.go'),
  ]);

  assert.match(billingMigration, /CREATE TABLE metering_unit_definitions/);
  assert.match(billingMigration, /FIXED_PRICE','INHERIT_GLOBAL/);
  assert.match(billingMigration, /CREATE TABLE pricing_lifecycle_commands/);
  assert.match(billingMigration, /ADD COLUMN pricing_snapshot JSONB/);
  assert.match(billingMigration, /ADD COLUMN request_snapshot JSONB/);
  assert.match(tokenMigration, /CREATE TABLE protocol_formula_versions/);
  assert.match(tokenMigration, /PUBLISHED','DEPRECATED/);
  assert.match(resolution, /ErrPriceAmbiguous/);
  assert.match(resolution, /configuration\.configType == "INHERIT_GLOBAL"/);
  assert.match(lifecycle, /lockTransactionKey\(ctx, tx, "pricing\.space"/);
  assert.match(lifecycle, /FALLBACK_GLOBAL/);
  assert.match(metering, /versioned, deterministic byte rule/);
});

test('gateway reserves the request maximum and authorizes final overage before stream success', async () => {
  const [proxy, proxyTests, billingClient, reconciliation] = await Promise.all([
    read('internal/token/proxy.go'),
    read('internal/token/proxy_test.go'),
    read('internal/token/billing.go'),
    read('internal/token/reconciliation.go'),
  ]);

  assert.match(proxy, /renewHold\(/);
  assert.match(proxy, /maximumUsage, err := EstimateChatUsage\(body, maximumOutput\)/);
  assert.match(proxy, /maximumCost, err := Cost\(price, maximumUsage\)/);
  assert.match(proxy, /MaximumCost: maximumCost/);
  assert.match(proxy, /client\.Extend\(/);
  assert.match(proxy, /return actual, walletInsufficient\(err\), true/);
  assert.match(proxyTests, /TestProxyStreamDoesNotChargeSSEEnvelopeBytesBeyondReservedOutput/);
  assert.match(proxyTests, /TestProxyStreamAuthorizesAdditionalUsageBeforeDone/);
  assert.match(proxyTests, /TestProxyStreamCommitsTrustedUsageAndWarnsWhenAdditionalAuthorizationIsInsufficient/);
  assert.match(proxyTests, /TestProxyStreamReportsSettlementPendingWhenCommitFails/);
  assert.match(proxyTests, /TestProxyStreamRoutesDefinitiveCommitConflictToReview/);
  assert.match(billingClient, /"additional_microcredits": additional/);
  assert.match(reconciliation, /PricingSnapshot/);
  assert.match(reconciliation, /PromptTokens - cached/);
});

test('wallet insufficient notice freezes organization purchase routing', async () => {
  const contract = YAML.parse(await read('contracts/openapi/token-gateway.yaml'));
  const notice = contract.components.schemas.BillingNotice;
  assert.deepEqual(notice.properties.action.enum, ['OPEN_RECHARGE', 'OPEN_ORGANIZATION_RECHARGE', 'CONTACT_ORG_ADMIN']);
  assert.equal(notice.properties.organization_id.format, 'uuid');
  assert.deepEqual(notice.allOf[0].then.required, ['organization_id']);
  assert.deepEqual(notice.allOf[0].then.properties.action.enum, ['OPEN_ORGANIZATION_RECHARGE', 'CONTACT_ORG_ADMIN']);
  assert.equal(notice.allOf[0].else.properties.action.const, 'OPEN_RECHARGE');
});

test('response-only settlement recovery distinguishes evidence and safe rejection release', async () => {
  const [reconciliation, replayRepository, proxyTests] = await Promise.all([
    read('internal/token/reconciliation.go'),
    read('internal/token/replay_repository.go'),
    read('internal/token/proxy_test.go'),
  ]);
  assert.match(reconciliation, /RecoverLegacyResponseStateSplits/);
  assert.match(reconciliation, /status='UPSTREAM_ACTIVE' AND execution_status='SETTLEMENT_PENDING'/);
  assert.match(replayRepository, /responseDispositionUsageEvidenceRequired/);
  assert.match(replayRepository, /responseDispositionRejectedBeforeGeneration/);
  assert.match(proxyTests, /TestNonStreamingMissingUsagePersistsProviderEvidenceRecoveryState/);
  assert.match(proxyTests, /TestPreGenerationRejectionSurvivesReleaseFailureForReconciliation/);
});

test('admin contract and React workspace expose units, price lifecycle, protocols, and formulas', async () => {
  const [source, page, workspace, copy] = await Promise.all([
    read('contracts/openapi/admin.yaml'),
    read('internal/portal/static/admin.html'),
    read('../pinvou3-app/src/features/account-platform/PricingLifecycleWorkspace.jsx'),
    read('../pinvou3-app/src/shared/admin-platform-i18n.js'),
  ]);
  const contract = YAML.parse(source);

  for (const route of [
    '/billing/admin/v1/metering-units',
    '/billing/admin/v1/pricing',
    '/billing/admin/v1/pricing/{price_version_id}/publish',
    '/billing/admin/v1/pricing/{price_version_id}/transition',
    '/token/admin/v1/protocols',
    '/token/admin/v1/protocols/{protocol_definition_id}/formulas',
    '/token/admin/v1/formulas/{formula_version_id}/preview',
  ]) assert.ok(contract.paths[route], `missing admin route ${route}`);

  assert.doesNotMatch(page, /pricing-admin\.js|pricing-admin-root/);
  assert.match(workspace, /createPriceDraft/);
  assert.match(workspace, /createMeteringUnit/);
  assert.equal((copy.match(/pricingLifecycle:/g) || []).length, 3);
});
