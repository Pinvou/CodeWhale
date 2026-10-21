import { isLocalGatewayUrl } from '../model-store/pinvou-model-link.js';
import { hasLinkedGuidanceService } from './model-guidance-state.js';

/** Session selection takes precedence. Never copy a credential into guidance state. */
export function guidanceSelectedModel(bridge) {
  const rows = Array.isArray(bridge?.savedModels) ? bridge.savedModels : [];
  // Match the runtime: a removed session binding falls back to the global model,
  // and an invalid global id falls back to the first saved row.
  const row = rows.find(model => model?.id === bridge?.currentSessionModelId)
    || rows.find(model => model?.id === bridge?.activeModelId) || rows[0];
  if (!row) return null;
  const effective = bridge?.effectiveModelConfig;
  const overrides = Array.isArray(effective?.env_overrides) ? effective.env_overrides : [];
  return {
    id: row.id, name: row.name, model: overrides.includes('model') ? effective.model : row.model,
    base_url: overrides.includes('base_url') ? effective.base_url : row.base_url,
    // The existing settings probe resolves saved credentials, not environment overrides.
    probeBlocked: overrides.includes('api_key') || effective?.credential_state === 'env_override',
  };
}

export function guidanceSelectionKey(model) {
  return model ? JSON.stringify([model.id, model.base_url, model.model, !!model.probeBlocked]) : null;
}

export function guidanceConnectionRequest(model) {
  if (!model?.id || model.probeBlocked || !isLocalGatewayUrl(model.base_url)) return null;
  try {
    if (!['http:', 'https:'].includes(new URL(model.base_url).protocol)) return null;
    return { baseUrl: model.base_url, apiKey: '', modelId: model.id };
  } catch { return null; }
}

/** Reuse the settings connection probe and its saved credential resolution. */
export async function probeGuidanceConnection(model, { canInvoke, invoke }) {
  const key = guidanceSelectionKey(model);
  const request = guidanceConnectionRequest(model);
  if (!request || !canInvoke('test_model_connection')) return { key, result: null };
  try {
    const result = await invoke('test_model_connection', request);
    return { key, result: typeof result?.ok === 'boolean' && typeof result.code === 'string' ? {
      ok: result.ok, code: result.code, http_status: result.http_status,
    } : null };
  } catch { return { key, result: null }; }
}

/** At most two explicit known ports, in addition to the existing bounded defaults. */
export function guidanceDiscoveryRequests(model, defaultPort) {
  const validPort = port => Number.isInteger(port) && port > 0 && port < 65536;
  let selectedPort = null;
  if (isLocalGatewayUrl(model?.base_url)) {
    try { selectedPort = Number(new URL(model.base_url).port) || null; } catch { /* No target. */ }
  }
  const customPort = validPort(defaultPort) ? defaultPort : validPort(selectedPort) ? selectedPort : null;
  const requests = [{ currentBaseUrl: model?.base_url || null, customPort }];
  if (validPort(selectedPort) && selectedPort !== customPort && ![8000, 8001, 8002, 11434, 1234].includes(selectedPort)) {
    requests.push({ customPort: selectedPort });
  }
  return requests;
}

/** A running alternative is not evidence that the current chat connection works. */
export function modelGuidancePresentation(snapshot, selectedModel, savedModels = []) {
  if (['checking', 'hidden', 'starting', 'installCms'].includes(snapshot.phase)) return snapshot;
  const services = snapshot.available || [];
  const target = selectedModel && isLocalGatewayUrl(selectedModel.base_url);
  if (target) {
    const details = { selectedModelName: selectedModel.name || selectedModel.model };
    const checked = snapshot.selectedConnection?.key === guidanceSelectionKey(selectedModel)
      ? snapshot.selectedConnection.result : null;
    if (checked?.ok === false && !['client_error', 'credential_unavailable'].includes(checked.code)) {
      return { ...snapshot, ...details, phase: 'connectionUnavailable' };
    }
    if (checked?.ok === true && hasLinkedGuidanceService(services, [selectedModel])) {
      return { ...snapshot, ...details, phase: 'available', canTry: false };
    }
    const endpoint = value => String(value || '').replace(/\/+$/, '');
    const candidates = snapshot.localCandidates || [];
    const knownTarget = candidates.find(candidate => endpoint(candidate?.base_url) === endpoint(selectedModel.base_url));
    if (checked?.ok === true && knownTarget && Array.isArray(knownTarget.models)) {
      return { ...snapshot, ...details, phase: 'connectionUnavailable', connectionMismatch: true };
    }
    return { ...snapshot, ...details, phase: 'connectionUnknown', canTry: false };
  }
  if (['available', 'ready'].includes(snapshot.phase)) {
    return { ...snapshot, phase: hasLinkedGuidanceService(services, savedModels) ? 'available' : 'unlinked', canTry: false };
  }
  return snapshot;
}
