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
const fixture = path.join(appRoot, 'tests/fixtures/account-platform-ui.jsx').replaceAll('\\', '/');
const chrome = [process.env.CHROME, process.env.EDGE,
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome',
  '/usr/bin/google-chrome-stable', '/snap/bin/chromium',
].find(candidate => candidate && fs.existsSync(candidate));
if (!chrome) throw new Error('Browser required: set CHROME to an installed Chromium/Chrome/Edge executable');

const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-account-ui-'));
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
    await page.goto(`${origin}/__account_review__?scenario=${scenario}&language=${language}`, { waitUntil: 'networkidle0' });
    await page.waitForFunction(() => window.__accountReview &&
      !document.querySelector('.ap-spinner'), { timeout: 15_000 });
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

async function tokens(page) {
  await page.click('nav button:nth-child(4)');
  await page.waitForSelector('.ap-consumption');
}

async function settle(page, key, outcome) {
  await page.evaluate((key, outcome) => window.__accountReview.settle(key, outcome), key, outcome);
}

async function waitPending(page, key) {
  await page.waitForFunction(key => window.__accountReview.pending.includes(key), { timeout: 15_000 }, key);
}

async function createOrganization(page) {
  await page.type('main form input', 'Public fixture organization');
  await page.click('main form button');
}

try {
  vite = await createServer({ configFile: false, root: path.join(appRoot, 'src'),
    cacheDir: path.join(tempRoot, 'vite-cache'), plugins: [react(), {
      name: 'account-ui-review-fixture',
      configureServer(server) {
        server.middlewares.use(async (request, response, next) => {
          if (new URL(request.url, 'http://127.0.0.1').pathname !== '/__account_review__') return next();
          try {
            const html = await server.transformIndexHtml(request.url,
              `<html><body><div id="account-root"></div><script type="module" src="/@fs/${fixture}"></script></body></html>`);
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

  for (const scenario of ['bootstrap-error', 'session-unavailable']) {
    await runCase(`${scenario} renders recoverable error without business controls`, scenario, async page => {
      assert.equal(await page.$$eval('nav', items => items.length), 0);
      assert.equal(await page.$$eval('main form', items => items.length), 0);
      await page.waitForSelector('[role=alert] button');
      assert.equal(await page.$$eval('a[href="/auth/login"]', items => items.length), 0,
        'service failure must not masquerade as a signed-out session');
    });
  }
  await runCase('organization switch selects its Token and ignores old credential rejection', 'credential-late', async page => {
    await tokens(page);
    await waitPending(page, 'credential');
    const organization = await page.evaluate(() => window.__accountReview.ids.organization);
    await page.select('.ap-header select', organization);
    await page.waitForFunction(() => document.body.textContent.includes('organization-provider'));
    assert.equal(await page.$$eval('.ap-token-list button.selected', items => items.length), 1);
    assert.equal(await page.$$eval('input[type=password]', items => items.length), 1);
    await settle(page, 'credential', 'abort');
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 100)));
    assert.ok(await page.evaluate(() => document.body.textContent.includes('organization-provider')),
      'old aborted request must not clear current credentials');
  });
  await runCase('old paid-order balance cannot overwrite the next context', 'balance-context', async page => {
    await waitPending(page, 'balance');
    const organization = await page.evaluate(() => window.__accountReview.ids.organization);
    await page.select('.ap-header select', organization);
    await page.waitForFunction(() => document.querySelector('.ap-balance strong').textContent === '222');
    await settle(page, 'balance');
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 100)));
    assert.equal(await page.$eval('.ap-header select', element => element.value), organization);
    assert.equal(await page.$eval('.ap-balance strong', element => element.textContent), '222');
  });
  await runCase('same-context paid order refreshes delayed balance and confirms credit', 'balance-paid', async page => {
    await waitPending(page, 'balance');
    await settle(page, 'balance');
    await page.waitForFunction(() => document.querySelector('.ap-balance strong').textContent === '333');
    assert.ok(await page.evaluate(() => document.body.textContent.includes(window.__accountReview.copy.paymentReceived)));
  });
  for (const [scenario, selector, index, value] of [
    ['eligible', '.ap-consumption input[placeholder="model-name"]', 0, 'model-a'],
    ['usage', '.ap-consumption input[placeholder="00000000-0000-0000-0000-000000000000"]', 1,
      '00000000-0000-4000-8000-000000000006'],
  ]) {
    await runCase(`${scenario} keeps the queried input stable until completion`, scenario, async page => {
      await tokens(page);
      await page.type(selector, value);
      await page.evaluate(index => document.querySelectorAll('.ap-consumption .ap-inline button')[index].click(), index);
      await waitPending(page, scenario);
      assert.equal(await page.$eval(selector, element => element.disabled), true);
      await page.focus(selector);
      await page.keyboard.type('-changed');
      assert.equal(await page.$eval(selector, element => element.value), value);
      await settle(page, scenario);
      await page.waitForFunction(selector => !document.querySelector(selector).disabled, {}, selector);
      const expected = scenario === 'eligible' ? 'tokenReady' : 'settlementPendingHelp';
      assert.ok(await page.evaluate(key => document.body.textContent.includes(window.__accountReview.copy[key]), expected));
    });
  }
  for (const scenario of ['action-401', 'context-401', 'poll-401']) {
    await runCase(`${scenario} returns to login and removes business controls`, scenario, async page => {
      if (scenario === 'action-401') await createOrganization(page);
      await page.waitForSelector('a[href="/auth/login"]', { timeout: 15_000 });
      assert.equal(await page.$$eval('nav', items => items.length), 0);
    });
  }
  await runCase('logout failure cannot unlock an outstanding model query', 'eligible', async page => {
    await tokens(page);
    const selector = '.ap-consumption input[placeholder="model-name"]';
    await page.type(selector, 'model-a');
    await page.click('.ap-consumption .ap-inline button');
    await waitPending(page, 'eligible');
    await page.click('.ap-sidebar-foot button');
    await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 100)));
    assert.equal(await page.$eval(selector, element => element.disabled), true,
      'another action must not release the outstanding query input lock');
    await settle(page, 'eligible');
    await page.waitForFunction(selector => !document.querySelector(selector).disabled, {}, selector);
  });
  for (const language of ['zh', 'en', 'ja']) {
    await runCase(`permission denial uses ${language} copy`, 'permission', async page => {
      await createOrganization(page);
      await page.waitForFunction(() => document.body.textContent.includes(window.__accountReview.copy.permissionDenied));
      assert.ok(await page.evaluate(() => !document.body.textContent.includes('UNTRANSLATED_FIXTURE_ERROR')));
      assert.equal(await page.$$eval('nav', items => items.length), 1);
    }, language);
  }
  await runCase('logout failure preserves the signed-in page and reports failure', 'logout', async page => {
    let navigations = 0;
    page.on('framenavigated', frame => { if (frame === page.mainFrame()) navigations += 1; });
    await page.click('.ap-sidebar-foot button');
    await page.waitForSelector('[role=alert]');
    assert.equal(await page.$$eval('nav', items => items.length), 1);
    assert.equal(navigations, 0);
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === '/api/logout').length), 1);
  });
  await runCase('recharge works without randomUUID and renders a real QR', 'legacy-crypto', async page => {
    // eslint-disable-next-line n/no-unsupported-features/node-builtins -- Puppeteer evaluates this expression in Chromium, not Node.
    assert.equal(await page.evaluate(() => typeof globalThis.crypto.randomUUID), 'undefined');
    await page.click('nav button:nth-child(3)');
    await page.waitForFunction(() => !document.querySelector('.ap-card .ap-inline .ap-primary').disabled);
    await page.click('.ap-card .ap-inline .ap-primary');
    await page.waitForSelector('.ap-payment img');
    const keys = await page.evaluate(() => window.__accountReview.calls
      .filter(call => call.path === '/api/token/v1/recharge/orders' && call.method === 'POST')
      .map(call => call.idempotencyKey));
    assert.equal(keys.length, 1);
    assert.ok(keys[0].length >= 16 && keys[0].length <= 128);
    assert.match(await page.$eval('.ap-payment img', image => image.src), /^data:image\/png;base64,/);
  }, 'en', true);

  for (const language of ['zh', 'en', 'ja']) {
    await runCase('desktop login forwards ' + language + ' copy, locks controls and logs out without reload', 'desktop-login', async page => {
      assert.equal(await page.$$eval('a[href="/auth/login"]', items => items.length), 0);
      await page.click('.ap-login .ap-primary');
      await waitPending(page, 'nativeLogin');
      assert.equal(await page.$eval('.ap-login .ap-primary', button => button.disabled), true);
      assert.equal(await page.$eval('.ap-login .ap-close', button => button.disabled), true);
      assert.deepEqual(await page.evaluate(() => window.__accountReview.calls.find(call => call.path === 'nativeLogin').callbackCopy),
        await page.evaluate(() => window.__accountReview.copy.callbackPage));
      await page.evaluate(() => window.__accountReview.settle('nativeLogin'));
      await page.waitForSelector('.ap-sidebar-foot button');
      let navigations = 0;
      page.on('framenavigated', frame => { if (frame === page.mainFrame()) navigations += 1; });
      await page.click('.ap-sidebar-foot button');
      await page.waitForSelector('.ap-login .ap-primary');
      assert.equal(await page.$$eval('nav', items => items.length), 0);
      assert.equal(navigations, 0);
      assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === 'nativeLogout').length), 1);
      await page.click('.ap-login .ap-close');
      assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === 'nativeClose').length), 1);
    }, language);
  }
  await runCase('temporary desktop login failure remains recoverable and never exposes provider details', 'desktop-login-failure', async page => {
    await page.click('.ap-login .ap-primary');
    await page.waitForSelector('[role=alert]');
    assert.equal(await page.$$eval('nav', items => items.length), 0);
    assert.equal(await page.$eval('.ap-login .ap-primary', button => button.disabled), false);
    assert.equal(await page.$eval('[role=alert]', item => item.textContent),
      await page.evaluate(() => window.__accountReview.copy.errors.oidc_token_endpoint_unavailable));
    assert.equal(await page.evaluate(() => document.body.textContent.includes('PRIVATE_PROVIDER_DETAIL')), false);
  });

  console.log(`Account UI: ${passed} PASS, ${failures.length} FAIL, 0 SKIP. Controlled API responses; real React/client/QR; not BFF/PG or Safari E2E.`);
  assert.equal(failures.length, 0, `Failing scenarios: ${failures.join(', ')}`);
} finally {
  try {
    const shutdown = await Promise.allSettled([browser?.close(), vite?.close()]);
    const resolved = path.resolve(tempRoot);
    assert.ok(resolved.startsWith(`${path.resolve(os.tmpdir())}${path.sep}`) &&
      path.basename(resolved).startsWith('pinvou-account-ui-'), 'Cleanup must stay inside its owned temporary directory');
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
