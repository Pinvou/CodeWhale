import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('payment provider recovery is version-frozen and independently approved', async () => {
  const [policy, accountMigration, billingMigration, recovery, consumer, service, admin, approvalUI] = await Promise.all([
    read('internal/platform/approvalpolicy/registry.go'),
    read('migrations/account/000033_payment_provider_resume_policy.up.sql'),
    read('migrations/billing/000040_payment_provider_resume_audit.up.sql'),
    read('internal/billing/provider_recovery.go'),
    read('internal/billing/approval_consumer.go'),
    read('internal/billing/service.go'),
    read('contracts/openapi/admin.yaml'),
    read('internal/portal/static/assets/approval-requests.js'),
  ]);

  assert.match(policy, /PAYMENT_PROVIDER_RESUME/);
  assert.match(policy, /SelfApprovalForbidden:\s+true/);
  assert.match(accountMigration, /APPROVAL_REQUIRED/);
  assert.match(accountMigration, /platform\.billing\.payment-controls\.resume\.request/);
  assert.match(accountMigration, /platform\.billing\.payment-controls\.resume\.approve/);
  assert.doesNotMatch(accountMigration, /PLATFORM_SUPER_ADMIN/);
  assert.match(billingMigration, /requester_user_id<>approver_user_id/);
  assert.match(billingMigration, /immutable/);
  assert.match(recovery, /providerResumeVersions/);
  assert.match(recovery, /providerFailureMatchesOrder/);
  assert.match(recovery, /review\.severity IN \('P0','P1'\)/);
  assert.match(recovery, /payment_provider_failure_confirmations/);
  assert.match(recovery, /status='FROZEN'/);
  assert.match(recovery, /billing\.payment-provider\.resumed\.v1/);
  assert.match(consumer, /executeApprovedProviderResumeCommand/);
  assert.match(service, /payment-provider-controls/);
  assert.match(admin, /getPaymentProviderControls/);
  assert.match(approvalUI, /createProviderResume/);
  assert.match(approvalUI, /resume_draft/);
  assert.match(approvalUI, /PAYMENT_PROVIDER_RESUME|draftInput/);
});
