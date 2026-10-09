import assert from 'node:assert/strict';
import fs from 'node:fs';
import {createServer} from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
const server=await createServer({root:import.meta.dirname+'/..',configFile:false,plugins:[react()],cacheDir:import.meta.dirname+'/../node_modules/.vite-ledger-test',server:{host:'127.0.0.1',port:0,fs:{allow:[import.meta.dirname+'/..',fs.realpathSync(import.meta.dirname+'/../node_modules')],deny:['**/.env*','**/*.{pem,crt,key}']}}});await server.listen();
const browser=await puppeteer.launch({executablePath:'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',headless:true,args:['--no-sandbox','--disable-gpu']});
let pass=0,fail=0;const results=[];
const run=async(name,fn)=>{const page=await browser.newPage();page.setDefaultTimeout(5000);const errors=[];page.on('pageerror',e=>errors.push(e.message));try{await fn(page);assert.deepEqual(errors,[]);pass++;results.push({name,pass:true});}catch(e){fail++;results.push({name,pass:false,error:e.message,diagnostic:await page.evaluate(()=>({text:document.body.innerText,calls:window.f10?.calls})).catch(()=>null)});}finally{await page.close();}};
try{
 for(const language of ['zh','en','ja'])await run('production finance tab / lossless precheck / draft / submit '+language,async page=>{
  await page.goto(server.resolvedUrls.local[0]+`tests/account_platform_ledger_repair_fixture.html?language=${language}${language==='en'?'&lost=1':''}`);
  await page.waitForFunction(()=>window.f10 && [...document.querySelectorAll('nav button')].some(b=>b.textContent===window.f10.ledgerRepairCopy[window.f10.language].title));
  await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(b=>b.textContent===window.f10.ledgerRepairCopy[window.f10.language].title).click());
  await page.waitForSelector('.adm-financial-repair-target');
  const values=await page.evaluate(()=>[window.f10.id(1),window.f10.id(2),window.f10.id(3),window.f10.id(4),'9223372036854775807']);const inputs=await page.$$('.adm-financial-repair-target input');for(let i=0;i<inputs.length;i++)await inputs[i].type(values[i]);
  await page.click('.adm-financial-repair-target button');await page.waitForSelector('.adm-financial-repair-draft');
  assert.ok(await page.evaluate(()=>{const c=window.f10.calls.find(c=>c.url==='/api/admin/account/admin/v2/approval-command-prechecks');return c.body.includes('"amount_microcredits":9223372036854775807')&&JSON.parse(c.body).reason_code==='ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT'&&c.headers['X-CSRF-Token']==='controlled-csrf';}));
  await page.type('.adm-financial-repair-draft textarea','Controlled ledger correction justification');await page.type('.adm-financial-repair-draft input','controlled-ledger-key');await page.click('.adm-financial-repair-draft button');
  if(language==='en'){await page.waitForSelector('.adm-financial-repair [role=alert]');await page.waitForFunction(()=>!document.querySelector('.adm-financial-repair-draft button').disabled);await page.click('.adm-financial-repair-draft button');}
  await page.waitForFunction(()=>document.body.textContent.includes(window.f10.id(9))&&!document.querySelector('.adm-financial-repair'));
  assert.ok(await page.evaluate(()=>window.f10.calls.filter(c=>c.url==='/api/admin/approvals/requests'&&c.method==='POST').every(c=>c.body.includes('"amount_microcredits":9223372036854775807')&&JSON.parse(c.body).idempotency_key==='approval:controlled-ledger-key')));
  await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.f10.financeCopy[window.f10.language].submit).click());await page.waitForFunction(()=>window.f10.status==='SUBMITTED');
 });
 await run('repair-only center directory, current detail, typed EDIT and business drilldown',async page=>{
  await page.goto(server.resolvedUrls.local[0]+'tests/account_platform_ledger_repair_fixture.html?language=en');
  await page.waitForSelector('.adm-financial-repair');
  await page.evaluate(()=>{window.f10.created=true;});
  await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.f10.adminPlatformCopy.en.nav.approvals).click());
  await page.waitForSelector('.adm-approval-workspace tbody tr');
  await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.f10.adminPlatformCopy.en.approvalCenter.directory).click());
  await page.waitForFunction(()=>document.querySelector('tbody')?.textContent.includes('LEDGER_REPAIR'));
  assert.ok(await page.evaluate(()=>window.f10.calls.some(c=>c.url.endsWith('/approvals/directory'))));
  await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.f10.adminPlatformCopy.en.approvalCenter.mine).click());
  await page.waitForSelector('.adm-approval-workspace tbody tr button');await page.click('.adm-approval-workspace tbody tr button');await page.waitForSelector('article textarea');
  await page.type('article textarea','Updated controlled ledger attribution reason');
  await page.evaluate(()=>[...document.querySelectorAll('article button')].find(b=>b.textContent===window.f10.financeCopy.en.editSubmit).click());
  await page.waitForFunction(()=>window.f10.status==='SUBMITTED');
  const calls=await page.evaluate(()=>window.f10.calls);
  assert.ok(calls.some(c=>c.url.endsWith('/admin/v2/approval-command-prechecks')));
  const update=calls.find(c=>c.method==='PUT');assert.ok(update.body.includes('\"amount_microcredits\":9223372036854775807'));assert.equal(JSON.parse(update.body).expected_version,1);
  await page.waitForFunction(()=>[...document.querySelectorAll('article button')].some(b=>b.textContent===window.f10.adminPlatformCopy.en.approvalCenter.openBusinessArea&&!b.disabled));
  await page.evaluate(()=>[...document.querySelectorAll('article button')].find(b=>b.textContent===window.f10.adminPlatformCopy.en.approvalCenter.openBusinessArea).click());
  await page.waitForSelector('.adm-financial-repair');
  assert.ok(await page.evaluate(()=>window.f10.calls.every(c=>!c.url.includes('/admin/v1/wallets')&&!c.url.includes('/admin/v1/payment-orders')&&!c.url.includes('/admin/v1/adjustments'))));
 });
 await run('repair approver only enters approvals without draft or finance reads',async page=>{
  await page.goto(server.resolvedUrls.local[0]+'tests/account_platform_ledger_repair_fixture.html?language=en&approve-only=1');
  await page.waitForFunction(()=>window.f10?.calls.some(c=>c.url.includes('/approvals/requests?')));
  assert.equal(await page.$('.adm-financial-repair'),null);
  assert.ok(await page.evaluate(()=>window.f10.calls.every(c=>!c.url.includes('/admin/v1/wallets')&&!c.url.includes('/admin/v1/payment-orders'))));
 });
 await run('permission denied hides installed Ledger form',async page=>{await page.goto(server.resolvedUrls.local[0]+'tests/account_platform_ledger_repair_fixture.html?deny=1');await page.waitForFunction(()=>window.f10 && document.body.textContent.includes(window.f10.adminPlatformCopy.zh.permissionDenied));assert.equal(await page.$('.adm-financial-repair'),null);assert.equal(await page.evaluate(()=>[...document.querySelectorAll('nav button')].some(b=>b.textContent===window.f10.ledgerRepairCopy.zh.title)),false);});
}finally{await browser.close();await server.close();}
const result={pass,fail,results,limits:['Actual AdminPlatformView/React/AdminPlatformClient with controlled BFF responses; no real issuer, PostgreSQL approval, canonical production build or full E2E.']};fs.writeFileSync(process.env.F10_REACT_RESULT || 'ledger-smoke-result.json',JSON.stringify(result,null,2));console.log(JSON.stringify(result));process.exitCode=fail?1:0;
