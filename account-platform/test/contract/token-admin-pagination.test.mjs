import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('Token migration 000023 owns the complete shared snapshot lifecycle', async () => {
  const up = await read('migrations/token/000023_admin_pagination_snapshots.up.sql');
  const down = await read('migrations/token/000023_admin_pagination_snapshots.down.sql');
  for (const table of [
    'token_admin_pagination_control',
    'token_admin_pagination_snapshots',
    'token_admin_pagination_snapshot_members',
    'token_admin_pagination_snapshot_tombstones',
    'token_admin_pagination_creation_windows',
  ]) {
    assert.match(up, new RegExp(`CREATE TABLE IF NOT EXISTS ${table}`));
    assert.match(down, new RegExp(`DROP TABLE IF EXISTS ${table}`));
  }
  for (const column of ['authorization_version', 'resource_type', 'scope_digest', 'filter_digest', 'sort_spec', 'page_limit', 'high_water', 'expires_at', 'member_count', 'member_bytes']) {
    assert.match(up, new RegExp(`\\b${column}\\b`));
  }
  assert.match(up, /member_count BETWEEN 0 AND 1000000/);
  assert.match(up, /accepting_new_snapshots BOOLEAN NOT NULL DEFAULT FALSE/);
  assert.match(down, /TOKEN_ADMIN_PAGINATION_GATE_ENABLED/);
  assert.match(down, /TOKEN_ADMIN_PAGINATION_CURSOR_ACTIVE/);
  assert.match(down, /expires_at>transaction_timestamp\(\)/);
  const manifest = JSON.parse(await read('migrations/manifest.json'));
  assert.ok(manifest.domains.token.some(item => item.file === '000023_admin_pagination_snapshots.up.sql' && item.version === 23 && /^[a-f0-9]{64}$/.test(item.sha256)));
  assert.ok(manifest.domains.token.some(item => item.file === '000023_admin_pagination_snapshots.down.sql' && item.version === 23 && /^[a-f0-9]{64}$/.test(item.sha256)));
});

test('Token pagination base freezes cursor bindings, capacity and stable errors without business adapters', async () => {
  const source = await read('internal/token/admin_pagination.go');
  for (const binding of ['SubjectID', 'AuthorizationVersion', 'Resource', 'ScopeDigest', 'FilterDigest', 'Sort', 'Limit', 'HighWaterUnixNano', 'ExpiresUnixNano']) {
    assert.match(source, new RegExp(`\\b${binding}\\b`));
  }
  for (const boundary of ['1_000_000', '20_000_000', '8 * 1024 * 1024 * 1024', 'AdminSnapshotMaxGlobal        = 64', 'AdminSnapshotSubjectPerMinute = 6', 'AdminSnapshotGlobalPerMinute  = 60']) {
    assert.ok(source.includes(boundary), `missing boundary ${boundary}`);
  }
  for (const code of ['INVALID_LIMIT', 'INVALID_CURSOR', 'CURSOR_EXPIRED', 'PAGINATION_FILTER_REQUIRED', 'PAGINATION_SNAPSHOT_LIMITED', 'PAGINATION_SNAPSHOT_UNAVAILABLE', 'QUERY_TIMEOUT']) {
    assert.ok(source.includes(code), `missing stable code ${code}`);
  }
  assert.match(source, /transaction_timestamp\(\)/);
  assert.match(source, /AdminSnapshotMemberSource/);
  assert.doesNotMatch(source, /FROM\s+(model_catalog|token_accounts|provisioning|usage_requests)/i);
});

test('Portal and Token preserve the three absolute deadline chains', async () => {
  const deadline = await read('internal/platform/requestdeadline/token_admin.go');
  const portal = await read('internal/portal/service.go');
  const gateway = await read('cmd/token-gateway/main.go');
  for (const chain of [
    ['10 * time.Second', '12 * time.Second', '14 * time.Second'],
    ['3 * time.Second', '5 * time.Second', '7 * time.Second'],
    ['5 * time.Second', '7 * time.Second', '9 * time.Second'],
  ]) for (const value of chain) assert.ok(deadline.includes(value), `missing deadline ${value}`);
  assert.match(portal, /Header\.Del\(requestdeadline\.AbsoluteDeadlineHeader\)/);
  assert.match(portal, /context\.WithDeadline\(requestContext, contract\.BFFDeadline\)/);
  assert.match(portal, /requestdeadline\.ApplyToUpstream\(request, contract\)/);
  assert.match(gateway, /requestdeadline\.TokenAdminMiddleware/);
});

test('deployment templates keep the gate closed and reserve the 14-second transport ceiling', async () => {
  const compose = YAML.parse(await read('deploy/compose/compose.yaml'));
  assert.equal(String(compose.services['token-gateway'].environment.TOKEN_ADMIN_PAGINATION_ENABLED), '${TOKEN_ADMIN_PAGINATION_ENABLED:-false}');
  assert.equal(String(compose.services['token-portal'].environment.TOKEN_ADMIN_INGRESS_MAX_TIMEOUT_SECONDS), '14');
  const values = YAML.parse(await read('deploy/helm/account-platform/values.yaml'));
  assert.equal(values.tokenGateway.adminPaginationEnabled, false);
  assert.equal(values.ingress.tokenAdminMaxTimeoutSeconds, 14);
  const ingress = await read('deploy/helm/account-platform/templates/ingress.yaml');
  const nginx = await read('deploy/nginx/account-platform.conf.template');
  assert.match(ingress, /proxy-read-timeout: \{\{ \.Values\.ingress\.tokenAdminMaxTimeoutSeconds \| quote \}\}/);
  const [sharedIngress, tokenAdminIngress] = ingress.split(/^---$/m);
  assert.doesNotMatch(sharedIngress, /proxy-(read|send)-timeout/);
  assert.match(tokenAdminIngress, /name: \{\{ include "pinvou\.fullname" \. \}\}-token-admin/);
  assert.match(tokenAdminIngress, /path: \{\{ \.Values\.ingress\.tokenAdminPath \}\}/);
  assert.match(nginx, /location \^~ \/api\/admin\/token\/[\s\S]*proxy_read_timeout 14s/);
  assert.match(nginx, /proxy_set_header X-Pinvou-Absolute-Deadline ""/);
});

test('Token OpenAPI publishes one reusable pagination envelope and stable code vocabulary', async () => {
  const document = YAML.parse(await read('contracts/openapi/token-gateway.yaml'));
  const envelope = document.components.schemas.AdminPaginationEnvelope;
  assert.deepEqual(envelope.required, ['items', 'next_cursor', 'has_more', 'limit', 'as_of', 'snapshot_expires_at', 'request_id']);
  assert.equal(envelope.properties.limit.maximum, 200);
  const codes = document.components.schemas.AdminPaginationErrorCode.enum;
  for (const code of ['INVALID_LIMIT', 'INVALID_CURSOR', 'CURSOR_EXPIRED', 'PAGINATION_FILTER_REQUIRED', 'PAGINATION_SNAPSHOT_LIMITED', 'RATE_LIMITED', 'PAGINATION_SNAPSHOT_UNAVAILABLE', 'QUERY_TIMEOUT', 'PRECHECK_TIMEOUT', 'QUERY_CAPACITY_EXCEEDED']) assert.ok(codes.includes(code));
});

test('PR gate executes Token PostgreSQL pagination tests and rejects skips', async () => {
  const workflow = await read('../.github/workflows/pr-check.yml');
  assert.match(workflow, /PINVOU_TEST_TOKEN_DATABASE_URL: postgresql:\/\//);
  assert.match(workflow, /PINVOU_TEST_TOKEN_PAGINATION_GATE=1 go test/);
  for (const name of [
    'TestTokenAdminPaginationPostgresIntegration',
    'TestTokenAdminPaginationPostgresMillionMemberBoundary',
    'TestTokenAdminPaginationMigrationFailsClosedAndIsIdempotent',
  ]) assert.ok(workflow.includes(name), `missing CI gate ${name}`);
  assert.match(workflow, /! grep -F -- '--- SKIP:'/);
  assert.match(workflow, /go test -short -race \.\/\.\.\./);
});
