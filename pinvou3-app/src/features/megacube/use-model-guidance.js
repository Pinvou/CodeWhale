import { useCallback, useEffect, useRef, useState } from 'react';
import { canInvoke, onPlatformConnectionChange } from '../../shared/platform.js';
import { invokeTauri } from '../../platform/tauri/client.js';
import { actOnCmsModel, fetchCmsDeviceStatus, fetchCmsModels, fetchCmsStatus, fetchCmsTasks } from './cms-api.js';
import { guidanceStorageKey, modelGuidanceState, readGuidanceHistory, rememberGuidance, rememberGuidanceDefault, tryStartGuidanceDefault } from './model-guidance-state.js';
import { guidanceDiscoveryRequests, guidanceSelectionKey, probeGuidanceConnection } from './model-guidance-connection.js';
import { guidanceMuteKey, muteGuidanceUntilAvailable, readGuidanceMute, reconcileGuidanceMute } from './model-guidance-mute.js';
import { CMS_INSTALL_REMINDER_CHANGED, cmsInstallReminderKey } from './cms-install-reminder.js';

export function useModelGuidance(isCube, selectedModel, savedModels, existingUse = null) {
  const [snapshot, setSnapshot] = useState({ phase: 'checking', canTry: false });
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const [dismissed, setDismissed] = useState(false);
  const [muteState, setMuteState] = useState({ key: null, muted: false });
  const state = useRef({ alive: false, generation: 0, inFlight: false, action: false, key: null, phase: 'checking', pending: null, userStarted: false, observation: {}, isCube, selectionKey: guidanceSelectionKey(selectedModel) });
  state.current.isCube = isCube;
  state.current.selectedModel = selectedModel;
  state.current.savedModels = savedModels;
  state.current.existingUse = existingUse;
  const selectedKey = guidanceSelectionKey(selectedModel);
  const muteKey = guidanceMuteKey(state.current.key, selectedModel);
  const load = useCallback(async () => {
    const context = state.current;
    if (context.inFlight) { context.reloadRequested = true; return; }
    context.inFlight = true;
    const observedAt = Date.now();
    const generation = context.generation;
    const selection = context.selectedModel;
    const stale = () => !context.alive || generation !== context.generation;
    try {
      const results = await Promise.allSettled([
        fetchCmsDeviceStatus(true), invokeTauri('cms_service_status'), fetchCmsStatus(), fetchCmsModels(), fetchCmsTasks(),
        probeGuidanceConnection(selection, { canInvoke, invoke: invokeTauri }),
      ]);
      if (stale()) return;
      const value = index => results[index].status === 'fulfilled' ? results[index].value : null;
      const observation = { device: value(0), service: value(1), existingUse: context.existingUse, status: value(2), models: value(3), tasks: value(4), discovery: null, selectedConnection: value(5) };
      const key = guidanceStorageKey(value(1)?.recoveryScope);
      if (key !== context.key) {
        context.key = key; context.pending = null; context.userStarted = false;
        setDismissed(false); setError('');
      }
      let storage;
      try { storage = localStorage; } catch { /* Storage failures are reflected in history. */ }
      let history = readGuidanceHistory(storage, key);
      if (context.existingUse === true) {
        rememberGuidance(storage, key, 'used');
        history = readGuidanceHistory(storage, key);
      }
      // Reuse the existing bounded loopback discovery. Include the known default port without scanning a range.
      const currentCard = observation.status?.data?.active_model || observation.models?.data?.active_model;
      const card = currentCard || history.lastDefault?.card;
      const models = Array.isArray(observation.models?.data?.models) ? observation.models.data.models : [];
      const port = models.find(model => model?.card === card)?.port;
      const detected = context.isCube || observation.device?.detected === true;
      if (detected && canInvoke('discover_local_vllm')) {
        try {
          const discoveries = await Promise.all(guidanceDiscoveryRequests(selection, port).map(request => invokeTauri('discover_local_vllm', { request })));
          if (discoveries.every(discovery => Array.isArray(discovery?.candidates))) {
            observation.discovery = { candidates: [...new Map(discoveries.flatMap(discovery => discovery.candidates).map(candidate => [candidate?.base_url, candidate])).values()] };
          }
        } catch { /* Unconfirmed discovery remains distinct from no candidate. */ }
      }
      if (stale()) return;
      history = readGuidanceHistory(storage, key);
      if (currentCard && observation.status?.available === true && observation.models?.available === true) {
        const name = models.find(model => model?.card === currentCard)?.model_id;
        rememberGuidanceDefault(storage, key, currentCard, typeof name === 'string' ? name : currentCard, observedAt);
        history = readGuidanceHistory(storage, key);
      }
      let next = modelGuidanceState(observation, history, context.isCube);
      next.service = observation.service;
      next.selectedConnection = observation.selectedConnection;
      next.localCandidates = observation.discovery?.candidates;
      const pendingTask = context.pending && (Array.isArray(observation.tasks?.data?.tasks) ? observation.tasks.data.tasks : []).find(task => task?.id === context.pending);
      if (context.pending && !pendingTask) {
        next = { ...next, phase: 'unknown', canTry: false };
        setError('guidance_result_unconfirmed');
      } else if (pendingTask && !['queued', 'running', 'cancelling'].includes(pendingTask.status)) {
        context.pending = null;
        if (pendingTask.status !== 'succeeded') setError(pendingTask.error ? (typeof pendingTask.error === 'string' ? pendingTask.error : JSON.stringify(pendingTask.error)) : 'guidance_not_ready');
        else if (!next.available?.length) setError('guidance_not_ready');
      }
      if (next.available?.length) {
        rememberGuidance(storage, key, 'seen');
        history = readGuidanceHistory(storage, key);
        next = { ...next, historyKnown: history.known };
      }
      const mute = reconcileGuidanceMute(storage, key, context.selectedModel, next, context.savedModels);
      setMuteState(mute);
      if (mute.available !== context.wasAvailable) setDismissed(false);
      context.wasAvailable = mute.available;
      const health = JSON.stringify([next.selectedConnection?.key, next.selectedConnection?.result?.ok]);
      if (context.connectionHealth && health !== context.connectionHealth) setDismissed(false);
      context.connectionHealth = health;
      context.phase = next.phase;
      context.observation = observation;
      next.pendingId = context.pending;
      setSnapshot(next.phase === 'available' && context.userStarted ? { ...next, phase: 'ready' } : next);
    } finally {
      context.inFlight = false;
      if (context.reloadRequested && context.alive) { context.reloadRequested = false; void load(); }
    }
  }, []);
  useEffect(() => {
    if (state.current.selectionKey === selectedKey) return;
    state.current.selectionKey = selectedKey;
    setDismissed(false); void load();
  }, [selectedKey, load]);
  useEffect(() => { void load(); }, [existingUse, load]);
  useEffect(() => {
    const context = state.current; context.alive = true;
    void load();
    const timer = setInterval(() => { void load(); }, 30000);
    const disconnect = onPlatformConnectionChange(() => {
      ++context.generation; context.key = null; context.phase = 'checking'; context.pending = null; context.userStarted = false;
      setSnapshot({ phase: 'checking', canTry: false }); setDismissed(false); setError('');
      // An in-flight request must settle before probing the new host.
      void load();
    });
    const changed = event => {
      if (!event.key?.startsWith(context.key || 'invalid:')) return;
      const target = guidanceMuteKey(context.key, context.selectedModel);
      try { setMuteState({ key: target, ...readGuidanceMute(localStorage, target) }); } catch { /* The next check reports unavailable storage. */ }
      void load();
    };
    globalThis.addEventListener('storage', changed);
    const installChanged = event => {
      if (event.detail?.key === cmsInstallReminderKey(context.observation.service)) void load();
    };
    globalThis.addEventListener(CMS_INSTALL_REMINDER_CHANGED, installChanged);
    return () => { context.alive = false; ++context.generation; clearInterval(timer); disconnect(); globalThis.removeEventListener('storage', changed); globalThis.removeEventListener(CMS_INSTALL_REMINDER_CHANGED, installChanged); };
  }, [load]);
  useEffect(() => {
    if (snapshot.phase !== 'starting' && !snapshot.pendingId) return undefined;
    const timer = setInterval(() => { void load(); }, 3000);
    return () => clearInterval(timer);
  }, [snapshot.phase, snapshot.pendingId, load]);
  const dismiss = () => {
    if (snapshot.phase === 'ready') state.current.userStarted = false;
    setDismissed(true);
  };
  const muteUntilAvailable = () => {
    const currentKey = guidanceMuteKey(state.current.key, state.current.selectedModel);
    try {
      if (!currentKey || currentKey !== muteKey || !muteGuidanceUntilAvailable(localStorage, currentKey)) throw new Error('guidance_mute_failed');
      setMuteState({ key: currentKey, known: true, muted: true });
      setError('');
    } catch { setError('guidance_mute_failed'); }
  };
  const start = async () => {
    const context = state.current;
    if (context.action || !snapshot.canTry || !canInvoke('cms_model_action')) return;
    const generation = context.generation, key = context.key;
    context.action = true; setBusy(true); setError('');
    try {
      const result = await tryStartGuidanceDefault(snapshot.card, {
        queryStatus: fetchCmsStatus, queryModels: fetchCmsModels, queryTasks: fetchCmsTasks,
        start: card => {
          if (context.generation !== generation || context.key !== key) throw new Error('guidance_unconfirmed');
          return actOnCmsModel(card, 'enable');
        },
      });
      if (!context.alive || context.generation !== generation || context.key !== key) return;
      context.userStarted = true;
      if (result.already_ready) { await load(); return; }
      context.pending = result.task_id;
      setSnapshot(value => ({ ...value, phase: 'starting', canTry: false, pendingId: result.task_id, activeTask: { id: result.task_id, status: 'running' } }));
      await load();
    } catch (reason) { if (context.alive) { setError(String(reason)); await load(); } }
    finally { context.action = false; if (context.alive) setBusy(false); }
  };
  const refresh = () => { setError(''); void load(); };
  return { snapshot, error, busy, dismissed, dismiss, start, refresh, muteUntilAvailable, muted: muteState.key === muteKey && muteState.muted };
}
