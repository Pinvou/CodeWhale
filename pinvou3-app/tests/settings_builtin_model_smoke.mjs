// Real browser/React behavior with controlled public API responses. This is not
// a BFF, PostgreSQL, Keycloak, payment, Safari, or production acceptance test.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const fixture = path.join(appRoot, 'tests/fixtures/settings-builtin-model.jsx').replaceAll('\\', '/');
const chrome = [process.env.CHROME, process.env.EDGE,
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome',
  '/usr/bin/google-chrome-stable', '/snap/bin/chromium',
].find(candidate => candidate && fs.existsSync(candidate));
if (!chrome) throw new Error('Browser required: set CHROME to an installed Chromium/Chrome/Edge executable');

const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-builtin-model-ui-'));
let browser;
let vite;
let origin;
let passed = 0;
const failures = [];

async function runCase(name, scenario, check, language = 'en', legacyCrypto = false) {
  const page = await browser.newPage();
  const pageErrors = [];
  const externalRequests = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  await page.setRequestInterception(true);
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.origin === origin || url.protocol === 'data:') void request.continue();
    else { externalRequests.push(url.origin); void request.abort('blockedbyclient'); }
  });
  if (legacyCrypto) await page.evaluateOnNewDocument(() => {
    // eslint-disable-next-line n/no-unsupported-features/node-builtins -- Puppeteer executes this callback in Chromium, not Node.
    Object.defineProperty(globalThis.Crypto.prototype, 'randomUUID', { value: undefined, configurable: true });
  });
  try {
    await page.goto(`${origin}/__builtin_review__?scenario=${scenario}&language=${language}`, { waitUntil: 'networkidle0' });
    await page.waitForFunction(() => window.__builtinReview &&
      document.querySelector('[data-testid=builtin-model-select]'), { timeout: 15_000 });
    await check(page);
    assert.deepEqual(pageErrors, [], 'React/browser must not raise an uncaught exception');
    assert.deepEqual(externalRequests, [], 'Fixture must not contact an external origin');
    passed += 1;
    console.log(`PASS ${name}`);
  } catch (error) {
    failures.push(name);
    console.error(`FAIL ${name}: ${error.message}`);
  } finally { await page.close(); }
}

try {
  vite = await createServer({ configFile: false, root: path.join(appRoot, 'src'),
    cacheDir: path.join(tempRoot, 'vite-cache'), optimizeDeps: { entries: [] }, plugins: [react(), {
      name: 'builtin-model-review-fixture',
      configureServer(server) {
        server.middlewares.use(async (request, response, next) => {
          if (new URL(request.url, 'http://127.0.0.1').pathname !== '/__builtin_review__') return next();
          try {
            const html = await server.transformIndexHtml(request.url,
              `<html><body><div id="settings-root"></div><script type="module" src="/@fs/${fixture}"></script></body></html>`);
            response.setHeader('Content-Type', 'text/html; charset=utf-8');
            response.end(html);
          } catch (error) { next(error); }
        });
      },
    }], server: { host: '127.0.0.1', port: 0, strictPort: true, fs: { allow: [appRoot] } },
  });
  await vite.listen();
  const address = vite.httpServer.address();
  assert.equal(typeof address, 'object');
  assert.equal(address.address, '127.0.0.1');
  origin = `http://127.0.0.1:${address.port}`;
  browser = await puppeteer.launch({ executablePath: chrome, headless: true,
    userDataDir: path.join(tempRoot, 'browser-profile') });

  for (const language of ['zh', 'en', 'ja']) {
    await runCase('catalog selection and refresh ' + language, '', async page => {
      const labels = await page.evaluate(() => ({ refresh: window.__builtinReview.copy.builtinModelRefresh,
        select: window.__builtinReview.copy.builtinModelSelect }));
      assert.ok(labels.refresh && labels.select);
      assert.equal(await page.$eval('[data-testid=builtin-model-refresh]', node => node.textContent.trim()), labels.refresh);
      assert.equal(await page.$eval('[data-testid=builtin-model-select]', node => node.disabled), true);
      await page.click('[data-testid=builtin-model-refresh]');
      await page.waitForFunction(() => !document.querySelector('[data-testid=builtin-model-select]').disabled);
      await page.select('[data-testid=builtin-model-select]', 'wire-b');
      await page.waitForFunction(() => !document.querySelector('[data-testid=builtin-model-refresh]').disabled);
      assert.equal(await page.$eval('[data-testid=builtin-model-select]', node => node.value), 'wire-b');
      const calls = await page.evaluate(() => window.__builtinReview.calls);
      assert.deepEqual(calls.filter(call => call.method === 'setDefaultModel'), [{ method: 'setDefaultModel', model: 'wire-b' }]);
      assert.equal(calls.filter(call => call.method === 'loadModels').length, 1);
      assert.match(await page.$eval('[data-testid=settings-dialog]', node => node.textContent), /ordinary-wire/);
    }, language);

    await runCase('directory failure cannot replace selection or expose provider details ' + language, '', async page => {
      await page.click('[data-testid=builtin-model-refresh]');
      await page.waitForFunction(() => !document.querySelector('[data-testid=builtin-model-select]').disabled);
      await page.evaluate(() => window.__builtinReview.setOutcome('denied'));
      await page.select('[data-testid=builtin-model-select]', 'wire-b');
      await page.waitForSelector('[data-testid=builtin-model-error]');
      assert.equal(await page.$eval('[data-testid=builtin-model-select]', node => node.value), '');
      assert.equal(await page.$eval('[data-testid=builtin-model-error]', node => node.textContent.trim()),
        await page.evaluate(() => window.__builtinReview.copy.uiAccountPlatform.loginRequired));
      assert.doesNotMatch(await page.$eval('body', node => node.textContent), /PUBLIC_PROVIDER_DETAIL|authentication_required/);
      assert.equal(await page.evaluate(() => window.__builtinReview.calls.filter(call => call.method === 'loadModels').length), 0);
      await page.evaluate(() => window.__builtinReview.setOutcome('success'));
      await page.click('[data-testid=builtin-model-refresh]');
      await page.waitForFunction(() => !document.querySelector('[data-testid=builtin-model-error]'));
    }, language);
  }
  for (const outcome of ['empty', 'malformed']) {
    await runCase(outcome + ' directory retains stored preference and custom models', '', async page => {
      await page.evaluate(outcome => window.__builtinReview.setOutcome(outcome), outcome);
      await page.click('[data-testid=builtin-model-refresh]');
      await page.waitForFunction(() => !document.querySelector('[data-testid=builtin-model-refresh]').disabled);
      assert.equal(await page.$eval('[data-testid=builtin-model-select]', node => node.value), '');
      assert.equal(await page.$eval('[data-testid=builtin-model-select]', node => node.disabled), true);
      assert.match(await page.$eval('[data-testid=settings-dialog]', node => node.textContent), /ordinary-wire/);
      if (outcome === 'malformed') await page.waitForSelector('[data-testid=builtin-model-error]');
    });
  }
  for (const mutation of [false, true]) {
    await runCase('unmount fences late ' + (mutation ? 'selection' : 'catalog'), '', async page => {
      if (mutation) {
        await page.click('[data-testid=builtin-model-refresh]');
        await page.waitForFunction(() => !document.querySelector('[data-testid=builtin-model-select]').disabled);
      }
      await page.evaluate(() => window.__builtinReview.setOutcome('delayed'));
      if (mutation) await page.select('[data-testid=builtin-model-select]', 'wire-b');
      else await page.click('[data-testid=builtin-model-refresh]');
      await page.waitForFunction(() => window.__builtinReview.pending);
      await page.click('[data-testid=fixture-toggle]');
      await page.waitForFunction(() => !document.querySelector('[data-testid=settings-dialog]'));
      await page.click('[data-testid=fixture-toggle]');
      await page.waitForSelector('[data-testid=builtin-model-select]');
      await page.evaluate(() => window.__builtinReview.settle());
      await page.waitForFunction(() => !window.__builtinReview.pending);
      assert.equal(await page.$eval('[data-testid=builtin-model-select]', node => node.value), '');
      assert.equal(await page.$eval('[data-testid=builtin-model-select]', node => node.disabled), true);
      assert.equal(await page.evaluate(() => window.__builtinReview.calls.filter(call => call.method === 'loadModels').length), 0);
    });
  }
  for (const method of ['login', 'loginSms', 'logout', 'scope', 'scope-normalized']) {
    for (const failed of [false, true]) {
      await runCase('identity ownership invalidates old directory ' + method + (failed ? ' failure' : ' success'), '', async page => {
        await page.click('[data-testid=builtin-model-refresh]');
        await page.waitForFunction(() => !document.querySelector('[data-testid=builtin-model-select]').disabled);
        await page.evaluate((method, failed) => { void window.__builtinReview.identityChange(method, failed); }, method, failed);
        await page.waitForFunction(() => document.querySelector('[data-testid=builtin-model-select]').disabled);
        assert.deepEqual(await page.$$eval('[data-testid=builtin-model-select] option', nodes => nodes.map(node => node.value)), ['']);
        if (!failed) {
          await page.waitForFunction(() => window.__builtinReview.identityPending);
          assert.equal(await page.$eval('[data-testid=builtin-model-refresh]', node => node.disabled), true);
          await page.evaluate(() => window.__builtinReview.settleIdentity());
        }
        await page.waitForFunction(() => !document.querySelector('[data-testid=builtin-model-refresh]').disabled);
        assert.equal(await page.$eval('[data-testid=builtin-model-select]', node => node.disabled), true);
        assert.equal(await page.$eval('[data-testid=builtin-model-select]', node => node.value), '');
      });
    }
  }
  for (const mutation of [false, true]) {
    await runCase('scope transition fences previously submitted ' + (mutation ? 'selection' : 'catalog'), '', async page => {
      if (mutation) {
        await page.click('[data-testid=builtin-model-refresh]');
        await page.waitForFunction(() => !document.querySelector('[data-testid=builtin-model-select]').disabled);
      }
      await page.evaluate(() => window.__builtinReview.setOutcome('delayed'));
      if (mutation) await page.select('[data-testid=builtin-model-select]', 'wire-b');
      else await page.click('[data-testid=builtin-model-refresh]');
      await page.waitForFunction(() => window.__builtinReview.pending);
      await page.evaluate(() => { void window.__builtinReview.identityChange('scope', false); });
      await page.waitForFunction(() => window.__builtinReview.identityPending);
      await page.evaluate(() => window.__builtinReview.settleIdentity());
      await page.waitForFunction(() => !document.querySelector('[data-testid=builtin-model-refresh]').disabled);
      await page.evaluate(() => window.__builtinReview.settle());
      await page.waitForFunction(() => !window.__builtinReview.pending);
      assert.equal(await page.$eval('[data-testid=builtin-model-select]', node => node.disabled), true);
      assert.equal(await page.$eval('[data-testid=builtin-model-select]', node => node.value), '');
      assert.equal(await page.evaluate(() => window.__builtinReview.calls.filter(call => call.method === 'loadModels').length), 0);
    });
  }
  assert.equal(failures.length, 0, failures.join(', '));

} finally {
  try {
    const shutdown = await Promise.allSettled([browser?.close(), vite?.close()]);
    const resolved = path.resolve(tempRoot);
    assert.ok(resolved.startsWith(`${path.resolve(os.tmpdir())}${path.sep}`) &&
      path.basename(resolved).startsWith('pinvou-builtin-model-ui-'), 'Cleanup must stay inside its owned temporary directory');
    fs.rmSync(resolved, { recursive: true, force: true, maxRetries: 3, retryDelay: 100 });
    for (const result of shutdown) {
      if (result.status === 'rejected') {
        console.error(`Account UI resource shutdown failed: ${result.reason}`);
        process.exitCode = 1;
      }
    }
  } catch (error) {
    console.error(`Account UI cleanup failed: ${error.message}`);
    process.exitCode = 1;
  }
}
