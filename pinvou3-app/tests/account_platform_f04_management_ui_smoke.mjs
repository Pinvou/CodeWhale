import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const chrome=process.env.CHROME||['C:/Program Files/Google/Chrome/Application/chrome.exe','C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe','C:/Program Files/Microsoft/Edge/Application/msedge.exe','/usr/bin/chromium','/usr/bin/google-chrome'].find(fs.existsSync);
assert.ok(chrome,'Browser unavailable; UI validation not performed');
const profile=fs.mkdtempSync(path.join(os.tmpdir(),'pinvou-f04-management-'));const ownedProfile=fs.realpathSync(profile);
let vite,browser,page,passed=0;const errors=[];
try{
 vite=await createServer({root,configFile:false,appType:'mpa',logLevel:'error',plugins:[react()],server:{host:'127.0.0.1',port:0,watch:null}});await vite.listen();
 browser=await puppeteer.launch({executablePath:chrome,headless:'new',userDataDir:profile,args:['--disable-gpu','--no-first-run']});page=await browser.newPage();page.setDefaultTimeout(10000);page.on('pageerror',e=>errors.push(e.message));
 const url=`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f04-management.html`;
 // Runs inside the browser page, where Web Storage is supported.
 // eslint-disable-next-line n/no-unsupported-features/node-builtins
 const fresh=async()=>{errors.length=0;await page.goto(url);await page.waitForFunction(()=>window.f04);await page.evaluate(()=>{window.sessionStorage.clear();window.f04.render();});await page.waitForSelector('.adm-pricing-workspace');};
 const click=async key=>{assert.ok(await page.evaluate(key=>{const label=window.f04.labels()[key];const button=[...document.querySelectorAll('button')].find(b=>b.textContent.trim()===label&&!b.disabled);if(!button)return false;button.click();return true;},key),`Unavailable ${key}`);};
 const fill=async(selector,value)=>page.$eval(selector,(input,value)=>{Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(input,value);input.dispatchEvent(new Event('input',{bubbles:true}));input.dispatchEvent(new Event('change',{bubbles:true}));},value);
 const submit=async selector=>page.$eval(selector,form=>form.dispatchEvent(new Event('submit',{bubbles:true,cancelable:true})));
 const waitUnknown=()=>page.waitForFunction(()=>document.body.textContent.includes(window.f04.labels().resultUnknown));
 const caseRun=async(name,fn)=>{await fn();assert.deepEqual(errors,[],'Unexpected React errors');passed++;console.log(`PASS ${name}`);};
 const createDraft=async()=>{await click('createDraft');await page.waitForSelector('.adm-pricing-editor');await fill('.adm-pricing-editor input[type=date]','2099-01-01');await fill('.adm-pricing-editor input[type=number]','9007199254740993');await submit('.adm-pricing-editor');};
 await caseRun('Draft lost acknowledgement keeps exact price and original key across language change',async()=>{
  await fresh();await page.evaluate(()=>{window.f04.unknownOnce=true;});await createDraft();await waitUnknown();
  assert.ok(await page.$eval('.adm-pricing-fieldset',el=>el.disabled));await page.evaluate(()=>{window.f04.language='ja';window.f04.render();});await waitUnknown();await click('verifyResult');await page.waitForFunction(()=>document.body.textContent.includes(window.f04.labels().recoveryConfirmed));
  assert.ok(await page.evaluate(()=>{const calls=window.f04.calls.filter(c=>c.path.endsWith('/pricing/drafts'));return calls.length===2&&calls[0].key===calls[1].key&&calls[0].body===calls[1].body&&calls[0].body.includes('"fixed_price_per_denominator":9007199254740993')&&calls[0].body.includes('"expected_etag":9007199254740993');}));
 });
 await caseRun('Unit invalid input releases the lock and corrected int64 retry preserves facts',async()=>{
  await fresh();await click('createUnitVersion');await page.waitForSelector('.adm-pricing-unit-form');await fill('.adm-pricing-unit-form input[maxlength]','CONTROLLED');const inputs=await page.$$('.adm-pricing-unit-form input[type=number]');await inputs[0].evaluate(input=>{Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(input,'1.5');input.dispatchEvent(new Event('input',{bubbles:true}));});await submit('.adm-pricing-unit-form');await page.waitForSelector('[role=alert]');
  assert.equal(await page.evaluate(()=>window.f04.calls.filter(c=>c.method==='POST').length),0);
  await fill('.adm-pricing-unit-form input[type=number]','1');await inputs[1].evaluate(input=>{Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(input,'9007199254740993');input.dispatchEvent(new Event('input',{bubbles:true}));});await page.evaluate(()=>{window.f04.unknownOnce=true;});await submit('.adm-pricing-unit-form');await waitUnknown();await click('verifyResult');await page.waitForFunction(()=>document.body.textContent.includes(window.f04.labels().recoveryConfirmed));
  assert.ok(await page.evaluate(()=>{const calls=window.f04.calls.filter(c=>c.path.endsWith('/metering-units')&&c.method==='POST');return calls.length===2&&calls[0].key===calls[1].key&&calls[0].body===calls[1].body&&calls[0].body.includes('"price_denominator":9007199254740993');}));
 });
 await caseRun('Unknown invocation is scoped to its owner and recovered when that owner returns',async()=>{
  await fresh();await page.evaluate(()=>{window.f04.unknownOnce=true;});await createDraft();await waitUnknown();await page.evaluate(()=>{window.f04.owner=window.f04.other;window.f04.render();});await page.waitForFunction(()=>!document.body.textContent.includes(window.f04.labels().resultUnknown));await page.evaluate(()=>{window.f04.owner=window.f04.id;window.f04.render();});await waitUnknown();await click('verifyResult');await page.waitForFunction(()=>document.body.textContent.includes(window.f04.labels().recoveryConfirmed));
 });
 for(const [action,derived,pathEnd]of [['publish','DRAFT','/publish'],['cancelFuture','PUBLISHED','/transition'],['deactivate','DRAFT','/deactivate']])await caseRun(`${action} lost response recovers the exact frozen invocation`,async()=>{
  await fresh();await page.evaluate(derived=>{window.f04.prices=[{...window.f04.price,lifecycle_status:derived,derived_status:derived}];},derived);
  // Reload source facts through the actual refresh control rather than mutating React state.
  await page.evaluate(()=>{document.querySelector('.adm-pricing-panel-head button').click();});await page.waitForFunction(()=>document.querySelector('tbody')?.textContent.includes('CONTROLLED'));await click(action);await page.waitForSelector('.adm-pricing-action');
  if(action==='cancelFuture')await page.$eval('.adm-pricing-action textarea',input=>{Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set.call(input,'Controlled cancellation reason');input.dispatchEvent(new Event('input',{bubbles:true}));});
  await page.evaluate(()=>{window.f04.unknownOnce=true;});await submit('.adm-pricing-action');await waitUnknown();await click('verifyResult');await page.waitForFunction(()=>document.body.textContent.includes(window.f04.labels().recoveryConfirmed));
  assert.ok(await page.evaluate(pathEnd=>{const calls=window.f04.calls.filter(c=>c.path.endsWith(pathEnd)&&c.method==='POST');return calls.length===2&&calls[0].key===calls[1].key&&calls[0].body===calls[1].body;},pathEnd));
 });
 await caseRun('Actual approval deep link edits only reason while retaining complete typed planned intent',async()=>{
  await fresh();await page.evaluate(()=>{history.replaceState({},'',`${location.pathname}?approval_request_id=${window.f04.id}`);window.f04.surface='management';window.f04.render();});await page.waitForSelector('article textarea');
  await page.type('article textarea','Controlled reason edit');await page.evaluate(()=>{const button=[...document.querySelectorAll('article button')].find(b=>b.textContent.includes('submit')&&!b.disabled);if(!button)throw new Error('Missing draft edit');button.click();});await page.waitForFunction(()=>window.f04.approvalStatus==='SUBMITTED');
  assert.ok(await page.evaluate(()=>{const edit=window.f04.calls.find(c=>c.method==='PUT');const body=edit&&JSON.parse(edit.body);const submit=window.f04.calls.find(c=>c.path.endsWith('/actions/submit'));return edit.body.includes('"lifecycle_plan_version":9007199254740993')&&body.normalized_command.effective_at==='2099-01-01T00:00:00+08:00'&&body.normalized_command.existing_session_cutoff_at==='2099-01-02T00:00:00+08:00'&&body.normalized_command.lifecycle_plan_id===window.f04.id&&JSON.parse(submit.body).expected_version===2;}));
 });
 for (const language of ['zh', 'en', 'ja']) await caseRun(`${language}: named pricing tables retain keyboard horizontal scrolling`, async () => {
  await fresh(); await page.setViewport({ width: 640, height: 800 });
  await page.evaluate(language => { window.f04.language = language; window.f04.render(); }, language);
  for (const [index, label] of [[0, 'priceVersions'], [1, 'meteringUnits']]) {
   const wrapper = (await page.$$('.adm-pricing-workspace > .adm-finance-subpanel > .adm-table-wrap'))[index];
   assert.ok(wrapper, `Missing pricing scroll region ${index}`);
   assert.ok(await wrapper.evaluate((element, label) => element.getAttribute('role') === 'region' && element.getAttribute('aria-label') === window.f04.labels()[label] && element.tabIndex === 0, label));
   // Force overflow using viewport constraints, leaving production table CSS
   // and native keyboard scrolling intact. No synthetic scroll event/write.
   await wrapper.evaluate(element => { element.style.maxWidth = '300px'; element.querySelector('table').style.minWidth = '900px'; });
   await wrapper.focus();
   assert.ok(await wrapper.evaluate(element => document.activeElement === element && element.scrollWidth > element.clientWidth && getComputedStyle(element).overflowX === 'auto'));
   await page.keyboard.press('ArrowRight');
   await page.waitForFunction(index => [...document.querySelectorAll('.adm-pricing-workspace > .adm-finance-subpanel > .adm-table-wrap')][index].scrollLeft > 0, {}, index);
  }
 });
 for(const language of ['zh','en','ja']) await caseRun(`${language}: actual credential page has a named heading and safety explanation`,async()=>{
  await fresh();await page.evaluate(language=>{window.history.replaceState({},'',`${location.pathname}?token_detail_tab=credentials`);window.f04.language=language;window.f04.surface='credentials';window.f04.render();},language);
  await page.waitForSelector('#token-credentials-heading');
  assert.ok(await page.$eval('#token-credentials-heading',el=>el.textContent.trim().length>0));
  assert.ok(await page.$eval('#token-credentials-heading',el=>el.closest('section').querySelector('.adm-source-lead').textContent.trim().length>0));
 });
 for(const type of ['TOKEN_CREDENTIAL_VALIDATE','TOKEN_MODEL_PUBLISH','TOKEN_MODEL_CATALOG_SYNC']) await caseRun(`${type}: lost ACK recovers original key without another write and allows closing terminal receipt`,async()=>{
  await fresh();await page.evaluate(type=>{window.f04.surface='command';window.f04.commandType=type;window.f04.unknownOnce=true;window.f04.render();},type);await page.waitForSelector('.adm-high-risk-dialog');await submit('.adm-high-risk-dialog');
  await page.waitForSelector('.adm-command-unknown');assert.ok(await page.$eval('.adm-high-risk-dialog header button',el=>el.disabled));
  await submit('.adm-high-risk-dialog');await page.waitForFunction(()=>!document.querySelector('.adm-high-risk-dialog header button').disabled);
  assert.ok(await page.evaluate(()=>{const calls=window.f04.calls.filter(c=>c.path.includes('/commands'));return calls.filter(c=>c.method==='POST').length===1&&calls.filter(c=>c.method==='GET').length===1&&calls[0].key===calls[1].key;}));
  if(type==='TOKEN_CREDENTIAL_VALIDATE') assert.ok(await page.evaluate(()=>document.body.textContent.includes(window.f04.credentialProbeLabel())));
  await page.$eval('.adm-high-risk-dialog header button',button=>button.click());await page.waitForFunction(()=>window.f04.closed===true);
 });
 await caseRun('Known entry expiry permits closing without treating an arbitrary timeout as safe',async()=>{
  await fresh();await page.evaluate(()=>{window.f04.surface='command';window.f04.commandType='TOKEN_CREDENTIAL_VALIDATE';window.f04.expiredEntryOnce=true;window.f04.render();});await page.waitForSelector('.adm-high-risk-dialog');await submit('.adm-high-risk-dialog');
  await page.waitForFunction(()=>document.body.textContent.includes(window.f04.expiredEntryLabel()));
  assert.equal(await page.$('.adm-command-unknown'),null);assert.equal(await page.$eval('.adm-high-risk-dialog header button',el=>el.disabled),false);
  assert.equal(await page.evaluate(()=>window.f04.calls.filter(c=>c.path.endsWith('/commands')&&c.method==='POST').length),1);
  await page.$eval('.adm-high-risk-dialog header button',button=>button.click());await page.waitForFunction(()=>window.f04.closed===true);
 });
 for(const language of ['zh','en','ja']) await caseRun(`${language}: missing source and published lifecycle remain distinct`,async()=>{
  await fresh();await page.evaluate(language=>{window.f04.language=language;window.f04.models=[{model_id:window.f04.id,display_name:'Controlled missing source',lifecycle_status:'PUBLISHED',protocol_code:'CONTROLLED',protocol_version:1,context_length:4096,source_available:false,catalog_version:1}];window.f04.surface='management';window.f04.render();},language);
  await page.waitForFunction(()=>document.body.textContent.includes('Controlled missing source'));
  assert.ok(await page.evaluate(()=>document.querySelector('.adm-source-missing-policy').textContent===window.f04.sourceMissingPolicyLabel()));
  assert.ok(await page.evaluate(()=>{const row=[...document.querySelectorAll('tr')].find(el=>el.textContent.includes('Controlled missing source'));const labels=window.f04.modelStatusLabels();return row.textContent.includes(labels.PUBLISHED)&&row.textContent.includes(labels.UNAVAILABLE)&&!row.textContent.includes(labels.DEPRECATED);}));
  await page.$eval('.adm-name-link',button=>button.click());await page.waitForSelector('#model-overview-heading');
  assert.ok(await page.$eval('.adm-model-detail .adm-source-missing-policy',el=>el.textContent===window.f04.sourceMissingPolicyLabel()));
  assert.equal(await page.evaluate(()=>window.f04.calls.filter(c=>c.method==='POST').length),0);
 });
 console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
}catch(error){console.error(`FAIL after ${passed} PASS: ${error.stack}`);if(page)console.error(JSON.stringify(await page.evaluate(()=>({text:document.body.textContent.slice(0,1200),paths:window.f04?.calls.map(c=>c.path)})).catch(()=>({}))));process.exitCode=1;}
finally{if(browser)await browser.close();if(vite)await vite.close();if(fs.existsSync(profile)){assert.equal(fs.realpathSync(profile),ownedProfile);assert.equal(path.dirname(ownedProfile),fs.realpathSync(os.tmpdir()));assert.ok(path.basename(ownedProfile).startsWith('pinvou-f04-management-'));fs.rmSync(ownedProfile,{recursive:true});}}
