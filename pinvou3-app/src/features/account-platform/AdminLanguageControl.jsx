import React, { useId } from 'react';
import { localeNames, supportedLocales } from '../../shared/supported-locales.js';
import { ADMIN_PORTAL_LANGUAGE_STORAGE_KEY, optionalPortalLanguageStorage, storePortalLanguage, storedPortalLanguage } from './portal-language.js';
import './admin-language-preference.css';

import { adminLanguagePreferenceCopy as words } from '../../shared/i18n.js';


export function initialAdminLanguagePreference() {
  const saved = storedPortalLanguage(optionalPortalLanguageStorage(), ADMIN_PORTAL_LANGUAGE_STORAGE_KEY);
  return saved ? { language: saved, feedback: 'saved' } : { language: globalThis.navigator?.language?.startsWith('ja') ? 'ja' : globalThis.navigator?.language?.startsWith('en') ? 'en' : 'zh', feedback: '' };
}

export function applyAdminLanguagePreference(language) {
  if (!supportedLocales.includes(language)) return null;
  const saved = storePortalLanguage(optionalPortalLanguageStorage(), language, ADMIN_PORTAL_LANGUAGE_STORAGE_KEY);
  return { language, feedback: saved ? 'saved' : 'unsaved' };
}

export function AdminLanguageControl({ label, language, feedback, onChange, sidebar = false }) {
  const id = useId(), copy = words[language] || words.zh;
  const help = [sidebar ? '' : copy.scope, feedback ? copy[feedback] : ''].filter(Boolean).join(' ');
  return <div className={sidebar ? "adm-language-control adm-language-sidebar" : "adm-language-control adm-language-settings"}>
    {!sidebar && <label htmlFor={id}>{label}</label>}
    <select id={id} aria-label={sidebar ? label : undefined} aria-describedby={help ? id + '-help' : undefined} value={language} onChange={event => onChange(event.target.value)}>
      {supportedLocales.map(locale => <option key={locale} value={locale}>{localeNames[locale]}</option>)}
    </select>
    <small id={id + '-help'} role="status" data-language-feedback={feedback || undefined}>{help}</small>
  </div>;
}
