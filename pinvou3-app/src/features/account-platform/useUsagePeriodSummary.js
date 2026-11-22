import { useLayoutEffect, useRef, useState } from 'react';
import { readUsagePeriodBatch, usagePeriodQuery } from './usage-period.js';

export function useUsagePeriodSummary(client, owner, versions) {
  const epoch = versions.session_epoch;
  const [state, setState] = useState(null);
  const live = useRef(null), pending = useRef(null), ready = useRef(null);
  const revision = useRef(0), renderedRevision = revision.current;
  live.current = { client, owner, epoch };
  const authority = () => JSON.stringify([client.contextVersion || 0, client.authorizationVersions || {}]);
  const compatible = () => Object.entries(versions).every(([key, expected]) => key === 'session_epoch' || !expected || Math.max(key === 'context_version' ? client.contextVersion || 0 : 0, client.authorizationVersions?.[key] || 0) <= expected);
  const sameOwner = () => ready.current?.client === client && ready.current.owner === owner && ready.current.epoch === epoch && live.current.client === client && live.current.owner === owner && live.current.epoch === epoch && (client.financeSessionEpoch || 0) === epoch && compatible();
  useLayoutEffect(() => {
    ready.current = { client, owner, epoch };
    return () => { ready.current = null; pending.current?.controller.abort(); pending.current = null; };
  }, [client, owner, epoch]);
  const invalidate = () => {
    if (!sameOwner()) return;
    revision.current++;
    pending.current?.controller.abort(); pending.current = null; setState(null);
  };
  const reset = () => { if (revision.current === renderedRevision) invalidate(); };
  const run = async (filters, resume = false) => {
    if (!sameOwner() || pending.current || revision.current !== renderedRevision) return;
    let query;
    try { query = usagePeriodQuery(filters); }
    catch { setState({ client, owner, epoch, status: 'invalid' }); return; }
    const key = JSON.stringify(query);
    const previous = resume && state?.client === client && state.owner === owner && state.epoch === epoch && state.key === key ? state.checkpoint : undefined;
    const ticket = { controller: new AbortController(), authority: authority(), revision: ++revision.current };
    const current = () => sameOwner() && pending.current === ticket && revision.current === ticket.revision && authority() === ticket.authority;
    pending.current = ticket;
    setState({ client, owner, epoch, key, checkpoint: previous, status: 'loading', ticket });
    try {
      const checkpoint = await readUsagePeriodBatch({ readPage: (cursor, signal) => client.usageHistory(cursor, signal), query, checkpoint: previous, signal: ticket.controller.signal, isCurrent: current });
      if (current()) setState({ client, owner, epoch, key, checkpoint, status: checkpoint.cursor === null ? 'complete' : 'partial' });
    } catch (error) {
      // A failed batch never commits its partial sum. Retry starts from the last
      // complete checkpoint, so an interrupted page cannot be counted twice.
      if (current() && error.name !== 'AbortError') setState({ client, owner, epoch, key, checkpoint: previous, status: 'error', denied: error.status === 401 || error.status === 403 });
    } finally {
      if (pending.current === ticket) {
        if (!current()) setState(previous => previous?.ticket === ticket ? null : previous);
        pending.current = null;
      }
    }
  };
  const visible = sameOwner() && state?.client === client && state.owner === owner && state.epoch === epoch ? state : null;
  return { state: visible, run, reset, invalidate };
}
