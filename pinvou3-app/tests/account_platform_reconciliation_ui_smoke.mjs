import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/snap/bin/chromium', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; no UI validation performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-f03-ui-'));
let vite; let browser; let page; let passed = 0;
const errors = [];
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  page = await browser.newPage(); page.setDefaultTimeout(10000); page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => { if (message.type() === 'error') errors.push(message.text()); });
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-reconciliation.html`;
  const fresh = async language => { errors.length = 0; await page.goto(url); await page.waitForFunction(() => window.f03); await page.evaluate(language => { window.f03.language = language; window.f03.render(); }, language); await page.waitForSelector('tbody tr'); };
  const click = async key => { assert.ok(await page.evaluate(key => { const text = window.f03.copy()[key]; const button = [...document.querySelectorAll('button')].find(value => value.textContent === text && !value.disabled); if (!button) return false; button.click(); return true; }, key), `Missing action ${key}`); };
  const selectCase = async () => { await click('cases'); await page.waitForFunction(() => document.querySelector('tbody')?.textContent.includes('controlled-merchant')); await page.evaluate(() => document.querySelector('tbody button').click()); await page.waitForSelector('details'); };
  const caseRun = async (name, action) => { await action(); assert.deepEqual(errors, []); passed++; console.log(`PASS ${name}`); };
  for (const language of ['zh', 'en', 'ja']) {
    await caseRun(`${language}: missing supplier occurrence dates remain explicit manual review cases`, async () => {
      await fresh(language); await page.evaluate(() => { window.f03.caseType = 'REFUND_DATE_UNVERIFIED'; }); await selectCase();
      assert.ok(await page.evaluate(() => document.querySelector('tbody').textContent.includes(window.f03.copy().caseTypes.REFUND_DATE_UNVERIFIED) && document.querySelector('section section h3').textContent.includes(window.f03.copy().caseTypes.REFUND_DATE_UNVERIFIED)));
      await fresh(language); await page.evaluate(() => { window.f03.caseType = 'CHARGEBACK_DATE_UNVERIFIED'; }); await selectCase();
      assert.ok(await page.evaluate(() => document.querySelector('tbody').textContent.includes(window.f03.copy().caseTypes.CHARGEBACK_DATE_UNVERIFIED)));
    });
    await caseRun(`${language}: financial read errors keep permission and input attribution`, async () => {
      await fresh(language);
      for (const [status, key] of [[403, 'permission'], [400, 'sourceChanged']]) {
        await page.evaluate(status => { window.f03.readStatus = status; }, status);
        await click('refresh'); await page.waitForSelector('[role=alert]');
        assert.equal(await page.$eval('[role=alert]', el => el.textContent), await page.evaluate(key => window.f03.copy()[key], key));
      }
    });
    await caseRun(`${language}: read-only differences retain exact financial facts`, async () => {
      await fresh(language); await page.evaluate(() => { window.f03.readonly = true; window.f03.switchIdentity(); }); await page.waitForSelector('tbody tr'); await selectCase();
      assert.ok(await page.evaluate(() => document.querySelector('details').textContent.includes('9007199254740993')));
      assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].some(value => [window.f03.copy().submit, window.f03.copy().run, window.f03.copy().import].includes(value.textContent))), false);
    });
  }
  await caseRun('Changing filters retires the old cursor and RUNNING null completion remains visible', async () => {
    await fresh('en'); await page.evaluate(() => { window.f03.paginated = true; window.f03.running = true; }); await click('refresh');
    await page.waitForFunction(() => document.querySelector('tbody')?.textContent.includes('RUNNING')); await click('next');
    await page.waitForFunction(() => window.f03.calls.some(call => call.url.includes('cursor=controlled-page-2')));
    await page.waitForFunction(() => [...document.querySelectorAll('button')].every(button => button.textContent !== window.f03.copy().refresh || !button.disabled));
    await page.select('select', 'ALIPAY');
    await page.waitForFunction(() => window.f03.calls.some(call => call.url.includes('channel=ALIPAY')));
    assert.ok(await page.evaluate(() => window.f03.calls.filter(call => call.url.includes('channel=ALIPAY')).every(call => !call.url.includes('controlled-page-2'))));
  });
  for (const language of ['zh', 'en', 'ja']) {
    await caseRun(`${language}: daily runs use half-open dates and manual unknown results require reading batches`, async () => {
      await fresh(language);
      assert.ok(await page.evaluate(() => {
        const query = new URL(window.f03.calls.find(value => value.url.includes('/reconciliation-runs?')).url, location.origin).searchParams;
        const from = document.querySelector('input[type=date]').value;
        const next = new Date(`${from}T00:00:00Z`); next.setUTCDate(next.getUTCDate() + 1);
        return query.get('business_date_from') === from && query.get('business_date_to') === next.toISOString().slice(0, 10);
      }));
      await page.evaluate(() => { window.f03.unknownRun = true; });
      for (const action of ['run', 'import']) {
        if (action === 'import') await page.evaluate(() => {
          const field = document.querySelector('textarea');
          const set = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set;
          set.call(field, '[{"entry_type":"PAYMENT","amount_fen":1}]'); field.dispatchEvent(new Event('input', { bubbles: true }));
        });
        await click(action);
        await page.waitForFunction(() => document.querySelector('[role=alert]')?.textContent === window.f03.copy().runUnknown);
        assert.notEqual(await page.evaluate(() => document.querySelector('[role=alert]').textContent), await page.evaluate(() => window.f03.copy().unknown));
      }
      await click('refresh');
      assert.ok(await page.evaluate(() => {
        const writes = window.f03.calls.filter(value => value.method === 'POST' && !value.url.endsWith('/commands'));
        return writes.length === 2 && writes.every(value => !value.key) && window.f03.calls.filter(value => value.url.includes('/reconciliation-runs?')).length === 2;
      }));
    });
  }
  await caseRun('Unknown command preserves actor resource facts and key across language changes', async () => {
    await fresh('en'); await selectCase(); await page.type('section section textarea', 'Controlled reconciliation claim reason'); await page.evaluate(() => { window.f03.unknown = true; }); await click('submit'); await page.waitForSelector('[role=alert]');
    assert.ok(await page.evaluate(() => [...document.querySelectorAll('section section input, section section textarea, section section select')].every(value => value.disabled)));
    await page.evaluate(() => { window.f03.language = 'ja'; window.f03.unknown = false; window.f03.render(); }); await click('retry'); await page.waitForFunction(() => document.querySelector('[role=status]')?.textContent === window.f03.copy().saved);
    assert.ok(await page.evaluate(() => { const values = window.f03.calls.filter(value => value.url.endsWith('/commands')); return values.length === 2 && values[0].key === values[1].key && values[0].body === values[1].body; }));
  });
  await caseRun('Wrong successful case ACK remains unknown and same-key retryable', async () => {
    await fresh('en'); await selectCase(); await page.type('section section textarea', 'Controlled reconciliation claim reason'); await page.evaluate(() => { window.f03.wrong = true; }); await click('submit'); await page.waitForSelector('[role=alert]');
    assert.ok(await page.evaluate(() => document.querySelector('[role=alert]').textContent === window.f03.copy().unknown));
    await page.evaluate(() => { window.f03.wrong = false; }); await click('retry'); await page.waitForFunction(() => document.querySelector('[role=status]')?.textContent === window.f03.copy().saved);
  });
  await caseRun('Late old-owner case completion cannot update the next identity', async () => {
    await fresh('en'); await selectCase(); await page.type('section section textarea', 'Controlled reconciliation claim reason'); await page.evaluate(() => { window.f03.delayed = true; }); await click('submit'); await page.waitForFunction(() => typeof window.f03.release === 'function');
    await page.evaluate(() => { window.f03.switchIdentity(); }); await page.waitForFunction(() => !document.querySelector('details')); await page.evaluate(() => window.f03.release());
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('[role=status]')].some(value => value.textContent === window.f03.copy().saved)), false);
  });
  let repairFailures=0;
  const repairRun=async (name, action) => { try { await caseRun(name,action); } catch(error) { repairFailures++;console.error(`FAIL ${name}: ${error.message}`); } };
  for (const language of ['zh','en','ja']) {
    await repairRun(`${language}: current repair all producer types are localized in list and detail`, async()=> {
      for(const type of ['UNSUPPORTED_PROVIDER_ENTRY','DUPLICATE_TRANSACTION','INTERNAL_PAID_PROVIDER_MISSING','INTERNAL_REFUND_PROVIDER_MISSING','INTERNAL_CHARGEBACK_PROVIDER_MISSING','AMOUNT_MISMATCH','REFUND_AMOUNT_MISMATCH','PAYMENT_LEDGER_INVALID','REFUND_LEDGER_INVALID','CHARGEBACK_LEDGER_INVALID','PAYMENT_PACKAGE_INVALID','PAYMENT_LEDGER_MISSING','REFUND_LEDGER_MISSING','CHARGEBACK_LEDGER_MISSING','PROVIDER_PAID_INTERNAL_MISSING','PROVIDER_REFUND_INTERNAL_MISSING','PROVIDER_CHARGEBACK_INTERNAL_MISSING','REFUND_DATE_UNVERIFIED','CHARGEBACK_DATE_UNVERIFIED','FUTURE_PROVIDER_EVIDENCE']) {
        await fresh(language);await page.evaluate(type=>{window.f03.caseType=type;},type);await selectCase();
        const actual=await page.evaluate(type=>({label:window.f03.copy().caseTypes[type],table:document.querySelector('tbody').textContent,detail:document.querySelector('section section h3').textContent}),type);
        if(type==='FUTURE_PROVIDER_EVIDENCE') {assert.ok(actual.table.includes(type));assert.ok(actual.detail.includes(type));}
        else {assert.ok(actual.label,`Missing ${type}`);assert.notEqual(actual.label,type);assert.ok(actual.table.includes(actual.label));assert.ok(actual.detail.includes(actual.label));}
      }
    });
    await repairRun(`${language}: current repair validates HTTP input codes without hiding state conflicts`,async()=>{
      await fresh(language);
      for(const [status,code,key] of [[400,'INVALID_FILTER','invalid'],[400,'INVALID_REQUEST','invalid'],[400,'PAGINATION_CURSOR_INVALID','sourceChanged'],[400,'UNRECOGNIZED_ERROR','sourceChanged'],[403,'PERMISSION_DENIED','permission'],[503,'AUTHORIZATION_UNAVAILABLE','unavailable']]) {
        await page.evaluate(value=>{window.f03.readStatus=value.status;window.f03.readCode=value.code;},{status,code});await click('refresh');
        await page.waitForFunction(key=>document.querySelector('[role=alert]')?.textContent===window.f03.copy()[key],{},key);
      }
      for(const [status,code,key] of [[400,'INVALID_REQUEST','invalid'],[409,'BILLING_CONFLICT','sourceChanged'],[503,'COMMAND_RESULT_UNKNOWN','runUnknown']]) {
        await fresh(language);await page.evaluate(value=>{window.f03.writeStatus=value.status;window.f03.writeCode=value.code;},{status,code});await click('run');
        await page.waitForFunction(key=>document.querySelector('[role=alert]')?.textContent===window.f03.copy()[key],{},key);
      }
    });
    await repairRun(`${language}: current repair case input rejects remain editable and unknown writes keep the key`,async()=>{
      await fresh(language);await selectCase();await page.type('section section textarea','Controlled reconciliation claim reason');
      await page.evaluate(()=>{window.f03.commandStatus=400;window.f03.commandCode='INVALID_REQUEST';});await click('submit');
      await page.waitForFunction(()=>document.querySelector('[role=alert]')?.textContent===window.f03.copy().invalid);
      assert.equal(await page.evaluate(()=>document.querySelector('section section textarea').disabled),false);
      await page.evaluate(()=>{window.f03.commandStatus=503;window.f03.commandCode='COMMAND_RESULT_UNKNOWN';});await click('submit');
      await page.waitForFunction(()=>document.querySelector('[role=alert]')?.textContent===window.f03.copy().unknown);
      const unresolved=await page.evaluate(()=>window.f03.calls.filter(value=>value.url.endsWith('/commands')).at(-1));
      await page.evaluate(()=>{window.f03.commandStatus=400;window.f03.commandCode='INVALID_REQUEST';});await click('retry');
      await page.waitForFunction(()=>document.querySelector('[role=alert]')?.textContent===window.f03.copy().unknown);
      await page.evaluate(()=>{window.f03.commandStatus=0;});await click('retry');
      await page.waitForFunction(()=>document.querySelector('[role=status]')?.textContent===window.f03.copy().saved);
      const calls=await page.evaluate(()=>window.f03.calls.filter(value=>value.url.endsWith('/commands')).slice(-3));
      assert.ok(calls.every(value=>value.key===unresolved.key && value.body===unresolved.body));
    });
  }
  for(const language of ['zh','en','ja']) {
    await repairRun(`${language}: current repair scoped cases capacity requests narrower filters without declaring valid input invalid`,async()=>{
      await fresh(language);await selectCase();
      await page.evaluate(()=>{window.f03.readStatus=422;window.f03.readCode='PAGINATION_FILTER_REQUIRED';});await click('refresh');
      await page.waitForFunction(()=>document.querySelector('[role=alert]')?.textContent===window.f03.copy().narrowFilters);
      assert.equal(await page.evaluate(()=>[...document.querySelectorAll('input')].some(value=>value.disabled)),false);
      assert.ok(await page.evaluate(()=>window.f03.calls.at(-1).url.includes('/reconciliation-cases?')));
      await page.evaluate(()=>{window.f03.readCode='UNRECOGNIZED_ERROR';});await click('refresh');
      await page.waitForFunction(()=>document.querySelector('[role=alert]')?.textContent===window.f03.copy().unavailable);
    });
  }
  if(repairFailures) throw new Error(`${repairFailures} current repair cases failed`);
  console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
} catch (error) { console.error(`FAIL after ${passed} PASS: ${error.message}`); process.exitCode = 1; }
finally { if (browser) await browser.close(); if (vite) await vite.close(); fs.rmSync(profile, { recursive: true, force: true }); }
