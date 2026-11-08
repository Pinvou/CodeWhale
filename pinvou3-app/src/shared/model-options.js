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

export { isBuiltInAccountPlatformModel, selectableModels };
