import { hasPermission } from './authorization-ui.js';

const participants = new WeakMap();
const retained = new WeakMap();
export const personalRechargeOwner = profile => `${profile.user_id}:${profile.active_context?.context_version || 0}:${profile.active_context?.personal_authorization_version || 0}`;
const sameIntent = (left, right) => left && right && left.key === right.key && left.productID === right.productID && left.channel === right.channel;
const recoveryError = () => new Error('RECHARGE_RECOVERY_UNAVAILABLE');
const recordFor = (client, actor) => retained.get(client)?.get(actor);

// Only native recent-auth captures an active participant. Ordinary view changes
// do not start a new retention policy or transfer another actor's invocation.
export function registerPersonalRechargeReauthentication(client, participant) {
  participants.set(client, participant);
  return () => { if (participants.get(client) === participant) participants.delete(client); };
}
export function capturePersonalRechargeReauthentication(client, actor) {
  const participant = participants.get(client);
  if (!participant || participant.owner.split(':')[0] !== actor) return;
  if (participant.epoch !== (client.financeSessionEpoch || 0)) throw recoveryError();
  const intent = participant.getIntent();
  if (!intent) return;
  const prior = recordFor(client, actor);
  if (prior) {
    if (prior.owner !== participant.owner || !sameIntent(prior.intent, intent)) throw recoveryError();
    return;
  }
  if (Object.keys(intent).sort().join(',') !== 'channel,key,productID' || typeof intent.key !== 'string' || typeof intent.productID !== 'string' || typeof intent.channel !== 'string') throw recoveryError();
  let records = retained.get(client);
  if (!records) { records = new Map(); retained.set(client, records); }
  records.set(actor, { owner: participant.owner, intent: Object.freeze({ key: intent.key, productID: intent.productID, channel: intent.channel }), authorizedEpoch: null });
}
export function authorizePersonalRechargeReauthentication(client, profile) {
  const record = recordFor(client, profile.user_id);
  if (!record) return;
  record.authorizedEpoch = null;
  if (record.owner === personalRechargeOwner(profile) && profile.active_context?.context_type !== 'ORGANIZATION' &&
      hasPermission(profile, 'personal.billing.read') && hasPermission(profile, 'personal.recharge.create')) record.authorizedEpoch = client.financeSessionEpoch || 0;
}
export function recoverPersonalRechargeReauthentication(client, owner) {
  const record = recordFor(client, owner.split(':')[0]);
  if (!record) return { intent: null, blocked: false };
  if (record.owner !== owner || record.authorizedEpoch !== (client.financeSessionEpoch || 0)) return { intent: null, blocked: true };
  return { intent: record.intent, blocked: false };
}
export function settlePersonalRechargeReauthentication(client, owner, intent) {
  const actor = owner.split(':')[0], record = recordFor(client, actor);
  // An authoritative old-view receipt may close its exact invocation, but can
  // neither clear a replacement key nor make the retired UI active again.
  if (record?.owner === owner && sameIntent(record.intent, intent)) retained.get(client).delete(actor);
}
