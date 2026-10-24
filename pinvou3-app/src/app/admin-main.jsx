import '../shared/legacy-polyfills.js';
import React, { useMemo, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { AdminPlatformView } from '../features/account-platform/AdminPlatformView.jsx';
import { AdminPlatformClient } from '../features/account-platform/admin-client.js';
import { adminPlatformCopy } from '../shared/i18n.js';
import { ADMIN_PORTAL_LANGUAGE_STORAGE_KEY, optionalPortalLanguageStorage, storedPortalLanguage, storePortalLanguage } from '../features/account-platform/portal-language.js';

const LANGUAGE_BY_TAG = { 'zh-CN':'zh', 'zh-Hans':'zh', 'en-US':'en', 'en-GB':'en', 'ja-JP':'ja' };

function initialLanguage() {
  const saved = storedPortalLanguage(optionalPortalLanguageStorage(), ADMIN_PORTAL_LANGUAGE_STORAGE_KEY);
  if (saved) return saved;
  return LANGUAGE_BY_TAG[navigator.language] || (navigator.language.startsWith('ja') ? 'ja' : navigator.language.startsWith('en') ? 'en' : 'zh');
}

function AdminPortalApp() {
  const [language, setLanguage] = useState(initialLanguage);
  const client = useMemo(() => new AdminPlatformClient(), []);
  const updateLanguage = value => {
    storePortalLanguage(optionalPortalLanguageStorage(), value, ADMIN_PORTAL_LANGUAGE_STORAGE_KEY);
    document.documentElement.lang = value === 'zh' ? 'zh-CN' : value === 'ja' ? 'ja-JP' : 'en';
    setLanguage(value);
    globalThis.dispatchEvent(new CustomEvent('pinvou-admin-language-change', { detail: { language: value } }));
  };
  return <AdminPlatformView client={client} copy={adminPlatformCopy[language]} language={language} onLanguageChange={updateLanguage} />;
}

createRoot(document.getElementById('admin-root')).render(<AdminPortalApp />);
