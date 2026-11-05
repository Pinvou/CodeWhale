import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';

const main = fs.readFileSync(new URL('../src/app/main.jsx', import.meta.url), 'utf8');
const dialog = fs.readFileSync(new URL('../src/features/projects/RebindFolderDialog.jsx', import.meta.url), 'utf8');
const action = fs.readFileSync(new URL('../src/features/projects/rebind-workspace-action.js', import.meta.url), 'utf8');
const expectedPorts = ['bridge', 'rebindDraft', 'projectOpsBusy', 'setProjectOpsBusy', 'setRebindDraft', 'setSettingsToast', 't', 'refreshCodexSessions'];

function assertActionBinding(root, view, implementation) {
  assert.doesNotMatch(root, /from ['"][^'"]*(?:rebindErrors|rebind-workspace-action)\.js['"]/u);
  assert.match(view, /import \{ runRebindWorkspace \} from '\.\/rebind-workspace-action\.js';/u);
  const wrapper = root.match(/const confirmRebindWorkspace = \(confirmExisting, runRebindWorkspace\) =>\s*runRebindWorkspace\(confirmExisting, \{([^}]+)\}\);/u);
  assert.ok(wrapper, 'the existing loaded dialog supplies the action synchronously');
  const ports = wrapper[1].split(',').map(value => value.trim()).filter(Boolean);
  assert.deepEqual(ports, expectedPorts, 'every port is the same captured render binding');
  assert.equal((view.match(/onConfirm\(/gu) || []).length, 1);
  assert.match(view, /onClick=\{\(\) => onConfirm\(!!warnExisting, runRebindWorkspace\)\}/u);
  assert.match(implementation, /warnExisting: classified\.passedFence \? false : prev\.warnExisting/u);
  assert.match(implementation, /\{ \.\.\.prev, warnExisting: true, error: null \}/u);
  assert.equal((implementation.match(/carryoverPartial \? \{ partial: carryoverPartial\(prev\) \} : \{\}/gu) || []).length, 3);
  assert.match(implementation, /if \(failed > 0 \|\| postBusy > 0\) \{/u);
}

test('the existing lazy dialog supplies the exact action and captured render ports', () => {
  assertActionBinding(main, dialog, action);
});

test('eager imports, altered ports, awaits and confirmation mutations stay red', () => {
  for (const [root, view] of [
    [main + "\nimport { runRebindWorkspace } from '../features/projects/rebind-workspace-action.js';", dialog],
    [main.replace('const confirmRebindWorkspace = (confirmExisting, runRebindWorkspace)', 'const confirmRebindWorkspace = async (confirmExisting, runRebindWorkspace)'), dialog],
    [main.replace('bridge, rebindDraft, projectOpsBusy, setProjectOpsBusy, setRebindDraft, setSettingsToast, t, refreshCodexSessions,', 'bridge, rebindDraft, false, setProjectOpsBusy, setRebindDraft, setSettingsToast, t, refreshCodexSessions,'), dialog],
    [main, dialog.replace('onConfirm(!!warnExisting, runRebindWorkspace)', 'onConfirm(!warnExisting, runRebindWorkspace)')],
    [main, dialog.replace('onConfirm(!!warnExisting, runRebindWorkspace)', 'onConfirm(true, runRebindWorkspace)')],
    [main, dialog.replace('onConfirm(!!warnExisting, runRebindWorkspace)', 'onConfirm(!!warnExisting)')],
  ]) {
    assert.throws(() => assertActionBinding(root, view, action));
  }
});

test('moving the source cannot hide missing fence, escalation, union or stay-open guards', () => {
  const union = '...(carryoverPartial ? { partial: carryoverPartial(prev) } : {}),';
  const locations = [...action.matchAll(/\.\.\.\(carryoverPartial \? \{ partial: carryoverPartial\(prev\) \} : \{\}\),/gu)];
  assert.equal(locations.length, 3);
  const lostUnions = locations.map(match => action.slice(0, match.index) + action.slice(match.index + union.length));
  for (const implementation of [
    action.replace('warnExisting: classified.passedFence ? false : prev.warnExisting', 'warnExisting: false'),
    action.replace('{ ...prev, warnExisting: true, error: null }', '{ ...prev, error: null }'),
    action.replace('if (failed > 0 || postBusy > 0) {', 'if (failed > 0) {'),
    ...lostUnions,
  ]) {
    assert.throws(() => assertActionBinding(main, dialog, implementation));
  }
});

async function actionHarness(executor, outcomes, initial = {}) {
  const { dictZh } = await import('../src/shared/i18n/zh.js');
  const state = { draft: { from: '/old', to: '/new', warnExisting: false, ...initial }, busy: false, calls: [], trace: [] };
  const fixed = {
    t: dictZh,
    bridge: { projects: { async rebindWorkspaceRoot(...args) {
      state.trace.push('backend');
      state.calls.push(args);
      const next = outcomes.shift();
      if (typeof next === 'string') throw next;
      return next;
    } } },
    setProjectOpsBusy(value) { state.busy = value; state.trace.push(`busy:${value}`); },
    setRebindDraft(value) { state.draft = typeof value === 'function' ? value(state.draft) : value; state.trace.push('draft'); },
    setSettingsToast(value) { state.toast = value; state.trace.push('toast'); },
    async refreshCodexSessions() { state.trace.push('refresh'); },
  };
  return {
    state,
    execute(confirmExisting) {
      return executor(confirmExisting, { ...fixed, rebindDraft: state.draft, projectOpsBusy: state.busy });
    },
  };
}

async function assertThreeAttemptCarryover(executor) {
  const harness = await actionHarness(executor, [
    { rebound_session_ids: [], failed_session_ids: [], post_busy_session_ids: ['x'] },
    'REBIND_ROOTS_PERSIST: no write\nrebound-session-ids:y',
    { rebound_session_ids: [], failed_session_ids: [], post_busy_session_ids: ['x', 'y'] },
  ]);
  const first = harness.execute(false);
  assert.deepEqual(harness.state.trace, ['busy:true', 'draft', 'backend'], 'busy and inline-reset precede IPC without another await');
  await first;
  assert.ok(harness.state.draft, 'post-busy-only completion keeps the retry draft');
  assert.deepEqual(harness.state.draft.partial.postBusyIds, ['x']);
  await harness.execute(true);
  assert.deepEqual(harness.state.draft.partial.postBusyIds, ['x', 'y']);
  assert.equal(harness.state.draft.partial.carryover, true);
  await harness.execute(false);
  assert.deepEqual(harness.state.calls.map(args => args[3]), [[], ['x'], ['x', 'y']]);
  assert.deepEqual(harness.state.calls.map(args => args[2]), [false, true, false]);
  assert.equal(harness.state.busy, false);
  assert.ok(harness.state.draft, 'post-busy-only completion keeps the only retry entry');
  assert.equal(harness.state.trace.filter(item => item === 'refresh').length, 2);
}

test('the real deferred action preserves ordered three-attempt carryover and strong-confirm forwarding', async () => {
  const { runRebindWorkspace } = await import('../src/features/projects/rebind-workspace-action.js');
  await assertThreeAttemptCarryover(runRebindWorkspace);
});

test('the real deferred action keeps the pre/post-fence strong-confirm distinctions', async () => {
  const { runRebindWorkspace } = await import('../src/features/projects/rebind-workspace-action.js');
  const harness = await actionHarness(runRebindWorkspace, [
    'REBIND_OLD_ROOT_EXISTS: confirm', 'REBIND_TO_ROOT: refused', 'REBIND_ROOTS_PERSIST: failed',
  ]);
  await harness.execute(false);
  assert.equal(harness.state.draft.warnExisting, true);
  await harness.execute(true);
  assert.equal(harness.state.draft.warnExisting, true);
  await harness.execute(true);
  assert.equal(harness.state.draft.warnExisting, false);
  assert.equal(harness.state.busy, false);
});

test('actual action mutations cannot discard prior carryover or close a post-busy-only report', async () => {
  const source = action.replace("'./rebindErrors.js'", JSON.stringify(new URL('../src/features/projects/rebindErrors.js', import.meta.url).href));
  const changedCarryover = source.replace(/mergeRebindCarryoverIds\(\s*prev\.partial && prev\.partial\.postBusyIds,\s*classified\.reboundIds,\s*\)/u, 'classified.reboundIds');
  const changedGate = source.replace('if (failed > 0 || postBusy > 0) {', 'if (failed > 0) {');
  for (const mutated of [changedCarryover, changedGate]) {
    assert.notEqual(mutated, source, 'the negative control must change the actual action');
    const { runRebindWorkspace } = await import(`data:text/javascript,${encodeURIComponent(mutated)}`);
    await assert.rejects(assertThreeAttemptCarryover(runRebindWorkspace), assert.AssertionError);
  }
});
