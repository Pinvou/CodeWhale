import { useCallback, useEffect, useRef } from 'react';
import { invokeTauri, tauriEvents, listenManualModelSelection } from '../../platform/tauri/client.js';
import { fetchCmsModels, fetchCmsStatus, fetchCmsTask } from './cms-api.js';
import { createCmsdSwitchFollow } from './switch-follow.js';
import { useCmsdTaskWatch } from './use-cmsd-tasks.js';
import { mergeHandoffRecovery } from './handoff-recovery.js';

export function useCmsdSwitchFollow({ bridge, snapshot, copy, onNotice, modelStoreAvailable = false }) {
  const latest = useRef({ snapshot, copy, onNotice });
  latest.current = { snapshot, copy, onNotice };
  const follower = useRef(null);
  const taskWatchRef = useRef(null);
  const routeRefresh = useRef(null);
  const refreshStoreRoutes = useCallback(async ({ fresh = false } = {}) => {
    if (!modelStoreAvailable) return;
    const previous = routeRefresh.current;
    if (previous) {
      await previous;
      if (!fresh) return;
    }
    if (!routeRefresh.current) {
      routeRefresh.current = invokeTauri('model_store_library')
        .then(value => { if (value?.link_sync_error) throw new Error(value.link_sync_error); })
        .finally(() => { routeRefresh.current = null; });
    }
    return routeRefresh.current;
  }, [modelStoreAvailable]);
  const reconcileRoutes = useRef(null);
  // Both index observations and private details must reconcile a snapshot
  // begun after task success; this ref also follows capability changes.
  reconcileRoutes.current = task => refreshStoreRoutes({ fresh: task?.status === 'succeeded' });
  useEffect(() => { if (bridge.available && modelStoreAvailable) void refreshStoreRoutes().catch(error => console.warn('[megacube] initial route reconciliation failed', error)); }, [bridge, modelStoreAvailable, refreshStoreRoutes]);
  useEffect(() => {
    if (!bridge.available) return undefined;
    let disposed = false;
    let retryTimer;
    const unlisten = [];
    // This private recovery path never feeds old tasks into the maintenance UI.
    const recover = () => { void follower.current?.recoverPending(); };
    const recoveryTimer = setInterval(recover, 3000);
    const queued = [];
    const dispatch = (method, payload, originalSnapshot) => {
      const state = originalSnapshot || bridge.state?.getMany?.(['sessions', 'models']) || latest.current.snapshot;
      const captured = { activeSessionId: state?.activeSessionId,
        currentSessionModelId: state?.currentSessionModelId, activeModelId: state?.activeModelId };
      if (follower.current) { void follower.current[method](payload, captured); }
      else if (queued.length < 50) queued.push([method, payload, captured]);
    };
    for (const [event, method] of [['chat:tool_start', 'toolStarted'], ['chat:tool_end', 'toolEnded']]) {
      void tauriEvents.listen(event, message => dispatch(method, message.payload)).then(stop => {
        if (disposed) stop(); else unlisten.push(stop);
      }).catch(error => console.warn('[megacube] switch handoff listener unavailable', error));
    }
    unlisten.push(listenManualModelSelection(payload => dispatch('manualSelection', payload)));
    void tauriEvents.listen('session:manual_model_selection', message =>
      dispatch('manualSelection', { sessionId: message.payload?.session_id }))
      .then(stop => { if (disposed) stop(); else unlisten.push(stop); })
      .catch(error => console.warn('[megacube] manual selection listener unavailable', error));
    const initialize = async () => {
      // The host key prevents WebKit/localStorage sharing between profiles or
      // different daemon endpoints from applying another profile's handoff.
      const service = await invokeTauri('cms_service_status');
      if (disposed) return;
      const key = /^[a-f0-9]{64}$/.test(service?.recoveryScope || '')
        ? `pinvou.cmsd.switch-follow.v1.${service.recoveryScope}` : null;
      const loadPending = () => {
        try {
          if (!key) return [];
          const values = [];
          const completed = new Set();
          for (let index = 0; index < localStorage.length; index++) {
            const name = localStorage.key(index);
            if (name?.startsWith(key + '.done.')) completed.add(name.slice((key + '.done.').length));
            else if (name?.startsWith(key + '.')) {
              try { values.push(JSON.parse(localStorage.getItem(name))); } catch { /* Preserve other valid receipts. */ }
            }
          }
          return mergeHandoffRecovery(values, service.switchFollowRecovery || [], completed)
            .sort((a, b) => a.started - b.started).slice(-50);
        }
        catch { return []; }
      };
      const savePending = (values, completed) => {
        try {
          if (!key) return;
          for (const value of values) localStorage.setItem(key + '.' + value.task.id, JSON.stringify(value));
          for (const id of completed) {
            localStorage.removeItem(key + '.' + id);
            localStorage.setItem(key + '.done.' + id, String(Date.now()));
          }
        }
        catch (error) { console.warn('[megacube] switch handoff persistence unavailable', error); }
      };
      follower.current = createCmsdSwitchFollow({
      getState: () => bridge.state?.getMany?.(['sessions', 'chat', 'models']) || latest.current.snapshot,
      queryModels: fetchCmsModels, queryStatus: fetchCmsStatus,
      queryTask: async taskId => {
        const result = await fetchCmsTask(taskId);
        if (!disposed && result?.available === true && result.data?.id === taskId
          && result.data?.kind === 'switch' && result.data?.status === 'succeeded') {
          // A completed CMS task can precede the store DB's desktop refresh.
          // Wait for a read begun after this success, including if startup's
          // earlier scan is still in flight. Failure keeps the receipt pending
          // without consuming the follower's readiness/registration retries.
          await reconcileRoutes.current(result.data);
        }
        return result;
      },
      listModels: () => invokeTauri('list_models'),
      sessionModelId: sessionId => invokeTauri('get_session_model_id', { sessionId }),
      saveModel: model => bridge.models.saveModel(model),
      switchModel: async (sessionId, modelId, options) => {
        await bridge.models.switchModel(sessionId, modelId, options);
        await bridge.models.loadModels();
      },
      listCards: () => invokeTauri('model_store_cards', { testMode: false }),
      loadPending, savePending,
      registrationScope: service.recoveryScope,
      onTaskRecovered: task => taskWatchRef.current?.observeRecoveredTask(task),
      notify: (kind, name) => {
        const { copy: text, onNotice: notice } = latest.current;
        notice(kind === 'failed' ? text.linkAutoSelectFailed
          : kind === 'linked' ? text.linkAutoSelected(name) : text.switchAutoSelected(name));
      },
      });
      for (const [method, payload, snapshotAtStart] of queued) dispatch(method, payload, snapshotAtStart);
      queued.length = 0;
      recover();
    };
    const start = () => { void initialize().catch(error => {
      console.warn('[megacube] switch handoff initialization failed', error);
      if (!disposed) retryTimer = setTimeout(start, 5000);
    }); };
    start();
    return () => {
      disposed = true;
      clearTimeout(retryTimer);
      clearInterval(recoveryTimer);
      for (const stop of unlisten) stop();
      follower.current?.dispose(); follower.current = null;
    };
  }, [bridge]);
  const taskWatch = useCmsdTaskWatch({
    onTaskObserved: task => {
      if (task.status === 'succeeded') void reconcileRoutes.current(task).then(() => follower.current?.observe(task)).catch(error => console.warn('[megacube] route reconciliation pending', error));
      else follower.current?.observe(task);
    },
    onTaskSettled: task => { void reconcileRoutes.current(task).then(() => follower.current?.settled(task)).catch(error => console.warn('[megacube] route reconciliation pending', error)); },
  });
  taskWatchRef.current = taskWatch;
  return taskWatch;
}
