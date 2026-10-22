import { AccountClosureWorkspace } from './AccountClosureWorkspace.jsx';
import { PersonalFinanceWorkspace } from './PersonalFinanceWorkspace.jsx';
import { PersonalUsageWorkspace } from './PersonalUsageWorkspace.jsx';
import { OrganizationDirectory } from './OrganizationDirectory.jsx';
import { OrganizationWorkspace } from './OrganizationWorkspace.jsx';
import { requirePersonalAccount } from './personal-contract.js';
import React, { useEffect, useMemo, useRef, useState } from 'react';
import { capturePersonalRechargeReauthentication, authorizePersonalRechargeReauthentication } from './personal-recharge-reauthentication.js';
import { createDesktopReauthenticationCoordinator } from './desktop-reauthentication.js';
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
  const [reauthentication, setReauthentication] = useState(null);
  const coordinatorRef = useRef(null);
  const identity = useRef(null);
  const copyRef = useRef(copy);
  copyRef.current = copy;
  identity.current = { auth, me, profileOwner, client };
  const epoch = useRef(0);
  const ready = useRef(false);
  ready.current = auth === 'ready' && profileOwner === client && !reauthentication;
  const refreshRef = useRef(null);

  const load = async (generation, signal) => {
    const session = await client.session(signal);
    if (generation !== epoch.current) return;
    if (!session.authenticated) { setAuth('guest'); return; }
    const profile = requirePersonalAccount(await client.bootstrap(signal));
    if (generation !== epoch.current) return;
    authorizePersonalRechargeReauthentication(client, profile);
    setMe(profile); setProfileOwner(client);
    setAuth('ready');
    if (profile.active_context?.context_type === 'ORGANIZATION' || !hasPermission(profile, 'personal.billing.read')) return profile;
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
    if (portal || typeof client.bindReauthentication !== 'function') return undefined;
    const coordinator = createDesktopReauthenticationCoordinator({
      client,
      getActor: () => {
        const current = identity.current;
        return current.auth === 'ready' && current.profileOwner === client && current.client === client ? current.me?.user_id : null;
      },
      onPrompt: ticket => {
        if (coordinatorRef.current !== coordinator) throw new Error('REAUTHENTICATION_EXPIRED');
        // Native login retires its old credentials before requesting a password
        // grant. Retire this view before that action can start, even on failure.
        capturePersonalRechargeReauthentication(client, ticket.actor);
        epoch.current++; ready.current = false; ticket.viewRetired = true;
        setBalance(null); setNotice(''); setError(''); setBusy(false);
        setReauthentication({ coordinator, ticket });
      },
      onVerified: (_ticket, profile) => {
        if (coordinatorRef.current !== coordinator) throw new Error('REAUTHENTICATION_EXPIRED');
        authorizePersonalRechargeReauthentication(client, profile);
        const generation = ++epoch.current;
        setMe(profile); setProfileOwner(client); setAuth('ready');
        setReauthentication(null); setError(''); setNotice(copyRef.current.desktopReauthenticationCompleted);
        // This restores the fresh actor's view only. The closure workspace reads
        // its durable intent and requires an explicit retry of the original key.
        if (profile.active_context?.context_type !== 'ORGANIZATION' && hasPermission(profile, 'personal.billing.read')) {
          void client.balance().then(value => { if (generation === epoch.current) setBalance(value); })
            .catch(reason => { if (generation === epoch.current) setError(localizedAccountPlatformError(copyRef.current, reason)); });
        }
      },
      onRejected: (ticket, reason) => {
        if (coordinatorRef.current !== coordinator) return;
        if (!ticket.viewRetired) { setError(localizedAccountPlatformError(copyRef.current, reason)); return; }
        epoch.current++; ready.current = false;
        setReauthentication(null); setMe(null); setBalance(null); setNotice(''); setBusy(false); setAuth('error');
        setError(localizedAccountPlatformError(copyRef.current, reason, {
          REAUTHENTICATION_ACTOR_MISMATCH: copyRef.current.desktopReauthenticationActorMismatch,
          REAUTHENTICATION_CANCELLED: copyRef.current.desktopReauthenticationCancelled,
        }));
      },
    });
    coordinatorRef.current = coordinator;
    const unbind = client.bindReauthentication(language => coordinator.request(language));
    return () => {
      if (coordinatorRef.current === coordinator) coordinatorRef.current = null;
      unbind(); coordinator.dispose();
    };
  }, [client, portal]);
  const reauthenticationClient = useMemo(() => {
    if (!reauthentication) return null;
    const { coordinator, ticket } = reauthentication;
    const value = Object.create(client);
    value.login = (account, password) => coordinator.authenticate(ticket, () => client.login(account, password));
    return value;
  }, [client, reauthentication]);
  useEffect(() => {
    const synchronize = () => { if (ready.current && !coordinatorRef.current?.hasPending()) void refreshRef.current(false); };
    const unsubscribeContext = client.onContextVersion?.(synchronize);
    const unsubscribeAuthorization = client.onAuthorizationVersion?.(synchronize);
    globalThis.addEventListener?.('focus', synchronize);
    return () => { unsubscribeContext?.(); unsubscribeAuthorization?.(); globalThis.removeEventListener?.('focus', synchronize); };
  }, [client]);
  useEffect(() => {
    const controller = new AbortController();
    const generation = ++epoch.current;
    setReauthentication(null); setProfileOwner(client); setAuth('loading'); setMe(null); setBalance(null); setNotice(''); setError('');
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
  if (reauthentication && reauthentication.ticket.client === client) return <div className="ap-shell ap-login">
    <AccountLogin key={reauthentication.ticket.id} client={reauthenticationClient} copy={copy} reauthentication
      onAuthenticated={() => reauthentication.coordinator.complete(reauthentication.ticket)}
      onAuthenticationFailure={reason => reauthentication.coordinator.fail(reauthentication.ticket, reason)} />
    <button type="button" onClick={() => reauthentication.coordinator.cancel(reauthentication.ticket)}>{copy.desktopReauthenticationCancel}</button>
    {onClose && <button type="button" onClick={() => { reauthentication.coordinator.cancel(reauthentication.ticket); onClose(); }}>{copy.close}</button>}
  </div>;
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
    <OrganizationDirectory key={`${me.user_id}:${client.financeSessionEpoch || 0}`} client={client} me={me} language={language} onChanged={() => refresh(false)} />
    {me.active_context?.context_type === 'ORGANIZATION' ? <OrganizationWorkspace key={`${me.user_id}:${me.active_context.organization_id}:${me.active_context.context_version}`} client={client} me={me} language={language} onChanged={() => refresh(false)} /> : <>
    {hasPermission(me, 'personal.billing.read') && <section className="ap-balance-hero"><span>{copy.availableCredits}</span><strong>{formatBalance(balance?.available_microcredits, language)}</strong><small>{copy.reservedCredits}: {formatBalance(balance?.reserved_microcredits, language)}</small></section>}
    <PersonalFinanceWorkspace key={`personal-finance:${me.user_id}`} client={client} me={me} language={language} onPaid={() => { if (renderedEpoch === epoch.current) return refresh(false); }} />
    <PersonalUsageWorkspace key={`personal-usage:${me.user_id}`} client={client} me={me} language={language} />
    <AccountClosureWorkspace key={`${me.user_id}:${me.personal_authorization_version || me.active_context?.personal_authorization_version || 0}:${client.financeSessionEpoch || 0}`} client={client} me={me} language={language} />
    </>}
    {hasPermission(me, 'personal.profile.update') && (portal ? <ProfileSettings key={me.user_id} copy={copy} me={me} client={client} onError={reason => { if (renderedEpoch === epoch.current) setError(localizedAccountPlatformError(copy, reason)); }} onChanged={result => changed(result, renderedEpoch)} onRecovering={() => { if (renderedEpoch === epoch.current) setNotice(copy.profileChangeRecovering); }} /> : <button onClick={() => client.openPortal(language).catch(reason => { if (renderedEpoch === epoch.current) setError(localizedAccountPlatformError(copy, reason)); })}>{copy.profileSettings}</button>)}
    {portal && hasPermission(me, 'personal.security.manage') && <a href="/auth/login?reauthenticate=1&return_to=%2F">{copy.reauthenticate}</a>}
    {portal && me.platform_permissions?.some(permission => permission.startsWith('platform.')) && <a href="/admin.html">{copy.adminCenter}</a>}
    {onClose && <button onClick={onClose}>{copy.close}</button>}
  </main>;
}
