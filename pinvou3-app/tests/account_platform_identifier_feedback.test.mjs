import test from 'node:test';
import assert from 'node:assert/strict';
import React, { act } from 'react';
import domino from '@mixmark-io/domino';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { useIdentifierFeedback } from '../src/features/account-platform/useIdentifierFeedback.js';

const FIRST = '019aa001-0000-7000-8000-000000000001';
const SECOND = '019aa001-0000-7000-8000-000000000002';
const options = { concurrency: false, timeout: 10000 };

// The real React hook and layout-effect cleanup run in memory. Only the external
// clipboard completion is controlled; this has no browser, layout or OS clipboard.
async function withFeedback(action, { observed = 3, accepted = 3 } = {}) {
  const window = domino.createWindow('<!doctype html><html><body></body></html>');
  const replacements = { fetch: globalThis.fetch, location: undefined, window, document: window.document, navigator: window.navigator, HTMLElement: window.HTMLElement, Node: window.Node, IS_REACT_ACT_ENVIRONMENT: true };
  const previous = new Map(Object.keys(replacements).map(name => [name, Object.getOwnPropertyDescriptor(globalThis, name)]));
  const tasks = [], gates = [], calls = [];
  const client = new AdminPlatformClient(); client.csrfToken = 'controlled-old-csrf';
  if (observed !== null) client.observeAuthorizationVersions(new Headers({ 'X-Platform-Authorization-Version': String(observed) }));
  const clients = new Set([client]);
  const base = { client, owner: 'controlled-actor:1:0', routeScope: '?section=organizations', value: FIRST, sessionEpoch: 0, platformAuthorizationVersion: accepted };
  let root, latest, current = base;
  const Probe = props => { latest = useIdentifierFeedback(props); return React.createElement('p', { 'data-feedback': latest.status, 'data-allowed': String(latest.canCopy) }, latest.status); };
  try {
    for (const [name, value] of Object.entries(replacements)) Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
    const { createRoot } = await import('react-dom/client');
    const container = window.document.createElement('main'); window.document.body.appendChild(container); root = createRoot(container);
    const api = {
      calls, base,
      handler: () => latest.copy,
      allowed: () => container.querySelector('p').getAttribute('data-allowed'),
      status: () => container.querySelector('p').getAttribute('data-feedback'),
      async render(updates = {}) { current = { ...current, ...updates }; clients.add(current.client); await act(async () => { root.render(React.createElement(Probe, current)); }); },
      clipboard(writeText) { Object.defineProperty(window.navigator, 'clipboard', { configurable: true, value: writeText ? { writeText } : undefined }); },
      hold() {
        let resolve, reject; const promise = new Promise((accept, refuse) => { resolve = accept; reject = refuse; });
        const item = { promise, resolve, reject }; gates.push(item); return item;
      },
      async start(copy = latest.copy) { let promise; await act(async () => { promise = copy(); tasks.push(promise); }); return { promise }; },
      async settle(invocation, gate, failure = false) { await act(async () => { if (failure) gate.reject(new Error('controlled rejection')); else gate.resolve(); await invocation.promise; }); },
    };
    await api.render(); await action(api);
  } finally {
    try {
      if (root) await act(async () => { for (const gate of gates) gate.resolve(); await Promise.allSettled(tasks); root.unmount(); });
    } finally {
      latest = null;
      for (const ownedClient of clients) ownedClient.retireSession?.();
      for (const [name, descriptor] of previous) { if (descriptor) Object.defineProperty(globalThis, name, descriptor); else delete globalThis[name]; }
    }
  }
}

test('Explicit copy keeps the exact value, waits for completion, and recovers after rejection', options, async () => {
  await withFeedback(async h => {
    const first = h.hold(); h.clipboard(value => { h.calls.push(value); return first.promise; });
    assert.deepEqual(h.calls, []); const invocation = await h.start();
    assert.deepEqual(h.calls, [FIRST]); assert.equal(h.status(), 'pending');
    await h.settle(invocation, first, true); assert.equal(h.status(), 'failed');
    const next = h.hold(); h.clipboard(value => { h.calls.push(value); return next.promise; });
    const retry = await h.start(); assert.equal(h.status(), 'pending');
    await h.settle(retry, next); assert.equal(h.status(), 'copied'); assert.deepEqual(h.calls, [FIRST, FIRST]);
  });
});

test('Old clipboard receipts cannot belong to another value, route, actor, client or session', options, async () => {
  for (const updates of [{ value: SECOND }, { routeScope: '?section=organizations&organization_cursor=next' }, { owner: 'controlled-actor:2:0' }, { client: {} }, { owner: 'controlled-actor:1:1' }]) {
    await withFeedback(async h => {
      const old = h.hold(); h.clipboard(value => { h.calls.push(value); return old.promise; });
      const invocation = await h.start(); await h.render(updates); assert.equal(h.status(), '');
      const next = h.hold(); h.clipboard(value => { h.calls.push(value); return next.promise; });
      const current = await h.start(); assert.equal(h.status(), 'pending');
      await h.settle(invocation, old); assert.equal(h.status(), 'pending');
      await h.settle(current, next); assert.equal(h.status(), 'copied');
      assert.deepEqual(h.calls, [FIRST, updates.value || FIRST]);
    });
  }
});

test('Unavailable and throwing clipboard are truthful and never prevent the next explicit copy', options, async () => {
  await withFeedback(async h => {
    h.clipboard(null); await h.start(); assert.equal(h.status(), 'unavailable'); assert.deepEqual(h.calls, []);
    h.clipboard(() => { throw new Error('controlled synchronous refusal'); }); await h.start(); assert.equal(h.status(), 'failed');
    h.clipboard(value => { h.calls.push(value); return Promise.resolve(); }); await h.start(); assert.equal(h.status(), 'copied'); assert.deepEqual(h.calls, [FIRST]);
    await h.render({ value: '' }); await h.start(); assert.equal(h.status(), ''); assert.deepEqual(h.calls, [FIRST]);
  });
});

test('A retained page cannot accept a receipt after the actual navigation changes before its next render', options, async () => {
  await withFeedback(async h => {
    globalThis.location = { pathname: '/admin.html', search: '?section=organizations' };
    await h.render({ routeScope: 'actual organizations' });
    const old = h.hold(); h.clipboard(value => { h.calls.push(value); return old.promise; });
    const invocation = await h.start();
    globalThis.location = { pathname: '/admin.html', search: '?section=users' };
    await h.settle(invocation, old); assert.equal(h.status(), '', 'retired navigation clears its receipt instead of claiming success');
    assert.equal(h.allowed(), 'false'); assert.deepEqual(h.calls, [FIRST]);
    await h.render({ routeScope: 'actual users' }); assert.equal(h.status(), '');
    h.clipboard(value => { h.calls.push(value); return Promise.resolve(); }); await h.start();
    assert.equal(h.status(), 'copied'); assert.deepEqual(h.calls, [FIRST, FIRST]);
  });
});


// No parent render is allowed between the real client authority change and the
// old completion. This is the focus refresh seam; the full workspace is static.
test('Live session retirement rejects old success/refusal and old handlers before parent render; the next owner recovers', options, async () => {
  for (const failure of [false, true]) await withFeedback(async h => {
    const old = h.hold(); h.clipboard(value => { h.calls.push(value); return old.promise; });
    const originalHandler = h.handler(), invocation = await h.start();
    globalThis.fetch = async (url, init) => {
      assert.equal(url, '/api/session'); assert.equal(init.credentials, 'same-origin'); assert.equal(init.cache, 'no-store');
      return new Response(JSON.stringify({ authenticated: true, csrf_token: 'controlled-next-csrf' }), { status: 200, headers: { 'Content-Type': 'application/json' } });
    };
    const session = await h.base.client.session(); assert.equal(session.authenticated, true);
    assert.equal(h.base.client.financeSessionEpoch, 1);
    await h.settle(invocation, old, failure); assert.equal(h.status(), ''); assert.equal(h.allowed(), 'false');
    await h.start(originalHandler); assert.deepEqual(h.calls, [FIRST], 'retired admission is blocked even without a pending status');
    await h.render({ routeScope: 'another value scope while the old accepted session remains' });
    h.clipboard(value => { h.calls.push(value); return Promise.resolve(); }); await h.start(); assert.deepEqual(h.calls, [FIRST]);
    await h.render({ owner: 'controlled-actor:3:1', sessionEpoch: 1 });
    assert.deepEqual(h.calls, [FIRST], 'accepting an owner never automatically writes');
    await h.start(); assert.equal(h.status(), 'copied'); assert.deepEqual(h.calls, [FIRST, FIRST]);
  });
});

test('Live platform authority rejects old success/refusal and old handlers without retiring the session', options, async () => {
  for (const failure of [false, true]) await withFeedback(async h => {
    const old = h.hold(); h.clipboard(value => { h.calls.push(value); return old.promise; });
    const originalHandler = h.handler(), invocation = await h.start();
    h.base.client.observeAuthorizationVersions(new Headers({ 'X-Platform-Authorization-Version': '4' }));
    assert.equal(h.base.client.financeSessionEpoch, 0); assert.equal(h.base.client.authorizationVersions.platform_authorization_version, 4);
    await h.settle(invocation, old, failure); assert.equal(h.status(), ''); assert.equal(h.allowed(), 'false');
    await h.start(originalHandler); assert.deepEqual(h.calls, [FIRST]);
    await h.render({ routeScope: 'new context with the same old accepted platform owner' });
    h.clipboard(value => { h.calls.push(value); return Promise.resolve(); }); await h.start(); assert.deepEqual(h.calls, [FIRST]);
    await h.render({ owner: 'controlled-actor:4:0', platformAuthorizationVersion: 4 });
    assert.deepEqual(h.calls, [FIRST]); await h.start(); assert.equal(h.status(), 'copied'); assert.deepEqual(h.calls, [FIRST, FIRST]);
  });
});

test('First, lower and equal platform baselines and unrelated personal/organization/context advances do not lock copy', options, async () => {
  await withFeedback(async h => {
    h.clipboard(value => { h.calls.push(value); return Promise.resolve(); });
    h.base.client.observeAuthorizationVersions(new Headers({ 'X-Platform-Authorization-Version': '2' }));
    await h.start(); assert.equal(h.status(), 'copied');
    h.base.client.observeAuthorizationVersions(new Headers({ 'X-Platform-Authorization-Version': '3' }));
    await h.start(); assert.equal(h.status(), 'copied');
    h.base.client.observeAuthorizationVersions(new Headers({ 'X-Platform-Authorization-Version': '2', 'X-Context-Version': '7', 'X-Personal-Authorization-Version': '4', 'X-Organization-Authorization-Version': '8' }));
    const generation = h.base.client.financeQueryAuthorizationGeneration;
    h.base.client.observeAuthorizationVersions(new Headers({ 'X-Context-Version': '8', 'X-Personal-Authorization-Version': '5', 'X-Organization-Authorization-Version': '9' }));
    assert.ok(h.base.client.financeQueryAuthorizationGeneration > generation);
    await h.start(); assert.equal(h.status(), 'copied'); assert.deepEqual(h.calls, [FIRST, FIRST, FIRST]);
  }, { observed: null, accepted: 3 });
});

test('Known higher platform authority is not adopted by a new child context; explicit accepted and legacy owners remain usable', options, async () => {
  await withFeedback(async h => {
    h.clipboard(value => { h.calls.push(value); return Promise.resolve(); });
    await h.start(); assert.deepEqual(h.calls, []); assert.equal(h.status(), ''); assert.equal(h.allowed(), 'false');
    await h.render({ value: SECOND }); await h.start(); assert.deepEqual(h.calls, []);
    await h.render({ owner: 'controlled-actor:4:0', platformAuthorizationVersion: 4 });
    await h.start(); assert.equal(h.status(), 'copied'); assert.deepEqual(h.calls, [SECOND]);
    await h.render({ client: {}, owner: 'legacy', sessionEpoch: undefined, platformAuthorizationVersion: undefined });
    await h.start(); assert.equal(h.status(), 'copied'); assert.deepEqual(h.calls, [SECOND, SECOND]);
  }, { observed: 4, accepted: 3 });
});
