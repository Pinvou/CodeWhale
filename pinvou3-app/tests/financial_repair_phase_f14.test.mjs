import test from 'node:test';
import assert from 'node:assert/strict';
import { financialRepairCommands, financialRepairApprovalDraft, financialRepairPrecheck } from '../src/features/account-platform/admin-financial-repair-drafts.js';
import { financialRepairInputs, financialRepairPrecheckBody } from '../src/features/account-platform/admin-financial-repair-form.js';
const command = "CHARGEBACK_FINANCIAL_REPAIR";
const target = "11111111-1111-4111-8111-111111111111";
const normalized = {"chargeback_case_id": "11111111-1111-4111-8111-111111111111", "payment_evidence_id": "22222222-2222-4222-8222-222222222222", "expected_case_version": "9007199254740993"};
const projection = { command_type: command, policy_version: 1, snapshot_schema_version: 1,
 target_resource_type: financialRepairCommands[command].targetResourceType, target_resource_id: target,
 normalized_command: normalized, critical_resource_versions: { version: '9007199254740993' },
 financial_snapshot: { original_microcredits: '9007199254740993' } };

test('F14 financial descriptor set exposes only delivered operations', () => {
 assert.deepEqual(Object.keys(financialRepairCommands), ["LEDGER_REPAIR", "RECEIVABLE_WRITE_OFF", "PAYMENT_FINANCIAL_REPAIR", "REFUND_EXCEPTION_REPAIR", "CHARGEBACK_FINANCIAL_REPAIR"]);
 assert.deepEqual(Object.keys(financialRepairInputs), ["LEDGER_REPAIR", "RECEIVABLE_WRITE_OFF", "PAYMENT_FINANCIAL_REPAIR", "REFUND_EXCEPTION_REPAIR", "CHARGEBACK_FINANCIAL_REPAIR"]);
 assert.equal(financialRepairPrecheck({ ...projection, command_type: 'UNKNOWN_REPAIR' }), null);
});

test('F14 owner target and complete normalized facts bind the request', () => {
 assert.ok(financialRepairPrecheck(projection));
 assert.equal(financialRepairPrecheck({ ...projection, target_resource_id: '33333333-3333-4333-8333-333333333333' }), null);
 for (const field of financialRepairCommands[command].fields) {
  const facts = { ...normalized }; delete facts[field.key];
  assert.equal(financialRepairPrecheck({ ...projection, normalized_command: facts }), null);
 }
 const input = { target_resource_id: target, payment_evidence_id: "22222222-2222-4222-8222-222222222222", business_date: '2026-10-07', usage_request_id: "22222222-2222-4222-8222-222222222222", loss_review_id: "22222222-2222-4222-8222-222222222222", untrusted_amount: '10' };
 const accepted = financialRepairPrecheckBody(command, input);
 assert.ok(accepted.body);
 assert.ok(!Object.hasOwn(accepted.body, 'untrusted_amount'));
 assert.ok(financialRepairPrecheckBody(command, { ...input, target_resource_id: 'not-an-owner-id' }).error);
});

test('F14 draft preserves int64 owner documents and closed authority', () => {
 const polluted = { ...projection, normalized_command: { ...normalized, attacker_amount: '10' } };
 const draft = financialRepairApprovalDraft(polluted, 'A legitimate operator explanation', 'f14-logical-attempt');
 assert.equal(draft.command_type, command);
 assert.equal(draft.target_resource_id, target);
 assert.strictEqual(draft.critical_resource_versions, projection.critical_resource_versions);
 assert.strictEqual(draft.financial_snapshot, projection.financial_snapshot);
 assert.equal(draft.financial_snapshot.original_microcredits, '9007199254740993');
 assert.equal(draft.normalized_command[financialRepairCommands[command].reasonKey], financialRepairCommands[command].reasonCode);
 assert.ok(!Object.hasOwn(draft.normalized_command, 'attacker_amount'));
 assert.throws(() => financialRepairApprovalDraft(projection, 'short', 'f14-logical-attempt'));
});
