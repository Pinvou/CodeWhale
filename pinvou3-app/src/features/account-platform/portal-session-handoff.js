// Consume once. Failure cannot replay a consumed ticket; the normal login is the recovery path.
export async function consumePortalSessionHandoff(ticket, send = fetch) {
  if (typeof ticket !== 'string' || ticket.length < 16 || ticket.length > 256) throw new Error('SESSION_HANDOFF_INVALID');
  const response = await send('/portal-session-handoff/consume', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    credentials: 'same-origin', cache: 'no-store', body: JSON.stringify({ ticket }),
  });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(payload.code === 'SESSION_HANDOFF_UNAVAILABLE' ? 'SESSION_HANDOFF_UNAVAILABLE' : 'SESSION_HANDOFF_INVALID');
  if (payload.redirect_to !== '/') throw new Error('SESSION_HANDOFF_INVALID');
  return '/';
}
