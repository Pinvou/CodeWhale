import { useLayoutEffect, useMemo, useState } from 'react';

const currentLocation = () => globalThis.location ? globalThis.location.pathname + globalThis.location.search : '';
const sessionVersion = client => client?.financeSessionEpoch ?? 0;
const positiveVersion = value => Number.isSafeInteger(value) && value > 0 ? value : 0;
const observedPlatformVersion = client => positiveVersion(client?.authorizationVersions?.platform_authorization_version);

export function useIdentifierFeedback({ client, owner, routeScope, value, sessionEpoch, platformAuthorizationVersion }) {
  // Accepted parent lineage stays fixed during child state updates. A live client
  // may retire before the parent has accepted its next authorization snapshot.
  const context = useMemo(() => ({
    active: false, generation: 0, location: currentLocation(),
    sessionEpoch: sessionEpoch === undefined ? sessionVersion(client) : sessionEpoch,
    platformVersion: positiveVersion(platformAuthorizationVersion) || observedPlatformVersion(client),
  }), [client, owner, routeScope, value, sessionEpoch, platformAuthorizationVersion]);
  const [result, setResult] = useState(null);
  useLayoutEffect(() => {
    context.active = true;
    return () => { context.active = false; context.generation++; };
  }, [context]);
  const current = () => {
    if (context.sessionEpoch !== sessionVersion(client) || context.location && context.location !== currentLocation()) return false;
    const observed = observedPlatformVersion(client);
    // Legacy callers without an accepted version can establish their first
    // header baseline. A known accepted version must never adopt a higher one.
    if (!context.platformVersion && observed) context.platformVersion = observed;
    return !observed || observed <= context.platformVersion;
  };
  const canCopy = current();
  const status = canCopy && result?.context === context ? result.status : '';
  const copy = async () => {
    if (!context.active || typeof value !== 'string' || !value) return;
    if (!current()) { setResult({ context, status: '' }); return; }
    if (status === 'pending') return;
    const generation = ++context.generation;
    const accept = next => {
      if (context.active && generation === context.generation) setResult({ context, status: current() ? next : '' });
    };
    accept('pending');
    try {
      const clipboard = globalThis.navigator?.clipboard;
      if (typeof clipboard?.writeText !== 'function') { accept('unavailable'); return; }
      await clipboard.writeText(value);
      accept('copied');
    } catch { accept('failed'); }
  };
  return { status, copy, context, canCopy };
}
