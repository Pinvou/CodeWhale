import { useEffect, useRef, useState } from 'react';
import { requireOrganizationAuthorization } from './organization-scope.js';

// Scope authority is separate from Me's organization-wide permission array.
// Version notifications retire it synchronously, before a parent refresh finishes.
export function useOrganizationScope(api, me, owner, onChanged, failureGuard, failureIdentity) {
 const epoch = api.client.financeSessionEpoch || 0;
 const live = useRef(null), authority = useRef(null), generation = useRef(0), controller = useRef(null), changed = useRef(onChanged);
 const [state, setState] = useState({ owner: '', api: null, me: null, context: null, loading: false, error: '', revision: 0 });
 live.current = { api, owner, epoch, me }; changed.current = onChanged;
 const current = () => live.current.api === api && live.current.owner === owner && live.current.me === me && (api.client.financeSessionEpoch || 0) === epoch;
 const retire = error => {
  authority.current = null; generation.current++; controller.current?.abort();
  if (current()) setState(old => ({ owner, api, me, context: null, loading: false, error, revision: old.revision }));
 };
 const refresh = async (explicit = false) => {
  if (!current()) return;
  if (explicit) failureGuard.retry(failureIdentity, 'authorization');
  else if (failureGuard.isDenied(failureIdentity, 'authorization')) { retire('permissions'); return; }
  retire(''); const stamp = ++generation.current, abort = new AbortController(); controller.current = abort;
  setState(old => ({ owner, api, me, context: null, loading: true, error: '', revision: old.revision }));
  try {
   const context = requireOrganizationAuthorization(await api.authorizationContext(abort.signal), me);
   if (stamp !== generation.current || !current()) return;
   authority.current = context;
   setState(old => ({ owner, api, me, context, loading: false, error: '', revision: old.revision + 1 }));
  } catch (reason) {
   if (stamp !== generation.current || !current() || reason.name === 'AbortError') return;
   retire(reason.status === 403 ? 'permissions' : reason.message === 'INVALID_ORGANIZATION_AUTHORIZATION' ? 'sourceChanged' : 'unavailable');
   if ((reason.status === 403 || reason.message === 'INVALID_ORGANIZATION_AUTHORIZATION') && failureGuard.deny(failureIdentity, 'authorization')) void changed.current?.();
  }
 };
 useEffect(() => {
  const invalidate = values => {
   if (!current()) return;
   const context = typeof values === 'number' ? values : Number(values?.context_version || 0);
   const authorization = Number(values?.organization_authorization_version || 0);
   if (context > me.active_context.context_version || authorization > me.active_context.organization_authorization_version) {
    retire('sourceChanged'); void changed.current?.();
   }
  };
  const stopContext = api.client.onContextVersion?.(invalidate);
  const stopAuthorization = api.client.onAuthorizationVersion?.(invalidate);
  void refresh();
  return () => { authority.current = null; generation.current++; controller.current?.abort(); stopContext?.(); stopAuthorization?.(); };
 }, [api, owner, me]); // eslint-disable-line react-hooks/exhaustive-deps
 const visible = state.owner === owner && state.api === api && state.me === me && current() ? state : { context: null, loading: false, error: '', revision: 0 };
 return { ...visible, refresh, isCurrent: context => current() && context !== null && authority.current === context };
}
