import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('every management write is classified and financial facts use the independent approval executor', async () => {
  const [policy, accountService, billingService, tokenService, accountMigration, billingMigration, approvals, approvalAuthorization, execution, billingExecution, billingConsumer, billingAuthorizationDelivery, billingMain, adminOpenAPI, eventSchema, portal, adminView] = await Promise.all([
    read('internal/platform/approvalpolicy/registry.go'),
    read('internal/account/service.go'),
    read('internal/billing/service.go'),
    read('internal/token/service.go'),
    read('migrations/account/000018_approval_commands.up.sql'),
    read('migrations/billing/000017_approval_execution.up.sql'),
    read('internal/account/approvals.go'),
    read('internal/account/approval_authorization.go'),
    read('internal/account/approval_execution.go'),
    read('internal/billing/approval_execution_http.go'),
    read('internal/billing/approval_consumer.go'),
    read('internal/billing/approval_delivery.go'),
    read('cmd/billing-service/main.go'),
    read('contracts/openapi/admin.yaml'),
    read('contracts/events/account.approval-command.authorized.v1.schema.json'),
    read('internal/portal/static/assets/approval-requests.js'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx')
  ]);

  const financialCommands = [
    'WALLET_ADJUSTMENT', 'ENTITLEMENT_COMPONENT_ADJUSTMENT', 'LEDGER_REPAIR', 'RECEIVABLE_WRITE_OFF',
    'PAYMENT_FINANCIAL_REPAIR', 'REFUND_EXCEPTION_REPAIR', 'CHARGEBACK_FINANCIAL_REPAIR',
    'RECONCILIATION_FINANCIAL_REPAIR', 'USAGE_SETTLEMENT_REPAIR'
  ];
  for (const command of financialCommands) {
    assert.match(policy, new RegExp(`\\b${command}\\b`));
    assert.match(accountMigration, new RegExp(`'${command}'`));
  }
  assert.match(policy, /ApprovalCount:\s+1/);
  assert.match(policy, /SelfApprovalForbidden:\s+true/);
  assert.match(policy, /BILLING_NORMAL_REFUND/);
  assert.match(policy, /ACCOUNT_ORGANIZATION_DELETE/);
  assert.match(policy, /ErrPolicyNotRegistered/);
  for (const job of ['ACCOUNT_WALLET_PROVISION', 'ACCOUNT_ROLE_BINDING_EXPIRE', 'ACCOUNT_APPROVAL_EXPIRE', 'ACCOUNT_PROFILE_CHANGE_RECOVERY', 'ACCOUNT_REGISTRATION_RECOVERY', 'ACCOUNT_COMMERCE_CUSTOMER_SYNC', 'BILLING_PAYMENT_EXPIRY_RECONCILE', 'BILLING_ENTITLEMENT_PACKAGE_EXPIRE', 'TOKEN_USAGE_RECONCILE', 'EVENT_OUTBOX_RELAY']) {
    assert.match(policy, new RegExp(job));
    assert.match(accountMigration, new RegExp(`'${job}'`));
  }

  for (const source of [accountService, billingService, tokenService]) {
    const managementWrites = source.split(/\r?\n/).filter(line => /admin\.(POST|PUT|PATCH|DELETE)\(/.test(line));
    assert.ok(managementWrites.length > 0);
    for (const line of managementWrites) {
      if (/admin\.POST\("\/commands"/.test(line)) {
        assert.match(line, /requireNewTokenCommandGovernanceActive\(\).*adminCreateTokenCommand/, `unguarded dynamic management write: ${line.trim()}`);
        continue;
      }
      if (/admin\.POST\("\/commands:precheck"/.test(line)) {
        assert.match(line, /requireNewTokenCommandGovernanceActive\(\).*adminTokenCommandPrecheck/, `unguarded dynamic precheck: ${line.trim()}`);
        continue;
      }
      if (/admin\.POST\("\/commands\/:command_reference\/recovery-actions"/.test(line)) {
        assert.match(line, /requireNewTokenCommandGovernanceActive\(\).*adminTokenAccountRecoveryAction/, `unguarded account recovery: ${line.trim()}`);
        const recovery = await read('internal/token/account_provisioning_saga.go');
        assert.match(recovery, /func \(s \*Service\) adminTokenAccountRecoveryAction[\s\S]*authorizeRecoveryOperator/);
        assert.match(recovery, /approvalpolicy\.Lookup\("TOKEN_ACCOUNT_CREATE"\)/);
        continue;
      }
      if (/admin\.POST\("\/credential-results\/:result_ref\/revoke-window"/.test(line)) {
        assert.match(line, /requireCredentialIsolationEnabled\(\).*adminRevokeCredentialLaunch/, `unguarded one-time credential window abort: ${line.trim()}`);
        continue;
      }
      if (/admin\.POST\("\/models\/:model_id\/lifecycle-plans\/:lifecycle_plan_id\/cancel"/.test(line)) {
        assert.match(line, /authorizer\.RequireAny\("platform\.models\.manage", "platform\.models\.lifecycle\.request"\).*requireNewTokenCommandGovernanceActive\(\)/, `unguarded lifecycle plan cancel: ${line.trim()}`);
        continue;
      }
      if (/admin\.POST\("\/models\/:model_id\/lifecycle-plans"/.test(line)) {
        assert.match(line, /authorizer\.Require\("platform\.models\.lifecycle\.request"\).*requireNewTokenCommandGovernanceActive\(\)/, `unguarded lifecycle plan write: ${line.trim()}`);
        continue;
      }
      assert.match(line, /approvalpolicy\.(?:Require|RequireLegacyTokenDirect)\(/, `unclassified management write: ${line.trim()}`);
    }
  }

  assert.match(accountMigration, /status IN \('DRAFT','SUBMITTED','APPROVED','EXECUTING','EXECUTED','EXECUTION_FAILED','REJECTED','WITHDRAWN','EXPIRED','INVALIDATED'\)/);
  assert.match(accountMigration, /approval_command_policies_immutable/);
  assert.match(accountMigration, /approval_requests_command_immutable/);
  assert.match(accountMigration, /invalid approval request state transition/);
  assert.match(accountMigration, /approval_decisions_validate/);
  assert.match(accountMigration, /requester=NEW\.approver_user_id/);
  assert.match(approvalAuthorization, /account\.approval-command\.authorized\.v1/);
  assert.match(approvals, /ErrApprovalSelfDecision/);
  assert.match(approvals, /rp\.permission_code=p\.approver_permission/);
  assert.match(approvals, /item\.RequesterPermission/);
  assert.match(approvals, /item\.ApproverPermission/);
  assert.match(approvals, /approvalPolicyForItem/);
  assert.doesNotMatch(approvals, /platform\.billing\.repairs\.approve/);
  assert.match(execution, /REQUESTER_AUTH_CHANGED/);
  assert.match(execution, /APPROVER_AUTH_CHANGED/);
  assert.match(execution, /RESOURCE_VERSION_CHANGED/);
  assert.match(billingMigration, /manual_adjustments_require_approval_execution/);
  assert.match(billingMigration, /manual_adjustments_request_idempotency_unique/);
  assert.match(billingExecution, /ExecuteApprovedAdjustment/);
  assert.match(billingConsumer, /generic approval authorization consumption is forbidden/);
  assert.match(billingAuthorizationDelivery, /DecodeAuthorizationEvent/);
  assert.match(billingAuthorizationDelivery, /ResolveAuthorizationBinding/);
  assert.match(billingMain, /broker\.ConsumeAuthorization\(workerContext, "billing-command-executor-v1"/);
  assert.doesNotMatch(billingMain, /broker\.Consume\(workerContext, "billing-command-executor-v1"/);
  assert.match(billingService, /\/execute-approved/);
  assert.doesNotMatch(billingService, /\/adjustments\/:adjustment_id\/approve", httpserver\.AdaptFunc\(s\.approveAdjustment\)/);
  assert.match(adminOpenAPI, /operationId: createAdminApprovalDraft/);
  assert.match(adminOpenAPI, /operationId: decideAdminApprovalRequest/);
  assert.match(eventSchema, /account\.approval-command\.authorized\.v1/);
  for (const language of ['zh', 'en', 'ja']) assert.match(portal, new RegExp(`${language}: \\{`));
  for (const filter of ['risk_class', 'requester_user_id', 'target', 'submitted_from', 'submitted_to', 'expires_from', 'expires_to']) assert.match(portal, new RegExp(filter));
  for (const frozenView of ['normalized_command', 'critical_resource_versions', 'redacted_evidence']) assert.match(portal, new RegExp(frozenView));
  assert.doesNotMatch(portal, /createAdjustment/);
  assert.match(adminView, /await client\.createAdjustment[\s\S]*?await ensureAdjustmentApproval/);
  assert.doesNotMatch(adminView, /client\.decideAdjustment/);
});
