import { useCallback, useEffect, useRef, useState } from 'react';
import { prepareAfterStartupPaint, warnStartupUpdateFailure } from './startup-update-scheduler.js';
import { readViewedUpdateSnapshot } from './startup-update-snapshot.js';

const EMPTY_UPDATES = [];

export function useModelStoreStartupUpdates(available, viewingStore = false) {
  const [state, setState] = useState(() => ({
    updates: EMPTY_UPDATES, hasUnread: false, snapshot: readViewedUpdateSnapshot(),
  }));
  const contextRef = useRef({ snapshot: state.snapshot });
  useEffect(() => {
    const context = contextRef.current;
    context.viewingStore = viewingStore;
    if (viewingStore) context.controller?.send({ type: 'viewed' });
  }, [viewingStore]);
  useEffect(() => {
    if (!available) return;
    const context = contextRef.current;
    let alive = true;
    let cancelCheck;
    const cancelPaint = prepareAfterStartupPaint((deadline) => {
      import('./startup-update-runtime.js').then((runtime) => {
        if (!alive) return;
        context.controller ??= runtime.createStartupUpdateController(context.snapshot, setState);
        context.snapshot = null;
        cancelCheck = runtime.startStartupUpdateCheck(context, deadline);
      }).catch(warnStartupUpdateFailure);
    });
    return () => { alive = false; cancelPaint(); cancelCheck?.(); };
  }, [available]);
  const reconcile = useCallback((models) => {
    const context = contextRef.current;
    context.models = models;
    context.controller?.send({ type: 'library', models });
  }, []);
  return {
    updates: available ? state.updates : EMPTY_UPDATES,
    hasUnread: available && !viewingStore && state.hasUnread,
    reconcile,
  };
}
