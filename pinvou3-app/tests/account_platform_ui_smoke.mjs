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
    console.error(`FAIL ${name}: ${error.message}; browser=${JSON.stringify(pageErrors)}`);
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
    assert.equal(await page.$$eval('nav button', items => items.length), 10);
    assert.equal(await page.$$eval('main form', items => items.length), 0);
    await page.evaluate(() => Array.from(document.querySelectorAll('nav button')).find(el => el.textContent === window.__accountReview.copy.sections.usage).click());
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
      await page.evaluate(() => Array.from(document.querySelectorAll('nav button')).find(el => el.textContent === window.__accountReview.copy.sections.tokens).click());
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
    await page.evaluate(() => Array.from(document.querySelectorAll('nav button')).find(el => el.textContent === window.__accountReview.copy.sections.tokens).click()); await waitPending(page, 'credential');
    await page.click('nav button:nth-child(1)');
    await page.select('.ap-context-card select', await page.evaluate(() => window.__accountReview.ids.organization));
    await page.waitForFunction(() => !document.querySelector('.ap-context-card select').disabled);
    await page.evaluate(() => Array.from(document.querySelectorAll('nav button')).find(el => el.textContent === window.__accountReview.copy.sections.tokens).click()); await page.waitForFunction(() => document.body.textContent.includes('organization-provider'));
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
      await page.evaluate(() => Array.from(document.querySelectorAll('nav button')).find(el => el.textContent === window.__accountReview.copy.sections.tokens).click()); await page.type(selector, value);
      await page.evaluate(index => document.querySelectorAll('.ap-consumption .ap-inline button')[index].click(), index);
      await waitPending(page, scenario); assert.equal(await page.$eval(selector, el => el.disabled), true);
      await settle(page, scenario); await page.waitForFunction(selector => !document.querySelector(selector).disabled, {}, selector);
      assert.ok(await page.evaluate(key => document.body.textContent.includes(window.__accountReview.copy[key]), scenario === 'eligible' ? 'tokenReady' : 'settlementPendingHelp'));
    });
    await runCase('restored ' + scenario + ' late completion cannot revive a closed Token panel', scenario, async page => {
      await page.evaluate(() => Array.from(document.querySelectorAll('nav button')).find(el => el.textContent === window.__accountReview.copy.sections.tokens).click()); await page.type(selector, value);
      await page.evaluate(index => document.querySelectorAll('.ap-consumption .ap-inline button')[index].click(), index);
      await waitPending(page, scenario); await page.evaluate(() => Array.from(document.querySelectorAll('nav button')).find(el => el.textContent === window.__accountReview.copy.sections.usage).click());
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
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-metrics article')].find(item => item.querySelector('span')?.textContent === window.__accountReview.copy.availableCredits)?.querySelector('strong')?.textContent === '222');
    await settle(page, 'balance');
    // A subsequent DOM task observes completion without depending on a network timing sleep.
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
    assert.equal(await page.$eval('.ap-context-card select', el => el.value), organization);
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('.ap-metrics article')].find(item => item.querySelector('span')?.textContent === window.__accountReview.copy.availableCredits)?.querySelector('strong')?.textContent), '222');
  });
  await runCase('paid order refreshes same-context balance', 'balance-paid', async page => {
    await waitPending(page, 'balance'); await settle(page, 'balance');
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-metrics article')].find(item => item.querySelector('span')?.textContent === window.__accountReview.copy.availableCredits)?.querySelector('strong')?.textContent === '333');
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


  const authMode = async (page, mode) => page.evaluate(mode => {
    const copy = window.__accountReview.copy;
    Array.from(document.querySelectorAll('[data-testid="account-platform-popover"] form button'))
      .find(element => element.textContent === copy.authModes[mode]).click();
  }, mode);
  const clickCopy = async (page, key) => page.evaluate(key => {
    const text = window.__accountReview.copy[key];
    const button = Array.from(document.querySelectorAll('button')).find(element => element.textContent === text);
    if (!button || button.disabled) throw new Error('Controlled action is unavailable: ' + key);
    button.click();
  }, key);
  const enterPhone = async page => page.type(panel + ' input[autocomplete="tel-national"]', '13800000000');
  const enterRegistration = async page => {
    await authMode(page, 'register');
    await page.type(panel + ' input[autocomplete="username"]', 'FixtureUser');
    await enterPhone(page);
    const passwords = await page.$$(panel + ' input[type="password"]');
    await passwords[0].type('PublicPassword123');
    await passwords[1].type('PublicPassword123');
    await passwords[1].evaluate(element => element.blur());
  };
  const waitRegistration = async page => page.waitForFunction(() => {
    const copy = window.__accountReview.copy;
    return document.querySelector('[data-testid="account-platform-popover"]').textContent.includes(copy.registrationReady);
  });
  const solveCaptcha = async page => {
    await clickCopy(page, 'sendCode');
    await page.waitForSelector('[role="dialog"] input[type="range"]');
    await page.$eval('[role="dialog"] input[type="range"]', element => {
      element.value = '20'; element.dispatchEvent(new Event('input', { bubbles: true }));
      element.dispatchEvent(new Event('change', { bubbles: true }));
    });
    await clickCopy(page, 'captchaConfirm');
  };
  for (const locale of ['zh', 'en', 'ja']) {
    await runCase('mobile SMS login uses validated challenge and localized form in ' + locale, 'popover-mobile-login', async page => {
      await authMode(page, 'sms'); await enterPhone(page); await solveCaptcha(page);
      await page.waitForFunction(() => !document.querySelector('[role="dialog"]'));
      await page.type(panel + ' input[autocomplete="one-time-code"]', '123456');
      await page.click(panel + ' button[type="submit"]');
      await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes('Fixture User'), {}, panel);
      const requests = await page.evaluate(() => window.__accountReview.calls);
      assert.equal(requests.filter(item => item.path === 'nativeSMSLogin').length, 1);
      assert.equal(requests.some(item => item.path === 'nativeLogin'), false);
      assert.deepEqual(requests.find(item => item.path === 'nativeSMSLogin').input,
        { mobile: '13800000000', challenge_id: '00000000-0000-4000-8000-000000000006', code: '123456' });
    }, locale);
    await runCase('registration login failure preserves successful registration truth in ' + locale, 'popover-mobile-register-login-failure', async page => {
      await enterRegistration(page); await waitRegistration(page); await solveCaptcha(page);
      await page.waitForFunction(() => !document.querySelector('[role="dialog"]'));
      await page.type(panel + ' input[autocomplete="one-time-code"]', '123456');
      await page.click(panel + ' button[type="submit"]');
      await page.waitForFunction(() => document.querySelector('[role="alert"]')?.textContent === window.__accountReview.copy.registrationCompletedLoginFailed);
      assert.equal(await page.$eval(panel + ' input[type="password"]', element => element.value), '');
      assert.equal(await page.$eval(panel + ' input[autocomplete="username"]', element => element.value), 'FixtureUser');
      assert.equal(await page.$$eval(panel + ' input[autocomplete="one-time-code"]', elements => elements.length), 0);
      assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(item => item.path === 'nativeRegister').length), 1);
      assert.equal(await page.evaluate(() => document.body.textContent.includes('PRIVATE_PROVIDER_DETAIL')), false);
    }, locale);
    await runCase('provider SMS rate facts are finite and translated in ' + locale, 'popover-mobile-rate', async page => {
      await authMode(page, 'sms'); await enterPhone(page); await solveCaptcha(page);
      await page.waitForFunction(() => document.querySelector('[role="alert"]')?.textContent === window.__accountReview.copy.smsRateLimited);
      assert.equal(await page.$$eval('[role="dialog"]', elements => elements.length), 0);
      assert.equal(await page.$$eval(panel + ' form button', elements => elements.some(element => element.disabled && /\d{5}/u.test(element.textContent))), true);
      assert.equal(await page.evaluate(() => document.body.textContent.includes('SMS_RATE_LIMITED')), false);
    }, locale);
  }
  const submitRegistration = async page => {
    await enterRegistration(page); await waitRegistration(page); await solveCaptcha(page);
    await page.waitForFunction(() => !document.querySelector('[role="dialog"]'));
    await page.type(panel + ' input[autocomplete="one-time-code"]', '123456');
    await page.click(panel + ' button[type="submit"]');
  };
  const assertRecoveredGuest = async page => {
    await page.waitForFunction(() => document.querySelector('[role="alert"]')?.textContent === window.__accountReview.copy.registrationRecovered);
    assert.equal(await page.$eval(panel + ' input[autocomplete="username"]', element => element.value), 'FixtureUser');
    assert.equal(await page.$eval(panel + ' input[type="password"]', element => element.value), '');
    assert.equal(await page.$$eval(panel + ' input[autocomplete="one-time-code"], [role="dialog"]', elements => elements.length), 0);
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(item => item.path === 'nativeLogin').length), 0);
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(item => item.path === 'nativeRegister').length), 1);
    assert.equal(await page.$$eval(panel + ' input[type="password"]', elements => elements.length), 1);
  };
  for (const locale of ['zh', 'en', 'ja']) {
    await runCase('fresh registration still signs in with the submitted password in ' + locale, 'popover-mobile-register-fresh', async page => {
      await submitRegistration(page); await waitPending(page, 'nativeLogin');
      const credentials = await page.evaluate(() => window.__accountReview.calls.find(item => item.path === 'nativeLogin').credentials);
      assert.deepEqual(credentials, { account: 'FixtureUser', password: 'PublicPassword123' });
      await settle(page, 'nativeLogin');
      await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes('Fixture User'), {}, panel);
      assert.equal(await page.$(panel + ' input[type="password"]'), null);
    }, locale);
    await runCase('recovered registration preserves first-password truth without automatic sign-in in ' + locale, 'popover-mobile-recovered', async page => {
      await submitRegistration(page); await assertRecoveredGuest(page);
    }, locale);
  }
  await runCase('same-client reopened registration receives recovered truth without reviving SMS credentials', 'popover-mobile-recovered-late-reopen', async page => {
    await submitRegistration(page); await waitPending(page, 'nativeRegister');
    await page.evaluate(() => window.__accountReview.setOpen(false));
    await page.waitForFunction(() => !document.querySelector('[data-testid="account-platform-popover"]'));
    await page.evaluate(() => window.__accountReview.setOpen(true));
    await page.waitForSelector(panel + ' form');
    await settle(page, 'nativeRegister'); await assertRecoveredGuest(page);
  });
  await runCase('closed registration completion cannot sign in or restore secret fields', 'popover-mobile-recovered-late-closed', async page => {
    await submitRegistration(page); await waitPending(page, 'nativeRegister');
    await page.evaluate(() => window.__accountReview.setOpen(false));
    await page.waitForFunction(() => !document.querySelector('[data-testid="account-platform-popover"]'));
    await settle(page, 'nativeRegister');
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(item => item.path === 'nativeLogin').length), 0);
    await page.evaluate(() => window.__accountReview.setOpen(true));
    await page.waitForSelector(panel + ' form button:not(:disabled)');
    assert.equal(await page.$$eval(panel + ' input[type="password"], ' + panel + ' input[autocomplete="one-time-code"]', elements => elements.every(element => element.value === '')), true);
    assert.equal(await page.$('[role="dialog"]'), null);
  });
  await runCase('recovered registration on a replaced client cannot overwrite the current identity', 'popover-mobile-recovered-late-replacement', async page => {
    await submitRegistration(page); await waitPending(page, 'nativeRegister');
    await page.evaluate(() => window.__accountReview.replaceClient());
    await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes('Other User'), {}, panel);
    await settle(page, 'nativeRegister');
    await page.waitForFunction(selector => !document.querySelector(selector + ' button:disabled'), {}, panel);
    assert.equal(await page.$eval(panel, element => element.textContent.includes('Other User')), true);
    assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(item => item.path === 'nativeLogin').length), 0);
    assert.equal(await page.$('[role="alert"]'), null);
  });
  await runCase('registration input edits fence a delayed precheck result', 'popover-mobile-precheck-late', async page => {
    await enterRegistration(page); await waitPending(page, 'mobilePrecheck');
    await page.$eval(panel + ' input[autocomplete="username"]', element => element.select());
    await page.type(panel + ' input[autocomplete="username"]', 'ChangedUser');
    await settle(page, 'mobilePrecheck');
    assert.equal(await page.$eval(panel + ' input[autocomplete="username"]', element => element.value), 'ChangedUser');
    assert.equal(await page.evaluate(() => document.body.textContent.includes(window.__accountReview.copy.registrationReady)), false);
    assert.equal(await page.evaluate(() => window.__accountReview.calls.some(item => item.path === 'nativeCaptcha')), false);
  });
  await runCase('mode change discards a delayed captcha instead of reviving its dialog', 'popover-mobile-captcha-late', async page => {
    await authMode(page, 'sms'); await enterPhone(page); await clickCopy(page, 'sendCode'); await waitPending(page, 'captcha');
    await authMode(page, 'password'); await settle(page, 'captcha');
    assert.equal(await page.$$eval('[role="dialog"]', elements => elements.length), 0);
    assert.equal(await page.$$eval(panel + ' input[autocomplete="tel-national"]', elements => elements.length), 0);
  });
  await runCase('closing during SMS send cannot resurrect the challenge in a reopened form', 'popover-mobile-sms-late', async page => {
    await authMode(page, 'sms'); await enterPhone(page); await solveCaptcha(page); await waitPending(page, 'sms');
    await page.evaluate(() => window.__accountReview.setOpen(false));
    await page.waitForFunction(() => !document.querySelector('[data-testid="account-platform-popover"]'));
    await page.evaluate(() => window.__accountReview.setOpen(true));
    await page.waitForSelector(panel + ' input[autocomplete="one-time-code"]');
    assert.equal(await page.$eval(panel + ' button[type="submit"]', element => element.disabled), true);
    await settle(page, 'sms');
    await page.waitForFunction(() => !document.querySelector('[data-testid="account-platform-popover"] form button').disabled);
    assert.equal(await page.$$eval('[role="dialog"]', elements => elements.length), 0);
    assert.equal(await page.$eval(panel + ' button[type="submit"]', element => element.disabled), true);
    assert.equal(await page.$eval(panel + ' input[autocomplete="one-time-code"]', element => element.value), '');
  });
  await runCase('unknown mobile rejection uses static fallback without provider detail', 'popover-mobile-unknown-error', async page => {
    await authMode(page, 'sms'); await enterPhone(page); await solveCaptcha(page);
    await page.waitForSelector('[role="alert"]');
    assert.equal(await page.evaluate(() => document.body.textContent.includes('PRIVATE_PROVIDER_DETAIL')), false);
    assert.equal(await page.evaluate(() => document.body.textContent.includes('SMS_UNAVAILABLE')), false);
    const text = await page.$eval('[role="alert"]', element => element.textContent);
    assert.ok(text.trim());
    assert.notEqual(text, await page.evaluate(() => window.__accountReview.copy.smsUnavailable));
  });
  await runCase('same-client logout completion invalidates the reopened account summary', 'popover-logout-delay', async page => {
    await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes('Fixture User'), {}, panel);
    await clickCopy(page, 'logout'); await waitPending(page, 'nativeLogout');
    await page.evaluate(() => window.__accountReview.setOpen(false));
    await page.waitForFunction(() => !document.querySelector('[data-testid="account-platform-popover"]'));
    await page.evaluate(() => window.__accountReview.setOpen(true));
    await page.waitForFunction(selector => document.querySelector(selector)?.textContent.includes('Fixture User'), {}, panel);
    await settle(page, 'nativeLogout'); await page.waitForSelector(panel + ' input[type="password"]');
    assert.equal(await page.evaluate(() => document.body.textContent.includes('Fixture User')), false);
  });
  for (const scenario of ['entitlement-401', 'orders-401']) {
    await runCase(scenario + ' clears every protected surface', scenario, async page => {
      await page.waitForSelector('.ap-login');
      assert.equal(await page.$$eval('nav', elements => elements.length), 0);
      assert.equal(await page.evaluate(() => document.body.textContent.includes('Personal Token')), false);
    });
  }
  await runCase('users without organizations retain wallet, personal history and managed Tokens', 'no-organization', async page => {
    const labels = await page.$$eval('nav button', elements => elements.map(element => element.textContent));
    const copy = await page.evaluate(() => window.__accountReview.copy.sections);
    assert.deepEqual(labels, [copy.overview, copy.credits, copy.entitlements, copy.orders, copy.invoice, copy.usage, copy.tokens, copy.myRoles, copy.profile]);
  });
  for (const locale of ['zh', 'en', 'ja']) {
    await runCase('personal plan/history keeps exact amounts and authority scope in ' + locale, 'entitlement-history', async page => {
      await page.evaluate(() => Array.from(document.querySelectorAll('nav button')).find(element => element.textContent === window.__accountReview.copy.sections.credits).click());
      await page.waitForSelector('.ap-entitlements .ap-trace-card');
      const text = await page.$eval('.ap-entitlements', element => element.textContent);
      assert.ok(text.includes('9,007,199,254,740,992'));
      assert.ok(text.includes('90,071,992,547,409.93'));
      await page.evaluate(() => Array.from(document.querySelectorAll('nav button')).find(element => element.textContent === window.__accountReview.copy.sections.orders).click());
      await page.waitForSelector('.ap-trace-details');
      assert.equal(await page.evaluate(() => document.querySelector('.ap-main').textContent.includes('1970')), false);
      const before = await page.evaluate(() => window.__accountReview.calls.filter(item => item.path === '/api/token/v1/me/entitlement-packages' || (item.path === '/api/token/v1/recharge/orders' && item.method === 'GET')).length);
      await page.evaluate(() => Array.from(document.querySelectorAll('nav button')).find(element => element.textContent === window.__accountReview.copy.sections.overview).click());
      await page.select('.ap-context-card select', '00000000-0000-4000-8000-000000000002');
      await page.waitForFunction(() => [...document.querySelectorAll('.ap-metrics article')].find(item => item.querySelector('span')?.textContent === window.__accountReview.copy.availableCredits)?.querySelector('strong')?.textContent === '222');
      assert.equal(await page.evaluate(() => window.__accountReview.calls.filter(item => item.path === '/api/token/v1/me/entitlement-packages' || (item.path === '/api/token/v1/recharge/orders' && item.method === 'GET')).length), before);
      const labels = await page.$$eval('nav button', elements => elements.map(element => element.textContent));
      const copy = await page.evaluate(() => window.__accountReview.copy.sections);
      assert.ok(labels.includes(copy.credits)); assert.ok(labels.includes(copy.entitlements)); assert.ok(labels.includes(copy.tokens));
      assert.equal(labels.includes(copy.orders) || labels.includes(copy.invoice), false);
    }, locale);
  }
  for (const scenario of ['qr-png', 'qr-invalid', 'qr-terminal']) {
    await runCase('personal wallet QR boundary ' + scenario, scenario, async page => {
      await page.evaluate(() => Array.from(document.querySelectorAll('nav button')).find(element => element.textContent === window.__accountReview.copy.sections.credits).click());
      await page.waitForSelector('.ap-payment');
      if (scenario === 'qr-png') {
        await page.waitForSelector('.ap-payment img');
        assert.equal(await page.$eval('.ap-payment img', element => element.getAttribute('src').startsWith('data:image/png;base64,')), true);
      } else assert.equal(await page.$$eval('.ap-payment img', elements => elements.length), 0);
      if (scenario === 'qr-terminal') assert.equal(await page.evaluate(() => document.querySelector('.ap-payment').textContent.includes(window.__accountReview.copy.scanToPay)), false);
    });
  }

  for (const locale of ['zh','en','ja']) {
    await runCase('own role grants and nullable validity localize in '+locale,'a08-my-roles',async page => {
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.myRoles).click());
      await page.waitForFunction(()=>document.querySelector('main')?.textContent.includes('MEMBER'));
      assert.ok(await page.evaluate(()=>document.querySelector('main').textContent.includes(window.__accountReview.copy.myRoleBindings.forever)));
      const calls=await page.evaluate(()=>window.__accountReview.calls.filter(item=>item.path.endsWith('/role-bindings')));
      assert.equal(calls.length,1);assert.equal(calls[0].path,'/api/account/v1/me/role-bindings');
    },locale);
    await runCase('profile recent-auth denial is translated in '+locale,'a08-profile-recent',async page => {
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.profile).click());
      await page.type('main form input','ChangedUser');await page.click('main form button');
      await page.waitForFunction(()=>[...document.querySelectorAll('[role="alert"]')].some(el=>el.textContent===window.__accountReview.copy.recentAuthenticationRequired));
      assert.equal(await page.evaluate(()=>document.body.textContent.includes('RECENT_AUTHENTICATION_REQUIRED')),false);
    },locale);
    await runCase('restored registration retains first-password truth in '+locale,'popover-mobile-restored-active',async page=>{
      await page.waitForFunction(()=>document.querySelector('[role="alert"]')?.textContent===window.__accountReview.copy.registrationRecovered);
      assert.equal(await page.$eval(panel+' input[autocomplete="username"]',el=>el.value),'FixtureUser');
      assert.equal(await page.$eval(panel+' input[type="password"]',el=>el.value),'');
      assert.equal(await page.evaluate(()=>window.__accountReview.calls.filter(item=>item.path==='nativeLogin'||item.path==='nativeRegister').length),0);
    },locale);
    await runCase('component decimal strings and absent expiry remain exact in '+locale,'entitlement-history',async page=>{
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.credits).click());
      await page.waitForSelector('.ap-entitlements .ap-trace-card .ap-card');
      const text=await page.$eval('.ap-entitlements .ap-trace-card .ap-card',el=>el.textContent);
      assert.ok(text.includes('9,007,199,254,740,993'));assert.ok(text.includes('9,007,199,254,740,992'));
      assert.ok(await page.evaluate(()=>document.querySelector('.ap-entitlements').textContent.includes(window.__accountReview.copy.noExpiry)));
      assert.equal(text.includes('1970'),false);
    },locale);
  }
  for (const scenario of ['a08-profile-username','a08-profile-retry']) {
    await runCase('profile completion clears old identity '+scenario,scenario,async page=>{
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.profile).click());
      await page.type('main form input','ChangedUser');await page.click('main form button');
      await page.waitForFunction(()=>document.querySelector('main').textContent.includes('ChangedUser'));
      await page.click('main .ap-primary');await page.waitForSelector('.ap-login');
      assert.equal(await page.$$eval('nav',els=>els.length),0);
      assert.equal(await page.evaluate(()=>document.body.textContent.includes('Fixture User')),false);
    });
  }
  await runCase('profile worker intermediate states keep polling without duplicate completion','a08-profile-worker',async page=>{
    await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.profile).click());
    await page.type('main form input','ChangedUser'); await page.click('main form button');
    await page.waitForFunction(()=>document.querySelector('main').textContent.includes('ChangedUser'));
    await page.waitForFunction(()=>document.querySelector('main .ap-primary') && !document.querySelector('main .ap-primary').disabled); await page.click('main .ap-primary');
    for (const state of ['VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED']) {
      await page.waitForFunction(status=>window.__accountReview.calls.filter(call=>call.path.endsWith(window.__accountReview.ids.usage)).length >= ['VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED'].indexOf(status)+1,{},state);
      assert.equal(await page.$$eval('main .ap-primary',els=>els.length),0);
    }
    await page.waitForSelector('.ap-login',{timeout:20000});
    const calls=await page.evaluate(()=>window.__accountReview.calls);
    assert.equal(calls.filter(call=>call.path.endsWith('/actions/complete')).length,1);
    assert.equal(calls.filter(call=>call.path==='/api/logout').length,1);
    const requestID=await page.evaluate(()=>window.__accountReview.ids.usage);
    assert.equal(calls.filter(call=>call.path.endsWith(requestID)).length,4);
  });
  await runCase('profile completion waits for actual portal logout and retries a failed deletion','a08-profile-logout-retry',async page=>{
    await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.profile).click());
    await page.type('main form input','ChangedUser'); await page.click('main form button');
    await page.waitForFunction(()=>document.querySelector('main').textContent.includes('ChangedUser'));
    await page.waitForFunction(()=>document.querySelector('main .ap-primary') && !document.querySelector('main .ap-primary').disabled); await page.click('main .ap-primary');
    await page.waitForSelector('main [role=alert]');
    assert.equal(await page.$$eval('.ap-login',els=>els.length),0);
    await page.evaluate(()=>[...document.querySelectorAll('main button')].find(el=>el.textContent===window.__accountReview.copy.logout).click());
    await page.waitForSelector('.ap-login');
    const calls=await page.evaluate(()=>window.__accountReview.calls);
    assert.equal(calls.filter(call=>call.path.endsWith('/actions/complete')).length,1);
    assert.equal(calls.filter(call=>call.path==='/api/logout').length,2);
  });
  await runCase('profile mobile uses owned change and challenge before clearing session','a08-profile-mobile',async page=>{
    await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.profile).click());
    await page.select('main form select','MOBILE');await page.type('main form input','13900000000');await page.click('main form button');
    await page.waitForFunction(()=>document.querySelector('main').textContent.includes('13900000000'));
    await page.evaluate(()=>[...document.querySelectorAll('main button')].find(el=>el.textContent===window.__accountReview.copy.sendCode).click());
    await page.waitForSelector('main input[type="range"]');
    await page.evaluate(()=>[...document.querySelectorAll('main button')].find(el=>el.textContent===window.__accountReview.copy.captchaConfirm).click());
    await page.waitForFunction(()=>!document.querySelector('main input[autocomplete="one-time-code"]').disabled);
    await page.type('main input[autocomplete="one-time-code"]','123456');await page.click('main .ap-primary');await page.waitForSelector('.ap-login');
    const calls=await page.evaluate(()=>window.__accountReview.calls);
    assert.ok(calls.some(item=>item.path.endsWith('/mobile-challenges')&&item.body.captcha_id));
    assert.equal(calls.find(item=>item.path.endsWith('/actions/complete')).body.code,'123456');
  });
  await runCase('pending restored registration blocks repeated sends and creates','popover-mobile-restored-pending',async page=>{
    await page.waitForFunction(()=>document.body.textContent.includes(window.__accountReview.copy.registrationDoNotRepeat));
    await enterRegistration(page);await waitRegistration(page);await clickCopy(page,'sendCode');
    assert.equal(await page.evaluate(()=>window.__accountReview.calls.filter(item=>item.path==='nativeRegister'||item.path==='nativeSendSMS').length),0);
  });
  await runCase('unavailable OS persistence keeps process-only recovery disclosure','popover-mobile-memory-only',async page=>{
    await enterRegistration(page);await waitRegistration(page);await solveCaptcha(page);
    await page.waitForFunction(()=>document.body.textContent.includes(window.__accountReview.copy.registrationRecoveryMemoryOnly));
    assert.equal(await page.evaluate(()=>JSON.stringify(window.__accountReview.calls).includes('registration_query_token')),false);
  });
  await runCase('forgot password confirms new password and returns to login','popover-mobile-forgot',async page=>{
    await authMode(page,'forgot');await enterPhone(page);
    const inputs=await page.$$(panel+' input[type="password"]');await inputs[0].type('ChangedPassword123');await inputs[1].type('ChangedPassword123');
    await solveCaptcha(page);await page.waitForFunction(()=>!document.querySelector('[role="dialog"]'));
    await page.type(panel+' input[autocomplete="one-time-code"]','123456');await page.click(panel+' button[type="submit"]');
    await page.waitForFunction(()=>document.querySelector('[role="alert"]')?.textContent===window.__accountReview.copy.passwordResetComplete);
    const calls=await page.evaluate(()=>window.__accountReview.calls);assert.equal(calls.filter(item=>item.path==='nativeResetPassword').length,1);
    assert.equal(calls.some(item=>item.path==='nativeLogin'),false);assert.equal(await page.$eval(panel+' input[type="password"]',el=>el.value),'');
  });
  await runCase('forgot password uncertain reply retries the original challenge after retyping secrets','popover-mobile-forgot-retry',async page=>{
    await authMode(page,'forgot'); await enterPhone(page);
    const retype = async () => { const inputs=await page.$$(panel+' input[type="password"]'); await inputs[0].type('ChangedPassword123'); await inputs[1].type('ChangedPassword123'); };
    await retype(); await solveCaptcha(page); await page.waitForFunction(()=>!document.querySelector('[role="dialog"]'));
    await page.type(panel+' input[autocomplete="one-time-code"]','123456'); await page.click(panel+' button[type="submit"]');
    await page.waitForSelector(panel+' [role="alert"]');
    assert.equal(await page.$$eval(panel+' input[type="password"]',els=>els.every(el=>el.value==='')),true);
    await retype();
    assert.equal(await page.$$eval(panel+' input[autocomplete="one-time-code"]',els=>els.length),1);
    await page.type(panel+' input[autocomplete="one-time-code"]','123456'); await page.click(panel+' button[type="submit"]');
    await page.waitForFunction(()=>document.querySelector('[role="alert"]')?.textContent===window.__accountReview.copy.passwordResetComplete);
    const calls=await page.evaluate(()=>window.__accountReview.calls); const resets=calls.filter(call=>call.path==='nativeResetPassword');
    assert.equal(resets.length,2); assert.deepEqual(resets[0].input,resets[1].input);
    assert.equal(calls.filter(call=>call.path==='nativeSendSMS').length,1);
    assert.equal(calls.some(call=>call.path==='nativeLogin'),false);
  });
  for (const locale of ['zh','en','ja']) {
    await runCase('admin role grant retries same facts once and preserves fixed roles in '+locale,'admin-a08-role-retry',async page=>{
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent.includes(window.__accountReview.adminCopy.nav.roleBindings)).click());
      await page.waitForSelector('main tbody tr');assert.equal(await page.$$eval('main tbody button',els=>els.length),0);
      await page.type('main form input', '00000000-0000-4000-8000-000000000001');
      await page.select('main form select','ORG_ADMIN');
      await page.type('main form label:nth-child(3) input','00000000-0000-4000-8000-000000000002');
      await page.click('main form button');await page.waitForSelector('main [role="alert"]');
      await page.click('main form button');await page.waitForSelector('main tbody button');
      const requests=await page.evaluate(()=>window.__accountReview.calls.filter(item=>item.path.endsWith('/role-bindings')&&item.method==='POST'));
      assert.equal(requests.length,2);assert.equal(requests[0].body.idempotency_key,requests[1].body.idempotency_key);assert.ok(requests.every(item=>item.csrf));
    },locale,true);
  }
  for(const scenario of ['admin-a08-approval','admin-a08-approval-malformed']) {
    await runCase('approval submission validates exact Billing facts '+scenario,scenario,async page=>{
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent.includes(window.__accountReview.adminCopy.nav.approvals)).click());
      await page.waitForSelector('main form:nth-of-type(2)');
      await page.type('main form:nth-of-type(2) input:nth-of-type(1)','00000000-0000-4000-8000-000000000005');
      const inputs=await page.$$('main form:nth-of-type(2) input');await inputs[1].type('10000');await inputs[2].type('Controlled financial reason');
      await page.click('main form:nth-of-type(2) button');
      if(scenario.endsWith('malformed')) {await page.waitForSelector('main [role="alert"]');assert.equal(await page.evaluate(()=>window.__accountReview.calls.some(item=>item.path.endsWith('/approval-requests')&&item.method==='POST')),false);}
      else {await page.waitForSelector('main tbody tr');const calls=await page.evaluate(()=>window.__accountReview.calls);const command=calls.find(item=>item.path.endsWith('/approval-requests')&&item.method==='POST').body;assert.equal(command.normalized_command.amount_microcredits,10000);assert.equal(command.normalized_command.requested_by,'00000000-0000-4000-8000-000000000001');assert.equal(calls.filter(item=>item.path.endsWith('/actions/submit')).length,1);}
    },'en',true);
  }
  for (const [status,locale] of [['APPROVED','zh'],['EXECUTING','en'],['EXECUTED','ja'],['REJECTED','en'],['EXECUTION_FAILED','en'],['WITHDRAWN','en'],['EXPIRED','en'],['INVALIDATED','en'],['SUBMITTED','en']]) {
    await runCase('approval retry recovers existing '+status+' in '+locale,'admin-a08-approval-replay-'+status,async page=>{
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent.includes(window.__accountReview.adminCopy.nav.approvals)).click());
      await page.waitForSelector('main form:nth-of-type(2)');
      const inputs=await page.$$('main form:nth-of-type(2) input');await inputs[0].type('00000000-0000-4000-8000-000000000005');await inputs[1].type('10000');await inputs[2].type('Controlled financial reason');
      await page.click('main form:nth-of-type(2) button');await page.waitForSelector('main [role="alert"]');
      await page.click('main form:nth-of-type(2) button');
      await page.waitForFunction(()=>[...document.querySelectorAll('main form:nth-of-type(2) input')].every(el=>el.value===''),{timeout:3000});
      assert.equal(await page.evaluate(()=>typeof window.__accountReview.adminCopy.approvals.existing==='string' && window.__accountReview.adminCopy.approvals.existing.length>0),true);
      assert.equal(await page.$('main [role="alert"]'),null);
      await page.waitForFunction(status=>document.querySelector('main tbody')?.textContent.includes(status),{},status);
      const calls=await page.evaluate(()=>window.__accountReview.calls);
      const drafts=calls.filter(call=>call.path.endsWith('/approval-requests')&&call.method==='POST');
      assert.equal(drafts.length,2);assert.deepEqual(drafts[0].body,drafts[1].body);
      assert.equal(calls.filter(call=>call.path.endsWith('/adjustments')&&call.method==='POST').length,1);
      assert.equal(calls.filter(call=>call.path.endsWith('/actions/submit')).length,1);
      assert.equal(await page.$$eval('main form:nth-of-type(2) input',els=>els.every(el=>el.value==='')),true);
      // A new user action with changed facts is independent of the recovered request.
      const next=await page.$$('main form:nth-of-type(2) input');await next[0].type('00000000-0000-4000-8000-000000000005');await next[1].type('20000');await next[2].type('Different financial reason');
      await page.click('main form:nth-of-type(2) button');
      await page.waitForFunction(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/adjustments')&&call.method==='POST').length===2);
      const adjustments=await page.evaluate(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/adjustments')&&call.method==='POST'));
      assert.notEqual(adjustments[0].body.idempotency_key,adjustments[1].body.idempotency_key);
    },locale,true);
  }
  for(const locale of ['zh','en','ja']) {
    await runCase('pending Billing adjustment opens existing approval queue in '+locale,'admin-a08-adjustment-workflow',async page=>{
      page.on('dialog',dialog=>void dialog.accept('Controlled decision reason'));
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent.includes(window.__accountReview.adminCopy.nav.billing)).click());
      await page.waitForSelector('.adm-tabs');
      await page.evaluate(()=>[...document.querySelectorAll('.adm-tabs button')].find(el=>el.textContent===window.__accountReview.adminCopy.billing.adjustments).click());
      await page.waitForSelector('main tbody button');await page.click('main tbody button');
      await page.waitForFunction(()=>window.__accountReview.calls.some(call=>call.path.endsWith('/approval-requests')&&call.method==='GET'),{timeout:3000});
      const calls=await page.evaluate(()=>window.__accountReview.calls);
      assert.equal(calls.some(call=>call.method==='POST'&&(call.path.endsWith('/approve')||call.path.endsWith('/adjustments')||call.path.endsWith('/approval-requests'))),false);
      assert.equal(await page.$eval('main form label:nth-child(5) input',el=>el.value),'00000000-0000-4000-8000-000000000005');
    },locale,true);
  }
  await runCase('pending Billing adjustment retains permitted direct rejection','admin-a08-adjustment-workflow-reject',async page=>{
    page.on('dialog',dialog=>void dialog.accept('Controlled rejection reason'));
    await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent.includes(window.__accountReview.adminCopy.nav.billing)).click());await page.waitForSelector('.adm-tabs');
    await page.evaluate(()=>[...document.querySelectorAll('.adm-tabs button')].find(el=>el.textContent===window.__accountReview.adminCopy.billing.adjustments).click());
    await page.waitForSelector('main tbody button');await page.click('main tbody button.danger');
    await page.waitForFunction(()=>document.querySelector('main tbody')?.textContent.includes('REJECTED'));
    const calls=await page.evaluate(()=>window.__accountReview.calls);assert.equal(calls.filter(call=>call.path.endsWith('/reject')&&call.method==='POST').length,1);assert.equal(calls.some(call=>call.path.endsWith('/approve')),false);
  },'en',true);
  for (const locale of ['zh','en','ja']) {
    await runCase('granular admin permissions retain authorized screens in '+locale,'admin-a08-limited',async page=>{
      await page.waitForSelector('nav'); await page.waitForSelector('main [role="alert"]');
      assert.equal(await page.$$eval('.adm-metric',items=>items.length),0);
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent.includes(window.__accountReview.adminCopy.nav.users)).click());
      await page.waitForFunction(()=>document.querySelector('main tbody')?.textContent.includes('AuthorizedIdentity'));
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent.includes(window.__accountReview.adminCopy.nav.billing)).click());
      await page.waitForSelector('main tbody tr');
      await page.evaluate(()=>[...document.querySelectorAll('.adm-tabs button')].find(el=>el.textContent===window.__accountReview.adminCopy.billing.prices).click());
      await page.waitForFunction(()=>[...document.querySelectorAll('main [role="alert"]')].some(el=>el.textContent===window.__accountReview.adminCopy.denied));
      assert.equal(await page.$$eval('main form',items=>items.length),0);
      await page.evaluate(()=>[...document.querySelectorAll('.adm-tabs button')].find(el=>el.textContent===window.__accountReview.adminCopy.billing.wallets).click());
      await page.waitForSelector('main tbody tr');
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent.includes(window.__accountReview.adminCopy.nav.roleBindings)).click());
      await page.waitForFunction(()=>[...(document.querySelector('main form select')?.options || [])].some(option=>option.value==='PLATFORM_BILLING_OPERATOR'));
      const options=await page.$$eval('main form select option',items=>items.map(el=>el.value));
      assert.deepEqual(options,['PLATFORM_BILLING_OPERATOR','ORG_ADMIN']);
    },locale);
  }
  for (const locale of ['zh','en','ja']) {
    await runCase('A09 organization pool, exact quota, 207 denial and payment QR in '+locale,'a09-quota-failed',async page=>{
      await page.select('.ap-context-card select',await page.evaluate(()=>window.__accountReview.ids.organization));
      await page.waitForFunction(()=>!document.querySelector('.ap-context-card select').disabled);
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.entitlements).click());
      await page.waitForSelector('.ap-entitlements-section form input');
      await page.type('.ap-entitlements-section form input','12.345');
      await page.click('.ap-entitlements-section form button');
      await page.waitForSelector('main > [role="alert"]');
      const dispatched=await page.evaluate(()=>window.__accountReview.calls.find(call=>call.path.endsWith('/quota-grants:batch')));
      assert.equal(dispatched.body.items[0].amount_microcredits,123450);
      assert.equal(dispatched.body.items[0].membership_version,7);
      assert.equal(await page.$eval('.ap-entitlements-section form input',el=>el.value),'12.345');
      assert.equal(await page.$eval('main > [role="alert"]',el=>el.textContent),await page.evaluate(()=>window.__accountReview.copy.permissionDenied));
      await page.click('.ap-entitlements-section .ap-inline .ap-primary');
      await page.waitForSelector('.ap-payment img',{timeout:5000});
      const purchase=await page.evaluate(()=>window.__accountReview.calls.find(call=>call.path.endsWith('/organizations/'+window.__accountReview.ids.organization+'/recharge/orders')));
      assert.deepEqual(purchase.body,{product_id:'PLAN_TRIAL',channel:'ALIPAY'});
      assert.equal(await page.$eval('.ap-payment img',el=>el.src.startsWith('data:image/png;base64,')),true);
      const paths=await page.evaluate(()=>window.__accountReview.calls.map(call=>call.path));
      assert.ok(paths.includes('/api/token/v1/organizations/00000000-0000-4000-8000-000000000002/entitlement-packages'));
    },locale);
  }
  await runCase('A09 unknown quota reply preserves exact key while completed fresh call gets new key','a09-quota-unknown',async page=>{
    await page.select('.ap-context-card select',await page.evaluate(()=>window.__accountReview.ids.organization));
    await page.waitForFunction(()=>!document.querySelector('.ap-context-card select').disabled);
    await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.entitlements).click());
    await page.waitForSelector('.ap-entitlements-section form input');await page.type('.ap-entitlements-section form input','1');
    await page.click('.ap-entitlements-section form button');await page.waitForSelector('main > [role="alert"]');
    assert.equal(await page.$eval('.ap-entitlements-section form input',el=>el.value),'1');
    await page.click('.ap-entitlements-section form button');await page.waitForFunction(()=>document.querySelector('.ap-entitlements-section form input')?.value==='');
    await page.type('.ap-entitlements-section form input','1');await page.click('.ap-entitlements-section form button');
    await page.waitForFunction(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/quota-grants:batch')).length===3);
    const writes=await page.evaluate(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/quota-grants:batch')));
    assert.equal(writes[0].idempotencyKey,writes[1].idempotencyKey);assert.deepEqual(writes[0].body,writes[1].body);
    assert.notEqual(writes[1].idempotencyKey,writes[2].idempotencyKey);
  });
  await runCase('A09 external global context refresh fences a late quota response','a09-quota-late',async page=>{
    await page.select('.ap-context-card select',await page.evaluate(()=>window.__accountReview.ids.organization));
    await page.waitForFunction(()=>!document.querySelector('.ap-context-card select').disabled);
    await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.entitlements).click());
    await page.waitForSelector('.ap-entitlements-section form input');await page.type('.ap-entitlements-section form input','1');await page.click('.ap-entitlements-section form button');
    await waitPending(page,'quota');await page.evaluate(()=>window.__accountReview.externalContext());
    await page.waitForSelector('.ap-resource-section.ap-context-personal');
    await settle(page,'quota','success');
    await page.waitForFunction(()=>![...document.querySelectorAll('main button')].some(el=>el.disabled));
    assert.equal(await page.$$eval('.ap-entitlements-section form',els=>els.length),0);
    assert.equal(await page.$$eval('main > [role="alert"]',els=>els.length),0);
  });
  await runCase('A09 popover observes same-context authorization decreases without refresh loops','popover-auth-scope',async page=>{
    await page.waitForFunction(()=>document.body.textContent.includes('Authority 100'));
    await page.evaluate(()=>window.__accountReview.changeAuthorization(2,true));
    await page.waitForFunction(()=>document.body.textContent.includes('Authority 2'));
    await page.evaluate(()=>window.__accountReview.changeAuthorization(1,true));
    await page.waitForFunction(()=>document.body.textContent.includes('Authority 1'));
    const before=await page.evaluate(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/bootstrap')).length);
    await page.evaluate(()=>window.__accountReview.observeOtherScope());
    await page.waitForFunction(before=>window.__accountReview.calls.filter(call=>call.path.endsWith('/bootstrap')).length>before,{},before);
    await new Promise(resolve=>setTimeout(resolve,400));
    const stable=await page.evaluate(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/bootstrap')).length);
    await new Promise(resolve=>setTimeout(resolve,400));
    assert.equal(await page.evaluate(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/bootstrap')).length),stable);
    assert.ok(stable-before<=2);
    assert.ok(await page.evaluate(()=>document.body.textContent.includes('Authority 1')));
  });
  await runCase('A09 per-scope authorization decrease refreshes controls and converges after an old-scope response','a09-auth-scope',async page=>{
    await page.select('.ap-context-card select',await page.evaluate(()=>window.__accountReview.ids.organization));
    await page.waitForFunction(()=>!document.querySelector('.ap-context-card select').disabled);
    await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.entitlements).click());
    await page.waitForSelector('.ap-entitlements-section form input');
    await page.evaluate(()=>window.__accountReview.changeAuthorization(2,false));
    await page.waitForFunction(()=>document.querySelectorAll('.ap-entitlements-section form').length===0);
    await page.evaluate(()=>window.__accountReview.changeAuthorization(1,true));
    await page.waitForSelector('.ap-entitlements-section form input');
    const before=await page.evaluate(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/bootstrap')).length);
    await page.evaluate(()=>window.__accountReview.observeOtherScope());
    await page.waitForFunction(before=>window.__accountReview.calls.filter(call=>call.path.endsWith('/bootstrap')).length>before,{},before);
    await new Promise(resolve=>setTimeout(resolve,400));
    const stable=await page.evaluate(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/bootstrap')).length);
    await new Promise(resolve=>setTimeout(resolve,400));
    assert.equal(await page.evaluate(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/bootstrap')).length),stable);
    assert.ok(stable-before<=2,'old-scope response must converge to active-scope authority');
    assert.equal(await page.$$eval('.ap-entitlements-section form',els=>els.length),1);
  });
  await runCase('A09 unit unknown outcome preserves facts/key and a completed new creation gets a fresh key','a09-unit-unknown',async page=>{
    await createUnit(page);await page.waitForSelector('main > [role="alert"]');
    assert.equal(await page.$eval('main form input',el=>el.value),'Public fixture unit');
    await page.click('main form button');await page.waitForFunction(()=>document.querySelector('main form input')?.value==='');
    await page.type('main form input','Public fixture unit');await page.click('main form button');
    await page.waitForFunction(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/units')&&call.method==='POST').length===3);
    const writes=await page.evaluate(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/units')&&call.method==='POST'));
    assert.equal(writes[0].idempotencyKey,writes[1].idempotencyKey);assert.deepEqual(writes[0].body,writes[1].body);
    assert.notEqual(writes[1].idempotencyKey,writes[2].idempotencyKey);
  });
  for (const scenario of ['a09-quota-missing-grant','a09-quota-wrong-owner','a09-quota-wrong-amount']) {
    await runCase('A09 malformed quota success retains exact operation '+scenario,scenario,async page=>{
      await page.select('.ap-context-card select',await page.evaluate(()=>window.__accountReview.ids.organization));
      await page.waitForFunction(()=>!document.querySelector('.ap-context-card select').disabled);
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.entitlements).click());
      await page.waitForSelector('.ap-entitlements-section form input');await page.type('.ap-entitlements-section form input','1');
      await page.click('.ap-entitlements-section form button');await page.waitForSelector('main > [role="alert"]');
      assert.equal(await page.$eval('.ap-entitlements-section form input',el=>el.value),'1');
      await page.click('.ap-entitlements-section form button');await page.waitForFunction(()=>document.querySelector('.ap-entitlements-section form input')?.value==='');
      const calls=await page.evaluate(()=>window.__accountReview.calls.filter(call=>call.path.endsWith('/quota-grants:batch')));
      assert.equal(calls.length,2);assert.equal(calls[0].idempotencyKey,calls[1].idempotencyKey);assert.deepEqual(calls[0].body,calls[1].body);
    });
  }
  for (const scenario of ['a09-purchase-invalid','a09-quota-revoke-invalid']) {
    await runCase('A09 wrong purchase or unproven revocation retains exact operation '+scenario,scenario,async page=>{
      await page.select('.ap-context-card select',await page.evaluate(()=>window.__accountReview.ids.organization));
      await page.waitForFunction(()=>!document.querySelector('.ap-context-card select').disabled);
      await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(el=>el.textContent===window.__accountReview.copy.sections.entitlements).click());
      const selector=scenario==='a09-purchase-invalid'?'.ap-entitlements-section .ap-inline .ap-primary':'.ap-entitlements-section .ap-trace-card button';
      await page.waitForSelector(selector);await page.click(selector);await page.waitForSelector('main > [role="alert"]');
      assert.equal(await page.$$eval('.ap-payment img',items=>items.length),0);
      await page.click(selector);
      if(scenario==='a09-purchase-invalid') await page.waitForSelector('.ap-payment img');
      else await page.waitForFunction(()=>document.querySelectorAll('.ap-entitlements-section .ap-trace-card button').length===0);
      const calls=await page.evaluate(suffix=>window.__accountReview.calls.filter(call=>call.method==='POST'&&call.path.endsWith(suffix)),scenario==='a09-purchase-invalid'?'/recharge/orders':'/revoke');
      assert.equal(calls.length,2);assert.equal(calls[0].idempotencyKey,calls[1].idempotencyKey);assert.deepEqual(calls[0].body,calls[1].body);
    });
  }
  for (const scenario of ['a09-unit-empty','a09-unit-wrong-org','a09-unit-wrong-name']) {
    await runCase('A09 unproven unit receipt retains facts/key and accepts exact replay '+scenario,scenario,async page=>{
      await createUnit(page);await page.waitForSelector('main > [role="alert"]');
      assert.equal(await page.$eval('main form input',el=>el.value),'Public fixture unit');
      await page.click('main form button');await page.waitForFunction(()=>document.querySelector('main form input')?.value==='');
      const calls=await page.evaluate(()=>window.__accountReview.calls.filter(call=>call.method==='POST'&&call.path.endsWith('/units')));
      assert.equal(calls.length,2);assert.equal(calls[0].idempotencyKey,calls[1].idempotencyKey);assert.deepEqual(calls[0].body,calls[1].body);
    });
  }
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
