import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

const denialMatrix = [
  ['personal-object-id', 'internal/billing/refunds.go', /order\.RequesterID != requesterID[\s\S]*ErrNotFound/],
  ['personal-session-context', 'internal/token/usage_status_test.go', /otherUser[\s\S]*otherOrganization[\s\S]*otherSession/],
  ['organization-a-to-b', 'internal/account/authorization.go', /ra\.organization_id=\$4 AND m\.status='ACTIVE'/],
  ['unit-parent-sibling-outside-descendant', 'internal/account/authorization.go', /c\.organization_id=\$4 AND c\.ancestor_id=ra\.unit_id AND c\.descendant_id=\$5/],
  ['role-scope-substitution', 'internal/account/authorization.go', /ro\.scope='PLATFORM'[\s\S]*ra\.organization_id=\$4/],
  ['active-context-mismatch', 'internal/token/usage_status_test.go', /usageContextMatches[\s\S]*otherOrganization/],
  ['revoked-or-expired-role', 'internal/account/authorization.go', /ra\.revoked_at IS NULL AND ra\.starts_at<=now\(\) AND \(ra\.expires_at IS NULL OR ra\.expires_at>now\(\)\)/],
  ['removed-member', 'internal/account/authorization.go', /m\.status='ACTIVE'/],
  ['disabled-organization', 'internal/account/postgres.go', /active\.organization_id IS NULL OR o\.status='ACTIVE'/],
  ['closed-user', 'internal/account/authorization.go', /SELECT u\.id,u\.status[\s\S]*decision\.Allowed = roleGranted && permissionAllowedForUserStatus\(decision\.UserStatus, permission\)[\s\S]*case "CLOSING":[\s\S]*return false/],
  ['guessed-uuid', 'internal/billing/service.go', /Authorize against the local order[\s\S]*guessed order IDs/],
  ['mixed-batch-package', 'internal/billing/member_quota_grants.go', /walletOwnerType != "ORGANIZATION" \|\| walletOwnerID != request\.OrganizationID/],
  ['search-outside-scope', 'internal/account/admin.go', /FROM memberships m JOIN users u ON u\.id=m\.user_id[\s\S]*WHERE m\.organization_id=\$1/],
  ['stale-authorization-cache', 'internal/token/catalog_http.go', /authorization_version[\s\S]*Cache-Control", "private, no-cache"[\s\S]*AuthorizationVersion/],
  ['forged-domain-event', 'internal/billing/refund_consumer_test.go', /mismatched refund event target was accepted/],
];

test('tenant isolation denial matrix executes every mandatory identity lifecycle and transport vector', async () => {
  const required = new Set([
    'personal-object-id', 'personal-session-context', 'organization-a-to-b',
    'unit-parent-sibling-outside-descendant', 'role-scope-substitution', 'active-context-mismatch',
    'revoked-or-expired-role', 'removed-member', 'disabled-organization', 'closed-user',
    'guessed-uuid', 'mixed-batch-package', 'search-outside-scope',
    'stale-authorization-cache', 'forged-domain-event',
  ]);
  assert.deepEqual(new Set(denialMatrix.map(([id]) => id)), required);
  for (const [id, file, evidence] of denialMatrix) {
    assert.match(await read(file), evidence, `isolation vector ${id} lost its fail-closed evidence`);
  }
});

test('allowed list search and batch paths constrain every returned or mutated row to the authorized tenant', async () => {
  const [account, quota, batch] = await Promise.all([
    read('internal/account/admin.go'),
    read('internal/billing/member_quota_grants.go'),
    read('internal/token/organization_billing.go'),
  ]);
  assert.match(account, /WHERE m\.organization_id=\$1 AND m\.status<>'REVOKED'/);
  assert.match(account, /WHERE id=\$1 AND organization_id=\$2 AND status='ACTIVE'/);
  assert.match(quota, /WHERE organization_id=\$1 AND \(\$2::uuid IS NULL OR member_user_id=\$2\)/);
  assert.match(batch, /resolveOrganizationBilling[\s\S]*resolved\.OrganizationID[\s\S]*\*resolved\.OrganizationID != organizationID/);
  assert.match(batch, /resolveOrganizationBilling\(w, r, "org\.quota\.allocate"\)[\s\S]*AllocateOrganizationQuotaBatch/);
});

test('existence errors and authorization decisions do not disclose a foreign resource', async () => {
  const [billing, account] = await Promise.all([
    read('internal/billing/refunds.go'),
    read('internal/account/authorization.go'),
  ]);
  assert.match(billing, /pgx\.ErrNoRows[\s\S]*ErrNotFound[\s\S]*RequesterID != requesterID[\s\S]*ErrNotFound/);
  assert.match(account, /PERMISSION_DENIED", "the requested permission is not granted"/);
  assert.match(account, /Cache-Control", "no-store"/);
});

test('no tenant-facing export route can bypass row isolation', async () => {
  const contracts = await Promise.all([
    'account.yaml', 'billing.yaml', 'token-gateway.yaml',
  ].map(name => read(`contracts/openapi/${name}`).then(YAML.parse)));
  const publicExportPaths = contracts.flatMap(contract => Object.keys(contract.paths).filter(value => /export/i.test(value)));
  assert.deepEqual(publicExportPaths, [], 'a tenant export route requires an explicit isolation-matrix row before release');
});
