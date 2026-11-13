import { requirePersonalAccount } from './personal-contract.js';

const emptyView = Object.freeze({ authorization: null, sessionEpoch: null });
const sessionEpochOf = client => client.financeSessionEpoch || 0;

export function adminAuthorizationView(snapshot, client) {
  return snapshot?.client === client && snapshot.authorization ? snapshot : emptyView;
}

export function adminAuthorizationSnapshotIsCurrent(snapshot, client) {
  return Boolean(snapshot && snapshot.client === client && snapshot.sessionEpoch === sessionEpochOf(client));
}

// The epoch belongs to the completed session/authorization acquisition, never
// to a later render of a directory that still holds the previous authorization.
export async function readAdminAuthorizationSnapshot(client, isCurrent = () => true) {
  if (!isCurrent()) return null;
  const session = await client.session();
  if (!isCurrent()) return null;
  const sessionEpoch = sessionEpochOf(client);
  let authorization = null;
  if (session.authenticated) {
    authorization = requirePersonalAccount(await client.authorization());
    if (!isCurrent()) return null;
    if (!authorization?.user_id || !Array.isArray(authorization.platform_permissions)) throw new Error('INVALID_AUTHORIZATION');
  }
  const snapshot = Object.freeze({ client, session, authorization, sessionEpoch });
  if (!isCurrent()) return null;
  if (!adminAuthorizationSnapshotIsCurrent(snapshot, client)) throw new Error('INVALID_AUTHORIZATION');
  return snapshot;
}
