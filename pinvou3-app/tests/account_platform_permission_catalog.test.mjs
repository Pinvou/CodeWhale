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
import { readAdminAuthorizationSnapshot, adminAuthorizationSnapshotIsCurrent } from '../src/features/account-platform/admin-authorization-snapshot.js';
import { adminLayoutCopy, adminRolePresentationCopy, platformRoleCopy } from '../src/shared/i18n.js';
import { grantablePlatformRole } from '../src/features/account-platform/platform-role-management.js';

const sourceRoot = new URL('../src/features/account-platform/', import.meta.url);
const nodeRequire = createRequire(import.meta.url);
const permission = 'platform.billing.wallets.read';
const actorIDs = ['019bb001-0000-7000-8000-000000000001', '019bb001-0000-7000-8000-000000000002'];
const personal = ['personal.billing.read', 'personal.session.login', 'personal.profile.read', 'personal.profile.update', 'personal.security.manage'];
const profile = actor => ({ user_id: actorIDs[actor], user_code: actor ? 'USR-1111-1111-1111-1111-DP' : 'USR-0000-0000-0000-0000-Q3', status: 'ACTIVE', username: 'Controlled actor ' + actor, display_name: null, mobile_verified: false, commerce_customer_status: 'NOT_CONFIGURED', personal_permissions: personal, platform_permissions: ['platform.roles.read'], active_context: { context_type: 'PERSONAL', membership_version: 1, context_version: 1, authorization_version: 1, personal_authorization_version: 1, platform_authorization_version: 3, organization_authorization_version: 0, permissions: personal } });
// Representative valid catalog rows use the real DTO and existing seed
// permission strings. This is not a snapshot of a live complete role catalog.
const roleRows = [
  { code: 'PLATFORM_BILLING_OPERATOR', scope: 'PLATFORM', assignable: true, permissions: [permission, 'platform.billing.ledger.read', 'platform.billing.orders.sync', 'platform.billing.repairs.request', 'platform.billing.adjustments.request', 'platform.billing.holds.release', 'platform.billing.payment-controls.resume.request', 'platform.usage.read'] },
  { code: 'PLATFORM_BILLING_APPROVER', scope: 'PLATFORM', assignable: true, permissions: [permission, 'platform.billing.ledger.read', 'platform.billing.repairs.approve', 'platform.billing.adjustments.approve', 'platform.billing.payment-controls.resume.approve', 'platform.usage.read'] },
];
const options = { concurrency: false, timeout: 15000 };
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; };
const replaceOne = (source, before, after) => { assert.equal(source.split(before).length, 2, 'unique source import/input'); return source.replace(before, () => after); };
const hashes = {};
let components;

// Compile actual JSX with the project's existing public Vite/Oxc transform.
// Relocate imports only; omit the single CSS side-effect import because this
// memory DOM has no layout engine. No server, browser, file output or UI mock.
async function loadComponents() {
  if (components) return components;
  let identifierURL;
  const compile = async name => {
    const raw = await readFile(new URL(name, sourceRoot), 'utf8');
    let source = raw;
    hashes[name] = { raw: createHash('sha256').update(raw).digest('hex'), loaded: createHash('sha256').update(source).digest('hex') };
    source = replaceOne(source, "'react'", JSON.stringify(pathToFileURL(nodeRequire.resolve('react')).href));
    if (name === 'AdminIdentifier.jsx') {
      source = replaceOne(source, "import './admin-identifier.css';", '');
      source = replaceOne(source, "'../../shared/i18n.js'", JSON.stringify(new URL('../../shared/i18n.js', sourceRoot).href));
      source = replaceOne(source, "'./useIdentifierFeedback.js'", JSON.stringify(new URL('useIdentifierFeedback.js', sourceRoot).href));
    } else {
      source = replaceOne(source, "'./AdminIdentifier.jsx'", JSON.stringify(identifierURL));
      source = replaceOne(source, "'./platform-role-presentation.js'", JSON.stringify(new URL('platform-role-presentation.js', sourceRoot).href));
    }
    const result = await transformWithOxc(source, name, { jsx: { runtime: 'classic' }, sourcemap: false });
    return 'data:text/javascript;base64,' + Buffer.from(result.code).toString('base64');
  };
  identifierURL = await compile('AdminIdentifier.jsx');
  const catalogURL = await compile('PlatformRoleCatalog.jsx');
  components = { ...(await import(identifierURL)), ...(await import(catalogURL)) };
  return components;
}

async function withCatalog(action) {
  const window = domino.createWindow('<!doctype html><html><body></body></html>');
  // Controlled React input delivery and programmatic focus-target spy only.
  // This DOM has no browser scroll, default keyboard focus or layout engine.
  window.document.oninput = null;
  const focusCalls = [], fixtureDocument = window.document;
  const previousCreate = Object.getOwnPropertyDescriptor(fixtureDocument, 'createElement');
  const createElement = fixtureDocument.createElement.bind(fixtureDocument);
  // Domino's inherited focus is a fixed no-op. Observe actual HTML targets
  // with own methods on real fixture nodes instead of changing a prototype.
  Object.defineProperty(fixtureDocument, 'createElement', { configurable: true, value(...args) {
    const node = createElement(...args);
    if (node instanceof window.HTMLElement) Object.defineProperty(node, 'focus', { configurable: true, value() { focusCalls.push(this); } });
    return node;
  } });
  const replacements = { window, document: window.document, navigator: window.navigator, HTMLElement: window.HTMLElement, Node: window.Node, location: undefined, fetch: globalThis.fetch, IS_REACT_ACT_ENVIRONMENT: true };
  const previous = new Map(Object.keys(replacements).map(name => [name, Object.getOwnPropertyDescriptor(globalThis, name)]));
  const calls = [], copies = [], gates = [], tasks = [];
  const client = new AdminPlatformClient();
  const server = { actor: 0, csrf: 'controlled-A' };
  let root, container, snapshot, roles, nextAuth;
  const track = promise => { tasks.push(promise); void promise.catch(() => {}); return promise; };
  const hold = () => { const gate = deferred(); gate.started = deferred(); gates.push(gate); return gate; };
  try {
    for (const [name, value] of Object.entries(replacements)) Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
    globalThis.fetch = async (url, init = {}) => {
      const method = init.method === undefined ? 'GET' : init.method;
      calls.push({ url, init, method });
      let body, gate;
      if (url === '/api/session') body = { authenticated: true, csrf_token: server.csrf };
      else if (url === '/api/account/v1/account/bootstrap') {
        assert.equal(init.method, 'POST'); assert.equal(init.body, '{}'); assert.equal(init.headers['X-CSRF-Token'], server.csrf);
        body = profile(server.actor); gate = nextAuth; nextAuth = null;
      } else { assert.equal(url, '/api/admin/account/admin/v1/roles'); assert.equal(method, 'GET'); body = { items: roleRows }; }
      const response = new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json', 'X-Platform-Authorization-Version': '3', 'X-Context-Version': '1' } });
      if (gate) { const decode = response.json.bind(response); response.json = async () => { gate.started.resolve(); await gate.promise; return decode(); }; }
      return response;
    };
    const clipboard = callback => Object.defineProperty(window.navigator, 'clipboard', { configurable: true, value: { writeText: value => { copies.push(value); return callback(value); } } });
    clipboard(() => Promise.resolve());
    const { AdminIdentifier, PlatformRoleCatalog } = await loadComponents();
    snapshot = await track(readAdminAuthorizationSnapshot(client));
    roles = await track(client.platformRoles());
    const { createRoot } = await import('react-dom/client');
    container = window.document.createElement('main'); window.document.body.appendChild(container); root = createRoot(container);
    const render = async (language = 'en', plain = false) => {
      const props = { client, me: snapshot.authorization, language, acceptedSessionEpoch: snapshot.sessionEpoch, roles, words: adminRolePresentationCopy[language], authorityCopy: platformRoleCopy[language], canGrant: grantablePlatformRole };
      await act(async () => root.render(plain ? React.createElement(AdminIdentifier, { client, owner: snapshot.authorization.user_id, sessionEpoch: snapshot.sessionEpoch, platformAuthorizationVersion: 3, routeScope: 'default-identifier', value: permission, label: adminRolePresentationCopy[language].permissionCodeLabel, language }) : React.createElement(PlatformRoleCatalog, props)));
    };
    const row = () => [...container.querySelectorAll('.adm-role-permission .adm-identifier code')].find(node => node.textContent === permission)?.parentElement.parentElement;
    const click = async node => { assert.ok(node, 'actual production control exists'); await act(async () => node.dispatchEvent(new window.Event('click', { bubbles: true, cancelable: true }))); };
    const api = {
      server, calls, copies, client, render, container, clipboard, row, click, hold, focusCalls,
      async search(value) { const input = container.querySelector('.adm-role-toolbar input'); const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set; await act(async () => { setter.call(input, value); input.dispatchEvent(new window.Event('input', { bubbles: true })); }); },
      words: language => adminLayoutCopy[language].identifierActions,
      copyButton: () => row()?.querySelector('.adm-identifier-copy'),
      feedback: () => row()?.querySelector('[role=status],[role=alert]')?.textContent || '',
      async chooseRole(language) { await click([...container.querySelectorAll('.adm-role-table button')].find(node => node.textContent === adminRolePresentationCopy[language].names.PLATFORM_BILLING_APPROVER)); },
      async settle(gate, refusal = false) { await act(async () => { if (refusal) gate.reject(new Error('controlled clipboard refusal')); else gate.resolve(); }); },
      readHeld() { nextAuth = hold(); const gate = nextAuth; return { gate, promise: track(readAdminAuthorizationSnapshot(client)) }; },
      async accept(promise) { const next = await promise; assert.ok(adminAuthorizationSnapshotIsCurrent(next, client)); snapshot = next; },
    };
    await action(api);
    assert.ok(calls.every(call => call.method === 'GET' || call.url === '/api/account/v1/account/bootstrap'), 'no role business writes');
  } finally {
    try {
      const release = async () => { for (const gate of gates) gate.resolve(); client.retireSession(); await Promise.allSettled(tasks); root?.unmount(); };
      if (root) await act(release); else await release();
    }
    finally {
      if (previousCreate) Object.defineProperty(fixtureDocument, 'createElement', previousCreate); else delete fixtureDocument.createElement;
      for (const [name, descriptor] of previous) { if (descriptor) Object.defineProperty(globalThis, name, descriptor); else delete globalThis[name]; }
    }
  }
}

test('Actual permission catalog displays full codes and explicitly copies the exact permission in all three languages', options, async context => {
  for (const language of ['zh', 'en', 'ja']) await withCatalog(async h => {
    await h.render(language);
    assert.equal(h.container.querySelector('.adm-role-detail h3').textContent, adminRolePresentationCopy[language].names.PLATFORM_BILLING_OPERATOR);
    assert.deepEqual([...h.container.querySelectorAll('.adm-role-permission code')].map(node => node.textContent).sort(), [...roleRows[0].permissions].sort());
    assert.equal(h.container.querySelectorAll('.adm-role-permission .adm-identifier-toggle').length, 0);
    assert.ok([...h.container.querySelectorAll('.adm-role-permission code')].every(node => node.classList.contains('is-expanded')));
    assert.deepEqual(h.copies, []);
    await h.click(h.copyButton());
    assert.deepEqual(h.copies, [permission], 'the actual copy handler receives the raw permission, not its label or role code');
    assert.equal(h.feedback(), h.words(language).copied);
    await h.render(language, true);
    const toggle = h.container.querySelector('.adm-identifier-toggle');
    assert.ok(toggle); assert.equal(toggle.getAttribute('aria-expanded'), 'false');
    await h.click(toggle); assert.equal(toggle.getAttribute('aria-expanded'), 'true');
    assert.deepEqual(h.copies, [permission], 'default expansion is not a clipboard write');
  });
  context.diagnostic(JSON.stringify({ actual_jsx_sources: hashes, compiler: 'existing Vite/Oxc classic JSX', css: 'omitted, no layout/browser/OS proof' }));
});

test('Actual selected-role changes retire an old permission receipt without releasing the new pending copy', options, async () => {
  for (const refusal of [false, true]) await withCatalog(async h => {
    await h.render(); const old = h.hold(); h.clipboard(() => old.promise); await h.click(h.copyButton()); assert.equal(h.feedback(), h.words('en').pending);
    await h.chooseRole('en'); assert.equal(h.feedback(), '');
    const current = h.hold(); h.clipboard(() => current.promise); await h.click(h.copyButton());
    await h.settle(old, refusal); assert.equal(h.feedback(), h.words('en').pending);
    await h.settle(current); assert.equal(h.feedback(), h.words('en').copied);
    assert.deepEqual(h.copies, [permission, permission]);
  });
});

test('Always-expanded permission failures give truthful manual-copy guidance and recover while default guidance stays unchanged', options, async () => {
  for (const language of ['zh', 'en', 'ja']) await withCatalog(async h => {
    await h.render(language); delete window.navigator.clipboard; await h.click(h.copyButton());
    assert.ok(h.words(language).unavailableExpanded); assert.equal(h.feedback(), h.words(language).unavailableExpanded);
    h.clipboard(() => Promise.reject(new Error('controlled refusal'))); await h.click(h.copyButton());
    assert.ok(h.words(language).failedExpanded); assert.equal(h.feedback(), h.words(language).failedExpanded);
    h.clipboard(() => Promise.resolve()); await h.click(h.copyButton()); assert.equal(h.feedback(), h.words(language).copied);
    await h.render(language, true); delete window.navigator.clipboard;
    await h.click(h.container.querySelector('.adm-identifier-copy'));
    assert.equal(h.container.querySelector('[role=alert]').textContent, h.words(language).unavailable);
    assert.ok(h.container.querySelector('.adm-identifier-toggle'));
  });
});

test('Actual catalog rerenders cannot pair old accepted authorization with a new live session while bootstrap is held', options, async () => {
  await withCatalog(async h => {
    await h.render(); const old = h.hold(); h.clipboard(() => old.promise); await h.click(h.copyButton());
    Object.assign(h.server, { actor: 1, csrf: 'controlled-B' }); const read = h.readHeld(); await read.gate.started.promise;
    assert.equal(h.client.financeSessionEpoch, 2); await h.render('ja'); await h.chooseRole('ja');
    await h.settle(old); assert.equal(h.feedback(), ''); assert.equal(h.copyButton().disabled, true);
    await h.click(h.copyButton()); assert.deepEqual(h.copies, [permission]);
    read.gate.resolve(); await h.accept(read.promise); await h.render('ja');
    assert.equal(h.copyButton().disabled, false); assert.deepEqual(h.copies, [permission]);
    h.clipboard(() => Promise.resolve()); await h.click(h.copyButton());
    assert.equal(h.feedback(), h.words('ja').copied); assert.deepEqual(h.copies, [permission, permission]);
  });
});

// These assertions observe actual component .focus() target selection; they do
// not prove native activeElement, visibility, scroll positions or keyboard.
test('Explicit catalog selection targets the current detail heading and return targets the actual current role in three languages', options, async () => {
  for (const language of ['zh', 'en', 'ja']) await withCatalog(async h => {
    await h.render(language); assert.deepEqual(h.focusCalls, [], 'mount does not steal focus');
    await h.chooseRole(language);
    const heading = h.container.querySelector('.adm-role-detail h3');
    const selected = h.container.querySelector('.adm-role-table button[aria-pressed=true]');
    assert.equal(heading.textContent, adminRolePresentationCopy[language].names.PLATFORM_BILLING_APPROVER);
    assert.equal(selected.getAttribute('aria-controls'), heading.id); assert.equal(heading.tabIndex, -1);
    assert.deepEqual(h.focusCalls, [heading], 'button propagation does not cause duplicate focus');
    await h.click(selected); assert.deepEqual(h.focusCalls, [heading, heading]);
    const back = h.container.querySelector('.adm-role-catalog-back');
    assert.equal(back.textContent, adminRolePresentationCopy[language].returnToRoleList);
    await h.click(back); assert.equal(h.focusCalls.at(-1), selected);
    assert.equal(selected.getAttribute('aria-pressed'), 'true');
    assert.deepEqual([...h.container.querySelectorAll('.adm-role-permission code')].map(node => node.textContent).sort(), [...roleRows[1].permissions].sort());
    assert.deepEqual(h.copies, []); assert.equal(h.calls.filter(call => call.url.includes('/admin/v1/roles')).length, 1, 'browsing does not request the catalog again');
  });
});

test('Search fallback never steals focus, explicit fallback choice survives clearing, and return ignores removed or hidden targets', options, async () => {
  await withCatalog(async h => {
    await h.render(); await h.chooseRole('en'); h.focusCalls.length = 0;
    await h.search('PLATFORM_BILLING_OPERATOR');
    assert.equal(h.container.querySelector('.adm-role-detail h3').textContent, adminRolePresentationCopy.en.names.PLATFORM_BILLING_OPERATOR);
    assert.deepEqual(h.focusCalls, []);
    const fallback = h.container.querySelector('.adm-role-table button');
    await h.click(h.container.querySelector('.adm-role-catalog-back')); assert.equal(h.focusCalls.at(-1), fallback);
    h.focusCalls.length = 0; await h.click(fallback);
    assert.equal(h.focusCalls.at(-1), h.container.querySelector('.adm-role-detail h3'));
    h.focusCalls.length = 0; await h.search('');
    assert.equal(h.container.querySelector('.adm-role-detail h3').textContent, adminRolePresentationCopy.en.names.PLATFORM_BILLING_OPERATOR, 'explicit choice replaces an older hidden selected code');
    assert.deepEqual(h.focusCalls, []);
    await h.render('ja'); assert.deepEqual(h.focusCalls, [], 'locale update does not steal focus');
    await h.search('not-a-catalog-role');
    assert.equal(h.container.querySelectorAll('.adm-role-detail').length, 0);
    assert.equal(h.container.querySelectorAll('.adm-role-catalog-back').length, 0);
    assert.deepEqual(h.focusCalls, []);
    await h.search(''); const current = h.container.querySelector('.adm-role-table button[aria-pressed=true]');
    await h.click(h.container.querySelector('.adm-role-catalog-back')); assert.equal(h.focusCalls.at(-1), current); assert.equal(current.isConnected, true);
    h.focusCalls.length = 0; h.container.hidden = true;
    await h.click(current); await h.click(h.container.querySelector('.adm-role-catalog-back'));
    assert.deepEqual(h.focusCalls, [], 'neither selection nor return focuses controls in a retained hidden host');
    h.container.hidden = false; await h.click(h.container.querySelector('.adm-role-catalog-back')); assert.equal(h.focusCalls.at(-1), current);
    assert.deepEqual(h.copies, []);
  });
});
