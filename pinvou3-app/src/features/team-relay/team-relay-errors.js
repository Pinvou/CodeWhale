export function relayError(copy, error) {
  const text = String(error || '');
  const code = text.split(':', 1)[0];
  const message = text.replace(/ \[details=.*\]$/, '');
  let details;
  const match = text.match(/ \[details=(\{.*\})\]$/);
  if (match) {
    try { details = JSON.parse(match[1]); } catch { /* Keep the original error readable. */ }
  }
  // Older Hubs used the general action code without a structured reason.
  const enrollmentDisabled = code === 'action_not_allowed' && (
    details?.reason === 'self_service_enrollment_disabled'
    || /^action_not_allowed: Self-service (account|team) enrollment is disabled on this Hub\.(?: \(request [^)]+\))?$/.test(message)
  );
  const localized = copy.errorMessages?.[enrollmentDisabled ? 'self_service_enrollment_disabled' : code] || message;
  const blockers = details?.blocking_threads;
  if (!Array.isArray(blockers) || !blockers.length) return localized;
  try {
    return copy.blockingWork(localized, blockers.map(item => copy.blockingThread(item)));
  } catch {
    return localized;
  }
}
