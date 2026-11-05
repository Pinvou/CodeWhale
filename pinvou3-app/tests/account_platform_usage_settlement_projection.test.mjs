import test from 'node:test';
import assert from 'node:assert/strict';
import { financialRepairPrecheck, financialRepairApprovalDraft } from '../src/features/account-platform/admin-financial-repair-drafts.js';
import { financialRepairPrecheckBody } from '../src/features/account-platform/admin-financial-repair-form.js';
import { currentApprovalEditDraft } from '../src/features/account-platform/admin-finance.js';

const id = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const projection = () => ({ command_type: 'USAGE_SETTLEMENT_REPAIR', policy_version: 1, snapshot_schema_version: 1,
  target_resource_type: 'USAGE_HOLD', target_resource_id: id(1),
  normalized_command: { hold_id: id(1), usage_request_id: id(2), loss_review_id: id(3), evidence_kind: 'LOSS_REVIEW', reason_code: 'MISSING_USAGE_SETTLEMENT' },
  critical_resource_versions: { hold_updated_at: '2026-10-07T00:00:00.123456789Z', loss_review_updated_at: '2026-10-07T00:00:00.123456789Z', facts_digest: 'a'.repeat(64), policy_version: 1 },
  financial_snapshot: { expected_microcredits: '9223372036854775807', settled_microcredits: '9007199254740993', settlement_microcredits: '9007199254740993', overage_microcredits: '9214364837600034814', wallet_id: id(4), wallet_version: '9007199254740993', allocations_digest: 'b'.repeat(64) } });

test('Usage exact frozen facts refuse incomplete, contaminated and inconsistent owner documents', () => {
  assert.ok(financialRepairPrecheck(projection()));
  const changes = [
    p => { delete p.critical_resource_versions.hold_updated_at; },
    p => { p.critical_resource_versions.extra = 1; },
    p => { p.critical_resource_versions.policy_version = 2; },
    p => { p.critical_resource_versions.loss_review_updated_at = 'invalid'; },
    p => { p.critical_resource_versions.facts_digest = 'A'.repeat(64); },
    p => { delete p.financial_snapshot.allocations_digest; },
    p => { p.financial_snapshot.extra = 1; },
    p => { p.financial_snapshot.wallet_version = '0'; },
    p => { p.financial_snapshot.wallet_id = '00000000-0000-0000-0000-000000000000'; },
    p => { p.financial_snapshot.settlement_microcredits = '1'; },
    p => { p.financial_snapshot.overage_microcredits = '-1'; },
    p => { p.financial_snapshot.expected_microcredits = '9223372036854775806'; },
    p => { p.financial_snapshot.settled_microcredits = 9007199254740992; },
    p => { p.normalized_command.reason_code = 'OTHER'; },
  ];
  for (const change of changes) { const p = projection(); change(p); assert.equal(financialRepairPrecheck(p), null); }
});

test('Usage precheck and normalized draft do not propagate unrelated caller fields', () => {
  const body = financialRepairPrecheckBody('USAGE_SETTLEMENT_REPAIR', { target_resource_id: id(1), usage_request_id: id(2), loss_review_id: id(3), expected_version: '99', amount_microcredits: '1' }).body;
  assert.deepEqual(Object.keys(body), ['command_type', 'target_resource_id', 'usage_request_id', 'loss_review_id', 'reason_code']);
  const p = projection(); p.normalized_command.payment_evidence_id = id(4);
  const draft = financialRepairApprovalDraft(p, 'Controlled usage repair explanation', 'usage-repair-key');
  assert.equal(Object.prototype.hasOwnProperty.call(draft.normalized_command, 'payment_evidence_id'), false);
});

test('Usage EDIT refuses changing any original identity and retains frozen original intent', async () => {
  const p = projection();
  const detail = { approval_request_id: id(9), status: 'DRAFT', allowed_actions: ['EDIT'], command_type: p.command_type, target: { resource_type: 'USAGE_HOLD', resource_id: id(1) }, command: p.normalized_command };
  for (const key of ['hold_id', 'usage_request_id', 'loss_review_id']) {
    const changed = projection(); changed.normalized_command[key] = id(4);
    await assert.rejects(currentApprovalEditDraft({ approvalCommandPrecheck: async () => changed }, detail, 'Controlled usage repair explanation'));
  }
  await assert.rejects(currentApprovalEditDraft({ approvalCommandPrecheck: async () => p }, { ...detail, target: { resource_type: 'USAGE_HOLD', resource_id: id(4) } }, 'Controlled usage repair explanation'));
});
