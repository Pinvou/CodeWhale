import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

test('explicit evaluation models bypass only the interactive model-selection guard', () => {
  const engine = readFileSync(new URL('../src-tauri/src/features/assistant/engine_pool.rs', import.meta.url), 'utf8');
  const prepare = engine.slice(engine.indexOf('    async fn prepare_runtime_model('), engine.indexOf('    async fn fresh_bridge_for_policy('));
  assert.match(prepare, /resolve_runtime_model_override\(explicit_model_override, \|\| \{[\s\S]*bail!\("model_selection_required:[\s\S]*resolve_spawn_model\(/u);
  assert.ok(prepare.indexOf('resolve_runtime_model_override(') < prepare.indexOf('model_selection_required:'), 'the missing-default check belongs to normal resolution, not pinned evaluation');
  assert.match(prepare, /runtime_model_provider[\s\S]*\.prepare\(RuntimeModelRequest/u, 'explicit snapshots still pass through runtime credential preparation');
});

test('evaluation turns rotate account-platform transport identity before model dispatch', () => {
  const engine = readFileSync(new URL('../src-tauri/src/features/assistant/engine_pool.rs', import.meta.url), 'utf8');
  const start = engine.indexOf('    async fn send_reserved_eval_user_message(');
  const end = engine.indexOf('    pub(crate) async fn send_reserved_user_message(', start);
  const evaluation = engine.slice(start, end);
  assert.match(evaluation, /reservation\.ensure_active\(\)\?[\s\S]*get_or_spawn\(session_id\)\.await\?[\s\S]*begin_logical_turn\(session_id\)[\s\S]*send_reserved_eval_message\(/u);
  assert.equal(evaluation.match(/begin_logical_turn\(/gu)?.length, 1, 'one identity boundary per admitted evaluation turn');
});

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

function harness(surface, selection) {
  const relative = surface === 'tauri' ? 'tauri/bridge/sessions.js' : 'web/bridge.js';
  const source = readFileSync(new URL(`../src/platform/${relative}`, import.meta.url), 'utf8');
  const start = source.indexOf('var ensureSessionInFlight = null;');
  const end = source.indexOf('  function reportSessionSwitchFailure', start);
  assert.ok(start >= 0 && end > start);
  const model = deferred();
  const enteredModel = deferred();
  const calls = [];
  const state = {
    activeSessionId: null, pendingDraftModelRef: selection,
    composerDraft: 'draft', modeDefaults: {}, pendingDraftMultiAgent: false,
  };
  const sessionStates = {};
  const context = vm.createContext({
    state, sessionStates, sessionSwitchRequestToken: 0, IS_WEB: true,
    invoke: async (command, args) => {
      calls.push({ command, args });
      if (command.endsWith('create_session')) return { id: 'created' };
      if (command === 'account_platform_select_session_model' || command === 'set_session_model') {
        enteredModel.resolve();
        return model.promise;
      }
      return null;
    },
    freshBuffer: () => ({}),
    getBuffer: id => sessionStates[id],
    switchActiveTo: id => { state.activeSessionId = id; sessionStates[id] = {}; },
    enterDraft: () => { context.sessionSwitchRequestToken += 1; state.activeSessionId = null; },
    refreshHistoryList: async () => {}, syncModeState: async () => {},
    syncActivePersona: async () => {}, syncMountedCollection: async () => {},
    notify: () => {}, addSystemItem: () => {}, bt: key => key,
  });
  vm.runInContext(source.slice(start, end), context);
  return { context, state, model, enteredModel, calls };
}

for (const surface of ['tauri', 'web']) {
  for (const selection of [
    { source: 'ACCOUNT_PLATFORM', model_id: 'catalog-model' },
    { source: 'LOCAL_CONFIG', saved_model_id: 'local-model' },
  ]) {
    test(`${surface}: ${selection.source} draft selection binds only the created session`, async () => {
      const h = harness(surface, selection);
      const result = h.context.ensureSession();
      const concurrent = h.context.ensureSession();
      await h.enteredModel.promise;
      h.model.resolve();
      assert.equal(await result, 'created');
      assert.equal(await concurrent, 'created');
      assert.equal(h.calls.filter(call => call.command.endsWith('create_session')).length, 1);
      assert.deepEqual(h.state.currentSessionModelRef, selection);
      const binding = h.calls.find(call => call.command === 'account_platform_select_session_model' || call.command === 'set_session_model');
      assert.equal(binding.args.input?.sessionId || binding.args.sessionId, 'created');
    });
  }

  for (const fail of [false, true]) {
    for (const newDraft of [false, true]) {
      test(`${surface}: ${fail ? 'failed' : 'completed'} model selection cannot overwrite ${newDraft ? 'a new draft' : 'another session'}`, async () => {
        const h = harness(surface, { source: 'ACCOUNT_PLATFORM', model_id: 'first-model' });
        const result = h.context.ensureSession();
        await h.enteredModel.promise;
        h.context.sessionSwitchRequestToken += 1;
        h.state.activeSessionId = newDraft ? null : 'other';
        h.state.currentSessionModelRef = { source: 'LOCAL_CONFIG', saved_model_id: 'other-model' };
        h.state.pendingDraftModelRef = { source: 'ACCOUNT_PLATFORM', model_id: 'new-draft-model' };
        if (fail) h.model.reject(new Error('selection failed'));
        else h.model.resolve();
        assert.equal(await result, null);
        assert.equal(h.state.activeSessionId, newDraft ? null : 'other');
        assert.equal(h.state.currentSessionModelRef.saved_model_id, 'other-model');
        assert.equal(h.state.pendingDraftModelRef.model_id, 'new-draft-model');
        const deletions = h.calls.filter(call => call.command === 'delete_session');
        assert.equal(deletions.length, fail ? 1 : 0);
        if (fail) assert.equal(deletions[0].args.id, 'created');
      });
    }
  }
}
