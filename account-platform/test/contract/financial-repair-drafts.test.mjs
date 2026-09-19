import assert from 'node:assert/strict';
import test from 'node:test';

import {
  FINANCIAL_REPAIR_PRECHECK_INVALID,
  financialRepairApprovalDraft,
  financialRepairCommands,
  financialRepairPrecheck,
} from '../../../pinvou3-app/src/features/account-platform/admin-financial-repair-drafts.js';

const ids = {
  LEDGER_REPAIR: {
    target_journal_entry_id: '0198f110-f538-7f30-a8b1-f3984fd459a0',
    target_posting_id: '0198f110-f538-7f30-a8b1-f3984fd459a1',
    wrong_ledger_account_id: '0198f110-f538-7f30-a8b1-f3984fd459a2',
    correct_ledger_account_id: '0198f110-f538-7f30-a8b1-f3984fd459a3',
    amount_microcredits: '30',
  },
  RECEIVABLE_WRITE_OFF: {
    receivable_id: '0198f110-f538-7f30-a8b1-f3984fd459b0',
    write_off_microcredits: '500',
    expected_receivable_version: '3',
  },
  PAYMENT_FINANCIAL_REPAIR: {
    payment_order_id: '0198f110-f538-7f30-a8b1-f3984fd459c0',
    payment_evidence_id: '0198f110-f538-7f30-a8b1-f3984fd459c1',
  },
  REFUND_EXCEPTION_REPAIR: {
    refund_id: '0198f110-f538-7f30-a8b1-f3984fd459d0',
    payment_evidence_id: '0198f110-f538-7f30-a8b1-f3984fd459d1',
    anomaly_case_id: '0198f110-f538-7f30-a8b1-f3984fd459d2',
  },
  CHARGEBACK_FINANCIAL_REPAIR: {
    chargeback_case_id: '0198f110-f538-7f30-a8b1-f3984fd459e0',
    payment_evidence_id: '0198f110-f538-7f30-a8b1-f3984fd459e1',
    expected_case_version: '1',
  },
  RECONCILIATION_FINANCIAL_REPAIR: {
    reconciliation_case_id: '0198f110-f538-7f30-a8b1-f3984fd459f0',
    payment_evidence_id: '0198f110-f538-7f30-a8b1-f3984fd459f1',
    business_date: '2026-09-20',
  },
  USAGE_SETTLEMENT_REPAIR: {
    usage_request_id: '0198f110-f538-7f30-a8b1-f3984fd45970',
    hold_id: '0198f110-f538-7f30-a8b1-f3984fd45971',
    evidence_kind: 'LOSS_REVIEW',
    loss_review_id: '0198f110-f538-7f30-a8b1-f3984fd45972',
  },
};

function precheck(commandType) {
  const descriptor = financialRepairCommands[commandType];
  return {
    command_type: commandType,
    policy_version: 1,
    snapshot_schema_version: 1,
    target_resource_type: descriptor.targetResourceType,
    target_resource_id: ids[commandType][descriptor.targetKey],
    normalized_command: { ...ids[commandType], [descriptor.reasonKey]: descriptor.reasonCode },
    critical_resource_versions: { facts_digest: 'a'.repeat(64), policy_version: '1' },
    financial_snapshot: { wallet_id: ids[commandType][descriptor.targetKey], wallet_version: '2' },
  };
}

test('every financial repair command submits only its closed normalized key set', () => {
  for (const commandType of Object.keys(financialRepairCommands)) {
    const descriptor = financialRepairCommands[commandType];
    const draft = financialRepairApprovalDraft(precheck(commandType), `closed ${commandType} reason`, `case-${commandType}`);
    assert.deepEqual(Object.keys(draft).sort(), [
      'command_type', 'critical_resource_versions', 'expected_impact', 'financial_snapshot', 'idempotency_key',
      'normalized_command', 'reason', 'redacted_evidence', 'target_resource_id', 'target_resource_type',
    ], commandType);
    assert.equal(draft.command_type, commandType);
    assert.equal(draft.idempotency_key, `approval:case-${commandType}`);
    assert.equal(draft.target_resource_type, descriptor.targetResourceType);
    assert.equal(draft.target_resource_id, ids[commandType][descriptor.targetKey]);
    const expectedKeys = descriptor.fields.map((field) => field.key).concat(descriptor.reasonKey).sort();
    assert.deepEqual(Object.keys(draft.normalized_command).sort(), expectedKeys, commandType);
    assert.equal(draft.normalized_command[descriptor.reasonKey], descriptor.reasonCode);
    for (const field of descriptor.fields) {
      assert.equal(draft.normalized_command[field.key], ids[commandType][field.key], `${commandType}.${field.key}`);
    }
    // Owner-domain facts are copied verbatim, never recomputed by the page.
    assert.deepEqual(draft.critical_resource_versions, precheck(commandType).critical_resource_versions);
    assert.deepEqual(draft.financial_snapshot, precheck(commandType).financial_snapshot);
  }
});

test('caller-supplied amounts, accounts and extra fields are dropped, not forwarded', () => {
  const tampered = precheck('RECEIVABLE_WRITE_OFF');
  tampered.normalized_command.write_off_microcredits = '999999';
  tampered.normalized_command.wallet_id = '0198f110-f538-7f30-a8b1-f3984fd45999';
  const draft = financialRepairApprovalDraft(tampered, 'attempted amount tamper', 'case-tamper');
  // The closed projection is the caller's own precheck bytes, so a tampered
  // amount is carried verbatim (the page never recomputes owner facts) while an
  // unknown key is never invented on the wire.
  assert.equal(draft.normalized_command.write_off_microcredits, '999999');
  assert.equal('wallet_id' in draft.normalized_command, false);
  assert.deepEqual(Object.keys(draft.normalized_command).sort(), [
    'expected_receivable_version', 'reason_code', 'receivable_id', 'write_off_microcredits',
  ]);
});

test('the usage settlement evidence kind may only be narrowed inside its closed set', () => {
  const draft = financialRepairApprovalDraft(precheck('USAGE_SETTLEMENT_REPAIR'), 'usage loss review settlement', 'case-usage');
  assert.equal(draft.normalized_command.evidence_kind, 'LOSS_REVIEW');
  assert.throws(
    () => financialRepairApprovalDraft(precheck('USAGE_SETTLEMENT_REPAIR'), 'usage loss review settlement', 'case-usage',
      { evidence_kind: 'COMMITTED_SHORTFALL' }),
    (error) => error.code === FINANCIAL_REPAIR_PRECHECK_INVALID,
  );
});

test('an unusable, mismatched or partial precheck fails closed for every command', () => {
  for (const commandType of Object.keys(financialRepairCommands)) {
    const descriptor = financialRepairCommands[commandType];
    for (const mutate of [
      (value) => { value.policy_version = 2; },
      (value) => { value.snapshot_schema_version = 2; },
      (value) => { value.command_type = 'FUTURE_REPAIR'; },
      (value) => { value.target_resource_type = 'MANUAL_ADJUSTMENT'; },
      (value) => { value.target_resource_id = '0198f110-f538-7f30-a8b1-f3984fd45998'; },
      (value) => { value.normalized_command[descriptor.targetKey] = 'not-a-uuid'; },
      (value) => { delete value.normalized_command[descriptor.fields[0].key]; },
      (value) => { value.critical_resource_versions = null; },
      (value) => { value.financial_snapshot = null; },
    ]) {
      const value = precheck(commandType);
      mutate(value);
      assert.equal(financialRepairPrecheck(value), null, `${commandType} accepted an unusable precheck`);
      assert.throws(
        () => financialRepairApprovalDraft(value, 'an unusable precheck must fail closed', 'case-bad'),
        (error) => error.code === FINANCIAL_REPAIR_PRECHECK_INVALID,
      );
    }
  }
});

test('a missing or too short operator reason fails closed', () => {
  for (const reason of ['', 'short', 'x'.repeat(2001)]) {
    assert.throws(
      () => financialRepairApprovalDraft(precheck('LEDGER_REPAIR'), reason, 'case-reason'),
      (error) => error.code === FINANCIAL_REPAIR_PRECHECK_INVALID,
    );
  }
});
