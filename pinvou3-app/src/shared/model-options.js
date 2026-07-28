function isBuiltInLlmApiModel(model) {
  return !!(model && (model.kind === 'builtin_llmapi' || model.id === 'builtin_llmapi'));
}

function isBuiltInLlmApiAvailable(status) {
  if (!status) return false;
  const accountExists = status.backend_user_state === 'exists' ||
    (!status.backend_user_state && status.backend_user_exists === true);
  return accountExists &&
    status.enabled === true &&
    String(status.provisioning_status || '').trim().toLowerCase() === 'ready';
}

function selectableModels(models, status) {
  const allowBuiltin = isBuiltInLlmApiAvailable(status);
  return (models || []).filter(model =>
    model && model.id && (!isBuiltInLlmApiModel(model) || allowBuiltin)
  );
}

export { isBuiltInLlmApiModel, isBuiltInLlmApiAvailable, selectableModels };
