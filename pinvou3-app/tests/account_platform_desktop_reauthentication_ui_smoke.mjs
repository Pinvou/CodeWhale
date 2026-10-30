import assert from 'node:assert/strict';
import fs from 'node:fs';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
const browserPath = [process.env.CHROME, '/usr/bin/google-chrome', '/usr/bin/chromium', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe'].find(value => value && fs.existsSync(value));
assert.ok(browserPath, 'Chromium is required; set CHROME to its executable');
const root = import.meta.dirname + '/..';
const server = await createServer({ root, configFile: false, plugins: [react()], cacheDir: root + '/node_modules/.vite-desktop-reauthentication', server: { host: '127.0.0.1', port: 0, fs: { allow: [root, fs.realpathSync(root + '/node_modules')], deny: ['**/.env*', '**/*.{pem,crt,key}'] } } });
await server.listen();
let browser, pass = 0, fail = 0;
const results = [];
async function click(page, section, label) {
  await page.evaluate(({ section, label }) => { const button = [...document.querySelectorAll(section + ' button')].find(value => value.textContent === label && !value.disabled); if (!button) throw new Error('Missing controlled UI action'); button.click(); }, { section, label });
}
async function begin(page) {
  await page.waitForSelector('main .ap-danger-section');
  await click(page, '.ap-danger-section', await page.evaluate(() => window.nativeReauth.closureCopy[window.nativeReauth.language].start));
  await page.waitForSelector('[role="alertdialog"] input');
  await page.type('[role="alertdialog"] input', 'CLOSE_MY_ACCOUNT');
  await page.$eval('form[role="alertdialog"]', form => form.requestSubmit());
}
async function password(page) {
  await page.waitForSelector('.ap-login form input[autocomplete="username"]');
  await page.type('.ap-login form input[autocomplete="username"]', 'controlled-original-user');
  await page.type('.ap-login form input[type="password"]', 'controlled-test-input');
  await page.$eval('.ap-login form', form => form.requestSubmit());
}
async function recharge(page) {
  await page.waitForFunction(() => document.querySelector('.ap-finance form button') && !document.querySelector('.ap-finance form button').disabled);
  await page.$eval('.ap-finance form', form => form.requestSubmit());
  await page.waitForFunction(() => window.nativeReauth.calls.some(value => value.resource === '/v1/recharge/orders' && value.method === 'POST'));
}
async function unknownRecharge(page) {
  await recharge(page);
  await page.waitForFunction(() => [...document.querySelectorAll('.ap-finance button')].some(value => value.textContent === window.nativeReauth.financeCopy[window.nativeReauth.language].retry && !value.disabled));
}
async function run(name, query, action, before, closure = true) {
  const page = await browser.newPage(); page.setDefaultTimeout(6000);
  const errors = []; page.on('pageerror', reason => errors.push(reason.message));
  try {
    await page.goto(server.resolvedUrls.local[0] + 'tests/fixtures/f10-desktop-reauthentication.html?' + query);
    if (before) await before(page);
    if (closure) await begin(page);
    await action(page); assert.deepEqual(errors, []);
    pass++; results.push({ name, pass: true });
  } catch (reason) {
    fail++; results.push({ name, pass: false, error: reason.message, diagnostic: await page.evaluate(() => ({ text: document.body.innerText, calls: window.nativeReauth?.calls, proof: window.nativeReauth?.proof() })).catch(() => null) });
  } finally { await page.close(); }
}
try {
  browser = await puppeteer.launch({ executablePath: browserPath, headless: true, args: ['--disable-gpu'] });
  for (const code of ['credential_store_unavailable', 'oidc_token_endpoint_unavailable']) await run('session recovery error retries without signing in: ' + code, 'session-error=' + code, async page => {
    await page.waitForSelector('.ap-login [role="alert"]');
    assert.equal(await page.$eval('.ap-login [role="alert"]', node => node.textContent), await page.evaluate(() => window.nativeReauth.copy.zh.requestFailed));
    await click(page, '.ap-login', await page.evaluate(() => window.nativeReauth.copy.zh.retry));
    await page.waitForSelector('main .ap-danger-section');
    assert.equal(await page.evaluate(() => window.nativeReauth.loginCalls), 0);
  }, undefined, false);
  for (const expired of [false, true]) await run('guest session stays a login without recovery error: ' + expired, 'session-guest=1' + (expired ? '&session-expired=1' : ''), async page => {
    await page.waitForSelector('.ap-login form');
    assert.equal(await page.$('.ap-login [role="alert"]'), null);
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('.ap-login button')].some(button => button.textContent === window.nativeReauth.copy.zh.retry)), false);
  }, undefined, false);
  await run('late recovery failure cannot contaminate a replacement client', 'session-probe=1&session-delay=1&session-error=credential_store_unavailable', async page => {
    await page.waitForFunction(() => typeof window.nativeReauth.resolveSession === 'function');
    await click(page, 'aside', 'Replace client');
    await page.waitForSelector('main .ap-danger-section');
    await click(page, 'aside', 'Release old session');
    await page.waitForFunction(() => window.nativeReauth.sessionDelivered === 2);
    assert.equal(await page.$('.ap-login [role="alert"]'), null);
    assert.ok(await page.$('main .ap-danger-section'));
  }, undefined, false);
  for (const language of ['zh', 'en', 'ja']) await run('same actor retains first closure intent and waits for explicit retry ' + language, 'language=' + language, async page => {
    await page.waitForSelector('.ap-login form');
    assert.equal(await page.$('main'), null, 'the old business view retires before native login');
    assert.ok(await page.evaluate(() => document.body.textContent.includes(window.nativeReauth.copy[window.nativeReauth.language].desktopReauthenticationLead)));
    const original = await page.evaluate(() => window.nativeReauth.proof()); assert.ok(original);
    await password(page); await page.waitForSelector('main .ap-danger-section');
    assert.deepEqual(await page.evaluate(() => window.nativeReauth.proof()), original);
    assert.equal(await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/me/account-closure-requests').length), 1);
    assert.equal(await page.$('input[type="password"]'), null);
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-danger-section button')].some(value => value.textContent === window.nativeReauth.financeCopy[window.nativeReauth.language].retry && !value.disabled));
    await click(page, '.ap-danger-section', await page.evaluate(() => window.nativeReauth.financeCopy[window.nativeReauth.language].retry));
    await page.waitForFunction(() => window.nativeReauth.saved);
    await page.waitForFunction(() => window.nativeReauth.proof() === null);
    const writes = await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/me/account-closure-requests'));
    assert.equal(writes.length, 2); assert.equal(writes[0].idempotencyKey, original.key); assert.equal(writes[1].idempotencyKey, original.key);
    assert.deepEqual(writes.map(value => value.body), [original.facts, original.facts]);
  });
  await run('an existing unknown-result closure retains the original key through native re-login', 'unknown=1', async page => {
    await page.waitForFunction(() => window.nativeReauth.proof() && [...document.querySelectorAll('.ap-danger-section button')].some(value => value.textContent === window.nativeReauth.financeCopy.zh.retry && !value.disabled));
    const original = await page.evaluate(() => window.nativeReauth.proof());
    await click(page, '.ap-danger-section', await page.evaluate(() => window.nativeReauth.financeCopy.zh.retry));
    await password(page); await page.waitForSelector('main .ap-danger-section');
    assert.deepEqual(await page.evaluate(() => window.nativeReauth.proof()), original);
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-danger-section button')].some(value => value.textContent === window.nativeReauth.financeCopy.zh.retry && !value.disabled));
    await click(page, '.ap-danger-section', await page.evaluate(() => window.nativeReauth.financeCopy.zh.retry));
    await page.waitForFunction(() => window.nativeReauth.proof() === null);
    const writes = await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/me/account-closure-requests'));
    assert.equal(writes.length, 3); assert.ok(writes.every(value => value.idempotencyKey === original.key && value.body.confirmation === original.facts.confirmation));
  });
  for (const [query, code] of [['failure=1', 'failure'], ['wrong-actor=1', 'actor']]) await run('native ' + code + ' refusal preserves proof and removes the old account view', query, async page => {
    await page.waitForSelector('.ap-login form'); const original = await page.evaluate(() => window.nativeReauth.proof());
    await password(page); await page.waitForFunction(() => document.querySelector('.ap-login [role="alert"]'));
    assert.equal(await page.$('main'), null); assert.deepEqual(await page.evaluate(() => window.nativeReauth.proof()), original);
    assert.equal(await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/me/account-closure-requests').length), 1);
    assert.equal(await page.$eval('.ap-login input[type="password"]', input => input.value), '');
  });
  await run('cancel before password login preserves first-attempt closure proof', '', async page => {
    await page.waitForSelector('.ap-login form'); const original = await page.evaluate(() => window.nativeReauth.proof());
    await click(page, '.ap-login', await page.evaluate(() => window.nativeReauth.copy.zh.desktopReauthenticationCancel));
    await page.waitForSelector('.ap-login [role="alert"]'); assert.equal(await page.$('main'), null);
    assert.equal(await page.evaluate(() => window.nativeReauth.loginCalls), 0); assert.deepEqual(await page.evaluate(() => window.nativeReauth.proof()), original);
  });
  await run('cancelled native call cannot restore the old view after its late success', 'delay=1', async page => {
    await page.waitForSelector('.ap-login form'); const original = await page.evaluate(() => window.nativeReauth.proof());
    await password(page); await page.waitForFunction(() => typeof window.nativeReauth.resolveLogin === 'function');
    await click(page, '.ap-login', await page.evaluate(() => window.nativeReauth.copy.zh.desktopReauthenticationCancel));
    await page.waitForSelector('.ap-login [role="alert"]');
    await page.evaluate(() => { window.nativeReauth.resolveLogin(); });
    await page.waitForFunction(() => window.nativeReauth.client.financeSessionEpoch === 1);
    assert.equal(await page.$('main'), null); assert.deepEqual(await page.evaluate(() => window.nativeReauth.proof()), original);
    assert.equal(await page.evaluate(() => window.nativeReauth.logoutCalls), 0);
    assert.equal(await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/me/account-closure-requests').length), 1);
  });
  await run('client replacement fences the old native completion and proof', 'delay=1', async page => {
    await page.waitForSelector('.ap-login form'); const original = await page.evaluate(() => window.nativeReauth.proof());
    await password(page); await page.waitForFunction(() => typeof window.nativeReauth.resolveLogin === 'function');
    await page.evaluate(() => window.nativeReauth.replaceClient()); await page.waitForSelector('main .ap-danger-section');
    await page.evaluate(() => { window.nativeReauth.resolveLogin(); });
    await page.waitForFunction(() => window.nativeReauth.recent);
    assert.deepEqual(await page.evaluate(() => window.nativeReauth.proof()), original);
    assert.equal(await page.evaluate(() => window.nativeReauth.client.financeSessionEpoch), 0);
    assert.equal(await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/me/account-closure-requests').length), 1);
    assert.equal(await page.evaluate(() => window.nativeReauth.logoutCalls), 0);
  });
  await run('a malformed closure receipt after native re-login keeps the exact original proof', 'invalid-closure-ack=1', async page => {
    await page.waitForSelector('.ap-login form'); const original = await page.evaluate(() => window.nativeReauth.proof());
    await password(page); await page.waitForSelector('main .ap-danger-section');
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-danger-section button')].some(value => value.textContent === window.nativeReauth.financeCopy.zh.retry && !value.disabled));
    await click(page, '.ap-danger-section', await page.evaluate(() => window.nativeReauth.financeCopy.zh.retry));
    await page.waitForFunction(() => window.nativeReauth.saved);
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-danger-section button')].some(value => value.textContent === window.nativeReauth.financeCopy.zh.retry && !value.disabled));
    assert.deepEqual(await page.evaluate(() => window.nativeReauth.proof()), original);
    const writes = await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/me/account-closure-requests'));
    assert.equal(writes.length, 2); assert.ok(writes.every(value => value.idempotencyKey === original.key));
    assert.ok(await page.evaluate(() => document.querySelector('.ap-danger-section').textContent.includes(window.nativeReauth.financeCopy.zh.unknown)));
  });
  await run('personal recharge UNKNOWN survives same-actor native re-login and explicit same-key retry', 'recharge=1', async page => {
    await page.waitForSelector('.ap-login form');
    const original = await page.evaluate(() => window.nativeReauth.calls.find(value => value.resource === '/v1/recharge/orders' && value.method === 'POST'));
    await password(page); await page.waitForSelector('main .ap-finance');
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-finance button')].some(value => value.textContent === window.nativeReauth.financeCopy.zh.retry && !value.disabled));
    assert.equal(await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/recharge/orders' && value.method === 'POST').length), 1);
    await click(page, '.ap-finance', await page.evaluate(() => window.nativeReauth.financeCopy.zh.retry));
    await page.waitForFunction(() => document.querySelector('.ap-finance article'));
    const writes = await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/recharge/orders' && value.method === 'POST'));
    assert.equal(writes.length, 2); assert.equal(writes[1].idempotencyKey, original.idempotencyKey); assert.deepEqual(writes[1].body, original.body);
    assert.deepEqual(await page.evaluate(() => window.nativeReauth.rechargeProof()), { intent: null, blocked: false });
  }, unknownRecharge);
  for (const changed of ['context-change', 'permissions-change']) await run('personal recharge stays isolated and forbids fresh keys after ' + changed, 'recharge=1&' + changed + '=1', async page => {
    await password(page); await page.waitForSelector('main .ap-finance');
    await page.waitForFunction(() => window.nativeReauth.rechargeProof().blocked);
    assert.ok(await page.evaluate(() => document.querySelector('.ap-finance').textContent.includes(window.nativeReauth.financeCopy.zh.reauthenticationRecoveryUnavailable)));
    assert.equal(await page.$eval('.ap-finance form button', button => button.disabled), true);
    await page.$eval('.ap-finance form', form => form.requestSubmit());
    assert.equal(await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/recharge/orders' && value.method === 'POST').length), 1);
  }, unknownRecharge);
  for (const failed of [false, true]) await run('personal recharge proof survives native ' + (failed ? 'failure' : 'cancel') + ' and exact owner restoration', 'recharge=1' + (failed ? '&failure=1' : ''), async page => {
    await page.waitForSelector('.ap-login form');
    const original = await page.evaluate(() => window.nativeReauth.calls.find(value => value.resource === '/v1/recharge/orders' && value.method === 'POST'));
    if (failed) await password(page);
    else await click(page, '.ap-login', await page.evaluate(() => window.nativeReauth.copy.zh.desktopReauthenticationCancel));
    await page.waitForSelector('.ap-login [role="alert"]'); assert.equal(await page.evaluate(() => window.nativeReauth.rechargeProof().blocked), true);
    if (failed) { await page.evaluate(() => { window.nativeReauth.failed = false; }); await password(page); }
    else await click(page, '.ap-login', await page.evaluate(() => window.nativeReauth.copy.zh.retry));
    await page.waitForSelector('main .ap-finance');
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-finance button')].some(value => value.textContent === window.nativeReauth.financeCopy.zh.retry && !value.disabled));
    assert.equal(await page.evaluate(() => window.nativeReauth.rechargeProof().intent.key), original.idempotencyKey);
    assert.equal(await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/recharge/orders' && value.method === 'POST').length), 1);
  }, unknownRecharge);
  await run('a late old-view authoritative recharge receipt settles only its captured invocation', 'recharge=1&recharge-delay=1', async page => {
    await page.waitForSelector('.ap-login form');
    assert.equal(await page.evaluate(() => window.nativeReauth.rechargeProof().blocked), true);
    await page.evaluate(() => window.nativeReauth.resolveRecharge());
    await page.waitForFunction(() => !window.nativeReauth.rechargeProof().blocked);
    assert.equal(await page.$('main'), null);
    await password(page); await page.waitForSelector('main .ap-finance');
    assert.deepEqual(await page.evaluate(() => window.nativeReauth.rechargeProof()), { intent: null, blocked: false });
    assert.equal(await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/recharge/orders' && value.method === 'POST').length), 1);
  }, recharge);
  await run('explicit personal refresh releases isolation only after a late authoritative old-view receipt', 'recharge=1&recharge-delay=1&context-change=1', async page => {
    await password(page); await page.waitForSelector('main .ap-finance');
    await page.waitForFunction(() => window.nativeReauth.rechargeProof().blocked);
    assert.equal(await page.$eval('.ap-finance form button', button => button.disabled), true);
    await click(page, '.ap-finance header', await page.evaluate(() => window.nativeReauth.financeCopy.zh.refresh));
    await page.waitForFunction(() => !document.querySelector('.ap-finance header button').disabled);
    assert.equal(await page.$eval('.ap-finance form button', button => button.disabled), true, 'explicit refresh cannot release an unresolved isolated record');
    await page.evaluate(() => window.nativeReauth.resolveRecharge());
    await page.waitForFunction(() => !window.nativeReauth.rechargeProof().blocked);
    assert.equal(await page.$eval('.ap-finance form button', button => button.disabled), true, 'late completion must not update the replacement UI');
    await click(page, '.ap-finance header', await page.evaluate(() => window.nativeReauth.financeCopy.zh.refresh));
    await page.waitForFunction(() => !document.querySelector('.ap-finance form button').disabled);
    assert.equal(await page.evaluate(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/recharge/orders' && value.method === 'POST').length), 1);
  }, recharge);
  await run('a retry denial cannot erase the earlier unknown recharge identity', 'recharge=1&recharge-denied=1', async page => {
    await password(page); await page.waitForSelector('main .ap-finance');
    const original = await page.evaluate(() => window.nativeReauth.rechargeProof().intent);
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-finance button')].some(value => value.textContent === window.nativeReauth.financeCopy.zh.retry && !value.disabled));
    await click(page, '.ap-finance', await page.evaluate(() => window.nativeReauth.financeCopy.zh.retry));
    await page.waitForFunction(() => window.nativeReauth.calls.filter(value => value.resource === '/v1/recharge/orders' && value.method === 'POST').length === 2);
    await page.waitForFunction(() => [...document.querySelectorAll('.ap-finance button')].some(value => value.textContent === window.nativeReauth.financeCopy.zh.retry && !value.disabled));
    assert.deepEqual(await page.evaluate(() => window.nativeReauth.rechargeProof().intent), original);
    assert.equal(await page.$eval('.ap-finance form button', button => button.disabled), true);
  }, unknownRecharge);
} finally {
  await browser?.close(); await server.close();
  console.log(JSON.stringify({ pass, fail, skip: 0, controlledIdentityProvider: true, results }, null, 2)); if (fail) process.exitCode = 1;
}
