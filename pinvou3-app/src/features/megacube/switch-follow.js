import { cmsdCardRowName, modelIdNeedsNameFallback, pickLinkedRowForCard } from './cms-view-model.js';
import { buildSavedModel, isLocalGatewayUrl } from '../model-store/pinvou-model-link.js';
import { isCmsCardId } from './card-id.js';

/** A switch completion is an explicit handoff, not a device-default subscription. */
export function createCmsdSwitchFollow({
  getState, queryModels, queryStatus, queryTask, listModels, sessionModelId, saveModel,
  switchModel, listCards, notify, schedule = setTimeout, cancel = clearTimeout,
  now = Date.now, loadPending = () => [], savePending = () => {},
  registrationScope = '', makeRegistrationId = cmsdRegistrationId, onTaskRecovered = () => {},
}) {
  const intents = new Map();
  const calls = new Map();
  const timers = new Set();
  let disposed = false;
  let activeIntent = null;
  let globalSelectionEpoch = 0;
  let recoveryInFlight = false;
  let recoveryCursor = 0;
  const wait = (intent, delay) => {
    intent.waiting = true;
    const timer = schedule(() => {
      timers.delete(timer); intent.waiting = false; void follow(intent);
    }, delay);
    timers.add(timer);
  };

  const persist = () => savePending([...intents.values()].filter(intent => !intent.done)
    .map(({ task, sessionId, binding, fallback, started }) => ({ task, sessionId, binding, fallback, started })),
  [...intents.values()].filter(intent => intent.done).map(intent => intent.task.id));
  const accept = value => {
    if (disposed || !value?.task?.id || !value.sessionId || intents.has(value.task.id)) return;
    value = { ...value, task: { ...value.task, requested_card: value.task.requested_card || value.task.card } };
    intents.set(value.task.id, { ...value, attempt: 0, busy: false, done: false });
    persist();
  };
  for (const value of loadPending().slice(-50)) {
    if (value?.task?.kind === 'switch' && typeof value.task.id === 'string'
      && typeof value.task.card === 'string' && typeof value.sessionId === 'string'
      && Number.isFinite(value.started) && now() - value.started < 86400000) accept(value);
  }
  const retry = intent => {
    if (disposed || intent.done) return;
    const delay = [3000, 10000, 30000][intent.attempt++];
    if (delay === undefined) {
      intent.done = true;
      persist();
      notify('failed');
      return;
    }
    wait(intent, delay);
  };
  const unchanged = async intent => {
    if (disposed || intent.done) return false;
    const state = getState() || {};
    const binding = await sessionModelId(intent.sessionId);
    const fallback = binding == null ? (await listModels())?.active_model_id : state.activeModelId;
    return !disposed && !intent.done && (binding || null) === intent.binding
      && (binding != null || (fallback || null) === intent.fallback);
  };
  const waitForIdle = intent => {
    if (disposed || intent.done) return;
    // Model loading may itself take minutes. The idle budget begins when
    // a succeeded task first meets a busy conversation, not at task acceptance.
    intent.idleWaitStarted ??= now();
    if (now() - intent.idleWaitStarted < 300000) wait(intent, 1000);
    else { intent.done = true; notify('failed'); }
  };
  const follow = async intent => {
    if (disposed || intent.done || intent.busy || intent.waiting) return;
    if (activeIntent) {
      wait(intent, 100);
      return;
    }
    activeIntent = intent;
    intent.busy = true;
    try {
      if (!await unchanged(intent) || disposed) { intent.done = true; return; }
      const state = getState() || {};
      if (state.sessionBusy?.[intent.sessionId]) {
        // Waiting for the originating tool turn must not consume health retries.
        waitForIdle(intent);
        return;
      }
      const [models, status, saved] = await Promise.all([queryModels(), queryStatus(), listModels()]);
      if (disposed) return;
      const target = models?.available && resolveSwitchTarget(models.data, intent.task.requested_card);
      const entry = target?.entry;
      if (target && intent.task.card !== intent.task.requested_card && intent.task.card !== target.runtime) { intent.done = true; return; }
      if (intent.task.selected_version && entry?.version_id !== intent.task.selected_version) throw new Error('model_version_changed');
      if (intent.task.deployment_revision && entry?.deployment_revision !== intent.task.deployment_revision) throw new Error('model_deployment_changed');
      if (!status?.available || !models?.available) throw new Error('cmsd_unavailable');
      // A later switch supersedes this one. Never route a stale completion to
      // the new default's alias, even if both tasks finish between two polls.
      if (status.data?.active_model !== target?.runtime) { intent.done = true; return; }
      if (typeof entry?.model_id !== 'string' || !entry.model_id.trim()
        || entry.status !== 'running' || entry.healthy !== true) {
        throw new Error('model_readiness_unconfirmed');
      }
      const gateway = `http://${status.data.gateway}/v1`;
      if (!isLocalGatewayUrl(gateway)) throw new Error('local_gateway_unconfirmed');
      const logicalCard = target.logical;
      const linkedCard = row => row?.model_store_card_id ? (row.model_store_link_detached ? null : row.model_store_card_id) : row?.store_card_id;
      const legacyUnambiguous = models.data.models.filter(model => model.model_id === entry.model_id).length === 1;
      const rows = (saved?.models || []).filter(row => (!row.model_store_link_detached || (!row.model_store_card_id && row.store_card_id)) && row.provider_kind !== 'coding_plan'
        && isLocalGatewayUrl(row?.base_url)
        && (!linkedCard(row) || [logicalCard, target.runtime].includes(linkedCard(row)))
        && (linkedCard(row) || legacyUnambiguous)
        && (row.model !== 'megacube-default' || linkedCard(row))
        && (linkedCard(row) || row.base_url.replace(/\/$/, '') === gateway
          || (entry.port && Number(new URL(row.base_url).port) === entry.port)));
      let row = pickLinkedRowForCard(rows, logicalCard, entry.model_id, gateway);
      const created = !row;
      if (row && [logicalCard, target.runtime].includes(linkedCard(row))) {
        const endpointMatches = row.base_url.replace(/\/$/, '') === gateway
          || (entry.port && Number(new URL(row.base_url).port) === entry.port);
        // A path-like ID needs a display-name fallback, but remains the exact
        // API identity. A device-default alias would subscribe this chat to
        // later unrelated default changes.
        const wireModel = entry.model_id;
        const aliasNeedsGateway = wireModel === 'megacube-default'
          && row.base_url.replace(/\/$/, '') !== gateway;
        if (!endpointMatches || row.model !== wireModel || aliasNeedsGateway || linkedCard(row) !== logicalCard) {
          // Keep the registration ID, display name, tuning and saved credential;
          // only align stale local connection facts for this exact card.
          row = { ...row, base_url: gateway, model: wireModel, model_store_card_id: target.fromStore ? logicalCard : null, model_store_link_requested: target.fromStore, store_card_id: logicalCard, api_key: null, credential_action: 'keep' };
          if (disposed || !await unchanged(intent)) { intent.done = true; return; }
          await saveModel(row);
        }
      }
      if (!row) {
        const fallback = modelIdNeedsNameFallback(entry.model_id);
        const cards = fallback ? await Promise.resolve().then(listCards).catch(() => []) : [];
        const id = await makeRegistrationId(registrationScope || gateway, logicalCard);
        if (saved.models.some(model => model.id === id)) throw new Error('registration_identity_conflict');
        row = buildSavedModel({
          modelName: entry.model_id,
          displayName: (fallback && cards?.find(card => card.id === logicalCard)?.name)
            || cmsdCardRowName(logicalCard, fallback ? logicalCard : entry.model_id),
          baseUrl: gateway, storeCardId: logicalCard, cardId: target.fromStore ? logicalCard : null,
          registrationId: id,
          // Store-linked rows follow runtime probes; only a user edit fixes a cap.
          contextWindow: target.fromStore ? null : (entry.max_model_len > 0 ? entry.max_model_len : null),
        });
        if (disposed || !await unchanged(intent)) { intent.done = true; return; }
        await saveModel(row);
      }
      if (disposed || !await unchanged(intent)) { intent.done = true; return; }
      // Registration and credential writes can take time. Revalidate the
      // device after those awaits, before committing a conversation choice.
      const [freshModels, freshStatus] = await Promise.all([queryModels(), queryStatus()]);
      if (disposed || intent.done) return;
      if (!freshModels?.available || !freshStatus?.available) throw new Error('cmsd_unavailable');
      if (freshStatus.data?.active_model !== target.runtime) { intent.done = true; return; }
      const freshTarget = resolveSwitchTarget(freshModels.data, intent.task.requested_card);
      const freshEntry = freshTarget?.entry;
      if (freshTarget?.runtime !== target.runtime || freshTarget?.logical !== logicalCard
        || freshEntry?.version_id !== entry.version_id || freshEntry?.deployment_revision !== entry.deployment_revision
        || freshEntry?.status !== 'running' || freshEntry?.healthy !== true
        || freshEntry.model_id !== entry.model_id || freshEntry.port !== entry.port
        || freshStatus.data?.gateway !== status.data.gateway) throw new Error('model_readiness_changed');
      if (!await unchanged(intent)) { intent.done = true; return; }
      if (await sessionModelId(intent.sessionId) !== row.id) {
        if (disposed || intent.done) return;
        await switchModel(intent.sessionId, row.id, { ifIdle: true,
          expectedModelId: intent.binding, expectedDefaultModelId: intent.fallback });
      }
      intent.done = true;
      if (await sessionModelId(intent.sessionId) !== row.id) return;
      notify(created ? 'linked' : 'selected', row.name || row.model);
    } catch (error) {
      // The backend's atomic idle gate also covers a turn that starts after
      // the UI snapshot. It must be retried rather than evicting that turn.
      if (String(error).includes('session_model_changed')) { intent.done = true; }
      else if (String(error).includes('session_turn_in_progress')) {
        waitForIdle(intent);
      } else retry(intent);
    } finally { intent.busy = false; activeIntent = null; persist(); }
  };
  return {
    accept,
    async recoverPending() {
      if (disposed || recoveryInFlight || typeof queryTask !== 'function') return;
      recoveryInFlight = true;
      try {
        // The public task index is bounded and can omit an accepted switch.
        // Query only our existing receipts, at most eight serial reads per pass;
        // rotate the batch so an unavailable old task cannot starve newer ones.
        const pending = [...intents.values()].filter(intent => (!intent.done || intent.recoveredRunning) && !intent.busy && !intent.waiting);
        if (!pending.length) return;
        const start = recoveryCursor % pending.length;
        const batch = Array.from({ length: Math.min(8, pending.length) }, (_, index) => pending[(start + index) % pending.length]);
        recoveryCursor = start + batch.length;
        for (const intent of batch) {
          if (disposed) break;
          if ((intent.done && !intent.recoveredRunning) || intent.busy || intent.waiting) continue;
          let result;
          try { result = await queryTask(intent.task.id); } catch { continue; }
          if (disposed || (intent.done && !intent.recoveredRunning) || intent.busy || intent.waiting || result?.available !== true) continue;
          const task = result.data;
          if (task?.id !== intent.task.id || !['queued', 'running', 'succeeded', 'failed', 'cancelled'].includes(task.status)
            || !bindActualTask(intent, task)) continue;
          intent.recoveredRunning = task.status === 'running';
          onTaskRecovered(task);
          // A manual choice cancels the handoff, not the accepted device work.
          // Retain only its progress observation until a definite terminal read.
          if (intent.done) continue;
          if (task.status === 'succeeded') await follow(intent);
          else if (['failed', 'cancelled'].includes(task.status)) { intent.done = true; persist(); }
        }
      } finally { recoveryInFlight = false; }
    },
    manualSelection({ sessionId } = {}) {
      if (!sessionId) globalSelectionEpoch += 1;
      for (const [id, call] of calls) {
        if (sessionId && call.sessionId === sessionId) calls.delete(id);
      }
      for (const intent of intents.values()) {
        if (sessionId ? intent.sessionId === sessionId : intent.binding == null) intent.done = true;
      }
      persist();
    },
    toolStarted(payload, snapshot) {
      if (!payload?.session_id || !/^(?:mcp_+megacube_admin_+)?cube_switch_model$/.test(payload.name || '')) return;
      const state = snapshot || getState() || {};
      let args = payload.args;
      try { if (typeof args === 'string') args = JSON.parse(args); } catch { return; }
      if (typeof args?.card !== 'string') return;
      const binding = state.activeSessionId === payload.session_id
        ? Promise.resolve(state.currentSessionModelId || null)
        : Promise.resolve().then(() => sessionModelId(payload.session_id));
      // Attach a rejection handler now; a failed tool may never produce an end.
      const context = Promise.resolve(binding).then(value => ({ binding: value || null }), () => null);
      calls.set(payload.id, { sessionId: payload.session_id, card: args.card,
        fallback: state.activeModelId || null, started: now(), context, globalSelectionEpoch });
      if (calls.size > 100) calls.delete(calls.keys().next().value);
    },
    async toolEnded(payload) {
      const call = calls.get(payload?.id);
      if (!call || call.ending) return;
      call.ending = true;
      try {
        if (!payload.success || payload.session_id !== call.sessionId || disposed) return;
        // Keep the call cancellable while a background session's binding is
        // still loading. A manual choice wins even when its value returns to A.
        const context = await call.context;
        if (disposed || calls.get(payload.id) !== call) return;
        // A global default choice only supersedes calls that depended on it.
        // Compare the event generation after the original binding resolves so
        // a background call and an A -> B -> A choice keep the same boundary.
        if (context?.binding == null && call.globalSelectionEpoch !== globalSelectionEpoch) return;
        const receipt = switchReceipt(payload.output);
        if (!context || !receipt || (receipt.requested_card && receipt.requested_card !== call.card)) return;
        accept({ task: { ...receipt, kind: 'switch', card: receipt.card || call.card, requested_card: call.card }, sessionId: call.sessionId,
          binding: context.binding, fallback: call.fallback, started: call.started });
      } finally {
        // An old ending must not consume a newly captured call with this ID.
        if (calls.get(payload.id) === call) calls.delete(payload.id);
      }
    },
    observe(task) {
      const intent = intents.get(task?.id);
      if (!intent || !bindActualTask(intent, task)) return;
      if (task.status === 'succeeded') return follow(intent);
      if (['failed', 'cancelled'].includes(task.status)) { intent.done = true; persist(); }
    },
    settled(task) {
      if (!task || task.kind !== 'switch' || task.status !== 'succeeded') return;
      const intent = intents.get(task.id);
      return intent && bindActualTask(intent, task) ? follow(intent) : undefined;
    },
    dispose() { disposed = true; calls.clear(); for (const timer of timers) cancel(timer); timers.clear(); },
  };
}

function bindActualTask(intent, task) {
  if (task.kind !== 'switch' || !isCmsCardId(task.card)
    || (task.requested_card && task.requested_card !== intent.task.requested_card)
    || (intent.task.card !== intent.task.requested_card && task.card !== intent.task.card)
    || (intent.task.selected_version && task.selected_version && task.selected_version !== intent.task.selected_version)
    || (intent.task.deployment_revision && task.deployment_revision && task.deployment_revision !== intent.task.deployment_revision)) return false;
  intent.task.card = task.card;
  intent.task.selected_version ||= task.selected_version || task.version_id;
  intent.task.deployment_revision ||= task.deployment_revision;
  return true;
}

/** Match the one current physical deployment; malformed maps never authorize a fallback. */
function resolveSwitchTarget(data, requested) {
  if (!Array.isArray(data?.models)) throw new Error('invalid_models');
  const map = data.store_bindings ?? {};
  if (!map || typeof map !== 'object' || Array.isArray(map) || Object.keys(map).length > 512) throw new Error('invalid_store_bindings');
  for (const [logical, runtime] of Object.entries(map)) {
    if (!isCmsCardId(logical) || !isCmsCardId(runtime)
      || data.models.filter(row => row.card === runtime && row.managed === true).length !== 1) throw new Error('invalid_store_bindings');
  }
  const runtime = Object.prototype.hasOwnProperty.call(map, requested) ? map[requested] : requested;
  const matches = data.models.filter(row => row.card === runtime);
  if (matches.length !== 1) throw new Error('model_selection_unconfirmed');
  const logicals = Object.keys(map).filter(logical => map[logical] === runtime);
  if (logicals.length > 1) throw new Error('model_selection_ambiguous');
  return { entry: matches[0], runtime, logical: logicals[0] || requested, fromStore: logicals.length === 1 };
}

/** Parse only structured MCP output; human prose never authorizes a handoff. */
export function switchReceipt(output, depth = 0) {
  try {
    if (depth > 4) return null;
    if (typeof output === 'string') {
      if (output.length > 262144) return null;
      output = JSON.parse(output);
    }
    const value = output?.result || output;
    if (value?.isError) return null;
    if (Array.isArray(value?.content)) {
      const text = value.content.find(item => item.type === 'text')?.text;
      return typeof text === 'string' && text.length < 262144 ? switchReceipt(JSON.parse(text), depth + 1) : null;
    }
    const data = value?.data;
    if (data?.error || !isCmsCardId(data?.task_id)) return null;
    if (['card','requested_card','selected_version','version_id','deployment_revision'].some(key => data[key] != null && !isCmsCardId(data[key]))) return null;
    if (data.version_id && data.selected_version && data.version_id !== data.selected_version) return null;
    return { id: data.task_id, card: data.card, requested_card: data.requested_card,
      selected_version: data.selected_version || data.version_id, deployment_revision: data.deployment_revision };
  } catch { return null; }
}
export function switchTaskId(output) { return switchReceipt(output)?.id || null; }

/** Stable per-profile/endpoint/card IDs make concurrent windows upsert one row. */
export async function cmsdRegistrationId(scope, card) {
  const bytes = new TextEncoder().encode(`${scope}\0${card}`);
  const hash = new Uint8Array(await globalThis.crypto.subtle.digest('SHA-256', bytes));
  return `m_cmsd_${[...hash].map(value => value.toString(16).padStart(2, '0')).join('').slice(0, 32)}`;
}
