// These fields jointly choose the endpoint and wire protocol. Snapshot the
// initialized form values so catalog defaults do not count as user edits.
const ROUTE_FIELDS = ['preset', 'provider_kind', 'vendor', 'endpoint_mode', 'model', 'base_url'];

export function modelRouteSnapshot(model) {
  return Object.fromEntries(ROUTE_FIELDS.map((field) => [field, String(model[field] || '').trim()]));
}

export function isModelRouteUnchanged(snapshot, model) {
  const current = modelRouteSnapshot(model);
  return ROUTE_FIELDS.every((field) => snapshot[field] === current[field]);
}
