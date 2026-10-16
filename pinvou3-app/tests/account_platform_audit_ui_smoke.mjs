import assert from 'node:assert/strict';
import fs from 'node:fs';
import {createServer} from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
const server=await createServer({root:import.meta.dirname+'/..',configFile:false,plugins:[react()],cacheDir:process.env.F17_VITE_CACHE||import.meta.dirname+'/../node_modules/.vite-audit-test',server:{host:'127.0.0.1',port:0,fs:{allow:[import.meta.dirname+'/..'],deny:['**/.env*','**/*.{pem,crt,key}']}}});await server.listen();
const executablePath=process.env.F17_BROWSER_EXECUTABLE||process.env.CHROME||[
 'C:/Program Files/Google/Chrome/Application/chrome.exe','C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
 'C:/Program Files/Microsoft/Edge/Application/msedge.exe','C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
 '/snap/bin/chromium','/usr/bin/chromium','/usr/bin/chromium-browser','/usr/bin/google-chrome','/usr/bin/google-chrome-stable',
 '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
].find(fs.existsSync);
assert.ok(executablePath,'Browser unavailable; audit UI validation not performed');
const browser=await puppeteer.launch({executablePath,headless:true,args:['--no-sandbox','--disable-gpu']});
const results=[];
const run=async(name,fn)=>{const page=await browser.newPage();page.setDefaultTimeout(8000);const errors=[];page.on('pageerror',e=>errors.push(e.message));try{await fn(page);assert.deepEqual(errors,[]);results.push({name,pass:true});}catch(e){results.push({name,pass:false,error:e.message,requests:await page.evaluate(()=>window.f17?.calls.map(({url,method})=>({url,method})) || [])});}finally{await page.close();}};
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

 await run('initial recent-auth rejection restores a correctable draft without automatically creating an export',async page=>{
  await open(page,'recentCreate=1');await fill(page);await button(page,'export');
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.reauthenticate&&!b.disabled));
  const first=await page.evaluate(()=>window.f17.calls.find(c=>c.url.endsWith('/audit-exports')&&c.method==='POST'));
  await button(page,'reauthenticate');await page.waitForFunction(()=>window.f17.reauthURL);
  const recoveryURL=await page.evaluate(()=>window.f17.reauthURL);
  await page.goto(server.resolvedUrls.local[0]+'tests/account_platform_audit_fixture.html?'+new URL(recoveryURL,'https://controlled.invalid').searchParams.toString()+'&recentCreate=1&reauthenticated=1');
  await page.waitForFunction(()=>window.f17&&document.querySelector('input[minlength]'));
  assert.equal(await page.evaluate(()=>window.f17.createCount),0);
  assert.equal(await page.$eval('input[minlength]',input=>input.value),'Controlled audit investigation');
  assert.equal(await page.$eval('input[minlength]',input=>input.disabled),false);
  await fill(page,'Corrected draft after authentication');await button(page,'export');
  await page.waitForFunction(()=>document.body.textContent.includes('controlled-page-only-key'));
  const next=await page.evaluate(()=>window.f17.calls.find(c=>c.url.endsWith('/audit-exports')&&c.method==='POST'));
  assert.notEqual(next.headers['Idempotency-Key'],first.headers['Idempotency-Key']);
  assert.equal(await page.evaluate(()=>sessionStorage.length),0);
 });
 await run('unknown then recent-auth rejection recovers the original immutable body and key only on explicit retry',async page=>{
  await open(page,'unknownRecent=1');await fill(page);await button(page,'export');await page.waitForSelector('[role=alert]');
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.retryExport&&!b.disabled));await button(page,'retryExport');
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.reauthenticate&&!b.disabled));
  const calls=await page.evaluate(()=>window.f17.calls.filter(c=>c.url.endsWith('/audit-exports')&&c.method==='POST'));
  assert.equal(calls.length,2);assert.equal(calls[1].body,calls[0].body);assert.equal(calls[1].headers['Idempotency-Key'],calls[0].headers['Idempotency-Key']);
  await button(page,'reauthenticate');await page.waitForFunction(()=>window.f17.reauthURL);
  const saved=await page.evaluate(()=>({url:window.f17.reauthURL,record:JSON.parse(sessionStorage.getItem('pinvou.admin.audit-export.stepup.'+window.f17.id))}));
  assert.equal(saved.record.outcomeUnknown,true);assert.equal(saved.record.attempt.idempotencyKey,calls[0].headers['Idempotency-Key']);
  await page.goto(server.resolvedUrls.local[0]+'tests/account_platform_audit_fixture.html?'+new URL(saved.url,'https://controlled.invalid').searchParams.toString()+'&unknownRecent=1&reauthenticated=1');
  await page.waitForFunction(()=>window.f17&&document.querySelector('input[minlength]'));
  assert.equal(await page.evaluate(()=>window.f17.createCount),0);assert.equal(await page.$eval('input[minlength]',input=>input.disabled),true);
  await button(page,'retryExport');await page.waitForFunction(()=>document.body.textContent.includes('controlled-page-only-key'));
  const resumed=await page.evaluate(()=>window.f17.calls.find(c=>c.url.endsWith('/audit-exports')&&c.method==='POST'));
  assert.equal(resumed.body,calls[0].body);assert.equal(resumed.headers['Idempotency-Key'],calls[0].headers['Idempotency-Key']);
  assert.equal(await page.evaluate(()=>sessionStorage.length),0);
 });
 await run('known job restores by GET with no key or ticket stored; receipt recovery and ticket issuance remain explicit',async page=>{
  await open(page,'ticketRecent=1');await fill(page);await button(page,'export');await page.waitForFunction(()=>document.body.textContent.includes('controlled-page-only-key'));
  const original=await page.evaluate(()=>window.f17.calls.find(c=>c.url.endsWith('/audit-exports')&&c.method==='POST'));
  await button(page,'download');await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.reauthenticate&&!b.disabled));
  await button(page,'reauthenticate');await page.waitForFunction(()=>window.f17.reauthURL);
  const saved=await page.evaluate(()=>({url:window.f17.reauthURL,raw:sessionStorage.getItem('pinvou.admin.audit-export.stepup.'+window.f17.id)}));
  assert.equal(JSON.parse(saved.raw).exportID,'00000000-0000-4000-8000-000000000017');
  assert.equal(saved.raw.includes('controlled-page-only-key'),false);assert.equal(saved.raw.includes('download_token'),false);
  await page.goto(server.resolvedUrls.local[0]+'tests/account_platform_audit_fixture.html?'+new URL(saved.url,'https://controlled.invalid').searchParams.toString()+'&ticketRecent=1&reauthenticated=1');
  await page.waitForFunction(()=>window.f17&&document.body.textContent.includes('READY'));
  assert.equal(await page.evaluate(()=>window.f17.calls.some(c=>c.method!=='GET'&&c.url.startsWith('/api/admin/account/admin/v1/audit-exports'))),false);
  assert.ok(await page.evaluate(()=>{const writes=window.f17.calls.filter(c=>c.method!=='GET');return writes.length>0&&writes.every(c=>c.method==='POST'&&c.url==='/api/account/v1/account/bootstrap');}));
  assert.equal(await page.evaluate(()=>document.body.textContent.includes('controlled-page-only-key')),false);
  await button(page,'recoverOriginal');await page.waitForFunction(()=>document.body.textContent.includes('controlled-page-only-key'));
  const resumed=await page.evaluate(()=>window.f17.calls.find(c=>c.url.endsWith('/audit-exports')&&c.method==='POST'));
  assert.equal(resumed.body,original.body);assert.equal(resumed.headers['Idempotency-Key'],original.headers['Idempotency-Key']);
  await button(page,'download');await page.waitForFunction(()=>window.f17.downloaded);
  assert.equal(await page.evaluate(()=>window.f17.calls.filter(c=>c.url.endsWith('/download-tickets')).length),1);
  assert.equal(await page.evaluate(()=>sessionStorage.length),0);
 });
 await run('another authenticated account cannot recover or replace the original uncertain request',async page=>{
  await open(page,'unknownRecent=1');await fill(page);await button(page,'export');await page.waitForSelector('[role=alert]');
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.retryExport&&!b.disabled));await button(page,'retryExport');
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.reauthenticate&&!b.disabled));await button(page,'reauthenticate');await page.waitForFunction(()=>window.f17.reauthURL);
  const url=await page.evaluate(()=>window.f17.reauthURL);
  await page.goto(server.resolvedUrls.local[0]+'tests/account_platform_audit_fixture.html?'+new URL(url,'https://controlled.invalid').searchParams.toString()+'&unknownRecent=1&reauthenticated=1&otherActor=1');
  await page.waitForFunction(()=>window.f17&&document.body.textContent.includes(window.f17.copy.audit.recoveryUnavailable));
  assert.equal(await page.evaluate(()=>window.f17.calls.some(c=>c.method!=='GET'&&c.url.startsWith('/api/admin/account/admin/v1/audit-exports'))),false);
  assert.ok(await page.evaluate(()=>{const writes=window.f17.calls.filter(c=>c.method!=='GET');return writes.length>0&&writes.every(c=>c.method==='POST'&&c.url==='/api/account/v1/account/bootstrap');}));
  assert.equal(await page.$eval('input[minlength]',input=>input.disabled),true);
  assert.equal(await page.evaluate(()=>sessionStorage.length),1); // Original actor's pending request is not discarded.
 });
 await run('a storage failure does not navigate away or unfreeze the uncertain request',async page=>{
  await open(page,'unknownRecent=1');await fill(page);await button(page,'export');await page.waitForSelector('[role=alert]');
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.retryExport&&!b.disabled));await button(page,'retryExport');
  await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(b=>b.textContent.trim()===window.f17.copy.audit.reauthenticate&&!b.disabled));
  await page.evaluate(()=>{Storage.prototype.setItem=()=>{throw new Error('controlled storage quota');};});
  await button(page,'reauthenticate');await page.waitForFunction(()=>document.body.textContent.includes(window.f17.copy.audit.recoveryStorageFailed));
  assert.equal(await page.evaluate(()=>Boolean(window.f17.reauthURL)),false);assert.equal(await page.$eval('input[minlength]',input=>input.disabled),true);
  assert.equal(await page.evaluate(()=>window.f17.createCount),2);
 });
}finally{await browser.close();await server.close();}
const result={pass:results.filter(r=>r.pass).length,fail:results.filter(r=>!r.pass).length,results,limits:['Real React AdminPlatformView / AdminPlatformClient with controlled BFF; no real issuer/owner PostgreSQL or external spreadsheet/deployment/E2E.']};fs.writeFileSync(process.env.F17_REACT_RESULT||'audit-smoke-result.json',JSON.stringify(result,null,2));console.log(JSON.stringify(result));process.exitCode=result.fail?1:0;
