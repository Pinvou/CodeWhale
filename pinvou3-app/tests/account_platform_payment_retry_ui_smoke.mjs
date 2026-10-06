// Actual React/client with controlled HTTP replies; not Billing/PG/provider acceptance.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const fixture = path.join(root, 'tests/fixtures/account-platform-ui.jsx').replaceAll('\\','/');
const chrome = [process.env.CHROME, process.env.EDGE, 'C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome'].find(value => value && fs.existsSync(value));
assert.ok(chrome, 'Real Chromium is required');
const owned = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-payment-retry-'));
let vite; let browser; let cleanupFailure; let passed = 0; const failures = [];
try {
  vite = await createServer({ configFile:false, root:path.join(root,'src'), cacheDir:path.join(owned,'vite-cache'), optimizeDeps:{ entries:[] }, plugins:[react(), {
    name:'payment-retry-fixture', configureServer(server) { server.middlewares.use(async (request,response,next) => {
      if (new URL(request.url,'http://127.0.0.1').pathname !== '/__account_review__') return next();
      try { response.setHeader('Content-Type','text/html; charset=utf-8'); response.end(await server.transformIndexHtml(request.url, `<html><body><div id="account-root"></div><script type="module" src="/@fs/${fixture}"></script></body></html>`)); } catch (error) { next(error); }
    }); },
  }], server:{ host:'127.0.0.1', port:0, fs:{ allow:[root] } } });
  await vite.listen(); const origin = `http://127.0.0.1:${vite.httpServer.address().port}`;
  browser = await puppeteer.launch({ executablePath:chrome, headless:true, userDataDir:path.join(owned,'profile'), args:['--no-sandbox','--disable-setuid-sandbox'] });
  for (const scenario of ['a10-personal-payment-retry','popover-a10-payment-retry','a10-personal-payment-empty','popover-a10-payment-empty','a10-personal-payment-foreign','popover-a10-payment-foreign','a10-personal-payment-changed','popover-a10-payment-changed','popover-a10-channel-missing']) {
    const page = await browser.newPage(); const errors=[]; const external=[];
    page.on('pageerror', error => errors.push(error.message)); await page.setRequestInterception(true);
    page.on('request', request => { const url=new URL(request.url()); if (url.origin===origin || url.protocol==='data:') void request.continue(); else { external.push(url.origin); void request.abort(); } });
    try {
      await page.goto(`${origin}/__account_review__?scenario=${scenario}&language=en`, { waitUntil:'networkidle0' });
      const popover=scenario.startsWith('popover-'); const scope=popover ? '[data-testid="account-platform-popover"]' : 'main';
      await page.waitForFunction(popover => popover ? document.querySelector('[data-testid="account-platform-popover"]')?.textContent.includes('Fixture User') : !!document.querySelector('nav'), {}, popover);
      await page.evaluate(popover => { const copy=window.__accountReview.copy; const elements=popover ? document.querySelectorAll('[data-testid="account-platform-popover"] button') : document.querySelectorAll('nav button'); [...elements].find(element => element.textContent.trim()===(popover ? copy.recharge : copy.sections.credits)).click(); }, popover);
      await page.waitForSelector(`${scope} select`);
      if (scenario.endsWith('channel-missing')) {
        assert.equal(await page.$$eval(`${scope} select option`, options => options.length),0);
        assert.equal(await page.evaluate(scope => [...document.querySelectorAll(`${scope} button`)].find(element => element.textContent.trim()===window.__accountReview.copy.createRecharge).disabled, scope),true);
      } else {
        const create=async () => page.evaluate(scope => [...document.querySelectorAll(`${scope} button`)].find(element => element.textContent.trim()===window.__accountReview.copy.createRecharge).click(), scope);
        await create(); await page.waitForSelector(`${scope} [role=alert]`);
        if (scenario.endsWith('changed')) await page.select(`${scope} select`, 'WECHAT');
        await create();
        await page.waitForFunction(() => window.__accountReview.calls.filter(item => item.path==='/api/token/v1/recharge/orders' && item.method==='POST').length===2);
        const calls=await page.evaluate(() => window.__accountReview.calls.filter(item => item.path==='/api/token/v1/recharge/orders' && item.method==='POST'));
        if (scenario.endsWith('changed')) { assert.notDeepEqual(calls[0].body,calls[1].body); assert.notEqual(calls[0].idempotencyKey,calls[1].idempotencyKey,'Changed payment facts must not borrow the old key'); }
        else { assert.deepEqual(calls[0].body,calls[1].body); assert.equal(calls[0].idempotencyKey,calls[1].idempotencyKey,'Exact unknown payment call must retain its logical key'); }
        await page.waitForSelector(`${scope} img`);
        if (popover) {
          await page.evaluate(() => [...document.querySelectorAll('[data-testid="account-platform-popover"] button')].find(element => element.title===window.__accountReview.copy.backToAccount).click());
          await page.evaluate(() => [...document.querySelectorAll('[data-testid="account-platform-popover"] button')].find(element => element.textContent.trim()===window.__accountReview.copy.recharge).click());
          await page.waitForSelector(`${scope} select`);
        }
        await page.waitForFunction(scope => ![...document.querySelectorAll(`${scope} button`)].find(element => element.textContent.trim()===window.__accountReview.copy.createRecharge)?.disabled, {}, scope);
        await create(); await page.waitForFunction(() => window.__accountReview.calls.filter(item => item.path==='/api/token/v1/recharge/orders' && item.method==='POST').length===3);
        const next=await page.evaluate(() => window.__accountReview.calls.filter(item => item.path==='/api/token/v1/recharge/orders' && item.method==='POST').at(-1));
        assert.notEqual(next.idempotencyKey,calls[1].idempotencyKey,'Confirmed call must release its key for a deliberate new call');
      }
      if (popover) assert.equal(await page.evaluate(() => document.querySelector('[data-testid="account-platform-popover"]').textContent.includes(window.__accountReview.ids.user)),false,'Internal user identity is used privately, never rendered');
      assert.deepEqual(errors,[]); assert.deepEqual(external,[]); passed+=1; console.log(`PASS ${scenario}`);
    } catch (error) { failures.push(scenario); console.error(`FAIL ${scenario}: ${error.message}`); } finally { await page.close(); }
  }
  console.log(`Payment retry UI: ${passed} PASS, ${failures.length} FAIL, 0 SKIP. Controlled HTTP; no Billing/PG/provider.`);
  assert.equal(failures.length,0);
} finally {
  const cleanup=await Promise.allSettled([browser?.close(),vite?.close()]);
  assert.ok(path.resolve(owned).startsWith(`${path.resolve(os.tmpdir())}${path.sep}`) && path.basename(owned).startsWith('pinvou-payment-retry-'));
  fs.rmSync(owned,{ recursive:true, force:true, maxRetries:3, retryDelay:100 });
  cleanupFailure=cleanup.find(result => result.status==='rejected')?.reason;
}
if (cleanupFailure) throw cleanupFailure;
