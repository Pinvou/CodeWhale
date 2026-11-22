export { resolveModelAccessState } from '../../shared/model-access-state.js';

export async function refreshModelAccessAfterAuthentication(modelsBridge) {
  if (!modelsBridge) return false;

  let modelsRefreshed = false;
  try {
    modelsRefreshed = await modelsBridge.loadModels?.() === true;
  } catch {}

  // Refresh the effective config even if the catalog request failed. Either
  // call publishes its own state slice, while the combined boolean lets a
  // caller distinguish a complete refresh from a swallowed bridge failure.
  let effectiveConfigRefreshed = false;
  try {
    effectiveConfigRefreshed = await modelsBridge.loadEffectiveModelConfig?.() === true;
  } catch {}
  return modelsRefreshed && effectiveConfigRefreshed;
}

// One UI owner coordinates identity and model reads for every login entry.
// Epochs retire old reads before credentials change; refreshes never select a
// different model or write a key on behalf of the user.
export function createModelAccessCoordinator(modelsBridge, accountClient) {
  let state = { accountStatus: accountClient ? 'unknown' : 'unsupported', refreshStatus: 'idle' };
  let generation = 0, stopped = false, pending = null, controller = null;
  const listeners = new Set();
  const publish = next => { if (!stopped) { state = next; for (const listener of listeners) listener(state); } };
  const retire = () => { generation++; controller?.abort(); pending = null; };
  const refresh = () => {
    if (stopped) return Promise.resolve(null);
    if (pending) return pending;
    const epoch = generation;
    controller = new AbortController();
    const signal = controller.signal;
    publish({ ...state, refreshStatus: 'checking' });
    const session = accountClient ? Promise.resolve().then(() => accountClient.session(signal)) : Promise.resolve(null);
    pending = Promise.allSettled([session, refreshModelAccessAfterAuthentication(modelsBridge)]).then(([account, models]) => {
      if (stopped || epoch !== generation) return null;
      const payload = account.status === 'fulfilled' ? account.value : null;
      const unavailable = payload?.errorCode && payload.errorCode !== 'authentication_required';
      const accountStatus = !accountClient ? 'unsupported' : account.status === 'rejected' || unavailable ? 'error'
        : payload?.authenticated === true ? 'authenticated' : payload?.authenticated === false ? 'guest' : 'error';
      const next = { accountStatus, refreshStatus: models.status === 'fulfilled' && models.value === true ? 'ready' : 'error' };
      publish(next); return next;
    }).finally(() => { if (epoch === generation) pending = null; });
    return pending;
  };
  const unsubscribe = accountClient?.onAuthenticationChange?.(phase => {
    if (phase === 'changing') { retire(); publish({ accountStatus: 'checking', refreshStatus: 'checking' }); }
    else { retire(); void refresh(); }
  });
  return {
    getState: () => state,
    subscribe: listener => { listeners.add(listener); return () => listeners.delete(listener); },
    refresh,
    refreshSelection: () => { retire(); return refresh(); },
    dispose: () => { stopped = true; retire(); unsubscribe?.(); listeners.clear(); },
  };
}
