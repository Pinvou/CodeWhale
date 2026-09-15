import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

import {
  adminSectionReadOnly,
  adminVisibleSections,
  authorizationVersionChanged,
  authorizationVersions,
  personalSurfaceAccess,
} from '../../../pinvou3-app/src/features/account-platform/authorization-ui.js';

const snapshot = ({ personal = [], platform = [], active = [], organization = '' } = {}) => ({
  personal_permissions: personal,
  platform_permissions: platform,
  active_context: {
    permissions: active,
    organization_id: organization || null,
    context_version: 7,
    personal_authorization_version: 3,
    platform_authorization_version: 5,
    organization_authorization_version: organization ? 11 : 0,
  },
});

test('personal surfaces use personal permissions independently from the active organization', () => {
  const value = snapshot({
    personal: ['personal.profile.read', 'personal.account.close.read', 'personal.recharge.create', 'personal.invitations.read', 'personal.invitations.accept'],
    active: ['org.usage.read.self'],
    organization: '95f13b84-ccfb-4b78-93c4-445af61e06db',
  });
  assert.deepEqual(personalSurfaceAccess(value), {
    profileRead: true,
    profileUpdate: false,
    contextSwitch: false,
    closureRead: true,
    closureRequest: false,
    invitationsRead: true,
    invitationsAccept: true,
    billingRead: false,
    rechargeCreate: false,
    usageRead: true,
    usageCancel: false,
  });
});

test('personal bootstrap remains usable while an older Account service omits root permission collections', () => {
  const value = {
    active_context: {
      organization_id: null,
      permissions: [
        'personal.profile.read',
        'personal.billing.read',
        'personal.recharge.create',
        'personal.usage.read',
        'personal.usage.cancel',
      ],
    },
  };
  assert.deepEqual(personalSurfaceAccess(value), {
    profileRead: true,
    profileUpdate: false,
    contextSwitch: false,
    closureRead: false,
    closureRequest: false,
    invitationsRead: false,
    invitationsAccept: false,
    billingRead: true,
    rechargeCreate: true,
    usageRead: true,
    usageCancel: true,
  });
});

test('admin navigation and read-only state are derived from exact platform permissions', () => {
  const value = snapshot({ platform: ['platform.users.read', 'platform.audit.read', 'platform.audit.export'] });
  assert.deepEqual(adminVisibleSections(value), ['overview', 'users', 'audit', 'settings']);
  assert.equal(adminSectionReadOnly(value, 'users'), true);
  assert.equal(adminSectionReadOnly(value, 'audit'), false);
});

test('independent admin routes require their exact read capability and hide unsupported prototype pages', () => {
  assert.deepEqual(adminVisibleSections(snapshot({ platform: ['platform.roles.read'] })), ['roles', 'settings']);
  assert.deepEqual(adminVisibleSections(snapshot({ platform: ['platform.products.read'] })), ['products', 'settings']);
  assert.deepEqual(adminVisibleSections(snapshot({ platform: ['platform.billing.wallets.read'] })), ['overview', 'billing', 'payments', 'approvals', 'settings']);
  assert.deepEqual(adminVisibleSections(snapshot()), []);
  assert.ok(!adminVisibleSections(snapshot({ platform: ['platform.organizations.read'] })).includes('myorg'));
});

test('every independent authorization version invalidates a rendered snapshot', () => {
  const current = authorizationVersions(snapshot());
  for (const key of ['context_version', 'personal_authorization_version', 'platform_authorization_version']) {
    assert.equal(authorizationVersionChanged(current, { ...current, [key]: current[key] + 1 }), true, key);
  }
  assert.equal(authorizationVersionChanged(current, { ...current }), false);
});

test('desktop, portal, and admin sources consume authoritative permissions without role-name gates', async () => {
  const sources = await Promise.all([
    readFile(new URL('../../../pinvou3-app/src/features/account-platform/AccountPlatformPopover.jsx', import.meta.url), 'utf8'),
    readFile(new URL('../../../pinvou3-app/src/features/account-platform/AccountPlatformView.jsx', import.meta.url), 'utf8'),
    readFile(new URL('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8'),
  ]);
  for (const source of sources) {
    assert.match(source, /personalSurfaceAccess|adminVisibleSections|hasPermission/);
    assert.doesNotMatch(source, /platform_roles|personal_roles|roles\.includes\(/);
  }
});

test('Portal forwards and desktop preserves every independent authorization version', async () => {
  const [portal, rust, bff, desktop] = await Promise.all([
    readFile(new URL('../../internal/portal/service.go', import.meta.url), 'utf8'),
    readFile(new URL('../../../pinvou3-app/src-tauri/src/features/account_platform/mod.rs', import.meta.url), 'utf8'),
    readFile(new URL('../../../pinvou3-app/src/features/account-platform/bff-client.js', import.meta.url), 'utf8'),
    readFile(new URL('../../../pinvou3-app/src/features/account-platform/desktop-client.js', import.meta.url), 'utf8'),
  ]);
  for (const header of ['X-Personal-Authorization-Version', 'X-Platform-Authorization-Version', 'X-Organization-Authorization-Version']) {
    assert.match(portal, new RegExp(header));
    assert.match(rust, new RegExp(header));
    assert.match(bff, new RegExp(header));
  }
  assert.match(desktop, /personalAuthorizationVersion/);
  assert.match(desktop, /platformAuthorizationVersion/);
  assert.match(desktop, /organizationAuthorizationVersion/);
});
