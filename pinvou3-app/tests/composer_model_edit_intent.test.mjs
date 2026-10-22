import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

const readSource = (relative) => fs.readFileSync(new URL(`../src/${relative}`, import.meta.url), 'utf8');
const sharedSource = readSource('shared/bridge-shared-helpers.js');
const settingsSource = readSource('platform/tauri/bridge/settings.js');
const composerSource = readSource('features/settings/composer-shared.jsx');

// Execute the actual non-JSX handler with its React closure inputs. Keep the
// next JSX boundary explicit so source movement fails instead of testing a copy.
const handlerStart = composerSource.indexOf('      function setReasoningEffortForCurrent(tier) {');
const handlerEnd = composerSource.indexOf('\n      if (!savedModels.length) return null;', handlerStart);
assert.ok(handlerStart >= 0 && handlerEnd > handlerStart, 'the real composer handler boundaries must exist');
const handlerSource = composerSource.slice(handlerStart, handlerEnd);

function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}

function makeRow() {
  return {
    id: 'linked-model', name: 'Linked model', alias: 'Original alias',
    preset: 'openai_compatible', provider_kind: 'custom',
    model: 'old-wire', base_url: 'http://127.0.0.1:8000/v1',
    model_store_card_id: 'fixture-card', model_store_link_detached: false,
    context_window_tokens: 32768, max_output_tokens: 4096,
    reasoning_effort: 'low', credential_state: 'configured', has_secret: true,
    credential_action: 'keep_existing',
  };
}

function createSettingsBridge({ pauseTelemetry = false } = {}) {
  let latestRow = makeRow();
  const state = { settings: {}, activeSessionId: 'fixture-session' };
  const listeners = new Map();
  const calls = [];
  const telemetryStarted = deferred();
  const telemetryRelease = deferred();
  const windowObject = {};
  const context = vm.createContext({ window: windowObject, console });
  vm.runInContext(sharedSource, context, { filename: 'shared/bridge-shared-helpers.js' });
  vm.runInContext(settingsSource, context, { filename: 'platform/tauri/bridge/settings.js' });
  const settings = windowObject.__PINVOU_TAURI_BRIDGE_FEATURES__.settings({
    state,
    notify() {},
    listen(name, listener) { listeners.set(name, listener); },
    async invoke(command, args) {
      calls.push({ command, args });
      if (command === 'track_behavior_event') {
        if (pauseTelemetry && args.request.eventName === 'provider_setup_started') {
          telemetryStarted.resolve();
          await telemetryRelease.promise;
        }
        return null;
      }
      if (command === 'save_model') return null;
      if (command === 'list_models') return { models: [latestRow], active_model_id: latestRow.id };
      if (command === 'get_settings') return { advanced: {} };
      if (command === 'get_effective_model_config') return latestRow;
      throw new Error(`Unexpected fixture command: ${command}`);
    },
  });
  return {
    state, calls, listeners, telemetryStarted, telemetryRelease,
    bridge: { available: true, models: settings },
    setLatestRow(row) { latestRow = row; },
  };
}

function createComposerHandler(source, { current, bridge, onClose, onError }) {
  const bind = vm.runInNewContext(`(function (current, reasoningEffortValue, bridge, setOpen, setEffortSaveError, t) {
    ${source}
    return setReasoningEffortForCurrent;
  })`, {}, { filename: 'composer-shared.jsx:setReasoningEffortForCurrent' });
  return bind(current, current.reasoning_effort, bridge,
    (open) => { if (!open) onClose(); }, onError, { saveModelFailed: 'fixture save failed' });
}

async function assertComposerIntentAcrossRefresh(source) {
  const harness = createSettingsBridge({ pauseTelemetry: true });
  await harness.bridge.models.loadModels();
  const original = harness.state.savedModels[0];
  const closed = deferred();
  const errors = [];
  const handler = createComposerHandler(source, {
    current: original,
    bridge: harness.bridge,
    onClose: closed.resolve,
    onError: (error) => errors.push(error),
  });
  handler('high');
  await harness.telemetryStarted.promise;
  assert.equal(harness.calls.filter(({ command }) => command === 'save_model').length, 0,
    'the real settings bridge must still be waiting for provider telemetry');

  // A store context repair and another editor can publish while telemetry is
  // awaited. Only emulate transport responses; Rust owns the eventual merge.
  harness.setLatestRow({
    ...original, alias: 'Latest alias', model: 'new-wire',
    base_url: 'http://127.0.0.1:8001/v1', context_window_tokens: 262144,
    model_store_card_id: null, model_store_link_detached: true,
  });
  assert.ok(harness.listeners.has('models:changed'));
  await harness.listeners.get('models:changed')({ payload: { model_ids: [original.id] } });
  assert.equal(harness.state.savedModels[0].model, 'new-wire');
  assert.equal(harness.state.savedModels[0].context_window_tokens, 262144);
  assert.equal(harness.state.savedModels[0].model_store_link_detached, true);
  assert.equal(harness.state.effectiveModelConfig.model, 'new-wire');

  harness.telemetryRelease.resolve();
  await closed.promise;
  assert.deepEqual(errors.filter(Boolean), [], 'a successful composer save must not show a failure');
  const saves = harness.calls.filter(({ command }) => command === 'save_model');
  assert.equal(saves.length, 1);
  const submitted = saves[0].args.model;
  assert.equal(submitted.id, original.id);
  assert.equal(submitted.reasoning_effort, 'high');
  assert.equal(submitted.reasoning_effort_only, true,
    'composer must send reasoning-only edit intent across an asynchronous refresh');
  assert.notEqual(submitted.context_window_edit_requested, true,
    'a reasoning-only composer save must not claim an explicit context choice');
  // The stale snapshot intentionally still travels with this incoming-only
  // intent. The backend must project only reasoning_effort onto its latest row.
  assert.equal(submitted.model, 'old-wire');
  assert.equal(submitted.context_window_tokens, 32768);
  assert.equal(submitted.model_store_card_id, 'fixture-card');
  assert.equal(original.reasoning_effort, 'low', 'the handler must not mutate its published snapshot');
  assert.equal(original.reasoning_effort_only, undefined);
}

test('the real composer handler retains reasoning-only intent through telemetry and models:changed', async () => {
  await assertComposerIntentAcrossRefresh(handlerSource);
});

test('the old whole-row composer implementation fails the same asynchronous regression', async () => {
  const legacyHandler = handlerSource.replace(/,\s*reasoning_effort_only:\s*true\b/u, '');
  assert.notEqual(legacyHandler, handlerSource, 'the negative control must remove the new edit intent');
  await assert.rejects(() => assertComposerIntentAcrossRefresh(legacyHandler), {
    name: 'AssertionError',
    message: /composer must send reasoning-only edit intent/u,
  });
});

for (const contextWindow of [32768, null]) {
  test(`ordinary Settings saves preserve explicit context ${contextWindow} without acquiring composer intent`, async () => {
    const harness = createSettingsBridge();
    await harness.bridge.models.loadModels();
    const model = {
      ...makeRow(), alias: 'Explicit settings edit', reasoning_effort: 'high',
      context_window_tokens: contextWindow,
      context_window_edit_requested: true,
      preserve_context_window_tokens: false, preserve_model_store_route: false,
    };
    const before = structuredClone(model);
    await harness.bridge.models.saveModel(model);
    const saves = harness.calls.filter(({ command }) => command === 'save_model');
    assert.equal(saves.length, 1);
    assert.deepEqual(saves[0].args.model, before, 'the bridge must forward the complete Settings edit unchanged');
    assert.equal(saves[0].args.model.reasoning_effort_only, undefined);
    assert.deepEqual(model, before, 'the bridge must not rewrite the caller’s explicit edit');
  });
}
