import assert from 'node:assert/strict';
import fs from 'node:fs';
import {createServer} from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
const server=await createServer({root:import.meta.dirname,configFile:false,plugins:[react()],cacheDir:import.meta.dirname+'/node_modules/.vite-payment-test',server:{host:'127.0.0.1',port:0,fs:{allow:[import.meta.dirname],deny:['**/.env*','**/*.{pem,crt,key}']}}});await server.listen();
const browserPath=[process.env.CHROME,process.env.CHROMIUM,'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe','C:/Program Files/Google/Chrome/Application/chrome.exe','/usr/bin/google-chrome','/usr/bin/chromium','/usr/bin/chromium-browser','/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].filter(Boolean).find(p=>fs.existsSync(p));if(!browserPath)throw new Error('Browser unavailable');
const browser=await puppeteer.launch({executablePath:browserPath,headless:true,args:['--no-sandbox','--disable-gpu']});
let pass=0,fail=0;const results=[];
const run=async(name,fn)=>{const page=await browser.newPage();const errors=[];page.on('pageerror',e=>errors.push(e.message));try{await fn(page);assert.deepEqual(errors,[]);pass++;results.push({name,pass:true});}catch(e){fail++;results.push({name,pass:false,error:e.message});}finally{await page.close();}};
try{
 for(const language of ['zh','en','ja'])await run('production Payment form / exact version / draft / lost ACK / submit '+language,async page=>{
  await page.goto(server.resolvedUrls.local[0]+`payment-fixture.html?language=${language}${language==='en'?'&lost=1':''}`);
  await page.waitForFunction(()=>window.f10);
  await page.evaluate(()=>[...document.querySelectorAll('nav button')].find(b=>b.textContent===window.f10.ledgerRepairCopy[window.f10.language].title).click());
  await page.waitForSelector('.adm-financial-repair-target');
  await page.select('.adm-financial-repair select','PAYMENT_FINANCIAL_REPAIR');
  const values=await page.evaluate(()=>[window.f10.id(1),window.f10.id(2)]);const inputs=await page.$$('.adm-financial-repair-target input');for(let i=0;i<inputs.length;i++)await inputs[i].type(values[i]);
  await page.click('.adm-financial-repair-target button');await page.waitForSelector('.adm-financial-repair-draft');
  assert.ok(await page.evaluate(()=>{const c=window.f10.calls.find(c=>c.url==='/api/admin/account/admin/v2/approval-command-prechecks');return JSON.parse(c.body).payment_evidence_id===window.f10.id(2)&&JSON.parse(c.body).reason_code==='CONFIRMED_PAYMENT_NOT_CREDITED'&&c.headers['X-CSRF-Token']==='controlled-csrf';}));
  await page.type('.adm-financial-repair-draft textarea','Controlled ledger correction justification');await page.type('.adm-financial-repair-draft input','controlled-ledger-key');await page.click('.adm-financial-repair-draft button');
  if(language==='en'){await page.waitForSelector('.adm-financial-repair [role=alert]');await page.waitForFunction(()=>!document.querySelector('.adm-financial-repair-draft button').disabled);await page.click('.adm-financial-repair-draft button');}
  await page.waitForFunction(()=>document.body.textContent.includes(window.f10.id(9))&&!document.querySelector('.adm-financial-repair'));
  assert.ok(await page.evaluate(()=>window.f10.calls.filter(c=>c.url==='/api/admin/approvals/requests'&&c.method==='POST').every(c=>c.body.includes('"credit_microcredits":9223372036854775807')&&c.body.includes('"wallet_version":9007199254740993')&&JSON.parse(c.body).normalized_command.payment_evidence_id===window.f10.id(2)&&JSON.parse(c.body).idempotency_key==='approval:controlled-ledger-key')));
  await page.evaluate(()=>[...document.querySelectorAll('button')].find(b=>b.textContent===window.f10.financeCopy[window.f10.language].submit).click());await page.waitForFunction(()=>window.f10.status==='SUBMITTED');
 });
 await run('permission denied hides installed repair form',async page=>{await page.goto(server.resolvedUrls.local[0]+'payment-fixture.html?deny=1');await page.waitForFunction(()=>window.f10);assert.equal(await page.$('.adm-financial-repair'),null);assert.equal(await page.evaluate(()=>[...document.querySelectorAll('nav button')].some(b=>b.textContent===window.f10.ledgerRepairCopy.zh.title)),false);});
}finally{await browser.close();await server.close();}
const result={pass,fail,results,limits:['Actual React/AdminPlatformClient with controlled BFF responses; no real issuer, PostgreSQL approval, canonical production build or full E2E.']};fs.writeFileSync(process.env.F12_REACT_RESULT || 'payment-smoke-result.json',JSON.stringify(result,null,2));console.log(JSON.stringify(result));process.exitCode=fail?1:0;
