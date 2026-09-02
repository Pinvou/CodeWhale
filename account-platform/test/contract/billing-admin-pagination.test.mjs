import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');
const parameterName = parameter => parameter.name || parameter.$ref?.split('/').at(-1);

const collectionPaths = [
  '/billing/admin/v1/wallets',
  '/billing/admin/v1/wallets/{wallet_id}/ledger',
  '/billing/admin/v1/receivables',
  '/billing/admin/v1/payment-orders',
  '/billing/admin/v1/refunds',
  '/billing/admin/v1/adjustments',
  '/billing/admin/v1/usage-holds',
  '/billing/admin/v1/late-payment-reviews',
  '/billing/admin/v1/payment-anomalies',
  '/billing/admin/v1/reconciliation-runs',
  '/billing/admin/v1/reconciliation-runs/{run_id}/cases',
  '/billing/admin/v1/reconciliation-cases',
];

test('Billing admin collection contracts publish stable cursor pages and bounded failures', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  for (const route of collectionPaths) {
    const operation = contract.paths[route]?.get;
    assert.ok(operation, `${route} must publish GET`);
    const parameters = operation.parameters.map(parameterName);
    assert.ok(parameters.includes('Limit'), `${route} must publish limit`);
    assert.ok(parameters.includes('Cursor'), `${route} must publish cursor`);
    for (const status of ['200', '400', '403', '410', '422', '429', '503']) assert.ok(operation.responses[status], `${route} must publish ${status}`);
    assert.equal(operation.responses['429'].$ref, '#/components/responses/PaginationSnapshotLimited');
    assert.equal(operation.responses['503'].$ref, '#/components/responses/PaginationSnapshotUnavailable');
  }
  assert.deepEqual(contract.components.responses.SnapshotCollection.content['application/json'].schema.required, ['items', 'next_cursor', 'snapshot_as_of']);
  assert.equal(contract.components.responses.SnapshotCollection.content['application/json'].schema.properties.items.maxItems, 200);
  const receivableSchema = contract.paths['/billing/admin/v1/receivables'].get.responses['200'].content['application/json'].schema;
  assert.deepEqual(receivableSchema.required, ['items', 'next_cursor', 'snapshot_as_of']);
  for (const response of ['PaginationSnapshotLimited', 'PaginationSnapshotUnavailable']) {
    assert.equal(contract.components.responses[response].headers['Retry-After'].required, true);
    assert.equal(contract.components.responses[response].headers['Retry-After'].schema.minimum, 1);
  }
});

test('Billing admin pagination freezes only ordered resource identifiers with bounded storage', async () => {
  const [migration, implementation, items] = await Promise.all([
    read('migrations/billing/000050_admin_pagination_snapshots.up.sql'),
    read('internal/billing/admin_pagination.go'),
    read('internal/billing/admin_pagination_items.go'),
  ]);
  for (const table of ['admin_pagination_snapshots', 'admin_pagination_snapshot_members', 'admin_pagination_snapshot_tombstones', 'admin_pagination_creation_windows']) assert.match(migration, new RegExp(`CREATE TABLE ${table}`));
  assert.doesNotMatch(migration, /CREATE TABLE admin_pagination_cursors/);
  assert.match(migration, /cursor_secret BYTEA NOT NULL CHECK \(octet_length\(cursor_secret\) = 32\)/);
  assert.match(migration, /UNIQUE \(snapshot_id, resource_id\)/);
  assert.match(migration, /position >= 0 AND position <= 1000000/);
  assert.match(implementation, /adminSnapshotTTL\s+= 15 \* time\.Minute/);
  assert.match(implementation, /adminSnapshotMaxMembers\s+= 1_000_000/);
  assert.match(implementation, /adminSnapshotMaxGlobal\s+= 64/);
  assert.match(implementation, /adminSnapshotMaxGlobalMember\s+= 20_000_000/);
  assert.match(implementation, /SET LOCAL statement_timeout='10s'/);
  assert.match(implementation, /LIMIT 1000001/);
  assert.match(implementation, /row_number\(\) OVER \(ORDER BY sort_at DESC,sort_id DESC,resource_id DESC\)-1/);
  assert.match(implementation, /memberCount > adminSnapshotMaxMembers/);
  assert.match(implementation, /ErrPaginationFilterRequired/);
  assert.match(implementation, /scopeDigest.*filterDigest/s);
  assert.match(items, /ORDER BY array_position\(\$1::uuid\[\],id\)/);
});

test('Billing admin pagination rejects cursor misuse and exposes exact recovery semantics', async () => {
  const implementation = await read('internal/billing/admin_pagination.go');
  assert.match(implementation, /hmac\.New\(sha256\.New, secret\)/);
  assert.match(implementation, /signedAdminCursor\(snapshotID, position\+limit, cursorSecret\)/);
  assert.match(implementation, /hmac\.Equal\(cursor\.Signature, signer\.Sum\(nil\)\)/);
  assert.match(implementation, /subjectID != request\.SubjectID \|\| resource != request\.Resource \|\| storedScope != scopeDigest \|\| storedFilter != filterDigest/);
  assert.match(implementation, /PAGINATION_CURSOR_INVALID/);
  assert.match(implementation, /PAGINATION_CURSOR_EXPIRED/);
  assert.match(implementation, /PAGINATION_FILTER_REQUIRED/);
  assert.match(implementation, /PAGINATION_SNAPSHOT_LIMITED/);
  assert.match(implementation, /PAGINATION_SNAPSHOT_UNAVAILABLE/);
  assert.match(implementation, /Header\(\)\.Set\("Retry-After", "10"\)/);
  assert.match(implementation, /Header\(\)\.Set\("Retry-After", "5"\)/);
  assert.match(implementation, /admin_pagination_snapshot_tombstones/);
  assert.doesNotMatch(implementation, /INSERT INTO admin_pagination_cursors/i);
  assert.match(implementation, /pgx\.RepeatableRead, AccessMode: pgx\.ReadOnly/);
});

test('snapshot cleanup, capacity metrics, alerts, and million-order evidence are release-gated', async () => {
  const [main, implementation, integration, metrics, rules, policy, gate, runbook, packageJSON, workflow] = await Promise.all([
    read('cmd/billing-service/main.go'),
    read('internal/billing/admin_pagination.go'),
    read('internal/billing/admin_pagination_integration_test.go'),
    read('internal/billing/admin_pagination_metrics.go'),
    read('deploy/helm/account-platform/templates/prometheus-rules.yaml'),
    read('deploy/performance-policy.json').then(JSON.parse),
    read('../scripts/account-platform-performance-gate.mjs'),
    read('deploy/runbooks/performance-capacity.md'),
    read('package.json').then(JSON.parse),
    read('../.github/workflows/pr-check.yml'),
  ]);
  assert.match(main, /NewAdminPaginationCleanupWorker\(repository, time\.Minute\)/);
  assert.match(implementation, /DELETE FROM admin_pagination_snapshots WHERE expires_at<=\$1/);
  for (const metric of ['pinvou_admin_pagination_active_snapshots', 'pinvou_admin_pagination_snapshot_members', 'pinvou_admin_pagination_storage_bytes', 'pinvou_admin_pagination_cleanup_oldest_age_seconds', 'pinvou_admin_pagination_snapshot_creations_total']) assert.match(metrics, new RegExp(metric));
  assert.match(rules, /14000000/);
  assert.match(rules, /17000000/);
  assert.match(rules, /sum\(max by \(resource\) \(pinvou_admin_pagination_snapshot_members\)\)/);
  assert.match(rules, /6012954214/);
  assert.match(rules, /7301444403/);
  assert.match(rules, /> 300/);
  assert.equal(policy.admin_pagination.payment_orders, 1_000_000);
  assert.equal(policy.admin_pagination.minimum_pages, 5_000);
  assert.equal(policy.admin_pagination.members_seen, 1_000_000);
  assert.equal(policy.admin_pagination.first_snapshot_p99_ms, 10_000);
  assert.match(gate, /admin_pagination\.first_snapshot_p99_ms/);
  assert.match(gate, /admin_pagination\.page_trace_sha256/);
  assert.match(gate, /evidence\.release_sha256 === provenance\.expectedReleaseSHA256/);
  assert.match(gate, /readFile\(path\.resolve\(arguments_\.pageTracePath\)\)/);
  assert.match(gate, /duplicate or omitted members/);
  assert.match(runbook, /1,000,000 条支付订单/);
  assert.match(packageJSON.scripts['check:production-performance'], /--require-evidence/);
  assert.match(integration, /PINVOU_TEST_BILLING_DATABASE_URL/);
  assert.match(integration, /concurrent mutations/);
  assert.match(integration, /LOCK TABLE wallets IN ACCESS EXCLUSIVE MODE/);
  assert.match(integration, /admin_pagination_snapshot_tombstones/);
  assert.match(integration, /adminSnapshotMaxGlobalMember\s*\/\s*adminSnapshotMaxMembers/);
  assert.match(integration, /adminSnapshotGlobalPerMin/);
  assert.match(integration, /paginationMaxStorageBytes = 1/);
  assert.match(workflow, /PINVOU_TEST_BILLING_DATABASE_URL:/);
  assert.match(workflow, /--- PASS: TestAdminPaginationPostgresIntegration/);
});
