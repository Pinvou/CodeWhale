import assert from 'node:assert/strict';
import fs from 'node:fs';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
const browserPath = [process.env.CHROME, '/usr/bin/google-chrome', '/usr/bin/chromium', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe'].find(value => value && fs.existsSync(value));
assert.ok(browserPath, 'Chromium is required; set CHROME to its executable');
const root=import.meta.dirname+'/..';
const server=await createServer({root,configFile:false,plugins:[react()],cacheDir:root+'/node_modules/.vite-installed-hosts',server:{host:'127.0.0.1',port:0,fs:{allow:[root,fs.realpathSync(root+'/node_modules')],deny:['**/.env*','**/*.{pem,crt,key}']}}}); await server.listen();
const browser=await puppeteer.launch({executablePath:browserPath,headless:true,args:['--disable-gpu']});
let pass=0,fail=0;const results=[];
async function run(name,query,action){const page=await browser.newPage();page.setDefaultTimeout(5000);const errors=[];page.on('pageerror',e=>errors.push(e.message));try{await page.goto(server.resolvedUrls.local[0]+'tests/fixtures/f10-installed-hosts.html?'+query);await page.waitForFunction(()=>window.installed&&document.querySelector('main'));await action(page);assert.deepEqual(errors,[]);pass++;results.push({name,pass:true});}catch(e){fail++;results.push({name,pass:false,error:e.message,diagnostic:await page.evaluate(()=>({text:document.body.innerText,calls:window.installed?.calls})).catch(()=>null)});}finally{await page.close();}}
async function openFinanceCorrection(page) {
  await page.waitForFunction(() => document.querySelector('.adm-panel') && [...document.querySelectorAll('.adm-panel nav button')].some(button => button.textContent === window.installed.entitlementCorrectionCopy.zh.title));
  await page.evaluate(() => [...document.querySelectorAll('.adm-panel nav button')].find(button => button.textContent === window.installed.entitlementCorrectionCopy.zh.title).click());
  await page.waitForSelector('section form input');
}
async function prepareFinanceCorrection(page) {
  await page.evaluate(() => {
    const inputs = [...document.querySelectorAll('section form input')];
    const set = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
    for (const [index, value] of [window.installed.id(5), window.installed.id(6), 'PRODUCT_SPLIT_CORRECTION'].entries()) {
      set.call(inputs[index], value); inputs[index].dispatchEvent(new Event('input', { bubbles: true }));
    }
  });
  await page.evaluate(() => [...document.querySelectorAll('button')].find(button => button.textContent === window.installed.entitlementCorrectionCopy.zh.precheck).click());
  await page.waitForSelector('textarea');
  await page.evaluate(() => {
    const input = document.querySelector('textarea');
    Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(input, 'Verified payment product split evidence');
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
}
async function submitFinanceCorrection(page) {
  await page.waitForFunction(() => [...document.querySelectorAll('button')].some(button => button.textContent === window.installed.entitlementCorrectionCopy.zh.submit && !button.disabled));
  await page.evaluate(() => [...document.querySelectorAll('button')].find(button => button.textContent === window.installed.entitlementCorrectionCopy.zh.submit).click());
}
async function waitCorrectionUnknown(page) {
  await page.waitForFunction(() => document.body.textContent.includes(window.installed.entitlementCorrectionCopy.zh.recoveryPending) && [...document.querySelectorAll('button')].some(button => button.textContent === window.installed.financeCopy.zh.retry && !button.disabled));
}
async function retireFinanceVersion(page) {
  const priorForm = await page.$('section form');
  const reads = await page.evaluate(() => window.installed.calls.filter(call => call.url === '/api/account/v1/account/bootstrap').length);
  await page.evaluate(() => { window.installed.platformVersion++; window.dispatchEvent(new Event('focus')); });
  await page.waitForFunction(count => window.installed.calls.filter(call => call.url === '/api/account/v1/account/bootstrap').length > count, {}, reads);
  await page.waitForFunction(form => !form.isConnected, {}, priorForm);
  await priorForm.dispose();
  await waitCorrectionUnknown(page);
}
try{
 for(const language of ['zh','en','ja']) {
  await run('personal closure host and real BFF write '+language,'mode=personal&language='+language,async page=>{await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.installed.accountClosureCopy[window.installed.language].start));await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.installed.accountClosureCopy[window.installed.language].start).click());await page.waitForSelector('form[role="alertdialog"] input');await page.type('form[role="alertdialog"] input','CLOSE_MY_ACCOUNT');await page.$eval('form[role="alertdialog"]',form=>form.requestSubmit());await page.waitForFunction(()=>document.body.textContent.includes(window.installed.accountClosureCopy[window.installed.language].stages.CLOSURE_REQUESTED));assert.ok(await page.evaluate(()=>window.installed.calls.some(c=>c.method==='POST'&&c.headers['Idempotency-Key']&&c.headers['X-CSRF-Token']==='controlled-csrf'&&JSON.parse(c.body).confirmation==='CLOSE_MY_ACCOUNT')));});
  await run('admin closure-only permission navigation '+language,'mode=admin&language='+language,async page=>{await page.waitForFunction(()=>document.body.textContent.includes(window.installed.accountClosureCopy[window.installed.language].statuses.REPAIR_REQUIRED));assert.ok(await page.evaluate(()=>window.installed.calls.some(c=>c.url==='/api/admin/account/admin/v1/account-closure-requests')));assert.equal(await page.$('textarea'),null);assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.url.includes('/billing/'))),false);});
  await run('ENT repair-only tab no wallet/payment calls '+language,'mode=ent&language='+language,async page=>{await page.waitForFunction(()=>[...document.querySelectorAll('nav button')].some(b=>b.textContent===window.installed.entitlementCorrectionCopy[window.installed.language].title));await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(b=>b.textContent===window.installed.entitlementCorrectionCopy[window.installed.language].title).click());await page.waitForSelector('section input');assert.ok(await page.evaluate(()=>document.body.textContent.includes(window.installed.entitlementCorrectionCopy[window.installed.language].explanation)));assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.url.includes('/billing/'))),false);});
 }
 for (const language of ['zh','en','ja']) await run('operations navigation remains unique '+language,'mode=admin&section=closures&ops-navigation=1&language='+language,async page=>{
  await page.waitForFunction(()=>document.body.textContent.includes(window.installed.accountClosureCopy[window.installed.language].statuses.REPAIR_REQUIRED));
  assert.equal(await page.evaluate(()=>[...document.querySelectorAll('main > nav button')].filter(button=>button.textContent===window.installed.adminPlatformCopy[window.installed.language].operations.nav).length),1);
 });
 for (const language of ['zh','en','ja']) await run('ENT installed directory excludes future commands '+language,'mode=ent&approval_view=directory&language='+language,async page=>{
  await page.evaluate(()=>[...document.querySelectorAll('main > nav button')].find(b=>b.textContent===window.installed.adminPlatformCopy[window.installed.language].nav.approvals).click());
  await page.waitForFunction(()=>document.querySelector('table')?.textContent.includes('ENTITLEMENT_COMPONENT_ADJUSTMENT'));
  assert.equal(await page.evaluate(()=>document.querySelector('table').textContent.includes('UNKNOWN_REPAIR')),false);
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
 await run('ENT Finance composite owner completes precheck, draft and submit','mode=ent&section=finance',async page=>{
  await openFinanceCorrection(page); await prepareFinanceCorrection(page); await submitFinanceCorrection(page);
  await page.waitForSelector('section article');
  const result = await page.evaluate(() => ({ calls:window.installed.calls, status:window.installed.ent.status, effects:window.installed.ent.effects, intent:window.installed.entitlementIntent() }));
  assert.equal(result.status,'SUBMITTED'); assert.equal(result.effects,1); assert.equal(result.intent,null);
  assert.equal(result.calls.filter(call=>call.url.endsWith('approval-command-prechecks')).length,1);
  const writes = result.calls.filter(call=>call.url==='/api/admin/approvals/requests'&&call.method==='POST');
  assert.equal(writes.length,1); assert.match(writes[0].body, /"wallet_version":9007199254740993/);
  assert.ok(result.calls.some(call=>call.url.endsWith('/actions/submit')&&call.method==='POST'));
 });
 await run('ENT Finance UNKNOWN keeps original actor, edit CAS, facts and key after version retirement','mode=ent&section=finance&lose-entitlement-edit',async page=>{
  await openFinanceCorrection(page);
  await page.evaluate(()=>{window.installed.ent.created=true;window.installed.ent.version=7;});
  await prepareFinanceCorrection(page); await submitFinanceCorrection(page); await waitCorrectionUnknown(page);
  const frozen = await page.evaluate(()=>window.installed.entitlementIntent());
  assert.equal(frozen.actor,'00000000-0000-4000-8000-000000000001'); assert.equal(frozen.editReference.existing.version,7);
  assert.equal(frozen.editReference.existing.approval_request_id,'00000000-0000-4000-8000-000000000009');
  await page.evaluate(()=>{window.installed.ent.facts.critical_resource_versions.wallet_version='9223372036854775807';});
  await retireFinanceVersion(page);
  assert.deepEqual(await page.evaluate(()=>window.installed.entitlementIntent()),frozen);
  assert.equal(await page.$('section article'),null);
  assert.equal(await page.evaluate(()=>window.installed.calls.filter(call=>call.method==='PUT').length),1);
  await page.evaluate(()=>[...document.querySelectorAll('button')].find(button=>button.textContent===window.installed.financeCopy.zh.retry).click());
  await page.waitForSelector('section article');
  const result = await page.evaluate(()=>({ calls:window.installed.calls, status:window.installed.ent.status, intent:window.installed.entitlementIntent() }));
  const updates = result.calls.filter(call=>call.method==='PUT');
  assert.equal(updates.length,2); assert.equal(updates[1].body,updates[0].body);
  assert.equal(JSON.parse(updates[1].body).expected_version,7);
  assert.equal(JSON.parse(updates[1].body).idempotency_key,'approval:'+frozen.key);
  assert.match(updates[1].body,/"wallet_version":9007199254740993/);
  assert.equal(result.calls.filter(call=>call.url==='/api/admin/approvals/requests'&&call.method==='POST').length,0);
  assert.equal(result.status,'SUBMITTED'); assert.equal(result.intent,null);
 });
 await run('ENT Finance retired version ignores late success and explicitly recovers only the same invocation','mode=ent&section=finance&hold-entitlement-create',async page=>{
  await openFinanceCorrection(page); await prepareFinanceCorrection(page); await submitFinanceCorrection(page);
  await page.waitForFunction(()=>typeof window.installed.releaseEntitlementCreate==='function');
  const frozen = await page.evaluate(()=>window.installed.entitlementIntent());
  await retireFinanceVersion(page);
  assert.deepEqual(await page.evaluate(()=>window.installed.entitlementIntent()),frozen);
  assert.equal(await page.evaluate(()=>window.installed.calls.filter(call=>call.url==='/api/admin/approvals/requests'&&call.method==='POST').length),1);
  await page.evaluate(()=>window.installed.releaseEntitlementCreate());
  await page.waitForFunction(()=>window.installed.ent.status==='SUBMITTED'&&window.installed.calls.filter(call=>call.url.endsWith('/00000000-0000-4000-8000-000000000009')&&call.method==='GET').length>=2);
  await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
  assert.equal(await page.$('section article'),null,'retired completion cannot display a receipt in the current version');
  await waitCorrectionUnknown(page);
  await page.evaluate(()=>[...document.querySelectorAll('button')].find(button=>button.textContent===window.installed.financeCopy.zh.retry).click());
  await page.waitForSelector('section article');
  const result = await page.evaluate(()=>({ calls:window.installed.calls, effects:window.installed.ent.effects, intent:window.installed.entitlementIntent() }));
  const creates = result.calls.filter(call=>call.url==='/api/admin/approvals/requests'&&call.method==='POST');
  assert.equal(creates.length,3); assert.ok(creates.every(call=>call.body===creates[0].body));
  assert.equal(JSON.parse(creates[0].body).idempotency_key,'approval:'+frozen.key);
  assert.equal(result.effects,1); assert.equal(result.intent,null);
 });
 await run('ENT Finance another actor cannot adopt the prior UNKNOWN proof','mode=ent&section=finance&lose-entitlement-create',async page=>{
  await openFinanceCorrection(page); await prepareFinanceCorrection(page); await submitFinanceCorrection(page); await waitCorrectionUnknown(page);
  const frozen = await page.evaluate(()=>window.installed.entitlementIntent());
  await page.evaluate(()=>window.installed.switchUser());
  await openFinanceCorrection(page);
  assert.equal(await page.evaluate(()=>window.installed.entitlementIntent()),null);
  assert.equal(await page.$('textarea'),null);
  assert.equal(await page.evaluate(()=>window.installed.calls.filter(call=>call.url==='/api/admin/approvals/requests'&&call.method==='POST').length),1);
  assert.deepEqual(await page.evaluate(actor=>JSON.parse(sessionStorage.getItem('pinvou.entitlement-correction.intent.v1:'+actor)),frozen.actor),frozen);
 });
 await run('ENT Finance storage denial blocks dispatch before history or draft writes','mode=ent&section=finance',async page=>{
  await openFinanceCorrection(page); await prepareFinanceCorrection(page);
  await page.evaluate(()=>{
    const original = Storage.prototype.setItem;
    Storage.prototype.setItem=function(key,value){if(key.startsWith('pinvou.entitlement-correction.intent.v1:'))throw new Error('Controlled entitlement storage denial');return original.call(this,key,value);};
  });
  await submitFinanceCorrection(page);
  await page.waitForFunction(()=>document.querySelector('[role="alert"]')?.textContent===window.installed.entitlementCorrectionCopy.zh.recoveryUnavailable);
  assert.equal(await page.evaluate(()=>window.installed.calls.some(call=>call.url.startsWith('/api/admin/approvals/requests'))),false);
 });
 await run('missing closure grants hide both entry and API read','mode=personal&deny',async page=>{await page.waitForFunction(()=>window.installed.calls.some(c=>c.url==='/api/account/v1/me/organizations'));assert.equal(await page.evaluate(()=>document.body.textContent.includes(window.installed.accountClosureCopy.zh.title)),false);assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.url.includes('account-closure'))),false);});
 await run('closure authoritative read failure cannot enable mutation','mode=personal&error',async page=>{await page.waitForFunction(()=>document.body.textContent.includes(window.installed.accountClosureCopy.zh.queryFailed));assert.equal(await page.evaluate(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.installed.accountClosureCopy.zh.start)),false);});
 await run('retired owner closure read cannot overwrite new owner','mode=personal&delay',async page=>{await page.waitForFunction(()=>typeof window.installed.resolve==='function');await page.evaluate(()=>{window.installed.switchUser();window.installed.resolve();});await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.installed.accountClosureCopy.zh.start));assert.equal(await page.evaluate(()=>document.body.textContent.includes(window.installed.accountClosureCopy[window.installed.language].stages.CLOSURE_REQUESTED)),false);});
 await run('org host invokes strict lossless reclaim SDK','mode=org',async page=>{await page.waitForFunction(()=>document.body.textContent.includes(window.installed.accountClosedGrantCopy.zh.title));await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.installed.accountClosedGrantCopy.zh.single));await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.installed.accountClosedGrantCopy.zh.single).click());await page.waitForSelector('[role="alertdialog"]');assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.method==='POST'&&c.url.includes('/account-closed-grants/'))),false,'reclaim requires actual feature confirmation');await page.evaluate(()=>[...document.querySelectorAll('[role="alertdialog"] button')].find(b=>b.textContent===window.installed.organizationCopy.zh.confirmAction).click());await page.waitForFunction(()=>window.installed.calls.some(c=>c.method==='POST'&&c.url.includes('/account-closed-grants/')));assert.ok(await page.evaluate(()=>{const c=window.installed.calls.find(c=>c.method==='POST'&&c.url.includes('/account-closed-grants/'));return c.headers['Idempotency-Key']&&c.headers['X-CSRF-Token']==='controlled-csrf'&&JSON.parse(c.body).reason==='ACCOUNT_CLOSED_FULL_RECLAIM';}));await page.waitForFunction(()=>window.installed.reclaimed&&window.installed.calls.filter(c=>c.url.endsWith('/quota-grants')).length>=3&&![...document.querySelectorAll('button')].some(b=>b.textContent===window.installed.accountClosedGrantCopy.zh.single));assert.ok(await page.evaluate(()=>{const c=window.installed.calls.find(c=>c.method==='POST'&&c.url.includes('/account-closed-grants/'));const proof=window.installed.reclaimIntent;return proof.actor===window.installed.user&&proof.scope===`closed-grant-reclaim:${window.installed.id(2)}:${window.installed.id(4)}`&&proof.key===c.headers['Idempotency-Key']&&proof.facts.grant===window.installed.id(4)&&proof.facts.reason==='ACCOUNT_CLOSED_FULL_RECLAIM'&&window.installed.reclaimProof()===null;}),'known SDK receipt must clear only the original captured actor/scope/key/facts proof');});
 await run('org host requires both reclaim and quota permissions','mode=org&deny',async page=>{await page.waitForFunction(()=>window.installed.calls.some(c=>c.url==='/api/account/v1/me/organizations'));assert.equal(await page.evaluate(()=>document.body.textContent.includes(window.installed.accountClosedGrantCopy.zh.title)),false);assert.equal(await page.evaluate(()=>window.installed.calls.some(c=>c.url.includes('quota-grants'))),false);});
}finally{await browser.close();await server.close();console.log(JSON.stringify({pass,fail,skip:0,results},null,2));if(fail)process.exitCode=1;}
