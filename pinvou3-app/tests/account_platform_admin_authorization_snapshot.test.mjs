import test from 'node:test';
import assert from 'node:assert/strict';
import React, { act } from 'react';
import domino from '@mixmark-io/domino';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { OrganizationAdminClient } from '../src/features/account-platform/organization-client.js';
import { readAdminAuthorizationSnapshot, adminAuthorizationView, adminAuthorizationSnapshotIsCurrent } from '../src/features/account-platform/admin-authorization-snapshot.js';
import { useIdentifierFeedback } from '../src/features/account-platform/useIdentifierFeedback.js';

const USER = { A: '019bb001-0000-7000-8000-000000000001', B: '019bb001-0000-7000-8000-000000000002', C: '019bb001-0000-7000-8000-000000000003' };
const ORG = { A: '019aa001-0000-7000-8000-000000000001', B: '019aa001-0000-7000-8000-000000000002', C: '019aa001-0000-7000-8000-000000000003' };
const CODES = { A: 'USR-0000-0000-0000-0000-Q3', B: 'USR-1111-1111-1111-1111-DP', C: 'USR-2222-2222-2222-2222-8X' };
const personal = ['personal.billing.read', 'personal.session.login', 'personal.profile.read', 'personal.profile.update', 'personal.security.manage'];
const profile = actor => ({ user_id: USER[actor], user_code: CODES[actor], status: 'ACTIVE', username: 'Controlled ' + actor, display_name: null, mobile_verified: false, commerce_customer_status: 'NOT_CONFIGURED', personal_permissions: personal, platform_permissions: ['platform.organizations.read'], active_context: { context_type: 'PERSONAL', membership_version: 1, context_version: 1, authorization_version: 1, personal_authorization_version: 1, platform_authorization_version: 3, organization_authorization_version: 0, permissions: personal } });
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; };
const options = { concurrency: false, timeout: 15000 };

// Actual production read/publish/view functions + actual React state and copy
// hook/client. Only external fetch/JSON and clipboard completion are controlled.
// This is not a mounted AdminPlatformView/Portal, browser, OS clipboard or DB.
async function withAcceptedOwner(action) {
  const window = domino.createWindow('<!doctype html><html><body></body></html>');
  const replacements = { window, document: window.document, navigator: window.navigator, HTMLElement: window.HTMLElement, Node: window.Node, location: undefined, fetch: globalThis.fetch, IS_REACT_ACT_ENVIRONMENT: true };
  const previous = new Map(Object.keys(replacements).map(name => [name, Object.getOwnPropertyDescriptor(globalThis, name)]));
  const gates = [], tasks = [], calls = [], copies = [], clients = new Set();
  const server = { actor: 'A', csrf: 'controlled-A', authenticated: true, authStatus: 200, malformed: false };
  let nextAuth, nextSession, root, ownerState, latestCopy;
  const makeClient = () => { const client = new AdminPlatformClient(); clients.add(client); return client; };
  const live = { client: makeClient(), presentation: 'initial', value: ORG.A };
  const track = promise => { tasks.push(promise); void promise.catch(() => {}); return promise; };
  const hold = () => { const gate = deferred(); gate.started = deferred(); gates.push(gate); return gate; };
  const reply = (body, status, gate) => {
    const response = new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', 'X-Platform-Authorization-Version': '3', 'X-Context-Version': '1' } });
    if (gate) { const decode = response.json.bind(response); response.json = async () => { gate.started.resolve(); await gate.promise; return decode(); }; }
    return response;
  };
  function Consumer({ client, view, value }) {
    const me = view.authorization, version = me.platform_authorization_version || me.active_context?.platform_authorization_version || me.authorization_version || 0;
    latestCopy = useIdentifierFeedback({ client, owner: me.user_id + ':' + version + ':' + view.sessionEpoch, routeScope: 'organizations', value, sessionEpoch: view.sessionEpoch, platformAuthorizationVersion: version });
    return React.createElement('button', { 'data-feedback': latestCopy.status, 'data-allowed': String(latestCopy.canCopy), disabled: !latestCopy.canCopy || latestCopy.status === 'pending' }, latestCopy.status);
  }
  function Owner({ client, presentation, value }) {
    const [snapshot, setSnapshot] = React.useState(null);
    const view = adminAuthorizationView(snapshot, client);
    ownerState = { snapshot, view, publish: setSnapshot };
    return view.authorization ? React.createElement(Consumer, { key: presentation, client, view, value }) : React.createElement('p', null, 'no accepted owner');
  }
  try {
    for (const [name, value] of Object.entries(replacements)) Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
    globalThis.fetch = async (url, init = {}) => {
      calls.push({ url, init });
      if (url === '/api/session') { const gate = nextSession; nextSession = null; return reply(server.authenticated ? { authenticated: true, csrf_token: server.csrf } : { authenticated: false }, server.authenticated ? 200 : 401, gate); }
      if (url === '/api/account/v1/account/bootstrap') {
        assert.equal(init.method, 'POST'); assert.equal(init.body, '{}'); assert.equal(init.headers['X-CSRF-Token'], server.csrf);
        const gate = nextAuth; nextAuth = null;
        const body = server.authStatus !== 200 ? { code: 'CONTROLLED_AUTH_FAILURE', message: 'Controlled authorization failure' } : server.malformed ? { ...profile(server.actor), active_context: null } : profile(server.actor);
        return reply(body, server.authStatus, gate);
      }
      const parsed = new URL(url, 'https://controlled.invalid');
      assert.equal(parsed.pathname, '/api/admin/account/admin/v1/organizations');
      return reply({ items: [{ organization_id: ORG[server.actor], name: 'Controlled organization ' + server.actor, status: 'ACTIVE', member_count: 1, created_at: '2026-10-01T00:00:00Z', updated_at: '2026-11-01T00:00:00Z' }], next_cursor: null }, 200);
    };
    Object.defineProperty(window.navigator, 'clipboard', { configurable: true, value: { writeText: value => { copies.push(value); return Promise.resolve(); } } });
    const { createRoot } = await import('react-dom/client');
    const container = window.document.createElement('main'); window.document.body.appendChild(container); root = createRoot(container);
    const render = async updates => { Object.assign(live, updates); await act(async () => { root.render(React.createElement(Owner, live)); }); };
    const publish = snapshot => { if (!adminAuthorizationSnapshotIsCurrent(snapshot, live.client)) return false; ownerState.publish(snapshot.session.authenticated ? snapshot : null); return true; };
    const startRead = async (guard = () => true) => {
      const client = live.client; let promise;
      await act(async () => { promise = track(readAdminAuthorizationSnapshot(client, () => client === live.client && guard()).then(snapshot => { if (snapshot && client === live.client && guard()) publish(snapshot); return snapshot; })); });
      return { promise };
    };
    const finishRead = async (invocation, gate) => { let result; await act(async () => { gate?.resolve(); result = await invocation.promise; }); return result; };
    const api = {
      server, calls, copies, client: () => live.client,
      snapshot: () => ownerState.snapshot, view: () => ownerState.view,
      status: () => container.querySelector('button')?.getAttribute('data-feedback') || '',
      allowed: () => container.querySelector('button')?.getAttribute('data-allowed') || 'none',
      oldHandler: () => latestCopy.copy, render,
      makeClient, hold, holdAuth: () => { nextAuth = hold(); return nextAuth; }, holdSession: () => { nextSession = hold(); return nextSession; },
      startRead, finishRead,
      async acquire() { return finishRead(await startRead()); },
      async acquireRaw() { return track(readAdminAuthorizationSnapshot(live.client)); },
      async publishRaw(snapshot) { let result; await act(async () => { result = publish(snapshot); }); return result; },
      async list() { let page; await act(async () => { page = await track(new OrganizationAdminClient(live.client).list()); live.value = page.items[0].organization_id; live.presentation += '-list'; root.render(React.createElement(Owner, live)); }); return page; },
      clipboard(callback) { Object.defineProperty(window.navigator, 'clipboard', { configurable: true, value: { writeText: value => { copies.push(value); return callback(value); } } }); },
      async copy(handler = latestCopy.copy) { let promise; await act(async () => { promise = track(handler()); }); return { promise }; },
      async settleCopy(invocation, gate, refusal = false) { await act(async () => { if (refusal) gate.reject(new Error('controlled clipboard refusal')); else gate.resolve(); await invocation.promise; }); },
    };
    await render({}); await action(api);
  } finally {
    try { if (root) await act(async () => { for (const gate of gates) gate.resolve(); for (const client of clients) client.retireSession(); await Promise.allSettled(tasks); root.unmount(); }); }
    finally { latestCopy = null; ownerState = null; for (const [name, descriptor] of previous) { if (descriptor) Object.defineProperty(globalThis, name, descriptor); else delete globalThis[name]; } }
  }
}

test('Actual accepted snapshot survives redraw/remount and faster directory while new session authorization is held', options, async () => {
  for (const refusal of [false, true]) await withAcceptedOwner(async h => {
    await h.acquire(); await h.list(); const acceptedA = h.snapshot();
    assert.ok(Object.isFrozen(acceptedA)); assert.equal(acceptedA.authorization.user_id, USER.A); assert.equal(acceptedA.sessionEpoch, 1);
    const old = h.hold(); h.clipboard(() => old.promise); const originalHandler = h.oldHandler(), invocation = await h.copy();
    Object.assign(h.server, { actor: 'B', csrf: 'controlled-B' }); const auth = h.holdAuth(), read = await h.startRead(); await auth.started.promise;
    assert.equal(h.client().financeSessionEpoch, 2); assert.equal(h.snapshot(), acceptedA);
    await h.settleCopy(invocation, old, refusal); assert.equal(h.status(), '');
    await h.copy(originalHandler); assert.deepEqual(h.copies, [ORG.A]);
    await h.render({ presentation: 'language-en-or-local-query' }); await h.list();
    assert.equal(h.view().authorization.user_id, USER.A); assert.equal(h.snapshot(), acceptedA);
    h.clipboard(() => Promise.resolve()); await h.copy();
    assert.deepEqual(h.copies, [ORG.A], 'a new child cannot combine old actor with live session');
    assert.equal(h.allowed(), 'false'); assert.equal(h.view().sessionEpoch, 1);
    await h.finishRead(read, auth); assert.equal(h.view().authorization.user_id, USER.B); assert.equal(h.view().sessionEpoch, 2);
    assert.deepEqual(h.copies, [ORG.A], 'publishing a real new snapshot is not an automatic copy');
    await h.copy(); assert.equal(h.status(), 'copied'); assert.deepEqual(h.copies, [ORG.A, ORG.B]);
  });
});

test('A second retirement while authorization waits and a retirement before publication cannot publish stale lineage', options, async () => {
  await withAcceptedOwner(async h => {
    await h.acquire(); const acceptedA = h.snapshot(); Object.assign(h.server, { actor: 'B', csrf: 'controlled-B' });
    const auth = h.holdAuth(), read = await h.startRead(); await auth.started.promise;
    h.client().retireSession('controlled-C');
    await assert.rejects(h.finishRead(read, auth), { name: 'AbortError' }); assert.equal(h.snapshot(), acceptedA);
    Object.assign(h.server, { actor: 'C', csrf: 'controlled-C' }); await h.acquire();
    const acceptedC = h.snapshot(), raw = await h.acquireRaw();
    h.client().retireSession('controlled-next');
    assert.equal(await h.publishRaw(raw), false); assert.equal(h.snapshot(), acceptedC);
    await h.render({ presentation: 'after-publication-retirement' }); h.clipboard(() => Promise.resolve()); await h.copy(); assert.deepEqual(h.copies, []);
    Object.assign(h.server, { actor: 'A', csrf: 'controlled-next' }); await h.acquire(); await h.list();
    await h.copy(); assert.equal(h.status(), 'copied'); assert.deepEqual(h.copies, [ORG.A]);
  });
});

test('Current request/disposed and client guards prevent late acceptance and allow the next real acquisition', options, async () => {
  await withAcceptedOwner(async h => {
    await h.acquire(); const acceptedA = h.snapshot(); let current = true;
    Object.assign(h.server, { actor: 'B', csrf: 'controlled-B' }); const session = h.holdSession();
    const before = h.calls.filter(call => call.url === '/api/account/v1/account/bootstrap').length;
    const stopped = await h.startRead(() => current); await session.started.promise; current = false;
    assert.equal(await h.finishRead(stopped, session), null); assert.equal(h.snapshot(), acceptedA);
    assert.equal(h.calls.filter(call => call.url === '/api/account/v1/account/bootstrap').length, before, 'cancelled session continuation does not start bootstrap');
    const auth = h.holdAuth(), old = await h.startRead(); await auth.started.promise;
    const nextClient = h.makeClient(); await h.render({ client: nextClient, presentation: 'new-client' }); assert.equal(h.view().authorization, null);
    await h.acquire(); const acceptedB = h.snapshot(); assert.equal(acceptedB.client, nextClient);
    await h.finishRead(old, auth); assert.equal(h.snapshot(), acceptedB);
    await h.list(); h.clipboard(() => Promise.resolve()); await h.copy(); assert.deepEqual(h.copies, [ORG.B]);
  });
});

test('Guest, malformed authorization and transport rejection never publish a mixed owner and subsequent valid acquisition recovers', options, async () => {
  for (const fault of ['guest', 'malformed', 'http']) await withAcceptedOwner(async h => {
    await h.acquire(); const acceptedA = h.snapshot(), oldHandler = h.oldHandler();
    Object.assign(h.server, { actor: 'B', csrf: 'controlled-B', authenticated: fault !== 'guest', malformed: fault === 'malformed', authStatus: fault === 'http' ? 500 : 200 });
    if (fault === 'guest') { await h.acquire(); assert.equal(h.snapshot(), null); }
    else { await assert.rejects(h.acquire(), fault === 'malformed' ? /INVALID_ACCOUNT_ACK/ : error => error.status === 500); assert.equal(h.snapshot(), acceptedA); }
    await h.render({ presentation: 'after-' + fault }); await h.copy(oldHandler); assert.deepEqual(h.copies, []);
    Object.assign(h.server, { authenticated: true, malformed: false, authStatus: 200 }); await h.acquire(); await h.list();
    h.clipboard(() => Promise.resolve()); await h.copy(); assert.equal(h.status(), 'copied'); assert.deepEqual(h.copies, [ORG.B]);
  });
});

test('Default reader clients need no new service epoch field and snapshot view rejects a different client', options, async () => {
  const client = { session: async () => ({ authenticated: true }), authorization: async () => profile('A') };
  const snapshot = await readAdminAuthorizationSnapshot(client);
  assert.equal(snapshot.sessionEpoch, 0); assert.equal(adminAuthorizationView(snapshot, client), snapshot);
  assert.equal(adminAuthorizationView(snapshot, {}).authorization, null);
  assert.equal(adminAuthorizationSnapshotIsCurrent(snapshot, {}), false);
});
