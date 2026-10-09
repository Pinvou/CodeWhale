import assert from 'node:assert/strict';
import fs from 'node:fs';
import {createServer} from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
const server=await createServer({root:import.meta.dirname+'/..',configFile:false,plugins:[react()],cacheDir:import.meta.dirname+'/../node_modules/.vite-audit-test',server:{host:'127.0.0.1',port:0,fs:{allow:[import.meta.dirname+'/..'],deny:['**/.env*','**/*.{pem,crt,key}']}}});await server.listen();
const browser=await puppeteer.launch({executablePath:'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',headless:true,args:['--no-sandbox','--disable-gpu']});
const results=[];
const run=async(name,fn)=>{const page=await browser.newPage();page.setDefaultTimeout(8000);const errors=[];page.on('pageerror',e=>errors.push(e.message));try{await fn(page);assert.deepEqual(errors,[]);results.push({name,pass:true});}catch(e){results.push({name,pass:false,error:e.message});}finally{await page.close();}};
const open=async(page,query='')=>{await page.goto(server.resolvedUrls.local[0]+'tests/account_platform_audit_fixture.html?section=audit&'+query);await page.waitForFunction(()=>window.f17&&document.querySelector('.adm-panel'));};
const fill=async(page,reason='Controlled audit investigation')=>{
 await page.evaluate(reason=>{
  const change=(input,value)=>{Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(input,value);input.dispatchEvent(new Event('input',{bubbles:true}));};
  const dates=[...document.querySelectorAll('input[type=datetime-local]')];change(dates[0],'2026-10-06T00:00');change(dates[1],'2026-10-07T00:00');
  change(document.querySelector('input[minlength]'),reason);
 },reason);
};
const button=async(page,key)=>page.evaluate(key=>[...document.querySelectorAll('button')].find(b=>b.textContent.trim()===window.f17.copy.audit[key]).click(),key);
try{
 for(const language of ['zh','en','ja'])await run('real AdminPlatformView audit / create / encrypted download '+language,async page=>{
  await open(page,'language='+language+(language==='en'?'&lost=1':''));
  assert.equal(await page.evaluate(()=>document.querySelector('input[type=checkbox]').disabled),true);
  await fill(page);await button(page,'export');
  if(language==='en'){await page.waitForSelector('[role=alert]');await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.retryExport&&!b.disabled));await button(page,'retryExport');}
  await page.waitForFunction(()=>document.body.textContent.includes('controlled-page-only-key'));
  assert.ok(await page.evaluate(()=>{const calls=window.f17.calls.filter(c=>c.url.endsWith('/audit-exports')&&c.method==='POST');return calls.every(c=>c.body===calls[0].body&&c.headers['Idempotency-Key']===calls[0].headers['Idempotency-Key']&&c.headers['X-CSRF-Token']==='controlled-csrf'&&c.cache==='no-store'&&JSON.parse(c.body).include_pii===false)&&!window.localStorage.length&&!Reflect.get(window, 'sessionStorage').length;}));
  await button(page,'download');await page.waitForFunction(()=>window.f17.downloaded);
  assert.ok(await page.evaluate(()=>{const c=window.f17.calls.find(c=>c.url.endsWith('/download'));return c.method==='GET'&&c.headers['X-Pinvou-Download-Token']==='a'.repeat(43)&&c.headers.Accept==='application/vnd.pinvou.audit-envelope+json'&&c.cache==='no-store'&&c.credentials==='same-origin';}));
 });
 await run('lost ticket ACK is not reissued or consumed after refresh',async page=>{
  await open(page,'ticketLost=1');await fill(page);await button(page,'export');await page.waitForFunction(()=>document.body.textContent.includes('controlled-page-only-key'));await button(page,'download');await page.waitForSelector('[role=alert]');
  await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent.trim()===window.f17.copy.refresh).click());
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.download&&b.disabled));
  assert.equal(await page.evaluate(()=>window.f17.calls.filter(c=>c.url.endsWith('/download-tickets')).length),1);assert.equal(await page.evaluate(()=>window.f17.calls.filter(c=>c.url.endsWith('/download')).length),0);
 });
 await run('read-only audit permission gives logs without export',async page=>{await open(page,'readOnly=1');assert.equal(await page.$('input[minlength]'),null);assert.equal(await page.evaluate(()=>window.f17.calls.some(c=>c.method==='POST'&&c.url.includes('/audit-'))),false);});
 await run('no permission hides audit and makes no audit calls',async page=>{await page.goto(server.resolvedUrls.local[0]+'tests/account_platform_audit_fixture.html?section=audit&deny=1');await page.waitForFunction(()=>window.f17&&document.querySelector('[role=alert]'));assert.equal(await page.evaluate(()=>window.f17.calls.some(c=>c.url.includes('/audit-'))),false);});
 await run('500 Unicode codepoints can be entered and submitted through actual form',async page=>{await open(page);await fill(page,'');await page.type('input[minlength]','😀'.repeat(500));assert.equal(await page.$eval('input[minlength]',input=>Array.from(input.value).length),500);await button(page,'export');await page.waitForFunction(()=>window.f17.calls.some(c=>c.url.endsWith('/audit-exports')));assert.equal(await page.evaluate(()=>Array.from(JSON.parse(window.f17.calls.find(c=>c.url.endsWith('/audit-exports')).body).reason).length),500);});
 await run('501 Unicode codepoints rejected before create',async page=>{await open(page);await fill(page,'😀'.repeat(501));await button(page,'export');await page.waitForSelector('[role=alert]');assert.equal(await page.evaluate(()=>window.f17.calls.some(c=>c.url.endsWith('/audit-exports'))),false);});
 await run('committed unknown then forbidden replay retains original immutable attempt',async page=>{
  await open(page,'unknownForbidden=1');await fill(page);await button(page,'export');await page.waitForSelector('[role=alert]');
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.retryExport&&!b.disabled));await button(page,'retryExport');await page.waitForFunction(()=>window.f17.createCount===2);await page.waitForFunction(()=>!document.querySelector('input[minlength]')||document.querySelector('input[minlength]').disabled||[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.export&&!b.disabled));
  assert.equal(await page.$eval('input[minlength]',input=>input.disabled),true);
  assert.equal(await page.$eval('input[type=checkbox]',input=>input.disabled),true);
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.retryExport&&!b.disabled));await button(page,'retryExport');await page.waitForFunction(()=>document.body.textContent.includes('controlled-page-only-key'));
  assert.equal(await page.evaluate(()=>window.f17.committedJob),true);
  assert.ok(await page.evaluate(()=>{const calls=window.f17.calls.filter(c=>c.url.endsWith('/audit-exports')&&c.method==='POST');return calls.length===3&&calls.every(c=>c.body===calls[0].body&&c.headers['Idempotency-Key']===calls[0].headers['Idempotency-Key']);}));
 });
 await run('initial definite rejection permits correction without unknown history',async page=>{await open(page,'initialForbidden=1');await fill(page);await button(page,'export');await page.waitForSelector('[role=alert]');await page.waitForFunction(()=>!document.querySelector('input[minlength]').disabled);await fill(page,'Corrected initial audit justification');await button(page,'export');await page.waitForFunction(()=>document.body.textContent.includes('controlled-page-only-key'));assert.ok(await page.evaluate(()=>{const calls=window.f17.calls.filter(c=>c.url.endsWith('/audit-exports')&&c.method==='POST');return calls.length===2&&calls[0].body!==calls[1].body&&calls[0].headers['Idempotency-Key']!==calls[1].headers['Idempotency-Key'];}));});
}finally{await browser.close();await server.close();}
const result={pass:results.filter(r=>r.pass).length,fail:results.filter(r=>!r.pass).length,results,limits:['Real React AdminPlatformView / AdminPlatformClient with controlled BFF; no real issuer/owner PostgreSQL or external spreadsheet/deployment/E2E.']};fs.writeFileSync(process.env.F17_REACT_RESULT||'audit-smoke-result.json',JSON.stringify(result,null,2));console.log(JSON.stringify(result));process.exitCode=result.fail?1:0;
