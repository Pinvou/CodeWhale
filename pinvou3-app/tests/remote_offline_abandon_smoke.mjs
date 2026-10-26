import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { pathToFileURL, fileURLToPath } from 'node:url';
import os from 'node:os';
import crypto from 'node:crypto';
import { spawnSync } from 'node:child_process';

const args=process.argv.slice(2);
// The owner never loads Vite. Reap its worker before removing any temp files:
// Vite/Rolldown may continue cache writes after their close promises settle.
if(process.env.PINVOU_OFFLINE_ABANDON_WORKER !== '1') {
  const ownedRoot=fs.mkdtempSync(path.join(os.tmpdir(),'pinvou-offline-abandon-owner-'));
  let worker;
  try {
    worker=spawnSync(process.execPath,[...process.execArgv,fileURLToPath(import.meta.url),...args],{
      stdio:'inherit',
      env:{...process.env,PINVOU_OFFLINE_ABANDON_WORKER:'1',
        TEMP:ownedRoot,TMP:ownedRoot,TMPDIR:ownedRoot},
    });
    if(worker.error) console.error(worker.error);
  } finally {
    // Exactly the root allocated above; the worker and its writers have exited.
    fs.rmSync(ownedRoot,{recursive:true,force:true});
  }
  process.exit(worker.status ?? 1);
}
const option=(name,fallback)=>{const index=args.indexOf(name);return index<0?fallback:args[index+1];};
const app=path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome=process.env.CHROME || [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/chromium','/usr/bin/google-chrome',
].find(candidate=>fs.existsSync(candidate));
if(!chrome) throw new Error('Chrome/Edge unavailable: set CHROME');
const req=createRequire(path.join(app,'package.json'));
const {createServer}=await import(pathToFileURL(req.resolve('vite')));
const react=(await import(pathToFileURL(req.resolve('@vitejs/plugin-react')))).default;
const puppeteer=await import(pathToFileURL(req.resolve('puppeteer-core')));
const sha=file=>crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');
const sources=['src/features/chat/ChatView.jsx','src/features/remote-control/remote-control-state.js',
'src/features/remote-control/remote-session-adapter.mjs','src/features/remote-control/remote-dispatch-ui.mjs',
'src/platform/tauri/bridge/desktop-remote-control.js','src/hooks/useBridge.js','package-lock.json',
'src/styles/base.css','src/styles/tailwind.css','tailwind.config.cjs','postcss.config.cjs',
'tests/fixtures/remote_offline_abandon_smoke.jsx','tests/fixtures/remote_offline_abandon_smoke.html'];
const originalHashes=Object.fromEntries(sources.map(file=>[file,sha(path.join(app,file))]));
const results={app,platform:process.platform,node:process.version,browser:chrome,
mode:'offline never-attempted abandon through actual ChatView and desktop JS bridge',sourceHashes:originalHashes,
mockBoundary:'Actual ChatView/children/helper and actual desktop JS bridge; mocked native invoke and finite unrelated bridge startup methods',
scenarios:[],serverErrors:[]};
let vite,browser;
// Tailwind's existing relative content/config paths use the application cwd.
// This changes only this disposable runner process, never repository files.
// Complete browser/dependency/source and report-path preflight before owning temp resources.
const requestedOutput=option('--output',null);
const reportPath=requestedOutput===null ? null : path.resolve(requestedOutput);
if(reportPath && fs.existsSync(reportPath) && !fs.statSync(reportPath).isFile()) {
  throw new Error('Report output must be a regular file');
}
if(reportPath) fs.mkdirSync(path.dirname(reportPath),{recursive:true});
process.chdir(app);
const scratch=fs.mkdtempSync(path.join(os.tmpdir(),'pinvou-offline-abandon-'));
const output=reportPath || path.join(scratch,'result.json');
const profile=path.join(scratch,'browser-profile');
try {
  vite=await createServer({root:app,configFile:false,appType:'mpa',logLevel:'error',
    cacheDir:path.join(scratch,'vite-cache'),plugins:[react()],
    server:{host:'127.0.0.1',port:0,watch:null}});
  await vite.listen();
  browser=await puppeteer.launch({executablePath:chrome,headless:true,userDataDir:profile,
    args:['--disable-gpu','--no-first-run','--no-default-browser-check','--disable-background-networking']});
  const url='http://127.0.0.1:'+vite.httpServer.address().port+'/tests/fixtures/remote_offline_abandon_smoke.html';
  for(const scenario of ['offline','failure-retry','unknown','recovering']) {
    const page=await browser.newPage();
    await page.setViewport({width:1280,height:900});
    const record={name:scenario,pageErrors:[],consoleErrors:[],requestFailures:[],externalRequests:[],assertions:[],laterAssertionsNotRun:[]};
    await page.setRequestInterception(true);
    page.on('request',request=>{
      const address=new URL(request.url());
      if ((address.protocol==='http:'||address.protocol==='https:')&&!['127.0.0.1','localhost'].includes(address.hostname)) {
        record.externalRequests.push(request.url());void request.abort();
      } else void request.continue();
    });
    page.on('pageerror',error=>record.pageErrors.push(error.stack || error.message));
    page.on('console',message=>{if(message.type()==='error')record.consoleErrors.push(message.text());});
    page.on('requestfailed',request=>record.requestFailures.push({url:request.url(),error:request.failure()?.errorText}));
    try {
      await page.goto(url+'?scenario='+scenario,{waitUntil:'networkidle0',timeout:60000});
      await page.waitForFunction(()=>window.offlineAbandonFixture?.ready===true,{timeout:15000});
      await page.waitForSelector('[data-testid="chat-scroll"]',{timeout:15000});
      await page.waitForSelector('[data-testid="chat-remote-pending-dispatches"]',{timeout:15000});
      record.mounted=true;
      record.assertions.push('actual complete ChatView mounted and pending surface present');
      const state=await page.evaluate(()=>{
        const p=window.offlineAbandonFixture;
        const container=document.querySelector('[data-testid="chat-remote-pending-dispatches"]');
        const buttons=[...container.querySelectorAll('button')];
        return {text:container.innerText,buttons:buttons.map(button=>({text:button.textContent.trim(),disabled:button.disabled})),
          abandon:buttons.filter(button=>button.textContent.trim()===p.copy.abandonPreparedTask).map(button=>({disabled:button.disabled})),
          commit:buttons.filter(button=>button.textContent.trim()===p.copy.commitPreparedTask).length,
          composerReadOnly:document.querySelector('[data-testid="chat-composer-input"]').readOnly,
          calls:p.calls,unexpected:p.unexpected,history:p.durableHistory};
      });
      record.initial=state;
      assert.deepEqual(record.pageErrors,[],'no page errors');
      assert.deepEqual(record.consoleErrors,[],'no console errors');
      assert.deepEqual(record.externalRequests,[],'no external HTTP requests');
      assert.deepEqual(state.unexpected,[],'no unexpected mock calls');
      assert.equal(state.composerReadOnly,true,'offline send remains read-only');
      assert.equal(state.commit,0,'no commit/resend action');
      if(scenario==='unknown') {
        assert.ok(state.text.includes(await page.evaluate(()=>window.offlineAbandonFixture.copy.outcomeUnknownNoResend)),'actual unknown warning rendered');
        assert.equal(state.abandon.length,0,'unknown has no abandon action');
        assert.equal(state.calls.filter(call=>call.name==='abandonRemoteTask').length,0);
        record.assertions.push('unknown aggregate has no abandon/commit/resend and no call');
        record.outcome='pass';
      } else {
        assert.ok(state.text.includes(await page.evaluate(()=>window.offlineAbandonFixture.copy.preparedPayloadLost)),'actual restart/payload-lost notice rendered');
        assert.equal(state.abandon.length,1,'one exact never-attempted action');
        assert.equal(state.abandon[0].disabled,false,'offline never-attempted abandon button must be enabled');
        record.assertions.push('offline never-attempted button enabled');
        const button=await page.$('[data-testid="chat-remote-pending-dispatches"] button');
        await button.click();
        await page.waitForFunction(()=>!!window.offlineAbandonFixture.pendingNative);
        const expected=await page.evaluate(()=>({id:window.offlineAbandonFixture.id,handle:window.offlineAbandonFixture.handle}));
        const invocations=await page.evaluate(()=>window.offlineAbandonFixture.calls.filter(call=>call.name==='abandonRemoteTask'||call.name==='desktop_remote_task_abandon_prepared_a'));
        assert.deepEqual(invocations.map(call=>call.name),['abandonRemoteTask','desktop_remote_task_abandon_prepared_a']);
        assert.deepEqual(invocations[0].args,[expected.id,expected.handle.clientRequestId,expected.handle.taskId]);
        assert.deepEqual(invocations[1].args,{localSessionId:expected.id,clientRequestId:expected.handle.clientRequestId,taskId:expected.handle.taskId});
        record.assertions.push('actual DOM click reaches real handler/real JS bridge with exact command payload');
        assert.equal(await button.evaluate(element=>element.disabled),true,'single-flight while pending');
        const box=await button.boundingBox();
        await page.mouse.click(box.x+box.width/2,box.y+box.height/2);
        assert.equal(await page.evaluate(()=>window.offlineAbandonFixture.calls.filter(call=>call.name==='desktop_remote_task_abandon_prepared_a').length),1,'physical second click while disabled does not duplicate');
        if(scenario==='failure-retry') {
          await page.evaluate(()=>window.offlineAbandonFixture.rejectAbandon());
          await page.waitForFunction(()=>{
            const p=window.offlineAbandonFixture;const c=document.querySelector('[data-testid="chat-remote-pending-dispatches"]');
            return c?.innerText.includes(p.copy.pendingActionFailed) && [...c.querySelectorAll('button')].some(b=>b.textContent.trim()===p.copy.abandonPreparedTask&&!b.disabled);
          });
          record.assertions.push('failure preserves handle and reenables exact same action');
          assert.equal(await page.evaluate(()=>window.offlineAbandonFixture.publishedHistory.unattemptedPending.length),1,'failed native abandon leaves verified handle present');
          const retry=await page.$('[data-testid="chat-remote-pending-dispatches"] button');
          await retry.click();await page.waitForFunction(()=>!!window.offlineAbandonFixture.pendingNative);
          const retried=await page.evaluate(()=>window.offlineAbandonFixture.calls.filter(call=>call.name==='desktop_remote_task_abandon_prepared_a'));
          assert.equal(retried.length,2);assert.deepEqual(retried[0].args,retried[1].args);
        }
        await page.evaluate(()=>window.offlineAbandonFixture.resolveAbandon());
        await page.waitForFunction(()=>window.offlineAbandonFixture.publishedHistory?.unattemptedPending.length===0);
        await page.waitForFunction(()=>!document.querySelector('[data-testid="chat-remote-pending-dispatches"]'));
        const readCount=await page.evaluate(()=>window.offlineAbandonFixture.calls.filter(call=>call.name==='desktop_remote_proxy_history_read').length);
        assert.equal(readCount,scenario==='failure-retry'?3:2,'initial read plus one refresh per settled attempt');
        record.assertions.push('success refresh through actual bridge publishes DTO and clears resolved notice');
        record.outcome='pass';
      }
      assert.deepEqual(record.pageErrors,[],'no page errors after actions');
      assert.deepEqual(record.consoleErrors,[],'no console errors after actions');
      assert.deepEqual(record.externalRequests,[],'no external HTTP requests after actions');
      assert.deepEqual(record.requestFailures,[],'no failed resource requests');
      assert.deepEqual(await page.evaluate(()=>window.offlineAbandonFixture.unexpected),[],'no unexpected mock calls after actions');
      assert.equal(await page.evaluate(()=>document.querySelector('[data-testid="chat-composer-input"]').readOnly),true,'offline send stays read-only after settlement');
    } catch(error) {
      record.outcome=record.mounted?'contract-failed':'harness-failed';
      record.error={name:error.name,message:error.message,stack:error.stack};
      if(record.mounted&&scenario!=='unknown')record.laterAssertionsNotRun=[
        'DOM click -> actual handler/JS bridge -> exact native command and arguments',
        'pending action single-flight under a second physical click',
        ...(scenario==='failure-retry'?['first failure retains handle and re-enables action','same-handle retry']:[]),
        'success and real JS bridge read publish clear the notice'];
    }
    record.finalProbe=await page.evaluate(()=>window.offlineAbandonFixture?{
      calls:window.offlineAbandonFixture.calls,unexpected:window.offlineAbandonFixture.unexpected,
      durableHistory:window.offlineAbandonFixture.durableHistory,ready:window.offlineAbandonFixture.ready}:null).catch(()=>null);
    await page.screenshot({path:path.join(path.dirname(output),scenario+'-attempt-'+path.basename(output,'.json')+'.png'),fullPage:true});
    results.scenarios.push(record);
    console.log(scenario+': '+record.outcome+(record.error?' | '+record.error.message:''));
    await page.close();
  }
} catch(error){results.fatal={name:error.name,message:error.message,stack:error.stack};console.error(error);}
finally {
  try {
    try { if(browser)await browser.close(); }
    finally { if(vite)await vite.close(); }
    results.sourceHashesAfter=Object.fromEntries(sources.map(file=>[file,sha(path.join(app,file))]));
    results.sourcesUnchanged=JSON.stringify(originalHashes)===JSON.stringify(results.sourceHashesAfter);
    results.profileRemoved=false;
    // Only this test's freshly-created disposable profile; never user browser data.
    if(fs.existsSync(profile)){fs.rmSync(profile,{recursive:true,force:true});results.profileRemoved=true;}
    fs.writeFileSync(output,JSON.stringify(results,null,2));
  } finally {
    // Report writing or source verification must not skip owned-resource cleanup.
    fs.rmSync(scratch,{recursive:true,force:true});
  }
}
const expected=results.scenarios.length===4 && results.scenarios.every(result=>result.mounted)
  && results.scenarios.find(result=>result.name==='unknown')?.outcome==='pass'
  && results.scenarios.filter(result=>result.name!=='unknown').every(result=>result.outcome==='contract-failed'&&result.error?.message.includes('button must be enabled'))
  && results.sourcesUnchanged;
console.log('baselineCounterexampleConfirmed='+expected);
console.log('remote_offline_abandon_smoke: '+results.scenarios.filter(result=>result.outcome==='pass').length+'/4 renderer scenarios passed (real JS bridge; mocked native IPC)');
process.exitCode=results.fatal||!results.sourcesUnchanged||results.scenarios.some(result=>result.outcome==='harness-failed')?1:results.scenarios.every(result=>result.outcome==='pass')?0:1;
