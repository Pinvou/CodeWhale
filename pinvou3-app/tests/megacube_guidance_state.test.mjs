import assert from 'node:assert/strict';
import { after, test } from 'node:test';
import { fileURLToPath } from 'node:url';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import { guidanceStorageKey, readGuidanceHistory, rememberGuidance, rememberGuidanceDefault, modelGuidanceState, hasLinkedGuidanceService, tryStartGuidanceDefault } from '../src/features/megacube/model-guidance-state.js';
const vite = await createServer({configFile:false,root:fileURLToPath(new URL('..', import.meta.url)),logLevel:'error',server:{middlewareMode:true,watch:null,hmr:false,ws:false},optimizeDeps:{noDiscovery:true},plugins:[react()]});
const { ModelGuidanceView } = await vite.ssrLoadModule('/src/features/megacube/ModelGuidancePanel.jsx');
const { CMS_MODEL_GUIDANCE_COPY } = await vite.ssrLoadModule('/src/shared/i18n/cms-model-guidance.js');
const { CmsDaemonIntroduction } = await vite.ssrLoadModule('/src/features/megacube/CmsDaemonIntroduction.jsx');
const { CMSD_SERVICE_COPY } = await vite.ssrLoadModule('/src/shared/i18n/cmsd-service.js');
globalThis.__guidanceCalls=[];
after(async()=>{await vite.close();delete globalThis.__guidanceCalls;});
const initialHistory={known:true,seenService:false,skippedSetup:false,used:false,lastDefault:null};
const modelObservation=()=>({device:{supported:true,detected:true,hardware:{state:'compatible'},running:{state:'idle'}}, service:{supported:true,installed:true},status:{available:true,data:{active_model:'local-model'}},models:{available:true,data:{models:[{card:'local-model',managed:true,status:'stopped',healthy:false}]}},tasks:{available:true,data:{tasks:[]}},discovery:{candidates:[]}});

test('a dotted default survives history and starts through the same recovery action', async () => {
  const values = new Map();
  const storage = { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value) };
  const key = guidanceStorageKey('a'.repeat(64));
  assert.equal(rememberGuidanceDefault(storage, key, 'qwen3.5', 'Qwen 3.5', 123), true);
  const history = readGuidanceHistory(storage, key);
  assert.equal(history.lastDefault.card, 'qwen3.5');
  const observation = modelObservation();
  observation.status.data.active_model = 'qwen3.5';
  observation.models.data.models[0].card = 'qwen3.5';
  const state = modelGuidanceState(observation, history);
  assert.equal(state.card, 'qwen3.5');
  assert.equal(state.phase, 'recover');
  assert.equal(state.canTry, true);
  const started = [];
  const reply = await tryStartGuidanceDefault(state.card, {
    queryStatus: async () => observation.status, queryModels: async () => observation.models,
    queryTasks: async () => observation.tasks,
    start: async card => { started.push(card); return { task_id: 'dotted-recovery' }; },
  });
  assert.equal(reply.task_id, 'dotted-recovery');
  assert.deepEqual(started, ['qwen3.5']);
});

test('invalid recovery identities cannot be persisted or submitted', async () => {
  for (const card of ['', '.', '..', 'a/b', 'a?b', 'a\nb', 'a'.repeat(129)]) {
    const values = new Map();
    const storage = { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value) };
    const key = guidanceStorageKey('a'.repeat(64));
    assert.equal(rememberGuidanceDefault(storage, key, card, 'Invalid', 123), false);
    values.set(`${key}.default`, JSON.stringify({ card, modelName: 'Invalid', observedAt: 123 }));
    assert.equal(readGuidanceHistory(storage, key).known, false);
    await assert.rejects(tryStartGuidanceDefault(card, {
      queryStatus: async () => { throw new Error('should_not_query'); },
    }), /guidance_default_changed/);
  }
});
test('only verified installation absence offers bundled cmsd; offline and uncertain services never do', () => {
  const observation = modelObservation();
  for (const service of [null, {}, { supported: true, installed: false, scope: 'unverified' }, { supported: true, installed: true }]) {
    assert.notEqual(modelGuidanceState({ ...observation, service }, initialHistory).phase, 'installCms');
  }
  assert.equal(modelGuidanceState({ ...observation, service: { supported: true, installed: false, canInstall: true } }, initialHistory).phase, 'installCms');
  for (const copy of Object.values(CMS_MODEL_GUIDANCE_COPY)) for (const phase of ['installCms']) {
    const html = renderToStaticMarkup(React.createElement(ModelGuidanceView, { copy, snapshot: { phase }, onOpenSettings() {}, onOpenModelStore() {} }));
    assert.ok(html.includes('data-mode="banner"')); assert.ok(html.includes(copy.titles[phase]));
    assert.ok(html.split('<details')[0].includes(copy.installCms));
  }
});

test('actual discovery readiness differs from process observations, pointers and unloaded downloads', () => {
  const observation = modelObservation(); observation.device.running = { state: 'running' };
  assert.equal(modelGuidanceState(observation, initialHistory).phase, 'recover');
  const candidate = { status: 'ready', base_url: 'http://127.0.0.1:18080/v1', models: [{ id: 'served', loaded: false }] };
  observation.discovery.candidates = [candidate];
  assert.equal(modelGuidanceState(observation, initialHistory).phase, 'recover');
  candidate.models[0].loaded = null;
  assert.equal(modelGuidanceState(observation, initialHistory).phase, 'recover');
  candidate.models[0].loaded = true;
  assert.equal(modelGuidanceState(observation, initialHistory).phase, 'available');
  candidate.base_url = 'https://api.example.com/v1';
  assert.notEqual(modelGuidanceState(observation, initialHistory).phase, 'available');
});

test('offline, failed and malformed observations do not assert that models are stopped', () => {
  const observation = modelObservation();
  for (const change of [{ discovery: null }, { discovery: {} }, { device: null }, { models: null }, { tasks: null },
    { models: { available: true, data: { models: {} } } }, { tasks: { available: true, data: { tasks: {} } } }]) {
    assert.equal(modelGuidanceState({ ...observation, ...change }, initialHistory, true).phase, 'unknown');
  }
  assert.equal(modelGuidanceState(observation, { ...initialHistory, known: false }).phase, 'unknown');
  const dormant = { ...observation, status: { available: false, reason: 'socket_missing: absent' }, models: null, tasks: null };
  assert.equal(modelGuidanceState(dormant, initialHistory).phase, 'unknown');
  assert.equal(modelGuidanceState(dormant, initialHistory).canTry, false);
  assert.equal(modelGuidanceState({ ...dormant, status: { available: false, reason: 'connect_failed' } }, initialHistory).phase, 'unknown');
  assert.equal(modelGuidanceState({ ...observation, device: { ...observation.device, hardware: { state: 'mismatch' } } }, initialHistory).phase, 'unknown');
});

test('history survives new readers, is isolated by profile/socket scope and never grants startup authority', () => {
  const values = new Map(); const storage = { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value) };
  const key = guidanceStorageKey('a'.repeat(64)); const other = guidanceStorageKey('b'.repeat(64));
  assert.deepEqual(readGuidanceHistory(storage, key), initialHistory);
  assert.equal(rememberGuidance(storage, key, 'seen', 100), true);
  assert.equal(readGuidanceHistory(storage, key).seenService, true);
  assert.equal(readGuidanceHistory(storage, other).seenService, false);
  assert.equal(rememberGuidanceDefault(storage, key, 'previous', 'Previous model', 200), true);
  assert.equal(rememberGuidanceDefault(storage, key, 'older', 'Older model', 100), true);
  assert.equal(readGuidanceHistory(storage, key).lastDefault.card, 'previous');
  assert.equal(rememberGuidance(storage, key, 'skipped', 200), true);
  assert.equal(readGuidanceHistory(storage, key).skippedSetup, true);
  values.set(`${key}.seen`, 'bad'); assert.equal(readGuidanceHistory(storage, key).known, false);
  assert.equal(readGuidanceHistory(storage, guidanceStorageKey('bad')).known, false);
  assert.equal(rememberGuidance({ setItem() { throw new Error('quota'); } }, key, 'seen'), false);
  assert.equal(readGuidanceHistory({ getItem() { throw new Error('denied'); } }, key).known, false);
});

test('polling and multiple windows do not keep rewriting history or trigger storage-event feedback', () => {
  const values = new Map(); let writes = 0;
  const storage = { getItem: key => values.get(key) ?? null, setItem: (key, value) => { writes++;values.set(key, value); } };
  const key = guidanceStorageKey('a'.repeat(64));
  for (let i = 1; i <= 100; i++) {
    rememberGuidance(storage, key, 'seen', i);
    rememberGuidanceDefault(storage, key, 'same', 'Same model', i);
  }
  assert.equal(writes, 2);
  rememberGuidanceDefault(storage, key, 'changed', 'New model', 200);
  rememberGuidanceDefault(storage, key, 'same', 'Same model', 150);
  assert.equal(writes, 3); assert.equal(readGuidanceHistory(storage, key).lastDefault.card, 'changed');
});

test('stopping or deleting a default does not erase the optional historical startup target', () => {
  const observation = modelObservation(); observation.status.data.active_model = null;
  const history = { ...initialHistory, seenService: true, lastDefault: { card: 'local-model', modelName: 'Previous model', observedAt: 200 } };
  const stopped = modelGuidanceState(observation, history);
  assert.equal(stopped.phase, 'recover'); assert.equal(stopped.canTry, true); assert.equal(stopped.historicalDefault, true);
  observation.models.data.models = [];
  const removed = modelGuidanceState(observation, history);
  assert.equal(removed.canTry, true); assert.equal(removed.missingDefault, true); assert.equal(removed.modelName, 'Previous model');
});

test('explicit default startup rechecks ownership/default/tasks and does not adopt or bypass an outage', async () => {
  const observation = modelObservation(); let writes = 0;
  const io = { queryStatus: async () => observation.status, queryModels: async () => observation.models,
    queryTasks: async () => observation.tasks, start: async () => { ++writes; return { task_id: 'real-task' }; } };
  assert.equal((await tryStartGuidanceDefault('local-model', io)).task_id, 'real-task');
  assert.equal(writes, 1);
  observation.models.data.models[0].managed = false;
  await assert.rejects(tryStartGuidanceDefault('local-model', io), /guidance_ownership_required/);
  observation.models.data.models = [];
  observation.status.available = false;
  await assert.rejects(tryStartGuidanceDefault('local-model', io), /guidance_unconfirmed/);
  observation.status.available = true; observation.status.data.active_model = 'other';
  await assert.rejects(tryStartGuidanceDefault('local-model', io), /guidance_default_changed/);
  assert.equal(writes, 1);
  observation.tasks.data.tasks = [];
  observation.status.data.active_model = 'local-model';
  observation.models.data.models = [{ card: 'local-model', managed: true, status: 'running', healthy: true }];
  assert.equal((await tryStartGuidanceDefault('local-model', io)).already_ready, true);
  assert.equal(writes, 1);
});

test('missing-default attempt reports rejection once, while an existing startup is queried rather than resubmitted', async () => {
  const observation = modelObservation(); observation.models.data.models = []; let writes = 0;
  const io = { queryStatus: async () => observation.status, queryModels: async () => observation.models,
    queryTasks: async () => observation.tasks, start: async () => { ++writes; return { error: { code: 'unknown_card' } }; } };
  await assert.rejects(tryStartGuidanceDefault('local-model', io), /unknown_card/); assert.equal(writes, 0);
  observation.tasks.data.tasks = [{ id: 'existing-task', card: 'local-model', kind: 'enable', status: 'running' }];
  assert.equal(modelGuidanceState(observation, initialHistory).phase, 'starting');
  assert.equal((await tryStartGuidanceDefault('local-model', io)).task_id, 'existing-task');
  assert.equal(writes, 0);
});

test('all guidance variants render real trilingual components without submitting any lifecycle action', () => {
  for (const copy of Object.values(CMS_MODEL_GUIDANCE_COPY)) for (const phase of ['recover', 'unknown', 'starting', 'ready']) {
    const html = renderToStaticMarkup(React.createElement(ModelGuidanceView, { copy,
      snapshot: { phase, canTry: phase === 'recover', modelName: 'local-model', missingDefault: true },
      onOpenModelStore() {}, onOpenSettings() {} }));
    assert.ok(html.includes(copy.titles[phase])); assert.ok(html.includes(copy.openStore)); assert.ok(html.includes(copy.openDevice));
    if (phase === 'recover') assert.ok(html.includes(copy.tryDefault));
    if (phase === 'unknown') assert.ok(!html.includes(`>${copy.tryDefault}<`));
  }
});

test('unavailable-model banners keep the store and errors visible with recovery options initially collapsed', () => {
  for (const copy of Object.values(CMS_MODEL_GUIDANCE_COPY)) {
    const html = renderToStaticMarkup(React.createElement(ModelGuidanceView, { copy,
      snapshot: { phase: 'recover', canTry: true, modelName: 'previous-default' },
      error: 'Error: [unknown_card] internal diagnostic', onOpenModelStore() {}, onOpenSettings() {} }));
    const [banner, options] = html.split('<details');
    assert.ok(banner.includes('data-mode="banner"'));
    assert.ok(banner.includes(copy.titles.recover));
    assert.ok(banner.includes(copy.openStore));
    assert.ok(banner.includes('role="alert"') && banner.includes(copy.errors.unknown_card));
    assert.ok(!banner.includes(copy.tryDefault));
    assert.ok(options.includes(copy.tryDefault) && options.includes(copy.openDevice));
    assert.doesNotMatch(options.split('>')[0], /\bopen(?:=|\s|$)/);
  }
});

test('the no-running-local-service explanation is visible only in setup and recovery banners', () => {
  const explanations = { zh: '当前无正在运行的本地模型服务可供使用。', en: 'No running local model service is available for use.', ja: '現在利用できる稼働中のローカルモデルサービスがありません。' };
  for (const [language, copy] of Object.entries(CMS_MODEL_GUIDANCE_COPY)) {
    for (const phase of ['recover', 'unknown', 'starting', 'ready']) {
      const html = renderToStaticMarkup(React.createElement(ModelGuidanceView, { copy, snapshot: { phase }, onOpenModelStore() {} }));
      assert.equal(html.split('<details')[0].includes(explanations[language]), phase === 'recover');
    }
  }
});

test('selected-model failure and unlinked-service banners explain the chat problem in all languages', () => {
  for (const copy of Object.values(CMS_MODEL_GUIDANCE_COPY)) for (const phase of ['connectionUnavailable', 'connectionUnknown', 'unlinked']) {
    const html = renderToStaticMarkup(React.createElement(ModelGuidanceView, { copy,
      snapshot: { phase, selectedModelName: 'Ornith 35B', available: [{}] }, onOpenModelStore() {} }));
    const banner = html.split('<details')[0];
    assert.ok(banner.includes(copy.titles[phase])); assert.ok(banner.includes(copy.bannerHints[phase]));
    assert.ok(banner.includes(copy.openStore));
    assert.ok(!banner.includes(copy.titles.ready));
    if (phase !== 'unlinked') assert.ok(banner.includes(copy.runningServiceHint));
  }
});

test('an endpoint is linked only to a matching saved local model, including the fixed gateway alias', () => {
  const services = [{ base_url: 'http://127.0.0.1:8000/v1', models: [null, { id: 'served', loaded: true }] }];
  assert.equal(hasLinkedGuidanceService(services, [{ base_url: services[0].base_url+'/', model: 'served' }]), true);
  assert.equal(hasLinkedGuidanceService(services, [{ base_url: services[0].base_url, model: 'megacube-default' }]), true);
  assert.equal(hasLinkedGuidanceService(services, [{ base_url: services[0].base_url, model: 'different' }]), false);
  assert.equal(hasLinkedGuidanceService(services, null), false);
});

test('missing files or deployment are routed to the store without submitting startup', async () => {
  const observation = modelObservation(); let writes = 0;
  const io = { queryStatus: async () => observation.status, queryModels: async () => observation.models,
    queryTasks: async () => observation.tasks, start: async () => { writes++;return { task_id: 'unexpected' }; } };
  observation.models.data.models[0].weights_complete = false;
  await assert.rejects(tryStartGuidanceDefault('local-model', io), /guidance_files_missing/);
  delete observation.models.data.models[0].weights_complete; observation.models.data.models[0].deployed = false;
  await assert.rejects(tryStartGuidanceDefault('local-model', io), /guidance_files_missing/);
  assert.equal(writes, 0);
});

test('persistent reminder action is translated, optional and absent from setup/task progress', () => {
  for (const copy of Object.values(CMS_MODEL_GUIDANCE_COPY)) {
    for (const phase of ['recover', 'unknown', 'connectionUnavailable', 'connectionUnknown', 'unlinked']) {
      const html = renderToStaticMarkup(React.createElement(ModelGuidanceView, {
        copy, snapshot: { phase }, onMute() {}, onDismiss() {}, error: 'guidance_mute_failed',
      }));
      const banner = html.split('<details')[0];
      assert.ok(banner.includes(`>${copy.mute}</button>`));
      assert.ok(banner.includes(copy.dismiss));
      assert.ok(banner.includes(copy.errors.guidance_mute_failed));
      assert.ok(html.includes(copy.muteHint));
    }
    for (const phase of ['starting']) {
      const html = renderToStaticMarkup(React.createElement(ModelGuidanceView, { copy, snapshot: { phase }, onMute() {} }));
      assert.ok(!html.includes(`>${copy.mute}</button>`));
      if (phase === 'starting') assert.ok(html.includes(copy.background));
    }
  }
});

test('installation reminder uses its own translated action and reset explanation', () => {
  for (const copy of Object.values(CMS_MODEL_GUIDANCE_COPY)) {
    const html = renderToStaticMarkup(React.createElement(ModelGuidanceView, {
      copy, snapshot: { phase: 'installCms' }, onMuteInstall() {}, onMute() {}, onOpenSettings() {},
    }));
    assert.ok(html.includes(`>${copy.mute}</button>`));
    assert.ok(html.includes(copy.installMuteHint));
    assert.ok(!html.includes(copy.muteHint));
    assert.ok(html.includes('data-testid="guidance-install-cmsd"'));
  }
});

test('service introduction explains value in all languages and keeps installed users’ details collapsed', () => {
  for (const copy of Object.values(CMSD_SERVICE_COPY)) for (const expanded of [false, true]) {
    const html = renderToStaticMarkup(React.createElement(CmsDaemonIntroduction, { copy, expanded }));
    assert.ok(html.includes(copy.introduction));
    assert.ok(html.includes(expanded ? copy.benefitsTitle : copy.aboutService));
    assert.ok(html.includes(copy.installBoundary));
    assert.equal(/<details[^>]* open=""/.test(html), expanded);
    assert.equal(copy.benefits.length, 4);
    for (const item of copy.benefits) { assert.ok(html.includes(item.title)); assert.ok(html.includes(item.description)); }
    assert.ok(!html.includes('<button'));
  }
});
