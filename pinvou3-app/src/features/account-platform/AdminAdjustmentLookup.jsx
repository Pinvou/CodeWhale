import React, { useEffect, useId, useRef, useState } from 'react';
import { requireFinanceCollection, requireFinanceID } from './finance-contract.js';

const optionFor = (kind, item) => {
  if (kind === 'wallet') return {
    id: requireFinanceID(item.wallet_id),
    ownerType: item.owner_type,
    code: requireFinanceID(item.owner_id),
  };
  const id = requireFinanceID(item.user_id);
  const name = [item.display_name, item.username, item.user_code].find(value => typeof value === 'string' && value.trim()) || id;
  return { id, name, code: typeof item.user_code === 'string' && item.user_code.trim() ? item.user_code : id };
};

const optionName = (kind, option, words) => {
  if (kind !== 'wallet') return option.name;
  const label = words.ownerTypes[option.ownerType];
  return typeof label === 'string' ? label : words.wallet;
};

// A lookup chooses an exact draft filter value. It never submits the list
// search, alters an applied route, or requests a related record automatically.
export function AdminAdjustmentLookup({ client, kind, label, words, value, onChange, available, onOpenRelated, onDirectoryAccess, routeSignature = '', appliedValue = '' }) {
  const inputID = useId(), groupID = useId();
  const [openScope, setOpenScope] = useState(null), [term, setTerm] = useState(''), [result, setResult] = useState(null), [selected, setSelected] = useState(null);
  const trigger = useRef(null), searchInput = useRef(null), active = useRef(null);
  const epoch = client.financeSessionEpoch || 0;
  const open = openScope === routeSignature;
  // Match the directory services’ Unicode whitespace trimming, including NEL.
  const query = term.replace(/^\p{White_Space}+|\p{White_Space}+$/gu, '');
  const visible = available && result?.client === client && result?.epoch === epoch && result.routeSignature === routeSignature && result.query === query ? result : null;
  const cancel = () => { active.current?.abort(); active.current = null; };
  useEffect(() => {
    setSelected(null);
  }, [client, epoch, available]);
  useEffect(() => {
    setOpenScope(null); setTerm(''); setResult(null);
    return () => cancel();
  }, [client, epoch, available, routeSignature]);
  useEffect(() => {
    setSelected(current => current?.option.id === value ? current : null);
  }, [value]);
  useEffect(() => { if (open) searchInput.current?.focus(); }, [open]);
  const reset = () => { cancel(); setTerm(''); setResult(null); };
  const close = () => { reset(); setOpenScope(null); trigger.current?.focus(); };
  const select = option => {
    if (!visible?.items.includes(option) || epoch !== (client.financeSessionEpoch || 0)) return;
    onChange(option.id); setSelected({ option, client, epoch, routeSignature }); close();
  };
  const search = async (cursor = '') => {
    if (!available || !open || !query || epoch !== (client.financeSessionEpoch || 0)) return;
    cancel();
    const controller = new AbortController();
    active.current = controller;
    const stamp = { client, epoch, query, routeSignature };
    setResult({ ...stamp, busy: true, items: cursor ? visible?.items || [] : [] });
    try {
      const page = await (kind === 'wallet' ? client.wallets({ q: query, cursor, limit: 25 }, controller.signal, { isolateRead: true }) : client.users({ q: query, cursor }, controller.signal, '', { isolateRead: true }));
      if (active.current !== controller || controller.signal.aborted || epoch !== (client.financeSessionEpoch || 0)) return;
      const combined = [...(cursor ? visible?.items || [] : []), ...requireFinanceCollection(page).map(item => optionFor(kind, item))];
      const items = [...new Map(combined.map(option => [option.id, option])).values()];
      onDirectoryAccess?.(true);
      setResult({ ...stamp, items, next: typeof page.next_cursor === 'string' && page.next_cursor !== cursor ? page.next_cursor : '' });
    } catch (error) {
      if (active.current === controller && !controller.signal.aborted && epoch === (client.financeSessionEpoch || 0)) {
        const denied = error?.status === 401 || error?.status === 403;
        // An explicit directory denial invalidates selected display data,
        // but the exact ID remains an independent adjustment filter.
        if (denied) {
          setSelected(null);
          onDirectoryAccess?.(false);
        }
        setResult({ ...stamp, items: [], error: denied ? 'forbidden' : 'error' });
      }
    } finally { if (active.current === controller) active.current = null; }
  };
  return <fieldset className="adm-adjustment-lookup">
    <legend>{label}</legend>
    <label htmlFor={inputID}>{words.exactID}<input id={inputID} aria-label={label + ': ' + words.exactID} value={value} maxLength={200} onChange={event => { reset(); setSelected(null); onChange(event.target.value); }} /></label>
    {available ? <button ref={trigger} type="button" aria-expanded={open} aria-controls={open ? groupID : undefined} onClick={() => { if (open) close(); else setOpenScope(routeSignature); }}>{kind === 'wallet' ? words.findWallet : words.findRequester}</button> : <small>{words.directoryUnavailable}</small>}
    {available && selected?.client === client && selected.epoch === epoch && selected.option.id === value && (selected.routeSignature === routeSignature || selected.option.id === appliedValue) && <div className="adm-adjustment-lookup-selection">
      <small><bdi>{optionName(kind, selected.option, words)}</bdi> · <bdi>{selected.option.code}</bdi></small>
      {onOpenRelated && <button className="adm-name-link" type="button" onClick={() => {
        if (epoch === (client.financeSessionEpoch || 0)) onOpenRelated(selected.option.id);
      }}>{kind === 'wallet' ? words.openWallet : words.openRequester}</button>}
    </div>}
    {available && open && <div id={groupID} className="adm-adjustment-lookup-results" onKeyDown={event => { if (event.key === 'Escape') { event.preventDefault(); close(); } }}>
      <label>{kind === 'wallet' ? words.walletHint : words.requesterHint}<input ref={searchInput} type="search" maxLength={200} value={term} onKeyDown={event => {
        if (event.key === 'Enter') { event.preventDefault(); if (!event.nativeEvent.isComposing && event.keyCode !== 229 && !visible?.busy) void search(); }
      }} onChange={event => {
        cancel(); setTerm(event.target.value); setResult(null);
      }} /></label>
      <button type="button" disabled={!query || Boolean(visible?.busy)} onClick={() => void search()}>{words.search}</button>
      {visible?.busy && <p role="status">{words.loading}</p>}
      {visible?.error && <p role="alert">{words[visible.error]}</p>}
      {visible && !visible.busy && !visible.error && !visible.items.length && <p role="status">{words.noMatches}</p>}
      {visible && !visible.busy && !visible.error && visible.items.length > 0 && <ul>{visible.items.map(option => <li key={option.id}><button type="button" onClick={() => select(option)}><bdi>{optionName(kind, option, words)}</bdi><small><bdi>{option.code}</bdi></small>{option.code !== option.id && <code>{option.id}</code>}</button></li>)}</ul>}
      {visible?.next && !visible.busy && <button type="button" onClick={() => void search(visible.next)}>{words.more}</button>}
    </div>}
  </fieldset>;
}
