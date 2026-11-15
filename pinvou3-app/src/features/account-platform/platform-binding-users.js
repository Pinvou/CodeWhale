import { useEffect, useMemo, useRef, useState } from 'react';

const locationKey = () => globalThis.location ? globalThis.location.pathname + globalThis.location.search : '';
const positiveVersion = value => Number.isSafeInteger(value) && value > 0 ? value : 0;

// Freeze the accepted parent lineage, never a newer live session inferred during
// a local dialog, paging or metadata update. Labels are optional presentation.
export function platformBindingContext(client, me, acceptedSessionEpoch, routeScope = '') {
  return Object.freeze({ client, owner: me?.user_id || '',
    epoch: acceptedSessionEpoch === undefined ? client.financeSessionEpoch || 0 : acceptedSessionEpoch,
    version: positiveVersion(me?.platform_authorization_version || me?.active_context?.platform_authorization_version || me?.authorization_version),
    location: locationKey(), routeScope });
}
export function platformBindingContextIsCurrent(context, client, me, acceptedSessionEpoch, routeScope = '') {
  const accepted = platformBindingContext(client, me, acceptedSessionEpoch, routeScope);
  const observed = positiveVersion(client.authorizationVersions?.platform_authorization_version);
  return context.client === client && context.owner === accepted.owner && context.epoch === accepted.epoch
    && context.version === accepted.version && context.routeScope === routeScope && context.location === locationKey()
    && context.epoch === (client.financeSessionEpoch || 0) && (!observed || observed <= context.version);
}

// At most the visible binding page (20 rows), or one detail/confirmation target.
// No catalog-wide cache or identity inference from a label. Partial lookup failure
// cannot turn a readable binding or an eligible command into a failed workflow.
export function usePlatformBindingUsers({ client, me, acceptedSessionEpoch, routeScope, userIDs, active }) {
  const canRead = me?.platform_permissions?.includes('platform.users.read') === true;
  const idsKey = JSON.stringify([...new Set(userIDs)].sort());
  const owner = me?.user_id || '';
  const version = me?.platform_authorization_version || me?.active_context?.platform_authorization_version || me?.authorization_version || 0;
  const context = useMemo(() => platformBindingContext(client, me, acceptedSessionEpoch, routeScope), [client, owner, version, acceptedSessionEpoch, routeScope, idsKey, active, canRead]);
  const [result, setResult] = useState(null), [retry, setRetry] = useState(0);
  const activeContext = useRef(context);
  activeContext.current = context;
  const current = () => active && canRead && platformBindingContextIsCurrent(context, client, me, acceptedSessionEpoch, routeScope);
  useEffect(() => {
    let live = true;
    const controller = new AbortController();
    const accept = () => live && activeContext.current === context && !controller.signal.aborted && current();
    if (current()) {
      const entries = {};
      for (const userID of JSON.parse(idsKey)) {
        void client.userDetail(userID, controller.signal).then(user => {
          if (!accept()) return;
          // The typed client also checks the canonical response identity.
          entries[userID] = { status: 'ready', user };
          setResult({ context, entries: { ...entries } });
        }).catch(failure => {
          if (!accept() || failure?.name === 'AbortError') return;
          entries[userID] = { status: 'unavailable' };
          setResult({ context, entries: { ...entries } });
        });
      }
    }
    return () => { live = false; controller.abort(); };
  }, [context, retry]); // eslint-disable-line react-hooks/exhaustive-deps
  const entries = current() && result?.context === context ? result.entries : {};
  const lookup = userID => !canRead ? { status: 'notPermitted' } : !current() ? { status: 'retired' } : entries[userID] || { status: 'loading' };
  return { lookup, retry: () => { if (current()) { setResult(null); setRetry(value => value + 1); } } };
}
