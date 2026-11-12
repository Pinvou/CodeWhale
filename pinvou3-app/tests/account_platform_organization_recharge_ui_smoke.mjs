import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const chrome=process.env.CHROME||['C:/Program Files/Google/Chrome/Application/chrome.exe','C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe','/usr/bin/chromium'].find(fs.existsSync);
assert.ok(chrome,'Browser unavailable; no React validation performed');
const profile=fs.mkdtempSync(path.join(os.tmpdir(),'pinvou-j16-'));let vite,browser,passed=0;
try {
 vite=await createServer({root,configFile:false,appType:'mpa',logLevel:'error',plugins:[react()],server:{host:'127.0.0.1',port:0,watch:null}});await vite.listen();
 browser=await puppeteer.launch({executablePath:chrome,headless:'new',userDataDir:profile,args:['--disable-gpu','--no-first-run']});
 const page=await browser.newPage();page.setDefaultTimeout(10000);const errors=[];page.on('pageerror',e=>errors.push(e.message));
 const url=`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-j16-organization.html`;
 const fresh=async(search='')=>{errors.length=0;await page.goto(url+search);await page.waitForFunction(()=>window.j16&&document.querySelector('button'));};
 const clickNotice=()=>page.evaluate(()=>{const b=[...document.querySelectorAll('button')].find(b=>b.textContent==='Recharge now');if(!b)throw Error('notice CTA missing');b.click();});
 await fresh();await clickNotice();await page.waitForSelector('[data-organization-purchase]');
 await page.waitForFunction(()=>document.activeElement?.hasAttribute('data-organization-purchase'));
 assert.equal(await page.evaluate(()=>window.j16.calls.some(c=>c.path==='/v1/recharge/products'||c.path.startsWith('/v1/me/balance'))),false);
 assert.equal(await page.evaluate(()=>window.j16.calls.some(c=>c.method==='POST')),false);passed++;console.log('PASS actual Timeline -> shared host -> Popover current organization catalogue and purchase focus');
 await page.select('[data-organization-purchase] form.ap-org-action select:nth-of-type(1)','current-product');
 // Selects are in separate labels, so use their ordered handles.
 const selects=await page.$$('[data-organization-purchase] form.ap-org-action select');await selects[1].select('ALIPAY');
 await page.$eval('[data-organization-purchase] form.ap-org-action',form=>form.requestSubmit());
 await page.waitForFunction(()=>document.querySelector('[data-organization-purchase] form.ap-org-action fieldset')?.disabled);
 await page.evaluate(()=>{window.j16.section=document.querySelector('[data-organization-purchase]');window.j16.beforeAuth=window.j16.bootstrapReads;window.j16.before=window.j16.calls.filter(c=>c.path.endsWith('/recharge/products')).length;});
 await page.click('.ap-center-tabs button:first-child');
 await page.waitForFunction(()=>window.j16.section.closest('[hidden]'));
 await clickNotice();await page.waitForFunction(()=>document.activeElement?.hasAttribute('data-organization-purchase'));
 assert.equal(await page.evaluate(()=>window.j16.section===document.querySelector('[data-organization-purchase]')),true);
 assert.equal(await page.evaluate(()=>window.j16.calls.filter(c=>c.path.endsWith('/recharge/products')).length===window.j16.before),true);
 assert.equal(await page.evaluate(()=>window.j16.bootstrapReads===window.j16.beforeAuth+1),true,'only the explicit host authority read; focus must not call View.load');
 await page.$eval('[data-organization-purchase] form.ap-org-action',form=>form.requestSubmit());
 await page.waitForFunction(()=>window.j16.calls.filter(c=>c.method==='POST').length===2);
 const writes=await page.evaluate(()=>window.j16.calls.filter(c=>c.method==='POST'));assert.equal(writes[0].key,writes[1].key);assert.deepEqual(writes[0].body,writes[1].body);assert.ok(writes[0].path.includes(windowOrg()));
 passed++;console.log('PASS repeated CTA preserves actual mounted UNKNOWN form, original facts/key and catalogue read count');
 for(const changed of ['denied','other']){
  await fresh();await clickNotice();await page.waitForSelector('[data-organization-purchase]');
  await page.evaluate(changed=>{window.j16[changed]=true;window.dispatchEvent(new Event('focus'));},changed);
  await page.waitForFunction(()=>!document.querySelector('[data-organization-purchase]')&&document.querySelector('[role=alert]'));
  assert.equal(await page.evaluate(()=>window.j16.calls.some(c=>c.method==='POST')),false);
  assert.equal(await page.evaluate(()=>window.j16.calls.some(c=>c.path.includes(window.j16.user))),false);
  passed++;console.log('PASS current '+changed+' refresh retires the notice target without other-organization fallback');
 }
 for(const bad of ['denied','other','authenticated']){
  await fresh();await page.evaluate(bad=>{window.j16[bad]=bad==='authenticated'?false:true;},bad);await clickNotice();await page.waitForFunction(()=>document.querySelector('[data-host-error]')?.textContent);
  assert.equal(await page.$('[data-organization-purchase]'),null);assert.equal(await page.evaluate(()=>window.j16.calls.length),0);passed++;console.log('PASS no fallback or purchase dispatch for '+bad);
 }
 await fresh('?personal-notice&personal-context');await clickNotice();
 await page.waitForSelector('.ap-finance.ap-recharge-only', {visible:true});
 await page.waitForFunction(()=>window.j16.personalProducts===1);
 assert.equal(await page.$('[data-organization-purchase]'),null);passed++;console.log('PASS personal notice opens the personal recharge tab and loads its catalogue');
 await fresh('?personal-notice');await clickNotice();
 await page.waitForSelector('.ap-center-tabs button:first-child[aria-pressed=true]');
 assert.equal(await page.$('[data-organization-purchase]'),null);passed++;console.log('PASS personal notice cannot open another context’s recharge tab');
 await fresh('?background-dialog');await clickNotice();
 await page.waitForFunction(()=>document.activeElement?.hasAttribute('data-organization-purchase'));
 assert.equal(await page.$eval('[data-background-dialog]',node=>Boolean(node.closest('[inert]'))),true);
 passed++;console.log('PASS isolated background dialog cannot block current organization purchase focus');
 assert.deepEqual(errors,[]);console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
} finally {if(browser)await browser.close();if(vite)await vite.close();assert.ok(profile.startsWith(path.join(os.tmpdir(),'pinvou-j16-')));fs.rmSync(profile,{recursive:true,force:true});}
function windowOrg(){return '0199a100-0001-7000-8000-000000000201';}
