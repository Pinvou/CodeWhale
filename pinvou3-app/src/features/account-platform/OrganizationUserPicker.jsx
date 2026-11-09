import React, { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import { requireFinanceID } from './finance-contract.js';
import { organizationLabel } from './organization-labels.js';

export function OrganizationUserPicker({ client, copy, value, onChange, disabled, presentation, remembered, onRemember }) {
  const [query, setQuery] = useState(''), [page, setPage] = useState(null), [busy, setBusy] = useState(false), [error, setError] = useState(null);
  const [pageScope, setPageScope] = useState(null);
  const generation = useRef(0), pending = useRef(null);
  const pageButton = useRef(null), searchInput = useRef(null), candidateSelect = useRef(null), pageActionHadFocus = useRef(false);
  const helpID = useId(), selectionID = useId(), errorID = useId();
  const words = presentation?.words;
  useEffect(() => () => { generation.current++; pending.current?.abort(); }, [client]);
  const load = async (cursor = '', previousQuery) => {
    if (disabled || busy || words && cursor && previousQuery === undefined && !pageScope) return;
    pending.current?.abort(); const controller = new AbortController(); pending.current = controller;
    const stamp = ++generation.current, searched = previousQuery ?? (words && cursor ? pageScope.query : query.trim());
    setBusy(true);
    if (!words || error?.kind === 'unavailable' && previousQuery === undefined && !cursor) setError(null);
    try {
      const result = await client.users({ q: searched, cursor }, controller.signal, '', words ? { isolateRead: true } : undefined);
      if (!Array.isArray(result?.items)) throw new Error('INVALID_USER_COLLECTION');
      for (const item of result.items) requireFinanceID(item.user_id);
      if (stamp !== generation.current) return;
      setPage(result);
      setError(null);
      if (words) {
        setPageScope({ query: searched, cursor });
        const candidate = value && result.items.find(item => item.user_id === value);
        if (candidate) onRemember?.(candidate);
      }
    } catch (reason) {
      if (stamp !== generation.current || reason?.name === 'AbortError') return;
      const kind = reason?.status === 403 ? 'permission' : 'unavailable';
      setError({ kind, query: searched, cursor, pageAction: Boolean(cursor) || previousQuery !== undefined });
      if (words && kind === 'permission') {
        setPage(null);
        setPageScope(null);
        onRemember?.(null);
      }
    }
    finally { if (stamp === generation.current) setBusy(false); }
  };
  const userOnPage = page?.items.find(item => item.user_id === value);
  const memberFor = id => presentation?.members?.find(item => item.user_id === id);
  const label = item => {
    if (!words) return `${item.username || item.display_name || item.user_code} · ${item.user_code || item.user_id}`;
    const member = memberFor(item.user_id);
    const roles = member?.roles?.length ? member.roles.map(role => organizationLabel(copy, 'Role', role)).join(', ') : words.unknownRole;
    return [...new Set([item.display_name || item.username || item.user_code || item.user_id, item.username, item.user_code || item.user_id, roles].filter(Boolean))].join(' · ');
  };
  const selected = words ? userOnPage || (remembered?.user_id === value ? remembered : null) : userOnPage;
  const member = value && memberFor(value);
  const available = selected?.status === 'ACTIVE' && member?.status === 'ACTIVE';
  const memberInactive = member?.status && member.status !== 'ACTIVE';
  const knownInactive = selected?.status && selected.status !== 'ACTIVE' || memberInactive;
  const memberRoles = member?.roles?.length ? member.roles.map(role => organizationLabel(copy, 'Role', role)).join(', ') : '';
  const selectionFact = !selected && member?.status ? memberInactive ? words?.memberInactiveFact : words?.memberActiveFact :
    available ? words?.activeFact : knownInactive ? words?.inactiveFact : words?.unconfirmedFact;
  const options = words && (error || busy) ? [] : page?.items || [];
  const emptyContinuation = words && !error && page?.items.length === 0 && pageScope?.cursor;
  const pageAction = words && (error ? error.kind === 'unavailable' && error.pageAction && { label: words.retryPage, cursor: error.cursor, query: error.query } :
    page?.next_cursor && pageScope ? { label: copy.next, cursor: page.next_cursor, query: pageScope.query } :
    emptyContinuation ? { label: words.firstPage, cursor: '', query: pageScope.query } : null);
  useLayoutEffect(() => {
    if (busy || !pageActionHadFocus.current) return;
    pageActionHadFocus.current = false;
    const doc = searchInput.current?.ownerDocument;
    const active = doc?.activeElement;
    if (!pageButton.current && doc && (!active?.isConnected || active === doc.body)) {
      const target = !error && page?.items.length ? candidateSelect.current : searchInput.current;
      if (!target?.disabled) target?.focus();
    }
  }, [busy, error, page]);
  const resultStatus = busy ? copy.loading : error ? '' : !page ? words?.notSearched : page.items.length === 0 ? emptyContinuation ? words?.noMore : words?.noResults : [words?.pageScope, pageScope?.query].filter(Boolean).join(' · ');
  const search = <input ref={searchInput} aria-label={words?.searchLabel || copy.search} aria-describedby={words ? error ? helpID + ' ' + errorID : helpID : undefined} placeholder={words?.searchPlaceholder} disabled={disabled || busy} value={query} onChange={event => setQuery(event.target.value)} onKeyDown={event => { if (event.key === 'Enter' && !event.isComposing && event.keyCode !== 229) { event.preventDefault(); void load(); } }} />;
  const choose = <select ref={candidateSelect} aria-label={words?.selectLabel || copy.user} aria-describedby={words && value ? selectionID : undefined} value={value} disabled={disabled || busy} onChange={event => { const next = event.target.value; onChange(next, words ? options.find(item => item.user_id === next) : undefined); }}>
    <option value="">{words?.choose || '—'}</option>{value && !options.some(item => item.user_id === value) && <option value={value}>{selected ? label(selected) : value}</option>}{options.map(item => <option key={item.user_id} value={item.user_id}>{label(item)}</option>)}
  </select>;
  return <div className={`adm-user-picker${words ? ' adm-owner-picker' : ''}`}>
    {words && <small id={helpID}>{words.searchScope}</small>}
    <div className="adm-filter-row">{words ? <label>{words.searchLabel}{search}</label> : search}<button type="button" disabled={disabled || busy} onClick={() => void load()}>{copy.search}</button></div>
    {words ? <label>{words.selectLabel}{choose}</label> : choose}
    {words ? <small className="adm-owner-picker-result" role="status">{resultStatus}</small> : busy && <span role="status">{copy.loading}</span>}
    {error && <span id={words ? errorID : undefined} role="alert">{words && error.kind === 'permission' ? words.denied : copy.unavailable}</span>}
    {!words && page?.items.length === 0 && <span>{copy.empty}</span>}
    {words && value && <div id={selectionID} className="adm-owner-picker-selection" role="status"><strong>{selected ? label(selected) : value}</strong>{!selected && memberRoles && <small>{words.memberRoleLabel}: {memberRoles}</small>}<small>{selectionFact}</small></div>}
    {!words && page?.next_cursor && <button type="button" disabled={disabled || busy} onClick={() => void load(page.next_cursor)}>{copy.next}</button>}
    {pageAction && <button type="button" ref={pageButton} disabled={disabled || busy && !pageActionHadFocus.current} aria-disabled={busy || undefined} onClick={event => {
      if (busy || disabled) return;
      pageActionHadFocus.current = event.currentTarget.ownerDocument.activeElement === event.currentTarget;
      void load(pageAction.cursor, pageAction.query);
    }}>{pageAction.label}</button>}
  </div>;
}
