export async function refreshModelAccessAfterAuthentication(modelsBridge) {
  if (!modelsBridge) return false;

  let modelsRefreshed = false;
  try {
    modelsRefreshed = await modelsBridge.loadModels?.() === true;
  } catch {}

  // Refresh the effective config even if the catalog request failed. Either
  // call publishes its own state slice, while the combined boolean lets a
  // caller distinguish a complete refresh from a swallowed bridge failure.
  let effectiveConfigRefreshed = false;
  try {
    effectiveConfigRefreshed = await modelsBridge.loadEffectiveModelConfig?.() === true;
  } catch {}
  return modelsRefreshed && effectiveConfigRefreshed;
}
