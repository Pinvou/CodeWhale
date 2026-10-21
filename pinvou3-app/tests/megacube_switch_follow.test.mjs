import test from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import { createCmsdSwitchFollow, switchTaskId, cmsdRegistrationId } from '../src/features/megacube/switch-follow.js';
import { mergeHandoffRecovery } from '../src/features/megacube/handoff-recovery.js';

const task = { id: 'switch-1', kind: 'switch', card: 'target', status: 'succeeded' };
function fixture(overrides = {}) {
  const state = { activeSessionId: 'conversation-a', currentSessionModelId: 'source',
    activeModelId: 'global', sessionBusy: {}, ...overrides.state };
  const bindings = new Map([['conversation-a', 'source']]);
  const rows = overrides.rows || [];
  const selected = [], saved = [], notices = [], timers = [];
  let healthy = overrides.healthy ?? true;
  let active = 'target';
  const api = createCmsdSwitchFollow({
    getState: () => state,
    queryModels: async () => ({ available: true, data: { models: [{
      card: 'target', model_id: 'Target', status: 'running', healthy, max_model_len: 65536, port: overrides.port,
    }] } }),
    queryStatus: async () => ({ available: true, data: { active_model: active, gateway: '127.0.0.1:8000' } }),
    listModels: async () => ({ models: rows }),
    sessionModelId: async id => bindings.get(id),
    saveModel: async model => { rows.push(model); saved.push(model); },
    switchModel: async (session, model, options) => {
      if (overrides.busyOnce && !selected.length) {
        overrides.busyOnce = false;
        throw new Error('session_turn_in_progress');
      }
      selected.push({ session, model, options }); bindings.set(session, model);
    },
    listCards: async () => [],
    notify: (...args) => notices.push(args),
    schedule: callback => { timers.push(callback); return callback; },
    cancel: callback => { const index = timers.indexOf(callback); if (index >= 0) timers.splice(index, 1); },
    makeRegistrationId: async (_scope, card) => 'm_cmsd_fixture_' + card,
    ...overrides.api,
  });
  if (overrides.arm !== false) api.accept({ task, sessionId: 'conversation-a', binding: 'source', fallback: 'global', started: Date.now() });
  return { api, state, bindings, rows, selected, saved, notices, timers,
    ready: () => { healthy = true; }, supersede: () => { active = 'another'; },
    async retry() { const callback = timers.shift(); assert.ok(callback); callback();
      await new Promise(resolve => setImmediate(resolve)); },
  };
}

test('existing registration is reused and the conversation binding changes, not the app default', async () => {
  const f = fixture({ rows: [{ id: 'registered', model: 'Target', store_card_id: 'target', base_url: 'http://127.0.0.1:8000/v1' }] });
  await f.api.settled(task);
  assert.equal(f.saved.length, 0);
  assert.deepEqual(f.selected, [{ session: 'conversation-a', model: 'registered', options: { ifIdle: true, expectedModelId: 'source', expectedDefaultModelId: 'global' } }]);
  assert.equal(f.state.activeModelId, 'global');
});
test('unregistered target creates one row with factual context and selects it once', async () => {
  const f = fixture();
  await f.api.settled(task); await f.api.settled(task);
  assert.equal(f.saved.length, 1); assert.equal(f.selected.length, 1);
  assert.equal(f.saved[0].store_card_id, 'target'); assert.equal(f.saved[0].context_window_tokens, 65536);
  assert.equal(f.selected[0].model, f.saved[0].id);
});
test('same model name in a cloud registration or another card is not selected', async () => {
  const f = fixture({ rows: [{ id: 'cloud', model: 'Target', base_url: 'https://cloud.example/v1' },
    { id: 'other', model: 'Target', store_card_id: 'other-card', base_url: 'http://127.0.0.1:8000/v1' }] });
  await f.api.settled(task); assert.equal(f.saved.length, 1);
  assert.notEqual(f.selected[0].model, 'cloud'); assert.notEqual(f.selected[0].model, 'other');
});
test('a delayed health sample is retried without permanently consuming the task', async () => {
  const f = fixture({ healthy: false }); await f.api.settled(task);
  assert.equal(f.selected.length, 0); f.ready(); await f.retry(); assert.equal(f.selected.length, 1);
});
test('the originating busy tool turn is allowed to finish', async () => {
  const f = fixture({ state: { sessionBusy: { 'conversation-a': true } } });
  f.api.observe({ ...task, status: 'running' }); await f.api.settled(task);
  assert.equal(f.saved.length, 0); assert.equal(f.selected.length, 0);
  f.state.sessionBusy['conversation-a'] = false; await f.retry(); assert.equal(f.selected.length, 1);
});
test('backend atomic idle rejection is retried and does not duplicate registration', async () => {
  const f = fixture({ busyOnce: true }); await f.api.settled(task); await f.retry();
  assert.equal(f.saved.length, 1); assert.equal(f.selected.length, 1);
});
test('navigation during a switch does not bind the newly visible conversation', async () => {
  const f = fixture(); f.api.observe({ ...task, status: 'running' });
  f.state.activeSessionId = 'conversation-b'; f.state.currentSessionModelId = 'another';
  await f.api.settled(task); assert.equal(f.selected[0].session, 'conversation-a');
});
test('an explicit manual model choice during readiness retry wins', async () => {
  const f = fixture({ healthy: false }); await f.api.settled(task);
  f.bindings.set('conversation-a', 'manual'); f.ready(); await f.retry();
  assert.equal(f.selected.length, 0); assert.equal(f.saved.length, 0);
});
test('failed switches, ordinary default changes and superseded completions do not change chat', async () => {
  const f = fixture(); await f.api.settled({ ...task, status: 'failed' });
  await f.api.settled({ ...task, kind: 'set_default' }); f.supersede(); await f.api.settled(task);
  assert.equal(f.selected.length, 0); assert.equal(f.saved.length, 0);
});
test('disposal cancels readiness retries', async () => {
  const f = fixture({ healthy: false }); await f.api.settled(task); f.api.dispose();
  assert.equal(f.timers.length, 0); assert.equal(f.selected.length, 0);
});

test('unrelated daemon switch tasks and first-poll history never authorize conversation changes', async () => {
  const f = fixture({ arm: false });
  await f.api.observe(task); await f.api.settled(task);
  assert.equal(f.selected.length, 0); assert.equal(f.saved.length, 0);
});
test('trusted MCP tool receipt binds the task to its originating session before navigation', async () => {
  const f = fixture({ arm: false });
  f.api.toolStarted({ id: 'call-1', session_id: 'conversation-a',
    name: 'mcp__megacube_admin__cube_switch_model', args: { card: 'target' } });
  f.state.activeSessionId = 'conversation-b';
  await f.api.toolEnded({ id: 'call-1', session_id: 'conversation-a', success: true,
    output: JSON.stringify({ content: [{ type: 'text', text: JSON.stringify({ data: { task_id: task.id } }) }] }) });
  await f.api.observe(task);
  assert.equal(f.selected[0].session, 'conversation-a');
});
test('the actual single-underscore CodeWhale tool name is accepted, hallucinated names are ignored', async () => {
  const f = fixture({ arm: false });
  for (const name of ['memp_megacube_admin_cube_switch_model', 'mcp_other_server_cube_switch_model']) {
    f.api.toolStarted({ id: 'ignored', session_id: 'conversation-a', name, args: { card: 'target' } });
    await f.api.toolEnded({ id: 'ignored', session_id: 'conversation-a', success: true,
      output: JSON.stringify({ data: { task_id: task.id } }) });
  }
  await f.api.observe(task); assert.equal(f.selected.length, 0);
  f.api.toolStarted({ id: 'actual', session_id: 'conversation-a', name: 'mcp_megacube_admin_cube_switch_model', args: { card: 'target' } });
  await f.api.toolEnded({ id: 'actual', session_id: 'conversation-a', success: true,
    output: JSON.stringify({ data: { task_id: task.id } }) });
  await f.api.observe(task); assert.equal(f.selected.length, 1);
});
test('a persisted accepted handoff resumes from an already-finished first observation after UI reopen', async () => {
  let pending = [];
  const f = fixture({ api: { savePending: rows => { pending = structuredClone(rows); } } });
  f.api.dispose();
  assert.equal(pending.length, 1);
  const reopened = fixture({ arm: false, api: {
    loadPending: () => structuredClone(pending), savePending: rows => { pending = structuredClone(rows); },
  } });
  await reopened.api.observe(task);
  assert.equal(reopened.selected.length, 1); assert.equal(pending.length, 0);
});
test('failed or mismatched tool receipts cannot authorize handoff, and prose is not a receipt', async () => {
  const f = fixture({ arm: false });
  f.api.toolStarted({ id: 'call-1', session_id: 'conversation-a', name: 'cube_switch_model', args: { card: 'target' } });
  await f.api.toolEnded({ id: 'call-1', session_id: 'conversation-b', success: true,
    output: JSON.stringify({ data: { task_id: task.id } }) });
  await f.api.observe(task); assert.equal(f.selected.length, 0);
  assert.equal(switchTaskId('success, task switch-1'), null);
  assert.equal(switchTaskId({ isError: true, data: { task_id: task.id } }), null);
});

test('desktop bridge keeps legacy switch payloads and carries idle/CAS guards only when requested', async () => {
  const manual = [];
  const root = { CustomEvent: class { constructor(type, options) { this.type = type; this.detail = options.detail; } },
    dispatchEvent: event => manual.push(event.detail) };
  const calls = [];
  vm.runInNewContext(fs.readFileSync(new URL('../src/platform/tauri/bridge/settings.js', import.meta.url), 'utf8'),
    { window: root, console, setTimeout, clearTimeout });
  const feature = root.__PINVOU_TAURI_BRIDGE_FEATURES__.settings({
    state: { activeSessionId: 'session', savedModels: [] }, notify() {}, listen() {},
    async invoke(command, value) { calls.push({ command, value }); return null; },
  });
  await feature.switchModel('session', 'target');
  assert.deepEqual(JSON.parse(JSON.stringify(manual)), [{ sessionId: 'session' }]);
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0])), {
    command: 'set_session_model', value: { sessionId: 'session', modelId: 'target' },
  });
  calls.length = 0;
  await feature.switchModel('session', 'target', { ifIdle: true, expectedModelId: 'source', expectedDefaultModelId: 'global' });
  assert.equal(manual.length, 1);
  assert.deepEqual(JSON.parse(JSON.stringify(calls[0])), { command: 'set_session_model',
    value: { sessionId: 'session', modelId: 'target', ifIdle: true, expectedModelId: 'source', expectedDefaultModelId: 'global' } });
});

test('a generic default alias is not a registered target identity', async () => {
  const f = fixture({ rows: [{ id: 'legacy-alias', model: 'megacube-default', base_url: 'http://127.0.0.1:8000/v1' }] });
  await f.api.settled(task); assert.equal(f.saved.length, 1);
  assert.notEqual(f.selected[0].model, 'legacy-alias');
});

test('post-commit registration response loss reuses the durable row on retry', async () => {
  const rows = [];
  let writes = 0;
  const f = fixture({ rows, api: { saveModel: async model => {
    rows.push(model); writes++; throw new Error('response lost after commit');
  } } });
  await f.api.settled(task); await f.retry();
  assert.equal(writes, 1); assert.equal(f.selected.length, 1);
});

test('stale exact-card registrations retain identity, display and tuning while reconnecting locally', async () => {
  const original = { id: 'old-registration', name: 'My custom target', model: 'Old-wire-name',
    store_card_id: 'target', base_url: 'http://127.0.0.1:19999/v1', reasoning_effort: 'high' };
  const f = fixture({ rows: [original] }); await f.api.settled(task);
  assert.equal(f.saved.length, 1); assert.equal(f.saved[0].id, original.id);
  assert.equal(f.saved[0].name, original.name); assert.equal(f.saved[0].reasoning_effort, 'high');
  assert.equal(f.saved[0].credential_action, 'keep'); assert.equal(f.saved[0].api_key, null);
  assert.equal(f.selected[0].model, original.id);
});

test('recovery keeps the browser receipt and never replays a completed repair seed', () => {
  const repair = { task: { id: 'original' }, started: 1 };
  const browser = { ...repair, started: 2 };
  assert.deepEqual(mergeHandoffRecovery([browser], [repair]), [browser]);
  assert.deepEqual(mergeHandoffRecovery([browser], [repair], new Set(['original'])), []);
});

test('simultaneous registration computes one stable ID per profile, endpoint and card', async () => {
  const [a, b, other] = await Promise.all([
    cmsdRegistrationId('profile-endpoint', 'card'), cmsdRegistrationId('profile-endpoint', 'card'),
    cmsdRegistrationId('other-profile-endpoint', 'card'),
  ]);
  assert.equal(a, b); assert.match(a, /^m_cmsd_[a-f0-9]{32}$/); assert.notEqual(a, other);
});

test('an exact-card legacy alias reconnects using factual wire identity and the gateway', async () => {
  for (const base_url of ['http://127.0.0.1:19999/v1', 'http://127.0.0.1:8000/v1']) {
    const f = fixture({ port: 19999, rows: [{ id: 'legacy', name: 'Retained', model: 'megacube-default',
      store_card_id: 'target', base_url }] });
    await f.api.settled(task);
    assert.equal(f.saved.length, 1); assert.equal(f.saved[0].id, 'legacy');
    assert.equal(f.saved[0].model, 'Target'); assert.equal(f.saved[0].base_url, 'http://127.0.0.1:8000/v1');
    assert.equal(f.selected[0].model, 'legacy');
  }
});

test('a path-model legacy alias is repaired to the concrete ID through the gateway', async () => {
  const f = fixture({ rows: [{ id: 'legacy-path', model: 'megacube-default', store_card_id: 'target',
    base_url: 'http://127.0.0.1:19999/v1' }], api: {
    queryModels: async () => ({ available: true, data: { models: [{ card: 'target',
      model_id: '/models/target', port: 19999, status: 'running', healthy: true }] } }),
  } });
  await f.api.settled(task); assert.equal(f.saved.length, 1);
  assert.equal(f.saved[0].id, 'legacy-path'); assert.equal(f.saved[0].model, '/models/target');
  assert.equal(f.saved[0].base_url, 'http://127.0.0.1:8000/v1');
});

test('a long model load does not spend the idle-wait budget before completion', async () => {
  let clock = Date.now();
  const f = fixture({ state: { sessionBusy: { 'conversation-a': true } }, api: { now: () => clock } });
  clock += 600000;
  await f.api.observe(task); assert.equal(f.notices.length, 0);
  clock += 3000; f.state.sessionBusy['conversation-a'] = false;
  await f.retry(); assert.equal(f.selected.length, 1);
});

test('optional card display-name failure does not block a healthy path-model registration', async () => {
  const f = fixture({ api: {
    queryModels: async () => ({ available: true, data: { models: [{ card: 'target',
      model_id: '/models/target', status: 'running', healthy: true }] } }),
    listCards: async () => { throw new Error('display catalog offline'); },
  } });
  await f.api.observe(task); assert.equal(f.saved.length, 1); assert.equal(f.selected.length, 1);
  assert.equal(f.saved[0].model, '/models/target');
});

test('a device switch during registration supersedes the pending selection', async () => {
  const f = fixture({ api: { saveModel: async () => { f.supersede(); } } });
  await f.api.observe(task); assert.equal(f.selected.length, 0);
});

test('manual A to B to A while a registry write is pending cancels automatic handoff', async () => {
  const f = fixture({ api: { saveModel: async () => {
    f.bindings.set('conversation-a', 'manual-b');
    f.api.manualSelection({ sessionId: 'conversation-a' });
    f.bindings.set('conversation-a', 'source');
  } } });
  await f.api.observe(task); assert.equal(f.selected.length, 0);
});

test('manual selection before the MCP receipt arrives cancels its captured call', async () => {
  const f = fixture({ arm: false });
  f.api.toolStarted({ id: 'call', session_id: 'conversation-a', name: 'cube_switch_model', args: { card: 'target' } });
  f.api.manualSelection({ sessionId: 'conversation-a' });
  await f.api.toolEnded({ id: 'call', session_id: 'conversation-a', success: true,
    output: { data: { task_id: task.id } } });
  await f.api.observe(task); assert.equal(f.selected.length, 0); assert.equal(f.saved.length, 0);
});

async function deferredReceiptFixture() {
  let resolveContext;
  const context = new Promise(resolve => { resolveContext = resolve; });
  let firstRead = true;
  const f = fixture({ arm: false, state: { activeSessionId: 'conversation-b' }, api: {
    sessionModelId: id => {
      if (firstRead) { firstRead = false; return context; }
      return Promise.resolve(f.bindings.get(id));
    },
  } });
  f.api.toolStarted({ id: 'call', session_id: 'conversation-a', name: 'cube_switch_model', args: { card: 'target' } });
  await new Promise(resolve => setImmediate(resolve));
  return { ...f, resolveContext };
}

const successfulReceipt = (taskId = task.id) => ({ id: 'call', session_id: 'conversation-a', success: true,
  output: { data: { task_id: taskId } } });

test('manual choices during receipt context lookup win even when the binding returns to its original value', async () => {
  for (const choices of [['manual-b', 'source'], ['source']]) {
    const f = await deferredReceiptFixture();
    const ending = f.api.toolEnded(successfulReceipt());
    for (const binding of choices) {
      f.bindings.set('conversation-a', binding);
      f.api.manualSelection({ sessionId: 'conversation-a' });
    }
    f.resolveContext('source'); await ending;
    await f.api.observe(task);
    assert.equal(f.selected.length, 0);
    assert.equal(f.saved.length, 0);
    f.api.dispose();
  }
});

test('disposing during receipt context lookup cannot revive a handoff', async () => {
  const f = await deferredReceiptFixture();
  const ending = f.api.toolEnded(successfulReceipt());
  f.api.dispose();
  f.resolveContext('source'); await ending;
  await f.api.observe(task);
  assert.equal(f.selected.length, 0); assert.equal(f.saved.length, 0);
});

test('duplicate tool endings while context is pending accept one original handoff', async () => {
  const f = await deferredReceiptFixture();
  const original = f.api.toolEnded(successfulReceipt());
  const duplicate = f.api.toolEnded(successfulReceipt('duplicate-switch'));
  f.resolveContext('source'); await Promise.all([original, duplicate]);
  await f.api.observe({ ...task, id: 'duplicate-switch' });
  assert.equal(f.selected.length, 0); assert.equal(f.saved.length, 0);
  await f.api.observe(task); await f.api.observe(task);
  assert.equal(f.selected.length, 1); assert.equal(f.saved.length, 1);
  f.api.dispose();
});

test('an awaiting old ending neither accepts nor removes a replacement call with the same id', async () => {
  const f = await deferredReceiptFixture();
  const oldEnding = f.api.toolEnded(successfulReceipt('old-switch'));
  f.api.toolStarted({ id: 'call', session_id: 'conversation-a', name: 'cube_switch_model', args: { card: 'target' } });
  f.resolveContext('source'); await oldEnding;
  await f.api.observe({ ...task, id: 'old-switch' });
  assert.equal(f.selected.length, 0); assert.equal(f.saved.length, 0);
  await f.api.toolEnded(successfulReceipt());
  await f.api.observe(task);
  assert.equal(f.selected.length, 1); assert.equal(f.saved.length, 1);
  f.api.dispose();
});

test('failed or invalid receipt endings consume their call and allow a later new call with that id', async () => {
  for (const receipt of [{ ...successfulReceipt(), success: false }, { ...successfulReceipt(), output: 'invalid receipt' }]) {
    const f = await deferredReceiptFixture();
    const ending = f.api.toolEnded(receipt);
    f.resolveContext('source'); await ending;
    await f.api.toolEnded(successfulReceipt());
    await f.api.observe(task);
    assert.equal(f.selected.length, 0); assert.equal(f.saved.length, 0);
    f.api.toolStarted({ id: 'call', session_id: 'conversation-a', name: 'cube_switch_model', args: { card: 'target' } });
    await f.api.toolEnded(successfulReceipt());
    await f.api.observe(task);
    assert.equal(f.selected.length, 1); assert.equal(f.saved.length, 1);
    f.api.dispose();
  }
});

test('logical MCP switch follows its physical receipt and retains one logical registration', async () => {
  const runtime = 'target--v2';
  const physical = { ...task, card: runtime, requested_card: 'target', selected_version: 'v2' };
  const f = fixture({ arm: false, api: {
    queryModels: async () => ({ available: true, data: { store_bindings: { target: runtime }, models: [{
      card: runtime, managed: true, version_id: 'v2', deployment_revision: 'recipe-v2', model_id: 'Target',
      status: 'running', healthy: true, max_model_len: 262144,
    }] } }),
    queryStatus: async () => ({ available: true, data: { active_model: runtime, gateway: '127.0.0.1:8000' } }),
  } });
  f.api.toolStarted({ id: 'logical', session_id: 'conversation-a', name: 'cube_switch_model', args: { card: 'target' } });
  await f.api.toolEnded({ id: 'logical', session_id: 'conversation-a', success: true,
    output: { data: { task_id: task.id, card: runtime, requested_card: 'target', selected_version: 'v2' } } });
  await f.api.settled(physical);
  assert.equal(f.saved.length, 1); assert.equal(f.selected.length, 1);
  assert.equal(f.saved[0].store_card_id, 'target'); assert.equal(f.saved[0].model_store_card_id, 'target');
  assert.equal(f.saved[0].id, 'm_cmsd_fixture_target'); assert.equal(f.saved[0].context_window_tokens, 262144);
});

test('a new current pointer after task completion never redirects the captured switch', async () => {
  const f = fixture({ arm: false, api: {
    queryModels: async () => ({ available: true, data: { store_bindings: { target: 'target--v3' }, models: [{
      card: 'target--v3', managed: true, version_id: 'v3', model_id: 'Target', status: 'running', healthy: true,
    }] } }),
    queryStatus: async () => ({ available: true, data: { active_model: 'target--v3', gateway: '127.0.0.1:8000' } }),
  } });
  f.api.accept({ task: { ...task, card: 'target--v2', requested_card: 'target', selected_version: 'v2' },
    sessionId: 'conversation-a', binding: 'source', fallback: 'global', started: Date.now() });
  await f.api.settled({ ...task, card: 'target--v2', requested_card: 'target' });
  assert.equal(f.selected.length, 0); assert.equal(f.saved.length, 0);
});

test('malformed current bindings and same-runtime deployment drift cannot authorize continuation', async () => {
  for (const mode of ['missing-target', 'recipe-drift']) {
    let reads = 0;
    const f = fixture({ rows: [{ id: 'registered', model: 'Target', store_card_id: 'target', model_store_card_id: 'target', base_url: 'http://127.0.0.1:8000/v1' }], api: {
      queryModels: async () => ({ available: true, data: { store_bindings: { target: mode === 'missing-target' ? 'missing' : 'target' }, models: [{
        card: 'target', managed: true, model_id: 'Target', status: 'running', healthy: true,
        deployment_revision: ++reads > 1 ? 'changed' : 'original',
      }] } }),
    } });
    await f.api.settled(task);
    assert.equal(f.selected.length, 0, mode); assert.equal(f.saved.length, 0, mode);
  }
});

test('a task receipt for an older recipe cannot follow a healthy replacement with the same physical card', async () => {
  const f = fixture({ arm: false, api: {
    queryModels: async () => ({ available: true, data: { store_bindings: { target: 'target--v2' }, models: [{
      card: 'target--v2', managed: true, version_id: 'v2', deployment_revision: 'new-recipe',
      model_id: 'Target', status: 'running', healthy: true,
    }] } }),
    queryStatus: async () => ({ available: true, data: { active_model: 'target--v2', gateway: '127.0.0.1:8000' } }),
  } });
  f.api.accept({ task: { ...task, card: 'target--v2', requested_card: 'target', selected_version: 'v2', deployment_revision: 'original-recipe' },
    sessionId: 'conversation-a', binding: 'source', fallback: 'global', started: Date.now() });
  await f.api.settled({ ...task, card: 'target--v2', requested_card: 'target', selected_version: 'v2', deployment_revision: 'original-recipe' });
  assert.equal(f.selected.length, 0); assert.equal(f.saved.length, 0);
});


test('reconnecting an existing store registration requests validated linkage and preserves a rejected route', async () => {
  for (const rejected of [false, true]) {
    const runtime = 'target--v2';
    const receipt = { ...task, card: runtime, requested_card: 'target', selected_version: 'v2' };
    const requests = [];
    const f = fixture({ arm: false,
      rows: [{ id: 'registered', model: 'OldTarget', model_store_card_id: 'target', store_card_id: 'target', base_url: 'http://127.0.0.1:8000/v1' }],
      api: {
        queryModels: async () => ({ available: true, data: { store_bindings: { target: runtime }, models: [{
          card: runtime, managed: true, version_id: 'v2', deployment_revision: 'recipe-v2',
          model_id: 'Target', status: 'running', healthy: true,
        }] } }),
        queryStatus: async () => ({ available: true, data: { active_model: runtime, gateway: '127.0.0.1:8000' } }),
        saveModel: async model => {
          requests.push(model);
          if (rejected) throw new Error('model store endpoint changed or is unavailable; refresh the card and retry');
        },
      },
    });
    f.api.accept({ task: receipt, sessionId: 'conversation-a', binding: 'source', fallback: 'global', started: Date.now() });
    await f.api.settled(receipt);
    assert.equal(requests.length, 1);
    assert.equal(requests[0].model_store_link_requested, true);
    assert.equal(requests[0].credential_action, 'keep');
    assert.equal(f.selected.length, rejected ? 0 : 1);
    if (rejected) {
      assert.equal(f.rows[0].model, 'OldTarget');
      assert.equal(f.bindings.get('conversation-a'), 'source');
    }
  }
});


test('specific cards retain exact normal and path IDs for new and reused registrations', async () => {
  for (const modelId of ['Target', '/models/target']) {
    for (const fromStore of [false, true]) {
      for (const existing of [false, true]) {
        const runtime = fromStore ? 'target--v2' : 'target';
        const receipt = { ...task, card: runtime, requested_card: 'target', selected_version: 'v2' };
        const old = { id: 'registered', name: 'Retained user name', model: 'megacube-default',
          store_card_id: 'target', ...(fromStore ? { model_store_card_id: 'target' } : {}),
          base_url: 'http://127.0.0.1:19999/v1', context_window_tokens: 4096, max_output_tokens: 1024 };
        const f = fixture({ arm: false, rows: existing ? [old] : [], api: {
          queryModels: async () => ({ available: true, data: {
            ...(fromStore ? { store_bindings: { target: runtime } } : {}),
            models: [{ card: runtime, managed: true, model_id: modelId, version_id: 'v2',
              status: 'running', healthy: true, max_model_len: 65536, port: 19999 }],
          } }),
          queryStatus: async () => ({ available: true, data: { active_model: runtime, gateway: '127.0.0.1:8000' } }),
          listCards: async () => [{ id: 'target', name: 'Catalog display name' }],
        } });
        f.api.accept({ task: receipt, sessionId: 'conversation-a', binding: 'source', fallback: 'global', started: Date.now() });
        await f.api.settled(receipt);
        assert.equal(f.saved.length, 1); assert.equal(f.selected.length, 1);
        const saved = f.saved[0];
        assert.equal(saved.model, modelId);
        assert.equal(saved.store_card_id, 'target');
        assert.equal(saved.model_store_card_id, fromStore ? 'target' : null);
        assert.equal(saved.model_store_link_requested, fromStore);
        if (existing) {
          assert.equal(saved.id, old.id); assert.equal(saved.name, old.name);
          assert.equal(saved.context_window_tokens, 4096); assert.equal(saved.max_output_tokens, 1024);
          assert.equal(saved.credential_action, 'keep'); assert.equal(saved.api_key, null);
        } else if (modelId.startsWith('/')) {
          assert.equal(saved.name, 'Catalog display name');
        }
        // A later default change is irrelevant to the persisted registration's
        // concrete wire identity; replay cannot turn it into a subscription.
        f.supersede(); await f.api.observe(receipt);
        assert.equal(f.saved.length, 1); assert.equal(f.selected.length, 1);
        assert.equal(saved.model, modelId);
        f.api.dispose();
      }
    }
  }
});

test('missing or unknown card model identity never saves or selects a default alias', async () => {
  for (const modelId of [undefined, null, '', '  ', {}, 123]) {
    const f = fixture({ api: {
      queryModels: async () => ({ available: true, data: { models: [{ card: 'target',
        model_id: modelId, status: 'running', healthy: true }] } }),
    } });
    await f.api.settled(task);
    assert.equal(f.saved.length, 0); assert.equal(f.selected.length, 0);
    assert.equal(f.bindings.get('conversation-a'), 'source');
    f.api.dispose();
  }
});
