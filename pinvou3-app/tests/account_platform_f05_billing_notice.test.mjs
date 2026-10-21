import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { timelineUserError,pairDeepSeekTimeline } from '../src/features/conversation/deepseek-conversation.js';
import { canOpenPersonalRecharge,walletRechargeTarget } from '../src/features/conversation/billing-notice.js';
const notice={schema_version:1,code:'WALLET_INSUFFICIENT',stage:'ADDITIONAL_RESERVATION',turn_outcome:'COMPLETED',settlement_status:'SETTLED',action:'OPEN_RECHARGE',context_type:'PERSONAL'};
test('persisted completed personal billing notice keeps successful outcome and actionable recharge facts',()=>{ const events=[{event:'user_start',turn_id:'public-turn',timestamp:1,ui_turn_index:0},{event:'assistant_done',turn_id:'public-turn',timestamp:2,status:'Completed',billing_notice:notice}];const rows=pairDeepSeekTimeline(events);assert.equal(rows[0].status,'Completed');assert.equal(rows[0].userError.severity,'warning');assert.equal(rows[0].userError.settlementStatus,'SETTLED');assert.deepEqual(walletRechargeTarget(rows[0].userError),{contextType:'PERSONAL',view:'recharge'}); });
test('non personal or unrelated notice cannot dispatch personal recharge',()=>{for(const input of [{...notice,context_type:'ORGANIZATION'},{...notice,action:'CONTACT_ORG_ADMIN'},{...notice,code:'UNRELATED'}]) assert.equal(canOpenPersonalRecharge(timelineUserError({billing_notice:input})),false);});
test('organization notices retain exact scope and never authorize personal recharge', () => {
  const organization_id = '0199a100-0001-7000-8000-000000000201';
  for (const code of ['WALLET_INSUFFICIENT', 'MEMBER_QUOTA_INSUFFICIENT']) {
    const input = {...notice, code, context_type:'ORGANIZATION', organization_id, action:code === 'MEMBER_QUOTA_INSUFFICIENT' ? 'CONTACT_ORG_ADMIN' : 'OPEN_ORGANIZATION_RECHARGE'};
    const result = timelineUserError({billing_notice:input});
    assert.equal(result.errorCode, code);
    assert.equal(result.organizationId, organization_id);
    assert.equal(result.severity, 'warning');
    assert.equal(walletRechargeTarget(result), null);
    assert.equal(timelineUserError({billing_notice:{...input, organization_id:'foreign text'}}), null);
    assert.equal(timelineUserError({billing_notice:{...input, schema_version:2}}), null);
  }
});

function liveBillingNotices() {
  const maps = [];
  const sets = [];
  const listeners = new Map();
  const state = { activeSessionId: 'other', sessions: [{ id: 'deleted' }, { id: 'other' }], tokens: { max: 0 } };
  const sessionStates = { deleted: {}, other: {} };
  const scheduledRunSessionOwners = {};
  const bridge = readFileSync(new URL('../src/platform/tauri/bridge.js', import.meta.url), 'utf8');
  const window = { __PINVOU_TAURI_BRIDGE_FEATURES__: {} };
  vm.runInNewContext(readFileSync(new URL('../src/shared/bridge-shared-helpers.js', import.meta.url), 'utf8'), { window });
  const { isScheduledRunSession } = window.PinvouBridgeShared.create('tauriMain', {
    state, sessionStates, scheduledRunSessionOwners,
  });
  const observedObject = new Proxy(Object, {
    get(target, key) {
      if (key === 'create') return prototype => {
        const result = Object.create(prototype);
        if (prototype === null) maps.push(result);
        return result;
      };
      return Reflect.get(target, key);
    },
  });
  vm.runInNewContext(readFileSync(new URL('../src/platform/tauri/bridge/chat-events.js', import.meta.url), 'utf8'), {
    window, Object: observedObject, console,
    Set: new Proxy(Set, {
      construct(target, args) {
        const result = Reflect.construct(target, args);
        sets.push(result);
        return result;
      },
    }),
  });
  const feature = window.__PINVOU_TAURI_BRIDGE_FEATURES__['chat-events']({
    state, sessionStates, listen(name, handler) { listeners.set(name, handler); },
    isScheduledRunSession,
  });
  const hookStart = bridge.indexOf('function (id, reason)', bridge.indexOf('onSessionBufferPurged:'));
  const hookEnd = bridge.indexOf('\n    },', hookStart) + '\n    }'.length;
  assert.ok(hookStart >= 0 && hookEnd > hookStart);
  const purge = vm.runInNewContext(`(${bridge.slice(hookStart, hookEnd)})`, {
    modeStateEpochs: {}, window: {}, chatFeature: {}, chatEventsFeature: feature,
  });
  const sessionWindow = {};
  const sessionListeners = new Map();
  vm.runInNewContext(readFileSync(new URL('../src/platform/tauri/bridge/sessions.js', import.meta.url), 'utf8'), {
    window: sessionWindow, setTimeout, clearTimeout,
  });
  sessionWindow.__PINVOU_TAURI_BRIDGE_FEATURES__.sessions({
    state, sessionStates, onSessionBufferPurged: purge,
    turnUsageDirty: {}, personaPlaceholderTitles: {}, scheduledRunSessionOwners,
    invalidateScheduledRecentRunsForSession() {}, notify() {},
    listen(name, handler) { sessionListeners.set(name, handler); return Promise.resolve(); },
  });
  return {
    addSession(id, { loaded = true, scheduled = false, listed = true } = {}) {
      if (listed) state.sessions.push({ id });
      if (loaded) sessionStates[id] = { scheduledRunSession: scheduled };
      if (scheduled) scheduledRunSessionOwners[id] = { phase: 'active' };
    },
    emit(id, generation, value) {
      listeners.get('chat:billing_notice')({ payload: { session_id: id, turn_generation: generation, billing_notice: value } });
    },
    deleteSession(id) {
      sessionListeners.get('session:deleted')({ payload: { id } });
    },
    evictSession(id) {
      delete sessionStates[id];
      purge(id, 'evict');
    },
    cancelTimers(id) {
      feature.cancelStreamRenderTimers(id);
    },
    activeSessionId() { return state.activeSessionId; },
    contains(value) { return maps.some(map => Object.values(map).includes(value)); },
    retainedDeletionIDs() { return sets.flatMap(set => [...set]); },
  };
}

test('session purge removes every pending billing generation and preserves another session', () => {
  const live = liveBillingNotices();
  const first = { ...notice };
  const next = { ...notice };
  const other = { ...notice };
  live.emit('deleted', 1, first);
  live.emit('deleted', 2, next);
  live.emit('other', 1, other);
  assert.ok(live.contains(first) && live.contains(next) && live.contains(other));
  live.deleteSession('deleted');
  assert.equal(live.contains(first), false);
  assert.equal(live.contains(next), false);
  assert.equal(live.contains(other), true);
});

test('real session deletion clears active ownership before a late billing notice', () => {
  const live = liveBillingNotices();
  const validActive = { ...notice };
  live.emit('other', 1, validActive);
  assert.equal(live.contains(validActive), true);
  assert.equal(live.activeSessionId(), 'other');
  live.deleteSession('other');
  assert.equal(live.activeSessionId(), null);
  assert.equal(live.contains(validActive), false);
  const late = { ...notice };
  live.emit('other', 2, late);
  assert.equal(live.contains(late), false);
});

test('production purge hook preserves pending notices on idle eviction and timer-only cleanup', () => {
  const live = liveBillingNotices();
  const pending = { ...notice };
  live.emit('deleted', 1, pending);
  live.evictSession('deleted');
  assert.equal(live.contains(pending), true);
  live.cancelTimers('deleted');
  assert.equal(live.contains(pending), true);
  const late = { ...notice };
  live.emit('deleted', 2, late);
  assert.equal(live.contains(late), true, 'evicted session is still a valid known session');
  live.deleteSession('deleted');
  assert.equal(live.contains(pending), false);
  assert.equal(live.contains(late), false);
});

test('a late billing notice cannot repopulate a deleted or unknown session', () => {
  const live = liveBillingNotices();
  live.deleteSession('deleted');
  for (const id of ['deleted', 'unknown']) {
    const late = { ...notice };
    live.emit(id, 3, late);
    assert.equal(live.contains(late), false);
  }
  const active = { ...notice };
  live.emit('other', 3, active);
  assert.equal(live.contains(active), true);
});

test('real scheduled predicate cannot revive a deleted scheduled billing notice', () => {
  const live = liveBillingNotices();
  const id = 'sched-00000000-0000-4000-8000-000000000001';
  live.addSession(id, { scheduled: true });
  const pending = { ...notice };
  live.emit(id, 1, pending);
  assert.equal(live.contains(pending), true);
  live.deleteSession(id);
  assert.equal(live.contains(pending), false);
  const late = { ...notice };
  live.emit(id, 2, late);
  assert.equal(live.contains(late), false);
});

test('valid unloaded and evicted scheduled sessions retain their billing notices', () => {
  const live = liveBillingNotices();
  const id = 'sched-00000000-0000-4000-8000-000000000002';
  live.addSession(id, { loaded: false, scheduled: true, listed: false });
  const unloaded = { ...notice };
  live.emit(id, 1, unloaded);
  assert.equal(live.contains(unloaded), true);
  live.evictSession(id);
  assert.equal(live.contains(unloaded), true);
  const afterEviction = { ...notice };
  live.emit(id, 2, afterEviction);
  assert.equal(live.contains(afterEviction), true);
});

test('bare scheduled prefixes remain valid while real deletion takes precedence', () => {
  const live = liveBillingNotices();
  const id = 'sched-00000000-0000-4000-8000-000000000003';
  const pending = { ...notice };
  live.emit(id, 1, pending);
  assert.equal(live.contains(pending), true, 'unloaded prefix requires no buffer or owner');
  live.evictSession(id);
  live.cancelTimers(id);
  assert.equal(live.contains(pending), true);
  assert.deepEqual(live.retainedDeletionIDs(), []);
  live.deleteSession(id);
  assert.equal(live.contains(pending), false);
  // Stale view facts must not override an observed scheduled-session deletion.
  live.addSession(id, { scheduled: true });
  const late = { ...notice };
  live.emit(id, 2, late);
  assert.equal(live.contains(late), false);
  const other = { ...notice };
  live.emit('sched-other-valid', 1, other);
  assert.equal(live.contains(other), true);
});

test('only true scheduled deletion retains a process-local id and repeat deletion is idempotent', () => {
  const live = liveBillingNotices();
  live.deleteSession('deleted');
  live.evictSession('sched-idle');
  live.cancelTimers('sched-timer-only');
  assert.deepEqual(live.retainedDeletionIDs(), []);
  live.deleteSession('sched-gone');
  live.deleteSession('sched-gone');
  assert.deepEqual(live.retainedDeletionIDs(), ['sched-gone']);
});
