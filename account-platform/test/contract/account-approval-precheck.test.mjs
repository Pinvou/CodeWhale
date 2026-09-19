import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');

// The business page must never assemble an approval draft from facts it derived
// itself. The Account proxy is the only surface that hands the page the closed
// owner projection, so it has to stay permission-gated, read-only, bounded and
// impossible to widen: it forwards exactly the closed input, revalidates the
// answer against the command's own target contract and stores nothing.
test('the Account precheck proxy stays permission-gated, read-only and bounded', async () => {
  const source = await readFile(path.join(root, 'internal/account/approval_precheck.go'), 'utf8');
  const service = await readFile(path.join(root, 'internal/account/service.go'), 'utf8');
  const authorization = await readFile(path.join(root, 'internal/account/authorization.go'), 'utf8');
  const main = await readFile(path.join(root, 'cmd/account-service/main.go'), 'utf8');

  assert.match(authorization, /PermissionPlatformBillingRepairsRequest\s*=\s*"platform\.billing\.repairs\.request"/,
    'the proxy reuses the existing financial repair request permission');
  assert.match(service,
    /adminV2\.POST\("\/approval-command-prechecks", s\.requirePermission\(PermissionPlatformBillingRepairsRequest\), httpserver\.AdaptFunc\(s\.adminApprovalCommandPrecheck\)\)/,
    'the precheck route must be registered behind the repair request permission');
  assert.match(main, /account\.NewHTTPApprovalPrecheckSource\(os\.Getenv\("BILLING_HTTP_URL"\), serviceCredential\)/,
    'the production wiring must use the Account service identity against Billing');
  assert.match(main, /WithApprovalPrecheckSource\(approvalPrechecks\)/, 'the source must be wired into the service');

  assert.match(source, /context\.WithDeadline\(ctx, deadline\)/, 'the proxy keeps an absolute deadline');
  assert.match(source, /approvalencoding\.MaximumDocumentBytes \+ len\(payload\)|maximumResponse := approvalencoding\.MaximumDocumentBytes - len\(payload\)/,
    'the request and answer share the closed document bound');
  assert.match(source, /http\.ErrUseLastResponse/, 'the proxy must never follow a redirect to another authority');
  assert.match(source, /decoder\.DisallowUnknownFields\(\)/, 'unknown fields must fail closed on both sides');
  assert.match(source, /Cache-Control", "no-store"/, 'the proxy answer is never cacheable');
  assert.match(source, /\/internal\/v1\/approval-command-prechecks/, 'the proxy posts to exactly the owner precheck path');
  assert.doesNotMatch(source, /insertJournal|insertPosting|createReceivableWithinTransaction|AppendOutbox|ClaimBillingApprovalProof|\.Query(?:Row)?\(/,
    'the proxy must not read or write any owner fact itself');
  assert.doesNotMatch(source, /redirectClient|http\.Client\{[^}]*CheckRedirect: nil/, 'redirects stay refused');
});

test('the proxy revalidates the owner answer against the closed command contract', async () => {
  const source = await readFile(path.join(root, 'internal/account/approval_precheck.go'), 'utf8');
  const portal = await readFile(path.join(root, '../pinvou3-app/src/features/account-platform/admin-financial-repair-drafts.js'), 'utf8');
  const goTargets = new Map();
  const goTable = source.slice(source.indexOf('approvalPrecheckTargetResourceTypes = map[string]string{'),
    source.indexOf('// ApprovalPrecheckRequest'));
  for (const match of goTable.matchAll(/"([A-Z_]+)":\s*"([A-Z_]+)"/g)) goTargets.set(match[1], match[2]);
  assert.equal(goTargets.size, 7, 'all seven financial repair commands must be closed in the proxy table');
  for (const [command, target] of goTargets) {
    const descriptor = portal.slice(portal.indexOf(`  ${command}: {`));
    const jsTarget = descriptor.match(/targetResourceType: '([A-Z_]+)'/)?.[1];
    assert.equal(jsTarget, target, `${command} must freeze the same target resource type in the page and the proxy`);
  }
  assert.match(source, /wire\.TargetResourceID != request\.TargetResourceID/,
    'the answer must bind to the requested target');
  assert.match(source, /wire\.TargetResourceType != expectedTargetType/,
    'the answer must carry exactly the command target type');
  assert.match(source, /wire\.PolicyVersion < 1 \|\| wire\.SnapshotSchemaVersion < 1/,
    'a degenerate policy or schema version must fail closed');
  assert.match(source, /closedApprovalPrecheckDocument\(wire\.NormalizedCommand\)/,
    'the normalized command must be a closed document');
  assert.match(source, /closedApprovalPrecheckDocument\(wire\.FinancialSnapshot\)/,
    'the financial snapshot must be a closed document');
});

test('the Account precheck path is contracted with a closed request and answer', async () => {
  const contract = YAML.parse(await readFile(path.join(root, 'contracts/openapi/account.yaml'), 'utf8'));
  const operation = contract.paths['/admin/v2/approval-command-prechecks']?.post;
  assert.ok(operation, 'the proxy path must be contracted');
  assert.equal(operation.operationId, 'readApprovalCommandPrecheckV2');
  for (const status of ['200', '400', '401', '403', '422', '503', '504']) {
    assert.ok(operation.responses[status], `the proxy contract must declare ${status}`);
  }
  const request = contract.components.schemas.ApprovalCommandPrecheckRequest;
  const response = contract.components.schemas.ApprovalCommandPrecheckResponse;
  assert.equal(request.additionalProperties, false);
  assert.equal(response.additionalProperties, false);
  assert.deepEqual(request.required, ['command_type', 'target_resource_id']);
  assert.deepEqual(request.properties.command_type.enum, [
    'LEDGER_REPAIR', 'RECEIVABLE_WRITE_OFF', 'PAYMENT_FINANCIAL_REPAIR', 'REFUND_EXCEPTION_REPAIR',
    'CHARGEBACK_FINANCIAL_REPAIR', 'RECONCILIATION_FINANCIAL_REPAIR', 'USAGE_SETTLEMENT_REPAIR',
  ]);
  assert.equal(request.properties.usage_request_id.format, 'uuid');
  assert.deepEqual(response.required, [
    'command_type', 'policy_version', 'snapshot_schema_version', 'target_resource_type', 'target_resource_id',
    'normalized_command', 'critical_resource_versions', 'financial_snapshot',
  ]);
  // The billing owner contract that the proxy forwards to stays closed too.
  const billing = YAML.parse(await readFile(path.join(root, 'contracts/openapi/billing.yaml'), 'utf8'));
  const owner = billing.components.schemas.BillingApprovalCommandPrecheckRequest;
  assert.equal(owner.additionalProperties, false);
  assert.equal(owner.properties.usage_request_id.format, 'uuid');
});

test('the precheck never widens the approval wire', async () => {
  const source = await readFile(path.join(root, 'internal/account/approval_precheck.go'), 'utf8');
  // The projection is returned verbatim; the proxy adds no approval identity,
  // no epoch, no authorization event and no critical version of its own.
  for (const forbidden of ['approval_request_id', 'authorization_event_id', 'approval_epoch', 'execution_key']) {
    assert.ok(!source.includes(`"${forbidden}"`), `the precheck answer must not carry ${forbidden}`);
  }
});
