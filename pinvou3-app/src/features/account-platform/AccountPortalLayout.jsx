import { PortalDialogScope } from './portal-dialog-scope.jsx';
import React from 'react';
import { accountDisplayLabel } from './account-display-label.js';
import { accountCenterCopy, organizationCopy } from '../../shared/i18n.js';
import { organizationLabel } from './organization-labels.js';
import { hasPermission } from './authorization-ui.js';
import { supportedLocales, localeNames } from '../../shared/supported-locales.js';
import './account-portal-layout.css';

const noRoles = [];

function NavIcon({ section }) {
  return <svg className="ap-nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
    {section === 'creditsUsage' && <><rect x="3" y="5" width="18" height="14" rx="3" /><path d="M3 9h18M7 16v-3M12 16v-5M17 16v-2" /></>}
    {section === 'ordersInvoices' && <><path d="M5 3h14v18l-3-2-4 2-4-2-3 2z" /><path d="M8 8h8M8 12h8M8 16h5" /></>}
    {section === 'myOrganization' && <><path d="M4 21V7l8-4 8 4v14M8 21v-5h8v5M8 8h1m6 0h1M8 12h1m6 0h1" /></>}
    {section === 'accountSettings' && <><circle cx="12" cy="8" r="4" /><path d="M4 21v-2a8 8 0 0 1 16 0v2" /></>}
  </svg>;
}

export function AccountPortalLayout({ me, copy, language, organizationName, organizationRoles = noRoles, onSwitchIdentity, onLanguageChange, navigation, busy, onRefresh, onLogout, children }) {
  const label = accountDisplayLabel(me, copy.user);
  const words = accountCenterCopy[language];
  const organizational = me.active_context?.context_type === 'ORGANIZATION';
  const keydown = event => {
    if (event.isComposing || event.keyCode === 229) return;
    const tabs = [...event.currentTarget.querySelectorAll('[role="tab"]')];
    const current = tabs.indexOf(event.target);
    if (current < 0) return;
    let next;
    if (['ArrowDown', 'ArrowRight'].includes(event.key)) next = (current + 1) % tabs.length;
    if (['ArrowUp', 'ArrowLeft'].includes(event.key)) next = (current + tabs.length - 1) % tabs.length;
    if (event.key === 'Home') next = 0;
    if (event.key === 'End') next = tabs.length - 1;
    if (next === undefined) return;
    event.preventDefault();
    tabs[next].focus();
    navigation.navigate(tabs[next].dataset.section);
  };
  return <div className="ap-shell ap-app ap-portal ap-restored-portal">
    <aside className="ap-sidebar">
      <div className="ap-sidebar-brand"><span className="ap-brand-mark">P</span><div><p className="ap-eyebrow">PINVOU</p><h1>{copy.title}</h1></div></div>
      <div className="ap-user-summary"><span className="ap-user-avatar" aria-hidden="true">{label.slice(0, 1).toUpperCase()}</span><div><strong>{label}</strong>{me.username && me.username !== label && <span>{me.username}</span>}</div></div>
      <section className="ap-identity-card" aria-label={words.identity}><small>{words.identity}</small><strong>{organizational ? organizationName || words.organization : words.personal}</strong>
        {organizational && <span>{organizationRoles.length ? organizationRoles.map(role => organizationLabel(organizationCopy[language], 'Role', role)).join(', ') : words.roleUnavailable}</span>}
        {hasPermission(me, 'personal.context.switch') && <button type="button" className="ap-text-button" disabled={busy} onClick={onSwitchIdentity}>{words.switchIdentity}</button>}
      </section>
      <div className="ap-portal-tabs" role="tablist" aria-label={copy.title} onKeyDown={keydown}>
        {navigation.sections.map(section => <button type="button" role="tab" id={`ap-tab-${section}`} aria-controls={`ap-panel-${section}`} aria-selected={navigation.section === section} tabIndex={navigation.section === section ? 0 : -1} className={navigation.section === section ? 'active' : ''} data-section={section} key={section} onClick={() => navigation.navigate(section)}><NavIcon section={section} /><span>{copy.sections[section]}</span></button>)}
      </div>
      <div className="ap-sidebar-foot"><select aria-label={copy.language} value={language} onChange={event => onLanguageChange(event.target.value)}>{supportedLocales.map(locale => <option key={locale} value={locale}>{localeNames[locale]}</option>)}</select><button type="button" disabled={busy} onClick={onLogout}>{copy.logout}</button></div>
    </aside>
    <main className="ap-main"><header className="ap-page-header"><div><h2>{copy.sections[navigation.section]}</h2><p>{words.refreshLead}</p></div><button type="button" disabled={busy} onClick={onRefresh}>{words.refreshAccount}</button></header>{children}</main>
  </div>;
}

export function AccountPortalPanel({ section, active, children }) {
  // biome-ignore lint/a11y/noNoninteractiveTabindex: The ARIA tabpanel must be keyboard reachable when its content has no focusable control.
  return <div className="ap-portal-panel" role="tabpanel" id={`ap-panel-${section}`} aria-labelledby={`ap-tab-${section}`} tabIndex={0} hidden={!active}><PortalDialogScope active={active}>{children}</PortalDialogScope></div>;
}
