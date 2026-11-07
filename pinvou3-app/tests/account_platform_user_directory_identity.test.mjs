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
import { adminPlatformCopy, adminLayoutCopy } from '../src/shared/i18n.js';

const sourceRoot = new URL('../src/features/account-platform/', import.meta.url);
const nodeRequire = createRequire(import.meta.url);
const options = { concurrency: false, timeout: 15000 };
const alphabet = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
// Representative DTOs use real GenerateUserCode/FormatUserCode checksum and
// cursor encoding shapes, not a full Go serializer or database fixture.
const codeFor = index => {
  const payload = '0'.repeat(14) + alphabet[Math.floor(index / 32)] + alphabet[index % 32], base = 'USR' + payload;
  const digest = createHash('sha256').update(base).digest(), bits = digest[0] << 2 | digest[1] >> 6;
  return ['USR', ...payload.match(/.{4}/g), alphabet[bits >> 5 & 31] + alphabet[bits & 31]].join('-');
};
const users = Array.from({ length: 27 }, (_, index) => ({ user_id: '019bb004-0000-7000-8000-' + (index + 1).toString(16).padStart(12, '0'), user_code: codeFor(index), username: index >= 25 ? undefined : 'Controlled user ' + index, display_name: 'Controlled duplicate', status: 'ACTIVE', created_at: '2026-10-01T00:00:00Z', updated_at: '2026-10-02T00:00:00Z' })).reverse();
const cursorFor = user => Buffer.from(JSON.stringify({ updated_at: user.updated_at, user_id: user.user_id })).toString('base64url');
// Independently model the producer's Unicode White_Space handling; the
// fixture does not import the consumer normalization. It only models these
// controlled query values and cursor boundaries, not all SQL or Go decoding.
const serviceSpaces = new Set([9, 10, 11, 12, 13, 32, 0x85, 0xa0, 0x1680, 0x2000, 0x2001, 0x2002, 0x2003, 0x2004, 0x2005, 0x2006, 0x2007, 0x2008, 0x2009, 0x200a, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000]);
const serviceTrim = value => {
  let start = 0, end = value.length;
  while (start < end && serviceSpaces.has(value.charCodeAt(start))) start++;
  while (end > start && serviceSpaces.has(value.charCodeAt(end - 1))) end--;
  return value.slice(start, end);
};
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; };

const hashes = {}, compiled = new Map();

// Transform actual JSX and relocate imports only. CSS is omitted; domino does
// not prove layout, native dialog focus/keyboard, browser or OS behavior.
async function compile(name) {
  if (compiled.has(name)) return compiled.get(name);
  const raw = await readFile(new URL(name, sourceRoot), 'utf8');
  hashes[name] = createHash('sha256').update(raw).digest('hex');
  let source = raw.replace(/^import ['"][^'"]+\.css['"];?\r?$/gm, '');
  const imports = [...source.matchAll(/^import [^\r\n]+? from (['"])([^'"]+)\1;/gm)];
  for (const match of imports) {
    const specifier = match[2];
    const resolved = specifier.endsWith('.jsx') ? await compile(specifier.replace(/^\.\//, ''))
      : specifier.startsWith('.') ? new URL(specifier, sourceRoot).href : pathToFileURL(nodeRequire.resolve(specifier)).href;
    source = source.replace(match[0], () => match[0].replace(match[1] + specifier + match[1], JSON.stringify(resolved)));
  }
  const output = await transformWithOxc(source, name, { jsx: { runtime: 'classic' }, sourcemap: false });
  const url = 'data:text/javascript;base64,' + Buffer.from(output.code).toString('base64');
  compiled.set(name, url);
  return url;
}

async function withUsers(action, initialSearch = '?section=users') {
  const window = domino.createWindow('<!doctype html><html><body></body></html>');
  // Let React use its input-event path in this controlled DOM. This does not
  // prove browser default editing, keyboard submission or focus behavior.
  window.document.oninput = null;
  const location = { pathname: '/admin.html', search: initialSearch, href: '/admin.html' + initialSearch, hash: '' };
  const replacements = { window, document: window.document, location, navigator: window.navigator, HTMLElement: window.HTMLElement, Node: window.Node, fetch: globalThis.fetch, IS_REACT_ACT_ENVIRONMENT: true };
  const previous = new Map(Object.keys(replacements).map(key => [key, Object.getOwnPropertyDescriptor(globalThis, key)]));
  const clients = [new AdminPlatformClient(), new AdminPlatformClient()], calls = [], copied = [], gates = [], tasks = [];
  const server = { status: 200, gate: null, csrf: 'controlled-A' };
  for (const client of clients) { client.csrfToken = server.csrf; client.observeAuthorizationVersions(new Headers({ 'X-Platform-Authorization-Version': '3' })); }
  let root, container, props = { client: clients[0], language: 'en', currentUserID: users[0].user_id, acceptedSessionEpoch: 0, acceptedPlatformAuthorizationVersion: 3, canManage: true };
  const route = { search: initialSearch, section: 'users', available: ['users', 'roles'], navigate(section, updates) { const next = new URLSearchParams(route.search); next.set('section', section); for (const [key, value] of Object.entries(updates)) value == null ? next.delete(key) : next.set(key, value); this.search = location.search = '?' + next; location.href = location.pathname + location.search; } };
  try {
    for (const [key, value] of Object.entries(replacements)) Object.defineProperty(globalThis, key, { configurable: true, writable: true, value });
    Object.defineProperty(window.navigator, 'clipboard', { configurable: true, value: { writeText: async value => { copied.push(value); } } });
    globalThis.fetch = (url, init = {}) => {
      const task = (async () => {
        assert.equal(init.method, undefined, 'these directory/copy/navigation scenes never write'); calls.push(url);
        if (url === '/api/session') return new Response(JSON.stringify({ authenticated: true, csrf_token: server.csrf }));
        const parsed = new URL(url, 'http://controlled.invalid'); let body, invalidCursor = false;
        if (parsed.pathname === '/api/admin/account/admin/v1/users') {
          assert.equal(parsed.searchParams.get('limit'), '25');
          const q = serviceTrim(parsed.searchParams.get('q') || '').toLowerCase(), cursor = serviceTrim(parsed.searchParams.get('cursor') || '');
          let filtered = users.filter(user => !q || (user.username || '').toLowerCase().includes(q) || user.display_name.toLowerCase().includes(q) || user.user_id === q);
          if (cursor && /[^A-Za-z0-9_\r\n-]/u.test(cursor)) invalidCursor = true;
          else if (cursor) { const at = filtered.findIndex(user => cursorFor(user) === cursor); assert.ok(at >= 0, 'representative cursor comes from a real row boundary'); filtered = filtered.slice(at + 1); }
          body = { items: filtered.slice(0, 25), next_cursor: filtered.length > 25 ? cursorFor(filtered[24]) : null };
        } else if (parsed.pathname.startsWith('/api/admin/account/admin/v1/user-code-lookups/')) {
          const code = decodeURIComponent(parsed.pathname.split('/').at(-1)); body = users.find(user => user.user_code.toLowerCase() === code.toLowerCase()); assert.ok(body);
        } else assert.fail('unexpected boundary ' + url);
        const status = invalidCursor ? 400 : server.status, gate = server.gate; server.gate = null;
        const response = new Response(JSON.stringify(status === 200 ? body : { code: invalidCursor ? 'INVALID_USER_FILTER' : 'ADMIN_UNAVAILABLE' }), { status, headers: { 'Content-Type': 'application/json', 'X-Platform-Authorization-Version': '3' } });
        if (gate) { const decode = response.json.bind(response); response.json = async () => { gate.started.resolve(); await gate.promise; return decode(); }; }
        return response;
      })(); tasks.push(task); void task.catch(() => {}); return task;
    };
    const { AdminUsersWorkspace } = await import(await compile('AdminUsersWorkspace.jsx'));
    const { createRoot } = await import('react-dom/client'); container = document.createElement('main'); document.body.appendChild(container); root = createRoot(container);
    const render = async (updates = {}) => { props = { ...props, ...updates }; await act(async () => root.render(React.createElement(AdminUsersWorkspace, { ...props, route, copy: adminPlatformCopy[props.language] }))); };
    const click = async node => { assert.ok(node, 'actual production control'); await act(async () => node.dispatchEvent(new window.Event('click', { bubbles: true, cancelable: true }))); };
    const summary = () => container.querySelector('.adm-user-result-summary')?.textContent || '';
    const rows = () => [...container.querySelectorAll('.adm-user-list tbody tr')];
    const paging = () => [...container.querySelectorAll('.adm-user-pagination button')];
    const h = { calls, copied, server, clients, container, route, location, render, click, rows, paging, summary, props: () => props,
      async navigate(updates) { route.navigate('users', updates); await render(); },
      async renderAfterClick(node) { await click(node); await render(); },
      clipboard(writeText) { Object.defineProperty(window.navigator, 'clipboard', { configurable: true, value: writeText ? { writeText } : undefined }); },
      hold() { const gate = { ...deferred(), started: deferred() }; gates.push(gate); server.gate = gate; return gate; },
      copyGate() { const gate = deferred(); gates.push(gate); return gate; },
      async settle(gate, reject = false) { await act(async () => reject ? gate.reject(new Error('controlled clipboard refusal')) : gate.resolve()); },
      async change(input, value) { const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set; await act(async () => { setter.call(input, value); input.dispatchEvent(new window.Event('input', { bubbles: true })); input.dispatchEvent(new window.Event('change', { bubbles: true })); }); },
      async submit(form) { await act(async () => form.dispatchEvent(new window.Event('submit', { bubbles: true, cancelable: true }))); },
    };
    await render(); await action(h);
  } finally {
    try { const retire = async () => { for (const gate of gates) gate.resolve(); for (const client of clients) client.retireSession(); await Promise.allSettled(tasks); root?.unmount(); }; if (root) await act(retire); else await retire(); }
    finally { for (const [key, descriptor] of previous) descriptor ? Object.defineProperty(globalThis, key, descriptor) : delete globalThis[key]; }
  }
}

test('Actual user list preserves names and exact explicit per-row codes with honest first-batch information in three languages', options, async context => {
  for (const language of ['zh', 'en', 'ja']) await withUsers(async h => {
    await h.render({ language }); const words = adminLayoutCopy[language].userResults;
    assert.equal(h.rows().length, 25); assert.ok(h.summary().includes(words.returned(25)) && h.summary().includes(words.capacity) && h.summary().includes(words.firstBatch));
    assert.ok(h.summary().includes(words.hasNext)); assert.deepEqual(h.copied, []);
    for (const index of [0, 1]) { const row = h.rows()[index]; assert.ok(row.textContent.includes(users[index].username || users[index].display_name)); assert.equal(row.querySelector('.adm-identifier-value code').textContent, users[index].user_code); await h.click(row.querySelector('.adm-identifier-copy')); assert.equal(h.copied.at(-1), users[index].user_code); }
    assert.equal(h.rows()[0].querySelector('[data-user-status-target]').disabled, true, 'self action remains blocked');
    await h.render({ canManage: false }); assert.equal(h.container.querySelectorAll('[data-user-status-target]').length, 0);
    assert.ok(h.calls.every(url => url.includes('/users?')), 'no auxiliary directory/profile reads');
  }); context.diagnostic(JSON.stringify({ actual_jsx_sha256: hashes, boundary: 'actual Workspace/Identifier/PortalDialog/client in controlled ReactDOM/domino, no CSS/default-keyboard/focus/browser/OS/Root/Go serializer proof' }));
});

test('Next and known previous keep real opaque cursors while external deep links expose no invented page ordinal or prior cursor', options, async () => {
  await withUsers(async h => {
    assert.equal(h.paging()[0].disabled, true); await h.renderAfterClick(h.paging()[1]);
    assert.equal(new URLSearchParams(h.route.search).get('user_cursor'), cursorFor(users[24])); assert.equal(h.rows().length, 2);
    assert.ok(h.summary().includes(adminLayoutCopy.en.userResults.continuation)); assert.equal(h.paging()[0].disabled, false);
    await h.renderAfterClick(h.paging()[0]); assert.equal(h.rows().length, 25); assert.equal(new URLSearchParams(h.route.search).get('user_cursor'), null);
    await h.renderAfterClick(h.paging()[1]); assert.equal(h.paging()[0].disabled, false, 'a real local predecessor existed before the external query');
    await h.navigate({ user_q: 'Controlled', user_cursor: cursorFor(users[24]) });
    assert.equal(h.paging()[0].disabled, true, 'external position has no known predecessor'); assert.ok(h.summary().includes(adminLayoutCopy.en.userResults.condition('Controlled')));
    await h.renderAfterClick(h.paging().find(button => button.textContent === adminLayoutCopy.en.userResults.backToFirst));
    assert.equal(new URLSearchParams(h.route.search).get('user_q'), 'Controlled'); assert.equal(new URLSearchParams(h.route.search).get('user_cursor'), null); assert.equal(h.rows().length, 25);
  });
  await withUsers(async h => {
    await h.renderAfterClick(h.paging()[1]);
    assert.equal(new URLSearchParams(h.route.search).get('user_q'), 'Controlled', 'local navigation keeps the applied normalized query');
    assert.equal(h.paging()[0].disabled, false, 'query trimming must not discard a known local predecessor');
    await h.renderAfterClick(h.paging()[0]); assert.equal(h.rows().length, 25);
  }, '?section=users&user_q=%20Controlled%20');
  await withUsers(async h => {
    const words = adminLayoutCopy.en.userResults;
    const back = () => h.paging().find(button => button.textContent === words.backToFirst);
    assert.equal(h.rows().length, 25);
    assert.ok(h.summary().includes(words.firstBatch), 'a whitespace-only cursor is an accepted first batch');
    assert.ok(!h.summary().includes(words.continuation)); assert.equal(back(), undefined);
    await h.navigate({ user_q: '\u0085', user_cursor: '\u0085\u00a0' });
    assert.equal(h.rows().length, 25); assert.ok(h.summary().includes(words.noCondition) && h.summary().includes(words.firstBatch));
    assert.equal(back(), undefined);
    await h.renderAfterClick(h.paging()[1]); assert.equal(h.rows().length, 2);
    assert.equal(h.paging()[0].disabled, false, 'a known first batch remains available after a whitespace deep link');
    await h.renderAfterClick(h.paging()[0]);
    assert.equal(h.rows().length, 25); assert.equal(new URLSearchParams(h.route.search).get('user_cursor'), null);
    await h.navigate({ user_q: '\ufeffControlled' });
    assert.equal(h.rows().length, 0);
    assert.ok([...h.container.querySelectorAll('.adm-user-result-summary > span')].some(node => node.textContent === words.condition('\ufeffControlled')), 'BOM is part of the actual read condition');
    const padded = '\u0085' + cursorFor(users[24]) + '\u00a0';
    await h.navigate({ user_q: '\u0085Controlled\u00a0', user_cursor: padded });
    assert.equal(new URL(h.calls.at(-1), 'http://controlled.invalid').searchParams.get('cursor'), padded, 'the actual client passes the opaque nonblank token to its producer');
    assert.equal(h.rows().length, 2); assert.ok(h.summary().includes(words.continuation) && h.summary().includes(words.condition('Controlled')));
    assert.ok(back()); await h.renderAfterClick(back());
    assert.equal(new URLSearchParams(h.route.search).get('user_q'), 'Controlled'); assert.equal(h.rows().length, 25);
  }, '?section=users&user_cursor=%20');
  await withUsers(async h => {
    assert.ok(h.container.querySelector('[role=alert]'), 'BOM is not a blank cursor under the producer contract');
    assert.equal(h.summary(), ''); assert.equal(h.rows().length, 0);
    await h.navigate({ user_cursor: null });
    assert.equal(h.rows().length, 25); assert.ok(h.summary().includes(adminLayoutCopy.en.userResults.firstBatch));
  }, '?section=users&user_cursor=%EF%BB%BF');
});

test('Retained old rows are labelled with their accepted condition and cannot acquire copy access from a new pending or failed URL query', options, async () => {
  await withUsers(async h => {
    h.server.status = 503; const gate = h.hold(); await h.navigate({ user_q: 'unmatched' }); await gate.started.promise;
    assert.equal(h.rows().length, 25); assert.ok(h.summary().includes(adminLayoutCopy.en.userResults.noCondition)); assert.ok(!h.summary().includes(adminLayoutCopy.en.userResults.condition('unmatched')));
    assert.equal(h.container.querySelectorAll('.adm-identifier-copy').length, 0); await h.settle(gate);
    assert.ok(h.container.querySelector('[role=alert]')); assert.ok(h.summary().includes(adminLayoutCopy.en.userResults.retained));
    h.server.status = 200; await h.click(h.container.querySelector('[role=alert] button'));
    assert.equal(h.rows().length, 0); assert.ok(h.summary().includes(adminLayoutCopy.en.userResults.condition('unmatched'))); assert.ok(!h.summary().includes(adminLayoutCopy.en.userResults.retained));
  });
});

test('Clipboard refusal or unavailability recovers and a late old-page receipt never becomes the current page success', options, async () => {
  await withUsers(async h => {
    h.clipboard(null); await h.click(h.rows()[0].querySelector('.adm-identifier-copy')); assert.ok(h.rows()[0].querySelector('[role=alert]'));
    h.clipboard(() => { throw new Error('controlled refusal'); }); await h.click(h.rows()[0].querySelector('.adm-identifier-copy')); assert.ok(h.rows()[0].querySelector('[role=alert]'));
    const gate = h.copyGate(); h.clipboard(value => { h.copied.push(value); return gate.promise; }); await h.click(h.rows()[0].querySelector('.adm-identifier-copy'));
    await h.navigate({ user_cursor: cursorFor(users[24]) }); await h.settle(gate);
    assert.equal(h.container.querySelectorAll('.adm-identifier-feedback').length, 0); h.clipboard(async value => { h.copied.push(value); });
    await h.click(h.rows()[0].querySelector('.adm-identifier-copy')); assert.equal(h.copied.at(-1), users[25].user_code); assert.ok(h.rows()[0].querySelector('[role=status]'));
  });
});

test('Live session retirement and accepted actor/version/client changes cannot bless retained result-copy context; a fresh accepted read restores it', options, async () => {
  await withUsers(async h => {
    const gate = h.copyGate(); h.clipboard(value => { h.copied.push(value); return gate.promise; }); await h.click(h.rows()[0].querySelector('.adm-identifier-copy'));
    h.server.csrf = 'controlled-B'; await h.clients[0].session(); await h.settle(gate); await h.render({ language: 'ja' });
    assert.equal(h.rows()[0].querySelector('.adm-identifier-copy').disabled, true); assert.equal(h.container.querySelectorAll('.adm-identifier-feedback').length, 0);
    await h.render({ acceptedSessionEpoch: h.clients[0].financeSessionEpoch }); assert.equal(h.container.querySelectorAll('.adm-identifier-copy').length, 0);
    await h.click(h.container.querySelector('[data-user-results-refresh]')); h.clipboard(async value => { h.copied.push(value); }); await h.click(h.rows()[0].querySelector('.adm-identifier-copy')); assert.equal(h.copied.at(-1), users[0].user_code);
    await h.render({ currentUserID: users[1].user_id }); assert.equal(h.container.querySelectorAll('.adm-identifier-copy').length, 0);
    await h.click(h.container.querySelector('[data-user-results-refresh]')); assert.ok(h.rows()[0].querySelector('.adm-identifier-copy'));
    await h.render({ acceptedPlatformAuthorizationVersion: 4 }); assert.equal(h.container.querySelectorAll('.adm-identifier-copy').length, 0);
    await h.click(h.container.querySelector('[data-user-results-refresh]')); assert.ok(h.rows()[0].querySelector('.adm-identifier-copy'));
    await h.render({ client: h.clients[1], acceptedSessionEpoch: 0 }); assert.ok(h.rows()[0].querySelector('.adm-identifier-copy')); assert.equal(h.container.querySelectorAll('.adm-identifier-feedback').length, 0);
  });
});

test('Actual precise-code form uses the dedicated existing endpoint and its results never pretend to have directory pagination', options, async () => {
  await withUsers(async h => {
    const form = h.container.querySelector('[data-user-code-search]'), code = users[26].user_code;
    await h.change(form.querySelector('input'), code); await h.submit(form);
    assert.ok(h.calls.some(url => url.endsWith('/user-code-lookups/' + code))); assert.equal(h.rows().length, 1);
    assert.ok(h.summary().includes(adminLayoutCopy.en.userResults.exact) && h.summary().includes(adminLayoutCopy.en.userResults.exactInfo));
    assert.ok(!h.summary().includes(adminLayoutCopy.en.userResults.capacity)); assert.ok(h.paging().every(button => button.disabled));
    await h.click(h.rows()[0].querySelector('.adm-identifier-copy')); assert.equal(h.copied.at(-1), code);
  });
});
