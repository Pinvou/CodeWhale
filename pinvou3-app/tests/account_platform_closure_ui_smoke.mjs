import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { fileURLToPath } from 'node:url';
import react from '../node_modules/@vitejs/plugin-react/dist/index.js';
import * as puppeteer from '../node_modules/puppeteer-core/lib/puppeteer/puppeteer-core.js';
import { createServer } from '../node_modules/vite/dist/node/index.js';
const work = path.dirname(fileURLToPath(import.meta.url)); const root = path.resolve(work, '..');
const modules = path.resolve(root, 'node_modules');
const chrome = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; no test performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-closure-'));
let vite, browser, page, count = 0; const errors = [];
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], resolve: { alias: { react: path.join(modules, 'react'), 'react-dom': path.join(modules, 'react-dom') } }, server: { host: '127.0.0.1', port: 0, watch: null, fs: { allow: [root, modules], deny: ['.env', '.env.*', '*.crt', '*.pem'] } } }); await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] }); page = await browser.newPage(); page.setDefaultTimeout(10000); page.on('pageerror', error => errors.push(error.message));
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/closure-fixture.html`;
  const fresh = async options => {
    errors.length = 0;
    // Cases use the same controlled origin; retained-proof assertions remain
    // within a case (locale/owner changes or reload), never across fresh cases.
    if (page.url().startsWith(url)) await page.evaluate(() => document.defaultView.sessionStorage.clear());
    await page.goto(url); await page.waitForFunction(() => window.closure);
    await page.evaluate(options => { Object.assign(window.closure, options); window.closure.render(); }, options || {});
    await page.waitForFunction(() => document.querySelector('section h2')?.textContent === window.closure.copy().title && !document.body.textContent.includes(window.closure.copy().queryLoading));
  };
  const click = async (key, accept = true) => {
    await page.waitForFunction(key => [...document.querySelectorAll('button')].some(button => button.textContent === window.closure.copy()[key] && !button.disabled), {}, key);
    await page.evaluate(key => [...document.querySelectorAll('button')].find(b => b.textContent === window.closure.copy()[key]).click(), key);
    if (key === 'start' && accept) {
      await page.waitForSelector('.ap-closure-confirm-dialog input');
      await page.type('.ap-closure-confirm-dialog input', 'CLOSE_MY_ACCOUNT');
      await page.click('.ap-closure-confirm-dialog button[type="submit"]');
    }
  };
  const run = async (name, fn) => { await fn(); assert.deepEqual(errors, []); console.log(`PASS ${name}`); count++; };
  for (const language of ['zh', 'en', 'ja']) await run(`${language}: real personal and repair views have localized labels`, async () => { await fresh({ language }); assert.ok(await page.evaluate(() => document.body.textContent.includes(window.closure.copy().start) && !document.body.textContent.includes('undefined'))); await fresh({ language, admin: true }); await page.waitForSelector('textarea'); assert.ok(await page.evaluate(() => document.body.textContent.includes(window.closure.finance().reason) && !document.body.textContent.includes('undefined'))); });
  await run('failed current query disables new closure; retry read enables after authoritative absence', async () => { await fresh({ queryError: 500 }); await click('queryRetry'); await page.waitForFunction(() => document.body.textContent.includes(window.closure.copy().queryFailed)); assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].some(b => b.textContent === window.closure.copy().start)), false); await page.evaluate(() => { window.closure.queryError = 0; }); await click('queryRetry'); await page.waitForFunction(() => [...document.querySelectorAll('button')].some(b => b.textContent === window.closure.copy().start)); assert.equal(await page.evaluate(() => window.closure.calls.filter(x => x.method === 'POST').length), 0); });
  await run('personal lost ACK reuses exact original confirmation and key across locale switch', async () => { await fresh(); await page.evaluate(() => { window.closure.unknown = true; }); await click('start'); await page.waitForFunction(() => document.body.textContent.includes(window.closure.finance().unknown)); await page.evaluate(() => { window.closure.language = 'ja'; window.closure.render(); }); await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === window.closure.finance().retry)); await page.evaluate(() => [...document.querySelectorAll('button')].find(b => b.textContent === window.closure.finance().retry).click()); await page.waitForFunction(() => document.body.textContent.includes(window.closure.copy().stages.CLOSURE_REQUESTED)); const calls = await page.evaluate(() => window.closure.calls.filter(x => x.method === 'POST')); assert.equal(calls.length, 2); assert.equal(calls[0].body, calls[1].body); assert.equal(calls[0].headers['Idempotency-Key'], calls[1].headers['Idempotency-Key']); assert.equal(calls[0].headers['X-CSRF-Token'], 'controlled-csrf'); });
  await run('retired owner completion never appears in next owner view', async () => { await fresh(); await page.evaluate(() => { window.closure.delay = true; }); await click('start'); await page.waitForFunction(() => typeof window.closure.resolve === 'function'); await page.evaluate(() => { window.closure.owner = '33333333-3333-4333-8333-333333333333'; window.closure.client.financeSessionEpoch++; window.closure.render(); window.closure.resolve(); }); await page.waitForFunction(() => [...document.querySelectorAll('button')].some(b => b.textContent === window.closure.copy().start)); assert.equal(await page.evaluate(() => document.body.textContent.includes(window.closure.copy().stages.CLOSURE_REQUESTED)), false); });
  await run('repair lost ACK freezes reason and same key; never adds a new invocation', async () => { await fresh({ admin: true }); await page.waitForSelector('textarea'); await page.type('textarea', 'Controlled closure stage retry'); await page.evaluate(() => { window.closure.unknown = true; [...document.querySelectorAll('button')].find(b => b.textContent === window.closure.finance().retry).click(); }); await page.waitForFunction(() => document.querySelector('textarea').disabled && document.body.textContent.includes(window.closure.finance().unknown)); await page.evaluate(() => [...document.querySelectorAll('button')].find(b => b.textContent === window.closure.finance().retry).click()); await page.waitForFunction(() => window.closure.calls.filter(x => x.method === 'POST').length === 2); const calls = await page.evaluate(() => window.closure.calls.filter(x => x.method === 'POST')); assert.equal(calls[0].body, calls[1].body); assert.equal(calls[0].headers['Idempotency-Key'], calls[1].headers['Idempotency-Key']); });
  await run('read-only authority displays state without mutation UI', async () => { await fresh({ readonly: true }); assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].some(b => b.textContent === window.closure.copy().start)), false); await fresh({ readonly: true, admin: true }); assert.equal(await page.evaluate(() => document.querySelector('textarea')), null); assert.equal(await page.evaluate(() => window.closure.calls.some(x => x.method === 'POST')), false); });
  await run('simulated WK prompt cancellation still permits feature-local typed confirmation without a POST', async () => {
    await fresh();
    await page.evaluate(() => { window.closure.promptCalls = 0; window.prompt = () => { window.closure.promptCalls++; return null; }; });
    await click('start', false);
    await page.waitForFunction(() => Boolean(document.querySelector('.ap-closure-confirm-dialog')), { timeout: 3000 });
    assert.equal(await page.evaluate(() => window.closure.promptCalls), 0);
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 0);
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !document.querySelector('.ap-closure-confirm-dialog'));
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 0);
  });
  for (const language of ['zh', 'en', 'ja']) await run(`${language}: typed cancellation, mismatch and explicit consent preserve zero/one dispatch`, async () => {
    await fresh({ language }); await click('start', false);
    await page.waitForSelector('.ap-closure-confirm-dialog input');
    assert.equal(await page.evaluate(() => document.activeElement === document.querySelector('.ap-closure-confirm-dialog input')), true);
    assert.equal(await page.evaluate(() => document.querySelector('.ap-closure-confirm-dialog').textContent.includes(window.closure.copy().confirmInput)), true);
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 0);
    await page.type('.ap-closure-confirm-dialog input', 'CLOSE_MY_ACCOUNT '); await page.click('.ap-closure-confirm-dialog button[type="submit"]');
    await page.waitForFunction(() => document.querySelector('.ap-closure-confirm-dialog [role="alert"]')?.textContent === window.closure.copy().confirmMismatch);
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 0);
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('.ap-closure-confirm-dialog'));
    await click('start', false); await page.waitForSelector('.ap-closure-confirm-dialog input');
    await page.click('.ap-closure-confirm-dialog button[type="button"]'); await page.waitForFunction(() => !document.querySelector('.ap-closure-confirm-dialog'));
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 0);
    await click('start'); await page.waitForFunction(() => document.body.textContent.includes(window.closure.copy().stages.CLOSURE_REQUESTED));
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 1);
    assert.equal(JSON.parse(await page.evaluate(() => window.closure.calls.find(call => call.method === 'POST').body)).confirmation, 'CLOSE_MY_ACCOUNT');
  });
  await run('late displayed accept/cancel cannot consume a newer consent; duplicate accept dispatches once', async () => {
    await fresh(); await click('start', false); await page.waitForSelector('.ap-closure-confirm-dialog input');
    await page.type('.ap-closure-confirm-dialog input', 'CLOSE_MY_ACCOUNT');
    await page.evaluate(() => {
      const form = document.querySelector('.ap-closure-confirm-dialog'), button = form.querySelector('button[type="button"]');
      const props = node => node[Object.keys(node).find(key => key.startsWith('__reactProps$'))];
      window.oldAccept = props(form).onSubmit; window.oldCancel = props(button).onClick;
    });
    await page.keyboard.press('Escape'); await page.waitForFunction(() => !document.querySelector('.ap-closure-confirm-dialog'));
    await click('start', false); await page.waitForSelector('.ap-closure-confirm-dialog input');
    await page.evaluate(() => { window.oldAccept({ preventDefault() {} }); window.oldCancel(); });
    assert.equal(await page.evaluate(() => Boolean(document.querySelector('.ap-closure-confirm-dialog'))), true);
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 0);
    await page.type('.ap-closure-confirm-dialog input', 'CLOSE_MY_ACCOUNT');
    await page.evaluate(() => { const form = document.querySelector('.ap-closure-confirm-dialog'); const submit = form[Object.keys(form).find(key => key.startsWith('__reactProps$'))].onSubmit; submit({ preventDefault() {} }); submit({ preventDefault() {} }); });
    await page.waitForFunction(() => window.closure.calls.filter(call => call.method === 'POST').length === 1);
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 1);
  });
  for (const change of ['permission', 'query', 'owner', 'client', 'epoch']) await run(`${change} change invalidates captured typed consent without a POST or focus theft`, async () => {
    await fresh(); await click('start', false); await page.waitForSelector('.ap-closure-confirm-dialog input');
    await page.type('.ap-closure-confirm-dialog input', 'CLOSE_MY_ACCOUNT');
    await page.evaluate(change => {
      const form = document.querySelector('.ap-closure-confirm-dialog'); window.staleSubmit = form[Object.keys(form).find(key => key.startsWith('__reactProps$'))].onSubmit;
      const focus = document.createElement('input'); focus.id = 'controlled-current-focus'; document.body.append(focus); focus.focus();
      const c = window.closure;
      if (change === 'permission') c.readonly = true;
      if (change === 'owner') c.owner = '33333333-3333-4333-8333-333333333333';
      if (change === 'client') { const old = c.client; c.client = Object.assign(Object.create(Object.getPrototypeOf(old)), old); }
      if (change === 'epoch') c.client.financeSessionEpoch++;
      if (change === 'query') { c.receipt = { closure_request_id: '11111111-1111-4111-8111-111111111111', status: 'IN_PROGRESS', stage: 'CLOSURE_REQUESTED', blockers: [], sla_expires_at: '2030-01-01T00:00:00Z' }; [...document.querySelectorAll('.ap-danger-section button')].find(button => button.textContent === c.copy().queryRetry).click(); }
      else c.render();
    }, change);
    await page.waitForFunction(() => !document.querySelector('.ap-closure-confirm-dialog'));
    await page.evaluate(() => { window.staleSubmit({ preventDefault() {} }); });
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 0);
    assert.equal(await page.evaluate(() => document.activeElement?.id === 'controlled-current-focus'), true);
  });
  await run('hook effect restoration cannot submit an old consent in a new client before its render', async () => {
    await fresh(); await click('start', false); await page.waitForSelector('.ap-closure-confirm-dialog input');
    await page.type('.ap-closure-confirm-dialog input', 'CLOSE_MY_ACCOUNT');
    await page.evaluate(() => { const c = window.closure, old = c.client; c.client = Object.assign(Object.create(Object.getPrototypeOf(old)), old); c.afterEffectSubmit = true; c.render(); });
    await page.waitForFunction(() => window.closure.afterEffectCalls === 1);
    await page.waitForFunction(() => !document.querySelector('.ap-closure-confirm-dialog'));
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 0);
  });
  await run('controlled mounted owner change cannot reuse old UNKNOWN facts in the hook effect gap', async () => {
    await fresh({ keepMounted: true, unknown: true }); await click('start');
    await page.waitForFunction(() => document.body.textContent.includes(window.closure.finance().unknown));
    const proof = await page.evaluate(() => Object.entries(document.defaultView.sessionStorage));
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 1);
    await page.evaluate(() => { const c = window.closure; c.owner = '33333333-3333-4333-8333-333333333333'; c.afterEffectSubmit = true; c.render(); });
    await page.waitForFunction(() => window.closure.afterEffectCalls === 1);
    await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === window.closure.copy().start && !button.disabled));
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 1);
    assert.deepEqual(await page.evaluate(() => Object.entries(document.defaultView.sessionStorage)), proof);
    await click('start', false); await page.waitForSelector('.ap-closure-confirm-dialog input');
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 1);
    await page.keyboard.press('Escape');
  });
  await run('unmounted typed consent cannot dispatch through a retained callback', async () => {
    // The preceding independent scenario deliberately retains the former
    // actor's proof. Start this controlled origin's next case independently.
    await page.evaluate(() => document.defaultView.sessionStorage.clear());
    await fresh(); await click('start', false); await page.waitForSelector('.ap-closure-confirm-dialog input');
    await page.type('.ap-closure-confirm-dialog input', 'CLOSE_MY_ACCOUNT');
    await page.evaluate(() => { const form = document.querySelector('.ap-closure-confirm-dialog'); window.unmountedSubmit = form[Object.keys(form).find(key => key.startsWith('__reactProps$'))].onSubmit; window.closure.admin = true; window.closure.render(); });
    await page.waitForSelector('textarea');
    await page.evaluate(() => { window.unmountedSubmit({ preventDefault() {} }); });
    assert.equal(await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST').length), 0);
  });
  for (const language of ['zh', 'en', 'ja']) await run(`${language}: fresh repair recent-authentication rejection is not a source change`, async () => {
    await fresh({ admin: true, language, repair401: true }); await page.waitForSelector('textarea'); await page.type('textarea', 'Controlled fresh closure retry');
    await page.evaluate(() => [...document.querySelectorAll('button')].find(button => button.textContent === window.closure.finance().retry).click());
    await page.waitForFunction(() => document.body.textContent.includes(window.closure.copy().recentAuthenticationRequired) && document.querySelector('article [role="alert"]'));
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('article [role="alert"]')].some(alert => alert.textContent === window.closure.finance().sourceChanged)), false);
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].some(button => button.textContent === window.closure.copy().reauthenticate)), true);
  });
  await run('UNKNOWN repair 401 preserves original warning, reason and key', async () => {
    await fresh({ admin: true, unknown: true }); await page.waitForSelector('textarea'); await page.type('textarea', 'Controlled uncertain closure retry');
    await page.evaluate(() => [...document.querySelectorAll('button')].find(button => button.textContent === window.closure.finance().retry).click());
    await page.waitForFunction(() => document.body.textContent.includes(window.closure.finance().unknown));
    const proof = await page.evaluate(() => Object.entries(document.defaultView.sessionStorage));
    await page.evaluate(() => { window.closure.repair401 = true; [...document.querySelectorAll('button')].find(button => button.textContent === window.closure.finance().retry).click(); });
    await page.waitForFunction(() => document.body.textContent.includes(window.closure.copy().recentAuthenticationRequired));
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('article [role="alert"]')].some(alert => alert.textContent === window.closure.finance().unknown)), true);
    assert.deepEqual(await page.evaluate(() => Object.entries(document.defaultView.sessionStorage)), proof);
    const calls = await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST'));
    assert.equal(calls.length, 2); assert.equal(calls[0].body, calls[1].body); assert.equal(calls[0].headers['Idempotency-Key'], calls[1].headers['Idempotency-Key']);
  });
  for (const invalidSLA of ['missing', 'invalid']) await run(`${invalidSLA} SLA write ACK remains UNKNOWN and recovers only with original facts/key`, async () => {
    await fresh({ invalidSLA }); await click('start');
    await page.waitForFunction(() => document.body.textContent.includes(window.closure.finance().unknown));
    const proof = await page.evaluate(() => Object.entries(document.defaultView.sessionStorage));
    assert.equal(proof.length, 1);
    await page.evaluate(() => { window.closure.invalidSLA = false; [...document.querySelectorAll('button')].find(button => button.textContent === window.closure.finance().retry).click(); });
    await page.waitForFunction(() => document.body.textContent.includes(window.closure.copy().requested));
    const calls = await page.evaluate(() => window.closure.calls.filter(call => call.method === 'POST'));
    assert.equal(calls.length, 2); assert.equal(calls[0].body, calls[1].body); assert.equal(calls[0].headers['Idempotency-Key'], calls[1].headers['Idempotency-Key']);
    assert.equal(await page.evaluate(() => Object.keys(document.defaultView.sessionStorage).length), 0);
  });
  console.log(`RESULT ${count} PASS 0 FAIL 0 SKIP`);
} catch (error) { console.error(`FAIL after ${count} PASS: ${error.stack}`); process.exitCode = 1; }
finally { if (browser) await browser.close(); if (vite) await vite.close(); const absolute = path.resolve(profile); assert.equal(path.dirname(absolute), path.resolve(os.tmpdir())); assert.ok(path.basename(absolute).startsWith('pinvou-closure-')); fs.rmSync(absolute, { recursive: true, force: false }); }
