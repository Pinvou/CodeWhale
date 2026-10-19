import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';
import { dict } from './helpers/i18n-all.js';

const panel = readFileSync(new URL('../src/features/remote-control/ControlledDevicePanel.jsx', import.meta.url), 'utf8');
function section(startMarker, endMarker) {
  const start = panel.indexOf(startMarker);
  const end = panel.indexOf(endMarker, start + startMarker.length);
  assert.ok(start >= 0 && end > start, `missing callback: ${startMarker}`);
  return panel.slice(start, end);
}
function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

// Exercise the production callbacks with controlled bridge completion order.
// This tests the JSX wiring, not a copy of the permission state machine.
function harness(remote) {
  const actions = [];
  const loads = [];
  const state = {
    taskPolicy: null, policyPending: false, localError: null,
    policyIntent: { current: 0 }, supported: true, active: false,
    stagedWorkspaces: [], bridge: { available: true, desktopRemoteControl: remote },
    useCallback: callback => (...args) => {
      const pending = callback(...args);
      loads.push(pending);
      return pending;
    },
    useEffect: effect => { state.cleanup = effect(); },
    setTaskPolicy: value => { state.taskPolicy = typeof value === 'function' ? value(state.taskPolicy) : value; },
    setPolicyPending: value => { state.policyPending = value; },
    setLocalError: value => { state.localError = value; },
    setStagedWorkspaces: value => { state.stagedWorkspaces = value; },
    run: (_name, action) => {
      const pending = action(remote, () => true);
      actions.push(pending);
      return pending;
    },
  };
  vm.createContext(state);
  vm.runInContext(section('const readTaskPolicy =', '  useEffect(() => {'), state);
  const load = vm.runInContext('readTaskPolicy', state);
  const effectStart = panel.indexOf('  useEffect(() => {', panel.indexOf('const readTaskPolicy ='));
  const effectEnd = panel.indexOf('  }, [supported, readTaskPolicy]);', effectStart);
  assert.ok(effectStart >= 0 && effectEnd > effectStart);
  vm.runInContext(panel.slice(effectStart, effectEnd) + '  }, [supported, readTaskPolicy]);', state);
  const toggle = vm.runInContext('(' + section("onChange={() => run('toggle'", '\n          />').trimEnd().slice('onChange={'.length, -1) + ')', state);
  const change = vm.runInContext('(' + section('onChange={event => {', '\n                }}>').slice('onChange={'.length) + '\n})', state);
  const retry = vm.runInContext('(' + section("onClick={() => run('task-permissions-read'", '} disabled=').slice('onClick={'.length) + ')', state);
  return {
    state, initialRead: loads[0], load: () => load(remote), toggle, retry,
    change(lane, value) {
      state.lane = lane;
      const count = actions.length;
      change({ target: { value } });
      return actions.length > count ? actions.at(-1) : undefined;
    },
  };
}
const plain = value => JSON.parse(JSON.stringify(value));

test('enable waits for saved policy and ignores a late initial read before editing either lane', async () => {
  for (const lane of ['chat', 'code']) {
    const initial = deferred();
    const savedRead = deferred();
    const writes = [];
    let reads = 0;
    const h = harness({
      controlledStart: async () => ({}),
      controlledPermissions: policy => {
        if (policy) { writes.push(plain(policy)); return Promise.resolve(policy); }
        return reads++ === 0 ? initial.promise : savedRead.promise;
      },
    });
    const enabling = h.toggle();
    await Promise.resolve();
    initial.resolve({ chat: 'plan', code: 'plan' });
    await h.initialRead;
    assert.equal(h.state.taskPolicy, null, 'old read cannot enable editing during startup');
    assert.equal(h.state.policyPending, true);
    assert.equal(h.change(lane, 'plan'), undefined, 'a pending authoritative read cannot be saved');
    savedRead.resolve({ chat: 'plan', code: 'plan' });
    await enabling;
    assert.deepEqual(plain(h.state.taskPolicy), { chat: 'plan', code: 'plan' });
    await h.change(lane, 'plan');
    assert.deepEqual(writes, [{ chat: 'plan', code: 'plan' }]);
  }
});

test('editing an inactive unconfigured lane never grants YOLO to the other lane', async () => {
  // Bridge seam only: the production Rust save and durable reload are covered
  // by saving_one_lane_before_enabling_persists_plan_without_authorizing_the_other.
  for (const first of ['chat', 'code']) {
    const second = first === 'chat' ? 'code' : 'chat';
    const writes = [];
    let stored = { chat: null, code: null };
    const h = harness({ controlledPermissions: async policy => {
      if (policy) {
        if (!policy.chat && !policy.code) throw new Error('task_permission_mode_required');
        stored = plain(policy);
        writes.push(stored);
      }
      return { ...stored };
    } });
    await h.initialRead;
    await h.change(first, 'plan');
    assert.deepEqual(writes, [{ chat: null, code: null, [first]: 'plan' }]);
    assert.equal(h.state.taskPolicy[second], null);
    await h.retry();
    assert.equal(h.state.taskPolicy[first], 'plan');
    await h.change(second, 'plan');
    assert.deepEqual(plain(h.state.taskPolicy), { chat: 'plan', code: 'plan' });
    assert.deepEqual(writes[1], { chat: 'plan', code: 'plan' });
  }
});

test('post-enable read failure blocks editing and refresh retries without writing defaults', async () => {
  let reads = 0;
  const h = harness({
    controlledStart: async () => ({}),
    controlledPermissions: async policy => {
      assert.equal(policy, undefined);
      if (reads++ < 2) throw new Error('read failed');
      return { chat: 'plan', code: 'plan' };
    },
  });
  await h.toggle();
  assert.equal(h.state.taskPolicy, null);
  assert.equal(h.state.policyPending, false);
  assert.equal(h.state.localError, 'remote_task_permissions_save_failed');
  assert.equal(h.change('chat', 'yolo'), undefined);
  await h.retry();
  assert.deepEqual(plain(h.state.taskPolicy), { chat: 'plan', code: 'plan' });
  assert.equal(h.state.policyPending, false);
});

test('pending saves block other channel edits and failed saves require readback', async () => {
  const saving = deferred();
  let stopped = false;
  const h = harness({
    controlledPermissions: policy => policy ? saving.promise : Promise.resolve({ chat: 'plan', code: 'plan' }),
    controlledStart: () => { assert.fail('enable cannot race an in-flight policy save'); },
    controlledStop: async () => { stopped = true; },
  });
  await h.initialRead;
  const pending = h.change('chat', 'yolo');
  assert.equal(h.state.policyPending, true);
  assert.equal(h.change('code', 'yolo'), undefined);
  await h.toggle();
  h.state.active = true;
  await h.toggle();
  assert.equal(stopped, true, 'stopping remains available during a policy save');
  saving.reject(new Error('save failed'));
  await pending;
  assert.equal(h.state.taskPolicy, null);
  assert.equal(h.state.localError, 'remote_task_permissions_save_failed');
  await h.retry();
  assert.deepEqual(plain(h.state.taskPolicy), { chat: 'plan', code: 'plan' });
});

test('enable reads back backend defaults while preserving an already configured lane', async () => {
  for (const previous of [{ chat: 'plan', code: 'plan' }, { chat: null, code: 'plan' }]) {
    const stored = { ...previous };
    const h = harness({
      controlledPermissions: async policy => { assert.equal(policy, undefined); return { ...stored }; },
      controlledStart: async () => { stored.chat ||= 'yolo'; stored.code ||= 'yolo'; },
    });
    await h.initialRead;
    await h.toggle();
    assert.deepEqual(plain(h.state.taskPolicy), stored);
    assert.equal(h.state.taskPolicy.code, 'plan');
  }
});

test('failed enable leaves policy unknown until an explicit successful refresh', async () => {
  const h = harness({
    controlledPermissions: async () => ({ chat: 'plan', code: 'plan' }),
    controlledStart: async () => { throw new Error('enable failed'); },
  });
  await h.initialRead;
  await assert.rejects(h.toggle(), /enable failed/);
  assert.equal(h.state.taskPolicy, null);
  assert.equal(h.state.policyPending, false);
  assert.equal(h.change('chat', 'yolo'), undefined);
  await h.retry();
  assert.deepEqual(plain(h.state.taskPolicy), { chat: 'plan', code: 'plan' });
});

test('unmount invalidates pending policy completion and loading failures never fabricate YOLO', async () => {
  const loading = deferred();
  const h = harness({ controlledPermissions: () => loading.promise });
  h.state.cleanup();
  loading.resolve({ chat: 'yolo', code: 'yolo' });
  await h.initialRead;
  assert.equal(h.state.taskPolicy, null);
  assert.match(panel, /disabled=\{\!\!busy \|\| policyPending \|\| !taskPolicy\}/);
  assert.doesNotMatch(panel, /taskPolicy\?\.(?:chat|code) \|\| 'yolo'/);
  for (const language of ['zh', 'en', 'ja']) {
    for (const key of ['taskPermissionLoading', 'taskPermissionUnavailable']) {
      assert.ok(dict[language].controlledDevice[key], `${language}: ${key}`);
    }
  }
});
