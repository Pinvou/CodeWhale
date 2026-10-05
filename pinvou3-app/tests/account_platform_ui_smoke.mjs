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

async function settle(page, key, outcome) {
  await page.evaluate((key, outcome) => window.__accountReview.settle(key, outcome), key, outcome);
}

async function waitPending(page, key) {
  await page.waitForFunction(key => window.__accountReview.pending.includes(key), { timeout: 15_000 }, key);
}

async function createUnit(page) {
  const organization = await page.evaluate(() => window.__accountReview.ids.organization);
  await page.select('.ap-context-card select', organization);
  await page.waitForFunction(() => !document.querySelector('.ap-context-card select').disabled);
  await page.click('nav button:nth-child(2)');
  await page.type('main form input', 'Public fixture unit');
  await page.click('main form button');
}

async function enterCredentials(page) {
  await page.type('.ap-login input[autocomplete="username"]', 'public-desktop-user');
  await page.type('.ap-login input[type="password"]', 'public-test-password');
}

try {
  vite = await createServer({ configFile: false, root: path.join(appRoot, 'src'),
    cacheDir: path.join(tempRoot, 'vite-cache'), optimizeDeps: { entries: [] }, plugins: [react(), {
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
    await runCase(scenario + ' is recoverable without protected controls', scenario, async page => {
      assert.equal(await page.$$eval('nav', items => items.length), 0);
      await page.waitForSelector('[role=alert] button');
      const retry = await page.evaluate(() => window.__accountReview.copy.retry);
      assert.ok(typeof retry === 'string' && retry.trim());
      assert.equal(await page.$eval('[role=alert] button', el => el.textContent), retry);
      assert.equal(await page.$$eval('a[href="/auth/login"]', items => items.length), 0);
    });
  }
  for (const locale of ['zh', 'en', 'ja']) {
  await runCase('personal center has localized settled usage and no self-service organization creation in ' + locale, 'normal', async page => {
    assert.equal(await page.$$eval('nav button', items => items.length), 5);
    assert.equal(await page.$$eval('main form', items => items.length), 0);
    await page.click('nav button:nth-child(4)');
    await page.waitForSelector('.ap-usage-row');
    assert.equal(await page.$$eval('.ap-usage-row', items => items.length), 1);
    assert.ok(await page.$eval('.ap-usage-row', item => item.textContent.includes('model-a')));
    assert.equal(await page.$eval('.ap-usage-row .ap-status', item => item.textContent),
      await page.evaluate(() => window.__accountReview.copy.usageStatuses.SETTLED));
  }, locale);
  }
  for (const locale of ['zh', 'en', 'ja']) for (const owner of ['USER', 'ORG']) {
    await runCase('existing ' + owner + '-owned Token creation and credential rotation in ' + locale, 'token-management', async page => {
      if (owner === 'ORG') {
        await page.select('.ap-context-card select', await page.evaluate(() => window.__accountReview.ids.organization));
        await page.waitForFunction(() => !document.querySelector('.ap-context-card select').disabled);
      }
      await page.click('nav button:nth-child(5)');
      await page.waitForFunction(() => document.querySelectorAll('main .ap-grid form').length === 2);
      assert.equal(await page.$eval('main h2', el => el.textContent), await page.evaluate(() => window.__accountReview.copy.tokensTitle));
      await page.type('main .ap-grid form input[maxlength="120"]', 'Public fixture owned token');
      await page.click('main .ap-grid form button');
      await page.waitForFunction(() => [...document.querySelectorAll('.ap-token-list button.selected')].some(el => el.textContent.includes('Public fixture owned token')));
      await page.waitForFunction(() => {
        const buttons = [...document.querySelectorAll('.ap-grid .ap-token-list button')];
        const selected = buttons.find(el => el.getAttribute('aria-pressed') === 'true');
        const other = buttons.find(el => el.getAttribute('aria-pressed') === 'false');
        return selected && other && getComputedStyle(selected).backgroundColor !== getComputedStyle(other).backgroundColor
          && getComputedStyle(selected).borderColor !== getComputedStyle(other).borderColor;
      });
      const originalViewport = page.viewport(); await page.setViewport({ ...originalViewport, width: 540 });
      await page.waitForFunction(() => getComputedStyle(document.querySelector('.ap-facts')).gridTemplateColumns.trim().split(/\s+/).length === 1);
      await page.setViewport(originalViewport);
      const created = await page.evaluate(() => window.__accountReview.calls.find(c => c.path === '/api/token/v1/token-accounts' && c.method === 'POST').body);
      assert.equal(created.owner_type, owner); assert.equal(created.owner_id, await page.evaluate(owner => owner === 'USER' ? window.__accountReview.ids.user : window.__accountReview.ids.organization, owner));
      assert.equal(created.owner_organization_id, owner === 'ORG' ? await page.evaluate(() => window.__accountReview.ids.organization) : null);
      await page.type('main .ap-grid form input[type=url]', 'https://public-fixture.invalid');
      await page.type('main .ap-grid form input[type=password]', 'public-fixture-rotation-secret');
      await page.type('main .ap-grid form input[placeholder="model-a, model-b"]', 'model-a');
      await page.click('main .ap-grid article:nth-child(2) form button'); await waitPending(page, 'rotation');
      assert.equal(await page.$eval('main .ap-grid form input[type=password]', el => el.value), '');
      await settle(page, 'rotation'); await page.waitForFunction(() => document.querySelector('main .ap-grid article:nth-child(2)').textContent.includes('v2'));
      assert.ok(await page.evaluate(() => !document.body.textContent.includes('public-fixture-rotation-secret')));
    }, locale);
  }
  await runCase('previous credentials cannot clear the next organization Token', 'credential-late', async page => {
    await page.click('nav button:nth-child(5)'); await waitPending(page, 'credential');
    await page.click('nav button:nth-child(1)');
    await page.select('.ap-context-card select', await page.evaluate(() => window.__accountReview.ids.organization));
    await page.waitForFunction(() => !document.querySelector('.ap-context-card select').disabled);
    await page.click('nav button:nth-child(5)'); await page.waitForFunction(() => document.body.textContent.includes('organization-provider'));
    await settle(page, 'credential', 'abort');
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.ok(await page.evaluate(() => document.body.textContent.includes('organization-provider')));
    assert.equal(await page.$$eval('.ap-token-list button.selected', items => items.length), 1);
  });
  for (const [scenario, selector, index, value] of [
    ['eligible', '.ap-consumption input[placeholder="model-name"]', 0, 'model-a'],
    ['usage', '.ap-consumption input[placeholder="00000000-0000-0000-0000-000000000000"]', 1, '00000000-0000-4000-8000-000000000006'],
  ]) {
    await runCase('restored ' + scenario + ' query is locked and shows its completed result', scenario, async page => {
      await page.click('nav button:nth-child(5)'); await page.type(selector, value);
      await page.evaluate(index => document.querySelectorAll('.ap-consumption .ap-inline button')[index].click(), index);
      await waitPending(page, scenario); assert.equal(await page.$eval(selector, el => el.disabled), true);
      await settle(page, scenario); await page.waitForFunction(selector => !document.querySelector(selector).disabled, {}, selector);
      assert.ok(await page.evaluate(key => document.body.textContent.includes(window.__accountReview.copy[key]), scenario === 'eligible' ? 'tokenReady' : 'settlementPendingHelp'));
    });
    await runCase('restored ' + scenario + ' late completion cannot revive a closed Token panel', scenario, async page => {
      await page.click('nav button:nth-child(5)'); await page.type(selector, value);
      await page.evaluate(index => document.querySelectorAll('.ap-consumption .ap-inline button')[index].click(), index);
      await waitPending(page, scenario); await page.click('nav button:nth-child(4)');
      await settle(page, scenario); await page.waitForFunction(() => !document.querySelector('.ap-sidebar-foot button').disabled);
      assert.equal(await page.$$eval('.ap-consumption', items => items.length), 0);
      assert.equal(await page.$$eval('.ap-usage-row', items => items.length), 1);
    });
  }
  for (const scope of ['account', 'admin']) for (const locale of ['zh', 'en', 'ja']) {
    await runCase('actual ' + scope + ' Portal entry localizes title at mount and language switch in ' + locale, 'portal-title-' + scope, async page => {
      await page.waitForSelector('select[aria-label]');
      const expected = await page.evaluate((scope, locale) => window.__accountReview.titleCopy(scope, locale), scope, locale);
      assert.equal(await page.title(), expected);
      const next = locale === 'ja' ? 'en' : 'ja';
      await page.select('select[aria-label]', next);
      await page.waitForFunction((scope, next) => { const expected = window.__accountReview.titleCopy(scope, next); return typeof expected === 'string' && expected.trim() && document.title === expected; }, {}, scope, next);
      assert.equal(await page.$eval('html', el => el.lang), next === 'ja' ? 'ja-JP' : 'en');
    }, locale);
  }
  await runCase('late paid balance cannot overwrite a switched context', 'balance-context', async page => {
    await waitPending(page, 'balance');
    const organization = await page.evaluate(() => window.__accountReview.ids.organization);
    await page.select('.ap-context-card select', organization);
    await page.waitForFunction(() => document.querySelector('.ap-metrics article:nth-child(2) strong').textContent === '222');
    await settle(page, 'balance');
    // A subsequent DOM task observes completion without depending on a network timing sleep.
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await page.$eval('.ap-context-card select', el => el.value), organization);
    assert.equal(await page.$eval('.ap-metrics article:nth-child(2) strong', el => el.textContent), '222');
  });
  await runCase('paid order refreshes same-context balance', 'balance-paid', async page => {
    await waitPending(page, 'balance'); await settle(page, 'balance');
    await page.waitForFunction(() => document.querySelector('.ap-metrics article:nth-child(2) strong').textContent === '333');
    assert.ok(await page.evaluate(() => document.body.textContent.includes(window.__accountReview.copy.paymentReceived)));
  });
  for (const scenario of ['action-401', 'context-401', 'poll-401']) {
    await runCase(scenario + ' clears protected controls', scenario, async page => {
      if (scenario === 'action-401') await createUnit(page);
      await page.waitForSelector('a[href="/auth/login"]', { timeout: 15_000 });
      assert.equal(await page.$$eval('nav', items => items.length), 0);
    });
  }
  for (const language of ['zh', 'en', 'ja']) {
    await runCase('unit permission denial uses ' + language + ' safe copy', 'permission', async page => {
      await createUnit(page);
      await page.waitForFunction(() => document.body.textContent.includes(window.__accountReview.copy.permissionDenied));
      assert.ok(await page.evaluate(() => !document.body.textContent.includes('UNTRANSLATED_FIXTURE_ERROR')));
      assert.equal(await page.$$eval('nav', items => items.length), 1);
    }, language);
  }
  await runCase('logout error retains account and does not reload', 'logout', async page => {
    let navigations = 0; page.on('framenavigated', f => { if (f === page.mainFrame()) navigations += 1; });
    await page.click('.ap-sidebar-foot button'); await page.waitForSelector('[role=alert]');
    assert.equal(await page.$$eval('nav', items => items.length), 1); assert.equal(navigations, 0);
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(c => c.path === '/api/logout').length), 1);
  });
  await runCase('recharge has an independent key and real QR without randomUUID', 'legacy-crypto', async page => {
    await page.click('nav button:nth-child(3)');
    await page.waitForFunction(() => !document.querySelector('.ap-card .ap-inline .ap-primary').disabled);
    await page.click('.ap-card .ap-inline .ap-primary'); await page.waitForSelector('.ap-payment img');
    const keys = await page.evaluate(() => window.__accountReview.calls.filter(c => c.path === '/api/token/v1/recharge/orders' && c.method === 'POST').map(c => c.idempotencyKey));
    assert.equal(keys.length, 1); assert.ok(keys[0].length >= 16 && keys[0].length <= 128);
    assert.match(await page.$eval('.ap-payment img', img => img.src), /^data:image\/png;base64,/);
  }, 'en', true);
  for (const language of ['zh', 'en', 'ja']) {
    await runCase('native credentials, mutation locks and logout in ' + language, 'desktop-login', async page => {
      await enterCredentials(page); await page.click('.ap-login .ap-primary'); await waitPending(page, 'nativeLogin');
      assert.equal(await page.$eval('.ap-login .ap-primary', el => el.disabled), true);
      assert.equal(await page.$eval('.ap-login .ap-close', el => el.disabled), true);
      assert.equal(await page.$eval('.ap-login input[type=password]', el => el.value), '');
      assert.deepEqual(await page.evaluate(() => window.__accountReview.calls.find(c => c.path === 'nativeLogin').credentials), { account: 'public-desktop-user', password: 'public-test-password' });
      await settle(page, 'nativeLogin'); await page.waitForSelector('.ap-sidebar-foot button');
      let navigations = 0; page.on('framenavigated', f => { if (f === page.mainFrame()) navigations += 1; });
      await page.click('.ap-sidebar-foot button'); await page.waitForSelector('.ap-login .ap-primary');
      assert.equal(await page.$$eval('nav', items => items.length), 0); assert.equal(navigations, 0);
      assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(c => c.path === 'nativeLogout').length), 1);
      await page.click('.ap-login .ap-close');
      assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(c => c.path === 'nativeClose').length), 1);
    }, language);
  }
  await runCase('temporary native error hides provider detail and permits retry', 'desktop-login-failure', async page => {
    await enterCredentials(page); await page.click('.ap-login .ap-primary'); await page.waitForSelector('[role=alert]');
    assert.equal(await page.$$eval('nav', items => items.length), 0);
    assert.equal(await page.$eval('.ap-login input[type=password]', el => el.value), '');
    assert.equal(await page.$eval('[role=alert]', el => el.textContent), await page.evaluate(() => window.__accountReview.copy.errors.oidc_token_endpoint_unavailable));
    assert.equal(await page.evaluate(() => document.body.textContent.includes('PRIVATE_PROVIDER_DETAIL')), false);
    await page.type('.ap-login input[type=password]', 'public-retry-password');
    assert.equal(await page.$eval('.ap-login .ap-primary', el => el.disabled), false);
  });

  const panel = '[data-testid="account-platform-popover"]';
  await runCase('actual sidebar entry restores session and wires credential login and closure', 'sidebar-login', async page => {
    const button = '[data-testid="account-platform-button"]';
    await page.waitForSelector(button);
    assert.equal(await page.$eval(button, element => element.getAttribute('aria-expanded')), 'false');
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === 'nativeSession').length), 1);
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === '/api/account/v1/account/bootstrap').length), 0);
    await page.click(button); await page.waitForSelector(`${panel} input[type=password]`);
    await page.type(`${panel} input[autocomplete=username]`, 'public-sidebar-user');
    await page.type(`${panel} input[type=password]`, 'public-sidebar-password');
    await page.click(`${panel} button[type=submit]`); await waitPending(page, 'nativeLogin');
    assert.equal(await page.$eval(`${panel} input[type=password]`, input => input.value), '');
    await settle(page, 'nativeLogin');
    await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes('Fixture User'), {}, panel);
    await page.$$eval(`${panel} button`, (buttons, title) => buttons.find(element => element.textContent === title).click(),
      await page.evaluate(() => window.__accountReview.copy.title));
    await page.waitForFunction(selector => !document.querySelector(selector), {}, panel);
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === 'openAccountCenter').length), 1);
    assert.equal(await page.$eval(button, element => element.getAttribute('aria-expanded')), 'false');
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === 'sidebarOpen').length), 1);
    assert.deepEqual(await page.evaluate(() => window.__accountReview.calls.find(call => call.path === 'nativeLogin').credentials),
      { account: 'public-sidebar-user', password: 'public-sidebar-password' });
  });
  await runCase('popover balance 401 clears protected content', 'popover-balance-401', async page => {
    await page.waitForSelector(`${panel} input[type=password]`);
    assert.equal(await page.$$eval(`${panel} button`, (buttons, logout) => buttons.filter(button => button.textContent === logout).length,
      await page.evaluate(() => window.__accountReview.copy.logout)), 0);
    assert.equal(await page.evaluate(() => document.body.textContent.includes('Fixture User')), false);
  });
  await runCase('popover ignores an outstanding read after close', 'popover-stale-read', async page => {
    await waitPending(page, 'bootstrap');
    await page.click(`${panel} button[title]`);
    await page.waitForFunction(selector => !document.querySelector(selector), {}, panel);
    await settle(page, 'bootstrap');
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await page.$(panel), null);
    assert.equal(await page.evaluate(() => document.body.textContent.includes('Late account')), false);
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === '/api/token/v1/me/balance').length), 0);
  });
  await runCase('popover locale reads cannot unlock an ongoing credential mutation', 'popover-login-delay', async page => {
    await page.waitForSelector(`${panel} input[type=password]`);
    await page.type(`${panel} input[autocomplete=username]`, 'public-popover-user');
    await page.type(`${panel} input[type=password]`, 'public-popover-password');
    await page.click(`${panel} button[type=submit]`); await waitPending(page, 'nativeLogin');
    await page.evaluate(() => window.__accountReview.changeLanguage('ja'));
    await page.waitForSelector(`${panel} input[type=password]`);
    assert.equal(await page.$eval(`${panel} button[type=submit]`, button => button.disabled), true);
    assert.equal(await page.$eval(`${panel} input[type=password]`, input => input.value), '');
    await page.$eval(`${panel} form`, form => form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })));
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === 'nativeLogin').length), 1);
    await settle(page, 'nativeLogin');
    await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes('Fixture User'), {}, panel);
  });
  await runCase('popover closure fences a late login without retaining its password', 'popover-login-close', async page => {
    await page.waitForSelector(`${panel} input[type=password]`);
    await page.type(`${panel} input[autocomplete=username]`, 'public-popover-user');
    await page.type(`${panel} input[type=password]`, 'public-popover-password');
    await page.click(`${panel} button[type=submit]`); await waitPending(page, 'nativeLogin');
    await page.click(`${panel} button[title]`); await settle(page, 'nativeLogin');
    await page.waitForFunction(selector => !document.querySelector(selector), {}, panel);
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === '/api/account/v1/account/bootstrap').length), 0);
    await page.evaluate(() => window.__accountReview.setOpen(true));
    await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes('Fixture User'), {}, panel);
    // Reopening restores the now valid native session; it does not reuse form credentials.
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === 'nativeLogin').length), 1);
  });
  await runCase('popover logout stays locked across locale reads and clears identity', 'popover-logout-delay', async page => {
    await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes('Fixture User'), {}, panel);
    await page.$$eval(`${panel} button`, (buttons, logout) => buttons.find(button => button.textContent === logout).click(),
      await page.evaluate(() => window.__accountReview.copy.logout));
    await waitPending(page, 'nativeLogout');
    await page.evaluate(() => window.__accountReview.changeLanguage('ja'));
    await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes('Fixture User'), {}, panel);
    assert.equal(await page.$$eval(`${panel} button`, buttons => buttons.at(-1).disabled), true);
    await settle(page, 'nativeLogout');
    await page.waitForSelector(`${panel} input[type=password]`);
    assert.equal(await page.evaluate(() => document.body.textContent.includes('Fixture User')), false);
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(call => call.path === 'nativeLogout').length), 1);
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
