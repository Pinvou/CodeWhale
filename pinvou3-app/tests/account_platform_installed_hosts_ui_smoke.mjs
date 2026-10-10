import assert from 'node:assert/strict';
import fs from 'node:fs';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
const root=import.meta.dirname+'/..';
const server=await createServer({root,configFile:false,plugins:[react()],cacheDir:root+'/node_modules/.vite-installed-hosts',server:{host:'127.0.0.1',port:0,fs:{allow:[root,fs.realpathSync(root+'/node_modules')],deny:['**/.env*','**/*.{pem,crt,key}']}}}); await server.listen();
const browser=await puppeteer.launch({executablePath:'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',headless:true,args:['--disable-gpu']});
let pass=0,fail=0;const results=[];
async function run(name,query,action){const page=await browser.newPage();page.setDefaultTimeout(5000);const errors=[];page.on('pageerror',e=>errors.push(e.message));try{await page.goto(server.resolvedUrls.local[0]+'tests/fixtures/f10-installed-hosts.html?'+query);await page.waitForFunction(()=>window.installed&&document.querySelector('main'));await action(page);assert.deepEqual(errors,[]);pass++;results.push({name,pass:true});}catch(e){fail++;results.push({name,pass:false,error:e.message,diagnostic:await page.evaluate(()=>({text:document.body.innerText,calls:window.installed?.calls})).catch(()=>null)});}finally{await page.close();}}
try{
 for(const language of ['zh','en','ja']) {
  await run('personal closure host and real BFF write '+language,'mode=personal&language='+language,async page=>{await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.installed.accountClosureCopy[window.installed.language].start));await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.installed.accountClosureCopy[window.installed.language].start).click());await page.waitForFunction(()=>document.body.textContent.includes(window.installed.accountClosureCopy[window.installed.language].stages.CLOSURE_REQUESTED));assert.ok(await page.evaluate(()=>window.installed.calls.some(c=>c.method==='POST'&&c.headers['Idempotency-Key']&&c.headers['X-CSRF-Token']==='controlled-csrf'&&JSON.parse(c.body).confirmation==='CLOSE_MY_ACCOUNT')));});
  await run('admin closure-only permission navigation '+language,'mode=admin&language='+language,async page=>{await page.waitForFunction(()=>document.body.textContent.includes(window.installed.accountClosureCopy[window.installed.language].statuses.REPAIR_REQUIRED));assert.ok(await page.evaluate(()=>window.installed.calls.some(c=>c.url==='/api/admin/account/admin/v1/account-closure-requests')));assert.equal(await page.$('textarea'),null);assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.url.includes('/billing/'))),false);});
  await run('ENT repair-only tab no wallet/payment calls '+language,'mode=ent&language='+language,async page=>{await page.waitForFunction(()=>[...document.querySelectorAll('nav button')].some(b=>b.textContent===window.installed.entitlementCorrectionCopy[window.installed.language].title));await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(b=>b.textContent===window.installed.entitlementCorrectionCopy[window.installed.language].title).click());await page.waitForSelector('section input');assert.ok(await page.evaluate(()=>document.body.textContent.includes(window.installed.entitlementCorrectionCopy[window.installed.language].explanation)));assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.url.includes('/billing/'))),false);});
 }
 for (const language of ['zh','en','ja']) await run('ENT installed directory excludes future commands '+language,'mode=ent&approval_view=directory&language='+language,async page=>{
  await page.evaluate(()=>[...document.querySelectorAll('main > nav button')].find(b=>b.textContent===window.installed.adminPlatformCopy[window.installed.language].nav.approvals).click());
  await page.waitForFunction(()=>document.querySelector('table')?.textContent.includes('ENTITLEMENT_COMPONENT_ADJUSTMENT'));
  assert.equal(await page.evaluate(()=>document.querySelector('table').textContent.includes('RECEIVABLE_WRITE_OFF')),false);
 });
 await run('ENT real form submits and center EDIT obtains fresh original-intent proof','mode=ent',async page=>{
  const clickText=async(expression)=>page.evaluate(expression);
  await page.waitForFunction(()=>[...document.querySelectorAll('nav button')].some(b=>b.textContent===window.installed.entitlementCorrectionCopy.zh.title));
  await clickText(()=>[...document.querySelectorAll('nav button')].find(b=>b.textContent===window.installed.entitlementCorrectionCopy.zh.title).click());
  await page.waitForSelector('section form input');
  await page.evaluate(()=>{const inputs=[...document.querySelectorAll('section form input')];const set=Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set;for(const [i,v] of [window.installed.id(5),window.installed.id(6),'PRODUCT_SPLIT_CORRECTION'].entries()){set.call(inputs[i],v);inputs[i].dispatchEvent(new Event('input',{bubbles:true}));}});
  await clickText(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.installed.entitlementCorrectionCopy.zh.precheck).click());
  await page.waitForSelector('textarea');
  await page.evaluate(()=>{const el=document.querySelector('textarea');Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set.call(el,'Verified payment product split evidence');el.dispatchEvent(new Event('input',{bubbles:true}));});
  await clickText(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.installed.entitlementCorrectionCopy.zh.submit).click());
  await page.waitForFunction(()=>document.body.textContent.includes(window.installed.id(9)) && document.body.textContent.includes(window.installed.entitlementCorrectionCopy.zh.approvalStatuses.SUBMITTED));
  assert.ok(await page.evaluate(()=>{const c=window.installed.calls.find(c=>c.method==='POST'&&c.url==='/api/admin/approvals/requests');return c.headers['X-CSRF-Token']==='controlled-csrf'&&c.headers['X-Pinvou-Approval-Action-Control-Version']==='1'&&c.body.includes('"wallet_version":9007199254740993')&&JSON.parse(c.body).normalized_command.package_id===window.installed.id(5);}));
  await page.evaluate(()=>{window.installed.ent.status='DRAFT';window.installed.ent.version=7;window.installed.ent.facts.critical_resource_versions.wallet_version='9223372036854775807';});
  await clickText(()=>[...document.querySelectorAll('main > nav button')].find(b=>b.textContent===window.installed.adminPlatformCopy.zh.nav.approvals).click());
  await page.waitForFunction(()=>document.querySelector('table')?.textContent.includes('ENTITLEMENT_COMPONENT_ADJUSTMENT'));
  await clickText(()=>[...document.querySelectorAll('table button')].find(b=>b.textContent===window.installed.financeCopy.zh.details).click());
  await page.waitForSelector('article textarea');
  assert.ok(await page.evaluate(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.installed.adminPlatformCopy.zh.approvalCenter.openBusinessArea)));
  await page.evaluate(()=>{const el=document.querySelector('article textarea');Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set.call(el,'Fresh verified product split evidence');el.dispatchEvent(new Event('input',{bubbles:true}));});
  await clickText(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.installed.financeCopy.zh.editSubmit).click());
  await page.waitForFunction(()=>window.installed.ent.status==='SUBMITTED'&&window.installed.calls.some(c=>c.method==='PUT'));
  assert.ok(await page.evaluate(()=>{const c=window.installed.calls.find(c=>c.method==='PUT');const b=JSON.parse(c.body);return b.expected_version===7&&c.body.includes('"wallet_version":9223372036854775807')&&b.normalized_command.payment_order_id===window.installed.id(6)&&b.normalized_command.reason_code==='PRODUCT_SPLIT_CORRECTION';}));
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.installed.adminPlatformCopy.zh.approvalCenter.openBusinessArea&&!b.disabled));
  await clickText(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.installed.adminPlatformCopy.zh.approvalCenter.openBusinessArea).click());
  await page.waitForFunction(()=>[...document.querySelectorAll('.adm-panel section h2')].some(heading=>heading.textContent===window.installed.entitlementCorrectionCopy.zh.title));
  assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.url.includes('/billing/'))),false);
 });
 await run('ENT approver-only center can decide without exposing request or wallet tabs','mode=ent&approve-only',async page=>{
  await page.waitForFunction(()=>document.querySelector('table')?.textContent.includes('ENTITLEMENT_COMPONENT_ADJUSTMENT'));
  assert.equal(await page.evaluate(()=>[...document.querySelectorAll('nav button')].some(b=>b.textContent===window.installed.entitlementCorrectionCopy.zh.title)),false);
  await page.evaluate(()=>[...document.querySelectorAll('table button')].find(b=>b.textContent===window.installed.financeCopy.zh.details).click());
  await page.waitForSelector('article textarea');
  await page.evaluate(()=>{const el=document.querySelector('article textarea');Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set.call(el,'Independent verified product split approval');el.dispatchEvent(new Event('input',{bubbles:true}));});
  await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.installed.financeCopy.zh.approve).click());
  await page.waitForFunction(()=>window.installed.ent.status==='APPROVED');
  assert.ok(await page.evaluate(()=>{const c=window.installed.calls.find(c=>c.url.endsWith('/actions/decide'));return c.headers['X-CSRF-Token']==='controlled-csrf'&&JSON.parse(c.body).decision==='APPROVE'&&JSON.parse(c.body).expected_version===1;}));
  assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.url.includes('/billing/')||c.url.includes('approval-command-prechecks'))),false);
 });
 await run('ENT failed precheck cannot create an approval','mode=ent&precheck-error',async page=>{
  await page.waitForFunction(()=>[...document.querySelectorAll('nav button')].some(b=>b.textContent===window.installed.entitlementCorrectionCopy.zh.title));
  await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(b=>b.textContent===window.installed.entitlementCorrectionCopy.zh.title).click());
  await page.waitForSelector('section form input');
  await page.evaluate(()=>{const inputs=[...document.querySelectorAll('section form input')];const set=Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set;for(const [i,v] of [window.installed.id(5),window.installed.id(6),'PRODUCT_SPLIT_CORRECTION'].entries()){set.call(inputs[i],v);inputs[i].dispatchEvent(new Event('input',{bubbles:true}));}});
  await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.installed.entitlementCorrectionCopy.zh.precheck).click());
  await page.waitForSelector('[role="alert"]');assert.equal(await page.$('textarea'),null);
  assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.url==='/api/admin/approvals/requests'&&c.method==='POST')),false);
 });
 await run('missing closure grants hide both entry and API read','mode=personal&deny',async page=>{await page.waitForFunction(()=>window.installed.calls.some(c=>c.url==='/api/account/v1/me/organizations'));assert.equal(await page.evaluate(()=>document.body.textContent.includes(window.installed.accountClosureCopy.zh.title)),false);assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.url.includes('account-closure'))),false);});
 await run('closure authoritative read failure cannot enable mutation','mode=personal&error',async page=>{await page.waitForFunction(()=>document.body.textContent.includes(window.installed.accountClosureCopy.zh.queryFailed));assert.equal(await page.evaluate(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.installed.accountClosureCopy.zh.start)),false);});
 await run('retired owner closure read cannot overwrite new owner','mode=personal&delay',async page=>{await page.waitForFunction(()=>typeof window.installed.resolve==='function');await page.evaluate(()=>{window.installed.switchUser();window.installed.resolve();});await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.installed.accountClosureCopy.zh.start));assert.equal(await page.evaluate(()=>document.body.textContent.includes(window.installed.accountClosureCopy[window.installed.language].stages.CLOSURE_REQUESTED)),false);});
 await run('org host invokes strict lossless reclaim SDK','mode=org',async page=>{await page.waitForFunction(()=>document.body.textContent.includes(window.installed.accountClosedGrantCopy.zh.title));await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.installed.accountClosedGrantCopy.zh.single));await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.installed.accountClosedGrantCopy.zh.single).click());await page.waitForFunction(()=>window.installed.calls.some(c=>c.method==='POST'&&c.url.includes('/account-closed-grants/')));assert.ok(await page.evaluate(()=>{const c=window.installed.calls.find(c=>c.method==='POST'&&c.url.includes('/account-closed-grants/'));return c.headers['Idempotency-Key']&&c.headers['X-CSRF-Token']==='controlled-csrf'&&JSON.parse(c.body).reason==='ACCOUNT_CLOSED_FULL_RECLAIM';}));});
 await run('org host requires both reclaim and quota permissions','mode=org&deny',async page=>{await page.waitForFunction(()=>window.installed.calls.some(c=>c.url==='/api/account/v1/me/organizations'));assert.equal(await page.evaluate(()=>document.body.textContent.includes(window.installed.accountClosedGrantCopy.zh.title)),false);assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.url.includes('quota-grants'))),false);});
}finally{await browser.close();await server.close();console.log(JSON.stringify({pass,fail,skip:0,results},null,2));if(fail)process.exitCode=1;}
