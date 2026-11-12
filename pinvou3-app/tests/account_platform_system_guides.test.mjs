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
import { adminPlatformCopy } from '../src/shared/i18n.js';

const sourceRoot = new URL('../src/features/account-platform/', import.meta.url);
const nodeRequire = createRequire(import.meta.url);
const options = { concurrency: false, timeout: 15000 };
const statusPermission = ['platform.system.status.read'];
const payload = (errors, failedService = false) => ({ checked_at: '2026-10-01T00:00:00Z', services: [{ key: 'portal', status: failedService ? 'UNHEALTHY' : 'HEALTHY', latency_ms: 1, ...(failedService ? { code: 'PROBE_HTTP_ERROR', message: 'HTTP 503' } : {}) }], relays: [], reconciliation: { pending: 0, stopped: 0 }, recent_errors: errors });
const diagnostic = { source: 'portal', code: 'PROBE_HTTP_ERROR', message: 'HTTP 503', updated_at: '2026-10-01T00:00:00Z' };
const unavailable = code => ({ source: 'token-reconciliation', code, message: code === 'RECONCILIATION_STATUS_UNAVAILABLE' ? 'reconciliation status unavailable' : 'reconciliation errors unavailable', updated_at: '2026-10-01T00:00:00Z' });
const deferred = () => { let resolve; const promise = new Promise(yes => { resolve = yes; }); return { promise, resolve }; };
let component, sourceHash;

async function loadComponent() {
  if (component) return component;
  const name = 'AdminSystemStatusWorkspace.jsx';
  const raw = await readFile(new URL(name, sourceRoot), 'utf8'); sourceHash = createHash('sha256').update(raw).digest('hex');
  let source = raw;
  for (const match of [...source.matchAll(/from (["'])([^"']+)\1/g)]) {
    const resolved = match[2].startsWith('.') ? new URL(match[2], sourceRoot).href : pathToFileURL(nodeRequire.resolve(match[2])).href;
    source = source.replace(match[0], 'from ' + JSON.stringify(resolved));
  }
  const output = await transformWithOxc(source, name, { jsx: { runtime: 'classic' }, sourcemap: false });
  component = (await import('data:text/javascript;base64,' + Buffer.from(output.code).toString('base64'))).AdminSystemStatusWorkspace;
  return component;
}

// Actual source component and client, controlled outer Fetch/JSON only. There
// is no browser, CSS/default-action/focus engine, server or executable runbook.
async function withSystem(action) {
  const window = domino.createWindow('<!doctype html><html><body></body></html>');
  const replacements = { window, document: window.document, navigator: window.navigator, HTMLElement: window.HTMLElement, Node: window.Node, fetch: globalThis.fetch, IS_REACT_ACT_ENVIRONMENT: true };
  const previous = new Map(Object.keys(replacements).map(name => [name, Object.getOwnPropertyDescriptor(globalThis, name)]));
  const clients = [new AdminPlatformClient(), new AdminPlatformClient()], calls = [], gates = [], fetchTasks = [];
  const server = { next: payload([]), status: 200, gate: null };
  let root, container, locale = 'en';
  try {
    for (const [name, value] of Object.entries(replacements)) Object.defineProperty(globalThis, name, { configurable: true, writable: true, value });
    globalThis.fetch = (url, init = {}) => {
      const task = (async () => {
        assert.equal(url, '/api/admin/system/status'); assert.equal(init.method, undefined); assert.equal(init.cache, 'no-store'); assert.equal(init.credentials, 'same-origin'); calls.push({ url, method: 'GET' });
        const body = server.next, status = server.status, gate = server.gate; server.gate = null;
        // systemStatus reads text with the actual lossless status decoder.
        const response = new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
        if (gate) { const decode = response.text.bind(response); response.text = async () => { gate.started.resolve(); await gate.promise; return decode(); }; }
        return response;
      })(); fetchTasks.push(task); void task.catch(() => {}); return task;
    };
    const Component = await loadComponent();
    const { createRoot } = await import('react-dom/client'); container = document.createElement('main'); document.body.appendChild(container); root = createRoot(container);
    const render = async (language = locale, permissions = statusPermission, client = clients[0]) => { locale = language; await act(async () => root.render(React.createElement(Component, { client, copy: adminPlatformCopy[language], language, permissions }))); };
    const click = async node => { assert.ok(node); await act(async () => node.dispatchEvent(new window.Event('click', { bubbles: true, cancelable: true }))); };
    const recent = () => [...container.querySelectorAll('.adm-system-sections article')].find(node => node.querySelector('h2')?.textContent === adminPlatformCopy[locale].system.recentErrors);
    await action({ server, clients, calls, container, render, click, recent, copy: language => adminPlatformCopy[language], hold() { const gate = { ...deferred(), started: deferred() }; gates.push(gate); server.gate = gate; return gate; }, async settle(gate) { await act(async () => gate.resolve()); } });
    assert.ok(calls.every(call => call.method === 'GET'), 'guides and disclosure do not perform a write');
  } finally {
    try { const retire = async () => { for (const gate of gates) gate.resolve(); for (const client of clients) client.retireSession(); await Promise.allSettled(fetchTasks); root?.unmount(); }; if (root) await act(retire); else await retire(); }
    finally { for (const [name, descriptor] of previous) descriptor ? Object.defineProperty(globalThis, name, descriptor) : delete globalThis[name]; }
  }
}

test('Actual system diagnostics describe only the bounded snapshot and expose substantial real-source read-only guides in three languages', options, async context => {
  for (const language of ['zh', 'en', 'ja']) await withSystem(async h => {
    await h.render(language); const copy = h.copy(language), text = h.recent().textContent;
    assert.ok(text.includes(copy.system.recentErrorsScope) && text.includes(copy.system.recentErrorsEmpty));
    assert.ok(text.includes(copy.system.reconciliationErrorsScope), 'usage diagnostics remain explicitly outside this check even when probes and counts succeed');
    assert.ok(copy.system.recentErrorsScope.includes('20'), 'server cap, not an invented total or time window');
    assert.equal(h.container.querySelectorAll('.adm-metrics article').length, 3, 'existing status metrics remain');
    const guides = [...h.container.querySelectorAll('.adm-runbook-panel details')]; assert.equal(guides.length, 2);
    for (const [index, file] of ['full-redeployment.md', 'keycloak-admin-access.md'].entries()) {
      assert.ok(guides[index].querySelector('summary').textContent.includes(copy.runbooks.readGuide));
      assert.equal(guides[index].querySelectorAll('ol>li').length, 3);
      assert.ok(guides[index].textContent.includes('account-platform/deploy/runbooks/' + file));
      assert.ok(guides[index].textContent.includes(copy.runbooks.manualLocatorHelp));
      assert.ok(guides[index].textContent.includes(copy.runbooks.noExecution));
    }
    assert.equal(h.container.querySelectorAll('.adm-runbook-panel button,.adm-runbook-panel a,.adm-runbook-panel form').length, 0, 'no execution or invented external manual URL');
    assert.equal(h.calls.length, 1);
  });
  context.diagnostic(JSON.stringify({ actual_jsx_sha256: sourceHash, boundary: 'real component/client/lossless decoder; controlled Fetch, no CSS/default details action/keyboard/focus/browser/OS' }));
});

test('Explicit unavailable diagnostics and real errors remain distinct from successful empty and request failure, with next-call recovery', options, async () => {
  await withSystem(async h => {
    h.server.next = { ...payload([diagnostic, unavailable('RECONCILIATION_STATUS_UNAVAILABLE'), unavailable('RECONCILIATION_ERRORS_UNAVAILABLE')], true), reconciliation: { pending: null, stopped: null } };
    await h.render(); const copy = h.copy('en');
    assert.ok(h.recent().textContent.includes(copy.system.reconciliationErrorsUnavailable));
    assert.ok(h.recent().textContent.includes(copy.system.reconciliationUnavailable));
    assert.ok(h.recent().textContent.includes(copy.system.reconciliationErrorsScope));
    assert.ok(h.recent().textContent.includes(h.copy('en').system.probeHTTPError + ' (503)'));
    assert.ok(!h.recent().textContent.includes(copy.system.recentErrorsEmpty));
    h.server.status = 503; h.server.next = { code: 'ADMIN_AUTH_UNAVAILABLE' };
    await h.click(h.container.querySelector('.adm-system-refresh'));
    assert.equal(h.container.querySelectorAll('.adm-system-sections article').length, 0);
    assert.ok(h.container.querySelector('[role=alert]'));
    assert.ok(!h.container.textContent.includes(copy.system.recentErrorsEmpty));
    h.server.status = 200; h.server.next = payload([]);
    await h.click(h.container.querySelector('[role=alert] button'));
    assert.ok(h.recent().textContent.includes(copy.system.recentErrorsEmpty));
    assert.equal(h.container.querySelectorAll('[role=alert]').length, 0);
    assert.equal(h.calls.length, 3);
  });
});

test('Denied reads issue no request and a late empty snapshot cannot replace the current client diagnostics', options, async () => {
  await withSystem(async h => {
    await h.render('en', []); assert.equal(h.calls.length, 0); assert.ok(h.container.textContent.includes(h.copy('en').permissionDenied));
    const old = h.hold(); await h.render(); await old.started.promise;
    assert.ok(h.container.querySelector('[role=status]')); assert.equal(h.recent(), undefined, 'loading is not a successful empty result');
    h.server.next = payload([diagnostic], true); await h.render('en', statusPermission, h.clients[1]);
    assert.ok(h.recent().textContent.includes(h.copy('en').system.probeHTTPError + ' (503)'));
    await h.settle(old);
    assert.ok(h.recent().textContent.includes(h.copy('en').system.probeHTTPError + ' (503)'));
    assert.ok(!h.recent().textContent.includes(h.copy('en').system.recentErrorsEmpty));
  });
});
