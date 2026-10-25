const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

export function registrationRepairActions(item) {
  const query = item?.status === 'IDENTITY_CREATING' && item?.stage === 'IDENTITY_CREATING'
    && Boolean(item?.identity_create_attempted_at) && item?.account_created === false;
  return {
    query,
    retry: query || item?.status === 'REPAIR_REQUIRED',
    compensate: ['REPAIR_REQUIRED', 'COMPENSATION_PENDING'].includes(item?.status) && item?.compensation_allowed === true,
  };
}

export function registrationInspectionFilters(values = {}) {
  const operationID = String(values.operation_id || '').trim();
  if (operationID && (!UUID.test(operationID) || /^0{8}-0{4}-0{4}-0{4}-0{12}$/.test(operationID))) return null;
  const result = {};
  if (operationID) result.operation_id = operationID.toLowerCase();
  for (const key of ['status', 'stage', 'responsibility', 'sla']) if (values[key]) result[key] = values[key];
  if (values.older_than_minutes !== '' && values.older_than_minutes != null) {
    const minutes = Number(values.older_than_minutes);
    if (!Number.isInteger(minutes) || minutes < 0 || minutes > 43200) return null;
    if (minutes) result.older_than_minutes = String(minutes);
  }
  return result;
}

// Operation age is display only. The existing older_than_minutes query remains
// time since updated_at; neither time changes the registration state or SLA.
export function registrationOperationAge(createdAt, now = Date.now()) {
  const created = Date.parse(createdAt);
  return Number.isFinite(created) && created <= now ? Math.floor((now - created) / 60000) : null;
}
