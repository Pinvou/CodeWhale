import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { OrganizationClient } from '../../../pinvou3-app/src/features/account-platform/organization-client.js';
import { hasPermission } from '../../../pinvou3-app/src/features/account-platform/authorization-ui.js';
import { hasUnitScopePermission, memberWithinUnitScope, organizationScopeRoots, requireOrganizationAuthorization, requireScopedMemberMutation, scopedUnitIDs } from '../../../pinvou3-app/src/features/account-platform/organization-scope.js';
import { AccountPlatformBffClient } from '../../../pinvou3-app/src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../../../pinvou3-app/src/features/account-platform/desktop-client.js';
import { organizationCopy } from '../../../pinvou3-app/src/shared/i18n.js';
const dto = JSON.parse(await readFile(new URL('../../../pinvou3-app/tests/fixtures/organization-unit-scope-dto.json', import.meta.url), 'utf8'));
const clone = value => JSON.parse(JSON.stringify(value));

test('production Account DTO keeps disjoint unit bindings separate from global grants', () => {
 for (const kind of ['unit', 'mixed']) {
  const { me, context } = dto[kind];
  assert.equal(requireOrganizationAuthorization(context, me), context);
  assert.equal(hasPermission(me, 'org.members.manage'), false);
  assert.equal(hasPermission(me, 'org.units.manage'), false);
  assert.deepEqual(new Set(organizationScopeRoots(context)), new Set([dto.root_a, dto.root_c]));
  for (const root of [dto.root_a, dto.root_c]) assert.equal(hasUnitScopePermission(context, root, 'org.members.manage'), true);
  assert.equal(hasUnitScopePermission(context, dto.outside_b, 'org.members.manage'), false);
 }
 assert.equal(hasPermission(dto.mixed.me, 'org.members.read'), true);
 assert.equal(hasPermission(dto.unit.me, 'org.members.read'), false);
});

test('authority response rejects wrong actors, contexts, versions and malformed bindings', () => {
 const { me, context } = dto.unit;
 for (const fields of [{ user_id: dto.target_id }, { organization_id: dto.outside_b }, { context_type: 'PERSONAL' }, { context_version: context.context_version + 1 }, { organization_authorization_version: 0 }, { organization_authorization_version: context.organization_authorization_version + 1 }, { unit_authorization_bindings: null }, { unit_authorization_bindings: [...context.unit_authorization_bindings, context.unit_authorization_bindings[0]] }, { unit_authorization_bindings: [{ unit_id: dto.root_a, role: 'UNIT_ADMIN', permissions: ['platform.organizations.manage'] }] }]) {
  assert.throws(() => requireOrganizationAuthorization({ ...context, ...fields }, me), /INVALID_ORGANIZATION_AUTHORIZATION/);
 }
 const missing = clone(context); delete missing.organization_authorization_version;
 assert.throws(() => requireOrganizationAuthorization(missing, me));
});

test('selected tree validates closure and cannot borrow objects from another authorized root', () => {
 const ids = scopedUnitIDs(dto.unit.tree_a, dto.root_a);
 assert.equal(ids.has(dto.child_a), true); assert.equal(ids.has(dto.root_c), false);
 for (const items of [[], [...dto.unit.tree_a.items, dto.unit.tree_a.items[0]], [...dto.unit.tree_a.items, { unit_id: dto.root_c, parent_id: null, status: 'ACTIVE' }], dto.unit.tree_a.items.map(unit => unit.unit_id === dto.child_a ? { ...unit, parent_id: dto.child_a } : unit)]) {
  assert.throws(() => scopedUnitIDs({ items }, dto.root_a));
 }
});

test('member mutation requires all old and new memberships inside the same chosen subtree', () => {
 const member = dto.unit.members_a.items.find(item => item.user_id === dto.target_id);
 const ids = scopedUnitIDs(dto.unit.tree_a, dto.root_a), before = clone(member);
 assert.equal(memberWithinUnitScope(member, ids), true);
 assert.equal(requireScopedMemberMutation(dto.unit.context, dto.root_a, ids, member, [dto.root_a]), member);
 assert.equal(requireScopedMemberMutation(dto.unit.context, dto.root_a, ids, member, []), member);
 for (const units of [[dto.outside_b], [dto.root_c], [dto.child_a, dto.root_c]]) assert.throws(() => requireScopedMemberMutation(dto.unit.context, dto.root_a, ids, member, units));
 for (const unit_ids of [[], [dto.child_a, dto.root_c]]) {
  const cross = { ...member, unit_ids }; assert.equal(memberWithinUnitScope(cross, ids), false); assert.throws(() => requireScopedMemberMutation(dto.unit.context, dto.root_a, ids, cross, [dto.root_a]));
 }
 assert.deepEqual(member, before);
});

test('organization client preserves unscoped callers and sends exact scoped tree/member queries', async () => {
 const calls = [], controller = new AbortController();
 const api = new OrganizationClient({ request: async (area, path, options) => { calls.push({ area, path, options }); return path.includes('/authorization-context') ? dto.unit.context : path.includes('/members?') ? { items: [], next_cursor: null } : dto.unit.tree_a; } });
 await api.authorizationContext(controller.signal); await api.units(dto.organization_id, controller.signal); await api.units(dto.organization_id, controller.signal, dto.root_a); await api.members(dto.organization_id, { unit_id: dto.root_a, cursor: 'owned-page', limit: 50 }, controller.signal);
 assert.equal(calls[0].path, '/v1/me/authorization-context'); assert.equal(calls[0].options.signal, controller.signal);
 assert.equal(calls[1].path, `/v1/organizations/${dto.organization_id}/units`);
 assert.equal(calls[2].path, `/v1/organizations/${dto.organization_id}/units?root_unit_id=${dto.root_a}`);
 const query = new URL(calls[3].path, 'https://fixture.example.test').searchParams;
 assert.equal(query.get('unit_id'), dto.root_a); assert.equal(query.get('cursor'), 'owned-page');
 const count = calls.length; await assert.rejects(api.units(dto.organization_id, controller.signal, 'invalid')); assert.equal(calls.length, count);
});

test('the same explicit scope uses real BFF and Desktop transports without host permission branches', async () => {
 const original = globalThis.fetch, calls = [];
 globalThis.fetch = async (url, options) => { calls.push({ url, options }); return new Response(JSON.stringify(String(url).endsWith('/authorization-context') ? dto.unit.context : dto.unit.tree_a), { status: 200 }); };
 try {
  const api = new OrganizationClient(new AccountPlatformBffClient());
  assert.equal((await api.authorizationContext()).user_id, dto.unit.me.user_id); await api.units(dto.organization_id, undefined, dto.root_a);
  assert.equal(calls[1].url, `/api/account/v1/organizations/${dto.organization_id}/units?root_unit_id=${dto.root_a}`);
 } finally { globalThis.fetch = original; }
 const nativeCalls = [];
 const api = new OrganizationClient(new AccountPlatformDesktopClient({ request: async input => { nativeCalls.push(input); return { status: 200, body: input.resource.endsWith('/authorization-context') ? dto.unit.context : dto.unit.tree_a }; } }));
 await api.authorizationContext(); await api.units(dto.organization_id, undefined, dto.root_a);
 assert.equal(nativeCalls[0].resource, '/v1/me/authorization-context'); assert.equal(nativeCalls[1].resource, `/v1/organizations/${dto.organization_id}/units?root_unit_id=${dto.root_a}`);
});

test('all unit-scope product copy is owned by shared i18n in Chinese English and Japanese', () => {
 for (const language of ['zh', 'en', 'ja']) for (const key of ['unitScopeTitle', 'unitScope', 'unitScopeRoot', 'unitScopeHint', 'unitScopeMembersHint', 'unitScopeInvitationHint', 'unitScopeInviteUnitsHint', 'unitScopeAssignmentHint']) {
  assert.equal(Object.hasOwn(organizationCopy[language], key), true); assert.equal(typeof organizationCopy[language][key], 'string'); assert.ok(organizationCopy[language][key]);
 }
});
