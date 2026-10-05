import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createAdminOperationScope } from './admin-operation-scope.js';
import { useAdminMutationOwner } from './admin-mutation-context.js';

export function useAdminOperation(onAuthorizationError, area) {
  const mutation = useAdminMutationOwner(area);
  const scope = useMemo(() => createAdminOperationScope(mutation.owner, area), [mutation.owner, area]);
  const scopeRef = useRef(null);
  scopeRef.current = scope;
  const mountedRef = useRef(false);
  const authorizationRef = useRef(onAuthorizationError);
  authorizationRef.current = onAuthorizationError;
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const [noticeKey, setNoticeKey] = useState('');
  useEffect(() => {
    mountedRef.current = true;
    setBusy(scope.busy);
    const unsubscribe = mutation.owner.subscribe(() => { if (mountedRef.current && scopeRef.current === scope) setBusy(scope.busy); });
    return () => { mountedRef.current = false; unsubscribe(); scope.invalidate(); };
  }, [scope, mutation.owner]);
  const invalidate = useCallback(() => {
    scope.invalidate();
    setBusy(scope.busy); setFailed(false); setNoticeKey('');
  }, [scope]);
  const run = useCallback(async (action, mutation = false) => {
    const ticket = scope.begin(mutation);
    if (!ticket) return;
    const isCurrent = () => mountedRef.current && scopeRef.current === scope && scope.isCurrent(ticket);
    setBusy(true); setFailed(false); setNoticeKey('');
    try {
      const key = await action({ signal: ticket.controller.signal, isCurrent });
      if (isCurrent()) setNoticeKey(key || '');
    } catch (reason) {
      if (isCurrent() && reason?.name !== 'AbortError') {
        if (!authorizationRef.current?.(reason)) setFailed(true);
      }
    } finally {
      scope.finish(ticket);
      // A stale mutation still owns its lock until this finally; it must not
      // overwrite the notice/detail of the newly selected view.
      if (mountedRef.current && scopeRef.current === scope) setBusy(scope.busy);
    }
  }, [scope]);
  return { busy: busy || mutation.busy, completed: mutation.completed, failed, noticeKey, run, invalidate };
}
