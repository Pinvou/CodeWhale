import './desktop-account.css';
import React, { useEffect, useRef, useState } from 'react';
import { accountDesktopCopy, accountLayoutCopy } from '../../shared/i18n.js';
import { supportedLocales, localeNames } from '../../shared/supported-locales.js';
import { accountDisplayLabel, maskedAccountMobile } from './account-display-label.js';
import { hasPermission } from './authorization-ui.js';
import { formatBalance } from './balance-view.js';
import { localizedAccountPlatformError } from './error-localization.js';
import { OrganizationDirectory } from './OrganizationDirectory.jsx';
import { OrganizationFinanceWorkspace } from './OrganizationFinanceWorkspace.jsx';
import { PersonalFinanceWorkspace } from './PersonalFinanceWorkspace.jsx';
import { PortalDialogScope } from './portal-dialog-scope.jsx';

export function DesktopAccountCenter({ client, me, balance, copy, language, onLanguageChange, busy, error, notice, onRefresh, onLogout, onPaid, rechargeRequest }) {
  const words = accountDesktopCopy[language];
  const [section, setSection] = useState(rechargeRequest ? 'recharge' : 'profile');
  const [rechargeOpened, setRechargeOpened] = useState(Boolean(rechargeRequest));
  const [webBusy, setWebBusy] = useState(false);
  const [webError, setWebError] = useState('');
  const opening = useRef(false), generation = useRef(0);
  useEffect(() => () => { generation.current++; }, [client]);
  useEffect(() => { if (rechargeRequest) { setSection('recharge'); setRechargeOpened(true); } }, [rechargeRequest]);
  const openWeb = async () => {
    if (opening.current) return;
    const stamp = generation.current;
    opening.current = true; setWebBusy(true); setWebError('');
    try { await client.openPortal(language); }
    catch (reason) { if (stamp === generation.current) setWebError(localizedAccountPlatformError(copy, reason)); }
    finally { if (stamp === generation.current) { opening.current = false; setWebBusy(false); } }
  };
  const organization = me.active_context.context_type === 'ORGANIZATION';
  const canRecharge = organization ? hasPermission(me, 'org.entitlements.purchase') : hasPermission(me, 'personal.billing.read') && hasPermission(me, 'personal.recharge.create');
  const name = accountDisplayLabel(me, copy.user);
  return <main className="ap-shell ap-app ap-desktop" aria-busy={busy}>
    <header className="ap-center-heading"><h1>{copy.title}</h1><select aria-label={copy.language} value={language} onChange={event => onLanguageChange(event.target.value)}>{supportedLocales.map(locale => <option key={locale} value={locale}>{localeNames[locale]}</option>)}</select></header>
    <nav className="ap-center-tabs" aria-label={copy.title}>{['profile', 'recharge'].map(value => <button type="button" key={value} aria-pressed={section === value} onClick={() => { setSection(value); if (value === 'recharge') setRechargeOpened(true); }}>{words[value]}</button>)}</nav>
    {error && <p className="ap-alert error" role="alert">{error}</p>}{notice && <p className="ap-alert" role="status">{notice}</p>}
    <div hidden={section !== 'profile'} className="ap-center-panel">
      <section className="ap-identity-card"><div className="ap-identity-summary"><span className="ap-identity-avatar" aria-hidden="true">{name.slice(0, 1).toUpperCase()}</span><div><h2>{name}</h2><span className="ap-muted">{copy.username}</span></div><button type="button" title={words.openWeb} disabled={webBusy} onClick={() => void openWeb()}>{copy.profileSettings}<span aria-hidden="true"> ↗</span></button></div><dl><div><dt>{copy.mobile}</dt><dd>{maskedAccountMobile(me.mobile)}</dd></div><div><dt>{copy.accountStatus}</dt><dd>{accountLayoutCopy[language].statuses[me.status] || me.status}</dd></div></dl><p className="ap-profile-lead">{words.profileLead}</p></section>
      <OrganizationDirectory key={`${me.user_id}:${client.financeSessionEpoch || 0}`} client={client} me={me} language={language} contextOnly onChanged={onRefresh} />
    </div>
    <div hidden={section !== 'recharge'} className="ap-center-panel"><PortalDialogScope active={section === 'recharge'}>
      <p className="ap-center-lead">{words.rechargeLead}</p>
      {!organization && hasPermission(me, 'personal.billing.read') && <section className="ap-balance-hero"><span>{copy.availableCredits}</span><strong>{formatBalance(balance?.available_microcredits, language)}</strong><small>{copy.reservedCredits}: {formatBalance(balance?.reserved_microcredits, language)}</small></section>}
      {!canRecharge && <p role="status" className="ap-alert">{words.rechargeUnavailable}</p>}
      {rechargeOpened && canRecharge && (organization ? <OrganizationFinanceWorkspace key={`${me.user_id}:${me.active_context.organization_id}:${me.active_context.context_version}`} client={client} me={me} language={language} rechargeOnly purchaseFocusRequest={rechargeRequest} /> : <PersonalFinanceWorkspace key={`desktop-recharge:${me.user_id}`} client={client} me={me} language={language} rechargeOnly onPaid={onPaid} />)}
    </PortalDialogScope></div>
    {section === 'profile' && <section className="ap-web-center"><h2>{words.webTitle}</h2><p>{words.webLead}</p><button type="button" disabled={webBusy} aria-label={words.openWeb} onClick={() => void openWeb()}>{webBusy ? words.openingWeb : words.openWeb}<span aria-hidden="true"> ↗</span></button>{webError && <p className="ap-alert error" role="alert">{webError}</p>}</section>}
    <footer className="ap-center-footer"><button type="button" disabled={busy} onClick={onRefresh}>{busy ? copy.loading : copy.refresh}</button><button type="button" disabled={busy} onClick={onLogout}>{copy.logout}</button></footer>
  </main>;
}
