import puppeteer from 'puppeteer-core';
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { spawn } from 'node:child_process';
import { preview } from 'vite';
const chrome = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome'].find(file => fs.existsSync(file));
if (!chrome) { console.error('SKIP: Chrome/Edge unavailable'); process.exit(2); }
const server = await preview({ preview: { host: '127.0.0.1', port: 0, strictPort: false } });
const base = `http://127.0.0.1:${server.httpServer.address().port}`;
// Some Windows Edge versions relaunch their process before exposing CDP.
// Connect to this test profile's DevTools port instead of depending on the
// original launcher process remaining alive.
let profileRoot, profile, browser, browserProcess, spawnFailure, testFailure;
const cleanupErrors = [];
try {
profileRoot = fs.realpathSync(os.tmpdir());
profile = fs.mkdtempSync(path.join(profileRoot, '.pinvou-controlled-open-'));
browserProcess = spawn(chrome, ['--headless=new', '--no-sandbox', '--disable-gpu', '--no-first-run', '--remote-debugging-port=0', `--user-data-dir=${profile}`, 'about:blank'], { windowsHide: true, stdio: 'ignore' });
browserProcess.on('error', error => { spawnFailure = error; });
for (let i = 0; i < 100 && !browser; i += 1) {
  if (spawnFailure) throw spawnFailure;
  try {
    const port = fs.readFileSync(path.join(profile, 'DevToolsActivePort'), 'utf8').split('\n')[0].trim();
    browser = await puppeteer.connect({ browserURL: `http://127.0.0.1:${port}` });
  } catch (_) { await new Promise(resolve => setTimeout(resolve, 200)); }
}
if (!browser) {
  throw new Error('test browser CDP did not start');
}
 const page=await browser.newPage();
 const errors=[]; page.on('pageerror',e=>errors.push(e.message));
 await page.setRequestInterception(true);
 page.on('request', r => /pinvou-desktop-classic-7-/.test(r.url()) ? r.respond({status:200,contentType:'application/javascript',body:'/* controlled test host */'}) : r.continue());
 const sharedSource=fs.readFileSync('src/shared/bridge-shared-helpers.js','utf8');
 const sessionsSource=fs.readFileSync('src/platform/tauri/bridge/sessions.js','utf8');
 await page.evaluateOnNewDocument(({sharedSource,sessionsSource}) => {
   const params=new URLSearchParams(location.search); const lang=params.get('lang') || 'zh'; const scenario=params.get('scenario'); Object.defineProperty(navigator,'languages',{get:()=>[lang]}); Object.defineProperty(navigator,'language',{get:()=>lang}); const s={controlledDevice:{status:'connected',generation:'g1'},controlledSession:{sessionId:scenario?.startsWith('budget')?null:'session-b',generation:'g1'},controlledTasks:[],controlledNavigationIntent:1,platformCapabilities:{loaded:true,os:'windows',desktopRemoteControl:true},sessions:[],messages:[],chatItems:[],artifacts:[],queuedMessages:[],settings:{},scheduledRunContext:(scenario?.startsWith('budget')||scenario==='cross-busy')?{sessionId:'scheduled',returnSessionId:'local'}:null,modeState:{mode:'yolo'},tokens:{input:0,max:0}};
   let notify=()=>{}; let allow=scenario==='local-nav-reject'?'defer-reject':scenario==='local-nav'?'defer':false; let exitAllow=scenario==='budget-reject'?'reject':false; let switches=0; let exits=0; let taskSubmissions=0; let releaseSwitch=null; let rejectSwitch=null; let resolvedSwitches=0; let oldSwitchSettled=false; let releaseExit=null; let exitSettled=false;
   const fallback=new Proxy({}, {get:()=>()=>Promise.resolve(null)});
   const remote=new Proxy({controlledTaskSend:()=>{taskSubmissions+=1;return Promise.resolve()}},{get:(target,k)=>k in target?target[k]:()=>Promise.resolve(null)});
   const bridge=new Proxy({available:true,init:()=>Promise.resolve(),state:{getMany:()=>({...s}),get:()=>({...s}),subscribeMany:(_d,fn)=>{notify=fn;return()=>{}},subscribe:()=>()=>{}}, lifecycle:{init:()=>Promise.resolve()}, sessions:{captureControlledNavigationOwner:generation=>({generation,intent:s.controlledNavigationIntent}),isControlledNavigationOwnerCurrent:o=>o?.intent===s.controlledNavigationIntent&&o.generation===s.controlledDevice.generation,switchToControlledSession:(id,owner)=>{if(id!==s.controlledSession.sessionId||owner?.intent!==s.controlledNavigationIntent||owner?.generation!==s.controlledDevice.generation)throw new Error('wrong session owner');switches+=1;if(allow==='defer')return new Promise((resolve,reject)=>{releaseSwitch=resolve;rejectSwitch=reject}).then(value=>{resolvedSwitches+=1;return value},error=>{resolvedSwitches+=1;throw error});if(scenario==='reject'&&!allow)return Promise.reject(new Error('mock failure'));return Promise.resolve(allow)},exitControlledScheduledRunChat:()=>{exits+=1;if(exitAllow==='defer')return new Promise(resolve=>{releaseExit=resolve}).finally(()=>{exitSettled=true});if(exitAllow==='reject')return Promise.reject(new Error('mock chat failure'));if(exitAllow===true){s.scheduledRunContext=null;notify({...s});return Promise.resolve(true)}return Promise.resolve(false)}}, desktopRemoteControl:remote, scheduled:fallback,platform:fallback}, {get:(target,k)=> k in target ? target[k] : fallback});
   if (scenario==='real-bridge'||scenario?.startsWith('local-nav')) {
     // Run the production session factory against a controlled host, while
     // keeping the actual App and its React recovery handler mounted.
     (0,eval)(sharedSource); (0,eval)(sessionsSource);
     s.activeSessionId='local'; s.queued=[]; s.archivedSessions=[]; if(scenario?.startsWith('local-nav')) s.sessions=[{id:'local-b',title:'Local B',updated_at:new Date().toISOString()}];
     s.scheduledTaskRecentRuns=[]; s.scheduledTaskRuns=[]; s.modeDefaults={work:'yolo',code:'yolo'};
     s.modeLane='work'; s.draftEpoch=0; s.composerDraft=''; s.pendingDraftMultiAgent=false;
     s.mountedCollections=[]; s.mountedCollectionsRevision=0; s.turnTimeline=[];
     s.personaEvents=[]; s.pinvouReviews=[]; s.pinvouSceneEvents=[];
     s.desktopRemoteControl={controlledDevice:s.controlledDevice,controlledSession:s.controlledSession,controlledTasks:[],intents:{controlled:1}};
     const sessionStates={};
     const api=window.__PINVOU_TAURI_BRIDGE_FEATURES__.sessions({
       state:s,sessionStates,notify:()=>notify({...s}),listen:null,bt:key=>key,
       addSystemItem(){},addChatItem(){},timeStr:()=>'',
       invoke:(name,args)=>{if(name!=='load_session')return Promise.resolve({});if(allow==='defer'||allow==='defer-reject')return new Promise((resolve,reject)=>{releaseSwitch=()=>{resolvedSwitches+=1;resolve({metadata:{id:args.id},messages:[],artifacts:[]})};rejectSwitch=()=>{resolvedSwitches+=1;reject(new Error('late real load failure'))}});return Promise.resolve({metadata:{id:allow?args.id:'wrong-session'},messages:[],artifacts:[]})},
       runSyncOnSession:(_sid,fn)=>fn(),persistMessagesFor(){},resetPendingAssistant(){},stopThinking(){},rerenderFromMessages(){},
       syncModeState:()=>Promise.resolve(),applyAuthoritativeModeState(){},currentDraftModeState:()=>({mode:'yolo',multiAgent:false}),
       syncActivePersona:()=>Promise.resolve(),syncMountedCollection:()=>Promise.resolve(),reconcileArtifacts(){},loadSessionModel:()=>Promise.resolve(),
       clearScheduledTaskSelection(){},invalidateScheduledRecentRunsForSession(){},refreshHistoryListForRun:()=>Promise.resolve(),turnUsageDirty:false,
       basename:value=>String(value||'').split('/').pop(),isAbsPath:()=>false,filterSessionArtifacts:value=>value,scheduleShellPoll(){},
       setScheduledTaskError(){},userMessageDisplayText:value=>value,loadMemoryOverview:()=>Promise.resolve(),isScheduledRunSession:()=>false,
       invalidateScheduledTaskReads(){},applyScheduledRunViewed(){},loadScheduledTaskRecentRuns:()=>Promise.resolve(),scheduledRunSessionOwners:{},personaPlaceholderTitles:{},
     });
     const original=api.switchToControlledSession;
     api.switchToControlledSession=(id,owner)=>{switches+=1;const isOld=allow==='defer'||allow==='defer-reject';return original(id,owner).finally(()=>{if(isOld)oldSwitchSettled=true})};
     bridge.sessions=api;
   }
   window.TauriBridge=bridge;
   window.testBridge={setAllow:v=>{allow=v},setExitAllow:v=>{exitAllow=v},getSwitches:()=>switches,getExits:()=>exits,getTaskSubmissions:()=>taskSubmissions,getResolved:()=>resolvedSwitches,getOldSettled:()=>oldSwitchSettled,hasPendingExit:()=>typeof releaseExit==='function'&&!exitSettled,releaseExit:()=>releaseExit(false),getExitSettled:()=>exitSettled,hasPendingOldLoad:()=>typeof releaseSwitch==='function'&&!oldSwitchSettled,switchRealNew:()=>bridge.sessions.switchToControlledSession('session-new',bridge.sessions.captureControlledNavigationOwner('g2')),switchRealSame:()=>bridge.sessions.switchToControlledSession('session-new',bridge.sessions.captureControlledNavigationOwner('g1')),replaceSame:()=>{s.controlledSession={sessionId:'session-new',generation:'g1'};s.desktopRemoteControl={...s.desktopRemoteControl,controlledSession:s.controlledSession};allow=true;notify({...s})},replace:()=>{s.controlledNavigationIntent+=1;s.controlledDevice={status:'connected',generation:'g2'};s.controlledSession={sessionId:'session-new',generation:'g2'};allow=false;notify({...s})},replaceReal:()=>{s.controlledNavigationIntent+=1;s.controlledDevice={status:'connected',generation:'g2'};s.controlledSession={sessionId:'session-new',generation:'g2'};s.desktopRemoteControl={controlledDevice:s.controlledDevice,controlledSession:s.controlledSession,controlledTasks:[],intents:{controlled:s.controlledNavigationIntent}};allow=true;notify({...s})},releaseSwitch:v=>releaseSwitch?.(v),rejectSwitch:()=>rejectSwitch?.(new Error('late mock failure')),stop:()=>{s.controlledNavigationIntent+=1;s.controlledDevice={status:'stopped',generation:null};if(s.desktopRemoteControl)s.desktopRemoteControl={...s.desktopRemoteControl,controlledDevice:s.controlledDevice,intents:{controlled:s.controlledNavigationIntent}};notify({...s})},failRefresh:async()=>{s.controlledNavigationIntent+=1;s.controlledNavigationPending=true;notify({...s});await new Promise(r=>setTimeout(r,20));s.controlledNavigationPending=false;notify({...s})},setDevice:v=>{s.controlledDevice=v;notify({...s})},state:s};
   window.testBridge.replaceSession = (id, nextAllow = false, pending = false) => {
     s.controlledSession = { sessionId: id, generation: s.controlledDevice.generation };
     s.controlledNavigationPending = pending;
     if (s.desktopRemoteControl) s.desktopRemoteControl = { ...s.desktopRemoteControl, controlledSession: s.controlledSession };
     allow = nextAllow;
     notify({ ...s });
   };
 }, {sharedSource,sessionsSource});
 await page.goto(`${base}/?lang=zh`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>window.testBridge.getSwitches()===1 && document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 await page.evaluate(()=>window.testBridge.replaceSession('session-next'));
 await page.waitForFunction(()=>window.testBridge.getSwitches()===2 && document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 await page.evaluate(()=>window.testBridge.replaceSession('session-b', 'defer'));
 await page.waitForFunction(()=>window.testBridge.getSwitches()===3 && window.testBridge.hasPendingOldLoad(),{timeout:8000});
 if (await page.evaluate(()=>document.body.innerText.includes('无法自动打开收到任务的会话。'))) throw new Error('old session failure returned before the new attempt settled');
 await page.evaluate(()=>window.testBridge.releaseSwitch(false));
 await page.waitForFunction(()=>window.testBridge.getResolved()===1 && document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 if (errors.length) throw new Error(JSON.stringify({scenario:'session-failure-replacement',errors}));
 console.log(JSON.stringify({scenario:'session-failure-replacement',errors}));
 await page.goto(`${base}/?lang=zh`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>window.testBridge.getSwitches()===1 && document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 await page.evaluate(()=>window.testBridge.replaceSession('session-next', false, true));
 await page.waitForFunction(()=>!document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 await page.evaluate(()=>window.testBridge.replaceSession('session-b', false, true));
 await new Promise(resolve=>setTimeout(resolve,100));
 if (await page.evaluate(()=>window.testBridge.getSwitches()!==1)) throw new Error('a new session open started while navigation was pending');
 if (await page.evaluate(()=>document.body.innerText.includes('无法自动打开收到任务的会话。'))) throw new Error('old session failure revived after an ABA change while navigation was pending');
 console.log(JSON.stringify({scenario:'session-failure-aba-pending',errors}));
 for (const [lang, expected] of [['zh','无法自动打开收到任务的会话。'], ['en','Could not open the session that received the task.'], ['ja','タスクを受信した会話を自動で開けませんでした。']]) {
   errors.length=0;
   await page.goto(`${base}/?lang=${lang}`,{waitUntil:'domcontentloaded'});
   await page.waitForFunction(text=>document.body.innerText.includes(text),{timeout:8000},expected).catch(async e=>{throw new Error(JSON.stringify({error:e.message, errors, body:(await page.evaluate(()=>document.body.innerText)).slice(0,800)}));});
   const before=await page.$$eval('[role=alert]',els=>els.map(e=>e.innerText));
   await page.click('[role=alert] button');
   await page.waitForFunction(()=>window.testBridge.getSwitches()===2);
   if (!await page.$('[role=alert]')) throw new Error('failed manual retry removed recovery');
   await page.evaluate(()=>window.testBridge.setAllow(true));
   await page.click('[role=alert] button');
   await page.waitForFunction(text=>!document.body.innerText.includes(text),{},expected);
   const count=await page.evaluate(()=>window.testBridge.getSwitches());
   const submissions=await page.evaluate(()=>window.testBridge.getTaskSubmissions());
   if(count!==3 || submissions!==0 || errors.length) throw new Error(JSON.stringify({lang,count,submissions,errors}));
   console.log(JSON.stringify({lang,before,switches:count,errors}));
 }

 for (const outcome of ['resolve','reject']) {
 errors.length=0;
 await page.goto(`${base}/?lang=zh&scenario=reject`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 await page.evaluate(()=>window.testBridge.setAllow('defer'));
 await page.click('[role=alert] button');
 await page.waitForFunction(()=>window.testBridge.getSwitches()===2);
 await page.evaluate(()=>window.testBridge.stop());
 await page.waitForFunction(()=>!document.body.innerText.includes('无法自动打开收到任务的会话。'));
 await page.evaluate(()=>window.testBridge.replace());
 const viewBefore=await page.$eval('[data-testid=app-root]',root=>root.dataset.currentView);
 await page.evaluate(outcome=>outcome==='resolve'?window.testBridge.releaseSwitch(true):window.testBridge.rejectSwitch(),outcome);
 await page.waitForFunction(()=>window.testBridge.getResolved()===1);
 await page.waitForFunction(()=>window.testBridge.getSwitches()===3 && document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 const viewAfter=await page.$eval('[data-testid=app-root]',root=>root.dataset.currentView);
 if (viewAfter!==viewBefore || !await page.$('[role=alert]') || await page.evaluate(()=>window.testBridge.getTaskSubmissions())!==0 || errors.length) throw new Error(JSON.stringify({scenario:'stale',viewBefore,viewAfter,errors}));
 console.log(JSON.stringify({scenario:'rejected-and-stale',outcome,switches:3,errors}));
 }
 await page.setViewport({width:1440,height:900});
 for (const outcome of ['resolve','reject']) {
 errors.length=0;
 await page.goto(`${base}/?lang=zh&scenario=local-nav${outcome==='reject'?'-reject':''}`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>window.testBridge.hasPendingOldLoad(),{timeout:8000});
 await page.click('[data-sidebar-toggle]');
 await page.waitForFunction(()=>[...document.querySelectorAll('[data-testid=regular-sidebar-item]')].some(e=>e.textContent.includes('Local B')),{timeout:8000}).catch(async e=>{throw new Error(JSON.stringify({error:e.message,body:await page.evaluate(()=>document.body.innerText),errors}));});
 await page.evaluate(()=>[...document.querySelectorAll('[data-testid=regular-sidebar-item]')].find(e=>e.textContent.includes('Local B')).click());
 if(!await page.evaluate(()=>window.testBridge.hasPendingOldLoad())) throw new Error('old load was not pending after local click');
 await page.evaluate(()=>window.testBridge.setAllow(true));
 await page.evaluate(outcome=>outcome==='reject'?window.testBridge.rejectSwitch():window.testBridge.releaseSwitch(),outcome);
 await page.waitForFunction(()=>window.testBridge.getOldSettled() && window.testBridge.state.activeSessionId==='local-b',{timeout:8000});
 await page.waitForFunction(()=>document.querySelector('[data-session-key="local-b"]')?.className.includes('bg-[#E1E5EA]'),{timeout:8000});
 if(await page.evaluate(()=>document.body.innerText.includes('无法自动打开收到任务的会话。'))) throw new Error('retired controlled failure contaminated the local session');
 if(await page.evaluate(()=>window.testBridge.getTaskSubmissions())!==0 || errors.length) throw new Error(JSON.stringify({scenario:'local-navigation',outcome,errors}));
 console.log(JSON.stringify({scenario:'local-navigation-invalidates-controlled-open',outcome,errors}));
 }
 errors.length=0;
 // A local selection retires the failed controlled open, including on refresh.
 await page.goto(`${base}/?lang=zh&scenario=local-nav-after-failure`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>window.testBridge.getSwitches()===1 && document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 await page.evaluate(()=>window.testBridge.setAllow(true));
 await page.click('[data-sidebar-toggle]');
 await page.waitForFunction(()=>[...document.querySelectorAll('[data-testid=regular-sidebar-item]')].some(e=>e.textContent.includes('Local B')),{timeout:8000});
 await page.evaluate(()=>[...document.querySelectorAll('[data-testid=regular-sidebar-item]')].find(e=>e.textContent.includes('Local B')).click());
 await page.waitForFunction(()=>window.testBridge.state.activeSessionId==='local-b' && !document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 const afterLocalSwitches=await page.evaluate(()=>window.testBridge.getSwitches());
 await page.evaluate(()=>window.testBridge.failRefresh());
 await new Promise(resolve=>setTimeout(resolve,500));
 const afterRefresh=await page.evaluate(()=>({active:window.testBridge.state.activeSessionId,switches:window.testBridge.getSwitches(),submissions:window.testBridge.getTaskSubmissions(),alert:document.body.innerText.includes('无法自动打开收到任务的会话。')}));
 if(afterLocalSwitches!==1 || afterRefresh.active!=='local-b' || afterRefresh.switches!==afterLocalSwitches || afterRefresh.submissions!==0 || afterRefresh.alert || errors.length) throw new Error(JSON.stringify({scenario:'failed-open-local-navigation-refresh',afterLocalSwitches,afterRefresh,errors}));
 console.log(JSON.stringify({scenario:'failed-open-local-navigation-refresh',afterRefresh,errors}));
 errors.length=0;
 await page.goto(`${base}/?lang=zh&scenario=real-bridge`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 if(await page.evaluate(()=>window.testBridge.state.activeSessionId)!=='local') throw new Error('real session factory committed a mismatched saved session');
 await page.evaluate(()=>window.testBridge.setAllow(true));
 await page.click('[role=alert] button');
 await page.waitForFunction(()=>window.testBridge.state.activeSessionId==='session-b',{timeout:8000});
 await page.waitForFunction(()=>!document.body.innerText.includes('无法自动打开收到任务的会话。'));
 if(await page.evaluate(()=>window.testBridge.getTaskSubmissions())!==0 || errors.length) throw new Error(JSON.stringify({scenario:'real-bridge',errors}));
 console.log(JSON.stringify({scenario:'real-session-factory-manual-recovery',active:'session-b',errors}));
 errors.length=0;
 await page.goto(`${base}/?lang=zh&scenario=real-bridge`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 await page.evaluate(()=>window.testBridge.setAllow('defer'));
 await page.click('[role=alert] button');
 await page.waitForFunction(()=>window.testBridge.getSwitches()===2 && window.testBridge.hasPendingOldLoad(),{timeout:8000});
 await page.evaluate(()=>window.testBridge.stop());
 await page.evaluate(()=>window.testBridge.replaceReal());
 if(!await page.evaluate(()=>window.testBridge.switchRealNew())) throw new Error('new owner did not switch');
 if(await page.evaluate(()=>window.testBridge.state.activeSessionId)!=='session-new') throw new Error('new session did not become active before old load');
 await page.evaluate(()=>window.testBridge.releaseSwitch(true));
 await page.waitForFunction(()=>window.testBridge.getResolved()===1 && window.testBridge.getOldSettled(),{timeout:8000});
 if(await page.evaluate(()=>window.testBridge.state.activeSessionId)!=='session-new' || await page.evaluate(()=>window.testBridge.getTaskSubmissions())!==0 || errors.length) throw new Error(JSON.stringify({scenario:'real-bridge-stale',errors}));
 console.log(JSON.stringify({scenario:'real-session-factory-stale-result',active:'session-new',errors}));
 errors.length=0;
 await page.goto(`${base}/?lang=zh&scenario=real-bridge`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 await page.evaluate(()=>window.testBridge.setAllow('defer'));
 await page.click('[role=alert] button');
 await page.waitForFunction(()=>window.testBridge.getSwitches()===2 && window.testBridge.hasPendingOldLoad(),{timeout:8000});
 await page.evaluate(()=>window.testBridge.replaceSame());
 if(!await page.evaluate(()=>window.testBridge.switchRealSame())) throw new Error('new same-generation session did not switch');
 if(await page.evaluate(()=>window.testBridge.state.activeSessionId)!=='session-new') throw new Error('new session did not become active');
 if(!await page.evaluate(()=>window.testBridge.hasPendingOldLoad())) throw new Error('old load settled before release');
 await page.evaluate(()=>window.testBridge.releaseSwitch(true));
 await page.waitForFunction(()=>window.testBridge.getResolved()===1 && window.testBridge.getOldSettled(),{timeout:8000});
 if(await page.evaluate(()=>window.testBridge.state.activeSessionId)!=='session-new' || errors.length) throw new Error(JSON.stringify({scenario:'same-generation-session',errors}));
 console.log(JSON.stringify({scenario:'real-session-same-generation-replacement',active:'session-new',errors}));
 errors.length=0;
 await page.goto(`${base}/?lang=zh&scenario=real-bridge`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 await page.evaluate(()=>window.testBridge.setAllow('defer-reject'));
 await page.click('[role=alert] button');
 await page.waitForFunction(()=>window.testBridge.getSwitches()===2 && window.testBridge.hasPendingOldLoad(),{timeout:8000});
 await page.evaluate(()=>{window.testBridge.stop();window.testBridge.replaceReal();window.testBridge.setAllow(false)});
 if(!await page.evaluate(()=>window.testBridge.hasPendingOldLoad())) throw new Error('old load settled before rejection');
 await page.evaluate(()=>window.testBridge.rejectSwitch());
 await page.waitForFunction(()=>window.testBridge.getResolved()===1 && window.testBridge.getOldSettled(),{timeout:8000});
 await page.waitForFunction(()=>window.testBridge.getSwitches()>=3 && document.body.innerText.includes('无法自动打开收到任务的会话。'),{timeout:8000});
 await page.waitForFunction(()=>{const b=document.querySelector('[role=alert] button');return b && !b.disabled},{timeout:8000});
 await page.evaluate(()=>window.testBridge.setAllow(true));
 await page.click('[role=alert] button');
 await page.waitForFunction(()=>window.testBridge.state.activeSessionId==='session-new' && !document.querySelector('[role=alert]'),{timeout:8000});
 if(await page.evaluate(()=>window.testBridge.getTaskSubmissions())!==0 || errors.length) throw new Error(JSON.stringify({scenario:'real-stale-reject',errors}));
 console.log(JSON.stringify({scenario:'real-session-stale-reject-new-owner-retry',active:'session-new',errors}));
 errors.length=0;
 await page.goto(`${base}/?lang=zh&scenario=cross-busy`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(() => {
   const alerts=[...document.querySelectorAll('[role=alert]')];
   return alerts.some(a=>a.textContent.includes('无法自动打开远程会话页面。')) && alerts.some(a=>a.textContent.includes('无法自动打开收到任务的会话。'));
 },{timeout:8000});
 const recoveryLabels=await page.evaluate(()=>[...document.querySelectorAll('[role=alert]')].filter(a=>a.textContent.includes('无法自动打开')).map(a=>({text:a.querySelector('span').textContent, label:a.querySelector('button').getAttribute('aria-label'), action:a.querySelector('button').textContent})));
 if(recoveryLabels.length!==2 || new Set(recoveryLabels.map(a=>a.label)).size!==2 || !recoveryLabels.every(a=>a.label?.includes(a.text) && a.label.includes(a.action))) throw new Error(JSON.stringify({scenario:'recovery-accessible-names',recoveryLabels}));
 await page.evaluate(()=>window.testBridge.setExitAllow('defer'));
 await page.evaluate(()=>[...document.querySelectorAll('[role=alert]')].find(a=>a.textContent.includes('无法自动打开收到任务的会话。')).querySelector('button').click());
 await page.waitForFunction(()=>window.testBridge.hasPendingExit(),{timeout:8000});
 await page.waitForFunction(()=>[...document.querySelectorAll('[role=alert]')].filter(a=>a.textContent.includes('无法自动打开')).length===2 && [...document.querySelectorAll('[role=alert]')].filter(a=>a.textContent.includes('无法自动打开')).every(a=>a.querySelector('button').disabled),{timeout:8000});
 const callsBefore=await page.evaluate(()=>({exits:window.testBridge.getExits(),switches:window.testBridge.getSwitches()}));
 await page.evaluate(()=>[...document.querySelectorAll('[role=alert]')].find(a=>a.textContent.includes('无法自动打开远程会话页面。')).querySelector('button').click());
 await new Promise(resolve=>setTimeout(resolve,500));
 const callsAfter=await page.evaluate(()=>({exits:window.testBridge.getExits(),switches:window.testBridge.getSwitches()}));
 if(JSON.stringify(callsBefore)!==JSON.stringify(callsAfter)) throw new Error('disabled chat recovery started');
 if(!await page.evaluate(()=>window.testBridge.hasPendingExit())) throw new Error('old manual exit settled during disabled click observation');
 await page.evaluate(()=>{window.testBridge.setExitAllow(false);window.testBridge.stop()});
 await page.waitForFunction(()=>![...document.querySelectorAll('[role=alert]')].some(a=>a.textContent.includes('无法自动打开远程会话页面。') || a.textContent.includes('无法自动打开收到任务的会话。')),{timeout:8000});
 if(!await page.evaluate(()=>window.testBridge.hasPendingExit())) throw new Error('old manual exit settled before replacement');
 await page.evaluate(()=>window.testBridge.replace());
 await page.waitForFunction(()=>{const a=[...document.querySelectorAll('[role=alert]')].find(e=>e.textContent.includes('无法自动打开远程会话页面。'));return a && a.querySelector('button').disabled},{timeout:8000});
 if(!await page.evaluate(()=>window.testBridge.hasPendingExit())) throw new Error('old manual exit was not pending');
 await page.evaluate(()=>window.testBridge.releaseExit());
 await page.waitForFunction(()=>window.testBridge.getExitSettled(),{timeout:8000});
 await page.waitForFunction(()=>{const a=[...document.querySelectorAll('[role=alert]')].find(e=>e.textContent.includes('无法自动打开远程会话页面。'));return a && !a.querySelector('button').disabled},{timeout:8000});
 await page.evaluate(()=>window.testBridge.setExitAllow(true));
 await page.evaluate(()=>[...document.querySelectorAll('[role=alert]')].find(a=>a.textContent.includes('无法自动打开远程会话页面。')).querySelector('button').click());
 await page.waitForFunction(()=>!document.body.innerText.includes('无法自动打开远程会话页面。'),{timeout:8000});
 if(await page.evaluate(()=>window.testBridge.getTaskSubmissions())!==0 || errors.length) throw new Error(JSON.stringify({scenario:'cross-busy',errors}));
 console.log(JSON.stringify({scenario:'cross-type-busy-gate-and-recovery',errors}));
 await page.goto(`${base}/?lang=zh&scenario=budget`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>document.body.innerText.includes('无法自动打开远程会话页面。'),{timeout:8000});
 await page.evaluate(async()=>{await window.testBridge.failRefresh(); await window.testBridge.failRefresh()});
 await new Promise(r=>setTimeout(r,500));
 const budget=await page.evaluate(()=>({exits:window.testBridge.getExits(),text:document.body.innerText}));
 if(budget.exits!==3 || !budget.text.includes('无法自动打开远程会话页面。')) throw new Error(JSON.stringify({budget,errors}));
 console.log(JSON.stringify({scenario:'budget',exits:budget.exits,errors}));
 await page.evaluate(()=>window.testBridge.setExitAllow('reject'));
 await page.click('[role=alert] button');
 await page.waitForFunction(()=>window.testBridge.getExits()===4);
 if (!await page.$('[role=alert]')) throw new Error('rejected chat retry removed recovery');
 await page.evaluate(()=>window.testBridge.setExitAllow(true));
 await page.click('[role=alert] button');
 await page.waitForFunction(()=>!document.body.innerText.includes('无法自动打开远程会话页面。'),{timeout:8000});
 if(await page.evaluate(()=>window.testBridge.getTaskSubmissions())!==0 || errors.length) throw new Error(JSON.stringify({scenario:'chat-retry',errors}));
 const finalExits=await page.evaluate(()=>window.testBridge.getExits());
 if(finalExits!==5) throw new Error(JSON.stringify({scenario:'chat-retry',finalExits}));
 console.log(JSON.stringify({scenario:'chat-manual-reject-then-success',exits:finalExits,errors}));
 errors.length=0;
 await page.goto(`${base}/?lang=zh&scenario=budget-reject`,{waitUntil:'domcontentloaded'});
 await page.waitForFunction(()=>document.body.innerText.includes('无法自动打开远程会话页面。'),{timeout:8000});
 if(await page.evaluate(()=>window.testBridge.getExits())!==3) throw new Error('automatic rejected chat navigation exceeded its budget');
 await page.evaluate(()=>window.testBridge.setExitAllow(true));
 await page.click('[role=alert] button');
 await page.waitForFunction(()=>!document.body.innerText.includes('无法自动打开远程会话页面。'),{timeout:8000});
 const autoRejectExits=await page.evaluate(()=>window.testBridge.getExits());
 if(autoRejectExits!==4 || await page.evaluate(()=>window.testBridge.getTaskSubmissions())!==0 || errors.length) throw new Error(JSON.stringify({scenario:'chat-auto-reject',autoRejectExits,errors}));
 console.log(JSON.stringify({scenario:'chat-auto-reject-then-manual-success',exits:autoRejectExits,errors}));

} catch (error) {
  testFailure = error;
} finally {
  try { if (browser) await browser.close(); else browserProcess?.kill(); } catch (error) { cleanupErrors.push(error); }
  try { await new Promise((resolve, reject) => server.httpServer.close(error => error ? reject(error) : resolve())); } catch (error) { cleanupErrors.push(error); }
  try { if (profile) {
    const resolved = fs.realpathSync(profile);
    if (path.dirname(resolved) !== profileRoot || !path.basename(resolved).startsWith('.pinvou-controlled-open-')) {
      cleanupErrors.push(new Error('refusing to remove a profile outside the temporary directory'));
    } else {
      await fs.promises.rm(resolved, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 });
    }
  } } catch (error) { cleanupErrors.push(error); }
}
if (testFailure) {
  for (const error of cleanupErrors) console.error('test cleanup failed:', error);
  throw testFailure;
}
if (cleanupErrors.length) throw new AggregateError(cleanupErrors, 'test cleanup failed');
