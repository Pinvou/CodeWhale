import React, { useMemo, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { AccountPlatformView } from '../features/account-platform/AccountPlatformView.jsx';
import { AccountPlatformBffClient } from '../features/account-platform/bff-client.js';
import { createLatestLanguageGate, dict, ensureLanguage } from '../shared/i18n.js';

const LANGUAGE_BY_TAG = { 'zh-CN':'zh', 'zh-Hans':'zh', 'en-US':'en', 'en-GB':'en', 'ja-JP':'ja' };

function initialLanguage() {
  let saved;
  try { saved = localStorage.getItem('pinvou.account.language'); } catch { /* Preferences are optional. */ }
  if (saved === 'zh' || saved === 'en' || saved === 'ja') return saved;
  return LANGUAGE_BY_TAG[navigator.language] || (navigator.language.startsWith('ja') ? 'ja' : navigator.language.startsWith('en') ? 'en' : 'zh');
}

function AccountPortalApp({ initialLanguage: languageAtMount }) {
  const [language, setLanguage] = useState(languageAtMount);
  const client = useMemo(() => new AccountPlatformBffClient(), []);
  const switchLanguage = useMemo(() => createLatestLanguageGate(), []);
  const updateLanguage = value => {
    switchLanguage(value, () => {
      try { localStorage.setItem('pinvou.account.language', value); } catch { /* Preferences are optional. */ }
      document.documentElement.lang = value === 'zh' ? 'zh-CN' : value === 'ja' ? 'ja-JP' : 'en';
      setLanguage(value);
    });
  };
  return <AccountPlatformView client={client} copy={dict[language].uiAccountPlatform} language={language} onLanguageChange={updateLanguage} portal />;
}

async function mountAccountPortal() {
  const preferred = initialLanguage();
  const language = await ensureLanguage(preferred) ? preferred : 'zh';
  document.documentElement.lang = language === 'zh' ? 'zh-CN' : language === 'ja' ? 'ja-JP' : 'en';
  createRoot(document.getElementById('account-root')).render(<AccountPortalApp initialLanguage={language} />);
}

mountAccountPortal();
