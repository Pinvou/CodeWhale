import React, { useEffect, useRef, useState } from 'react';
import { adminLayoutCopy, accountLayoutCopy, organizationCopy } from '../../shared/i18n.js';
import { localizedAccountPlatformError } from './error-localization.js';
import { PortalDialog } from './PortalDialog.jsx';
import { AdminIdentifier } from './AdminIdentifier.jsx';
import './admin-user-management.css';

const userDisplayName = user => user.username || user.display_name || user.user_code || user.user_id;

function UserDetails({ client, userID, copy, language, route, currentUserID, acceptedSessionEpoch, acceptedPlatformAuthorizationVersion }) {
  const [profile, setProfile] = useState(null), [memberships, setMemberships] = useState(null), [error, setError] = useState(null), [membershipError, setMembershipError] = useState(null), [revision, setRevision] = useState(0);
  const words = adminLayoutCopy[language], organizationWords = organizationCopy[language];
  useEffect(() => {
    const controller = new AbortController(); setProfile(null); setMemberships(null); setError(null); setMembershipError(null);
    void client.userDetail(userID, controller.signal).then(user => { if (!controller.signal.aborted) setProfile(user); }).catch(reason => { if (!controller.signal.aborted) setError(reason); });
    // Organizations can be disabled independently of personal user management.
    void client.userMemberships(userID, controller.signal).then(value => { if (!controller.signal.aborted) setMemberships(value.items); }).catch(reason => { if (!controller.signal.aborted) setMembershipError(reason); });
    return () => controller.abort();
  }, [client, userID, revision]);
  const identifierSessionEpoch = acceptedSessionEpoch === undefined ? client.financeSessionEpoch || 0 : acceptedSessionEpoch;
  const identifierPlatformVersion = acceptedPlatformAuthorizationVersion || 0;
  const identifierOwner = `${currentUserID}:${identifierPlatformVersion}:${identifierSessionEpoch}`;
  const date = value => value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language]) : '—';
  return <section className="adm-panel"><header className="adm-user-detail-head">
    <button type="button" onClick={() => route.navigate('users', { user_id: null })}>{words.backToUserList}</button>
    {profile && <div className="adm-user-profile-identity"><h3>{userDisplayName(profile)}</h3><span className="adm-user-profile-status">{accountLayoutCopy[language].statuses[profile.status] || profile.status}</span></div>}
  </header>
    {error && <p role="alert">{localizedAccountPlatformError(copy, error)}<button onClick={() => setRevision(value => value + 1)}>{copy.retry}</button></p>}
    {!profile && !error && <p role="status">{copy.loading}</p>}
    {profile && <><dl className="adm-user-details adm-user-profile-facts">
      <div><dt>{words.userID}</dt><dd><p className="adm-user-identifier-help">{words.userIdentifierHelp.id}</p><AdminIdentifier client={client} owner={identifierOwner} sessionEpoch={identifierSessionEpoch} platformAuthorizationVersion={identifierPlatformVersion} routeScope={globalThis.location?.href || route.search} value={profile.user_id} label={words.userID} language={language} /></dd></div>
      <div><dt>{copy.userManagement.exactCode}</dt><dd><p className="adm-user-identifier-help">{words.userIdentifierHelp.code}</p><AdminIdentifier client={client} owner={identifierOwner} sessionEpoch={identifierSessionEpoch} platformAuthorizationVersion={identifierPlatformVersion} routeScope={globalThis.location?.href || route.search} value={profile.user_code} label={copy.userManagement.exactCode} language={language} /></dd></div>
      {[[words.createdAt, date(profile.created_at)], [words.lastSeen, date(profile.last_seen_at)]].map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}
    </dl>
      {route.available.includes('roles') && <button onClick={() => route.navigate('roles', { role_user_id: userID, role_view: 'bindings' })}>{words.viewUserPlatformGrants}</button>}
      <h4>{words.memberships}</h4>
      {membershipError && <p role="alert">{localizedAccountPlatformError(copy, membershipError)}<button onClick={() => setRevision(value => value + 1)}>{copy.retry}</button></p>}
      {!memberships && !membershipError && <p role="status">{copy.loading}</p>}
      {memberships?.length === 0 && <p>{copy.noData}</p>}<ul className="adm-user-memberships">{memberships?.map(item => <li key={item.organization_id}>
        {route.available.includes('organizations') ? <button className="adm-name-link" onClick={() => route.navigate('organizations', { organization_id: item.organization_id })}>{item.organization_name}</button> : item.organization_name}
        <dl className="adm-user-membership-facts">
          <div><dt>{words.userMembershipStatus}</dt><dd><span className="adm-user-membership-label">{organizationWords[`enumStatus_${item.membership_status}`] || item.membership_status}</span></dd></div>
          <div><dt>{organizationWords.roles}</dt><dd>{item.roles?.length ? item.roles.map(role => <span className="adm-user-membership-label" key={role}>{organizationWords[`enumRole_${role}`] || role}</span>) : '—'}</dd></div>
        </dl>
      </li>)}</ul>
    </>}
  </section>;
}

export function AdminUsersWorkspace({ client, copy, language, canManage, currentUserID, route, acceptedSessionEpoch, acceptedPlatformAuthorizationVersion }) {
  const params = new URLSearchParams(route.search), detailID = params.get('user_id') || '';
  const appliedQuery = params.get('user_q') || '', appliedCursor = params.get('user_cursor') || '';
  const [query, setQuery] = useState(params.get('user_q') || ''), [userCode, setUserCode] = useState('');
  const [page, setPage] = useState(null), [cursor, setCursor] = useState(''), [previous, setPrevious] = useState([]);
  const [error, setError] = useState(''), [busy, setBusy] = useState(false), [queryKind, setQueryKind] = useState('directory'), [confirmation, setConfirmation] = useState(null);
  const mutationBusy = useRef(false), generation = useRef(0), live = useRef(null), appliedCode = useRef('');
  const words = adminLayoutCopy[language];
  live.current = { client, canManage, currentUserID };
  const read = async (nextCursor = cursor, search = appliedQuery) => {
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
  const load = (nextCursor = '', search = appliedQuery) => {
    const current = new URLSearchParams(route.search);
    if ((current.get('user_q') || '') === search.trim() && (current.get('user_cursor') || '') === nextCursor) void read(nextCursor, search);
    else route.navigate('users', { user_q: search.trim() || null, user_cursor: nextCursor || null, user_id: null });
  };
  const lookupCode = async (code = userCode) => {
    if (mutationBusy.current) return;
    const searchCode = code.trim(); appliedCode.current = searchCode;
    const request = ++generation.current; setBusy(true); setError(''); setPage(null); setPrevious([]); setCursor(''); setQueryKind('code');
    try { const item = await client.userByCode(searchCode); if (request === generation.current) setPage({ items: [item] }); }
    catch { if (request === generation.current) setError(copy.userManagement.codeLookupFailed); }
    finally { if (request === generation.current) setBusy(false); }
  };
  const refreshResults = () => {
    if (busy || mutationBusy.current) return;
    void (queryKind === 'code' ? lookupCode(appliedCode.current) : read(appliedCursor, appliedQuery));
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
  if (detailID) return <UserDetails key={detailID} {...{ client, userID: detailID, copy, language, route, currentUserID, acceptedSessionEpoch, acceptedPlatformAuthorizationVersion }} />;
  return <section className="adm-panel"><div className="adm-user-search-toolbar">
    <form data-user-directory-search onSubmit={event => { event.preventDefault(); if (mutationBusy.current) return; setPrevious([]); load('', query.trim()); }}><label>{copy.search}<input disabled={Boolean(busy && mutationBusy.current)} value={query} onChange={event => setQuery(event.target.value)} /></label><button type="submit" disabled={busy}>{words.userSearch.submit}</button></form>
    <form data-user-code-search onSubmit={event => { event.preventDefault(); void lookupCode(); }}><label>{copy.userManagement.exactCode}<input disabled={Boolean(busy && mutationBusy.current)} maxLength={64} value={userCode} onChange={event => { if (mutationBusy.current) return; generation.current++; setBusy(false); setPage(null); setError(''); setUserCode(event.target.value); }} placeholder={copy.userManagement.codePlaceholder} /></label><button type="submit" disabled={busy || !userCode.trim()}>{copy.userManagement.lookup}</button></form>
    <button type="button" data-user-results-refresh disabled={busy || !page} onClick={refreshResults}>{words.userSearch.refresh}</button>
  </div><p className="adm-user-search-help">{words.userSearch.refreshHelp}</p>{error && <p role="alert">{error}<button disabled={busy} onClick={() => void (queryKind === 'code' ? lookupCode(appliedCode.current) : read(appliedCursor, appliedQuery))}>{copy.retry}</button></p>}{busy && <p role="status">{copy.loading}</p>}
    {page && <div className="adm-table-wrap"><table className="adm-table"><thead><tr>{[copy.userManagement.account, copy.userManagement.exactCode, copy.accounts.status, copy.accounts.actions].map(label => <th key={label}>{label}</th>)}</tr></thead><tbody>{page.items.map(user => {
      const label = userDisplayName(user), self = user.user_id === currentUserID;
      const action = user.status === 'ACTIVE' ? copy.accounts.disable : copy.accounts.enable;
      return <tr key={user.user_id}>
        <td data-label={copy.userManagement.account}>{user.status === 'DELETED' ? <span>{label}</span> : <button className="adm-name-link" onClick={() => route.navigate('users', { user_id: user.user_id })}>{label}</button>}</td>
        <td data-label={copy.userManagement.exactCode}><code>{user.user_code || '—'}</code></td>
        <td data-label={copy.accounts.status}>{accountLayoutCopy[language].statuses[user.status] || user.status}</td>
        <td data-label={copy.accounts.actions}>{canManage && ['ACTIVE', 'DISABLED'].includes(user.status) && <>
          <button type="button" className={user.status === 'ACTIVE' ? 'adm-user-disable-action' : undefined} data-user-status-target={user.user_id}
            aria-label={`${words.userStatusTitle(action)}: ${label} (${user.user_code || user.user_id})`}
            disabled={busy || self} title={self ? words.selfStatusBlocked : undefined}
            onClick={() => setConfirmation({ user, status: user.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE' })}>{action}</button>
          {self && <small className="adm-user-status-blocked">{words.selfStatusBlocked}</small>}
        </>}</td>
      </tr>;
    })}</tbody></table></div>}
    {page?.items.length === 0 && <p>{copy.noData}</p>}
    <nav><button disabled={busy || previous.length === 0} onClick={() => { const values = [...previous], target = values.pop(); setPrevious(values); load(target); }}>{copy.userManagement.previous}</button><button disabled={busy || !page?.next_cursor} onClick={() => { setPrevious(values => [...values, cursor]); load(page.next_cursor); }}>{copy.userManagement.next}</button></nav>
    {confirmation && canManage && confirmation.user.user_id !== currentUserID && <PortalDialog title={words.userStatusTitle(confirmation.status === 'DISABLED' ? copy.accounts.disable : copy.accounts.enable)} closeLabel={accountLayoutCopy[language].closeDialog} busy={busy} onClose={() => setConfirmation(null)}>
      <dl className="adm-user-status-target">
        <dt>{copy.userManagement.account}</dt><dd>{userDisplayName(confirmation.user)}</dd>
        <dt>{copy.userManagement.exactCode}</dt><dd><code>{confirmation.user.user_code || '—'}</code></dd>
        <dt>{words.userID}</dt><dd><code>{confirmation.user.user_id}</code></dd>
      </dl>
      {confirmation.status === 'DISABLED' && <p>{words.disableSessionsWarning}</p>}
      <div className="ap-dialog-actions"><button type="button" data-user-status-confirm={confirmation.status} className={confirmation.status === 'DISABLED' ? 'adm-user-disable-action' : undefined} disabled={busy} onClick={() => void changeStatus(confirmation)}>{words.confirmUserStatus(confirmation.status === 'DISABLED' ? copy.accounts.disable : copy.accounts.enable)}</button></div>
    </PortalDialog>}
  </section>;
}
