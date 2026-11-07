import React, { useEffect, useState } from 'react';
import { platformRoleName } from './platform-role-presentation.js';
import { PlatformRoleBindingIdentity } from './PlatformRoleBindingIdentity.jsx';
import { usePlatformBindingUsers } from './platform-binding-users.js';
import { usePortalDialogScope } from './portal-dialog-scope.jsx';

export function PlatformRoleBindings({ words, copy, bindings, target, blocked, onApplyTarget, canRevoke, onRevoke, onView, client, me, acceptedSessionEpoch, routeScope, active }) {
  const scopeActive = usePortalDialogScope();
  const [query, setQuery] = useState(''), [status, setStatus] = useState(''), [pageNumber, setPageNumber] = useState(1);
  const [targetDraft, setTargetDraft] = useState(target);
  useEffect(() => { setTargetDraft(target); setPageNumber(1); setQuery(''); setStatus(''); }, [target]);
  const search = query.trim().toLowerCase();
  const visible = bindings?.filter(item => (!status || item.status === status) && [item.user_id, item.role_code, platformRoleName(words, item.role_code)].some(value => value.toLowerCase().includes(search))) || [];
  const pageCount = Math.max(1, Math.ceil(visible.length / 20)), page = Math.min(pageNumber, pageCount);
  const items = visible.slice((page - 1) * 20, page * 20);
  const users = usePlatformBindingUsers({ client, me, acceptedSessionEpoch, routeScope, userIDs: items.map(item => item.user_id), active: active && scopeActive });
  const failedNames = items.some(item => users.lookup(item.user_id).status === 'unavailable');
  const date = value => value ? new Date(value).toLocaleString() : words.forever;
  return <>
    <form className="adm-role-toolbar" onSubmit={event => { event.preventDefault(); if (!blocked) onApplyTarget(targetDraft); }}>
      <label>{copy.target}<input value={targetDraft} disabled={blocked} onChange={event => setTargetDraft(event.target.value)} /></label><button disabled={blocked}>{copy.refresh}</button>
    </form>
    <div className="adm-role-toolbar">
      <label><span className="adm-visually-hidden">{words.bindingSearchPlaceholder}</span><input type="search" value={query} onChange={event => { setQuery(event.target.value); setPageNumber(1); }} placeholder={words.bindingSearchPlaceholder} /></label>
      <select aria-label={copy.status} value={status} onChange={event => { setStatus(event.target.value); setPageNumber(1); }}><option value="">{words.allStatuses}</option>{['ACTIVE', 'SCHEDULED', 'EXPIRED', 'REVOKED'].map(value => <option key={value} value={value}>{words.statuses[value]}</option>)}</select>
    </div>
    {failedNames && <p className="adm-binding-lookup-note">{words.bindingUserUnavailable} <button type="button" onClick={users.retry}>{words.reloadBindingUsers}</button></p>}
    {bindings && <>
      {bindings.truncated && <p className="adm-inline-message" role="status">{copy.bindingsTruncated}</p>}
      {/* biome-ignore lint/a11y/noNoninteractiveTabindex: This named overflow region needs keyboard focus for native horizontal table scrolling. */}
      <div className="adm-table-wrap" role="region" aria-label={words.bindingManagement} tabIndex={0}>
        <table className="adm-table adm-role-binding-table">
          <thead><tr>{[words.user, words.role, words.validity, words.source, copy.status, words.view].map((label, index) => <th key={index}>{label}</th>)}</tr></thead>
          <tbody>{items.map(item => <tr key={item.binding_id}>
            <td><PlatformRoleBindingIdentity userID={item.user_id} entry={users.lookup(item.user_id)} words={words} /></td>
            <td><strong>{platformRoleName(words, item.role_code)}</strong><small>{item.role_code}</small></td>
            <td>{words.validFrom}: {date(item.starts_at)}<small>{words.validUntil}: {item.expires_at == null ? words.noExpiry : date(item.expires_at)}</small></td>
            <td>{typeof words.sources[item.grant_source] === 'string' ? words.sources[item.grant_source] : item.grant_source}</td>
            <td><span className={`adm-status ${item.status === 'ACTIVE' ? 'ok' : item.status === 'SCHEDULED' ? 'warn' : 'neutral'}`}>{words.statuses[item.status]}</span></td>
            <td><div className="adm-row-actions"><button type="button" onClick={() => onView(item)}>{words.view}</button>{canRevoke(item) && <button type="button" className="adm-binding-danger" disabled={blocked} onClick={() => onRevoke(item)}>{copy.revoke}</button>}</div></td>
          </tr>)}</tbody>
        </table>{!items.length && <p className="adm-empty">{words.noBindings}</p>}
      </div>
      <footer className="adm-pagination"><span>{words.pageSummary(page, pageCount, visible.length)}</span><div><button type="button" disabled={page <= 1} onClick={() => setPageNumber(page - 1)}>{words.previousPage}</button><button type="button" disabled={page >= pageCount} onClick={() => setPageNumber(page + 1)}>{words.nextPage}</button></div></footer>
    </>}
  </>;
}
