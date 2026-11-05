import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import test from 'node:test';

// This contract reads the actual repository entry without a build or React
// dependency. Browser evidence separately covers the compiled component.
const root = fileURLToPath(new URL('../../../', import.meta.url));
const feature = resolve(root, 'pinvou3-app/src/features/account-platform');
const source = readFileSync(resolve(feature, 'AdminTokenResourcesWorkspace.jsx'), 'utf8');
const { ownerCandidateQuery, tokenAccountCreateInput, tokenOwnerOrganizationSelection, tokenResourceAccess } = await import(pathToFileURL(resolve(feature, 'admin-token-resource-state.js')));
const { AdminPlatformClient } = await import(pathToFileURL(resolve(feature, 'admin-client.js')));
const options = select => [...select.matchAll(/<option\b[^>]*value="([^"]*)"/g)].map(match => match[1]);

test('actual owner dialog exposes all candidate identity types', () => {
  const dialog = source.slice(source.indexOf('function OwnerDialog('), source.indexOf('function RouteHealth('));
  const select = dialog.match(/<select value=\{ownerType\}[\s\S]*?<\/select>/)?.[0];
  assert.ok(select, 'the real candidate type select must exist');
  assert.deepEqual(options(select), ['USER', 'ORGANIZATION', 'UNIT']);
  for (const type of ['USER', 'ORGANIZATION', 'UNIT']) assert.ok(select.includes(`copy.enumLabels.${type}`), `${type} uses existing translated labels`);
});

test('actual account list exposes server owner enums rather than candidate enums', () => {
  const select = source.match(/<select name="owner_type"[\s\S]*?<\/select>/)?.[0];
  assert.ok(select, 'the real account filter select must exist');
  assert.deepEqual(options(select), ['', 'USER', 'ORG', 'ORG_UNIT']);
});

test('real helpers keep organization-to-unit selection and existing scope payloads', () => {
  const org = { id: '11111111-1111-4111-8111-111111111111', type: 'ORGANIZATION', status: 'ACTIVE' };
  const unit = { id: '22222222-2222-4222-8222-222222222222', type: 'UNIT', status: 'ACTIVE' };
  const user = { id: '33333333-3333-4333-8333-333333333333', type: 'USER', status: 'ACTIVE' };
  assert.equal(ownerCandidateQuery('UNIT', { q: ' public ' }).owner_type, 'ORGANIZATION');
  assert.deepEqual(ownerCandidateQuery('UNIT', { q: ' node ', cursor: ' public-cursor ' }, org.id), { q: 'node', owner_type: 'UNIT', status: 'ACTIVE', organization_id: org.id, cursor: 'public-cursor' });
  assert.equal(ownerCandidateQuery('ORGANIZATION', {}, org.id).organization_id, '');
  assert.deepEqual(tokenAccountCreateInput({ candidate: org, name: ' public org ', priority: '7' }), { owner_type: 'ORG', owner_id: org.id, owner_organization_id: org.id, owner_unit_id: null, name: 'public org', share_scope: 'ORG_ALL', default_priority: 7 });
  assert.deepEqual(tokenAccountCreateInput({ candidate: unit, organization: org, name: 'public unit', priority: 0 }), { owner_type: 'ORG_UNIT', owner_id: unit.id, owner_organization_id: org.id, owner_unit_id: unit.id, name: 'public unit', share_scope: 'OWNER_SUBTREE', default_priority: 0 });
  assert.equal(tokenAccountCreateInput({ candidate: user, name: 'public user' }).share_scope, 'OWNER_UNIT_ONLY');
  assert.equal(tokenAccountCreateInput({ candidate: unit, name: 'public unit' }), null);
  for (const status of ['DISABLED', 'PROVISIONING']) assert.equal(tokenAccountCreateInput({ candidate: { ...org, status }, name: 'public org' }), null);
  assert.equal(tokenAccountCreateInput({ candidate: { ...org, type: 'UNKNOWN' }, name: 'public org' }), null);
});

test('real access helper requires both permissions and enabled forms', () => {
  const both = ['platform.tokens.manage', 'platform.tokens.owner-candidates.read'];
  assert.equal(tokenResourceAccess(both, true).requestOwnerCandidates, true);
  for (const permissions of [[], both.slice(0, 1), both.slice(1)]) assert.equal(tokenResourceAccess(permissions, true).requestOwnerCandidates, false);
  assert.equal(tokenResourceAccess(both, false).requestOwnerCandidates, false);
});

test('history restoration uses the current parent identity instead of stale local selection', () => {
  const id = '11111111-1111-4111-8111-111111111111';
  const other = { id: '33333333-3333-4333-8333-333333333333', type: 'ORGANIZATION', display_label: 'other parent' };
  const cached = { id, type: 'ORGANIZATION', display_label: 'current parent' };
  const restored = { id, type: 'ORGANIZATION', display_label: id };
  assert.strictEqual(tokenOwnerOrganizationSelection('UNIT', id, cached), cached);
  for (const previous of [null, other, { ...cached, type: 'USER' }]) {
    const organization = tokenOwnerOrganizationSelection('UNIT', id, previous);
    assert.deepEqual(organization, restored);
    const candidate = { id: '22222222-2222-4222-8222-222222222222', type: 'UNIT', status: 'ACTIVE' };
    assert.equal(tokenAccountCreateInput({ candidate, organization, name: 'restored unit' }).owner_organization_id, id);
  }
  for (const [type, parent] of [['USER', id], ['ORGANIZATION', id], ['UNIT', ''], ['UNIT', 'invalid']]) {
    assert.equal(tokenOwnerOrganizationSelection(type, parent, cached), null);
  }
  const dialog = source.slice(source.indexOf('function OwnerDialog('), source.indexOf('function RouteHealth('));
  assert.match(dialog, /const organization = tokenOwnerOrganizationSelection\(ownerType, organizationID, selectedOrganization\)/);
  assert.match(dialog, /tokenAccountCreateInput\(\{ candidate: selected, organization, name, priority \}\)/);
});

test('real client sends candidate enums and account enums on their own endpoints', async () => {
  const calls = [];
  const client = Object.create(AdminPlatformClient.prototype);
  client.request = async (...args) => { calls.push(args); return { items: [] }; };
  await client.tokenOwnerCandidates(ownerCandidateQuery('UNIT', {}, '11111111-1111-4111-8111-111111111111'));
  await client.tokenAccounts({ owner_type: 'ORG_UNIT' });
  assert.equal(calls[0][0], 'token');
  const candidate = new URL(calls[0][1], 'https://public.example');
  assert.equal(candidate.pathname, '/admin/v1/owner-candidates');
  assert.equal(candidate.searchParams.get('owner_type'), 'UNIT');
  assert.equal(candidate.searchParams.get('organization_id'), '11111111-1111-4111-8111-111111111111');
  const accounts = new URL(calls[1][1], 'https://public.example');
  assert.equal(accounts.pathname, '/admin/v1/token-accounts');
  assert.equal(accounts.searchParams.get('owner_type'), 'ORG_UNIT');
});
