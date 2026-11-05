import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import * as acorn from 'acorn';

const read = relative => readFileSync(new URL(`../src/${relative}`, import.meta.url), 'utf8');
function runtime() {
  const window = {};
  const context = vm.createContext({ window, console });
  for (const file of ['shared/model-service-errors.js', 'shared/bridge-messages.js', 'shared/bridge-shared-helpers.js', 'platform/web/bridge/turn-terminal.js']) {
    vm.runInContext(read(file), context, { filename: file });
  }
  return { window, context };
}
function productionFunction(source, name) {
  let found;
  function walk(node) {
    if (!node || typeof node !== 'object') return;
    if (node.type === 'FunctionDeclaration' && node.id.name === name) found = node;
    for (const value of Object.values(node)) {
      if (Array.isArray(value)) value.forEach(walk);
      else if (value && typeof value === 'object') walk(value);
    }
  }
  walk(acorn.parse(source, { ecmaVersion: 'latest' }));
  assert.ok(found, `actual production function ${name} must exist`);
  return source.slice(found.start, found.end);
}

test('shared registry exposes Account model identity through the actual Web domain adapter', () => {
  const rt = runtime();
  const state = {
    activeModelId: 'carrier', currentSessionModelId: 'carrier',
    currentSessionModelRef: { source: 'ACCOUNT_PLATFORM', model_id: 'current' },
    pendingDraftModelRef: { source: 'ACCOUNT_PLATFORM', model_id: 'draft' },
    defaultModelRef: { source: 'LOCAL_CONFIG', saved_model_id: 'local-default' },
    effectiveModelConfig: { model: 'current' }, savedModels: [{ id: 'carrier' }], modelsLoaded: true,
  };
  rt.window.PinvouPlatform = { kind: 'web' };
  rt.window.TauriBridge = { available: true, getState: () => state };
  vm.runInContext(read('platform/web/bridge/domain-adapter.js'), rt.context);
  assert.deepEqual(JSON.parse(JSON.stringify(rt.window.TauriBridge.state.get('models'))), state);
  assert.equal(rt.window.PinvouBridgeShared.STATE_SLICE_FIELDS.llmapi, undefined, 'retired authority must not return');
});

test('shared catalog reader retains typed defaults, last success, and newest request ownership', async () => {
  const rt = runtime();
  const state = { savedModels: [{ id: 'prior' }], activeModelId: 'prior', modelsLoaded: true };
  let resolveOld;
  let request = 0;
  const catalog = rt.window.PinvouBridgeShared.create('tauriSettings', {
    state, modelsLoadSeq: { value: 0 }, notify() {},
    invoke() {
      request += 1;
      if (request === 1) return new Promise(resolve => { resolveOld = resolve; });
      if (request === 2) return Promise.resolve({ models: [{ id: 'new' }], active_model_id: 'new', default_model_ref: { source: 'ACCOUNT_PLATFORM', model_id: 'new' } });
      return Promise.reject(new Error('controlled transport failure'));
    },
  });
  const old = catalog.loadModels();
  assert.equal(await catalog.loadModels(), true);
  resolveOld({ models: [{ id: 'stale' }], default_model_ref: { source: 'ACCOUNT_PLATFORM', model_id: 'stale' } });
  assert.equal(await old, false);
  assert.equal(state.savedModels[0].id, 'new');
  assert.equal(state.defaultModelRef.model_id, 'new');
  assert.equal(await catalog.loadModels(), false);
  assert.equal(state.savedModels[0].id, 'new');
  assert.equal(state.modelsLoaded, true);
});

test('both production terminal writers keep model errors and scoped billing facts in the same turn', async t => {
  for (const lane of ['desktop', 'web']) {
    await t.test(lane, () => {
      const rt = runtime();
      const state = { activeSessionId: 'session', currentSessionModelRef: { source: 'ACCOUNT_PLATFORM', model_id: 'current' }, settings: { language: 'en' } };
      let record;
      if (lane === 'desktop') {
        rt.context.state = state;
        rt.context.latestOpenTimelineStart = () => state.turnTimeline[0];
        rt.context.bridgeMessages = () => rt.window.PinvouBridgeMessages;
        vm.runInContext(productionFunction(read('platform/tauri/bridge/chat-events.js'), 'recordTurnCompleted'), rt.context);
        record = payload => rt.context.recordTurnCompleted(payload);
      } else {
        record = payload => rt.window.PinvouWebTurnTerminal.recordCompleted(state, state.turnTimeline[0], payload);
      }
      for (const scope of ['PERSONAL', 'ORGANIZATION']) {
        state.activeTurnTimelineId = `turn-${scope}`;
        state.turnTimeline = [{ event: 'user_start', turn_id: state.activeTurnTimelineId, ui_turn_index: 7 }];
        const notice = { schema_version: 1, code: scope === 'PERSONAL' ? 'WALLET_INSUFFICIENT' : 'MEMBER_QUOTA_INSUFFICIENT', context_type: scope, organization_id: scope === 'ORGANIZATION' ? '0199a100-0001-7000-8000-000000000201' : undefined };
        const payload = { error: 'model service unavailable: HTTP 503', status: 'Failed', billing_notice: notice };
        const result = record(payload);
        assert.equal(result.turn_id, `turn-${scope}`);
        assert.equal(result.ui_turn_index, 7);
        assert.equal(result.billing_notice, notice);
        assert.equal(result.user_error, payload.user_error);
        assert.ok(result.user_error?.title.includes('Built-in model'), 'actual managed-model error conversion must survive delegation');
        assert.equal(payload.userError, result.user_error);
        assert.equal(state.activeTurnTimelineId, null);
      }
    });
  }
});

test('retained compact API uses the production shared implementation on both lanes', async () => {
  const rt = runtime();
  for (const lane of ['tauriInteraction', 'web']) {
    const calls = [];
    const state = { activeSessionId: 'selected' };
    const api = rt.window.PinvouBridgeShared.create(lane, {
      state, invoke: async (command, args) => { calls.push([command, args]); },
    });
    await api.compactNow();
    assert.equal(calls[0][0], 'compact_now');
    assert.equal(calls[0][1].sessionId, 'selected');
    state.activeSessionId = null;
    await api.compactNow();
    assert.equal(calls.length, 1);
  }
});
