import React, { useMemo, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { AccountPlatformView } from '../features/account-platform/AccountPlatformView.jsx';
import { AccountPlatformBffClient } from '../features/account-platform/bff-client.js';
import { accountPlatformCopy } from '../shared/account-platform-i18n.js';

const LANGUAGE_BY_TAG = { 'zh-CN':'zh', 'zh-Hans':'zh', 'en-US':'en', 'en-GB':'en', 'ja-JP':'ja' };

function initialLanguage() {
  const saved = localStorage.getItem('pinvou.account.language');
  if (saved === 'zh' || saved === 'en' || saved === 'ja') return saved;
  return LANGUAGE_BY_TAG[navigator.language] || (navigator.language.startsWith('ja') ? 'ja' : navigator.language.startsWith('en') ? 'en' : 'zh');
}

function AccountPortalApp() {
  const [language, setLanguage] = useState(initialLanguage);
  const client = useMemo(() => new AccountPlatformBffClient(), []);
  const updateLanguage = value => {
    localStorage.setItem('pinvou.account.language', value);
    document.documentElement.lang = value === 'zh' ? 'zh-CN' : value === 'ja' ? 'ja-JP' : 'en';
    setLanguage(value);
  };
  return <AccountPlatformView client={client} copy={accountPlatformCopy[language]} language={language} onLanguageChange={updateLanguage} portal />;
}

createRoot(document.getElementById('account-root')).render(<AccountPortalApp />);
