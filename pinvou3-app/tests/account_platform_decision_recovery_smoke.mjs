import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import {createServer} from './helpers/vite-fixture.mjs';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const chrome=process.env.CHROME||['C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe','C:/Program Files/Microsoft/Edge/Application/msedge.exe','/usr/bin/chromium','/usr/bin/google-chrome'].find(fs.existsSync);
assert.ok(chrome,'Browser unavailable; zero cases is not passing');
const profile=fs.mkdtempSync(path.join(os.tmpdir(),'original-decision-'));let vite,browser,page,passed=0,failed=0;const errors=[];
try{
 vite=await createServer({root,configFile:false,appType:'mpa',logLevel:'error',plugins:[react()],server:{host:'127.0.0.1',port:0,watch:null,fs:{allow:[root],deny:[]}}});await vite.listen();
 browser=await puppeteer.launch({executablePath:chrome,headless:'new',userDataDir:profile,args:['--disable-gpu','--no-first-run']});page=await browser.newPage();page.setDefaultTimeout(6000);page.on('pageerror',e=>errors.push(e.message));
 const url=`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-decision.html`;
 const fresh=async(mode,language='en',action='APPROVE')=>{errors.length=0;await page.goto(`${url}?mode=${mode}&language=${language}&action=${action}`);await page.waitForSelector('article');await page.waitForFunction(()=>!document.querySelector('[role=status]'));await page.evaluate(()=>{const input=document.querySelector('textarea');const set=Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype,'value').set;set.call(input,window.decision.reason);input.dispatchEvent(new Event('input',{bubbles:true}));});};
 const click=async key=>{assert.ok(await page.evaluate(key=>{const b=[...document.querySelectorAll('button')].find(x=>x.textContent===window.decision.copy()[key]&&!x.disabled);if(!b)return false;b.click();return true;},key),`missing enabled ${key}`);};
 const saved=async()=>page.waitForFunction(()=>[...document.querySelectorAll('[role=status]')].some(x=>x.textContent===window.decision.copy().saved)||document.querySelector('article')?.textContent.includes(window.decision.action==='APPROVE'?'APPROVED':'REJECTED'));
 const calls=()=>page.evaluate(()=>window.decision.calls);
 const original=(rows,action='APPROVE')=>{const writes=rows.filter(x=>x.method==='POST');assert.equal(writes.length,1);assert.ok(writes[0].url.endsWith('/actions/decide'));const body=JSON.parse(writes[0].body);assert.equal(body.expected_version,4);assert.equal(body.command_protocol,1);assert.equal(body.action_control_version,1);assert.equal(body.decision,action==='APPROVE'?'APPROVED':'REJECTED');assert.equal(body.reason," \u0085操作理由：请批准 😀\u0085 " );assert.equal(writes[0].headers['X-Pinvou-Approval-Action-Control-Version'],'1');return writes;};
 const run=async(name,fn)=>{try{await fn();assert.deepEqual(errors,[]);passed++;console.log(`PASS ${name}`);}catch(e){failed++;console.log(`FAIL ${name}: ${e.message}`);}};
 for(const action of ['APPROVE','REJECT'])for(const language of ['en','zh','ja'])await run(`${language} actual ${action} normal ACK/raw reason`,async()=>{
  await fresh('ack',language,action);await click(action==='APPROVE'?'approve':'reject');await saved();const rows=await calls();original(rows,action);assert.equal(rows.filter(x=>x.url.includes('decision-result')).length,0);
 });
 for(const action of ['APPROVE','REJECT'])await run(`mounted ${action} lost ACK confirms exact immutable receipt`,async()=>{
  await fresh('lost','en',action);await click(action==='APPROVE'?'approve':'reject');await saved();const rows=await calls();original(rows,action);assert.equal(rows.filter(x=>x.url.includes('decision-result')).length,1);assert.equal(await page.$('[role=alert]'),null);
 });
 for(const action of ['APPROVE','REJECT'])await run(`retained ${action} unknown survives missing proof and confirms original after control change`,async()=>{
  await fresh('unknown','en',action);await click(action==='APPROVE'?'approve':'reject');await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(x=>x.textContent===window.decision.copy().retry&&!x.disabled));assert.equal(await page.evaluate(()=>document.querySelector('[role=alert]')?.textContent),await page.evaluate(()=>window.decision.copy().unknown));await page.evaluate(()=>{window.decision.mode='lost';window.decision.version=2;});await click('retry');await saved();const rows=await calls();original(rows,action);const reads=rows.filter(x=>x.url.includes('decision-result'));assert.equal(reads.length,2);for(const r of reads){assert.equal(r.method,'GET');const q=new URL(r.url,'http://controlled.invalid').searchParams;assert.equal(q.get('expected_version'),'4');assert.equal(q.get('origin_action_control_version'),'1');assert.equal(q.get('decision'),action==='APPROVE'?'APPROVED':'REJECTED');assert.equal(q.get('reason_digest').length,64);}assert.equal(reads[1].headers['X-Pinvou-Approval-Action-Control-Version'],'2');
 });
 console.log(`SUMMARY ${passed} PASS ${failed} FAIL 0 SKIP`);if(failed)process.exitCode=1;
}finally{await browser?.close();await vite?.close();fs.rmSync(profile,{recursive:true,force:true});}
