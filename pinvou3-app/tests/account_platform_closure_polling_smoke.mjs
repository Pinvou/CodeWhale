import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const modules = path.join(root, 'node_modules');
const chrome = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome'].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; no test performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-closure-poll-'));
let vite, browser, page, background, passed = 0;
const errors = [];
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], resolve: { alias: { react: path.join(modules, 'react'), 'react-dom': path.join(modules, 'react-dom') } }, server: { host: '127.0.0.1', port: 0, watch: null, fs: { allow: [root, modules], deny: ['.env', '.env.*', '*.crt', '*.pem'] } } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  page = await browser.newPage(); background = await browser.newPage();
  page.setDefaultTimeout(10000); page.on('pageerror', error => errors.push(error.message));
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/closure-fixture.html`;
  const wait = async condition => page.waitForFunction(condition, { polling: 50 });
  const readCount = () => page.evaluate(() => globalThis.closure.calls.filter(call => call.method === 'GET').length);
  const writes = () => page.evaluate(() => globalThis.closure.calls.filter(call => call.method === 'POST'));
  const click = async key => {
    await page.waitForFunction(key => [...document.querySelectorAll('.ap-danger-section button')].some(button => button.textContent === globalThis.closure.copy()[key] && !button.disabled), {}, key);
    await page.evaluate(key => [...document.querySelectorAll('.ap-danger-section button')].find(button => button.textContent === globalThis.closure.copy()[key]).click(), key);
  };
  const ready = () => wait(() => document.querySelector('.ap-danger-section') && !document.body.textContent.includes(globalThis.closure.copy().queryLoading));
  const fresh = async ({ existing = false, native = false } = {}) => {
    errors.length = 0; await page.bringToFront();
    if (page.url().startsWith(url)) await page.evaluate(() => document.defaultView.sessionStorage.clear());
    await page.goto(url); await wait(() => globalThis.closure);
    await page.evaluate(({ existing, native }) => {
      const c = globalThis.closure; c.language = 'zh'; c.keepMounted = true;
      if (native) c.client = c.nativeClient;
      c.receipt = existing ? { ...c.receiptTemplate } : null; c.render();
    }, { existing, native });
    await ready(); await click('queryRetry'); await ready();
  };
  const hide = async () => { await background.bringToFront(); await wait(() => document.visibilityState === 'hidden'); };
  const show = async () => { await page.bringToFront(); await wait(() => document.visibilityState === 'visible'); };
  const visibleTick = async () => {
    await hide(); await show();
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  };
  const panel = async active => {
    await page.evaluate(active => { globalThis.closure.sectionActive = active; globalThis.closure.render(); }, active);
    await page.waitForFunction(active => document.querySelector('#ap-panel-accountSettings').hidden === !active, {}, active);
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
  };
  const run = async (name, action) => { await action(); assert.deepEqual(errors, []); passed++; console.log(`PASS ${name}`); };
  const confirm = async () => {
    await page.waitForSelector('.ap-closure-confirm-dialog input');
    await page.type('.ap-closure-confirm-dialog input', 'CLOSE_MY_ACCOUNT');
    await page.click('.ap-closure-confirm-dialog button[type="submit"]');
  };

  await run('the real 15 second timer refreshes progress with GET only and stops at completion', async () => {
    await fresh({ existing: true }); const before = await readCount();
    await page.evaluate(() => { globalThis.closure.receipt.stage = 'FINANCIAL_CLEARANCE'; });
    await page.waitForFunction(before => globalThis.closure.calls.filter(call => call.method === 'GET').length > before && document.body.textContent.includes(globalThis.closure.copy().stages.FINANCIAL_CLEARANCE), { timeout: 21000, polling: 50 }, before);
    await page.evaluate(() => { globalThis.closure.receipt = { ...globalThis.closure.receipt, status: 'COMPLETED', stage: 'ANONYMIZED' }; });
    await visibleTick(); await wait(() => document.body.textContent.includes(globalThis.closure.copy().stages.ANONYMIZED));
    const stopped = await readCount();
    await new Promise(resolve => setTimeout(resolve, 16000));
    await visibleTick(); assert.equal(await readCount(), stopped); assert.deepEqual(await writes(), []);
  });
  await run('typed confirmation survives visibility and a complete polling interval without reading or writing', async () => {
    await fresh(); await click('start'); await page.waitForSelector('.ap-closure-confirm-dialog input');
    await page.type('.ap-closure-confirm-dialog input', 'CLOSE_MY_'); const before = await readCount();
    await visibleTick(); await new Promise(resolve => setTimeout(resolve, 16000));
    assert.equal(await page.$eval('.ap-closure-confirm-dialog input', input => input.value), 'CLOSE_MY_');
    assert.equal(await readCount(), before); assert.deepEqual(await writes(), []);
    await page.keyboard.press('Escape'); await wait(() => !document.querySelector('.ap-closure-confirm-dialog'));
  });
  await run('polling cannot start during a delayed POST or discard its success receipt', async () => {
    await fresh(); await page.evaluate(() => { globalThis.closure.delay = true; globalThis.closure.persistRequests = true; });
    await click('start'); await confirm(); await wait(() => globalThis.closure.calls.some(call => call.method === 'POST'));
    const before = await readCount(); await visibleTick(); await visibleTick(); assert.equal(await readCount(), before);
    await page.evaluate(() => { globalThis.closure.delay = false; globalThis.closure.resolve(); });
    await wait(() => document.body.textContent.includes(globalThis.closure.copy().requested) && document.body.textContent.includes(globalThis.closure.copy().stages.CLOSURE_REQUESTED));
    assert.equal((await writes()).length, 1);
  });
  await run('an earlier GET cannot erase a confirmed POST receipt even when abort is ignored', async () => {
    await fresh(); await page.evaluate(() => { globalThis.closure.holdRead = true; globalThis.closure.persistRequests = true; });
    await visibleTick(); await wait(() => globalThis.closure.readHolds.length === 1);
    await click('start'); await confirm();
    await wait(() => document.body.textContent.includes(globalThis.closure.copy().requested));
    assert.equal(await page.evaluate(() => globalThis.closure.readHolds[0].signal.aborted), true);
    const before = await readCount();
    await page.evaluate(() => { globalThis.closure.holdRead = false; }); await visibleTick();
    await page.waitForFunction(before => globalThis.closure.calls.filter(call => call.method === 'GET').length === before + 1, {}, before);
    assert.equal(await page.evaluate(() => globalThis.closure.readHolds.length), 1);
    await page.evaluate(() => { globalThis.closure.holdRead = false; globalThis.closure.releaseReads(); });
    await ready();
    assert.equal(await page.evaluate(() => document.body.textContent.includes(globalThis.closure.copy().stages.CLOSURE_REQUESTED)), true);
    assert.equal((await writes()).length, 1);
  });
  await run('an UNKNOWN retry keeps the original key and facts and fences an earlier GET without reopening consent', async () => {
    await fresh(); await page.evaluate(() => { globalThis.closure.unknown = true; });
    await click('start'); await confirm(); await wait(() => document.body.textContent.includes(globalThis.closure.finance().unknown));
    await page.evaluate(() => { globalThis.closure.holdRead = true; }); await visibleTick(); await wait(() => globalThis.closure.readHolds.length === 1);
    await page.evaluate(() => {
      globalThis.closure.persistRequests = true;
      [...document.querySelectorAll('button.ap-danger')].find(button => button.textContent === globalThis.closure.finance().retry).click();
    });
    await wait(() => document.body.textContent.includes(globalThis.closure.copy().requested));
    await page.evaluate(() => { globalThis.closure.holdRead = false; globalThis.closure.releaseReads(); }); await ready();
    const sent = await writes(); assert.equal(sent.length, 2);
    assert.match(sent[0].headers['Idempotency-Key'], /^closure-/);
    assert.equal(sent[0].headers['Idempotency-Key'], sent[1].headers['Idempotency-Key']);
    assert.equal(sent[0].body, sent[1].body);
    assert.equal(JSON.parse(sent[0].body).confirmation, 'CLOSE_MY_ACCOUNT');
    assert.equal(await page.evaluate(() => document.body.textContent.includes(globalThis.closure.copy().stages.CLOSURE_REQUESTED)), true);
  });
  await run('initial/manual/background reads never overlap and a subsequent read still works', async () => {
    await fresh({ existing: true }); await page.evaluate(() => { globalThis.closure.holdRead = true; });
    const before = await readCount(); await click('queryRetry'); await wait(() => globalThis.closure.readHolds.length === 1);
    await click('queryRetry'); await visibleTick(); assert.equal(await readCount(), before + 1);
    await page.evaluate(() => { globalThis.closure.holdRead = false; globalThis.closure.releaseReads(); }); await ready();
    await click('queryRetry'); await ready(); assert.equal(await readCount(), before + 2); assert.deepEqual(await writes(), []);
  });
  for (const status of [401, 403]) await run(`${status} retires automatic reads until an explicit manual recovery`, async () => {
    await fresh({ existing: true }); await page.evaluate(status => { globalThis.closure.queryError = status; }, status);
    await visibleTick(); await wait(() => document.body.textContent.includes(globalThis.closure.copy().queryFailed));
    const stopped = await readCount(); await visibleTick(); assert.equal(await readCount(), stopped);
    await panel(false); await panel(true); assert.equal(await readCount(), stopped);
    await page.evaluate(() => { globalThis.closure.queryError = 0; }); await visibleTick(); assert.equal(await readCount(), stopped);
    await click('queryRetry'); await ready(); assert.equal(await readCount(), stopped + 1);
    await visibleTick();
    await page.waitForFunction(stopped => globalThis.closure.calls.filter(call => call.method === 'GET').length === stopped + 2 && !document.body.textContent.includes(globalThis.closure.copy().queryLoading), {}, stopped);
    assert.equal(await readCount(), stopped + 2); assert.deepEqual(await writes(), []);
  });
  await run('silent transient failures preserve validated facts and bounded failures allow manual recovery', async () => {
    await fresh({ existing: true }); await page.evaluate(() => { globalThis.closure.queryError = 503; });
    for (let attempt = 1; attempt <= 3; attempt++) {
      const before = await readCount(); await visibleTick();
      await page.waitForFunction(before => globalThis.closure.calls.filter(call => call.method === 'GET').length === before + 1, {}, before);
      await page.evaluate(() => new Promise(resolve => { setTimeout(resolve, 50); }));
      assert.equal(await page.evaluate(() => document.body.textContent.includes(globalThis.closure.copy().stages.CLOSURE_REQUESTED)), true);
      assert.equal(await page.evaluate(() => document.body.textContent.includes(globalThis.closure.copy().queryFailed)), attempt === 3);
    }
    const stopped = await readCount(); await visibleTick(); assert.equal(await readCount(), stopped);
    await panel(false); await panel(true); assert.equal(await readCount(), stopped);
    await page.evaluate(() => { globalThis.closure.queryError = 0; }); await click('queryRetry'); await ready();
    assert.equal(await page.evaluate(() => document.body.textContent.includes(globalThis.closure.copy().queryFailed)), false); assert.deepEqual(await writes(), []);
  });
  await run('hiding aborts an active poll and ignores its late result, then visibility resumes a fresh read', async () => {
    await fresh({ existing: true });
    await page.evaluate(() => { globalThis.closure.receipt = { ...globalThis.closure.receipt, stage: 'FINANCIAL_CLEARANCE' }; globalThis.closure.holdRead = true; });
    await visibleTick(); await wait(() => globalThis.closure.readHolds.length === 1); await hide();
    await wait(() => globalThis.closure.readHolds[0].signal.aborted);
    await page.evaluate(() => { globalThis.closure.holdRead = false; globalThis.closure.releaseReads(); });
    assert.equal(await page.evaluate(() => document.body.textContent.includes(globalThis.closure.copy().stages.CLOSURE_REQUESTED)), true);
    await show(); await wait(() => document.body.textContent.includes(globalThis.closure.copy().stages.FINANCIAL_CLEARANCE)); assert.deepEqual(await writes(), []);
  });
  await run('native retired reads do not block successors or clear a newer read, including confirmation cancellation', async () => {
    await fresh({ existing: true, native: true });
    await page.evaluate(() => { globalThis.closure.holdRead = true; }); await visibleTick();
    await wait(() => globalThis.closure.readHolds.length === 1);
    assert.equal(await page.evaluate(() => globalThis.closure.readHolds[0].signal), undefined);
    await hide(); const before = await readCount();
    await page.evaluate(() => { globalThis.closure.holdRead = false; globalThis.closure.receipt.stage = 'FINANCIAL_CLEARANCE'; });
    await show();
    await page.waitForFunction(before => globalThis.closure.calls.filter(call => call.method === 'GET').length === before + 1 && document.body.textContent.includes(globalThis.closure.copy().stages.FINANCIAL_CLEARANCE), {}, before);
    assert.equal(await page.evaluate(() => globalThis.closure.readHolds.length), 1);
    await page.evaluate(() => { globalThis.closure.holdRead = true; globalThis.closure.receipt.stage = 'CLOSURE_REQUESTED'; });
    await visibleTick(); await wait(() => globalThis.closure.readHolds.length === 2);
    await page.evaluate(() => { globalThis.closure.readHolds.shift().release(); });
    await page.evaluate(() => new Promise(resolve => { setTimeout(resolve, 50); }));
    await hide(); const successor = await readCount();
    await page.evaluate(() => { globalThis.closure.holdRead = false; globalThis.closure.receipt.stage = 'FINANCIAL_CLEARANCE'; });
    await show();
    await page.waitForFunction(successor => globalThis.closure.calls.filter(call => call.method === 'GET').length === successor + 1, {}, successor);
    await page.evaluate(() => { globalThis.closure.releaseReads(); });
    await wait(() => document.body.textContent.includes(globalThis.closure.copy().stages.FINANCIAL_CLEARANCE));
    await fresh({ native: true });
    await page.evaluate(() => { globalThis.closure.holdRead = true; }); await visibleTick();
    await wait(() => globalThis.closure.readHolds.length === 1); await click('start');
    await page.waitForSelector('.ap-closure-confirm-dialog input'); await page.keyboard.press('Escape');
    await wait(() => !document.querySelector('.ap-closure-confirm-dialog'));
    const cancelled = await readCount();
    await page.evaluate(() => { globalThis.closure.holdRead = false; globalThis.closure.receipt = { ...globalThis.closure.receiptTemplate, stage: 'FINANCIAL_CLEARANCE' }; });
    await page.waitForFunction(cancelled => globalThis.closure.calls.filter(call => call.method === 'GET').length === cancelled + 1 && document.body.textContent.includes(globalThis.closure.copy().stages.FINANCIAL_CLEARANCE), { timeout: 21000, polling: 50 }, cancelled);
    assert.equal(await page.evaluate(() => globalThis.closure.readHolds.length), 1);
    await page.evaluate(() => { globalThis.closure.releaseReads(); });
    await ready(); assert.deepEqual(await writes(), []);
  });
  await run('a hidden real Portal panel stops polling and returning refreshes before a retired GET settles', async () => {
    await fresh({ existing: true }); await panel(false); const before = await readCount();
    assert.equal(await page.evaluate(() => document.visibilityState), 'visible');
    await new Promise(resolve => setTimeout(resolve, 16000)); assert.equal(await readCount(), before);
    await page.evaluate(() => { globalThis.closure.receipt.stage = 'FINANCIAL_CLEARANCE'; }); await panel(true);
    await page.waitForFunction(before => globalThis.closure.calls.filter(call => call.method === 'GET').length === before + 1 && document.body.textContent.includes(globalThis.closure.copy().stages.FINANCIAL_CLEARANCE), {}, before);
    await page.evaluate(() => { globalThis.closure.holdRead = true; globalThis.closure.receipt.stage = 'CLOSURE_REQUESTED'; });
    await visibleTick(); await wait(() => globalThis.closure.readHolds.length === 1); await panel(false);
    await wait(() => globalThis.closure.readHolds[0].signal.aborted);
    const retired = await readCount();
    await page.evaluate(() => { globalThis.closure.holdRead = false; globalThis.closure.receipt.stage = 'FINANCIAL_CLEARANCE'; }); await panel(true);
    await page.waitForFunction(retired => globalThis.closure.calls.filter(call => call.method === 'GET').length === retired + 1, {}, retired);
    await page.evaluate(() => { globalThis.closure.releaseReads(); });
    await wait(() => document.body.textContent.includes(globalThis.closure.copy().stages.FINANCIAL_CLEARANCE)); assert.deepEqual(await writes(), []);
  });
  for (const change of ['owner', 'epoch-without-render', 'permission', 'unmount']) await run(`${change} retires an active GET before its late result can change the view`, async () => {
    await fresh();
    await page.evaluate(() => { globalThis.closure.receipt = { ...globalThis.closure.receiptTemplate }; globalThis.closure.holdRead = true; });
    await visibleTick(); await wait(() => globalThis.closure.readHolds.length === 1);
    await page.evaluate(change => {
      const c = globalThis.closure; c.holdRead = false; c.receipt = null;
      if (change === 'owner') { c.owner = '33333333-3333-4333-8333-333333333333'; c.render(); }
      if (change === 'epoch-without-render') c.client.financeSessionEpoch = (c.client.financeSessionEpoch || 0) + 1;
      if (change === 'permission') { c.deniedRead = true; c.render(); }
      if (change === 'unmount') c.unmount();
      c.releaseReads();
    }, change);
    if (change === 'owner') await ready();
    else await page.evaluate(() => new Promise(resolve => { setTimeout(resolve, 50); }));
    assert.equal(await page.evaluate(() => document.body.textContent.includes(globalThis.closure.copy().stages.CLOSURE_REQUESTED)), false);
    const stopped = await readCount(); await visibleTick();
    if (change === 'owner') {
      await page.waitForFunction(stopped => globalThis.closure.calls.filter(call => call.method === 'GET').length === stopped + 1, {}, stopped);
      assert.equal(await page.evaluate(() => document.body.textContent.includes(globalThis.closure.copy().stages.CLOSURE_REQUESTED)), false);
    } else assert.equal(await readCount(), stopped);
    assert.deepEqual(await writes(), []);
  });
  console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
} catch (error) { console.error(`FAIL after ${passed} PASS: ${error.stack}`); process.exitCode = 1; }
finally {
  if (browser) await browser.close(); if (vite) await vite.close();
  const absolute = path.resolve(profile); assert.equal(path.dirname(absolute), path.resolve(os.tmpdir())); assert.ok(path.basename(absolute).startsWith('pinvou-closure-poll-'));
  fs.rmSync(absolute, { recursive: true, force: false });
}
