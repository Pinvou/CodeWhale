import React, { useEffect, useRef, useState } from 'react';
import { adminLayoutCopy, accountLayoutCopy } from '../../shared/i18n.js';
import { localizedAccountPlatformError } from './error-localization.js';
import { PortalDialog } from './PortalDialog.jsx';

function UserDetails({ client, userID, copy, language, route }) {
  const [profile, setProfile] = useState(null), [memberships, setMemberships] = useState(null), [error, setError] = useState(null), [membershipError, setMembershipError] = useState(null), [revision, setRevision] = useState(0);
  const words = adminLayoutCopy[language];
  useEffect(() => {
    const controller = new AbortController(); setProfile(null); setMemberships(null); setError(null); setMembershipError(null);
    void client.userDetail(userID, controller.signal).then(user => { if (!controller.signal.aborted) setProfile(user); }).catch(reason => { if (!controller.signal.aborted) setError(reason); });
    // Organizations can be disabled independently of personal user management.
    void client.userMemberships(userID, controller.signal).then(value => { if (!controller.signal.aborted) setMemberships(value.items); }).catch(reason => { if (!controller.signal.aborted) setMembershipError(reason); });
    return () => controller.abort();
  }, [client, userID, revision]);
  const date = value => value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language]) : '—';
  return <section className="adm-panel"><button onClick={() => route.navigate('users', { user_id: null })}>{words.backToDirectory}</button>
    {error && <p role="alert">{localizedAccountPlatformError(copy, error)}<button onClick={() => setRevision(value => value + 1)}>{copy.retry}</button></p>}
    {!profile && !error && <p role="status">{copy.loading}</p>}
    {profile && <><h3>{profile.username || profile.display_name || profile.user_code}</h3><dl className="adm-user-details">{[[words.userID, profile.user_id], [copy.userManagement.exactCode, profile.user_code], [copy.accounts.status, accountLayoutCopy[language].statuses[profile.status] || profile.status], [words.createdAt, date(profile.created_at)], [words.lastSeen, date(profile.last_seen_at)]].map(([label, value]) => <React.Fragment key={label}><dt>{label}</dt><dd>{value || '—'}</dd></React.Fragment>)}</dl>
      {route.available.includes('roles') && <button onClick={() => route.navigate('roles', { role_user_id: userID, role_view: 'bindings' })}>{words.nav.roles}</button>}
      <h4>{words.memberships}</h4>
      {membershipError && <p role="alert">{localizedAccountPlatformError(copy, membershipError)}<button onClick={() => setRevision(value => value + 1)}>{copy.retry}</button></p>}
      {!memberships && !membershipError && <p role="status">{copy.loading}</p>}
      {memberships?.length === 0 && <p>{copy.noData}</p>}<ul>{memberships?.map(item => <li key={item.organization_id}>{route.available.includes('organizations') ? <button onClick={() => route.navigate('organizations', { organization_id: item.organization_id })}>{item.organization_name}</button> : item.organization_name}<span> · {item.membership_status} · {(item.roles || []).join(', ')}</span></li>)}</ul>
    </>}
  </section>;
}

export function AdminUsersWorkspace({ client, copy, language, canManage, currentUserID, route }) {
  const params = new URLSearchParams(route.search), detailID = params.get('user_id') || '';
  const [query, setQuery] = useState(params.get('user_q') || ''), [userCode, setUserCode] = useState('');
  const [page, setPage] = useState(null), [cursor, setCursor] = useState(''), [previous, setPrevious] = useState([]);
  const [error, setError] = useState(''), [busy, setBusy] = useState(false), [queryKind, setQueryKind] = useState('directory'), [confirmation, setConfirmation] = useState(null);
  const mutationBusy = useRef(false), generation = useRef(0), live = useRef(null);
  const words = adminLayoutCopy[language];
  live.current = { client, canManage, currentUserID };
  const read = async (nextCursor = cursor, search = query) => {
    const request = ++generation.current; setBusy(true); setError(''); setQueryKind('directory');
    try {
      const result = await client.users({ q: search, cursor: nextCursor });
      if (request !== generation.current) return;
      if (!Array.isArray(result?.items)) throw new Error('INVALID_USER_COLLECTION');
      setPage(result); setCursor(nextCursor);
    } catch (reason) { if (request === generation.current) setError(localizedAccountPlatformError(copy, reason)); }
    finally { if (request === generation.current) setBusy(false); }
  };
  useEffect(() => {
    const search = new URLSearchParams(route.search), value = search.get('user_q') || '';
    setQuery(value); setConfirmation(null);
    if (!detailID) void read(search.get('user_cursor') || '', value);
    return () => { generation.current++; };
  }, [client, route.search]); // the route is the directory's query/cursor authority.
  useEffect(() => () => { generation.current++; }, []);
  const load = (nextCursor = '', search = query) => {
    const current = new URLSearchParams(route.search);
    if ((current.get('user_q') || '') === search.trim() && (current.get('user_cursor') || '') === nextCursor) void read(nextCursor, search);
    else route.navigate('users', { user_q: search.trim() || null, user_cursor: nextCursor || null, user_id: null });
  };
  const lookupCode = async () => {
    if (mutationBusy.current) return;
    const request = ++generation.current; setBusy(true); setError(''); setPage(null); setPrevious([]); setCursor(''); setQueryKind('code');
    try { const item = await client.userByCode(userCode); if (request === generation.current) setPage({ items: [item] }); }
    catch { if (request === generation.current) setError(copy.userManagement.codeLookupFailed); }
    finally { if (request === generation.current) setBusy(false); }
  };
  const changeStatus = async target => {
    const current = live.current;
    if (current.client !== client || !current.canManage || target.user.user_id === current.currentUserID || mutationBusy.current || !['ACTIVE', 'DISABLED'].includes(target.status)) return;
    const mutation = {}; mutationBusy.current = mutation;
    const request = ++generation.current; setBusy(true); setError('');
    try {
      const result = await client.setUserStatus(target.user.user_id, target.status);
      if (request !== generation.current) return;
      if (result !== null) throw new Error('INVALID_USER_STATUS_ACK');
      setConfirmation(null); await read();
    } catch (reason) { if (request === generation.current) setError(localizedAccountPlatformError(copy, reason)); }
    finally { if (mutationBusy.current === mutation) mutationBusy.current = false; if (request === generation.current) setBusy(false); }
  };
  if (detailID) return <UserDetails key={detailID} {...{ client, userID: detailID, copy, language, route }} />;
  return <section className="adm-panel"><div className="adm-filter-row">
    <form onSubmit={event => { event.preventDefault(); setPrevious([]); load('', query); }}><label>{copy.search}<input value={query} onChange={event => setQuery(event.target.value)} /></label><button disabled={busy}>{copy.refresh}</button></form>
    <form onSubmit={event => { event.preventDefault(); void lookupCode(); }}><label>{copy.userManagement.exactCode}<input disabled={Boolean(busy && mutationBusy.current)} maxLength={64} value={userCode} onChange={event => { if (mutationBusy.current) return; generation.current++; setBusy(false); setPage(null); setError(''); setUserCode(event.target.value); }} placeholder={copy.userManagement.codePlaceholder} /></label><button disabled={busy || !userCode.trim()}>{copy.userManagement.lookup}</button></form>
  </div>{error && <p role="alert">{error}<button disabled={busy} onClick={() => void (queryKind === 'code' ? lookupCode() : read())}>{copy.retry}</button></p>}{busy && <p role="status">{copy.loading}</p>}
    {page && <div className="adm-table-wrap"><table className="adm-table"><thead><tr>{[copy.userManagement.account, copy.userManagement.exactCode, copy.accounts.status, copy.accounts.actions].map(label => <th key={label}>{label}</th>)}</tr></thead><tbody>{page.items.map(user => <tr key={user.user_id}><td data-label={copy.userManagement.account}>{user.status === 'DELETED' ? <span>{user.username || user.display_name || user.user_code || user.user_id}</span> : <button className="adm-name-link" onClick={() => route.navigate('users', { user_id: user.user_id })}>{user.username || user.display_name || user.user_code || user.user_id}</button>}</td><td data-label={copy.userManagement.exactCode}><code>{user.user_code || '—'}</code></td><td data-label={copy.accounts.status}>{accountLayoutCopy[language].statuses[user.status] || user.status}</td><td data-label={copy.accounts.actions}>{canManage && ['ACTIVE', 'DISABLED'].includes(user.status) && <button disabled={busy || user.user_id === currentUserID} title={user.user_id === currentUserID ? words.selfStatusBlocked : undefined} onClick={() => setConfirmation({ user, status: user.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE' })}>{user.status === 'ACTIVE' ? copy.accounts.disable : copy.accounts.enable}</button>}</td></tr>)}</tbody></table></div>}
    {page?.items.length === 0 && <p>{copy.noData}</p>}
    <nav><button disabled={busy || previous.length === 0} onClick={() => { const values = [...previous], target = values.pop(); setPrevious(values); load(target); }}>{copy.userManagement.previous}</button><button disabled={busy || !page?.next_cursor} onClick={() => { setPrevious(values => [...values, cursor]); load(page.next_cursor); }}>{copy.userManagement.next}</button></nav>
    {confirmation && canManage && confirmation.user.user_id !== currentUserID && <PortalDialog title={confirmation.status === 'DISABLED' ? copy.accounts.disable : copy.accounts.enable} closeLabel={accountLayoutCopy[language].closeDialog} busy={busy} onClose={() => setConfirmation(null)}><p>{confirmation.user.username || confirmation.user.user_code}</p>{confirmation.status === 'DISABLED' && <p>{words.disableSessionsWarning}</p>}<button disabled={busy} onClick={() => void changeStatus(confirmation)}>{words.confirmAction}</button></PortalDialog>}
  </section>;
}
