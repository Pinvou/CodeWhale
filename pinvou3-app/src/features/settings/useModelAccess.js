import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createModelAccessCoordinator, resolveModelAccessState } from './model-access.js';

export function useModelAccess({ modelsBridge, accountClient, enabled, sessionId, selectionKey, modelState }) {
  const owner = useRef(null);
  const previousSelection = useRef(JSON.stringify([sessionId, selectionKey]));
  const [state, setState] = useState({ accountStatus: 'unknown', refreshStatus: 'idle' });
  useEffect(() => {
    if (!enabled) return undefined;
    const coordinator = createModelAccessCoordinator(modelsBridge, accountClient);
    owner.current = coordinator;
    setState(coordinator.getState());
    const unsubscribe = coordinator.subscribe(setState);
    void coordinator.refresh();
    const focus = () => { void coordinator.refresh(); };
    globalThis.addEventListener?.('focus', focus);
    return () => { unsubscribe(); coordinator.dispose(); if (owner.current === coordinator) owner.current = null; globalThis.removeEventListener?.('focus', focus); };
  }, [modelsBridge, accountClient, enabled]);
  useEffect(() => {
    const identity = JSON.stringify([sessionId, selectionKey]);
    if (previousSelection.current === identity) return;
    previousSelection.current = identity;
    if (owner.current) void owner.current.refreshSelection();
  }, [sessionId, selectionKey]);
  const refresh = useCallback(() => owner.current?.refresh() || Promise.resolve(null), []);
  return useMemo(() => ({ ...state, resolved: resolveModelAccessState(modelState, state), refresh }), [modelState, state, refresh]);
}
