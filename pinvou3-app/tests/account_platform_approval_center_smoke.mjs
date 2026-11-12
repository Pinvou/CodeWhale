import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import {createServer} from './helpers/vite-fixture.mjs';
import {approvalReadCopy, pendingApprovalReadCopy} from '../src/shared/i18n.js';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const chrome=process.env.CHROME || ['C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe','C:/Program Files/Microsoft/Edge/Application/msedge.exe','C:/Program Files/Google/Chrome/Application/chrome.exe','C:/Program Files (x86)/Google/Chrome/Application/chrome.exe','/snap/bin/chromium','/usr/bin/chromium','/usr/bin/chromium-browser','/usr/bin/google-chrome','/usr/bin/google-chrome-stable','/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(fs.existsSync);
assert.ok(chrome,'Browser unavailable; zero UI cases is not passing');
const profile=fs.mkdtempSync(path.join(os.tmpdir(),'f02-approval-center-'));
let vite,browser,page,passed=0,failed=0;
const errors=[];
try {
 vite=await createServer({root,configFile:false,appType:'mpa',logLevel:'error',plugins:[react()],server:{host:'127.0.0.1',port:0,watch:null,fs:{allow:[root,fs.realpathSync(path.join(root,'node_modules'))],deny:[]}}});await vite.listen();
 browser=await puppeteer.launch({executablePath:chrome,headless:'new',userDataDir:profile,args:['--disable-gpu','--no-first-run']});page=await browser.newPage();page.setDefaultTimeout(5000);page.on('pageerror',e=>errors.push(e.message));
 const url=`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-approval-center.html`;
 const settled=async()=>page.waitForFunction(()=>!document.querySelector('[role=status]'));
 const click=async(pathname)=>{const found=await page.evaluate(pathname=>{const c=window.approvalCenter;const text=c.financeCopy()[pathname] || pathname.split('.').reduce((v,k)=>v?.[k],c.copy()) || pathname;const button=[...document.querySelectorAll('button')].find(b=>b.textContent.trim()===text&&!b.disabled&&b.getClientRects().length);if(!button)return false;button.click();return true;},pathname);assert.ok(found,`Missing enabled control: ${pathname}`);};
 const fresh=async(language='en')=>{errors.length=0;await page.goto(`${url}?language=${language}&section=users`);await page.waitForFunction(()=>window.approvalCenter);await settled();};
 const enter=async()=>{await page.click('.adm-sidebar [data-section=approvals]');await page.waitForSelector('.adm-approval-workspace');await page.waitForSelector('tbody tr');await settled();};
 const run=async(name,fn)=>{try{await fn();assert.deepEqual(errors,[]);passed++;console.log(`PASS ${name}`);}catch(e){failed++;console.log(`FAIL ${name}: ${e.message}`);}};
 for(const language of ['en','zh','ja']) await run(`${language} real host mine + current EDIT/SUBMIT`,async()=>{await fresh(language);await enter();assert.ok(await page.evaluate(()=>window.approvalCenter.calls.some(c=>c.url.includes(`requester_user_id=${window.approvalCenter.id}`))));await click('details');await page.waitForSelector('article');assert.ok(await page.evaluate(()=>[...document.querySelectorAll('article button')].some(b=>b.textContent===window.approvalCenter.financeCopy().editSubmit)));});
 await run('pending excludes self, execution filter uses actual reader',async()=>{await fresh();await enter();await click('approvalCenter.pending');await settled();await page.waitForFunction(()=>document.querySelector('.adm-approval-workspace tbody')?.textContent.includes(window.approvalCenter.other));assert.ok(await page.evaluate(()=>{const rows=[...document.querySelectorAll('.adm-approval-workspace tbody tr')];return rows.length===1&&rows[0].children[3].textContent===window.approvalCenter.other;}));await click('approvalCenter.execution');await settled();await page.waitForFunction(()=>window.approvalCenter.calls.some(c=>c.url.includes('status=EXECUTION_FAILED')));});
 await run('command target status filter survives expired cursor',async()=>{await fresh();await enter();await page.select('.adm-approval-toolbar select','WALLET_ADJUSTMENT');await page.type('.adm-approval-toolbar input','controlled-target');await page.select('.adm-approval-toolbar label:last-of-type select','SUBMITTED');await settled();await page.click('.adm-approval-toolbar button');await page.waitForFunction(()=>window.approvalCenter.calls.some(c=>c.url.includes('target=controlled-target')&&c.url.includes('command_type=WALLET_ADJUSTMENT')));await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.approvalCenter.financeCopy().next&&!b.disabled)&&!document.querySelector('[role=status]'));await page.evaluate(()=>{window.approvalCenter.expired=true;});await click('next');await settled();await page.waitForFunction(()=>window.approvalCenter.calls.some(c=>c.url.includes('cursor=controlled-next')));const calls=await page.evaluate(()=>window.approvalCenter.calls.filter(c=>c.url.includes('/approvals/requests?')).slice(-2).map(c=>new URL(c.url,location.origin).searchParams.toString()));for(const query of calls){const params=new URLSearchParams(query);assert.equal(params.get('command_type'),'WALLET_ADJUSTMENT');assert.equal(params.get('target'),'controlled-target');assert.equal(params.get('status'),'SUBMITTED');}assert.equal(new URLSearchParams(calls[1]).get('cursor'),null);});
 await run('directory invokes production capability client, unavailable is explicit',async()=>{await fresh();await enter();await click('approvalCenter.directory');await page.waitForFunction(()=>document.querySelector('.adm-approval-workspace tbody')?.textContent.includes('PAYMENT_PROVIDER_RESUME'));assert.ok(await page.evaluate(()=>window.approvalCenter.calls.some(c=>c.url.endsWith('/approvals/directory'))));await page.evaluate(()=>{window.approvalCenter.brokenDirectory=true;});await click('approvalCenter.refresh');await page.waitForSelector('[role=alert]');assert.equal(await page.$('.adm-approval-workspace tbody'),null);});
 await run('directory excludes false or missing visibility and future commands after a real directory read', async () => {
  await fresh(); await enter();
  await page.evaluate(() => { window.approvalCenter.visibilityCases = true; });
  await click('approvalCenter.directory');
  await page.waitForSelector('.adm-approval-directory-toolbar');
  await page.waitForFunction(() => document.querySelector('.adm-approval-policy-table')?.textContent.includes('PAYMENT_PROVIDER_RESUME'));
  assert.equal(await page.evaluate(() => window.approvalCenter.calls.some(call => call.url.endsWith('/approvals/directory'))), true);
  const policies = await page.$eval('.adm-approval-policy-table', table => table.textContent);
  assert.ok(policies.includes('WALLET_ADJUSTMENT'));
  for (const hidden of ['PAYMENT_PRODUCT_RESUME', 'LEDGER_REPAIR', 'UNKNOWN_REPAIR']) assert.equal(policies.includes(hidden), false, hidden);
  assert.equal(await page.$$eval('.adm-approval-policy-table tbody tr', rows => rows.length), 2);
 });
 await run('business drilldown reaches current wallet panel',async()=>{await fresh();await enter();await click('details');await page.waitForSelector('article');await click('approvalCenter.openBusinessArea');await page.waitForFunction(()=>window.approvalCenter.calls.some(c=>c.url.includes('/admin/v1/wallets?')));});
 await run('no approval permission has no center caller',async()=>{await fresh();await page.evaluate(()=>{window.approvalCenter.permission=false;window.approvalCenter.mount();});await page.waitForFunction(()=>!document.querySelector('.adm-sidebar [data-section=approvals]'));assert.ok(await page.evaluate(()=>![...document.querySelectorAll('button')].some(b=>b.textContent===window.approvalCenter.copy().nav.approvals)));assert.ok(await page.evaluate(()=>!window.approvalCenter.calls.some(c=>c.url.includes('/approvals/'))));});
 await run('unknown submit retains original expected version and current action control',async()=>{await fresh();await enter();await click('details');await page.waitForSelector('article');await page.evaluate(()=>{window.approvalCenter.unknown=true;});await click('submit');await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.approvalCenter.financeCopy().retry&&!b.disabled));assert.ok(await page.evaluate(()=>[...document.querySelectorAll('.adm-approval-tabs button')].every(b=>b.disabled)));await page.evaluate(()=>{window.approvalCenter.unknown=false;});await click('retry');await page.waitForFunction(()=>window.approvalCenter.calls.filter(c=>c.url.endsWith('/actions/submit')).length===2);const calls=await page.evaluate(()=>window.approvalCenter.calls.filter(c=>c.url.endsWith('/actions/submit')));assert.equal(calls[0].body,calls[1].body);assert.equal(JSON.parse(calls[1].body).expected_version,1);assert.equal(calls[1].headers['X-Pinvou-Approval-Action-Control-Version'],'1');assert.equal(calls[1].headers['X-CSRF-Token'],'SYNTHETIC_ONLY_CSRF');});
 await run('mine transient failure preserves accepted rows; retry malformed and foreign collections safely',async()=>{
  await fresh();await enter();
  for(const invalidCollection of [false,'object','foreign']) {
   await page.evaluate(value=>{window.approvalCenter.listStatus=value?0:503;window.approvalCenter.invalidCollection=value;},invalidCollection);
   await click(approvalReadCopy.en.refreshRead);await settled();await page.waitForSelector('.adm-approval-workspace [role=alert]');
   assert.equal(await page.$$('.adm-approval-workspace tbody tr').then(rows=>rows.length),1,'failed current-scope reads preserve the accepted first page');
   assert.ok(await page.$('.adm-approval-previous'));
   await page.evaluate(()=>{window.approvalCenter.listStatus=0;window.approvalCenter.invalidCollection=false;});
   await click(approvalReadCopy.en.retryRead);await settled();await page.waitForSelector('.adm-approval-workspace tbody tr');
   assert.equal(await page.$('.adm-approval-workspace [role=alert]'),null);
  }
 });
 for(const status of [401,403]) await run(`mine ${status} clears accepted facts and detail`,async()=>{
  await fresh();await enter();await click('details');await page.waitForSelector('.adm-approval-workspace article');
  await page.evaluate(status=>{window.approvalCenter.listStatus=status;},status);await click(approvalReadCopy.en.refreshRead);await settled();
  await page.waitForSelector('.adm-approval-workspace [role=alert]');
  assert.equal(await page.$('.adm-approval-workspace tbody'),null);assert.equal(await page.$('.adm-approval-workspace article'),null);
 });
 await run('failed continuation retries same cursor; identical query explicitly resets to first page',async()=>{
  await fresh();await enter();await page.evaluate(()=>{window.approvalCenter.cursorStatus=503;});await click('next');await settled();
  await page.waitForSelector('.adm-approval-workspace [role=alert]');assert.equal(await page.$$('.adm-approval-workspace tbody tr').then(rows=>rows.length),1);
  await page.evaluate(()=>{window.approvalCenter.cursorStatus=0;});await click(approvalReadCopy.en.retryRead);await settled();
  const continuation=await page.evaluate(()=>window.approvalCenter.calls.filter(c=>c.url.includes('/approvals/requests?')).slice(-2).map(c=>c.url));
  assert.equal(new URL(continuation[0],url).searchParams.get('cursor'),'controlled-next');assert.equal(continuation[0],continuation[1]);
  const before=await page.evaluate(()=>window.approvalCenter.calls.length);await page.click('.adm-approval-query');
  await page.waitForFunction(before=>window.approvalCenter.calls.slice(before).some(c=>c.url.includes('/approvals/requests?')&&!new URL(c.url,location.origin).searchParams.has('cursor')),{},before);await settled();
 });
 await run('repeated current mine tab preserves URL, applied controls and unsubmitted draft',async()=>{
  await fresh();await enter();await page.select('.adm-approval-toolbar label:last-of-type select','SUBMITTED');await page.click('.adm-approval-query');await settled();
  await page.type('.adm-approval-toolbar input','unsubmitted-target');const before=await page.evaluate(()=>({href:location.href,count:window.approvalCenter.calls.length}));
  await click('approvalCenter.mine');assert.equal(await page.url(),before.href);assert.equal(await page.$eval('.adm-approval-toolbar input',input=>input.value),'unsubmitted-target');
  assert.equal(await page.$eval('.adm-approval-toolbar label:last-of-type select',select=>select.value),'SUBMITTED');
  assert.equal(await page.evaluate(()=>window.approvalCenter.calls.length),before.count,'current-tab click must not request different facts');
 });
 await run('pending date range rejects reversed interval then sends exact UTC half-open bounds',async()=>{
  await fresh();await enter();await click('approvalCenter.pending');await settled();
  const setDates=async(from,to)=>page.$$eval('.adm-approval-toolbar input[type=date]',(inputs,values)=>{const setter=Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set;inputs.forEach((input,i)=>{setter.call(input,values[i]);input.dispatchEvent(new Event('input',{bubbles:true}));});},[from,to]);
  const before=await page.evaluate(()=>window.approvalCenter.calls.length);await setDates('2026-11-12','2026-11-11');await page.click('.adm-approval-query');
  await page.waitForFunction(text=>document.querySelector('.adm-approval-expiry-help [role=alert]')?.textContent===text,{},pendingApprovalReadCopy.en.invalidExpiry);
  assert.equal(await page.evaluate(()=>window.approvalCenter.calls.length),before);
  await setDates('2026-11-11','2026-11-12');await page.click('.adm-approval-query');await settled();
  const query=new URL(await page.evaluate(()=>window.approvalCenter.calls.filter(c=>c.url.includes('/approvals/requests?')).at(-1).url),url).searchParams;
  assert.equal(query.get('expires_from'),'2026-11-11T00:00:00Z');assert.equal(query.get('expires_to'),'2026-11-12T00:00:00Z');assert.equal(query.get('status'),'SUBMITTED');
 });
 console.log(`SUMMARY ${passed} PASS ${failed} FAIL 0 SKIP`);if(failed)process.exitCode=1;
} finally {await browser?.close();await vite?.close();fs.rmSync(profile,{recursive:true,force:true});}
