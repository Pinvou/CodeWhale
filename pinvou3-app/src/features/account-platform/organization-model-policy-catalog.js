import { organizationID } from './organization-client.js';

export function requireOrganizationModelPolicyCatalog(catalog) {
  const invalid = () => { throw new Error('INVALID_MODEL_CATALOG'); };
  if (!catalog || !Array.isArray(catalog.items)) invalid();
  const ids = new Set();
  for (const model of catalog.items) {
    if (!model || typeof model !== 'object' || Array.isArray(model)) invalid();
    try { organizationID(model.model_id); } catch { invalid(); }
    if (ids.has(model.model_id)) invalid();
    ids.add(model.model_id);
    for (const field of ['display_name', 'upstream_model_ref']) {
      if (model[field] != null && typeof model[field] !== 'string') invalid();
    }
  }
  return catalog.items;
}
