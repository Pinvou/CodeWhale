import React, { useMemo, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { AdminPlatformView } from '../features/account-platform/AdminPlatformView.jsx';
import { AdminPlatformClient } from '../features/account-platform/admin-client.js';
import { createLatestLanguageGate, dict, ensureLanguage } from '../shared/i18n.js';

const LANGUAGE_BY_TAG = { 'zh-CN':'zh', 'zh-Hans':'zh', 'en-US':'en', 'en-GB':'en', 'ja-JP':'ja' };

function initialLanguage() {
  let saved;
  try { saved = localStorage.getItem('pinvou.admin.language'); } catch { /* Preferences are optional. */ }
  if (saved === 'zh' || saved === 'en' || saved === 'ja') return saved;
  return LANGUAGE_BY_TAG[navigator.language] || (navigator.language.startsWith('ja') ? 'ja' : navigator.language.startsWith('en') ? 'en' : 'zh');
}

function AdminPortalApp({ initialLanguage: languageAtMount }) {
  const [language, setLanguage] = useState(languageAtMount);
  const client = useMemo(() => new AdminPlatformClient(), []);
  const switchLanguage = useMemo(() => createLatestLanguageGate(), []);
  const updateLanguage = value => {
    switchLanguage(value, () => {
      try { localStorage.setItem('pinvou.admin.language', value); } catch { /* Preferences are optional. */ }
      document.documentElement.lang = value === 'zh' ? 'zh-CN' : value === 'ja' ? 'ja-JP' : 'en';
      document.title = dict[value].uiAdminPlatform.title;
      setLanguage(value);
    });
  };
  return <AdminPlatformView client={client} copy={dict[language].uiAdminPlatform} language={language} onLanguageChange={updateLanguage} />;
}

async function mountAdminPortal() {
  const preferred = initialLanguage();
  const language = await ensureLanguage(preferred) ? preferred : 'zh';
  document.documentElement.lang = language === 'zh' ? 'zh-CN' : language === 'ja' ? 'ja-JP' : 'en';
  document.title = dict[language].uiAdminPlatform.title;
  createRoot(document.getElementById('admin-root')).render(<AdminPortalApp initialLanguage={language} />);
}

mountAdminPortal();
