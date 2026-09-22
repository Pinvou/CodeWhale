import '../shared/legacy-polyfills.js';
import { useEffect, useMemo, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { AccountPlatformView } from '../features/account-platform/AccountPlatformView.jsx';
import { AccountPlatformBffClient } from '../features/account-platform/bff-client.js';
import {
  browserPortalLanguage,
  consumePortalLanguage,
  PORTAL_LANGUAGE_STORAGE_KEY,
  portalHTMLLanguage,
  storedPortalLanguage,
} from '../features/account-platform/portal-language.js';
import { accountPlatformCopy } from '../shared/account-platform-i18n.js';

function initialLanguage() {
  const requested = consumePortalLanguage(location.href, localStorage, history.replaceState.bind(history));
  return requested || storedPortalLanguage(localStorage) || browserPortalLanguage(navigator.language) || 'zh';
}

function AccountPortalApp() {
  const [language, setLanguage] = useState(initialLanguage);
  const client = useMemo(() => new AccountPlatformBffClient(), []);
  useEffect(() => {
    document.documentElement.lang = portalHTMLLanguage(language);
  }, [language]);
  const updateLanguage = value => {
    localStorage.setItem(PORTAL_LANGUAGE_STORAGE_KEY, value);
    setLanguage(value);
  };
  return <AccountPlatformView client={client} copy={accountPlatformCopy[language]} language={language} onLanguageChange={updateLanguage} portal />;
}

createRoot(document.getElementById('account-root')).render(<AccountPortalApp />);
