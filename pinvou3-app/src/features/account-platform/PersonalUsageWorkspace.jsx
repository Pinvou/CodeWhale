import React, { useEffect, useRef, useState } from 'react';
import { accountPlatformCopy, modelUsageCopy } from '../../shared/i18n.js';
import { hasPermission } from './authorization-ui.js';
import { secureRequestID } from './request-id.js';
import { Usage } from './Usage.jsx';

function requirePage(value) {
  if (!value || !Array.isArray(value.items) || (value.next_cursor != null && typeof value.next_cursor !== 'string')) throw new Error('INVALID_USAGE_PAGE');
  return value;
}

export function PersonalUsageWorkspace({ client, me, language }) {
  const copy = { ...accountPlatformCopy[language], ...modelUsageCopy[language] };
  const [records, setRecords] = useState([]);
  const [cursors, setCursors] = useState(['']);
  const [page, setPage] = useState(1);
  const [next, setNext] = useState(null);
  const [busy, setBusy] = useState(false);
  const [unavailable, setUnavailable] = useState(false);
  const generation = useRef(0);
  const cancelIntents = useRef(new Map());
  const organizational = me.active_context?.context_type === 'ORGANIZATION';
  const owner = `${me.user_id}:${me.active_context?.context_version || 0}:${me.active_context?.organization_id || ''}:${me.active_context?.personal_authorization_version || 0}:${me.active_context?.organization_authorization_version || 0}`;
  const canRead = hasPermission(me, organizational ? 'org.usage.read.self' : 'personal.usage.read');
  const canCancel = hasPermission(me, 'personal.usage.cancel');
  const load = async (cursor, targetPage, stamp, signal) => {
    const result = requirePage(await client.usageHistory(cursor, signal));
    if (stamp !== generation.current) return;
    setRecords(result.items); setNext(result.next_cursor || null); setPage(targetPage); setUnavailable(false);
    setCursors(current => [...current.slice(0, targetPage - 1), cursor]);
  };
  useEffect(() => {
    const controller = new AbortController();
    const stamp = ++generation.current;
    cancelIntents.current.clear(); setRecords([]); setCursors(['']); setPage(1); setNext(null); setUnavailable(false); setBusy(canRead);
    if (canRead) void load('', 1, stamp, controller.signal).catch(error => {
      if (stamp === generation.current && error.name !== 'AbortError') setUnavailable(true);
    }).finally(() => { if (stamp === generation.current) setBusy(false); });
    return () => { generation.current++; controller.abort(); };
    // All reads and pending cancellation intents belong to this identity/version.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, owner, canRead]);
  const run = async action => {
    if (busy) return;
    const stamp = ++generation.current; setBusy(true);
    try { await action(stamp); }
    catch (error) { if (stamp === generation.current && error.name !== 'AbortError') setUnavailable(true); }
    finally { if (stamp === generation.current) setBusy(false); }
  };
  const cancel = id => run(async stamp => {
    if (!canCancel) return;
    const key = cancelIntents.current.get(id) || `usage-cancel-${secureRequestID()}`;
    cancelIntents.current.set(id, key);
    try {
      await client.cancelUsage(id, key);
      if (stamp !== generation.current) return;
      cancelIntents.current.delete(id);
      await load(cursors[page - 1], page, stamp);
    } catch (error) {
      // Unknown ACK retains the exact cancellation identity for a retry.
      if (stamp === generation.current && Number.isInteger(error.status) && error.status < 500) cancelIntents.current.delete(id);
      throw error;
    }
  });
  if (!canRead) return null;
  return <section className="ap-personal-usage">
    <button disabled={busy} onClick={() => void run(stamp => load('', 1, stamp))}>{copy.refresh}</button>
    {busy && <p role="status">{copy.loading}</p>}
    {unavailable && records.length > 0 && <p role="alert">{copy.usageUnavailable}</p>}
    <Usage copy={copy} language={language} records={records} unavailable={unavailable && records.length === 0} page={page} hasPrevious={page > 1} hasMore={Boolean(next)} busy={busy} canCancel={canCancel}
      onPrevious={() => void run(stamp => load(cursors[page - 2], page - 1, stamp))}
      onNext={() => void run(stamp => load(next, page + 1, stamp))} onCancel={id => void cancel(id)} />
  </section>;
}
