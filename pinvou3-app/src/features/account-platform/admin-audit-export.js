export function newAuditExportAttempt(filter, reason, includePII, idempotencyKey) {
  const normalizedReason = String(reason || '').trim();
  if (!filter?.from || !filter?.to || !Number.isFinite(Date.parse(filter.from)) ||
    !Number.isFinite(Date.parse(filter.to)) || Date.parse(filter.to) <= Date.parse(filter.from) ||
    new TextEncoder().encode(normalizedReason).length < 10 ||
    new TextEncoder().encode(normalizedReason).length > 500 || !idempotencyKey) {
    throw Object.assign(new Error('audit export request is invalid'), { code: 'INVALID_AUDIT_EXPORT_REQUEST' });
  }
  return Object.freeze({ filter: Object.freeze({ ...filter }), reason: normalizedReason,
    includePII: Boolean(includePII), idempotencyKey });
}

export function submitAuditExportAttempt(client, attempt) {
  return client.createAuditExport(attempt.filter, attempt.reason, attempt.includePII, attempt.idempotencyKey);
}
