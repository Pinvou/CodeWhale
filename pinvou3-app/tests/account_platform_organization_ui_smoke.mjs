import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const chrome=process.env.CHROME||['C:/Program Files/Google/Chrome/Application/chrome.exe','C:/Program Files/Microsoft/Edge/Application/msedge.exe','C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe','/usr/bin/chromium','/usr/bin/chromium-browser','/usr/bin/google-chrome','/usr/bin/google-chrome-stable','/snap/bin/chromium','/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(fs.existsSync);
assert.ok(chrome,'Browser unavailable; no UI validation performed');
const profile=fs.mkdtempSync(path.join(os.tmpdir(),'pinvou-f06-ui-'));
let vite,browser,page,passed=0;const errors=[];
try{
 vite=await createServer({root,cacheDir:process.env.F06_VITE_CACHE||path.join(root,'node_modules/.vite-f06-org'),configFile:false,appType:'mpa',logLevel:'error',plugins:[react()],server:{host:'127.0.0.1',port:0,watch:null}});await vite.listen();
 browser=await puppeteer.launch({executablePath:chrome,headless:'new',userDataDir:profile,args:['--disable-gpu','--no-first-run']});page=await browser.newPage();page.setDefaultTimeout(10000);page.on('pageerror',error=>errors.push(error.message));
 const url=`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-organization.html`;
 const fresh=async language=>{errors.length=0;await page.goto(url);await page.waitForFunction(()=>window.f06);await page.evaluate(language=>{window.f06.language=language;window.f06.render();},language);await page.waitForFunction(()=>document.body.textContent.includes('Controlled Unit'));};
 const form=async()=>{await page.evaluate(()=>{const form=[...document.querySelectorAll('form')].find(item=>item.querySelector('h4')?.textContent===window.f06.copy().createUnit);const input=form.querySelector('input');Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(input,'Controlled New Unit');input.dispatchEvent(new Event('input',{bubbles:true}));});};
 const click=async key=>{assert.ok(await page.evaluate(key=>{const text=window.f06.copy()[key];const button=[...document.querySelectorAll('button')].find(item=>item.textContent===text&&!item.disabled);if(!button)return false;button.click();return true;},key),`Missing action ${key}`);};
 const run=async(name,fn)=>{await fn();assert.deepEqual(errors,[]);passed++;console.log(`PASS ${name}`);};
 await run('New quota allocations expose only backend-supported BASE and BONUS',async()=>{
  await fresh('en');await page.evaluate(()=>{window.f06.enumMode=true;window.f06.authorizationVersion++;window.f06.render();});
  await page.waitForFunction(()=>[...document.querySelectorAll('form')].some(f=>f.querySelector('h4')?.textContent===window.f06.copy().allocate));
  assert.deepEqual(await page.evaluate(()=>{const f=[...document.querySelectorAll('form')].find(f=>f.querySelector('h4')?.textContent===window.f06.copy().allocate);return [...f.querySelector('select').options].map(o=>o.value).filter(Boolean);}),['BASE','BONUS']);
 });
 const prepareQuota = async component => {
  await page.evaluate(component => {
   const form = [...document.querySelectorAll('form')].find(f => f.querySelector('h4')?.textContent === window.f06.copy().allocate);
   const inputs = form.querySelectorAll('input');
   for (const [input, value] of [[inputs[0], window.f06.user], [inputs[1], window.f06.unit], [inputs[2], window.f06.other], [inputs[3], '100']]) {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, value); input.dispatchEvent(new Event('input', { bubbles: true }));
   }
   const select = form.querySelector('select');
   if (component === 'PLAN') select.add(new Option('Controlled unsupported PLAN', 'PLAN'));
   select.value = component; select.dispatchEvent(new Event('change', { bubbles: true }));
  }, component);
 };
 await run('Unsupported PLAN preparation never dispatches a new allocation', async () => {
  await fresh('en'); await page.evaluate(() => { window.f06.enumMode = true; window.f06.authorizationVersion++; window.f06.render(); });
  await page.waitForFunction(() => [...document.querySelectorAll('form')].some(f => f.querySelector('h4')?.textContent === window.f06.copy().allocate));
  await prepareQuota('PLAN'); await click('allocate');
  await page.waitForFunction(() => [...document.querySelectorAll('form')].find(f => f.querySelector('h4')?.textContent === window.f06.copy().allocate)?.querySelector('[role=alert]')?.textContent === window.f06.copy().invalid);
  assert.equal(await page.evaluate(() => window.f06.calls.some(c => c.method === 'POST')), false);
 });
 for (const component of ['BASE', 'BONUS']) await run(`${component}: allocation UNKNOWN retains original facts and key across language change`, async () => {
  await fresh('en'); await page.evaluate(() => {
   window.f06.enumMode = true; window.f06.authorizationVersion++; window.f06.unknown = true; window.f06.render();
   const originalFetch = globalThis.fetch;
   globalThis.fetch = async (url, options = {}) => {
    const path = new URL(String(url), location.href).pathname;
    if (options.method !== 'POST' || !path.endsWith('/quota-grants:batch')) return originalFetch(url, options);
    window.f06.calls.push({ path, method: options.method, body: options.body, key: options.headers['Idempotency-Key'] });
    const input = JSON.parse(options.body).items[0];
    return new Response(JSON.stringify({ items: [{ index: 0, status: 'SUCCEEDED', grant: { grant_id: window.f06.unit, organization_id: window.f06.org, member_user_id: input.member_user_id, package_id: input.package_id, component_id: input.component_id, status: 'ACTIVE', membership_version: input.membership_version, allocation_version: 1, original_microcredits: input.amount_microcredits, available_microcredits: input.amount_microcredits, reserved_microcredits: '0', used_microcredits: '0', expired_microcredits: '0', revoked_microcredits: '0' } }] }), { status: window.f06.unknown ? 202 : 207, headers: { 'Content-Type': 'application/json' } });
   };
  });
  await page.waitForFunction(() => [...document.querySelectorAll('form')].some(f => f.querySelector('h4')?.textContent === window.f06.copy().allocate));
  await prepareQuota(component); await click('allocate');
  await page.waitForFunction(() => [...document.querySelectorAll('form')].find(f => f.querySelector('h4')?.textContent === window.f06.copy().allocate)?.querySelector('[role=alert]')?.textContent === window.f06.copy().unknown);
  assert.ok(await page.evaluate(() => [...document.querySelectorAll('form')].find(f => f.querySelector('h4')?.textContent === window.f06.copy().allocate).querySelector('fieldset').disabled));
  await page.evaluate(() => { window.f06.unknown = false; window.f06.language = 'ja'; window.f06.render(); }); await click('retry');
  await page.waitForFunction(() => [...document.querySelectorAll('form')].find(f => f.querySelector('h4')?.textContent === window.f06.copy().allocate)?.querySelector('[role=status]')?.textContent === window.f06.copy().saved);
  assert.ok(await page.evaluate(component => {
   const calls = window.f06.calls.filter(c => c.method === 'POST' && c.path.endsWith('/quota-grants:batch')), expected = { items: [{ member_user_id: window.f06.user, membership_version: 1, package_id: window.f06.unit, component_id: window.f06.other, component_type: component, amount_microcredits: 100 }] };
   return calls.length === 2 && !!calls[0].key && calls[0].key === calls[1].key && calls[0].body === calls[1].body && JSON.stringify(JSON.parse(calls[1].body)) === JSON.stringify(expected);
  }, component));
 });
 for(const language of ['zh','en','ja'])await run(`${language}: organization read-only financial facts and permission controls`,async()=>{await fresh(language);await page.evaluate(()=>{window.f06.readonly=true;window.f06.render();});await page.waitForFunction(()=>!document.querySelector('.ap-org-action'));assert.ok(await page.evaluate(()=>document.body.textContent.includes('900,719,925,474.0993')));assert.equal(await page.evaluate(()=>[...document.querySelectorAll('button')].some(button=>button.textContent===window.f06.copy().createUnit)),false);});
 await run('Actual202 remains unknown and same immutable key/body survive language change',async()=>{await fresh('en');await form();await page.evaluate(()=>{window.f06.unknown=true;});await click('createUnit');await page.waitForFunction(()=>document.querySelector('[role=alert]')?.textContent===window.f06.copy().unknown);assert.ok(await page.evaluate(()=>[...document.querySelectorAll('form')].find(item=>item.querySelector('h4')?.textContent===window.f06.copy().createUnit).querySelector('fieldset').disabled));await page.evaluate(()=>{window.f06.unknown=false;window.f06.language='ja';window.f06.render();});await click('retry');await page.waitForFunction(()=>[...document.querySelectorAll('[role=status]')].some(item=>item.textContent===window.f06.copy().saved));assert.ok(await page.evaluate(()=>{const calls=window.f06.calls.filter(call=>call.method==='POST');return calls.length===2&&calls[0].key===calls[1].key&&calls[0].body===calls[1].body;}));});
 await run('Late write cannot settle or mutate the next identity',async()=>{await fresh('en');await form();await page.evaluate(()=>{window.f06.delayed=true;});await click('createUnit');await page.waitForFunction(()=>typeof window.f06.release==='function');await page.evaluate(()=>window.f06.switchIdentity());await page.waitForFunction(()=>!document.querySelector('.ap-org-action fieldset')?.disabled);await page.evaluate(()=>window.f06.release());assert.equal(await page.evaluate(()=>[...document.querySelectorAll('[role=status]')].some(item=>item.textContent===window.f06.copy().saved)),false);});
 await run('Context switch sends real expected version and retires organization panel',async()=>{await fresh('en');await click('personal');await page.waitForFunction(()=>!document.querySelector('.ap-org-action'));assert.ok(await page.evaluate(()=>{const call=window.f06.calls.find(call=>call.path.endsWith('/active-context'));return JSON.parse(call.body).expected_context_version===1&&JSON.parse(call.body).context_type==='PERSONAL';}));});
 for(const language of ['zh','en','ja'])await run(`${language}: actual Admin DTO and denied management controls`,async()=>{await fresh(language);await page.evaluate(()=>{window.f06.admin=true;window.f06.render();});await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(button=>button.textContent===`Controlled Org · ${window.f06.copy().enumStatus_ACTIVE}`));await page.evaluate(()=>[...document.querySelectorAll('button')].find(button=>button.textContent===`Controlled Org · ${window.f06.copy().enumStatus_ACTIVE}`).click());await page.waitForFunction(()=>document.querySelector('.adm-panel h3')?.textContent==='Controlled Org');assert.equal(await page.evaluate(()=>[...document.querySelectorAll('button')].some(button=>button.textContent===window.f06.copy().delete||button.textContent===window.f06.copy().disable)),false);assert.ok(await page.evaluate(()=>!document.body.textContent.includes('undefined')));});
 await run('Admin unknown create survives changing selected organization and retries identical facts',async()=>{await fresh('en');await page.evaluate(()=>{window.f06.admin=true;window.f06.unknown=true;window.f06.render();});await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(button=>button.textContent===`Controlled Org · ${window.f06.copy().enumStatus_ACTIVE}`));await page.evaluate(()=>{const form=[...document.querySelectorAll('form')].find(item=>item.querySelector('h4')?.textContent===window.f06.copy().create);const inputs=form.querySelectorAll('input');for(const [input,value] of [[inputs[0],'Controlled New Org'],[inputs[1],window.f06.user]]){Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(input,value);input.dispatchEvent(new Event('input',{bubbles:true}));}});await click('create');await page.waitForFunction(()=>document.querySelector('[role=alert]')?.textContent===window.f06.copy().unknown);await page.evaluate(()=>[...document.querySelectorAll('button')].find(button=>button.textContent===`Other Org · ${window.f06.copy().enumStatus_DISABLED}`).click());await page.waitForFunction(()=>document.querySelector('.adm-panel h3')?.textContent==='Other Org');assert.ok(await page.evaluate(()=>[...document.querySelectorAll('[role=alert]')].some(item=>item.textContent===window.f06.copy().unknown)));await page.evaluate(()=>{window.f06.unknown=false;});await click('retry');await page.waitForFunction(()=>[...document.querySelectorAll('[role=status]')].some(item=>item.textContent===window.f06.copy().saved));assert.ok(await page.evaluate(()=>{const calls=window.f06.calls.filter(call=>call.method==='POST');return calls.length===2&&calls[0].key===calls[1].key&&calls[0].body===calls[1].body;}));});
 await run('Organization purchase excludes a channel absent from real provider capabilities',async()=>{await fresh('en');await page.evaluate(()=>{window.f06.purchase=true;window.f06.authorizationVersion++;window.f06.capabilityChannels=['WECHAT'];window.f06.render();});await page.waitForFunction(()=>[...document.querySelectorAll('form')].some(form=>form.querySelector('h4')?.textContent===window.f06.copy().purchase));await page.waitForFunction(()=>document.body.textContent.includes(window.f06.copy().noPurchaseChannel));assert.equal(await page.evaluate(()=>{const form=[...document.querySelectorAll('form')].find(form=>form.querySelector('h4')?.textContent===window.f06.copy().purchase);return [...form.querySelectorAll('select')[1].options].some(option=>option.value==='ALIPAY');}),false);assert.equal(await page.evaluate(()=>window.f06.calls.some(call=>call.path.endsWith('/payment-capabilities'))),true);assert.equal(await page.evaluate(()=>window.f06.calls.some(call=>call.method==='POST')),false);});
 await run('Paused product cannot initiate organization purchase',async()=>{await fresh('en');await page.evaluate(()=>{window.f06.purchase=true;window.f06.authorizationVersion++;window.f06.productStatus='PRODUCT_PAUSED';window.f06.render();});await page.waitForFunction(()=>[...document.querySelectorAll('form')].some(form=>form.querySelector('h4')?.textContent===window.f06.copy().purchase));await page.waitForFunction(()=>document.body.textContent.includes(window.f06.copy().noPurchaseChannel));assert.ok(await page.evaluate(()=>[...document.querySelectorAll('form')].find(form=>form.querySelector('h4')?.textContent===window.f06.copy().purchase).querySelector('button').disabled));assert.equal(await page.evaluate(()=>window.f06.calls.some(call=>call.method==='POST')),false);});
 for(const language of ['zh','en','ja'])for(const testOnly of [false,true])await run(`${language}: ${testOnly?'controlled test':'formal'} available channels retain current payment mode`,async()=>{await fresh(language);await page.evaluate(testOnly=>{window.f06.purchase=true;window.f06.authorizationVersion++;window.f06.testOnly=testOnly;window.f06.render();},testOnly);await page.waitForFunction(()=>[...document.querySelectorAll('select option')].some(option=>option.value==='controlled-org-product'));await page.evaluate(()=>{const form=[...document.querySelectorAll('form')].find(form=>form.querySelector('h4')?.textContent===window.f06.copy().purchase);const product=form.querySelectorAll('select')[0];product.value='controlled-org-product';product.dispatchEvent(new Event('change',{bubbles:true}));});await page.waitForFunction(()=>[...document.querySelectorAll('select option')].some(option=>option.value==='ALIPAY'));assert.ok(await page.evaluate(testOnly=>document.body.textContent.includes(window.f06.copy()[testOnly?'testCollection':'formalCollection']),testOnly));assert.equal(await page.evaluate(()=>[...document.querySelectorAll('form')].find(form=>form.querySelector('h4')?.textContent===window.f06.copy().purchase).querySelector('button').disabled),false);assert.equal(await page.evaluate(()=>window.f06.calls.some(call=>call.method==='POST')),false);});
 for (const language of ['zh', 'en', 'ja']) await run(`${language}: model policy labels and current read-only permission`, async () => {
  await fresh(language);
  await page.evaluate(() => { window.f06.modelPolicy = true; window.f06.readonly = true; window.f06.authorizationVersion++; window.f06.render(); });
  await page.waitForFunction(() => document.querySelector('.ap-org-model-policy select'));
  assert.ok(await page.evaluate(() => {
   const section = document.querySelector('.ap-org-model-policy'), copy = window.f06.copy();
   return section.querySelector('h3').textContent === copy.modelPolicy && section.querySelector('fieldset').disabled &&
    [...section.querySelector('select').options].map(option => option.textContent).join('|') === [copy.ALLOW_ALL, copy.ALLOW_LIST, copy.DENY_LIST].join('|') &&
    ![...section.querySelectorAll('button')].some(button => button.textContent === copy.save) && !section.textContent.includes('undefined');
  }));
  assert.equal(await page.evaluate(() => window.f06.calls.some(call => call.method === 'PUT')), false);
 });
 await run('Model policy committed PUT unknown ACK retries original CAS, models and key after language change', async () => {
  await fresh('en');
  await page.evaluate(() => { window.f06.modelPolicy = true; window.f06.authorizationVersion++; window.f06.render(); });
  await page.waitForFunction(() => document.querySelector('.ap-org-model-policy select'));
  await page.select('.ap-org-model-policy select', 'ALLOW_LIST');
  await page.click('.ap-org-model-policy input[type=checkbox]');
  await page.type('.ap-org-model-policy input:not([type=checkbox])', 'Controlled policy edit');
  await page.evaluate(() => { window.f06.unknown = true; document.querySelector('.ap-org-model-policy form button').click(); });
  await page.waitForFunction(() => document.querySelector('.ap-org-model-policy [role=alert]')?.textContent === window.f06.copy().unknown);
  assert.ok(await page.evaluate(() => window.f06.policy.version === 1 && document.querySelector('.ap-org-model-policy fieldset').disabled));
  await page.evaluate(() => { window.f06.unknown = false; window.f06.language = 'ja'; window.f06.render(); });
  await page.waitForFunction(() => document.querySelector('.ap-org-model-policy form button')?.textContent === window.f06.copy().retry);
  await page.click('.ap-org-model-policy form button');
  await page.waitForFunction(() => document.querySelector('.ap-org-model-policy [role=status]')?.textContent === window.f06.copy().saved);
  assert.ok(await page.evaluate(() => {
   const calls = window.f06.calls.filter(call => call.method === 'PUT'), body = JSON.parse(calls[0].body);
   return calls.length === 2 && calls[0].key === calls[1].key && calls[0].body === calls[1].body && body.expected_version === 0 &&
    body.mode === 'ALLOW_LIST' && body.model_ids.length === 1 && body.model_ids[0] === window.f06.other && window.f06.policy.version === 1;
  }));
 });
 for (const name of ['null', 'missing', 'invalid', 'duplicate', 'display', 'upstream']) for (const refresh of [false, true]) {
  await run(`Model policy ${refresh ? 'refresh' : 'initial'} ${name} catalog becomes unavailable without a PUT`, async () => {
   await fresh('en');
   if (refresh) {
    await page.evaluate(() => { window.f06.modelPolicy = true; window.f06.policy.mode = 'ALLOW_LIST'; window.f06.authorizationVersion++; window.f06.render(); });
    await page.waitForFunction(() => document.querySelector('.ap-org-model-policy input[type=checkbox]'));
   }
   await page.evaluate(({ name, refresh }) => {
    const model = { model_id: window.f06.other, display_name: 'Controlled model' };
    window.f06.catalogItems = name === 'null' ? [null] : name === 'missing' ? [{}] : name === 'invalid' ? [{ model_id: 'not-a-uuid' }] : name === 'duplicate' ? [model, { ...model }] : name === 'display' ? [{ ...model, display_name: {} }] : [{ ...model, upstream_model_ref: [] }];
    if (refresh) document.querySelector('.ap-org-model-policy > button').click();
    else { window.f06.modelPolicy = true; window.f06.policy.mode = 'ALLOW_LIST'; window.f06.authorizationVersion++; window.f06.render(); }
   }, { name, refresh });
   await page.waitForFunction(() => document.querySelector('.ap-org-model-policy [role=alert]')?.textContent === window.f06.copy().unavailable);
   assert.equal(await page.evaluate(() => !!document.querySelector('.ap-org-model-policy form')), false);
   assert.equal(await page.evaluate(() => window.f06.calls.some(call => call.method === 'PUT')), false);
  });
 }
 for(const language of ['zh','en','ja']) await run(`${language}: real organization role/status/component labels preserve protocol options and unknown diagnostics`,async()=>{
  await fresh(language);await page.evaluate(()=>{window.f06.enumMode=true;window.f06.authorizationVersion++;window.f06.render();});
  await page.waitForFunction(()=>document.body.textContent.includes('CONTROLLED_UNKNOWN_STATE') && document.body.textContent.includes(window.f06.copy().enumStatus_SCHEDULED));
  assert.ok(await page.evaluate(()=>{const c=window.f06.copy(),text=document.body.textContent;return ['OWNER','MEMBER','ORG_ADMIN','BILLING_ADMIN','AUDITOR','UNIT_ADMIN'].every(v=>text.includes(c['enumRole_'+v])) && ['ACTIVE','SCHEDULED','SUSPENDED','FROZEN'].every(v=>text.includes(c['enumStatus_'+v])) && text.includes(c.enumComponent_BASE) && text.includes(c.enumComponent_BONUS) && text.includes('Controlled package');}));
  assert.ok(await page.evaluate(()=>{const form=[...document.querySelectorAll('form')].find(f=>f.querySelector('h4')?.textContent===window.f06.copy().allocate),select=form.querySelector('select');return JSON.stringify([...select.options].map(o=>o.value).filter(Boolean))===JSON.stringify(['BASE','BONUS']) && ['BASE','BONUS'].every(v=>[...select.options].some(o=>o.value===v && o.textContent===window.f06.copy()['enumComponent_'+v]));}));
  assert.equal(await page.evaluate(()=>window.f06.calls.some(c=>c.method!=='GET')),false);
 });
 for(const language of ['zh','en','ja']) await run(`${language}: disabled organization reads its real suspended member DTO`,async()=>{
  await fresh(language);await page.evaluate(()=>{window.f06.enumMode=true;window.f06.admin=true;window.f06.render();});
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===`Other Org · ${window.f06.copy().enumStatus_DISABLED}`));
  await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===`Other Org · ${window.f06.copy().enumStatus_DISABLED}`).click());
  await page.waitForFunction(()=>document.body.textContent.includes(window.f06.copy().enumStatus_SUSPENDED));
  assert.ok(await page.evaluate(()=>!document.querySelector('[role=alert]') && window.f06.calls.some(c=>c.path.endsWith('/members')) && !window.f06.calls.some(c=>c.method!=='GET')));
 });
 for(const language of ['zh','en','ja']) for(const suffix of ['/me/organizations','/units','/balance']) await run(`${language}: ${suffix} read 403 differs from read 503 without a write`,async()=>{
  await fresh(language);
  for(const status of [403,503]){
   await page.evaluate(({suffix,status})=>{window.f06.readError={path:suffix,status};window.f06.contextVersion++;window.f06.render();},{suffix,status});
   await page.waitForFunction(status=>[...document.querySelectorAll('[role=alert]')].some(a=>a.firstChild?.textContent===window.f06.copy()[status===403?'permissions':'unavailable']),{},status);
  }
  assert.ok(await page.evaluate(suffix=>window.f06.calls.some(c=>c.method==='GET'&&c.path.endsWith(suffix)) && window.f06.calls.every(c=>c.method==='GET'),suffix));
 });
 await run('scheduled role grant retains original future facts and unknown retry across label language change',async()=>{
  await fresh('en');await page.evaluate(()=>{window.f06.enumMode=true;window.f06.authorizationVersion++;window.f06.render();});
  await page.waitForFunction(()=>[...document.querySelectorAll('form')].some(f=>f.querySelector('h4')?.textContent===window.f06.copy().grantRole));
  await page.waitForFunction(()=>{const f=[...document.querySelectorAll('form')].find(f=>f.querySelector('h4')?.textContent===window.f06.copy().grantRole);return f && [...f.querySelector('select').options].some(o=>o.value===window.f06.user);});
  await page.evaluate(()=>{const f=[...document.querySelectorAll('form')].find(f=>f.querySelector('h4')?.textContent===window.f06.copy().grantRole),selects=f.querySelectorAll('select'),inputs=f.querySelectorAll('input');for(const [select,value] of [[selects[0],window.f06.user],[selects[1],'AUDITOR']]){select.value=value;select.dispatchEvent(new Event('change',{bubbles:true}));}for(const [input,value] of [[inputs[0],'2099-01-01T00:00'],[inputs[2],'Controlled future role']]){Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(input,value);input.dispatchEvent(new Event('input',{bubbles:true}));input.dispatchEvent(new Event('change',{bubbles:true}));}window.f06.unknown=true;});
  await click('grantRole');await page.waitForFunction(()=>[...document.querySelectorAll('[role=alert]')].some(a=>a.textContent===window.f06.copy().unknown));
  await page.evaluate(()=>{window.f06.language='ja';window.f06.unknown=false;window.f06.render();});await click('retry');
  await page.waitForFunction(()=>[...document.querySelectorAll('[role=status]')].some(a=>a.textContent===window.f06.copy().saved));
  assert.ok(await page.evaluate(()=>{const calls=window.f06.calls.filter(c=>c.method==='POST'&&c.path.endsWith('/role-bindings'));return calls.length===2 && calls[0].key===calls[1].key && calls[0].body===calls[1].body && JSON.parse(calls[1].body).starts_at===new Date('2099-01-01T00:00').toISOString() && JSON.parse(calls[1].body).role_code==='AUDITOR';}));
 });
 await run('scheduled binding uses the existing revoke confirmation and preserves exact binding identity',async()=>{
  await fresh('en');await page.evaluate(()=>{window.f06.enumMode=true;window.f06.authorizationVersion++;window.f06.render();});
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent===window.f06.copy().revoke));
  await page.evaluate(()=>new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve))));
  await click('revoke');
  await page.waitForFunction(()=>document.querySelector('[role=alertdialog]'));
  assert.ok(await page.evaluate(()=>document.querySelector('[role=alertdialog]').textContent.includes(window.f06.unit) && !window.f06.calls.some(c=>c.method==='DELETE')));
  assert.ok(await page.evaluate(()=>{const b=[...document.querySelectorAll('[role=alertdialog] button')].find(b=>b.textContent===window.f06.copy().confirmAction);b.click();b.click();return true;}));
  await page.waitForFunction(()=>document.body.textContent.includes(window.f06.copy().enumStatus_REVOKED));
  assert.ok(await page.evaluate(()=>{const calls=window.f06.calls.filter(c=>c.method==='DELETE');return calls.length===1 && calls[0].path.endsWith('/role-bindings/'+window.f06.unit) && typeof calls[0].key==='string' && calls[0].key.length>=16;}));
 });
 for(const language of ['zh','en','ja']) await run(`${language}: model-policy read permission errors preserve unavailable and write gates`,async()=>{
  await fresh(language);await page.evaluate(()=>{window.f06.modelPolicy=true;window.f06.authorizationVersion++;window.f06.render();});
  await page.waitForFunction(()=>document.querySelector('.ap-org-model-policy form'));
  for(const status of [403,503]) {
   await page.evaluate(status=>{window.f06.readError={path:'/model-policy',status};window.f06.authorizationVersion++;window.f06.render();},status);
   await page.waitForFunction(status=>document.querySelector('.ap-org-model-policy [role=alert]')?.textContent===window.f06.copy()[status===403?'permissions':'unavailable'],{},status);
   assert.equal(await page.evaluate(()=>Boolean(document.querySelector('.ap-org-model-policy form'))),false);
  }
  assert.ok(await page.evaluate(()=>window.f06.calls.every(c=>c.method==='GET')));
 });
 console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
}catch(error){console.error(`FAIL after ${passed} PASS: ${error.stack || error.message}`);if(page)console.error(JSON.stringify(await page.evaluate(()=>({dialog:Boolean(document.querySelector('[role=alertdialog]')),alerts:[...document.querySelectorAll('[role=alert]')].map(a=>a.firstChild?.textContent),deletePaths:window.f06?.calls.filter(c=>c.method==='DELETE').map(c=>c.path),roleRevoked:window.f06?.roleRevoked,revokedLabelVisible:document.body.textContent.includes(window.f06?.copy().enumStatus_REVOKED)}))));process.exitCode=1;}
finally{if(browser)await browser.close();if(vite)await vite.close();fs.rmSync(profile,{recursive:true,force:true});}
