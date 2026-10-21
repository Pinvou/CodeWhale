#!/usr/bin/env node
// Fixture browser regression by default; --snapshot FILE accepts a supplied read-only inventory.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import puppeteer from 'puppeteer-core';
import { createServer } from 'vite';
import { mergeModels } from '../src/features/model-store/model-store-logic.js';

if (process.argv.length > 2 && (process.argv[2] !== '--snapshot' || !process.argv[3])) throw new Error('Usage: model_store_ownership_smoke.mjs [--snapshot FILE]');
const snapshot = process.argv[2] === '--snapshot'
  ? JSON.parse(fs.readFileSync(path.resolve(process.argv[3]), 'utf8'))
  : { source: 'deterministic browser fixture', models: [
    { card_id: 'managed', card_name: 'Managed model', status: 'deployed', running: true, cmsd: { managed: true, status: 'running' } },
    { card_id: 'observed', card_name: 'Observed model', status: 'deployed', running: true, cmsd: { managed: false, status: 'running', canAdopt: true } },
    { card_id: 'downloaded', card_name: 'Downloaded model', status: 'pending', running: false, cmsd: { managed: false, status: 'stopped', canAdopt: false } },
  ] };
const output = process.env.CMS_UI_EVIDENCE ? path.resolve(process.env.CMS_UI_EVIDENCE)
  : fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-ownership-ui-'));
fs.mkdirSync(output, { recursive: true });
const app = fileURLToPath(new URL('..', import.meta.url)); process.chdir(app);
const expected = mergeModels([], snapshot);
const report = { source: snapshot.source, scope: 'Read-only model rows from a fixture or supplied snapshot; no full Tauri navigation or lifecycle execution', checks: [], errors: [], passed: false };
const entry = `import React from 'react';
import {createRoot} from 'react-dom/client';
import '/src/styles/base.css';import '/src/styles/tailwind.css';
import {ModelRow} from '/src/features/model-store/ModelRow.jsx';
import {ModelStoreView} from '/src/features/model-store/ModelStoreView.jsx';
import {mergeModels} from '/src/features/model-store/model-store-logic.js';
import {dictZh} from '/src/shared/i18n/zh.js';import {dictEn} from '/src/shared/i18n/en.js';import {dictJa} from '/src/shared/i18n/ja.js';
const params=new URLSearchParams(location.search),dict=({zh:dictZh,en:dictEn,ja:dictJa})[params.get('lang')||'zh'];
const copy=dict.uiModelStore,h=React.createElement;
window.__actions=[];const action=()=>window.__actions.push('unexpected action');
if(params.has('poll')){
 window.__reads=[];window.__navigated=false;
 window.__library={models:[{card_id:'refresh-card',card_name:'Refresh model',status:'deployed',running:false,cmsd:{managed:true,status:'stopped'}}]};
 window.__TAURI__={core:{invoke:async command=>{
  window.__reads.push(command);
  if(command==='model_store_availability')return {available:true};
  if(command==='model_store_library')return structuredClone(window.__library);
  if(command==='model_store_cards'||command==='model_store_active_runs')return [];
  if(command==='model_store_test_store_state')return {enabled:false};
  if(command==='model_store_take_startup_cleanup')return {killed:[],skipped_root:[]};
  throw new Error('Fixture rejects '+command);
 }}};
 createRoot(document.getElementById('root')).render(h(ModelStoreView,{theme:'light',t:dict,bs:{savedModels:[]}}));
}else fetch('/snapshot.json').then(r=>r.json()).then(library=>createRoot(document.getElementById('root')).render(h('main',{style:{padding:12}},mergeModels([],library).map(model=>h(ModelRow,{key:model.id,model,copy,onOpen:action,onDownload:action,onDeploy:action,onEnable:action,onStop:action,onUninstall:action})))));
`;

const server = await createServer({ root: app, configFile: false, logLevel: 'error', plugins: [react(), {
  name: 'ownership-presentation', resolveId(id) { if (id === '/ownership.jsx') return '\0ownership.jsx'; },
  load(id) { if (id === '\0ownership.jsx') return entry; },
  configureServer(s) { s.middlewares.use((req, res, next) => {
    if (req.url?.split('?')[0] === '/snapshot.json') { res.setHeader('Content-Type', 'application/json'); res.end(JSON.stringify(snapshot)); return; }
    if (req.url?.split('?')[0] !== '/') return next();
    res.setHeader('Content-Type', 'text/html'); res.end('<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1"></head><body><div id="root"></div><script type="module" src="/ownership.jsx"></script></body></html>');
  }); },
}], server: { host: '127.0.0.1', port: 0, watch: null, hmr: false, ws: false } });
let browser;
try {
  await server.listen();
  browser = await puppeteer.launch({ executablePath: process.env.CHROME || '/usr/bin/google-chrome', headless: true, args: ['--no-sandbox', '--disable-gpu'] });
  const page = await browser.newPage(); page.on('pageerror', error => report.errors.push(error.message));
  for (const lang of ['zh', 'en', 'ja']) for (const width of [390, 1200]) {
    await page.setViewport({ width, height: 900 }); await page.goto(server.resolvedUrls.local[0] + '?lang=' + lang, { waitUntil: 'networkidle0' });
    await page.waitForFunction(count => document.querySelectorAll('[data-testid="model-store-row"]').length === count, {}, expected.length);
    const rows = await page.$$eval('[data-testid="model-store-row"]', nodes => nodes.map(n => ({ card: n.dataset.cardId, state: n.dataset.state,
      label: n.querySelector('[data-testid="model-store-row-state"]').textContent,
      ownership: n.querySelector('[data-testid="model-store-row-ownership"]')?.textContent,
      stopDisabled: n.querySelector('[data-testid="model-store-stop"]')?.disabled,
      separated: !n.querySelector('[data-testid="model-store-row-state"]').parentElement.querySelector('[data-testid="model-store-row-cmsd"]') })));
    for (const model of expected) {
      const row = rows.find(r => r.card === model.id); assert.equal(row.state, model.state); assert.ok(row.label); assert.ok(row.ownership); assert.equal(row.separated, true);
      if (model.state === 'running' && model.record.cmsd?.managed === false) assert.equal(row.stopDisabled, true);
    }
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    assert.deepEqual(await page.evaluate(() => window.__actions), []);
    await page.screenshot({ path: path.join(output, `${lang}-${width}.png`), fullPage: true });
    report.checks.push({ lang, width, rows });
  }
  await page.setViewport({width:1200,height:900});
  await page.goto(server.resolvedUrls.local[0]+'?lang=zh&poll=1',{waitUntil:'networkidle0'});
  await page.waitForSelector('[data-card-id="refresh-card"][data-state="deployed"]');
  const cardReads=await page.evaluate(()=>window.__reads.filter(name=>name==='model_store_cards').length);
  await page.evaluate(()=>{window.__library.models[0].running=true;window.__library.models[0].cmsd.status='running';});
  await page.waitForSelector('[data-card-id="refresh-card"][data-state="running"]',{timeout:8000});
  assert.equal(await page.evaluate(()=>window.__reads.filter(name=>name==='model_store_cards').length),cardReads);
  report.checks.push({case:'An external lifecycle change refreshes the mounted store within the local polling interval without fetching the remote catalogue'});
  await page.evaluate(()=>{window.__library.models[0].running=false;window.__library.models[0].cmsd.status='stopped';window.dispatchEvent(new Event('focus'));});
  await page.waitForSelector('[data-card-id="refresh-card"][data-state="deployed"]');
  report.checks.push({case:'Focus refresh reconciles a stopped current deployment'});
  assert.deepEqual(report.errors, []); report.passed = true;
} catch (error) { report.errors.push(error.stack); throw error; }
finally { fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify(report, null, 2)); await browser?.close(); await server.close(); }
console.log(JSON.stringify({ passed: report.passed, cases: report.checks.length, output }));
