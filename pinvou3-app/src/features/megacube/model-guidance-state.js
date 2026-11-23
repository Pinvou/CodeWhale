import { isLocalGatewayUrl } from '../model-store/pinvou-model-link.js';
import { isCmsCardId } from './card-id.js';

/** Setup history is UI preference/evidence, never model ownership or readiness. */
export function guidanceStorageKey(scope) {
  return /^[a-f0-9]{64}$/.test(scope || '') ? `pinvou.megacube.guidance.v1.${scope}` : null;
}

export function readGuidanceHistory(storage, key) {
  const unavailable = { known: false, seenService: false, skippedSetup: false, lastDefault: null };
  if (!key) return unavailable;
  try {
    const read = name => {
      const value = storage.getItem(`${key}.${name}`);
      if (value == null) return false;
      if (!/^\d{1,16}$/.test(value) || Number(value) <= 0) throw new Error('invalid_guidance_history');
      return true;
    };
    const raw = storage.getItem(`${key}.default`);
    let lastDefault = null;
    if (raw != null) {
      if (raw.length > 4096) throw new Error('invalid_guidance_history');
      lastDefault = JSON.parse(raw);
      if (!isCmsCardId(lastDefault?.card)
        || typeof lastDefault.modelName !== 'string' || lastDefault.modelName.length > 512
        || !Number.isFinite(lastDefault.observedAt) || lastDefault.observedAt <= 0) throw new Error('invalid_guidance_history');
    }
    return { known: true, seenService: read('seen'), skippedSetup: read('skipped'), used: read('used'), lastDefault };
  } catch { return unavailable; }
}

export function rememberGuidanceDefault(storage, key, card, modelName, observedAt = Date.now()) {
  if (!key || !isCmsCardId(card) || !Number.isFinite(observedAt) || observedAt <= 0) return false;
  try {
    const previous = readGuidanceHistory(storage, key).lastDefault;
    if (previous?.observedAt > observedAt) return true;
    const name = String(modelName || card).slice(0, 512);
    if (previous?.card === card && previous.modelName === name) return true;
    storage.setItem(`${key}.default`, JSON.stringify({ card, modelName: name, observedAt }));
    return true;
  } catch { return false; }
}

export function rememberGuidance(storage, key, event, now = Date.now()) {
  if (!key || !['seen', 'skipped', 'used'].includes(event) || !Number.isFinite(now) || now <= 0) return false;
  try {
    const previous = storage.getItem(`${key}.${event}`);
    if (/^\d{1,16}$/.test(previous || '') && Number(previous) > 0) return true;
    storage.setItem(`${key}.${event}`, String(now)); return true;
  } catch { return false; }
}

/** Only actual endpoint discovery counts; processes, default pointers and downloaded models do not. */
export function availableLocalServices(discovery) {
  return (Array.isArray(discovery?.candidates) ? discovery.candidates : []).filter(candidate =>
    candidate && ['ready', 'busy'].includes(candidate.status) && isLocalGatewayUrl(candidate.base_url) && Array.isArray(candidate.models)
    && candidate.models.some(model => model && typeof model.id === 'string' && model.id && model.loaded === true));
}

export function hasLinkedGuidanceService(services, savedModels) {
  const rows = Array.isArray(savedModels) ? savedModels : [];
  return services.some(service => rows.some(row =>
    row && typeof row.base_url === 'string' && row.base_url.replace(/\/+$/, '') === service.base_url.replace(/\/+$/, '')
    && service.models.some(model => model && model.loaded === true && (model.id === row.model || row.model === 'megacube-default'))));
}

export function modelGuidanceState(observation, history, isCube = false) {
  const { device, service, status, models, tasks, discovery } = observation;
  const cube = isCube || (device?.supported === true
    && (device.detected === true || device.hardware?.state === 'compatible'));
  const validCard = value => isCmsCardId(value) ? value : null;
  const currentDefault = validCard(status?.data?.active_model) || validCard(models?.data?.active_model);
  const card = currentDefault || history.lastDefault?.card;
  const rows = Array.isArray(models?.data?.models) ? models.data.models : [];
  const row = rows.find(model => model?.card === card);
  const available = availableLocalServices(discovery);
  const activeTask = (Array.isArray(tasks?.data?.tasks) ? tasks.data.tasks : []).find(task => task && ['queued', 'running', 'cancelling'].includes(task.status)
    && ['enable', 'switch'].includes(task.kind));
  const queried = status?.available === true && models?.available === true && Array.isArray(models.data?.models)
    && tasks?.available === true && Array.isArray(tasks.data?.tasks);
  const result = { card, historicalDefault: !currentDefault && !!history.lastDefault,
    modelName: typeof row?.model_id === 'string' ? row.model_id : history.lastDefault?.modelName || card,
    canTry: !!card && queried && (!row || row.managed === true) && !activeTask,
    ownershipRequired: !!row && row.managed !== true,
    missingDefault: !!card && queried && !row, activeTask, available, historyKnown: history.known };
  if (!cube) return { ...result, phase: device ? 'hidden' : 'checking', canTry: false };
  if (activeTask) return { ...result, phase: 'starting', canTry: false };
  // Installation absence must be verified by the component controller, not
  // inferred from a missing socket, an offline daemon or a failed query.
  if (service?.supported === true && service.installed === false && service.canInstall === true) {
    return { ...result, phase: 'installCms', canTry: false };
  }
  if (available.length) return { ...result, phase: 'available', canTry: false };
  const hardwareMismatch = ['mismatch', 'incompatible'].includes(device?.hardware?.state);
  if (hardwareMismatch) return { ...result, phase: 'unknown', canTry: false };
  if (device?.supported !== true || !queried || !Array.isArray(discovery?.candidates) || !history.known || observation.existingUse === null) {
    return { ...result, phase: 'unknown', canTry: false };
  }
  return { ...result, phase: 'recover' };
}

/** Explicit start only. Fresh facts must agree; never fall back to a card script or adopt an external model. */
export async function tryStartGuidanceDefault(expectedCard, { queryStatus, queryModels, queryTasks, start }) {
  if (!isCmsCardId(expectedCard)) throw new Error('guidance_default_changed');
  const [status, models, tasks] = await Promise.all([queryStatus(), queryModels(), queryTasks()]);
  if (!status?.available || !models?.available || !tasks?.available
    || !Array.isArray(models.data?.models) || !Array.isArray(tasks.data?.tasks)) throw new Error('guidance_unconfirmed');
  if ((status.data?.active_model || models.data?.active_model || expectedCard) !== expectedCard) throw new Error('guidance_default_changed');
  const row = models.data.models.find(model => model?.card === expectedCard);
  if (row && row.managed !== true) throw new Error('guidance_ownership_required');
  const running = tasks.data.tasks.find(task => task && ['queued', 'running', 'cancelling'].includes(task.status)
    && ['enable', 'switch'].includes(task.kind));
  if (running) return { task_id: running.id, already_running: true };
  if (!row) throw new Error('[unknown_card] guidance_default_missing');
  if (row.weights_complete === false || row.deployed === false) throw new Error('guidance_files_missing');
  if (row?.status === 'running' && row.healthy === true) return { already_ready: true };
  const reply = await start(expectedCard);
  if (reply?.error) {
    if (typeof reply.error === 'string') throw new Error(reply.error);
    throw new Error(`[${reply.error.code || 'rejected'}] ${reply.error.message || JSON.stringify(reply.error)}`);
  }
  if (!reply?.task_id) throw new Error('guidance_result_unconfirmed');
  return reply;
}
