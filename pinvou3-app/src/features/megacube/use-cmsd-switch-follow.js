import { useCallback, useEffect, useRef } from 'react';
import { invokeTauri } from '../../platform/tauri/client.js';
import { fetchCmsModels, fetchCmsStatus, fetchCmsTask } from './cms-api.js';
import { useCmsdTaskWatch } from './use-cmsd-tasks.js';
import { mergeHandoffRecovery } from './handoff-recovery.js';
import { publishRuntimeTask, runtimeTasksFromOutput } from './runtime-task-events.js';

// Only current-fact confirmation. No operation, session choice or authority is
// inferred from a historical task; explicit current selection can supersede it.
export function runtimeTaskState(task, value) {
  if (!Array.isArray(value?.linked_runtime_cards) || !Array.isArray(value?.linked_runtime_pending)) return 'pending';
  const targets = [task.logical_card, task.requested_card, task.card].filter(Boolean);
  if (!targets.some(card => value.linked_runtime_cards.includes(card))) return 'irrelevant';
  const models = Array.isArray(value.models) ? value.models : [];
  const matches = models.filter(model => targets.includes(model.card_id) || targets.includes(model.runtime_card_id));
  if (matches.length === 1) {
    const model = matches[0], cmsd = model.cmsd;
    if (model.status === 'deployed' && model.card_update_pending !== true) {
      if (task.selected_version && model.active_version_id && task.selected_version !== model.active_version_id) return 'superseded';
      if (task.logical_card && model.card_id === task.logical_card && model.runtime_card_id
        && task.card !== task.logical_card && task.card !== model.runtime_card_id) return 'superseded';
    }
    if (cmsd?.managed === true && cmsd.status === 'running' && cmsd.healthy === true
      && task.deployment_revision && cmsd.deploymentRevision
      && task.deployment_revision !== cmsd.deploymentRevision) return 'superseded';
  }
  return targets.some(card => value.linked_runtime_pending.includes(card)) ? 'pending' : 'confirmed';
}

export function useCmsdSwitchFollow({ bridge, snapshot, copy, onNotice, eventBuffer, modelStoreAvailable = false }) {
  const latest = useRef({ snapshot, copy, onNotice });
  latest.current = { snapshot, copy, onNotice };
  const follower = useRef(null);
  const taskWatchRef = useRef(null);
  const routeRefresh = useRef(null);
  const refreshStoreRoutes = useCallback(async ({ fresh = false } = {}) => {
    if (!modelStoreAvailable) return;
    const previous = routeRefresh.current;
    if (previous) {
      const value = await previous;
      if (!fresh) return value;
    }
    if (!routeRefresh.current) {
      routeRefresh.current = invokeTauri('model_store_library')
        .then(value => { if (value?.link_sync_error) throw new Error(value.link_sync_error); return value; })
        .finally(() => { routeRefresh.current = null; });
    }
    return routeRefresh.current;
  }, [modelStoreAvailable]);
  const reconcileRoutes = useRef(null);
  // Both index observations and private details must reconcile a snapshot
  // begun after task success; this ref also follows capability changes.
  reconcileRoutes.current = async task => {
    const value = await refreshStoreRoutes({ fresh: task?.status === 'succeeded' });
    if (modelStoreAvailable && task?.status === 'succeeded' && runtimeTaskState(task, value) === 'pending') {
      throw new Error('model_runtime_unconfirmed');
    }
    return value;
  };
  useEffect(() => { if (bridge.available && modelStoreAvailable) void refreshStoreRoutes().catch(error => console.warn('[megacube] initial route reconciliation failed', error)); }, [bridge, modelStoreAvailable, refreshStoreRoutes]);
  useEffect(() => {
    if (!bridge.available) return undefined;
    let disposed = false;
    let retryTimer;
    const unlisten = [];
    // This private recovery path never feeds old tasks into the maintenance UI.
    const recover = () => { void follower.current?.recoverPending(); };
    const recoveryTimer = setInterval(recover, 3000);
    const runtimeCalls = new Map();
    const queued = [];
    const observeRuntimeTool = (method, payload) => {
      if (method === 'toolStarted' && /^(?:mcp_+megacube_admin_+)?cube_(?:start_model|get_task)$/.test(payload?.name || '')) {
        let args = payload.args;
        try { if (typeof args === 'string') args = JSON.parse(args); } catch { args = null; }
        runtimeCalls.set(payload.id, { name: payload.name, session: payload.session_id, card: args?.card });
        if (runtimeCalls.size > 100) runtimeCalls.delete(runtimeCalls.keys().next().value);
      } else if (method === 'toolEnded') {
        const call = runtimeCalls.get(payload?.id);
        runtimeCalls.delete(payload?.id);
        if (call && payload.success && payload.session_id === call.session) {
          const fallback = /cube_start_model$/.test(call.name) ? { kind: 'enable', card: call.card, requested_card: call.card } : {};
          for (const task of runtimeTasksFromOutput(payload.output, fallback)) publishRuntimeTask(task);
        }
      }
    };
    const dispatch = (method, payload, originalSnapshot) => {
      const state = originalSnapshot || bridge.state?.getMany?.(['sessions', 'models']) || latest.current.snapshot;
      const captured = { activeSessionId: state?.activeSessionId,
        currentSessionModelId: state?.currentSessionModelId, activeModelId: state?.activeModelId };
      if (follower.current) { void follower.current[method](payload, captured); }
      else if (queued.length < 50) queued.push([method, payload, captured]);
    };
    unlisten.push(eventBuffer.subscribe((method, payload, captured) => {
      if (method === 'runtimeTask') {
        taskWatchRef.current?.observeAcceptedRuntimeTask(payload);
        return;
      }
      observeRuntimeTool(method, payload);
      dispatch(method, payload, captured);
    }));
    const initialize = async () => {
      // The host key prevents WebKit/localStorage sharing between profiles or
      // different daemon endpoints from applying another profile's handoff.
      // Listeners and the bounded event queue above are installed before
      // loading the rare handoff controller, preserving event-time snapshots.
      const [service, { createCmsdSwitchFollow }] = await Promise.all([
        invokeTauri('cms_service_status'), import('./switch-follow.js'),
      ]);
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
  }, [bridge, eventBuffer]);
  const taskWatch = useCmsdTaskWatch({
    onRuntimeTasksSucceeded: async tasks => {
      const value = await refreshStoreRoutes({ fresh: true });
      const result = { confirmed: [], superseded: [], irrelevant: [] };
      for (const task of tasks) {
        const state = modelStoreAvailable ? runtimeTaskState(task, value) : 'irrelevant';
        if (state !== 'pending') result[state].push(task.id);
      }
      return result;
    },
    onTaskObserved: task => {
      if (task.status === 'succeeded') return reconcileRoutes.current(task)
        .then(() => follower.current?.observe(task)).then(() => true)
        .catch(error => { console.warn('[megacube] route reconciliation pending', error); return false; });
      else follower.current?.observe(task);
    },
    onTaskSettled: task => { void reconcileRoutes.current(task).then(() => follower.current?.settled(task)).catch(error => console.warn('[megacube] route reconciliation pending', error)); },
  });
  taskWatchRef.current = taskWatch;
  return taskWatch;
}
