import { remoteProxyComposerIdentity } from './remote-session-adapter.mjs';

const RENEWAL_LEAD_MS = 5_000;
const RENEWABLE_ERRORS = new Set([
  'capability_snapshot_expired', 'capability_snapshot_stale',
  'capability_snapshot_missing', 'capability_snapshot_required', 'session_context_stale',
]);

function fail(code) {
  throw Object.assign(new Error(code), { code });
}

function matchesCapability(connection, capability) {
  return capability
    && capability.connectionId === connection.connectionId
    && capability.controllerLeaseId === connection.controllerLeaseId
    && String(capability.leaseEpoch) === String(connection.leaseEpoch)
    && capability.relayEpoch === connection.relayEpoch
    && capability.remoteControlGeneration === connection.generation
    && capability.remoteSessionRef === connection.sessionRef
    && Number(capability.capabilityRevision) === Number(connection.capabilityRevision);
}

/**
 * Freshness is checked at the action boundary, rather than at React render time.
 * This prepares only a NEW task: no reserve/commit/unknown outcome is replayed.
 * The draft owner may survive renewal; the returned dispatch owner must use the
 * new revision. All native lease, mode, skill and model checks remain in reserve.
 */
export async function prepareRemoteTaskCapabilities({
  binding, ownerIdentity, readState, snapshot, renew, isCurrent, now = Date.now,
}) {
  const read = () => {
    const state = readState();
    const connection = state.controllerConnection;
    if (!ownerIdentity || !isCurrent() || connection?.status !== 'connected'
      || connection.localSessionId !== binding.localSessionId
      || connection.sessionRef !== binding.remoteRef
      || remoteProxyComposerIdentity(binding.localSessionId, connection) !== ownerIdentity) {
      fail('remote_binding_mismatch');
    }
    return state;
  };
  read();
  let needsRenewal = false;
  try {
    await snapshot(binding.remoteRef);
  } catch (error) {
    read();
    if (!RENEWABLE_ERRORS.has(error?.code)) throw error;
    needsRenewal = true;
  }
  let state = read();
  if (needsRenewal || state.controllerConnection.capabilityStatus !== 'ready'
    || !matchesCapability(state.controllerConnection, state.remoteCapabilities)
    || !(Number(state.remoteCapabilities?.expiresAtMs) - now() > RENEWAL_LEAD_MS)) {
    // controllerCapabilitySend joins an existing handshake and waits for B's
    // authenticated reply, not just the request's transport acknowledgement.
    await renew(binding.remoteRef);
    read();
    await snapshot(binding.remoteRef);
    state = read();
  }
  if (state.controllerConnection.mappingConfirmed !== true
    || state.controllerConnection.capabilityStatus !== 'ready'
    || !matchesCapability(state.controllerConnection, state.remoteCapabilities)
    || !(Number(state.remoteCapabilities?.expiresAtMs) - now() > RENEWAL_LEAD_MS)) {
    fail('capability_snapshot_stale');
  }
  return state.controllerConnection;
}
