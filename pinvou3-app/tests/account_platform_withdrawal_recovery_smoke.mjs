import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import {createServer} from 'vite';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const chrome=process.env.CHROME||['C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe','C:/Program Files/Microsoft/Edge/Application/msedge.exe','/usr/bin/chromium','/usr/bin/google-chrome'].find(fs.existsSync);
assert.ok(chrome,'Browser unavailable; zero cases is not passing');
const profile=fs.mkdtempSync(path.join(os.tmpdir(),'original-withdrawal-'));let vite,browser,page,passed=0,failed=0;const errors=[];
try{
 vite=await createServer({root,configFile:false,appType:'mpa',logLevel:'error',plugins:[react()],server:{host:'127.0.0.1',port:0,watch:null,fs:{allow:[root],deny:[]}}});await vite.listen();
 browser=await puppeteer.launch({executablePath:chrome,headless:'new',userDataDir:profile,args:['--disable-gpu','--no-first-run']});page=await browser.newPage();page.setDefaultTimeout(6000);page.on('pageerror',e=>errors.push(e.message));
 const url=`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-withdrawal.html`;
 const fresh=async(mode,language='en')=>{errors.length=0;await page.goto(`${url}?mode=${mode}&language=${language}`);await page.waitForSelector('article');await page.waitForFunction(()=>!document.querySelector('[role=status]'));};
 const click=async key=>{assert.ok(await page.evaluate(key=>{const b=[...document.querySelectorAll('button')].find(x=>x.textContent===window.withdrawal.copy()[key]&&!x.disabled);if(!b)return false;b.click();return true;},key),`missing enabled ${key}`);};
 const saved=async()=>page.waitForFunction(()=>[...document.querySelectorAll('[role=status]')].some(x=>x.textContent===window.withdrawal.copy().saved)||document.querySelector('article')?.textContent.includes('WITHDRAWN'));
 const calls=()=>page.evaluate(()=>window.withdrawal.calls);
 const original=rows=>{const writes=rows.filter(x=>x.method==='POST');assert.equal(writes.length,1);assert.ok(writes[0].url.endsWith('/actions/withdraw'));assert.deepEqual(JSON.parse(writes[0].body),{expected_version:4,command_protocol:1});assert.equal(writes[0].headers['X-Pinvou-Approval-Action-Control-Version'],'1');return writes;};
 const run=async(name,fn)=>{try{await fn();assert.deepEqual(errors,[]);passed++;console.log(`PASS ${name}`);}catch(e){failed++;console.log(`FAIL ${name}: ${e.message}`);}};
 for(const language of ['en','zh','ja'])await run(`${language} actual WITHDRAW normal ACK retains original facts`,async()=>{await fresh('ack',language);await click('withdraw');await saved();const rows=await calls();original(rows);assert.equal(rows.filter(x=>x.url.includes('withdraw-result')).length,0);});
 await run('real client + mounted hook recover committed lost ACK by readonly receipt',async()=>{await fresh('lost');await click('withdraw');await saved();const rows=await calls();original(rows);assert.equal(rows.filter(x=>x.url.includes('withdraw-result')).length,1);assert.equal(await page.$('[role=alert]'),null);});
 await run('retained unknown survives failed proof; control change never replaces original write',async()=>{await fresh('unknown');await click('withdraw');await page.waitForFunction(()=>[...document.querySelectorAll('button')].some(x=>x.textContent===window.withdrawal.copy().retry&&!x.disabled));assert.equal(await page.evaluate(()=>document.querySelector('[role=alert]')?.textContent),await page.evaluate(()=>window.withdrawal.copy().unknown));await page.evaluate(()=>{window.withdrawal.mode='lost';window.withdrawal.version=2;});await click('retry');await saved();const rows=await calls();original(rows);const reads=rows.filter(x=>x.url.includes('withdraw-result'));assert.equal(reads.length,2);for(const r of reads){assert.equal(r.method,'GET');const q=new URL(r.url,'http://controlled.invalid').searchParams;assert.equal(q.get('expected_version'),'4');assert.equal(q.get('origin_action_control_version'),'1');}assert.equal(reads[1].headers['X-Pinvou-Approval-Action-Control-Version'],'2');});
 console.log(`SUMMARY ${passed} PASS ${failed} FAIL 0 SKIP`);if(failed)process.exitCode=1;
}finally{await browser?.close();await vite?.close();fs.rmSync(profile,{recursive:true,force:true});}
