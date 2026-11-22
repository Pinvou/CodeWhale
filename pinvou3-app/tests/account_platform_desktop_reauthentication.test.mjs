import test from 'node:test';
import assert from 'node:assert/strict';
import { createDesktopReauthenticationCoordinator } from '../src/features/account-platform/desktop-reauthentication.js';
import { readClosureIntent, saveClosureIntent } from '../src/features/account-platform/closure-intent.js';

const actor = '00000000-0000-4000-8000-000000000001';
const other = '00000000-0000-4000-8000-000000000002';
const profile = id => ({ user_id: id, status: 'ACTIVE', active_context: { context_type: 'PERSONAL', organization_id: null, membership_version: 1, context_version: 1, authorization_version: 1, personal_authorization_version: 1, platform_authorization_version: 1, organization_authorization_version: 1, permissions: [] }, personal_permissions: [], platform_permissions: [] });
const deferred = () => { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; };
function fixture(overrides = {}) {
  let ticket;
  const verified = [], rejected = [];
  const client = { financeSessionEpoch: 0, bootstrap: async () => profile(actor), ...overrides };
  const coordinator = createDesktopReauthenticationCoordinator({ client, getActor: () => actor, onPrompt: value => { ticket = value; }, onVerified: (...values) => verified.push(values), onRejected: (...values) => rejected.push(values) });
  return { client, coordinator, verified, rejected, ticket: () => ticket };
}
const isCode = code => reason => reason.code === code;

test('same-actor bootstrap is mandatory before native re-login completion', async () => {
  const f = fixture(), pending = f.coordinator.request('ja'), ticket = f.ticket();
  assert.throws(() => f.coordinator.complete(ticket), isCode('REAUTHENTICATION_EXPIRED'));
  await assert.rejects(pending, isCode('REAUTHENTICATION_EXPIRED'));
  assert.equal(f.verified.length, 0);
});
test('another actor cannot complete the original request', async () => {
  const f = fixture({ bootstrap: async () => profile(other) });
  const pending = f.coordinator.request('en'), ticket = f.ticket();
  await assert.rejects(f.coordinator.authenticate(ticket, async () => { f.client.financeSessionEpoch++; }), isCode('REAUTHENTICATION_ACTOR_MISMATCH'));
  await assert.rejects(pending, isCode('REAUTHENTICATION_ACTOR_MISMATCH'));
  assert.equal(f.verified.length, 0); assert.equal(f.rejected.length, 1);
});
test('password failure rejects the re-login instead of restoring the old authenticated view', async () => {
  const f = fixture(), pending = f.coordinator.request('zh'), failure = new Error('controlled grant rejected');
  await assert.rejects(f.coordinator.authenticate(f.ticket(), async () => { throw failure; }), reason => reason === failure);
  await assert.rejects(pending, reason => reason === failure);
  assert.equal(f.verified.length, 0); assert.equal(f.coordinator.hasPending(), false);
});
test('invalid fresh Account acknowledgement cannot restore authentication', async () => {
  const f = fixture({ bootstrap: async () => ({ user_id: actor }) }), pending = f.coordinator.request('en');
  await assert.rejects(f.coordinator.authenticate(f.ticket(), async () => {}), /INVALID_ACCOUNT_ACK/);
  await assert.rejects(pending, /INVALID_ACCOUNT_ACK/); assert.equal(f.verified.length, 0);
});
test('cancel during native login fences a late success without logging out a newer session', async () => {
  const gate = deferred(); let bootstraps = 0, logouts = 0;
  const f = fixture({ bootstrap: async () => { bootstraps++; return profile(actor); }, logout: async () => { logouts++; } });
  const pending = f.coordinator.request('en'), ticket = f.ticket();
  const native = f.coordinator.authenticate(ticket, () => gate.promise);
  f.coordinator.cancel(ticket); await assert.rejects(pending, isCode('REAUTHENTICATION_CANCELLED'));
  f.client.financeSessionEpoch++; gate.resolve();
  await assert.rejects(native, isCode('REAUTHENTICATION_EXPIRED'));
  assert.equal(bootstraps, 0); assert.equal(logouts, 0); assert.equal(f.verified.length, 0);
});
test('cancel during bootstrap refuses its late response', async () => {
  const gate = deferred(), started = deferred();
  const f = fixture({ bootstrap: () => { started.resolve(); return gate.promise; } });
  const pending = f.coordinator.request('ja'), ticket = f.ticket();
  const native = f.coordinator.authenticate(ticket, async () => {});
  await started.promise; f.coordinator.cancel(ticket); gate.resolve(profile(actor));
  await assert.rejects(pending, isCode('REAUTHENTICATION_CANCELLED'));
  await assert.rejects(native, isCode('REAUTHENTICATION_EXPIRED')); assert.equal(f.verified.length, 0);
});
test('client disposal fences old native work and releases the waiting original operation', async () => {
  const gate = deferred(), f = fixture(), pending = f.coordinator.request('zh'), ticket = f.ticket();
  const native = f.coordinator.authenticate(ticket, () => gate.promise);
  f.coordinator.dispose(); await assert.rejects(pending, isCode('REAUTHENTICATION_EXPIRED'));
  gate.resolve(); await assert.rejects(native, isCode('REAUTHENTICATION_EXPIRED'));
  await assert.rejects(f.coordinator.request('zh'), isCode('REAUTHENTICATION_EXPIRED')); assert.equal(f.verified.length, 0);
});
test('a replaced session during bootstrap or before completion cannot be certified by an old read', async () => {
  const gate = deferred(), started = deferred(), f = fixture({ bootstrap: () => { started.resolve(); return gate.promise; } });
  const pending = f.coordinator.request('en'), ticket = f.ticket();
  const native = f.coordinator.authenticate(ticket, async () => {});
  await started.promise; f.client.financeSessionEpoch++; gate.resolve(profile(actor));
  await assert.rejects(native, isCode('REAUTHENTICATION_EXPIRED')); await assert.rejects(pending, isCode('REAUTHENTICATION_EXPIRED'));
  const g = fixture(), later = g.coordinator.request('en');
  await g.coordinator.authenticate(g.ticket(), async () => {}); g.client.financeSessionEpoch++;
  assert.throws(() => g.coordinator.complete(g.ticket()), isCode('REAUTHENTICATION_EXPIRED'));
  await assert.rejects(later, isCode('REAUTHENTICATION_EXPIRED')); assert.equal(g.verified.length, 0);
});
test('duplicate prompts and duplicate native submission do not start another authentication action', async () => {
  const gate = deferred(), f = fixture(), pending = f.coordinator.request('en'), ticket = f.ticket(); let nativeCalls = 0;
  await assert.rejects(f.coordinator.request('ja'), isCode('REAUTHENTICATION_IN_PROGRESS'));
  const native = f.coordinator.authenticate(ticket, () => { nativeCalls++; return gate.promise; });
  await assert.rejects(f.coordinator.authenticate(ticket, async () => { nativeCalls++; }), isCode('REAUTHENTICATION_EXPIRED'));
  gate.resolve(); await native; f.coordinator.complete(ticket); await pending; assert.equal(nativeCalls, 1);
});
test('an authentication result leaves both first-attempt and unknown-result closure proof intact', async () => {
  const items = new Map(), storage = { getItem: key => items.get(key) ?? null, setItem: (key, value) => items.set(key, value), removeItem: key => items.delete(key) };
  const original = saveClosureIntent({ actor, scope: 'account-closure', key: 'closure-controlled-original-key', facts: { confirmation: 'CLOSE_MY_ACCOUNT' } }, storage);
  for (const cancel of [false, true]) {
    const f = fixture(), pending = f.coordinator.request('zh');
    if (cancel) { f.coordinator.cancel(f.ticket()); await assert.rejects(pending, isCode('REAUTHENTICATION_CANCELLED')); }
    else { await f.coordinator.authenticate(f.ticket(), async () => { f.client.financeSessionEpoch++; }); f.coordinator.complete(f.ticket()); await pending; }
    assert.deepEqual(readClosureIntent(actor, 'account-closure', storage), original);
    assert.equal(readClosureIntent(other, 'account-closure', storage), null);
  }
});
