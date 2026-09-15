export function invitationCreateBody(target, roles) {
  const body = { roles, expires_in_hours: 72 };
  if (typeof target === 'string') {
    body.email = target;
    return body;
  }
  body.target_type = String(target?.type ?? target?.target_type ?? '');
  body.target = String(target?.value ?? target?.target ?? '');
  return body;
}

export function invitationSubmissionAttempt(current, organizationId, targetType, target, createKey) {
  const fingerprint = `${organizationId}:${targetType}:${target}`;
  return current?.fingerprint === fingerprint ? current : { fingerprint, key: createKey() };
}
