import React, { useEffect, useRef, useState } from 'react';
import { requireFinanceID } from './finance-contract.js';

export function OrganizationUserPicker({ client, copy, value, onChange, disabled }) {
  const [query, setQuery] = useState(''), [page, setPage] = useState(null), [busy, setBusy] = useState(false), [error, setError] = useState(false);
  const generation = useRef(0), pending = useRef(null);
  useEffect(() => () => { generation.current++; pending.current?.abort(); }, [client]);
  const load = async (cursor = '') => {
    if (disabled || busy) return;
    pending.current?.abort(); const controller = new AbortController(); pending.current = controller;
    const stamp = ++generation.current; setBusy(true); setError(false);
    try {
      const result = await client.users({ q: query.trim(), cursor }, controller.signal);
      if (!Array.isArray(result?.items)) throw new Error('INVALID_USER_COLLECTION');
      for (const item of result.items) requireFinanceID(item.user_id);
      if (stamp === generation.current) setPage(result);
    } catch (reason) { if (stamp === generation.current && reason.name !== 'AbortError') setError(true); }
    finally { if (stamp === generation.current) setBusy(false); }
  };
  return <div className="adm-user-picker"><div className="adm-filter-row"><input aria-label={copy.search} disabled={disabled || busy} value={query} onChange={event => setQuery(event.target.value)} onKeyDown={event => { if (event.key === 'Enter' && !event.isComposing && event.keyCode !== 229) { event.preventDefault(); void load(); } }} /><button type="button" disabled={disabled || busy} onClick={() => void load()}>{copy.search}</button></div>
    <select aria-label={copy.user} value={value} disabled={disabled || busy} onChange={event => onChange(event.target.value)}><option value="">—</option>{value && !page?.items.some(item => item.user_id === value) && <option value={value}>{value}</option>}{(page?.items || []).map(item => <option key={item.user_id} value={item.user_id}>{item.username || item.display_name || item.user_code} · {item.user_code || item.user_id}</option>)}</select>
    {busy && <span role="status">{copy.loading}</span>}{error && <span role="alert">{copy.unavailable}</span>}{page?.items.length === 0 && <span>{copy.empty}</span>}{page?.next_cursor && <button type="button" disabled={disabled || busy} onClick={() => void load(page.next_cursor)}>{copy.next}</button>}
  </div>;
}
