#!/usr/bin/env node
// Opt-in: production React -> native Pinvou commands -> own systemd/cmsd -> real CPU model.
import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import crypto from 'node:crypto';
import assert from 'node:assert/strict';
import net from 'node:net';
import http from 'node:http';
import readline from 'node:readline';
import { execFile, spawn } from 'node:child_process';
import { promisify } from 'node:util';
import { fileURLToPath } from 'node:url';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import puppeteer from 'puppeteer-core';
if (process.argv[2] !== '--run-live' || !process.argv[3]) throw new Error('Explicit --run-live INPUT.json required; creates only private service/container/profile');
const input = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'));
const app = fileURLToPath(new URL('..', import.meta.url));
process.chdir(app);
const output = path.resolve(input.evidence); fs.mkdirSync(output, { recursive: false, mode: 0o700 });
const nonce = crypto.randomBytes(5).toString('hex');
const work = fs.mkdtempSync('/tmp/pinvou-startup-');
const profile = path.join(work, '.qa', 'mcp-switch-real-' + nonce); fs.mkdirSync(profile, { recursive: true, mode: 0o700 });
const unit = `pinvou-cmsd-startup-${nonce}.service`, container = `megacube-startup-${nonce}`, card = `qa-startup-${nonce}`;
const command = promisify(execFile);
const run = async (program, args, options = {}) => (await command(program, args, { timeout: 60000, maxBuffer: 4*1024*1024, ...options })).stdout.trim();
const systemd = (...args) => run('systemctl', ['--user', ...args]);
const docker = (...args) => run('docker', args);
const freePort = async () => { const s=net.createServer(); await new Promise(r=>s.listen(0,'127.0.0.1',r)); const p=s.address().port; await new Promise(r=>s.close(r)); return p; };
const gateway = await freePort(), modelPort = await freePort();
const env = Object.fromEntries(Object.entries(process.env).filter(([k])=>!k.startsWith('CMSD_')));
Object.assign(env, { PINVOU3_HOME: profile, PINVOU3_MEGACUBE_ACCEPTANCE: '1', CODEWHALE_HOME: path.join(work,'codewhale'),
  CMSD_SOCKET: path.join(work,'cmsd.sock'), PINVOU3_CMSD_UNIT: unit, PINVOU3_CMSD_GATEWAY: '127.0.0.1:'+gateway,
  PINVOU3_RESOURCE_DIR: input.resources, XDG_CONFIG_HOME: path.join(os.homedir(), '.config') });
const nativeState = path.join(profile, 'megacube/cmsd-component/state');
const nativePrefix = 'megacube-model-' + crypto.createHash('sha256').update(path.join(nativeState, 'env')).digest('hex').slice(0, 16);
const nativeUnitDirectory = path.join(os.homedir(), '.config/systemd/user');
const nativeTemplate = path.join(nativeUnitDirectory, nativePrefix + '@.service');
const nativeTemplateBackup = nativeTemplate + '.cmsd-before-profile-v1';
const maybeStat = file => {
 try { return fs.lstatSync(file); } catch (error) { if (error.code === 'ENOENT') return null; throw error; }
};
for (const owned of [nativeTemplate, nativeTemplateBackup]) assert.equal(maybeStat(owned), null, 'private template namespace already exists');
let nativeTemplateSha256 = null;
const began=Date.now();
const calls=[]; const report={ startedAt:new Date().toISOString(), scope:'Production React browser UI with real native Pinvou commands, private systemd user unit and real CPU inference. Visual recovery cases use view-only fixtures. Boot IDs are simulated on this host; no physical reboot or multi-device claim.', checks:[], generations:[], screenshots:[], errors:[], passed:false, work, unit, container, calls };
let browser, page, server, host, hostLog, modelCreated=false, installed=false, observePromise=null;
const original=await systemd('show','cmsd.service','-p','MainPID','-p','ExecMainStartTimestampMonotonic');
const legacy=path.join(os.homedir(),'.config/systemd/user/megacube-model@.service');const legacyHash=crypto.createHash('sha256').update(fs.readFileSync(legacy)).digest('hex');
const originalContainers=(await docker('ps','-q')).split('\n').filter(Boolean);
const protectedContainers={};for(const id of originalContainers)protectedContainers[id]=await docker('inspect','--format','{{.State.Pid}} {{.State.StartedAt}} {{.RestartCount}}',id);
const cli=async(...args)=>{ const value=JSON.parse(await run(input.binary,['--cmsd-component-cli',...args],{env}));if(value.error)throw new Error(JSON.stringify(value.error));return value; };
const api=(method, pathname)=>new Promise((resolve,reject)=>{const req=http.request({socketPath:env.CMSD_SOCKET,path:pathname,method,timeout:15000},res=>{let data='';res.on('data',c=>data+=c);res.on('end',()=>{try{const v=JSON.parse(data);res.statusCode<400?resolve(v):reject(new Error(data));}catch(e){reject(e);}});});req.on('error',reject);req.on('timeout',()=>req.destroy(new Error('api timeout')));req.end();});
const wait=async(fn,seconds=90)=>{const deadline=Date.now()+seconds*1000;while(Date.now()<deadline){const v=await fn();if(v)return v;await new Promise(r=>setTimeout(r,250));}throw new Error('acceptance condition timed out');};
const settle=async(id)=>wait(async()=>{const s=await cli('status');return s.operation?.id===id&&!['running','queued'].includes(s.operation.status)?s:null;});
const lifecycle=async(action)=>{const value=await hostRequest({command:'guidance-action',card,action});if(value.task_id){const task=await wait(async()=>{const t=await api('GET','/v1/tasks/'+value.task_id);return ['succeeded','failed','cancelled'].includes(t.status)?t:null;});assert.equal(task.status,'succeeded',JSON.stringify(task));}return value;};
let serial=Promise.resolve(), sequence=0; const pending=new Map();
const hostRequest=request=>{const task=serial.then(()=>new Promise((resolve,reject)=>{const id=++sequence;const timer=setTimeout(()=>{pending.delete(id);reject(new Error('native command timeout'));},60000);pending.set(id,{resolve,reject,timer});host.stdin.write(JSON.stringify({...request,id})+'\n');}));serial=task.catch(()=>{});return task;};
const observe=()=>{if(!observePromise){observePromise=hostRequest({command:'guidance-observe',port:modelPort});observePromise.finally(()=>{setTimeout(()=>{observePromise=null;},100);}).catch(()=>{});}return observePromise;};
async function native(name,args={}){
  args??={};calls.push({name,action:args.action,at:Date.now()});
  if(name==='cms_service_status')return cli('status');
  if(name==='cms_service_bundled_package')return cli('bundled');
  if(name==='cms_service_inspect_package')return cli('inspect',args.packagePath);
  if(name==='cms_service_logs')return cli('logs');
  if(name==='cms_service_action')return cli('action',args.action,...(args.packagePath?[args.packagePath,args.expectedSha256]:[]));
  if(name==='cms_model_action'){const result=await hostRequest({command:'guidance-action',card:args.card,action:args.action});observePromise=null;return result;}
  if(name==='cms_device_status')return cli(args.inspectRunning?'device-inspect':'device-status');
  if(name==='discover_local_vllm')return (await observe()).discovery;
  if(['cms_status','cms_models','cms_tasks'].includes(name))return (await observe())[name.slice(4)];
  if(['cms_capacity','cms_engines'].includes(name)){try{return {available:true,data:await api('GET','/v1/'+name.slice(4))};}catch(e){return {available:false,reason:String(e)};}}
  throw new Error('Unexpected native call '+name);
}
async function open(lang='zh',width=1200,mode='real'){
 if(page)await page.close();page=await browser.newPage();await page.setViewport({width,height:1000});
 page.on('pageerror',e=>report.errors.push(e.message));await page.exposeFunction('__native',native);
 await page.evaluateOnNewDocument(()=>{window.__TAURI__={core:{invoke:(n,a)=>window.__native(n,a)}};});
 await page.goto(server.resolvedUrls.local[0]+`?lang=${lang}&mode=${mode}`,{waitUntil:'networkidle0'});
 await page.waitForSelector(mode==='real'?'[data-testid="cms-service-panel"]':'[data-testid="model-guidance"]');
 if(mode==='real')assert.equal(await page.$('[data-testid="cms-local-control-panel"]'),null);
}
async function infer(label){const response=await fetch('http://127.0.0.1:'+modelPort+'/v1/chat/completions',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({model:'SmolLM2-Startup',messages:[{role:'user',content:'Say hello in one short sentence.'}],max_tokens:24,temperature:0}),signal:AbortSignal.timeout(60000)});const value=await response.json();assert.ok(response.ok&&value.choices?.[0]?.message?.content);report.generations.push({label,text:value.choices[0].message.content,usage:value.usage});}
const save=()=>fs.writeFileSync(path.join(output,'report.json'),JSON.stringify(report,null,2));
async function cleanupPrivateNativeTemplate() {
 const owned = [nativeTemplate, nativeTemplateBackup];
 const marker = path.join(nativeState, 'native-unit-namespace-v1');
 const receipt = maybeStat(marker);
 if (owned.some(file => maybeStat(file))) assert.ok(receipt, 'private namespace receipt is missing');
 if (receipt) {
  assert.ok(receipt.isFile() && !receipt.isSymbolicLink() && receipt.uid === process.getuid());
  assert.equal(fs.readFileSync(marker, 'utf8'), `cmsd-native-unit-namespace-v1\n${nativePrefix}\n`);
 }
 assert.equal(await systemd('list-units', nativePrefix + '@*.service', '--all', '--no-legend', '--plain'), '', 'private namespace unexpectedly has a native model instance');
 const unexpected = fs.readdirSync(nativeUnitDirectory).filter(name => name.startsWith(nativePrefix + '@')
  && name.includes('.service') && !owned.includes(path.join(nativeUnitDirectory, name)));
 assert.deepEqual(unexpected, [], 'private namespace unexpectedly has an instance registration');
 const removed = [];
 for (const file of owned) {
  const metadata = maybeStat(file);
  if (!metadata) continue;
  assert.ok(metadata.isFile() && !metadata.isSymbolicLink() && metadata.uid === process.getuid());
  assert.ok(metadata.size <= 65536 && (metadata.mode & 0o022) === 0);
  const content = fs.readFileSync(file, 'utf8');
  assert.ok(content.startsWith('[Unit]\nDescription=MegaCube model engine (card %i)\n'));
  assert.ok(content.includes('EnvironmentFile=' + path.join(nativeState, 'env', '%i.env') + '\n'));
  if (file === nativeTemplate && nativeTemplateSha256) assert.equal(crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex'), nativeTemplateSha256);
  fs.unlinkSync(file);
  removed.push(path.basename(file));
 }
 await systemd('daemon-reload');
 report.private_namespace_templates_removed = removed;
 report.no_test_native_unit_instances = true;
}
async function cleanupPrivateResources() {
if(host){host.stdin.end();host.kill('SIGTERM');}if(browser)await browser.close();if(server)await server.close();if(modelCreated)await docker('rm','-f',container);if(installed){await systemd('enable','--runtime',unit);const result=await cli('action','uninstall');const state=await settle(result.operationId);assert.equal(state.operation.status,'succeeded');const link=path.join('/run/user',String(process.getuid()),'systemd/user/default.target.wants',unit);try{fs.lstatSync(link);throw new Error('dangling runtime startup link remains');}catch(error){if(error.code!=='ENOENT')throw error;}}else await systemd('stop',unit).catch(()=>{});
 await cleanupPrivateNativeTemplate();
 assert.equal(await systemd('show','cmsd.service','-p','MainPID','-p','ExecMainStartTimestampMonotonic'),original);assert.equal(crypto.createHash('sha256').update(fs.readFileSync(legacy)).digest('hex'),legacyHash);for(const [id,fact]of Object.entries(protectedContainers))assert.equal(await docker('inspect','--format','{{.State.Pid}} {{.State.StartedAt}} {{.RestartCount}}',id),fact);
 report.checks.push('Private service uninstalled and test container removed; original cmsd, model containers and legacy unit template unchanged');
}

try{
 const absent=await cli('status');assert.equal(absent.canInstall,true);assert.equal(absent.installed,false);
 hostLog=fs.createWriteStream(path.join(output,'native-host.log'));
 host=spawn(input.host,[],{env,stdio:['pipe','pipe','pipe']});host.stderr.pipe(hostLog);
 let readyResolve;const ready=new Promise(resolve=>readyResolve=resolve);
 readline.createInterface({input:host.stdout}).on('line',line=>{let value;try{value=JSON.parse(line);}catch{return;}if(value.ready)readyResolve();const job=pending.get(value.id);if(job){pending.delete(value.id);clearTimeout(job.timer);value.error?job.reject(new Error(value.error)):job.resolve(value.result);}});
 await Promise.race([ready,new Promise((_,reject)=>setTimeout(()=>reject(new Error('host not ready')),60000))]);
 const entry=`import React from 'react';import {createRoot} from 'react-dom/client';import '/src/styles/base.css';import '/src/styles/tailwind.css';import {CmsSettingsPanel} from '/src/features/megacube/CmsSettingsPanel.jsx';import {ModelGuidancePanel,ModelGuidanceView} from '/src/features/megacube/ModelGuidancePanel.jsx';import {dictZh} from '/src/shared/i18n/zh.js';import {dictEn} from '/src/shared/i18n/en.js';import {dictJa} from '/src/shared/i18n/ja.js';const p=new URLSearchParams(location.search),t=({zh:dictZh,en:dictEn,ja:dictJa})[p.get('lang')||'zh'];const device={isCube:true},h=React.createElement;createRoot(document.getElementById('root')).render(p.get('mode')==='recovery-preview'?h(ModelGuidanceView,{copy:t.uiMegacube.guidance,snapshot:{phase:'recover',canTry:true},onStart(){},onOpenModelStore(){},onOpenSettings(){}}):h(React.Fragment,null,h(ModelGuidancePanel,{copy:t.uiMegacube.guidance,device,existingUse:true,savedModels:[],onOpenSettings(){window.__openedDevice=true},onOpenModelStore(){window.__openedStore=true}}),h(CmsSettingsPanel,{copy:t.uiMegacube,onOpenModelStore(){window.__openedStore=true}})));`;
 server=await createServer({root:app,configFile:false,logLevel:'error',plugins:[react(),{name:'real-startup-acceptance',resolveId(id){if(id==='/startup-real.jsx')return '\0startup-real.jsx';},load(id){if(id==='\0startup-real.jsx')return entry;},configureServer(s){s.middlewares.use((req,res,next)=>{if(req.url?.split('?')[0]!=='/')return next();res.setHeader('Content-Type','text/html');res.end('<!doctype html><html><head><meta name="viewport" content="width=device-width, initial-scale=1"></head><body><div id="root" style="padding:12px;min-width:0"></div><script type="module" src="/startup-real.jsx"></script></body></html>');});}}],server:{host:'127.0.0.1',port:0,watch:null,hmr:false,ws:false}});await server.listen();
 browser=await puppeteer.launch({executablePath:input.chrome||'/usr/bin/google-chrome',headless:true,args:['--no-sandbox','--disable-gpu']});
 for(const lang of ['zh','en','ja'])for(const width of [390,1200]){await open(lang,width,'recovery-preview');assert.equal(await page.$eval('[data-testid="model-guidance"]',n=>n.dataset.mode),'banner');assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);if(width===390){const geometry=await page.$eval('[data-testid="model-guidance"]',n=>({width:n.querySelector('h3').getBoundingClientRect().width,height:n.getBoundingClientRect().height}));assert.ok(geometry.width>250&&geometry.height<330,JSON.stringify(geometry));}const screenshot=path.join(output,`recovery-${lang}-${width}.png`);await page.screenshot({path:screenshot});report.screenshots.push(screenshot);}
 report.checks.push('All three recovery banner translations fit 390px and 1200px (view-only layout fixtures)');
 for(const lang of ['zh','en','ja']){
   await open(lang,390);await page.waitForSelector('[data-testid="guidance-install-cmsd"]');
   assert.equal(await page.$eval('[data-testid="cms-service-introduction"] details',n=>n.open),true);
   assert.equal(await page.$$eval('[data-testid="cms-service-introduction"] li',nodes=>nodes.length),4);
   assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);
   await page.click('[data-testid="cms-service-prepare-install"]');
   await page.waitForSelector('[data-testid="cms-bundled-confirm"] [role="dialog"]');
   await page.waitForFunction(()=>!document.querySelector('[data-testid="cms-bundled-install"]').disabled);
   assert.equal(await page.$eval('[data-testid="cms-bundled-confirm"] [role="dialog"]',n=>n.getAttribute('aria-modal')),'true');
   assert.equal(await page.$('[data-testid="cms-bundled-confirm"] input[type="checkbox"]'),null);
   const dialogShot=path.join(output,`bundled-confirm-${lang}-390.png`);await page.screenshot({path:dialogShot});report.screenshots.push(dialogShot);
   await page.click('[data-testid="cms-bundled-cancel"]');assert.equal((await cli('status')).installed,false);
   const screenshot=path.join(output,`install-introduction-${lang}-390.png`);await page.screenshot({path:screenshot,fullPage:true});report.screenshots.push(screenshot);
   await page.click('[data-testid="guidance-mute"]');await page.waitForFunction(()=>!document.querySelector('[data-testid="model-guidance"]'));
   await page.waitForSelector('[data-testid="cms-install-reminder-restore"]');
   assert.equal((await cli('status')).installed,false);
   await open(lang,390);await page.waitForSelector('[data-testid="cms-install-reminder-restore"]');
   assert.equal(await page.$('[data-testid="model-guidance"]'),null);
   assert.ok(await page.$('[data-testid="cms-service-prepare-install"]'));
   await page.click('[data-testid="cms-install-reminder-restore"]');await page.waitForSelector('[data-testid="guidance-install-cmsd"]');
 }
 report.checks.push('Real absent-service state in three languages: installation intro expanded, persistent dismissal survives page reopen, direct installation remains reachable, manual restore synchronizes homepage');
 await open();await page.waitForSelector('[data-testid="guidance-install-cmsd"]');
 await page.evaluate(()=>Object.defineProperty(window,'localStorage',{configurable:true,get(){throw new Error('acceptance storage denied');}}));
 await page.click('[data-testid="guidance-mute"]');await page.waitForSelector('[data-testid="model-guidance"] [role="alert"]');
 assert.ok(await page.$('[data-testid="guidance-install-cmsd"]'));
 report.checks.push('Injected browser storage denial keeps installation banner visible and reports preference save failure');
 await open();await page.waitForSelector('[data-testid="guidance-install-cmsd"]');await page.click('[data-testid="guidance-install-cmsd"]');assert.equal(await page.evaluate(()=>window.__openedDevice),true);
 assert.equal((await cli('status')).installed,false);report.checks.push('Existing-user absent-service banner routes to device management without installing automatically');
 await open();await page.waitForSelector('[data-testid="guidance-install-cmsd"]');await page.click('[data-testid="guidance-mute"]');
 await page.waitForSelector('[data-testid="cms-install-reminder-restore"]');
 const installsBefore=calls.filter(c=>c.name==='cms_service_action'&&c.action==='install').length;
 await page.click('[data-testid="cms-service-prepare-install"]');await page.waitForFunction(()=>{const button=document.querySelector('[data-testid="cms-bundled-install"]');return button&&!button.disabled;});
 assert.equal(calls.filter(c=>c.name==='cms_service_action'&&c.action==='install').length,installsBefore);
 assert.equal(await page.$eval('[data-testid="cms-service-advanced"]',n=>n.open),false);
 await page.evaluate(()=>{const button=document.querySelector('[data-testid="cms-bundled-install"]');button.click();button.click();});
 await wait(async()=>{const s=await cli('status');if(s.operation?.status==='failed')throw new Error(s.operation.error);return s.installed&&s.activeState==='active';});installed=true;
 nativeTemplateSha256 = crypto.createHash('sha256').update(fs.readFileSync(nativeTemplate)).digest('hex');
 assert.equal(calls.filter(c=>c.name==='cms_service_action'&&c.action==='install').length,installsBefore+1);
 report.checks.push('Bundled package uses one modal confirmation with automatic integrity checks; cancellation leaves service absent; double click submits exactly one real private installation');
 const service=await cli('status');report.lingerEnabled=service.lingerEnabled;
 await page.waitForFunction(()=>!Object.keys(localStorage).some(key=>key.endsWith('.cmsInstallDismissed')));
 await page.waitForFunction(()=>!document.querySelector('[data-testid="guidance-install-cmsd"]'));
 assert.equal(await page.$eval('[data-testid="cms-service-introduction"] details',n=>n.open),false);
 report.checks.push('Actual confirmed cmsd installation clears only its installation dismissal and collapses the benefit details');
 const configPath=path.join(profile,'megacube/cmsd-component/cmsd.toml');const config=fs.readFileSync(configPath,'utf8');const configValue=key=>JSON.parse(config.match(new RegExp('^'+key+'\\s*=\\s*(.+)$','m'))[1]);
 const cardPath=path.join(configValue('cards_dir'),card);fs.mkdirSync(cardPath,{recursive:true,mode:0o700});
 const weights=path.resolve(input.weights);const files=fs.readdirSync(weights).filter(n=>n.endsWith('.safetensors'));
 fs.writeFileSync(path.join(cardPath,'common.sh'),'IMAGE="lmsysorg/sglang:v0.5.19"\nCONTAINER_NAME='+JSON.stringify(container)+'\nMODEL_ID="SmolLM2-Startup"\nCMSD_RESERVED_GIB="1.5"\nLMM_BIG_FILES=('+files.map(n=>JSON.stringify(n+':'+fs.statSync(path.join(weights,n)).size)).join(' ')+')\nLMM_SMALL_FILES=("config.json")\n');
 fs.writeFileSync(path.join(cardPath,'.deploy-env'),'IMAGE="lmsysorg/sglang:v0.5.19"\nCONTAINER_NAME='+JSON.stringify(container)+'\nMODEL_DIR='+JSON.stringify(weights)+'\nPORT='+modelPort+'\n');
 fs.writeFileSync(path.join(cardPath,'card.json'),JSON.stringify({name:'SmolLM2-Startup',engine_name:'sglang'}));
 const template=fs.readFileSync(input.cpuServer,'utf8');assert.ok(template.includes("MODEL_ID = 'SmolLM2-135M-Instruct-qa-cpu'"));fs.writeFileSync(path.join(cardPath,'server.py'),template.replace("MODEL_ID = 'SmolLM2-135M-Instruct-qa-cpu'","MODEL_ID = 'SmolLM2-Startup'"));
 await docker('create','--name',container,'--label','megacube.acceptance=startup-'+nonce,'--memory','2g','--cpus','2','--restart=no','-p',`127.0.0.1:${modelPort}:8080`,'-v',weights+':/qa/model:ro','-v',path.join(cardPath,'server.py')+':/qa/server.py:ro','--entrypoint','python3','lmsysorg/sglang:v0.5.19','/qa/server.py');modelCreated=true;
 await open();assert.equal(await page.$('[data-testid="guidance-install-cmsd"]'),null);
 // Discovered unowned card is only adopted after an explicit enable click.
 await page.waitForFunction(card=>[...document.querySelectorAll('.cms-model')].some(n=>n.textContent.includes(card)),{},'SmolLM2-Startup');
 await page.evaluate(()=>{for(const d of document.querySelectorAll('details'))if(d.querySelector(':scope > summary')?.textContent.includes('其他已停止模型'))d.open=true;});
 await page.evaluate(()=>{const article=[...document.querySelectorAll('.cms-model')].find(n=>n.textContent.includes('SmolLM2-Startup'));article.querySelector('button.cms-primary').click();});
 await wait(async()=>{const models=await api('GET','/v1/models');const m=models.models.find(m=>m.card===card);return m?.managed&&m.status==='running'&&m.healthy;});
 await infer('after-explicit-UI-enable');
 await open();const modelSwitch=`[data-testid="cms-model-startup-${card}"] [role="switch"]`;await page.waitForSelector(modelSwitch);await page.waitForFunction(s=>!document.querySelector(s).disabled,{},modelSwitch);
 const runtimeBefore=await docker('inspect','--format','{{.State.Pid}} {{.State.StartedAt}}',container);
 await page.click(modelSwitch);await wait(async()=>!(await api('GET','/v1/models')).models.find(m=>m.card===card).boot_enabled);
 let row=(await api('GET','/v1/models')).models.find(m=>m.card===card);assert.equal(row.recovery.desired_running,true);assert.equal(row.status,'running');
 for (let round = 0; round < 10; round++) {
  for (const desired of [true, false]) {
   const beforeActions = calls.filter(c => c.name === 'cms_model_action').length;
   // Polling can replace the switch between Puppeteer's pointer move and click.
   // Check and activate the current enabled DOM control in one browser task.
   await wait(() => page.evaluate((selector, enabled) => {
    const control = document.querySelector(selector);
    if (!control || control.disabled || control.getAttribute('aria-checked') !== String(!enabled)) return false;
    control.click();
    return true;
   }, modelSwitch, desired));
   await wait(async () => (await api('GET', '/v1/models')).models.find(m => m.card === card).boot_enabled === desired);
   await page.waitForFunction((selector, enabled) => {
    const control = document.querySelector(selector);
    return !control.disabled && control.getAttribute('aria-checked') === String(enabled);
   }, {}, modelSwitch, desired);
   assert.equal(calls.filter(c => c.name === 'cms_model_action').length, beforeActions + 1);
  }
 }
 const policyFile=path.join(profile,'megacube/cmsd-component/state/recovery',card+'.json');
 const policyMtime=fs.statSync(policyFile,{bigint:true}).mtimeNs;
 const revisionFile=path.join(profile,'megacube/cmsd-component/state/lifecycle-revision.json');
 const revisionBefore=fs.readFileSync(revisionFile,'utf8');
 const beforeDuplicate=fs.readFileSync(path.join(profile,'megacube/cmsd-component/state/recovery',card+'.json'),'utf8');
 for(let i=0;i<20;i++)await lifecycle('disable-boot');
 assert.equal(fs.readFileSync(path.join(profile,'megacube/cmsd-component/state/recovery',card+'.json'),'utf8'),beforeDuplicate);
 assert.equal(fs.statSync(policyFile,{bigint:true}).mtimeNs,policyMtime);assert.equal(fs.readFileSync(revisionFile,'utf8'),revisionBefore);
 report.checks.push('Ten UI enable/disable cycles and twenty duplicate disable requests preserve runtime PID and do not rewrite identical policy');
 const servicePid=await systemd('show',unit,'-p','MainPID');
 await page.click('[data-testid="cms-service-startup"] [role="switch"]');await wait(async()=>!(await cli('status')).bootEnabled);assert.equal(await systemd('show',unit,'-p','MainPID'),servicePid);assert.equal(await docker('inspect','--format','{{.State.Pid}} {{.State.StartedAt}}',container),runtimeBefore);
 await systemd('enable','--runtime',unit);assert.equal((await cli('status')).bootEnabled,false);await systemd('disable',unit);
 report.checks.push('Model and cmsd startup switches persist false without stopping/restarting either process');
 report.checks.push('Temporary systemd enablement is not reported as persistent next-boot policy');
 for(const lang of ['zh','en','ja']){await open(lang,390);await page.waitForSelector(modelSwitch);assert.equal(await page.$eval(modelSwitch,n=>n.getAttribute('aria-checked')),'false');assert.equal(await page.$eval('[data-testid="cms-service-startup"] [role="switch"]',n=>n.getAttribute('aria-checked')),'false');assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false);const screenshot=path.join(output,`startup-${lang}-390.png`);await page.screenshot({path:screenshot});report.screenshots.push(screenshot);}
 await infer('after-UI-close-reopen');
 const restart=await cli('action','restart');assert.equal((await settle(restart.operationId)).operation.status,'succeeded');
 row=(await api('GET','/v1/models')).models.find(m=>m.card===card);assert.equal(row.boot_enabled,false);assert.equal(row.recovery.desired_running,true);assert.equal(await docker('inspect','--format','{{.State.Pid}} {{.State.StartedAt}}',container),runtimeBefore);
 await infer('after-cmsd-restart');report.checks.push('Reopened UI in three languages and restarted cmsd: disabled preferences remain, running model survives and still infers');
 if(input.oldPackage){const manifest=JSON.parse(fs.readFileSync(path.join(input.oldPackage,'manifest.json')));const pid=await systemd('show',unit,'-p','MainPID');const update=await cli('action','update',input.oldPackage,manifest.sha256);const rejected=await settle(update.operationId);assert.equal(rejected.operation.status,'failed');assert.ok(rejected.operation.error.includes('component_boot_policy_unsupported'));assert.equal(await systemd('show',unit,'-p','MainPID'),pid);report.checks.push('Old cmsd component is rejected before stopping current service once independent boot policy is persisted');}
 await lifecycle('disable');await lifecycle('enable-boot');
 await systemd('restart',unit);await wait(async()=>{try{return (await api('GET','/v1/status')).version;}catch{return false;}});
 await new Promise(r=>setTimeout(r,3500));assert.equal(await docker('inspect','--format','{{.State.Running}}',container),'false');row=(await api('GET','/v1/models')).models.find(m=>m.card===card);assert.equal(row.boot_enabled,true);assert.equal(row.recovery.desired_running,false);
 report.checks.push('Enabling boot for a stopped model and restarting cmsd on the same boot does not start the model');
 const recoveryPath=path.join(configValue('state_dir'),'recovery',card+'.json');
 await systemd('stop',unit);let recovery=JSON.parse(fs.readFileSync(recoveryPath));recovery.boot_id='acceptance-prior-boot';fs.writeFileSync(recoveryPath,JSON.stringify(recovery));await systemd('start',unit);
 await wait(async()=>{try{return (await api('GET','/v1/models')).models.find(m=>m.card===card)?.healthy && (await docker('inspect','--format','{{.State.Running}}',container))==='true';}catch{return false;}});await infer('after-simulated-next-boot');
 assert.ok((await api('GET','/v1/tasks')).tasks.some(t=>t.kind==='recover'&&t.card===card&&t.status==='succeeded'));
 await lifecycle('disable-boot');await systemd('stop',unit);await docker('stop','-t','10',container);recovery=JSON.parse(fs.readFileSync(recoveryPath));recovery.boot_id='acceptance-second-prior-boot';fs.writeFileSync(recoveryPath,JSON.stringify(recovery));await systemd('start',unit);await wait(async()=>{try{return (await api('GET','/v1/status')).version;}catch{return false;}});await new Promise(r=>setTimeout(r,5000));assert.equal(await docker('inspect','--format','{{.State.Running}}',container),'false');
 assert.equal((await api('GET','/v1/models')).models.find(m=>m.card===card).recovery.desired_running,false);
 report.checks.push('Single-host simulated next boot: enabled model recovers through budget and infers; disabled boot preference stays stopped');
 report.passed=true;
}catch(error){report.errors.push(error.stack);throw error;}
finally{
 try { await cleanupPrivateResources(); } catch(error){report.passed=false;report.errors.push('cleanup: '+error.stack);}hostLog?.end();report.finishedAt=new Date().toISOString();report.durationSeconds=(Date.now()-began)/1000;report.artifactSha256={};for(const [key,file]of Object.entries({pinvou:input.binary,host:input.host,cmsd:path.join(input.resources,'runtime/cmsd/cmsd')}))report.artifactSha256[key]=crypto.createHash('sha256').update(fs.readFileSync(file)).digest('hex');save();
}
console.log(JSON.stringify({passed:report.passed,checks:report.checks.length,generations:report.generations.length,evidence:output}));

if (!report.passed) process.exitCode = 1;
