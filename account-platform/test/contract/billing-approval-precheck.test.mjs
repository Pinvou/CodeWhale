import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');

// The read-only precheck is the only surface the business page may use to
// assemble an approval draft before any governed command exists. It must stay
// Account-only, bounded, read-only and impossible to widen: it reuses exactly
// the closed owner inputs the proof authority uses, so a precheck can never be
// more permissive than the proof that follows it.
test('the Billing approval precheck stays Account-only, bounded and read-only', async () => {
  const source = await readFile(path.join(root, 'internal/billing/approval_precheck.go'), 'utf8');
  assert.match(source, /workload\.Service != "account-service"/, 'only the Account service identity may call the precheck');
  assert.match(source, /context\.WithTimeout\(r\.Context\(\), 2\*time\.Second\)/, 'the precheck keeps the two second absolute deadline');
  assert.match(source, /io\.LimitReader\(r\.Body, approvalencoding\.MaximumDocumentBytes\+1\)/, 'the precheck keeps the closed document bound');
  assert.match(source, /decoder\.DisallowUnknownFields\(\)/, 'unknown request fields must fail closed');
  assert.match(source, /Cache-Control", "no-store"/, 'the precheck answer is never cacheable');
  assert.doesNotMatch(source, /insertJournal|insertPosting|createReceivableWithinTransaction|AppendOutbox|ClaimBillingApprovalProof/,
    'the precheck must not write any fact or issue any claim');
  for (const input of [
    'LedgerRepairProofInput', 'ReceivableWriteOffProofInput', 'PaymentFinancialRepairProofInput',
    'RefundExceptionRepairProofInput', 'ChargebackFinancialRepairProofInput',
    'ReconciliationFinancialRepairProofInput', 'UsageSettlementRepairProofInput',
  ]) {
    assert.ok(source.includes(`repository.${input}(`), `${input} must be the only assembly path for that command`);
  }
  // The usage settlement precheck can only describe the implemented first
  // release; the unsupported evidence kind stays out of the surface entirely.
  assert.match(source, /EvidenceKind: usageSettlementRepairEvidenceKind/,
    'the usage settlement precheck is pinned to the implemented evidence kind');
  assert.doesNotMatch(source, /COMMITTED_SHORTFALL/, 'the unimplemented evidence kind must not appear in the precheck');
});

test('the precheck route reuses the proof-read workload permission and is contracted', async () => {
  const service = await readFile(path.join(root, 'internal/billing/service.go'), 'utf8');
  assert.match(service,
    /protected\.POST\("\/internal\/v1\/approval-command-prechecks", serviceauth\.Require\("internal\.billing\.approval-proof\.read"\), httpserver\.AdaptFunc\(s\.internalBillingApprovalPrecheck\)\)/,
    'the precheck must be registered with the existing read-only workload permission');
  const contract = YAML.parse(await readFile(path.join(root, 'contracts/openapi/billing.yaml'), 'utf8'));
  const operation = contract.paths['/internal/v1/approval-command-prechecks']?.post;
  assert.ok(operation, 'the precheck path must be contracted');
  assert.equal(operation.operationId, 'readBillingApprovalPrecheckInternal');
  for (const status of ['200', '400', '401', '403', '422', '503', '504']) {
    assert.ok(operation.responses[status], `the precheck contract must declare ${status}`);
  }
  assert.deepEqual(operation.security, [{ serviceToken: [] }]);
  const request = contract.components.schemas.BillingApprovalCommandPrecheckRequest;
  const response = contract.components.schemas.BillingApprovalCommandPrecheckResponse;
  assert.equal(request.additionalProperties, false);
  assert.equal(response.additionalProperties, false);
  assert.deepEqual(request.required, ['command_type', 'target_resource_id']);
  assert.deepEqual(request.properties.command_type.enum, [
    'LEDGER_REPAIR', 'RECEIVABLE_WRITE_OFF', 'PAYMENT_FINANCIAL_REPAIR', 'REFUND_EXCEPTION_REPAIR',
    'CHARGEBACK_FINANCIAL_REPAIR', 'RECONCILIATION_FINANCIAL_REPAIR', 'USAGE_SETTLEMENT_REPAIR',
  ]);
  assert.deepEqual(response.required, [
    'command_type', 'policy_version', 'snapshot_schema_version', 'target_resource_type', 'target_resource_id',
    'normalized_command', 'critical_resource_versions', 'financial_snapshot',
  ]);
});

test('the business entry consumes only the closed precheck projection', async () => {
  const module = await readFile(path.join(root, '../pinvou3-app/src/features/account-platform/admin-financial-repair-drafts.js'), 'utf8');
  // The page renders the server projection; it never derives domain facts and
  // never invents keys on the wire.
  assert.match(module, /export function financialRepairPrecheck\(value\)/, 'the precheck validator must be exported');
  assert.match(module, /export function financialRepairApprovalDraft\(precheck, operatorReason, requestKey, options = \{\}\)/,
    'the draft builder must take the precheck projection, the operator reason and the request key only');
  assert.match(module, /normalized\[projection\.command\.reasonKey\] = projection\.command\.reasonCode/,
    'the fixed reason code is owner-defined, never operator-supplied');
  assert.match(module, /critical_resource_versions: projection\.critical/, 'critical facts must be copied verbatim');
  assert.match(module, /financial_snapshot: projection\.financial/, 'financial facts must be copied verbatim');
});
