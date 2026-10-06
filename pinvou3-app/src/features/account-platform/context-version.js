// Response versions belong to one authenticated user and login lifecycle.
// A late response from a previous login must never refresh the current account.
export function createContextVersionTracker() {
  let epoch = 0;
  let userId = null;
  let contextVersion = 0;
  let authorizationVersion = 0;
  const listeners = new Set();
  const positiveVersion = value => {
    if (typeof value !== 'number' && typeof value !== 'string') return 0;
    if (typeof value === 'string' && !/^[1-9]\d*$/u.test(value)) return 0;
    const result = Number(value);
    return Number.isSafeInteger(result) && result > 0 ? result : 0;
  };
  return {
    get epoch() { return epoch; },
    get contextVersion() { return contextVersion; },
    get authorizationVersion() { return authorizationVersion; },
    reset() { epoch += 1; userId = null; contextVersion = 0; authorizationVersion = 0; },
    bindUser(nextUserId, expectedEpoch) {
      if (expectedEpoch !== epoch || typeof nextUserId !== 'string' || !nextUserId) return false;
      if (userId !== nextUserId) { epoch += 1; userId = nextUserId; contextVersion = 0; authorizationVersion = 0; }
      return true;
    },
    subscribe(listener) { listeners.add(listener); return () => listeners.delete(listener); },
    observe(context, authorization, expectedEpoch) {
      if (expectedEpoch !== epoch) return;
      const nextContext = positiveVersion(context);
      const nextAuthorization = positiveVersion(authorization);
      // Authorization counters are scoped to the active organization, not global.
      // Never combine an old context's counter with the current context.
      if (!nextContext || nextContext < contextVersion) return;
      if (nextContext === contextVersion && (!nextAuthorization || nextAuthorization === authorizationVersion)) return;
      if (nextContext > contextVersion) {
        contextVersion = nextContext;
        authorizationVersion = nextAuthorization;
      } else {
        authorizationVersion = nextAuthorization;
      }
      for (const listener of listeners) listener(contextVersion, authorizationVersion);
    },
  };
}
