export const ACCOUNT_PLATFORM_SOURCE = 'ACCOUNT_PLATFORM';
export const LOCAL_CONFIG_SOURCE = 'LOCAL_CONFIG';

export function platformModelRuntimeSupported(item) {
  return ['OPENAI_CHAT_V1', 'DEEPSEEK_V4_CHAT_V1'].includes(item?.runtime_protocol);
}

export function platformModelSelectableAsDefault(item) {
  return item?.context_eligible === true && item.lifecycle_status === 'PUBLISHED' && platformModelRuntimeSupported(item);
}

export function platformModelSelectableForNewSession(item, now = Date.now()) {
  if (!item || item.context_eligible !== true || !platformModelRuntimeSupported(item)) return false;
  if (item.lifecycle_status === 'PUBLISHED') return true;
  if (item.lifecycle_status !== 'DEPRECATED' || !item.new_session_cutoff_at) return false;
  const cutoff = Date.parse(item.new_session_cutoff_at);
  return Number.isFinite(cutoff) && cutoff > now;
}

export function composerPlatformModels(items) {
  return (Array.isArray(items) ? items : [])
    .filter(item => item?.context_eligible === true && item.lifecycle_status !== 'REMOVED');
}

export function sameModelSelectionRef(left, right) {
  if (!left || !right || left.source !== right.source) return false;
  if (left.source === ACCOUNT_PLATFORM_SOURCE) return left.model_id === right.model_id;
  if (left.source === LOCAL_CONFIG_SOURCE) return left.saved_model_id === right.saved_model_id;
  return false;
}
