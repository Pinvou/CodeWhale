import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || process.env.CHROMIUM || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', '/snap/bin/chromium', '/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge'].find(fs.existsSync);
if (!chrome) throw new Error('Browser unavailable');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-f05-native-code-')); let vite; let browser; let page; let passed = 0; let activeCase = 'setup';
try {
 vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } }); await vite.listen();
 browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] }); page = await browser.newPage();
 const errors=[]; page.on('pageerror', error=>errors.push(error.name)); let failed=0;
 const fresh=async()=>{errors.length=0;await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f05-native-code.html`); await page.waitForFunction(()=>window.nativeCode && document.querySelector('button'));};
 const open=async()=>{await page.click('#root button');await page.waitForFunction(()=>document.body.textContent.includes('Platform B'));};
 const click=async(label)=>page.evaluate(text=>{[...document.querySelectorAll('button')].find(el=>el.textContent.includes(text)).click();},label);
 const run=async(name,fn)=>{activeCase=name;try{await fn();assert.deepEqual(errors,[]);passed++;console.log(`PASS ${name}`);}catch(error){failed++;console.log(`FAIL ${name}: ${error.name}`);}};
 await run('explicit Code platform reference displayed instead of carrier or chat reference',async()=>{await fresh();await open();assert.match(await page.$eval('#root button',el=>el.textContent),/Platform A/);});
 await run('platform selection uses registered command with exact Code owner',async()=>{await fresh();await open();await click('Platform B');await page.waitForFunction(()=>window.nativeCode.calls.length>0);assert.deepEqual(await page.evaluate(()=>window.nativeCode.calls),[['account_platform_select_session_model',{input:{sessionId:'code-a',modelId:'public-b'}}]]);});
 await run('local selection remains exact Code local command',async()=>{await fresh();await open();await click('Local A');await page.waitForFunction(()=>window.nativeCode.calls.length>0);assert.deepEqual(await page.evaluate(()=>window.nativeCode.calls),[['set_session_model',{sessionId:'code-a',modelId:'local-a'}]]);});
 await run('busy controls do not dispatch model changes',async()=>{await fresh();await page.evaluate(async()=>{window.nativeCode.busy=true;await window.nativeCode.render();});await page.waitForFunction(()=>document.querySelector('#root button').disabled);await page.click('#root button');assert.deepEqual(await page.evaluate(()=>window.nativeCode.calls),[]);});
 await run('ineligible catalog entry cannot be chosen',async()=>{await fresh();await open();assert.equal(await page.evaluate(()=>[...document.querySelectorAll('button')].find(el=>el.textContent.includes('Platform blocked')).disabled),true);await click('Platform blocked');assert.deepEqual(await page.evaluate(()=>window.nativeCode.calls),[]);});
 await run('signed-out catalog stays unavailable without dispatch',async()=>{await fresh();await page.evaluate(async()=>{window.nativeCode.authenticated=false;await window.nativeCode.render();});await page.click('#root button');await page.waitForFunction(()=>document.body.textContent.includes(window.nativeCode.copy().uiSettingsDetail.platformModelsSignIn));assert.deepEqual(await page.evaluate(()=>window.nativeCode.calls),[]);});
 await run('session switch refresh uses new explicit Code owner',async()=>{await fresh();await open();await page.evaluate(async()=>{window.nativeCode.owner='code-b';window.nativeCode.modelRef={source:'ACCOUNT_PLATFORM',model_id:'public-b'};await window.nativeCode.render();});await page.waitForFunction(()=>document.querySelector('#root button').textContent.includes('Platform B'));await click('Platform A');await page.waitForFunction(()=>window.nativeCode.calls.length>0);assert.deepEqual(await page.evaluate(()=>window.nativeCode.calls),[['account_platform_select_session_model',{input:{sessionId:'code-b',modelId:'public-a'}}]]);});
 await run('failed catalog selection remains directly visible and preserves reference in three locales',async()=>{for(const language of ['zh','en','ja']){await fresh();await page.evaluate(async lang=>{window.nativeCode.language=lang;await window.nativeCode.render();},language);await open();await page.evaluate(()=>window.nativeCode.fail=true);await click('Platform B');await page.waitForFunction(()=>document.querySelector('[role="alert"]')?.textContent===window.nativeCode.copy().uiSettingsDetail.modelSwitchFailed);assert.equal(await page.evaluate(()=>window.nativeCode.modelRef.model_id),'public-a');assert.equal(await page.evaluate(()=>window.nativeCode.calls.length),1);}});
 await run('three shared locales preserve catalog choices',async()=>{for(const language of ['zh','en','ja']){await fresh();await page.evaluate(async lang=>{window.nativeCode.language=lang;await window.nativeCode.render();},language);await open();assert.equal(await page.evaluate(()=>[...document.querySelectorAll('button')].find(el=>el.textContent.includes('Platform blocked')).disabled),true);}});
 console.log(`RESULT ${passed} PASS ${failed} FAIL 0 SKIP`); if(failed)process.exitCode=1;
} catch(error){console.error(`FAIL setup: ${error.name}`);process.exitCode=1;}
finally { if(browser)await browser.close();if(vite)await vite.close();if(fs.existsSync(profile))fs.rmSync(profile,{recursive:true,force:true}); }
