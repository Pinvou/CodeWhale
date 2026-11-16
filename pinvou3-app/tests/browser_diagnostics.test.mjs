import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { EventEmitter } from 'node:events';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { observeBrowser, createDiagnosticWriter, diagnosticText, diagnosticUrl, snapshotDocument } from './helpers/browser-diagnostics.mjs';

const limits = { bytes: 8192, events: 8, pages: 3, dom: 2048, image: 256, captureMs: 20 };
function setup(t, options = {}) {
  const ledger = fs.mkdtempSync(path.join(os.tmpdir(), 'browser-diagnostics-test-'));
  t.after(() => fs.rmSync(ledger, { recursive: true, force: true }));
  const order = [], emitted = [], captureOptions = [];
  class Page extends EventEmitter {
    url() { return 'http://127.0.0.1:1234/fixture?token=private#fragment'; }
    async evaluate(fn, options) { order.push('snapshot'); captureOptions.push(options);return { url: this.url(), title: 'fixture', readyState: 'complete', elements: 3, html: '<body>controlled DOM Bearer confidential</body>' }; }
    async screenshot() { order.push('screenshot'); return Buffer.from('controlled-image'); }
    async close(...args) { order.push('native-page-close'); return args; }
    async waitForSelector(...args) { this.waitArguments = args;if (this.waitError) throw this.waitError;return 'native-result'; }
  }
  const page = new Page();
  const context = { async newPage() { return page; }, async pages() { return [page]; }, async close() { order.push('native-context-close'); return 'context-result'; } };
  const browser = Object.assign(new EventEmitter(), { async newPage() { return page; }, async pages() { return []; }, async createBrowserContext() { return context; } });
  const diagnostics = observeBrowser(browser, { ledger, limits, emit: line => emitted.push(line), ...options });
  const files = () => {
    const root = path.join(ledger, 'browser-diagnostics');
    if (!fs.existsSync(root)) return [];
    return fs.readdirSync(root).flatMap(directory => fs.readdirSync(path.join(root, directory)).map(name => ({ name, data: fs.readFileSync(path.join(root, directory, name), 'utf8') })));
  };
  return { ledger, page, context, browser, diagnostics, order, files, emitted, captureOptions };
}

test('URL and error text discard credentials, query, fragments and common secret forms', () => {
  assert.equal(diagnosticUrl('https://user:password@example.invalid/a?access_token=private#secret'), 'https://example.invalid/a');
  assert.equal(diagnosticUrl('http://192.168.1.2/a?x=y'), 'http://[private-host]/a');
  assert.equal(diagnosticUrl('data:text/plain,secret'), '[data]');
  assert.equal(diagnosticUrl('https://[fd00::1]/a'), 'https://[private-host]/a');
  const text = diagnosticText('Bearer confidential token=another wss://user:password@example.invalid/ws?ticket=third#fourth');
  for (const value of ['confidential', 'another', 'password', 'third', 'fourth']) assert.ok(!text.includes(value), text);
});

test('shared newPage captures browser errors and last DOM before close without reading headers or handles', async t => {
  const f = setup(t);const page = await f.browser.newPage();
  page.emit('console', { type: () => 'error', text: () => 'module failed token=private', args() { throw new Error('must not evaluate arguments'); } });
  page.emit('pageerror', new Error('controlled JavaScript failure'));
  page.emit('requestfailed', { url: () => 'http://127.0.0.1/missing?secret=private', method: () => 'GET', resourceType: () => 'script', failure: () => ({ errorText: 'net::ERR_CONNECTION_RESET' }), headers() { throw new Error('must not read headers'); } });
  page.emit('response', { status: () => 503, url: () => 'http://127.0.0.1/module?token=private' });
  assert.deepEqual(await page.close('original-argument'), ['original-argument']);
  await f.diagnostics.close();
  assert.ok(f.order.indexOf('snapshot') < f.order.indexOf('native-page-close'));
  const events = f.files().filter(x => x.name === 'events.ndjson').flatMap(x => x.data.trim().split('\n').map(JSON.parse));
  for (const kind of ['console', 'pageerror', 'requestfailed', 'http-error', 'page-state']) assert.ok(events.some(x => x.kind === kind), kind);
  assert.ok(f.files().some(x => x.name.endsWith('.dom.txt') && x.data.includes('controlled DOM')));
  assert.ok(f.files().some(x => x.name.endsWith('.jpg')));
  assert.ok(!f.files().map(x => x.data).join('').includes('private'));
  assert.ok(!f.files().map(x => x.data).join('').includes('confidential'));
});

test('incognito context close captures pages before disposing them and keeps native return value', async t => {
  const f = setup(t);const context = await f.browser.createBrowserContext();await context.newPage();
  assert.equal(await context.close(), 'context-result');await f.diagnostics.close();
  assert.ok(f.order.indexOf('snapshot') < f.order.indexOf('native-context-close'));
});

test('a stalled capture is bounded and never prevents native close', async t => {
  const f = setup(t);await f.browser.newPage();f.page.evaluate = () => new Promise(() => {});
  await f.page.close();await f.diagnostics.close();
  assert.ok(f.order.includes('native-page-close'));
  assert.ok(f.files().some(x => x.name === 'events.ndjson' && x.data.includes('capture-timeout')));
});

test('non-loopback pages retain only event metadata, never DOM or screenshots', async t => {
  const f = setup(t);await f.browser.newPage();f.page.url = () => 'https://example.invalid/account?token=private';
  f.page.evaluate = () => { throw new Error('must not inspect remote page'); };
  await f.diagnostics.close();assert.equal(f.order.length, 0);
  assert.ok(f.files().some(x => x.data.includes('not a loopback fixture')));
  assert.ok(f.files().every(x => !x.name.endsWith('.dom.txt') && !x.name.endsWith('.jpg')));
});

test('event/byte limits keep an omission summary and diagnostics are not lifecycle JSON receipts', t => {
  const f = setup(t);const writer = createDiagnosticWriter(f.ledger, { bytes: 2048, events: 4 });
  for (let n = 0; n < 100; n++) writer.record({ kind: 'console', text: 'x'.repeat(300) });
  writer.finish();const files = fs.readdirSync(writer.directory);
  assert.ok(files.includes('summary.txt'));assert.ok(files.every(name => !name.endsWith('.json')));
  assert.ok(files.reduce((sum, name) => sum + fs.statSync(path.join(writer.directory, name)).size, 0) <= 2048);
  const summary = JSON.parse(fs.readFileSync(path.join(writer.directory, 'summary.txt')));
  assert.ok(summary.omitted > 0);assert.equal(summary.events, 4);
});

test('diagnostic callback failures and unavailable output do not alter native close', async t => {
  const f = setup(t);await f.browser.newPage();
  assert.doesNotThrow(() => f.page.emit('console', { type() { throw new Error('controlled capture race'); } }));
  fs.rmSync(path.join(f.ledger, 'browser-diagnostics'), { recursive: true });
  await f.page.close();await f.diagnostics.close();assert.ok(f.order.includes('native-page-close'));
});

test('no error listener is installed that could swallow a native page crash', async t => {
  const f = setup(t);await f.browser.newPage();
  assert.throws(() => f.page.emit('error', new Error('original crash')), /original crash/);
  await f.diagnostics.close();
});

test('a symbolic ledger is rejected without writing into its target', t => {
  const f = setup(t);const alias = path.join(f.ledger, 'alias');const target = path.join(f.ledger, 'target');fs.mkdirSync(target);
  fs.symlinkSync(target, alias, process.platform === 'win32' ? 'junction' : 'dir');
  assert.equal(createDiagnosticWriter(alias), null);assert.deepEqual(fs.readdirSync(target), []);
});

test('a ledger bounds browser reservations across independent writers', t => {
  const f = setup(t);
  const cap = { ...limits, browsers: 2 };
  assert.ok(createDiagnosticWriter(f.ledger, cap));
  assert.equal(createDiagnosticWriter(f.ledger, cap), null);
  assert.equal(fs.readdirSync(path.join(f.ledger, 'browser-diagnostics')).length, 2);
});

test('a rejected wait preserves its error and captures pending requests without mutating a retriable page', async t => {
  const f = setup(t);const page = await f.browser.newPage();
  const original = new Error('controlled selector timeout token=private');page.waitError = original;
  const request = { url: () => 'http://127.0.0.1/entry.jsx?token=private', resourceType: () => 'script' };
  page.emit('request', request);
  const options = { timeout: 1, custom: 'argument-private' };
  await assert.rejects(page.waitForSelector('#ready', options), error => error === original);
  assert.deepEqual(page.waitArguments, ['#ready', options]);
  assert.equal(f.captureOptions[0].maskForScreenshot, false);
  assert.ok(!f.order.includes('screenshot'));
  const record = JSON.parse(f.emitted[0].split('[browser-fixture-diagnostic] ')[1]);
  assert.equal(record.method, 'waitForSelector');assert.equal(record.snapshot.elements, 3);
  assert.equal(record.pending[0].url, 'http://127.0.0.1/entry.jsx');
  assert.ok(!f.emitted.join('').includes('private'));
  page.waitError = undefined;assert.equal(await page.waitForSelector('#retry'), 'native-result');
  await page.close();await f.diagnostics.close();
  assert.ok(f.files().some(file => file.name.startsWith('failure-') && file.name.endsWith('.dom.txt')));
});

test('failure diagnostics still reach stderr after browser slots or artifact storage are exhausted', async t => {
  const f = setup(t, { limits: { ...limits, browsers: 0 } });const page = await f.browser.newPage();
  page.waitError = new Error('original wait failure');
  for (let n = 0; n < 8; n++) await assert.rejects(page.waitForSelector('#ready'), error => error === page.waitError);
  assert.equal(f.emitted.length, limits.pages);
  assert.ok(f.emitted.every(line => Buffer.byteLength(line) < 15_000));
  assert.ok(f.emitted.every(line => JSON.parse(line.split('[browser-fixture-diagnostic] ')[1]).method === 'waitForSelector'));
  await f.diagnostics.close();
});

test('snapshot failure or an unavailable stderr sink cannot replace a native rejection', async t => {
  const f = setup(t, { emit() { throw new Error('output unavailable'); } });const page = await f.browser.newPage();
  page.waitError = new Error('original wait failure');page.evaluate = () => new Promise(() => {});
  await assert.rejects(page.waitForSelector('#ready'), error => error === page.waitError);
  await f.diagnostics.close();
});

test('a failed later page is captured even after the ordinary page snapshot limit', async t => {
  const f = setup(t);await f.browser.newPage();let late;
  for (let n = 0; n < limits.pages; n++) {
    late = new f.page.constructor();
    f.browser.emit('targetcreated', { page: async () => late });
    await new Promise(resolve => setImmediate(resolve));
  }
  late.waitError = new Error('late page timeout');
  await assert.rejects(late.waitForSelector('#ready'), error => error === late.waitError);
  const value = JSON.parse(f.emitted[0].split('[browser-fixture-diagnostic] ')[1]);
  assert.ok(value.page > limits.pages);assert.equal(value.snapshot.elements, 3);
  assert.ok(f.files().some(file => file.name.startsWith('failure-') && file.name.endsWith('.dom.txt')));
  await f.diagnostics.close();
});

test('failed module evidence survives successful import traffic and the event file limit', async t => {
  const f = setup(t);const page = await f.browser.newPage();
  page.emit('requestfailed', { url: () => 'http://127.0.0.1/entry.jsx?token=private', method: () => 'GET', resourceType: () => 'script', failure: () => ({ errorText: 'net::ERR_NETWORK_CHANGED' }) });
  for (let n = 0; n < 300; n++) {
    const request = { url: () => `http://127.0.0.1/module-${n}.js`, resourceType: () => 'script' };
    page.emit('request', request);page.emit('requestfinished', request);
  }
  page.waitError = new Error('blank page timeout');
  await assert.rejects(page.waitForSelector('#ready'), error => error === page.waitError);
  const value = JSON.parse(f.emitted[0].split('[browser-fixture-diagnostic] ')[1]);
  assert.equal(value.critical.length, 1);assert.equal(value.critical[0].error, 'net::ERR_NETWORK_CHANGED');
  assert.equal(value.critical[0].url, 'http://127.0.0.1/entry.jsx');
  assert.ok(!f.emitted[0].includes('private'));
  assert.ok(!value.recent.some(event => event.kind === 'requestfailed'));
  await f.diagnostics.close();
});

test('a navigation racing with capture cannot persist a remote DOM or screenshot', async t => {
  const f = setup(t);await f.browser.newPage();
  f.page.evaluate = async () => ({ url: 'https://example.invalid/account', html: 'remote-private' });
  await f.page.close();await f.diagnostics.close();
  assert.ok(!f.order.includes('screenshot'));
  assert.ok(f.files().every(file => !file.name.endsWith('.dom.txt') && !file.name.endsWith('.jpg') && !file.data.includes('remote-private')));
});

test('a stalled DOM capture still emits the triggering module error', async t => {
  const f = setup(t);const page = await f.browser.newPage();
  page.emit('pageerror', new Error('controlled failed module token=private'));
  page.evaluate = () => new Promise(() => {});page.waitError = new Error('original wait failure');
  await assert.rejects(page.waitForSelector('#ready'), error => error === page.waitError);
  const value = JSON.parse(f.emitted[0].split('[browser-fixture-diagnostic] ')[1]);
  assert.equal(value.snapshot, 'unavailable');assert.equal(value.critical[0].kind, 'pageerror');
  assert.match(value.critical[0].text, /controlled failed module/);assert.ok(!f.emitted[0].includes('private'));
  await f.diagnostics.close();
});

test('the document-side origin check runs before accessing or masking a remote document', () => {
  const previous = Object.getOwnPropertyDescriptor(globalThis, 'location');
  Object.defineProperty(globalThis, 'location', { configurable: true, value: { hostname: 'example.invalid', protocol: 'https:' } });
  try { assert.equal(snapshotDocument({ maxCharacters: 100, secretPattern: '' }), null); }
  finally {
    if (previous) Object.defineProperty(globalThis, 'location', previous);
    else delete globalThis.location;
  }
});

const chrome = process.env.CHROME || [
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  '/usr/bin/google-chrome', '/usr/bin/chromium', '/usr/bin/chromium-browser',
].find(candidate => fs.existsSync(candidate));

test('real browser evidence survives a failing assertion and preserves success and cleanup', { skip: chrome ? false : 'Chrome unavailable', timeout: 120_000 }, t => {
  const f = setup(t);
  const probe = fileURLToPath(new URL('./helpers/browser-diagnostics-probe.mjs', import.meta.url));
  for (const fail of ['1', '0']) {
    const result = spawnSync(process.execPath, [probe], {
      env: { ...process.env, CHROME: chrome, PINVOU_CI_RESOURCE_LEDGER: f.ledger, DIAGNOSTIC_PROBE_FAIL: fail },
      encoding: 'utf8', timeout: 45_000,
    });
    assert.equal(result.error, undefined);
    assert.equal(result.status, fail === '1' ? 27 : 0, result.stderr);
    if (fail === '1') assert.match(result.stderr, /controlled primary assertion/);
  }
  const files = f.files();
  const events = files.filter(x => x.name === 'events.ndjson').flatMap(x => x.data.trim().split('\n').map(JSON.parse));
  for (const kind of ['console', 'pageerror', 'http-error', 'requestfailed', 'page-state']) assert.ok(events.some(x => x.kind === kind), kind);
  const dom = files.filter(x => x.name.endsWith('.dom.txt')).map(x => x.data).join('');
  assert.match(dom, /controlled fixture DOM/);
  for (const value of ['input-private', 'textarea-private', 'marked-private', 'query-private', 'console-private', 'request-private']) {
    assert.ok(!files.filter(x => !x.name.endsWith('.jpg')).map(x => x.data).join('').includes(value), value);
  }
  assert.ok(files.some(x => x.name.endsWith('.jpg') && x.data.length > 100));
  const receipts = fs.readdirSync(f.ledger).filter(name => name.endsWith('.json')).map(name => JSON.parse(fs.readFileSync(path.join(f.ledger, name))));
  assert.ok(receipts.some(x => x.kind === 'browser'));
  assert.ok(receipts.some(x => x.kind === 'http'));
  assert.ok(receipts.every(x => x.state === 'released'), JSON.stringify(receipts));
});
