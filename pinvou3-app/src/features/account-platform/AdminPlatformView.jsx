import { AdminManagementWorkspace } from './AdminManagementWorkspace.jsx';
import { requirePersonalAccount } from './personal-contract.js';
import React, { useEffect, useRef, useState } from 'react';
import { supportedLocales, localeNames } from '../../shared/supported-locales.js';
import { SettingsWorkspace } from './SettingsWorkspace.jsx';
import { RechargeProductWorkspace } from './RechargeProductWorkspace.jsx';
import { AdminFinanceWorkspace } from './AdminFinanceWorkspace.jsx';
import { AdminReconciliationWorkspace } from './AdminReconciliationWorkspace.jsx';
import { financeCopy, reconciliationCopy, tokenManagementCopy } from '../../shared/i18n.js';
import { localizedAccountPlatformError } from './error-localization.js';
import './admin-platform.css';

function Users({ client, copy, canManage }) {
  const [query, setQuery] = useState('');
  const [page, setPage] = useState(null);
  const [previous, setPrevious] = useState([]);
  const [cursor, setCursor] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const generation = useRef(0);
  useEffect(() => () => { generation.current++; }, []);
  const load = async (nextCursor = cursor, search = query) => {
    const request = ++generation.current;
    setBusy(true); setError('');
    try {
      const result = await client.users({ q: search, cursor: nextCursor });
      if (request !== generation.current) return;
      if (!Array.isArray(result?.items)) throw new Error('INVALID_USER_COLLECTION');
      setPage(result); setCursor(nextCursor);
    } catch (reason) { if (request === generation.current) setError(localizedAccountPlatformError(copy, reason)); }
    finally { if (request === generation.current) setBusy(false); }
  };
  useEffect(() => { void load(''); return () => { generation.current++; }; }, [client]); // eslint-disable-line react-hooks/exhaustive-deps
  const changeStatus = async (user, status) => {
    if (!canManage || !['ACTIVE', 'DISABLED'].includes(status)) return;
    const request = ++generation.current;
    setBusy(true); setError('');
    try {
      const result = await client.setUserStatus(user.user_id, status);
      if (request !== generation.current) return;
      if (result !== null) throw new Error('INVALID_USER_STATUS_ACK');
      await load();
    } catch (reason) { if (request === generation.current) setError(localizedAccountPlatformError(copy, reason)); }
    finally { if (request === generation.current) setBusy(false); }
  };
  return <section className="adm-panel"><h2>{copy.nav.users}</h2><form onSubmit={event => { event.preventDefault(); setPrevious([]); void load('', query); }}><label>{copy.search}<input value={query} onChange={event => setQuery(event.target.value)} /></label><button disabled={busy}>{copy.refresh}</button></form>
    {error && <div role="alert">{error}<button disabled={busy} onClick={() => void load()}>{copy.retry}</button></div>}
    {busy && <p role="status">{copy.loading}</p>}
    {page && <table><thead><tr><th>{copy.userManagement.account}</th><th>{copy.accounts.status}</th><th>{copy.accounts.actions}</th></tr></thead><tbody>{page.items.map(user => <tr key={user.user_id}><td>{user.username || user.display_name || user.user_code || user.user_id}</td><td>{user.status}</td><td>{canManage && ['ACTIVE', 'DISABLED'].includes(user.status) && <button disabled={busy} onClick={() => void changeStatus(user, user.status === 'ACTIVE' ? 'DISABLED' : 'ACTIVE')}>{user.status === 'ACTIVE' ? copy.accounts.disable : copy.accounts.enable}</button>}</td></tr>)}</tbody></table>}
    {page?.items.length === 0 && <p>{copy.noData}</p>}
    <nav><button disabled={busy || previous.length === 0} onClick={() => { const next = [...previous]; const target = next.pop(); setPrevious(next); void load(target); }}>{copy.userManagement.previous}</button><button disabled={busy || !page?.next_cursor} onClick={() => { setPrevious(value => [...value, cursor]); void load(page.next_cursor); }}>{copy.userManagement.next}</button></nav>
  </section>;
}

function RepairQueue({ client, copy, profile = false }) {
  const [items, setItems] = useState(null);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const generation = useRef(0);
  const load = async () => {
    const request = ++generation.current;
    setBusy(true); setError('');
    try {
      const result = await (profile ? client.profileChangeRepairs() : client.registrationRepairs());
      if (request !== generation.current) return;
      if (!Array.isArray(result?.items)) throw new Error('INVALID_REPAIR_COLLECTION');
      setItems(result.items);
    } catch (reason) { if (request === generation.current) setError(localizedAccountPlatformError(copy, reason)); }
    finally { if (request === generation.current) setBusy(false); }
  };
  useEffect(() => { void load(); return () => { generation.current++; }; }, [client, profile]); // eslint-disable-line react-hooks/exhaustive-deps
  const act = async (item, compensate = false) => {
    if (!globalThis.confirm(compensate ? copy.userManagement.confirmCompensation : copy.userManagement.confirmRetry)) return;
    const request = ++generation.current;
    setBusy(true); setError('');
    try {
      if (profile) await client.retryProfileChange(item.profile_change_request_id);
      else if (compensate) await client.compensateRegistration(item.registration_operation_id);
      else await client.retryRegistration(item.registration_operation_id);
      if (request === generation.current) await load();
    } catch (reason) { if (request === generation.current) setError(localizedAccountPlatformError(copy, reason)); }
    finally { if (request === generation.current) setBusy(false); }
  };
  return <section className="adm-panel"><h2>{profile ? copy.userManagement.profileRepair : copy.userManagement.registrationRepair}</h2><p>{profile ? copy.userManagement.profileRepairLead : copy.userManagement.registrationRepairLead}</p><button disabled={busy} onClick={() => void load()}>{copy.refresh}</button>
    {error && <div role="alert">{error}</div>}{busy && <p role="status">{copy.loading}</p>}
    {items && <table><thead><tr><th>{copy.userManagement.operation}</th><th>{copy.accounts.status}</th><th>{copy.accounts.actions}</th></tr></thead><tbody>{items.map(item => { const id = profile ? item.profile_change_request_id : item.registration_operation_id; return <tr key={id}><td>{id}<small>{item.stage}</small></td><td>{item.status}</td><td><button disabled={busy} onClick={() => void act(item)}>{copy.userManagement.retry}</button>{!profile && item.compensation_allowed && <button disabled={busy} onClick={() => void act(item, true)}>{copy.userManagement.compensate}</button>}</td></tr>; })}</tbody></table>}
    {items?.length === 0 && <p>{copy.noData}</p>}
  </section>;
}

export function AdminPlatformView({ client, copy, language, onLanguageChange }) {
  const [authorization, setAuthorization] = useState(null);
  const [credentialOrigin, setCredentialOrigin] = useState('');
  const [authorizationOwner, setAuthorizationOwner] = useState(client);
  const [state, setState] = useState('loading');
  const [error, setError] = useState('');
  const [section, setSection] = useState('users');
  const generation = useRef(0);
  const load = async () => {
    const request = ++generation.current;
    setAuthorizationOwner(client); setAuthorization(null); setState('loading'); setError('');
    try {
      const session = await client.session();
      if (request !== generation.current) return;
      if (!session.authenticated) { setState('guest'); return; }
      const result = requirePersonalAccount(await client.authorization());
      if (request !== generation.current) return;
      if (!result?.user_id || !Array.isArray(result.platform_permissions)) throw new Error('INVALID_AUTHORIZATION');
      setCredentialOrigin(typeof session.credential_origin === 'string' ? session.credential_origin : '');
      setAuthorization(result); setState('ready');
    } catch (reason) { if (request === generation.current) { setError(localizedAccountPlatformError(copy, reason)); setState('error'); } }
  };
  useEffect(() => { void load(); return () => { generation.current++; }; }, [client]); // eslint-disable-line react-hooks/exhaustive-deps
  const permissions = authorization?.platform_permissions || [];
  const pages = [
    ['users', 'platform.users.read', copy.nav.users],
    ['registration', 'platform.registrations.repair', copy.userManagement.registrationRepair],
    ['profile', 'platform.profile-changes.repair', copy.userManagement.profileRepair],
    ['settings', 'platform.sms-config.read', copy.settings.title],
    ['finance', 'platform.billing.wallets.read', financeCopy[language].finance],
    ['products', 'platform.products.read', financeCopy[language].productAdmin],
    ['reconciliation', 'platform.billing.reconciliation.read', reconciliationCopy[language].title],
    ['management', ['platform.models.read', 'platform.tokens.read', 'platform.pricing.read', 'platform.usage.read', 'platform.models.lifecycle.request', 'platform.models.lifecycle.approve', 'platform.models.catalog.rollback.request', 'platform.models.catalog.rollback.approve', 'platform.tokens.credentials.revoke.request', 'platform.tokens.credentials.revoke.approve'].find(permission => permissions.includes(permission)), tokenManagementCopy[language].title],
  ].filter(([, permission]) => permissions.includes(permission));
  const visibleSection = pages.some(([key]) => key === section) ? section : pages[0]?.[0];
  const logout = async () => {
    const request = ++generation.current;
    setAuthorizationOwner(client); setAuthorization(null); setState('loading'); setError('');
    try { await client.logout(); if (request === generation.current) setState('guest'); }
    catch (reason) { if (request === generation.current) { setState('error'); setError(localizedAccountPlatformError(copy, reason)); } }
  };
  const renderedState = authorizationOwner === client ? state : 'loading';
  return <main className="adm-shell"><header><h1>{copy.title}</h1><select aria-label={copy.settings.language} value={language} onChange={event => onLanguageChange(event.target.value)}>{supportedLocales.map(locale => <option key={locale} value={locale}>{localeNames[locale]}</option>)}</select>{renderedState === 'ready' && <button onClick={logout}>{copy.logout}</button>}</header>
    {renderedState === 'loading' && <p role="status">{copy.loading}</p>}{error && <div role="alert">{error}<button onClick={load}>{copy.retry}</button></div>}
    {renderedState === 'guest' && <a href="/auth/login?return_to=%2Fadmin.html">{copy.login}</a>}
    {renderedState === 'ready' && <><nav>{pages.map(([key, , label]) => <button key={key} aria-pressed={visibleSection === key} onClick={() => setSection(key)}>{label}</button>)}</nav>{!pages.length && <p role="alert">{copy.permissionDenied}</p>}
      <div key={authorization.user_id}>{visibleSection === 'management' && <AdminManagementWorkspace client={client} copy={copy} language={language} permissions={permissions} owner={authorization.user_id} credentialOrigin={credentialOrigin} />}{visibleSection === 'reconciliation' && <AdminReconciliationWorkspace client={client} me={authorization} language={language} />}{visibleSection === 'products' && <RechargeProductWorkspace client={client} me={authorization} copy={financeCopy[language]} language={language} />}{visibleSection === 'finance' && <AdminFinanceWorkspace client={client} me={authorization} language={language} />}{visibleSection === 'users' && <Users client={client} copy={copy} canManage={permissions.includes('platform.users.status.manage')} />}{visibleSection === 'registration' && <RepairQueue client={client} copy={copy} />}{visibleSection === 'profile' && <RepairQueue key="profile" client={client} copy={copy} profile />}{visibleSection === 'settings' && <SettingsWorkspace client={client} copy={copy} language={language} onLanguageChange={onLanguageChange} permissions={permissions} />}</div>
    </>}
  </main>;
}
