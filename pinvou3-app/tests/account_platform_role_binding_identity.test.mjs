import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { createHash } from 'node:crypto';
import { createRequire } from 'node:module';
import { pathToFileURL } from 'node:url';
import React, { act } from 'react';
import domino from '@mixmark-io/domino';
import { transformWithOxc } from 'vite';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { readAdminAuthorizationSnapshot } from '../src/features/account-platform/admin-authorization-snapshot.js';
import { platformRolePending } from '../src/features/account-platform/platform-role-management.js';
import { adminRolePresentationCopy, platformRoleCopy } from '../src/shared/i18n.js';

const sourceRoot = new URL('../src/features/account-platform/', import.meta.url);
const nodeRequire = createRequire(import.meta.url);
const owner = '019bb002-0000-7000-8000-000000000001';
const userIDs = ['019bb002-0000-7000-8000-000000000002', '019bb002-0000-7000-8000-000000000003'];
const bindingIDs = ['019bb002-0000-7000-8000-000000000004', '019bb002-0000-7000-8000-000000000005'];
const personal = ['personal.billing.read', 'personal.session.login', 'personal.profile.read', 'personal.profile.update', 'personal.security.manage'];
const role = { code: 'PLATFORM_MODEL_ADMIN', scope: 'PLATFORM', assignable: true, permissions: ['platform.models.read'] };
const protectedRole = { code: 'PLATFORM_SUPER_ADMIN', scope: 'PLATFORM', assignable: false, permissions: ['platform.models.read'] };
// Distinct registry codes follow the real Go GenerateUserCode SHA-256 checksum contract.
const profile = permissions => ({ user_id: owner, user_code: 'USR-2222-2222-2222-2222-8X', status: 'ACTIVE', username: 'Controlled administrator', display_name: null, mobile_verified: false, commerce_customer_status: 'NOT_CONFIGURED', personal_permissions: personal, platform_permissions: permissions, active_context: { context_type: 'PERSONAL', membership_version: 1, context_version: 1, authorization_version: 1, personal_authorization_version: 1, platform_authorization_version: 3, organization_authorization_version: 0, permissions: personal } });
const userProfiles = userIDs.map((user_id, index) => ({ user_id, user_code: index ? 'USR-1111-1111-1111-1111-DP' : 'USR-0000-0000-0000-0000-Q3', username: 'Controlled user ' + index, display_name: 'Same display name', status: 'ACTIVE', created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-01T00:00:00Z' }));
const bindings = userIDs.map((user_id, index) => ({ binding_id: bindingIDs[index], user_id, role_code: role.code, scope: 'PLATFORM', grant_source: 'ADMIN', created_by: owner, starts_at: '2026-10-01T00:00:00Z', expires_at: null, status: 'ACTIVE' }));
const deferred = () => { let resolve; const promise = new Promise(yes => { resolve = yes; }); return { promise, resolve }; };
const options = { concurrency: false, timeout: 15000 };
const hashes = {}, compiled = new Map();

// Transform actual JSX and relocate imports only. CSS is omitted; domino does
// not prove layout, native dialog focus/keyboard, browser or OS behavior.
async function compile(name) {
  if (compiled.has(name)) return compiled.get(name);
  const raw = await readFile(new URL(name, sourceRoot), 'utf8');
  hashes[name] = createHash('sha256').update(raw).digest('hex');
  let source = raw.replace(/^import ['"][^'"]+\.css['"];?\r?$/gm, '');
  const imports = [...source.matchAll(/from (['"])([^'"]+)\1/g)];
  for (const match of imports) {
    const specifier = match[2];
    const resolved = specifier.endsWith('.jsx') ? await compile(specifier.replace(/^\.\//, ''))
      : specifier.startsWith('.') ? new URL(specifier, sourceRoot).href : pathToFileURL(nodeRequire.resolve(specifier)).href;
    source = source.replace(match[0], 'from ' + JSON.stringify(resolved));
  }
  const output = await transformWithOxc(source, name, { jsx: { runtime: 'classic' }, sourcemap: false });
  const url = 'data:text/javascript;base64,' + Buffer.from(output.code).toString('base64');
  compiled.set(name, url);
  return url;
}

async function withWorkspace(action, configuration = {}) {
  const window = domino.createWindow('<!doctype html><html><body></body></html>');
  // Domino already supplies a no-op, non-writable focus implementation.
  // These tests validate command/identity state, never focus or accessibility.
  // Advertise input-event delivery for controlled React onChange, not native editing.
  window.document.oninput = null;
  const initialSearch = configuration.initialSearch || '?section=roles&role_view=bindings';
  const location = { pathname: '/admin.html', search: initialSearch, hash: '', href: '/admin.html' + initialSearch };
  const replacements = { window, document: window.document, navigator: window.navigator, HTMLElement: window.HTMLElement, Node: window.Node, location, fetch: globalThis.fetch, IS_REACT_ACT_ENVIRONMENT: true };
  const previous = new Map(Object.keys(replacements).map(name => [name, Object.getOwnPropertyDescriptor(globalThis, name)]));
  const client = new AdminPlatformClient(), calls = [], gates = [], fetchTasks = [];
  const server = { csrf: 'controlled-A', permissions: ['platform.roles.read', 'platform.roles.assign', 'platform.users.read'], rows: bindings.map(item => ({ ...item })), failUsers: new Set(), heldUsers: new Map(), failBindings: new Map(), heldBindings: new Map(), loseDelete: false, ...configuration };
  let root, container, snapshot, language = 'en';
  const route = { section: 'roles', search: location.search, navigate(section, updates) { const next = new URLSearchParams(route.search); next.set('section', section); for (const [key, value] of Object.entries(updates)) value == null ? next.delete(key) : next.set(key, value); route.section = section; route.search = location.search = '?' + next; location.href = location.pathname + location.search; } };
  try {
    for (const [name, value] of Object.entries(replacements)) Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
    globalThis.fetch = (url, init = {}) => {
      const task = (async () => {
        const method = init.method === undefined ? 'GET' : init.method;
        calls.push({ url, method, body: init.body, csrf: init.headers?.['X-CSRF-Token'], key: init.headers?.['Idempotency-Key'] });
        let body, status = 200, gate;
        if (url === '/api/session') body = { authenticated: true, csrf_token: server.csrf };
        else if (url === '/api/account/v1/account/bootstrap') { assert.equal(method, 'POST'); assert.equal(init.body, '{}'); assert.equal(init.headers['X-CSRF-Token'], server.csrf); body = profile(server.permissions); }
        else if (url === '/api/admin/account/admin/v1/roles') { assert.equal(method, 'GET'); body = { items: [role, protectedRole] }; }
        else if (url.startsWith('/api/admin/account/admin/v1/role-bindings') && method === 'GET') {
          const filter = new URL(url, 'http://controlled.invalid').searchParams.get('user_id');
          gate = server.heldBindings.get(filter); server.heldBindings.delete(filter);
          status = server.failBindings.get(filter) || 200;
          body = status === 200 ? { items: server.rows.filter(item => !filter || item.user_id === filter), truncated: false } : { code: 'CONTROLLED_LIST_FAILURE', message: 'controlled failure' };
        } else if (url.startsWith('/api/admin/account/admin/v1/users/')) {
          assert.equal(method, 'GET'); const userID = decodeURIComponent(url.split('/').at(-1));
          assert.ok(server.permissions.includes('platform.users.read'), 'no unauthorized profile lookup');
          assert.ok(userProfiles.some(user => user.user_id === userID), 'no broad directory lookup');
          gate = server.heldUsers.get(userID); server.heldUsers.delete(userID);
          if (server.failUsers.has(userID)) { status = 503; body = { code: 'ADMIN_UNAVAILABLE', message: 'controlled unavailable' }; }
          else body = userProfiles.find(user => user.user_id === userID);
        } else if (method === 'DELETE') {
          assert.ok(url.startsWith('/api/admin/account/admin/v1/role-bindings/'));
          assert.equal(init.body, undefined); assert.equal(init.headers['X-CSRF-Token'], server.csrf);
          const bindingID = url.split('/').at(-1), row = server.rows.find(item => item.binding_id === bindingID); assert.ok(row);
          row.status = 'REVOKED'; row.revoked_by = owner; body = { ...row };
          if (server.loseDelete) { server.loseDelete = false; throw new Error('controlled lost ACK'); }
        } else assert.fail('unexpected HTTP boundary ' + method + ' ' + url);
        const response = new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', 'X-Platform-Authorization-Version': '3', 'X-Context-Version': '1' } });
        if (gate) { const decode = response.json.bind(response); response.json = async () => { gate.started.resolve(); await gate.promise; return decode(); }; }
        return response;
      })(); fetchTasks.push(task); void task.catch(() => {}); return task;
    };
    const { AdminRolesWorkspace } = await import(await compile('AdminRolesWorkspace.jsx'));
    const { PlatformRoleBindings } = await import(await compile('PlatformRoleBindings.jsx'));
    let typedBindings;
    snapshot = await readAdminAuthorizationSnapshot(client);
    const { createRoot } = await import('react-dom/client'); container = window.document.createElement('main'); window.document.body.appendChild(container); root = createRoot(container);
    function CommitProbe({ properties, beforePassive }) {
      React.useLayoutEffect(() => { beforePassive?.(); }, [beforePassive]);
      return React.createElement(AdminRolesWorkspace, properties);
    }
    const render = async (locale = language, beforePassive) => {
      language = locale;
      const properties = { client, me: snapshot.authorization, acceptedSessionEpoch: snapshot.sessionEpoch, language, route, initialTarget: new URLSearchParams(route.search).get('role_user_id') || '', onAuthorizationChange() {} };
      await act(async () => root.render(React.createElement(CommitProbe, { properties, beforePassive })));
    };
    const renderBindings = async (locale = language) => {
      language = locale; typedBindings ||= await client.platformRoleBindings();
      await act(async () => root.render(React.createElement(PlatformRoleBindings, { client, me: snapshot.authorization, acceptedSessionEpoch: snapshot.sessionEpoch, words: adminRolePresentationCopy[language], copy: platformRoleCopy[language], bindings: typedBindings, target: '', blocked: false, routeScope: route.section + ':' + route.search, active: true, canRevoke: () => false, onApplyTarget() {}, onView() {}, onRevoke() {} })));
    };
    const click = async node => { assert.ok(node, 'real production control'); await act(async () => node.dispatchEvent(new window.Event('click', { bubbles: true, cancelable: true }))); };
    const button = (selector, label) => [...document.querySelectorAll(selector)].find(node => node.textContent === label);
    const edit = async (node, value) => {
      assert.ok(node, 'actual filter control');
      await act(async () => {
        if (node.tagName === 'SELECT') { node.value = value; node.dispatchEvent(new window.Event('change', { bubbles: true })); }
        else { Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set.call(node, value); node.dispatchEvent(new window.Event('input', { bubbles: true })); }
      });
    };
    const submit = async form => { assert.ok(form); await act(async () => form.dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }))); };
    const hold = (map, id) => { const gate = { ...deferred(), started: deferred() }; gates.push(gate); map.set(id, gate); return gate; };
    const holdUser = userID => hold(server.heldUsers, userID);
    const holdBinding = userID => hold(server.heldBindings, userID);
    await action({ server, client, calls, route, location, render, renderBindings, click, button, edit, submit, holdUser, holdBinding, container, words: locale => adminRolePresentationCopy[locale], copy: locale => platformRoleCopy[locale], async settle(gate) { await act(async () => gate.resolve()); }, async accept() { snapshot = await readAdminAuthorizationSnapshot(client); }, snapshot: () => snapshot, deletes: () => calls.filter(call => call.method === 'DELETE'), row: id => [...container.querySelectorAll('.adm-role-binding-table tbody tr')].find(node => node.textContent.includes(id)) });
  } finally {
    try { const retire = async () => { for (const gate of gates) gate.resolve(); client.retireSession(); await Promise.allSettled(fetchTasks); root?.unmount(); platformRolePending(client, owner, null); }; if (root) await act(retire); else await retire(); }
    finally {
      for (const [name, descriptor] of previous) descriptor ? Object.defineProperty(globalThis, name, descriptor) : delete globalThis[name];
    }
  }
}


test('Actual binding identities and three-language revocation review keep the exact canonical target through cancel and confirm', options, async context => {
  for (const locale of ['zh', 'en', 'ja']) await withWorkspace(async h => {
    await h.render(locale);
    const words = h.words(locale), copy = h.copy(locale);
    for (let index = 0; index < 2; index++) {
      const row = h.row(userIDs[index]); assert.ok(row);
      assert.ok(row.textContent.includes('Same display name'));
      assert.ok(row.textContent.includes(userProfiles[index].user_code));
      assert.ok(row.textContent.includes(userIDs[index]));
      assert.ok(row.querySelector('.adm-binding-danger'));
    }
    await h.click(h.button('.adm-role-binding-table tbody tr:last-child button', words.view));
    assert.ok(document.querySelector('[role=dialog]').textContent.includes(userIDs[1]));
    await h.click(h.button('[role=dialog] button', copy.revoke));
    let review = document.querySelector('.adm-role-revoke-review'); assert.ok(review);
    assert.ok(review.textContent.includes(userIDs[1]) && review.textContent.includes(bindingIDs[1]));
    assert.ok(review.textContent.includes(words.scopes.PLATFORM) && review.textContent.includes(words.revokeBindingImpact));
    assert.equal(document.querySelectorAll('[role=dialog]').length, 1, 'confirmation replaces the detail, never nests aria-modal dialogs');
    assert.equal(h.deletes().length, 0, 'opening a review never dispatches');
    await h.click(h.button('[role=dialog] button', words.cancel));
    assert.equal(document.querySelectorAll('.adm-role-revoke-review').length, 0);
    assert.ok(document.querySelector('[role=dialog]').textContent.includes(userIDs[1]));
    assert.equal(h.deletes().length, 0, 'cancel returns to the same detail without a write');
    await h.click(h.button('[role=dialog] button', copy.revoke));
    await h.click(h.button('.adm-role-revoke-review button', copy.revoke));
    assert.equal(h.deletes().length, 1);
    assert.equal(h.deletes()[0].url, '/api/admin/account/admin/v1/role-bindings/' + bindingIDs[1], 'same-name users cannot redirect revocation');
    assert.equal(h.deletes()[0].body, undefined);
    assert.equal(document.querySelectorAll('[role=dialog]').length, 0);
    assert.ok(h.container.textContent.includes(words.revokedNotice));
  });
  context.diagnostic(JSON.stringify({ actual_jsx_sources: hashes, boundary: 'controlled fetch/JSON; no CSS, browser, keyboard/focus or OS evidence' }));
});

test('Missing user-read permission and partial profile failure preserve binding access and explicit recovery', options, async () => {
  await withWorkspace(async h => {
    await h.render();
    assert.equal(h.calls.filter(call => call.url.includes('/users/')).length, 0);
    assert.ok(h.row(userIDs[0]).textContent.includes(h.words('en').bindingUserNoPermission));
    assert.ok(h.row(userIDs[0]).textContent.includes(userIDs[0]));
    await h.click(h.button('.adm-role-binding-table tbody tr:first-child button', h.words('en').view));
    assert.ok(document.querySelector('[role=dialog]').textContent.includes(userIDs[0]));
    assert.equal(document.querySelectorAll('.adm-role-drawer-foot button').length, 0, 'read-only actor gets no revoke action');
    assert.equal(h.deletes().length, 0);
  }, { permissions: ['platform.roles.read'] });
  await withWorkspace(async h => {
    h.server.failUsers.add(userIDs[0]); await h.render();
    assert.ok(h.row(userIDs[0]).textContent.includes(h.words('en').bindingUserUnavailable));
    assert.ok(h.row(userIDs[1]).textContent.includes('Same display name'), 'one failed profile does not blank another');
    assert.ok(h.row(userIDs[0]).querySelector('.adm-binding-danger'), 'profile failure does not remove an eligible revoke');
    h.server.failUsers.clear(); await h.click(h.button('.adm-binding-lookup-note button', h.words('en').reloadBindingUsers));
    assert.ok(h.row(userIDs[0]).textContent.includes(userProfiles[0].user_code));
    assert.equal(h.deletes().length, 0);
  });
});

test('Held old profile receipts cannot be relabelled after live session retirement and accepted recovery', options, async () => {
  await withWorkspace(async h => {
    const old = h.holdUser(userIDs[0]); await h.renderBindings(); await old.started.promise;
    assert.ok(h.row(userIDs[0]).textContent.includes(h.words('en').bindingUserLoading));
    h.server.csrf = 'controlled-B'; await h.client.session();
    await h.renderBindings('ja'); const before = h.calls.filter(call => call.url.includes('/users/')).length;
    await h.settle(old); await h.renderBindings('ja');
    assert.ok(!h.row(userIDs[0]).textContent.includes('Same display name'));
    assert.ok(h.row(userIDs[0]).textContent.includes(h.words('ja').bindingUserRetired));
    assert.equal(h.calls.filter(call => call.url.includes('/users/')).length, before, 'old accepted lineage cannot re-admit metadata reads');
    await h.accept(); await h.renderBindings('ja');
    assert.ok(h.row(userIDs[0]).textContent.includes(userProfiles[0].user_code));
    assert.equal(h.deletes().length, 0);
  });
});

test('Retired routes and lost fresh assignment authority cannot dispatch an old review', options, async () => {
  await withWorkspace(async h => {
    await h.render(); const copy = h.copy('en');
    await h.click(h.row(userIDs[0]).querySelector('.adm-binding-danger'));
    const staleConfirm = h.button('.adm-role-revoke-review button', copy.revoke); assert.ok(staleConfirm);
    h.location.search = '?section=users';
    await h.click(staleConfirm);
    assert.equal(h.deletes().length, 0, 'live URL fence applies before React sees new route props');
    h.route.section = 'users'; h.route.search = h.location.search; await h.render();
    assert.equal(document.querySelectorAll('.adm-role-revoke-review').length, 0);
    h.route.section = 'roles'; h.route.search = h.location.search = '?section=roles&role_view=bindings'; await h.render();
    await h.click(h.row(userIDs[1]).querySelector('.adm-binding-danger'));
    h.server.permissions = ['platform.roles.read', 'platform.users.read'];
    await h.click(h.button('.adm-role-revoke-review button', copy.revoke));
    assert.equal(h.deletes().length, 0, 'existing fresh-authority check still rejects before DELETE');
    assert.ok(h.container.textContent.includes(copy.denied));
    assert.equal(platformRolePending(h.client, owner), null);
  });
});

test('Unknown revoke recovery retains its original minimal command across navigation and targets the same grant', options, async () => {
  await withWorkspace(async h => {
    await h.render(); h.server.loseDelete = true;
    await h.click(h.row(userIDs[1]).querySelector('.adm-binding-danger'));
    await h.click(h.button('.adm-role-revoke-review button', h.copy('en').revoke));
    const pending = platformRolePending(h.client, owner);
    assert.deepEqual(pending, { owner, kind: 'revoke', bindingID: bindingIDs[1], unknown: true });
    assert.equal(h.deletes().length, 1); assert.equal(document.querySelectorAll('[role=dialog]').length, 0);
    h.route.section = 'users'; h.route.search = h.location.search = '?section=users'; await h.render();
    h.route.section = 'roles'; h.route.search = h.location.search = '?section=roles&role_view=bindings&role_user_id=' + userIDs[0]; await h.render();
    assert.equal(platformRolePending(h.client, owner), pending);
    const queryControls = [...h.container.querySelectorAll('.adm-binding-server-query button')];
    assert.equal(queryControls.length, 2); assert.ok(queryControls.every(button => button.disabled), 'unknown command keeps both server target changes blocked');
    await h.click(h.button('.adm-role-risk-note button', h.copy('en').retry));
    assert.equal(h.deletes().length, 2);
    assert.deepEqual(h.deletes()[0], h.deletes()[1], 'recovery replays the exact original DELETE protocol');
    assert.equal(platformRolePending(h.client, owner), null);
    assert.ok(h.container.textContent.includes(h.words('en').revokedNotice));
  });
});


test('Optional profile reads are limited to deduplicated visible-page targets and a late page cannot relabel the next one', options, async () => {
  await withWorkspace(async h => {
    h.server.rows = Array.from({ length: 21 }, (_, index) => ({ ...bindings[index === 20 ? 1 : 0], binding_id: '019bb002-0000-7000-8000-' + (index + 100).toString(16).padStart(12, '0'), role_code: index === 20 ? protectedRole.code : role.code }));
    const old = h.holdUser(userIDs[0]); await h.render(); await old.started.promise;
    assert.equal(h.container.querySelectorAll('.adm-role-binding-table tbody tr').length, 20);
    const profileCalls = () => h.calls.filter(call => call.url.includes('/users/'));
    assert.deepEqual(profileCalls().map(call => call.url), ['/api/admin/account/admin/v1/users/' + userIDs[0]], 'no metadata read for unseen users or duplicate bindings');
    await h.click(h.button('.adm-pagination button', h.words('en').nextPage));
    assert.equal(h.container.querySelectorAll('.adm-role-binding-table tbody tr').length, 1);
    assert.ok(h.row(userIDs[1]).textContent.includes(userProfiles[1].user_code));
    assert.equal(h.row(userIDs[1]).querySelectorAll('.adm-binding-danger').length, 0, 'protected role eligibility is unchanged');
    await h.settle(old);
    assert.ok(h.row(userIDs[1]).textContent.includes(userProfiles[1].user_code));
    assert.ok(!h.row(userIDs[1]).textContent.includes(userProfiles[0].user_code));
    assert.equal(profileCalls().length, 2);
    assert.equal(h.deletes().length, 0);
  });
});

// Real Workspace/client and controlled server response. This exercises
// actual handlers, not native default key actions, focus or browser layout.
test('Applied user query, header refresh and local grant filters preserve their distinct request boundaries', options, async () => {
  for (const language of ['zh', 'en', 'ja']) await withWorkspace(async h => {
    // The real producer derives EXPIRED when expires_at <= now. This row
    // ended after its start and long before the controlled review date.
    Object.assign(h.server.rows[1], { expires_at: '2026-10-02T00:00:00Z', status: 'EXPIRED' });
    await h.render(language); const words = h.words(language), filters = words.bindingFilters;
    const form = () => h.container.querySelector('.adm-binding-server-query');
    const target = () => form().querySelector('input');
    const keyword = () => h.container.querySelector('.adm-binding-local-filters input');
    const status = () => h.container.querySelector('.adm-binding-local-filters select');
    const matches = () => h.container.querySelector('.adm-binding-filter-summary').textContent;
    const condition = () => h.container.querySelector('.adm-binding-applied-query code').textContent;
    const queries = () => h.calls.filter(call => call.method === 'GET' && call.url.includes('/admin/v1/role-bindings')).map(call => new URL(call.url, 'http://controlled.invalid').searchParams.get('user_id'));
    const reset = () => h.button('.adm-binding-local-filters button', filters.resetLocal);
    assert.equal(form().querySelector('button[type=submit]').textContent, filters.query);
    assert.equal(condition(), filters.allUsers); assert.equal(matches(), filters.loaded(2, 2));
    const originalQueries = queries();
    await h.edit(keyword(), 'Same display name'); assert.equal(matches(), filters.loaded(0, 2), 'local role filter does not invent user name search');
    await h.click(reset()); await h.edit(status(), 'EXPIRED');
    assert.equal(matches(), filters.loaded(1, 2)); assert.ok(h.row(userIDs[1])); assert.equal(h.row(userIDs[0]), undefined);
    await h.click(reset()); assert.equal(matches(), filters.loaded(2, 2)); assert.deepEqual(queries(), originalQueries);
    await h.edit(target(), ' ' + userIDs[0].toUpperCase() + ' ');
    assert.deepEqual(queries(), originalQueries); assert.equal(condition(), filters.allUsers, 'a typed draft is not yet applied');
    const firstCommit = [];
    await h.submit(form());
    await h.render(language, () => firstCommit.push({ condition: condition(), rows: h.container.querySelectorAll('.adm-role-binding-table tbody tr').length, counts: h.container.querySelectorAll('.adm-binding-filter-summary').length }));
    assert.deepEqual(firstCommit, [{ condition: userIDs[0], rows: 0, counts: 0 }], 'new route condition never renders the old result before passive read cleanup');
    assert.equal(queries().at(-1), userIDs[0]); assert.equal(condition(), userIDs[0]); assert.equal(matches(), filters.loaded(1, 1));
    await h.edit(target(), userIDs[1]); const beforeRefresh = queries().length;
    await h.click(h.button('.adm-role-workspace-head button', h.copy(language).refresh));
    assert.equal(queries().length, beforeRefresh + 1); assert.equal(queries().at(-1), userIDs[0]);
    assert.equal(target().value, userIDs[1]); assert.equal(condition(), userIDs[0]);
    await h.edit(status(), 'ACTIVE');
    await h.click(h.button('.adm-binding-server-query button', filters.clearUser)); await h.render(language);
    assert.equal(queries().at(-1), null); assert.equal(new URLSearchParams(h.route.search).has('role_user_id'), false);
    assert.ok(reset().disabled, 'clearing the applied user also resets the local status');
    assert.equal(condition(), filters.allUsers); assert.equal(matches(), filters.loaded(2, 2));
    await h.edit(status(), 'EXPIRED'); await h.edit(target(), userIDs[0]);
    await h.click(h.button('.adm-binding-server-query button', filters.clearUser));
    assert.ok(!reset().disabled, 'clearing only an unsubmitted draft keeps the local filter');
    assert.equal(matches(), filters.loaded(1, 2));
    const beforeInvalid = queries(); await h.edit(target(), 'not-a-user-id'); await h.submit(form());
    assert.deepEqual(queries(), beforeInvalid); assert.equal(condition(), filters.allUsers);
    assert.ok(h.container.querySelector('[role=alert]')); assert.equal(h.deletes().length, 0);
  });
});

const bindingQueries = calls => calls.filter(call => call.method === 'GET' && call.url.includes('/admin/v1/role-bindings')).map(call => new URL(call.url, 'http://controlled.invalid').searchParams.get('user_id'));
const rawBindingRoute = (...ids) => '?' + new URLSearchParams([['section', 'roles'], ['role_view', 'bindings'], ...ids.map(id => ['role_user_id', id])]);
const appliedQuery = container => container.querySelector('.adm-binding-applied-query code')?.textContent;
const bindingCount = container => container.querySelector('.adm-role-summary article:last-child small')?.textContent;

test('Binding deep links report the condition actually sent and let an invalid link recover without widening it', options, async () => {
  for (const language of ['zh', 'en', 'ja']) {
    await withWorkspace(async h => {
      await h.render(language);
      assert.deepEqual(bindingQueries(h.calls), [null]);
      assert.equal(appliedQuery(h.container), h.words(language).bindingFilters.allUsers, 'a whitespace deep link reads all users and must say so');
      assert.equal(bindingCount(h.container), h.words(language).scopes.PLATFORM);
      assert.ok(h.row(userIDs[0]) && h.row(userIDs[1]));
      await h.click(h.button('.adm-binding-server-query button', h.words(language).bindingFilters.clearUser));
      await h.render(language);
      assert.equal(new URLSearchParams(h.route.search).has('role_user_id'), false);
    }, { initialSearch: rawBindingRoute('  ', userIDs[1]) });

    await withWorkspace(async h => {
      await h.render(language);
      assert.deepEqual(bindingQueries(h.calls), [userIDs[0]], 'only the first URL value is canonicalized and sent');
      assert.equal(appliedQuery(h.container), userIDs[0]);
      assert.equal(bindingCount(h.container), userIDs[0]);
      assert.ok(h.row(userIDs[0])); assert.equal(h.row(userIDs[1]), undefined);
    }, { initialSearch: rawBindingRoute(' ' + userIDs[0].toUpperCase() + ' ', userIDs[1]) });

    await withWorkspace(async h => {
      const bad = 'not-a-user-id'.repeat(24), filters = h.words(language).bindingFilters;
      await h.render(language);
      assert.deepEqual(bindingQueries(h.calls), [], 'an invalid deep link never requests an unrestricted list');
      const warning = h.container.querySelector('.adm-binding-invalid-query');
      assert.ok(warning); assert.equal(warning.getAttribute('role'), 'alert'); assert.equal(warning.textContent, filters.invalidURL);
      assert.equal(appliedQuery(h.container), undefined);
      assert.equal(bindingCount(h.container), filters.notApplied);
      assert.equal(h.container.querySelector('.adm-binding-server-query input').value, bad, 'the user can correct or clear the draft');
      assert.equal(h.container.querySelectorAll('.adm-binding-filter-summary').length, 0);
      await h.click(h.button('.adm-binding-server-query button', filters.clearUser)); await h.render(language);
      assert.deepEqual(bindingQueries(h.calls), [null]); assert.equal(appliedQuery(h.container), filters.allUsers);
      assert.equal(h.container.querySelectorAll('.adm-binding-invalid-query').length, 0);
      assert.equal(h.deletes().length, 0);
    }, { initialSearch: rawBindingRoute('not-a-user-id'.repeat(24), userIDs[0]) });
  }
});

test('A completed list read error is absent on the new user’s first commit while its own failure still appears', options, async () => {
  await withWorkspace(async h => {
    const language = 'en', messages = () => [...h.container.querySelectorAll('[role=alert]')].map(node => node.textContent);
    h.server.failBindings.set(userIDs[0], 503);
    await h.render(language);
    assert.deepEqual(messages(), [h.copy(language).loadFailed]);
    const incoming = h.holdBinding(userIDs[1]), firstCommit = [];
    h.route.navigate('roles', { role_user_id: userIDs[1] });
    await h.render(language, () => firstCommit.push({ condition: appliedQuery(h.container), alerts: messages() }));
    assert.deepEqual(firstCommit, [{ condition: userIDs[1], alerts: [] }], 'the previous user’s failure cannot be announced for the new user');
    await incoming.started.promise;
    assert.equal(h.container.querySelectorAll('.adm-binding-filter-summary').length, 0);
    await h.settle(incoming);
    assert.deepEqual(messages(), []); assert.ok(h.row(userIDs[1]));
    h.server.failBindings.set(userIDs[1], 403);
    const refresh = () => h.button('.adm-role-workspace-head button', h.copy(language).refresh);
    await h.click(refresh());
    assert.deepEqual(messages(), [h.copy(language).denied]); assert.equal(h.row(userIDs[1]), undefined);
    h.server.failBindings.delete(userIDs[1]); await h.click(refresh());
    assert.deepEqual(messages(), []); assert.ok(h.row(userIDs[1]));
    assert.deepEqual(bindingQueries(h.calls), [userIDs[0], userIDs[1], userIDs[1], userIDs[1]]);
    assert.equal(h.deletes().length, 0);
  }, { initialSearch: rawBindingRoute(userIDs[0]) });
});
