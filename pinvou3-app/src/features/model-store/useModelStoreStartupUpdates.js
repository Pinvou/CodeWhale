import { useCallback, useEffect, useReducer, useRef } from 'react';
import { invokeTauri } from '../../platform/tauri/client.js';
import { scheduleStartupUpdateCheck, initialStartupUpdateState, reduceStartupUpdates, hasUnreadStartupUpdates } from './startup-update-check.js';
import { loadViewedUpdateRevisions, saveViewedUpdateRevisions } from './startup-update-read-state.js';

const EMPTY_UPDATES = [];

export function useModelStoreStartupUpdates(available, viewingStore = false) {
  const [state, dispatch] = useReducer(reduceStartupUpdates, undefined, () => initialStartupUpdateState(loadViewedUpdateRevisions()));
  const modelsRef = useRef(null);
  const viewingStoreRef = useRef(viewingStore);
  useEffect(() => {
    saveViewedUpdateRevisions(state.viewedRevisions);
  }, [state.viewedRevisions]);
  useEffect(() => {
    viewingStoreRef.current = viewingStore;
    if (viewingStore) dispatch({ type: 'viewed' });
  }, [viewingStore]);
  useEffect(() => scheduleStartupUpdateCheck({
    available,
    requestFrame: (callback) => window.requestAnimationFrame(callback),
    cancelFrame: (frame) => window.cancelAnimationFrame(frame),
    setTimer: (callback, delay) => window.setTimeout(callback, delay),
    clearTimer: (timer) => window.clearTimeout(timer),
    check: () => invokeTauri('model_store_startup_update_check'),
    onReport: (report) => {
      const rows = Array.isArray(report?.updates) ? report.updates : EMPTY_UPDATES;
      dispatch({ type: 'report', updates: rows, models: modelsRef.current, viewingStore: viewingStoreRef.current });
    },
  }), [available]);

  const reconcile = useCallback((models) => {
    modelsRef.current = models;
    dispatch({ type: 'library', models });
  }, []);
  return {
    updates: available ? state.updates : EMPTY_UPDATES,
    hasUnread: available && !viewingStore && hasUnreadStartupUpdates(state),
    reconcile,
  };
}
