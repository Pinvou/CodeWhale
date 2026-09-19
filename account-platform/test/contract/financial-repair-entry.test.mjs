import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import {
  financialRepairCommandTypes, financialRepairInputs, financialRepairPrecheckBody,
  financialRepairProjectionRows, financialRepairDraftFromProjection,
} from '../../../pinvou3-app/src/features/account-platform/admin-financial-repair-form.js';

const root = path.resolve(import.meta.dirname, '../..');
const api = '0198fbf7-c3ba-7bc4-8849-c157740963de';
const other = '0198fbf7-c3ba-7bc4-8849-c157740963df';

// The business page may only send the closed precheck input. These expectations
// are the frozen key sets of the Account precheck request schema.
const expectedInputs = {
  LEDGER_REPAIR: ['target_resource_id', 'target_journal_entry_id', 'wrong_ledger_account_id', 'correct_ledger_account_id', 'amount_microcredits'],
  RECEIVABLE_WRITE_OFF: ['target_resource_id', 'write_off_microcredits', 'expected_version'],
  PAYMENT_FINANCIAL_REPAIR: ['target_resource_id', 'payment_evidence_id'],
  REFUND_EXCEPTION_REPAIR: ['target_resource_id', 'payment_evidence_id', 'anomaly_case_id'],
  CHARGEBACK_FINANCIAL_REPAIR: ['target_resource_id', 'payment_evidence_id'],
  RECONCILIATION_FINANCIAL_REPAIR: ['target_resource_id', 'payment_evidence_id', 'business_date'],
  USAGE_SETTLEMENT_REPAIR: ['target_resource_id', 'usage_request_id', 'loss_review_id'],
};

test('the repair entry freezes one closed precheck input per command', () => {
  assert.deepEqual([...financialRepairCommandTypes].sort(), Object.keys(expectedInputs).sort());
  for (const [command, keys] of Object.entries(expectedInputs)) {
    assert.deepEqual(financialRepairInputs[command].map((field) => field.key), keys, `${command} input set`);
  }
});

test('the repair entry forwards only validated closed values', () => {
  const valid = financialRepairPrecheckBody('PAYMENT_FINANCIAL_REPAIR', { target_resource_id: api, payment_evidence_id: other });
  assert.deepEqual(valid, { body: { command_type: 'PAYMENT_FINANCIAL_REPAIR', target_resource_id: api, payment_evidence_id: other } });
  const integers = financialRepairPrecheckBody('RECEIVABLE_WRITE_OFF', { target_resource_id: api, write_off_microcredits: '500', expected_version: '3' });
  assert.deepEqual(integers.body, { command_type: 'RECEIVABLE_WRITE_OFF', target_resource_id: api, write_off_microcredits: 500, expected_version: 3 });
  const date = financialRepairPrecheckBody('RECONCILIATION_FINANCIAL_REPAIR', { target_resource_id: api, payment_evidence_id: other, business_date: '2026-09-20' });
  assert.equal(date.body.business_date, '2026-09-20');
  for (const [name, command, values] of [
    ['padded target', 'PAYMENT_FINANCIAL_REPAIR', { target_resource_id: ` ${api}`, payment_evidence_id: other }],
    ['missing evidence', 'PAYMENT_FINANCIAL_REPAIR', { target_resource_id: api }],
    ['zero amount', 'RECEIVABLE_WRITE_OFF', { target_resource_id: api, write_off_microcredits: '0', expected_version: '1' }],
    ['bad date', 'RECONCILIATION_FINANCIAL_REPAIR', { target_resource_id: api, payment_evidence_id: other, business_date: '2026-13-01' }],
    ['unknown command', 'FUTURE_REPAIR', { target_resource_id: api }],
  ]) {
    assert.equal(financialRepairPrecheckBody(command, values).error, 'FINANCIAL_REPAIR_PRECHECK_INVALID', name);
  }
});

function receivableProjection(overrides = {}) {
  return {
    command_type: 'RECEIVABLE_WRITE_OFF',
    policy_version: 1,
    snapshot_schema_version: 1,
    target_resource_type: 'RECEIVABLE',
    target_resource_id: api,
    // The owner encodes int64 facts as JSON numbers.
    normalized_command: { receivable_id: api, write_off_microcredits: 500, expected_receivable_version: 3, reason_code: 'APPROVED_RECEIVABLE_WRITE_OFF' },
    critical_resource_versions: { receivable_version: 3 },
    financial_snapshot: { outstanding_microcredits: 500 },
    ...overrides,
  };
}

test('the repair entry renders and submits only the owner projection', () => {
  const rendered = financialRepairProjectionRows(receivableProjection());
  assert.ok(rendered, 'a closed projection must render');
  assert.deepEqual(rendered.rows.map((row) => row.key), ['target', 'receivable_id', 'write_off_microcredits', 'expected_receivable_version', 'reason_code']);
  assert.equal(JSON.parse(rendered.critical).receivable_version, 3);
  assert.equal(JSON.parse(rendered.financial).outstanding_microcredits, 500);
  assert.equal(financialRepairProjectionRows(receivableProjection({ target_resource_id: other })), null, 'a mismatched target must not render');
  assert.equal(financialRepairProjectionRows(receivableProjection({ policy_version: 2 })), null, 'an unknown policy version must not render');

  const draft = financialRepairDraftFromProjection(receivableProjection(), 'approved write-off after review', 'repair-1');
  assert.equal(draft.error, undefined);
  assert.equal(draft.draft.command_type, 'RECEIVABLE_WRITE_OFF');
  assert.equal(draft.draft.idempotency_key, 'approval:repair-1');
  assert.deepEqual(draft.draft.normalized_command, { receivable_id: api, write_off_microcredits: 500, expected_receivable_version: 3, reason_code: 'APPROVED_RECEIVABLE_WRITE_OFF' });
  assert.deepEqual(draft.draft.critical_resource_versions, { receivable_version: 3 });
  assert.deepEqual(draft.draft.financial_snapshot, { outstanding_microcredits: 500 });
  assert.deepEqual(draft.draft.redacted_evidence, []);
  assert.equal(financialRepairDraftFromProjection(receivableProjection(), 'too short', 'repair-1').error, 'FINANCIAL_REPAIR_PRECHECK_INVALID');
  assert.equal(financialRepairDraftFromProjection(null, 'approved write-off after review', 'repair-1').error, 'FINANCIAL_REPAIR_PRECHECK_INVALID');
});

test('the business page reaches the owner only through the precheck BFF', async () => {
  const client = await readFile(path.join(root, '../pinvou3-app/src/features/account-platform/admin-client.js'), 'utf8');
  const workspace = await readFile(path.join(root, '../pinvou3-app/src/features/account-platform/AdminApprovalWorkspace.jsx'), 'utf8');
  const form = await readFile(path.join(root, '../pinvou3-app/src/features/account-platform/AdminFinancialRepairDraftForm.jsx'), 'utf8');
  const bff = await readFile(path.join(root, 'internal/portal/approval_prechecks.go'), 'utf8');
  const service = await readFile(path.join(root, 'internal/portal/service.go'), 'utf8');

  assert.match(client, /fetch\(`\$\{this\.baseURL\}\/api\/admin\/approvals\/prechecks`/, 'the client must use the dedicated precheck BFF path');
  assert.match(client, /'X-CSRF-Token': this\.csrfToken/, 'the precheck must carry the CSRF token');
  assert.match(workspace, /const views = \['mine', 'pending', 'execution', 'directory', 'repair'\]/, 'the repair page must be a first-class approval-center view');
  assert.match(workspace, /<AdminFinancialRepairDraftForm client=\{client\} copy=\{c\.financialRepair\}/, 'the view must render the repair form');
  assert.match(form, /client\.approvalCommandPrecheck\(prepared\.body\)/, 'the form must read facts through the precheck');
  assert.match(form, /client\.createGovernedApprovalDraft\(prepared\.draft\)/, 'the form must submit the closed draft through the governed writer');
  assert.doesNotMatch(form, /financial_snapshot:\s*\{/, 'the page must never build a financial snapshot itself');
  assert.match(service, /router\.POST\("\/api\/admin\/approvals\/prechecks", httpserver\.AdaptFunc\(s\.adminApprovalPrechecks\)\)/, 'the BFF route must be registered');
  assert.match(bff, /\/admin\/v2\/approval-command-prechecks/, 'the BFF must forward to the Account precheck proxy');
  assert.match(bff, /"Bearer "\+session\.AccessToken/, 'the BFF must forward the user identity, not a service identity');
  assert.match(bff, /client\.CheckRedirect = func\(\*http\.Request, \[\]\*http\.Request\) error \{ return http\.ErrUseLastResponse \}/, 'the BFF must never follow a redirect');
  const catalog = JSON.parse(await readFile(path.join(root, 'deploy/authorization-entrypoints.json'), 'utf8'));
  const entry = catalog.http_policies.find((item) => item.path === '/api/admin/approvals/prechecks');
  assert.deepEqual(entry, {
    source: 'account-platform/internal/portal/service.go', receiver: 'router', method: 'POST',
    path: '/api/admin/approvals/prechecks', scope: 'authenticated',
    strategy: 'authenticated.portal-approval-precheck-csrf-account-authorized',
  }, 'the BFF route must stay catalogued as an authenticated CSRF-protected proxy');
});
