import React, { useEffect, useLayoutEffect, useRef, useState } from 'react';
import { PortalDialog } from './PortalDialog.jsx';
import { requireFinanceID } from './finance-contract.js';
import { looksLikeUserCode, platformRoleName } from './platform-role-presentation.js';

export function PlatformRoleGrantDialog({ words, copy, client, owner, canSearchUsers, roles, target, roleCode, starts, expires, reason, update, busy, error, confirmation, onReview, onConfirm, onClose }) {
  const [step, setStep] = useState(1), [query, setQuery] = useState(''), [users, setUsers] = useState([]);
  const [searching, setSearching] = useState(false), [searchError, setSearchError] = useState('');
  const [searched, setSearched] = useState(false);
  const generation = useRef(0), request = useRef(null), authority = useRef(null);
  const heading = useRef(null);
  // The button that advanced the wizard is removed. Keep focus inside the
  // dialog so Escape and keyboard navigation continue on the next step.
  useLayoutEffect(() => { heading.current?.focus(); }, [step]);
  authority.current = { owner, client, canSearchUsers, epoch: client.financeSessionEpoch || 0 };
  useEffect(() => {
    generation.current++; request.current?.abort(); setUsers([]); setSearching(false); setSearchError(''); setSearched(false);
    return () => { generation.current++; request.current?.abort(); };
  }, [owner, client, canSearchUsers]);
  const search = async event => {
    event.preventDefault(); if (!canSearchUsers || busy || searching) return;
    request.current?.abort(); const controller = new AbortController(); request.current = controller;
    const token = { ...authority.current, generation: ++generation.current };
    const current = () => generation.current === token.generation && authority.current.owner === token.owner && authority.current.client === token.client && authority.current.canSearchUsers && (client.financeSessionEpoch || 0) === token.epoch;
    setSearching(true); setSearchError(''); setSearched(false);
    try {
      const value = query.trim();
      const result = looksLikeUserCode(value) ? { items: [await client.userByCode(value, controller.signal)] } : await client.users(value, controller.signal);
      if (!current()) return;
      if (!Array.isArray(result?.items)) throw Error('INVALID_USER_LIST');
      setUsers(result.items.filter(user => { try { requireFinanceID(user.user_id); return true; } catch { return false; } }));
      setSearched(true);
    } catch (failure) { if (current() && failure.name !== 'AbortError') { setUsers([]); setSearchError(words.usersLoadFailed); } }
    finally { if (current()) setSearching(false); }
  };
  const selectedUser = users.find(user => user.user_id === target);
  const userLabel = user => [user?.display_name, user?.username, user?.user_code, user?.user_id].find(value => typeof value === 'string' && value) || '';
  const continueFromUser = () => {
    try { requireFinanceID(target.trim().toLowerCase()); setStep(2); setSearchError(''); }
    catch { setSearchError(copy.failed); }
  };
  const command = confirmation?.body;
  const date = value => value ? new Date(value).toLocaleString() : words.forever;
  return <PortalDialog title={words.newGrant} closeLabel={words.cancel} onClose={onClose} busy={busy} drawer>
    <div className="adm-role-grant">
      <ol className="adm-role-steps">{[words.stepUser, words.stepRole, words.stepConfirm].map((label, index) => <li key={label} className={step === index + 1 ? 'active' : step > index + 1 ? 'done' : ''}><span>{index + 1}</span>{label}</li>)}</ol>
      {(error || searchError) && <p className="adm-inline-message error" role="alert">{error || searchError}</p>}
      {step === 1 && <section><h3 ref={heading} tabIndex={-1}>{words.chooseUser}</h3><p>{words.chooseUserLead}</p>
        {canSearchUsers && <><form className="adm-role-search" onSubmit={search}><label><span>{words.user}</span><input type="search" value={query} onChange={event => setQuery(event.target.value)} placeholder={words.userSearchPlaceholder} disabled={busy} /></label><button disabled={busy} aria-disabled={searching}>{searching ? copy.loading : words.searchAction}</button></form>
          {users.length > 0 && <label className="adm-role-field"><span>{words.chooseUser}</span><select disabled={busy} value={selectedUser ? target : ''} onChange={event => update('target', event.target.value)}><option value="">{words.chooseUser}</option>{users.map(user => <option key={user.user_id} value={user.user_id}>{userLabel(user)}{typeof user.user_code === 'string' && user.user_code !== userLabel(user) ? ` · ${user.user_code}` : ''}</option>)}</select></label>}{searched && users.length === 0 && <p role="status">{words.noUsers}</p>}<small className="adm-role-help">{words.noEmailSearch}</small></>}
        <label className="adm-role-field"><span>{copy.target}</span><input value={target} disabled={busy} onChange={event => update('target', event.target.value)} /></label>
        <div className="adm-role-reference"><span>{words.scope}</span><span className="adm-role-scope platform">{words.scopes.PLATFORM}</span></div>
        <div className="adm-role-drawer-foot"><button type="button" onClick={onClose}>{words.cancel}</button><button type="button" className="adm-role-primary" disabled={busy || !target.trim()} onClick={continueFromUser}>{words.next}</button></div>
      </section>}
      {step === 2 && <form onSubmit={event => { event.preventDefault(); if (onReview()) setStep(3); }}><h3 ref={heading} tabIndex={-1}>{words.chooseRoleAndValidity}</h3><p>{words.chooseRoleLead}</p>
        <label className="adm-role-field"><span>{copy.role}</span><select required value={roleCode} disabled={busy} onChange={event => update('roleCode', event.target.value)}><option value="">{words.chooseRole}</option>{roles.map(role => <option key={role.code} value={role.code}>{platformRoleName(words, role.code)}</option>)}</select></label>
        <div className="adm-role-field-row"><label className="adm-role-field"><span>{copy.starts}</span><input type="datetime-local" value={starts} disabled={busy} onChange={event => update('starts', event.target.value)} /></label><label className="adm-role-field"><span>{copy.expires}</span><input type="datetime-local" value={expires} disabled={busy} onChange={event => update('expires', event.target.value)} /></label></div>
        <label className="adm-role-field"><span>{copy.reason}</span><textarea value={reason} maxLength={500} placeholder={words.reasonPlaceholder} disabled={busy} onChange={event => update('reason', event.target.value)} /></label>{roleCode === 'PLATFORM_AUDIT_PII_EXPORT' && <p className="adm-role-risk-note">{copy.pii}</p>}
        <div className="adm-role-drawer-foot"><button type="button" disabled={busy} onClick={() => setStep(1)}>{words.previous}</button><button className="adm-role-primary" disabled={busy || !roleCode}>{words.next}</button></div>
      </form>}
      {step === 3 && command && <section><h3 ref={heading} tabIndex={-1}>{words.confirmGrant}</h3><p>{words.confirmGrantLead}</p><dl className="adm-role-preview">
        <div><dt>{words.user}</dt><dd>{userLabel(selectedUser) || command.user_id}<code>{command.user_id}</code></dd></div>
        <div><dt>{words.role}</dt><dd>{platformRoleName(words, command.role_code)}<code>{command.role_code}</code></dd></div>
        <div><dt>{words.permissionCount}</dt><dd>{roles.find(role => role.code === command.role_code)?.permissions.length ?? '—'}</dd></div>
        <div><dt>{words.scope}</dt><dd>{words.scopes.PLATFORM}</dd></div><div><dt>{words.validity}</dt><dd>{date(command.starts_at)} → {date(command.expires_at)}</dd></div><div><dt>{copy.reason}</dt><dd>{command.reason || words.notProvided}</dd></div>
      </dl><p className="adm-role-risk-note">{words.authorizationImpact}</p><div className="adm-role-drawer-foot"><button type="button" disabled={busy} onClick={() => setStep(2)}>{words.previous}</button><button type="button" className="adm-role-primary" disabled={busy} onClick={onConfirm}>{busy ? copy.loading : words.confirmAndGrant}</button></div></section>}
    </div>
  </PortalDialog>;
}
