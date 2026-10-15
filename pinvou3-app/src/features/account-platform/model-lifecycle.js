const rejectionCodes = new Set(['MODEL_UNAVAILABLE', 'MODEL_DEPRECATED']);

export function isModelLifecycleRejection(value) {
  return value?.schema_version === 1
    && rejectionCodes.has(value.code)
    && value.model_id !== '00000000-0000-0000-0000-000000000000'
    && /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(String(value.model_id || ''));
}

// Only the current turn's authoritative terminal can trigger replacement.
// Display text, older failed turns and an in-progress newer turn are excluded.
export function latestModelLifecycleRejection(events) {
  if (!Array.isArray(events)) return null;
  let start = null;
  let startIndex = -1;
  for (let index = events.length - 1; index >= 0; index--) {
    const event = events[index];
    if (event?.event === 'user_start') {
      if (!event.turn_id) return null;
      start = event; startIndex = index; break;
    }
  }
  if (!start) return null;
  for (let index = events.length - 1; index > startIndex; index--) {
    const event = events[index];
    if (event?.event === 'assistant_done' && event.turn_id === start.turn_id) {
      if (!['failed', 'send_error'].includes(String(event.status || '').toLowerCase())) return null;
      return isModelLifecycleRejection(event.model_rejection) ? event.model_rejection : null;
    }
  }
  return null;
}
