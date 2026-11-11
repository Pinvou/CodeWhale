import React from 'react';
import { adminLayoutCopy } from '../../shared/i18n.js';
import { supportedLocales, localeNames } from '../../shared/supported-locales.js';
import { adminSectionGroups } from './admin-sections.js';
import pinvouBrand from './assets/brand-blue.png';
import './admin-portal-layout.css';

const icons = { overview: '◫', users: '♙', roles: '⚿', organizations: '▦', myorg: '◇', billing: '▤', payments: '↔', products: '▣', tokens: '◈', models: '◈', tokenResources: '▧', modelOperations: '⌁', operations: '▥', approvals: '✓', audit: '≡', system: '⚙', settings: '⋯' };
export function AdminPortalLayout({ copy, language, onLanguageChange, navigation, ready, onLogout, children }) {
  const words = adminLayoutCopy[language], group = adminSectionGroups.find(item => item.sections.includes(navigation.section));
  return <div className="adm-shell adm-restored-shell">
    <a className="adm-skip-link" href="#admin-content">{words.skipToContent}</a>
    <aside className="adm-sidebar"><div className="adm-brand"><img className="adm-mark" src={pinvouBrand} width="38" height="38" alt="" aria-hidden="true" /><div><strong>{copy.title}</strong><span>{words.product}</span></div></div>
      <nav aria-label={words.primaryNavigation}>{adminSectionGroups.map(item => {
        const sections = item.sections.filter(key => navigation.available.includes(key));
        return sections.length > 0 && <div className="adm-nav-group" key={item.key}><span className="adm-nav-label">{words.navGroups[item.key]}</span>{sections.map(key => <button key={key} type="button" data-section={key} className={navigation.section === key ? 'active' : ''} aria-current={navigation.section === key ? 'page' : undefined} onClick={() => navigation.navigate(key)}><span aria-hidden="true">{icons[key]}</span>{words.nav[key]}</button>)}</div>;
      })}</nav><div className="adm-sidebar-foot"><a href="/">{words.home}</a><select aria-label={copy.settings.language} value={language} onChange={event => onLanguageChange(event.target.value)}>{supportedLocales.map(locale => <option key={locale} value={locale}>{localeNames[locale]}</option>)}</select></div>
    </aside>
    <main className="adm-main" id="admin-content" tabIndex={-1}><header className="adm-topbar"><div><span className="adm-eyebrow">{words.product}</span><h1>{copy.title}</h1></div>{ready && <button type="button" onClick={onLogout}>{copy.logout}</button>}</header>
      <div className="adm-page">{ready && <><nav className="adm-breadcrumbs" aria-label={words.primaryNavigation}><button type="button" disabled={!navigation.available.includes('overview')} onClick={() => navigation.navigate('overview')}>{words.nav.overview}</button><span>/</span>{group && <span>{words.navGroups[group.key]}</span>}<span>/</span><strong>{words.nav[navigation.section] || words.unauthorized}</strong></nav><div className="adm-page-head"><div><span className="adm-eyebrow">{group && words.navGroups[group.key]}</span><h2>{words.nav[navigation.section] || words.unauthorized}</h2></div></div></>}{children}</div>
    </main>
  </div>;
}
