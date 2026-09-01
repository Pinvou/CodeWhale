import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import {
  canGrantRole,
  canRevokeBinding,
  filterBindings,
  filterRoles,
  permissionGroups,
  roleCatalogScope,
  roleCatalogScopes,
  roleGrantMode,
  roleScopeLabel,
  roleScopes,
  summarizeRoleScopes,
} from '../src/features/account-platform/admin-role-management.js';

const copy = {
  navGroups: { identity: '身份与组织', finance: '财务与支付', models: '模型与 Token', governance: '治理与安全' },
  roles: {
    names: { PLATFORM_BILLING_OPERATOR: '平台计费操作员', PERSONAL_USER: '个人用户' },
    scopes: { PERSONAL: '个人', PLATFORM: '平台', ORGANIZATION: '组织', UNIT: '组织节点' },
    lifecycleManaged: '生命周期维护', ownershipFlow: '所有权流程', secureFlow: '专属安全流程', directGrant: '直接授权',
    permissionActions: { read: '查看', request: '发起' },
    permissionTokens: { billing: '计费', wallets: '钱包', usage: '用量' },
  },
};

const roles = [
  { code: 'PERSONAL_USER', scope: 'PERSONAL', assignable: false, permissions: ['personal.profile.read'] },
  { code: 'PLATFORM_BILLING_OPERATOR', scope: 'PLATFORM', assignable: true, description: 'Billing operations', permissions: ['platform.billing.wallets.read', 'platform.usage.read'] },
  { code: 'OWNER', scope: 'ORGANIZATION', assignable: false, permissions: ['org.organization.read'] },
  { code: 'UNIT_ADMIN', scope: 'UNIT', assignable: true, permissions: ['org.members.read'] },
];

test('role scope and grantability follow server definitions and protected lifecycles', () => {
  assert.equal(roleScopeLabel(copy, 'PLATFORM'), '平台');
  assert.equal(roleGrantMode(copy, roles[0]), '生命周期维护');
  assert.equal(roleGrantMode(copy, roles[2]), '所有权流程');
  assert.equal(canGrantRole(roles[1]), true);
  assert.equal(canGrantRole(roles[2]), false);
  assert.equal(canRevokeBinding({ role_code: 'PLATFORM_BILLING_OPERATOR', status: 'ACTIVE' }, new Map(roles.map((role) => [role.code, role]))), true);
  assert.equal(canRevokeBinding({ role_code: 'OWNER', status: 'ACTIVE' }, new Map(roles.map((role) => [role.code, role]))), false);
  assert.deepEqual(roleCatalogScopes, ['PERSONAL', 'PLATFORM', 'ORGANIZATION']);
  assert.deepEqual(roleScopes, ['PERSONAL', 'PLATFORM', 'ORGANIZATION', 'UNIT']);
  assert.equal(roleCatalogScope('UNIT'), 'ORGANIZATION');
  assert.deepEqual(summarizeRoleScopes(roles), { PERSONAL: 1, PLATFORM: 1, ORGANIZATION: 2 });
});

test('role catalog action column keeps its view action on one line', async () => {
  const styles = await readFile(new URL('../src/features/account-platform/admin-role-management.css', import.meta.url), 'utf8');
  assert.match(styles, /\.adm-role-table th:last-child,\.adm-role-table td:last-child\{width:96px;min-width:96px;white-space:nowrap\}/);
  assert.match(styles, /\.adm-role-table td:last-child \.adm-name-link\{white-space:nowrap\}/);
});

test('role catalog searches localized names and exact permission codes', () => {
  assert.deepEqual(filterRoles(roles, { query: '', scope: '', grantable: '' }, copy).map((role) => role.code), ['OWNER', 'UNIT_ADMIN', 'PERSONAL_USER', 'PLATFORM_BILLING_OPERATOR']);
  assert.deepEqual(filterRoles(roles, { query: '平台计费', scope: '', grantable: '' }, copy).map((role) => role.code), ['PLATFORM_BILLING_OPERATOR']);
  assert.deepEqual(filterRoles(roles, { query: 'wallets.read', scope: 'PLATFORM', grantable: 'direct' }, copy).map((role) => role.code), ['PLATFORM_BILLING_OPERATOR']);
  assert.deepEqual(filterRoles(roles, { query: '', scope: 'ORGANIZATION', grantable: '' }, copy).map((role) => role.code), ['OWNER', 'UNIT_ADMIN']);
  assert.equal(permissionGroups(copy, roles[1])[0].label, '财务与支付');
});

test('grant records search business labels without relying on email or raw UUID entry', () => {
  const bindings = [{ binding_id: 'b1', user_id: 'u1', role_code: 'PLATFORM_BILLING_OPERATOR', scope: 'PLATFORM', status: 'ACTIVE' }];
  const users = new Map([['u1', { username: 'vinpay', display_name: '王敏', user_code: 'USR-001' }]]);
  const organizations = new Map();
  const result = filterBindings(bindings, { query: '王敏', scope: 'PLATFORM', status: 'ACTIVE' }, new Map(roles.map((role) => [role.code, role])), users, organizations, copy);
  assert.equal(result.length, 1);
});
