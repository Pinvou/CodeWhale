import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('payment provider and product recovery are version-frozen and independently approved', async () => {
  const [policy, accountMigration, productPolicyMigration, billingMigration, productBillingMigration, recovery, health, consumer, authorizationDomain, service, admin, approvalUI, adminView, adminClient, adminFinance] = await Promise.all([
    read('internal/platform/approvalpolicy/registry.go'),
    read('migrations/account/000033_payment_provider_resume_policy.up.sql'),
    read('migrations/account/000038_payment_product_resume_policy.up.sql'),
    read('migrations/billing/000040_payment_provider_resume_audit.up.sql'),
    read('migrations/billing/000048_payment_product_resume_audit.up.sql'),
    read('internal/billing/provider_recovery.go'),
    read('internal/billing/provider_health.go'),
    read('internal/billing/approval_consumer.go'),
    read('internal/billing/approval_delivery_domain.go'),
    read('internal/billing/service.go'),
    read('contracts/openapi/admin.yaml'),
    read('internal/portal/static/assets/approval-requests.js'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('../pinvou3-app/src/features/account-platform/admin-finance.js'),
  ]);

  assert.match(policy, /PAYMENT_PROVIDER_RESUME/);
  assert.match(policy, /PAYMENT_PRODUCT_RESUME/);
  assert.match(policy, /SelfApprovalForbidden:\s+true/);
  assert.match(accountMigration, /APPROVAL_REQUIRED/);
  assert.match(accountMigration, /platform\.billing\.payment-controls\.resume\.request/);
  assert.match(accountMigration, /platform\.billing\.payment-controls\.resume\.approve/);
  assert.doesNotMatch(accountMigration, /PLATFORM_SUPER_ADMIN/);
  assert.match(billingMigration, /requester_user_id<>approver_user_id/);
  assert.match(billingMigration, /immutable/);
  assert.match(productPolicyMigration, /PAYMENT_PRODUCT_RESUME/);
  assert.match(productPolicyMigration, /PAYMENT_PROVIDER_RESUME',2/);
  assert.doesNotMatch(productPolicyMigration, /UPDATE approval_command_policies/);
  assert.match(productPolicyMigration, /observation_high_watermark/);
  assert.match(productPolicyMigration, /evaluation_high_watermark/);
  assert.match(productBillingMigration, /payment_product_resume_executions/);
  assert.match(productBillingMigration, /immutable/);
  assert.match(recovery, /providerResumeVersions/);
  assert.match(recovery, /productResumeVersions/);
  assert.match(recovery, /providerFailureMatchesOrder/);
	assert.match(recovery, /review\.severity IN \('P0','P1'\)/);
  assert.match(recovery, /payment_provider_failure_confirmations/);
	assert.match(recovery, /mapping\.status='FROZEN'/);
  assert.match(recovery, /payment_observation_gaps/);
  assert.match(recovery, /trusted_probe/);
  assert.match(recovery, /executeProductResume/);
  assert.match(recovery, /UPDATE payment_provider_controls SET new_orders_enabled=TRUE/);
  assert.match(recovery, /UPDATE payment_product_controls SET new_orders_enabled=TRUE/);
  assert.doesNotMatch(recovery, /for productID := range expected\.ProductControlVersions/);
  assert.match(health, /TrustedProbe/);
  assert.match(recovery, /billing\.payment-provider\.resumed\.v1/);
  assert.match(recovery, /billing\.payment-product\.resumed\.v1/);
  assert.match(consumer, /generic approval authorization consumption is forbidden/);
  assert.match(authorizationDomain, /domain\.repository\.executeProviderResume/);
  assert.match(authorizationDomain, /domain\.repository\.executeProductResume/);
  assert.match(authorizationDomain, /domain\.probe\(ctx, observed\.ProbeOrderID\)/);
  assert.match(service, /payment-provider-controls/);
  assert.match(admin, /getPaymentProviderControls/);
  assert.doesNotMatch(approvalUI, /createControlResume|resume_draft|productResumeForms/);
  assert.match(adminClient, /paymentProviderControls/);
  assert.match(adminView, /function PaymentControlsPanel/);
  assert.match(adminFinance, /product \? product\.resume_draft : control\?\.resume_draft/);
  assert.match(adminFinance, /allowed_actions \|\| \[\]\)\.includes\('SUBMIT'\)/);
  assert.match(adminFinance, /submitExistingGovernedApproval\(client, existingApproval\)/);
  assert.match(adminFinance, /createAndSubmitGovernedApproval\(client, draftInput\)/);
  assert.doesNotMatch(adminFinance, /client\.createApprovalDraft\(|client\.submitApprovalRequest\(/);
  assert.match(adminClient, /approvalRequest\(requestID, signal\)/);
  assert.match(adminView, /PAYMENT_PROVIDER_RESUME/);
  assert.match(adminView, /PAYMENT_PRODUCT_RESUME/);
  assert.match(adminView, /source_case_id/);
  assert.match(adminView, /action_applied/);
  assert.match(adminView, /observation_high_watermark/);
  assert.match(adminView, /continueControlResume/);
  assert.match(adminView, /approval_command_type: commandType, approval_target: targetID/);
});
