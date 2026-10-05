function isBuiltInAccountPlatformModel(model) {
  return !!(model && (
    model.kind === 'builtin_account_platform' ||
    model.id === 'builtin_account_platform'
  ));
}

function selectableModels(models) {
  // Catalog visibility is independent of the current login/credit state.
  // Token Gateway remains authoritative when each request is prepared.
  return (models || []).filter(model => model && model.id);
}

function builtinModelCatalog(value) {
  // Match the Gateway catalog contract: existing IDs retain their wire spelling;
  // an empty default means there is no authorized selection, not a changed preference.
  const validModel = model => typeof model === 'string' && model.trim().length > 0;
  if (!value || typeof value !== 'object' || Array.isArray(value)
    || !Array.isArray(value.available_models) || value.available_models.length > 4096
    || !value.available_models.every(validModel) || typeof value.default_model !== 'string'
    || (value.default_model !== '' && (!validModel(value.default_model) || !value.available_models.includes(value.default_model)))
    || new Set(value.available_models).size !== value.available_models.length) {
    const error = new Error('account_platform_request_failed');
    error.code = 'account_platform_request_failed';
    throw error;
  }
  const result = { available_models: [...value.available_models], default_model: value.default_model };
  if (new TextEncoder().encode(JSON.stringify(result)).length > 2 * 1024 * 1024) {
    const error = new Error('account_platform_request_failed');
    error.code = 'account_platform_request_failed';
    throw error;
  }
  return result;
}

export { isBuiltInAccountPlatformModel, selectableModels, builtinModelCatalog };
