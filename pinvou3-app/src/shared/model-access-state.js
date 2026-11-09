import { isBuiltInAccountPlatformModel, isLocalModel } from './model-options.js';
import { modelAccessAppliesToView } from './model-access-scope.js';
export { baseUrlIsLoopback, isLocalModel, activeModelIsLocal } from './model-options.js';
export { modelAccessAppliesToView } from './model-access-scope.js';

function hasUserCredential(model) {
  if (['missing', 'unavailable'].includes(model?.credential_state)) return false;
  return ['configured', 'env_override'].includes(model?.credential_state) || model?.has_secret === true;
}

function modelNeedsUserCredential(model) {
  return model?.credential_mode !== 'backend_managed' && model?.requires_user_api_key !== false
    && model?.credential_mode !== 'none' && !isLocalModel(model);
}

function resolveModelAccessState(bs, { accountStatus = 'unknown', refreshStatus = 'idle' } = {}) {
  const models = Array.isArray(bs?.savedModels) ? bs.savedModels : [];
  const draftSelection = bs?.pendingDraftModelRef;
  const draftModel = draftSelection?.source === 'LOCAL_CONFIG' ? models.find(model => model.id === draftSelection.saved_model_id) : null;
  const config = draftModel ? { ...draftModel, model_ref: draftSelection } : bs?.effectiveModelConfig;
  const loaded = bs?.modelsLoaded === true && Array.isArray(bs.savedModels);
  const currentSelection = bs?.pendingDraftModelRef || bs?.currentSessionModelRef || bs?.defaultModelRef;
  const selection = currentSelection || config?.model_ref;
  const platformSelection = selection?.source === 'ACCOUNT_PLATFORM' && !!selection.model_id;
  const alternatives = models.filter(model => model?.id && !isBuiltInAccountPlatformModel(model));
  const usableAlternatives = alternatives.filter(model => isLocalModel(model) || hasUserCredential(model) || model.requires_user_api_key === false);
  const base = { accountStatus, canLogin: accountStatus === 'guest', alternatives: usableAlternatives.length };
  if (!draftSelection && currentSelection && config?.model_ref && (currentSelection.source !== config.model_ref.source
    || (currentSelection.model_id || currentSelection.saved_model_id) !== (config.model_ref.model_id || config.model_ref.saved_model_id))) return { ...base, kind: refreshStatus === 'error' ? 'read_error' : 'checking' };
  if (!loaded) return { ...base, kind: refreshStatus === 'error' ? 'read_error' : 'checking' };
  if (draftSelection?.source === 'LOCAL_CONFIG' && !draftModel) return { ...base, kind: 'select_model' };
  if (platformSelection) {
    if (accountStatus === 'authenticated') return { ...base, kind: 'ready' };
    if (['unknown', 'checking'].includes(accountStatus)) return { ...base, kind: 'checking' };
    return { ...base, kind: accountStatus === 'guest' ? 'sign_in' : 'account_error' };
  }
  const carrierOnly = alternatives.length === 0 && models.some(isBuiltInAccountPlatformModel);
  const selectedConfig = config && String(config.model || '').trim()
    && !(carrierOnly && !selection);
  if (selectedConfig) {
    if (config.credential_mode === 'backend_managed') return { ...base, kind: accountStatus === 'authenticated' ? 'ready' : accountStatus === 'guest' ? 'sign_in' : 'account_error' };
    if (!modelNeedsUserCredential(config) || hasUserCredential(config)) return { ...base, kind: 'ready' };
    if (config.credential_state === 'unavailable') return { ...base, kind: 'credential_unavailable' };
    if (alternatives.length || selection) return { ...base, kind: usableAlternatives.length ? 'switch_model' : 'credential_missing' };
  }
  if (refreshStatus === 'error') return { ...base, kind: 'read_error' };
  if (!config) return { ...base, kind: refreshStatus === 'ready' ? 'read_error' : 'checking' };
  if (alternatives.length || accountStatus === 'authenticated') return { ...base, kind: 'select_model' };
  if (['unknown', 'checking'].includes(accountStatus)) return { ...base, kind: 'checking' };
  if (accountStatus === 'error') return { ...base, kind: 'account_error' };
  return { ...base, kind: 'first_use' };
}

function shouldShowModelAccessGate(bs, currentView, bridgeAvailable, isRemoteTranscriptActive = false, access) {
  return modelAccessAppliesToView(bs, currentView, bridgeAvailable, isRemoteTranscriptActive)
    && !bs?.activeSessionId && resolveModelAccessState(bs, access).kind === 'first_use';
}

export { resolveModelAccessState, shouldShowModelAccessGate };
