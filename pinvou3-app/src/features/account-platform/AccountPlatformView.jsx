import { requirePersonalAccount } from './personal-contract.js';
import React, { useEffect, useRef, useState } from 'react';
import { supportedLocales, localeNames } from '../../shared/supported-locales.js';
import { hasPermission } from './authorization-ui.js';
import { formatBalance } from './balance-view.js';
import { accountDisplayLabel } from './account-display-label.js';
import { localizedAccountPlatformError } from './error-localization.js';
import { ProfileSettings } from './ProfileSettings.jsx';
import { AccountLogin } from './AccountLogin.jsx';
import './account-platform.css';

export function AccountPlatformView({ client, copy, language, onLanguageChange, portal = false, onClose, onAuthenticated }) {
  const [auth, setAuth] = useState('loading');
  const [me, setMe] = useState(null);
  const [profileOwner, setProfileOwner] = useState(client);
  const [balance, setBalance] = useState(null);
  const [error, setError] = useState('');
  const [notice, setNotice] = useState('');
  const [busy, setBusy] = useState(false);
  const epoch = useRef(0);
  const ready = useRef(false);
  ready.current = auth === 'ready' && profileOwner === client;
  const refreshRef = useRef(null);

  const load = async (generation, signal) => {
    const session = await client.session(signal);
    if (generation !== epoch.current) return;
    if (!session.authenticated) { setAuth('guest'); return; }
    const profile = requirePersonalAccount(await client.bootstrap(signal));
    if (generation !== epoch.current) return;
    setMe(profile); setProfileOwner(client);
    setAuth('ready');
    if (!hasPermission(profile, 'personal.billing.read')) return profile;
    try {
      const result = await client.balance(signal);
      if (generation === epoch.current) setBalance(result);
    } catch (reason) {
      if (generation === epoch.current && reason?.name !== 'AbortError') setError(localizedAccountPlatformError(copy, reason));
    }
    return generation === epoch.current ? profile : undefined;
  };
  const refresh = async (explicit = true) => {
    const generation = ++epoch.current;
    setBusy(true); if (explicit) setError(''); setBalance(null);
    try { return await load(generation); }
    catch (reason) { if (generation === epoch.current) { setError(localizedAccountPlatformError(copy, reason)); setAuth('error'); } }
    finally { if (generation === epoch.current) setBusy(false); }
  };
  refreshRef.current = refresh;
  useEffect(() => {
    const synchronize = () => { if (ready.current) void refreshRef.current(false); };
    const unsubscribeContext = client.onContextVersion?.(synchronize);
    const unsubscribeAuthorization = client.onAuthorizationVersion?.(synchronize);
    globalThis.addEventListener?.('focus', synchronize);
    return () => { unsubscribeContext?.(); unsubscribeAuthorization?.(); globalThis.removeEventListener?.('focus', synchronize); };
  }, [client]);
  useEffect(() => {
    const controller = new AbortController();
    const generation = ++epoch.current;
    setProfileOwner(client); setAuth('loading'); setMe(null); setBalance(null); setNotice(''); setError('');
    void load(generation, controller.signal).catch(reason => {
      if (generation === epoch.current && reason?.name !== 'AbortError') { setAuth('error'); setError(localizedAccountPlatformError(copy, reason)); }
    });
    return () => { epoch.current++; controller.abort(); };
    // Identity ownership changes with the transport, not translated labels.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client]);
  const logout = async () => {
    const generation = ++epoch.current;
    setProfileOwner(client); setAuth('loading'); setMe(null); setBalance(null); setNotice(''); setError(''); setBusy(true);
    try { await client.logout(); if (generation !== epoch.current) return false; setAuth('guest'); return true; }
    catch (reason) { if (generation === epoch.current) { setAuth('error'); setError(localizedAccountPlatformError(copy, reason)); } }
    finally { if (generation === epoch.current) setBusy(false); }
  };
  const changed = async (result, generation) => {
    if (generation !== epoch.current) return;
    if (result?.status !== 'COMPLETED') { setNotice(copy.profileChangeRecovering); return; }
    if (portal) { if (!await logout()) return; globalThis.location.assign('/auth/login?return_to=%2F'); }
    else { await logout(); }
  };
  if (auth === 'loading' || profileOwner !== client) return <div className="ap-shell ap-centered" role="status">{copy.loading}</div>;
  if (auth !== 'ready') return <div className="ap-shell ap-login">
    {error && <div className="ap-alert error" role="alert">{error}</div>}
    {auth === 'error' && <button disabled={busy} onClick={() => void refresh()}>{copy.retry}</button>}
    {portal ? <a href="/auth/login?return_to=%2F">{copy.login}</a> : <AccountLogin key={auth} client={client} copy={copy} onAuthenticated={async () => { const profile = await refresh(); if (profile) onAuthenticated?.(profile); }} />}
    {onClose && <button onClick={onClose}>{copy.close}</button>}
  </div>;
  const renderedEpoch = epoch.current;
  return <main className={`ap-shell ap-app ${portal ? 'ap-portal' : 'ap-desktop'}`}>
    <header><h1>{copy.title}</h1><select aria-label={copy.language} value={language} onChange={event => onLanguageChange(event.target.value)}>{supportedLocales.map(locale => <option key={locale} value={locale}>{localeNames[locale]}</option>)}</select><button disabled={busy} onClick={() => void refresh()}>{copy.refresh}</button><button disabled={busy} onClick={logout}>{copy.logout}</button></header>
    {error && <div className="ap-alert error" role="alert">{error}</div>}
    {notice && <div className="ap-alert" role="status">{notice}</div>}
    <section><h2>{accountDisplayLabel(me, copy.user)}</h2><dl><dt>{copy.username}</dt><dd>{me.username || '—'}</dd><dt>{copy.mobile}</dt><dd>{me.mobile || '—'}</dd><dt>{copy.accountStatus}</dt><dd>{me.status}</dd></dl></section>
    {hasPermission(me, 'personal.billing.read') && <section className="ap-balance-hero"><span>{copy.availableCredits}</span><strong>{formatBalance(balance?.available_microcredits, language)}</strong><small>{copy.reservedCredits}: {formatBalance(balance?.reserved_microcredits, language)}</small></section>}
    {hasPermission(me, 'personal.profile.update') && (portal ? <ProfileSettings key={me.user_id} copy={copy} me={me} client={client} onError={reason => { if (renderedEpoch === epoch.current) setError(localizedAccountPlatformError(copy, reason)); }} onChanged={result => changed(result, renderedEpoch)} onRecovering={() => { if (renderedEpoch === epoch.current) setNotice(copy.profileChangeRecovering); }} /> : <button onClick={() => client.openPortal(language).catch(reason => { if (renderedEpoch === epoch.current) setError(localizedAccountPlatformError(copy, reason)); })}>{copy.profileSettings}</button>)}
    {portal && hasPermission(me, 'personal.security.manage') && <a href="/auth/login?reauthenticate=1&return_to=%2F">{copy.reauthenticate}</a>}
    {portal && me.platform_permissions?.some(permission => permission.startsWith('platform.')) && <a href="/admin.html">{copy.adminCenter}</a>}
    {onClose && <button onClick={onClose}>{copy.close}</button>}
  </main>;
}
