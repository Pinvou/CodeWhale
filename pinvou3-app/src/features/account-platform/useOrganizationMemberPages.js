import { useEffect, useRef, useState } from 'react';

const empty = (identity, api) => ({ identity, api, items: [], nextCursor: null, loading: false, error: '' });
// Member metadata is read one page at a time. It never owns a financial
// invocation, and owner or client changes retire both the cursor and pending reads.
export function useOrganizationMemberPages({ api, org, owner, enabled, quota = false, unitID = '' }) {
 const epoch = api.client.financeSessionEpoch || 0;
 const identity = `${owner}:${org}:${unitID}:${quota}:${enabled}:${epoch}`;
 const current = () => live.current.identity === identity && live.current.api === api && (api.client.financeSessionEpoch || 0) === epoch;
 const [state, setState] = useState(() => empty(identity, api));
 const generation = useRef(0), live = useRef({ identity, api }), controller = useRef(null), running = useRef(false), seen = useRef(new Set());
 live.current = { identity, api };
 const start = async (cursor = '', append = false) => {
  if (!enabled || !current() || append && running.current) return;
  controller.current?.abort(); const abort = new AbortController(); controller.current = abort;
  const stamp = ++generation.current; running.current = true;
  if (!append) seen.current = new Set();
  setState(old => ({ ...(append && old.identity === identity && old.api === api ? old : empty(identity, api)), loading: true, error: '' }));
  try {
   const query = { limit: 50, cursor, ...(unitID ? { unit_id: unitID } : {}) };
   const page = await (quota ? api.quotaMembers(org, query, abort.signal) : api.members(org, query, abort.signal));
   if (stamp !== generation.current || !current()) return;
   if (page.next_cursor !== null && (page.next_cursor === cursor || seen.current.has(page.next_cursor))) throw new Error('INVALID_ORGANIZATION_ACK');
   seen.current.add(cursor);
   setState(old => ({ identity, api, items: append ? [...new Map([...old.items, ...page.items].map(item => [item.user_id, item])).values()] : page.items, nextCursor: page.next_cursor, loading: false, error: '' }));
  } catch (reason) {
   if (stamp === generation.current && current() && reason.name !== 'AbortError') setState(old => ({ ...(reason.status === 403 ? empty(identity, api) : old), loading: false, error: reason.status === 403 ? 'permissions' : 'unavailable' }));
  } finally { if (stamp === generation.current && current()) running.current = false; }
 };
 useEffect(() => {
  if (enabled) void start(); else { generation.current++; controller.current?.abort(); running.current = false; setState(empty(identity, api)); }
  return () => { generation.current++; controller.current?.abort(); running.current = false; };
 }, [api, identity]); // eslint-disable-line react-hooks/exhaustive-deps
 const visible = state.identity === identity && state.api === api ? state : empty(identity, api);
 return { ...visible, refresh: () => start(), loadMore: () => visible.nextCursor ? start(visible.nextCursor, true) : undefined };
}
