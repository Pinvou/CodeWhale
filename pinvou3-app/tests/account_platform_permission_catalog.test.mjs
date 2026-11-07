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
      server, calls, copies, client, render, container, clipboard, row, click, hold,
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
    finally { for (const [name, descriptor] of previous) { if (descriptor) Object.defineProperty(globalThis, name, descriptor); else delete globalThis[name]; } }
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
