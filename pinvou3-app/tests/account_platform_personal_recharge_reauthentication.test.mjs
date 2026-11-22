import test from 'node:test';
import assert from 'node:assert/strict';
import { personalRechargeOwner, registerPersonalRechargeReauthentication, capturePersonalRechargeReauthentication, authorizePersonalRechargeReauthentication, recoverPersonalRechargeReauthentication, settlePersonalRechargeReauthentication } from '../src/features/account-platform/personal-recharge-reauthentication.js';
const actor = '00000000-0000-4000-8000-000000000001', other = '00000000-0000-4000-8000-000000000002';
const profile = (user_id = actor, context_version = 1) => ({ user_id, personal_permissions: ['personal.billing.read', 'personal.recharge.create'], active_context: { context_type: 'PERSONAL', context_version, personal_authorization_version: 1 } });
const original = { key: 'recharge-controlled-original-key', productID: 'controlled-product', channel: 'UNIONPAY' };
function fixture() {
  const client = { financeSessionEpoch: 3 }, owner = personalRechargeOwner(profile());
  const unregister = registerPersonalRechargeReauthentication(client, { owner, epoch: 3, getIntent: () => original });
  capturePersonalRechargeReauthentication(client, actor); unregister();
  return { client, owner };
}
test('native snapshot freezes original facts and becomes retryable only after exact fresh Account verification', () => {
  const f = fixture(); assert.deepEqual(recoverPersonalRechargeReauthentication(f.client, f.owner), { intent: null, blocked: true });
  f.client.financeSessionEpoch++; authorizePersonalRechargeReauthentication(f.client, profile());
  const recovered = recoverPersonalRechargeReauthentication(f.client, f.owner);
  assert.deepEqual(recovered, { intent: original, blocked: false }); assert.equal(Object.isFrozen(recovered.intent), true);
});
test('another client or actor cannot recover the captured invocation', () => {
  const f = fixture(); f.client.financeSessionEpoch++;
  authorizePersonalRechargeReauthentication(f.client, profile(other));
  assert.deepEqual(recoverPersonalRechargeReauthentication(f.client, f.owner), { intent: null, blocked: true });
  assert.deepEqual(recoverPersonalRechargeReauthentication(f.client, personalRechargeOwner(profile(other))), { intent: null, blocked: false });
  assert.deepEqual(recoverPersonalRechargeReauthentication({ financeSessionEpoch: 4 }, f.owner), { intent: null, blocked: false });
});
test('changed context, authorization or permission retains an isolated snapshot and forbids a fresh key', () => {
  for (const changed of [profile(actor, 2), { ...profile(), active_context: { ...profile().active_context, personal_authorization_version: 2 } }, { ...profile(), personal_permissions: ['personal.billing.read'] }, { ...profile(), active_context: { ...profile().active_context, context_type: 'ORGANIZATION' } }]) {
    const f = fixture(); f.client.financeSessionEpoch++; authorizePersonalRechargeReauthentication(f.client, changed);
    assert.equal(recoverPersonalRechargeReauthentication(f.client, personalRechargeOwner(changed)).blocked, true);
    authorizePersonalRechargeReauthentication(f.client, profile());
    assert.deepEqual(recoverPersonalRechargeReauthentication(f.client, f.owner).intent, original);
  }
});
test('cancel or failed login does not settle the snapshot and returning to the original owner can recover it', () => {
  const f = fixture(); assert.equal(recoverPersonalRechargeReauthentication(f.client, f.owner).blocked, true);
  f.client.financeSessionEpoch++; authorizePersonalRechargeReauthentication(f.client, profile());
  assert.deepEqual(recoverPersonalRechargeReauthentication(f.client, f.owner).intent, original);
});
test('a late authoritative receipt closes only its complete original key and facts', () => {
  const f = fixture();
  for (const wrong of [{ ...original, key: 'recharge-controlled-new-key' }, { ...original, channel: 'ALIPAY' }, { ...original, productID: 'another-product' }]) {
    settlePersonalRechargeReauthentication(f.client, f.owner, wrong); assert.equal(recoverPersonalRechargeReauthentication(f.client, f.owner).blocked, true);
  }
  settlePersonalRechargeReauthentication(f.client, personalRechargeOwner(profile(actor, 2)), original);
  assert.equal(recoverPersonalRechargeReauthentication(f.client, f.owner).blocked, true);
  settlePersonalRechargeReauthentication(f.client, f.owner, original);
  assert.deepEqual(recoverPersonalRechargeReauthentication(f.client, f.owner), { intent: null, blocked: false });
});
test('a different pending slot fails closed and old participant cleanup cannot remove the replacement', () => {
  const f = fixture();
  const removeOld = registerPersonalRechargeReauthentication(f.client, { owner: f.owner, epoch: 3, getIntent: () => original });
  const replacement = { ...original, key: 'recharge-controlled-new-key' };
  registerPersonalRechargeReauthentication(f.client, { owner: f.owner, epoch: 3, getIntent: () => replacement }); removeOld();
  assert.throws(() => capturePersonalRechargeReauthentication(f.client, actor), /RECHARGE_RECOVERY_UNAVAILABLE/);
  authorizePersonalRechargeReauthentication(f.client, profile());
  assert.deepEqual(recoverPersonalRechargeReauthentication(f.client, f.owner).intent, original);
});
test('a stale participant epoch cannot be captured under another session', () => {
  const client = { financeSessionEpoch: 4 }, owner = personalRechargeOwner(profile());
  registerPersonalRechargeReauthentication(client, { owner, epoch: 3, getIntent: () => original });
  assert.throws(() => capturePersonalRechargeReauthentication(client, actor), /RECHARGE_RECOVERY_UNAVAILABLE/);
});
