// Actual React/client in a new native browser profile; Account HTTP is controlled.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const chrome=process.env.CHROME||['C:/Program Files/Google/Chrome/Application/chrome.exe','/usr/bin/chromium','/usr/bin/google-chrome'].find(fs.existsSync);
if(!chrome)throw new Error('Native browser unavailable; lookup UI not verified');
const profile=fs.mkdtempSync(path.join(os.tmpdir(),'pinvou-code-lookup-'));let vite,browser,passed=0;
const code='USR-1111-1111-1111-1111-11';const second='USR-2222-2222-2222-2222-22';
try{
 vite=await createServer({root,configFile:false,appType:'mpa',logLevel:'error',plugins:[react()],server:{host:'127.0.0.1',port:0,watch:null}});await vite.listen();
 browser=await puppeteer.launch({executablePath:chrome,headless:true,userDataDir:profile,args:['--no-sandbox','--disable-gpu','--no-first-run']});const page=await browser.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));
 await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f01-ui.html`);await page.waitForFunction(()=>window.f01);await page.evaluate(()=>window.f01.render('admin'));await page.waitForSelector('input[maxlength="64"]');
 await page.evaluate(()=>{const original=globalThis.fetch;globalThis.codeFixture={calls:[],fail:false,delay:false};globalThis.fetch=(url,options)=>{if(!url.includes('/user-code-lookups/')){if(url.endsWith('/status')&&codeFixture.holdStatus){codeFixture.holdStatus=false;return new Promise(resolve=>{codeFixture.releaseStatus=()=>resolve(original(url,options))})}return original(url,options)};const state=globalThis.codeFixture;state.calls.push({url,method:options?.method||'GET'});const code=decodeURIComponent(url.split('/').at(-1));const response=name=>new Response(JSON.stringify({user_id:window.f01.id,user_code:code.toUpperCase(),username:name,status:state.active?'ACTIVE':'DELETED'}),{status:200});if(state.fail)return Promise.resolve(new Response(JSON.stringify({code:'USER_NOT_FOUND',message:'the user lookup could not be completed'}),{status:404}));if(state.delay){state.delay=false;return new Promise(resolve=>{state.release=()=>resolve(response('lookup-stale'))})}return Promise.resolve(response('lookup-current'))}});
 const enter=async value=>{await page.click('input[maxlength="64"]',{clickCount:3});await page.keyboard.press('Backspace');await page.type('input[maxlength="64"]',value);};
 await enter(code.toLowerCase());await page.keyboard.press('Enter');await page.waitForFunction(()=>document.querySelector('tbody')?.textContent.includes('lookup-current'));
 if(!await page.evaluate(()=>document.querySelector('tbody').textContent.includes('DELETED')&&!document.querySelector('tbody button')&&codeFixture.calls.length===1&&codeFixture.calls[0].method==='GET'))throw new Error('exact deleted lookup or status-action fence lost');if(!await page.$eval('tbody code',(element,expected)=>element.textContent===expected,code))throw new Error('username hid permanent code column');console.log('PASS actual production client/UI exact lookup + explicit code column + deleted safe state');passed++;
 for(const language of ['zh','en','ja']){await page.select('header select',language);await page.waitForFunction(()=>document.querySelector('input[maxlength="64"]')?.closest('label')?.textContent===window.f01.copy().admin.userManagement.exactCode)}console.log('PASS three-language current UI copy');passed++;
 await page.evaluate(()=>{codeFixture.fail=true});await enter(second);await page.keyboard.press('Enter');await page.waitForFunction(()=>document.querySelector('[role=alert]')?.textContent.includes(window.f01.copy().admin.userManagement.codeLookupFailed));if(await page.evaluate(()=>document.querySelector('tbody')?.textContent.includes('lookup-current')))throw new Error('failed lookup retained prior identity');console.log('PASS uniform visible lookup failure without old identity');passed++;
 await page.evaluate(()=>{codeFixture.fail=false;codeFixture.delay=true});await enter(code);await page.keyboard.press('Enter');await page.waitForFunction(()=>typeof codeFixture.release==='function');await enter(second);await page.keyboard.press('Enter');await page.waitForFunction(()=>document.querySelector('tbody')?.textContent.includes('lookup-current'));await page.evaluate(()=>codeFixture.release());await new Promise(resolve=>setTimeout(resolve,150));if(await page.evaluate(()=>document.querySelector('tbody')?.textContent.includes('lookup-stale')))throw new Error('late lookup repainted the new query');console.log('PASS delayed lookup cannot overwrite new code result');passed++;
 // A failed exact query must retry the same code route, never list search.
 await page.evaluate(()=>{codeFixture.fail=true});await enter(code);await page.keyboard.press('Enter');await page.waitForFunction(()=>document.querySelector('[role=alert]'));
 const retryCounts=await page.evaluate(()=>({lookup:codeFixture.calls.length,list:f01.calls.filter(x=>x.url.includes('/admin/v1/users?')).length}));
 await page.click('[role=alert] button');await page.waitForFunction(before=>codeFixture.calls.length===before+1,retryCounts.lookup);
 if(!await page.evaluate(before=>f01.calls.filter(x=>x.url.includes('/admin/v1/users?')).length===before.list&&codeFixture.calls.at(-1).url.includes('/user-code-lookups/'),retryCounts))throw new Error('failed exact lookup retried directory');console.log('PASS exact lookup retry keeps the owner path');passed++;
 // Pending status mutations retain the existing busy fence while reads remain cancellable.
 await page.evaluate(()=>{codeFixture.fail=false;codeFixture.active=true});await enter(code);await page.keyboard.press('Enter');await page.waitForFunction(()=>document.querySelector('tbody')?.textContent.includes('ACTIVE'));
 await page.evaluate(()=>{codeFixture.holdStatus=true});await page.click('tbody button');await page.waitForFunction(()=>typeof codeFixture.releaseStatus==='function');
 if(!await page.$eval('input[maxlength="64"]',element=>element.disabled))throw new Error('exact code edit unlocked status mutation');
 await page.click('input[maxlength="64"]');await page.keyboard.type('unrelated');
 if(!await page.$eval('input[maxlength="64"]',(element,expected)=>element.value===expected&&element.disabled,code))throw new Error('code editing bypassed pending status fence');
 await page.evaluate(()=>codeFixture.releaseStatus());await page.waitForFunction(()=>!document.querySelector('input[maxlength="64"]').disabled);console.log('PASS pending status mutation prevents code-edit unlock');passed++;
 if(errors.length)throw new Error('React/browser errors in controlled lookup UI');console.log(`USER_CODE_LOOKUP_UI_PASS ${passed}`);
}finally{
 if(browser)await browser.close();if(vite)await vite.close();if(path.dirname(profile)!==path.resolve(os.tmpdir())||!path.basename(profile).startsWith('pinvou-code-lookup-'))throw new Error('unowned browser profile cleanup refused');fs.rmSync(profile,{recursive:true,force:true});
}
