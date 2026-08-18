import React, { useMemo, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { AdminPlatformView } from '../features/account-platform/AdminPlatformView.jsx';
import { AdminPlatformClient } from '../features/account-platform/admin-client.js';
import { dict } from '../shared/i18n.js';

const LANGUAGE_BY_TAG = { 'zh-CN':'zh', 'zh-Hans':'zh', 'en-US':'en', 'en-GB':'en', 'ja-JP':'ja' };

function initialLanguage() {
  const saved = localStorage.getItem('pinvou.admin.language');
  if (saved === 'zh' || saved === 'en' || saved === 'ja') return saved;
  return LANGUAGE_BY_TAG[navigator.language] || (navigator.language.startsWith('ja') ? 'ja' : navigator.language.startsWith('en') ? 'en' : 'zh');
}

function AdminPortalApp() {
  const [language, setLanguage] = useState(initialLanguage);
  const client = useMemo(() => new AdminPlatformClient(), []);
  const updateLanguage = value => {
    localStorage.setItem('pinvou.admin.language', value);
    document.documentElement.lang = value === 'zh' ? 'zh-CN' : value === 'ja' ? 'ja-JP' : 'en';
    setLanguage(value);
  };
  return <AdminPlatformView client={client} copy={dict[language].uiAdminPlatform} language={language} onLanguageChange={updateLanguage} />;
}

createRoot(document.getElementById('admin-root')).render(<AdminPortalApp />);
