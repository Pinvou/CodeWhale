import '../shared/legacy-polyfills.js';
import React, { useLayoutEffect, useMemo, useState } from 'react';
import { createRoot } from 'react-dom/client';
import { AdminPlatformView } from '../features/account-platform/AdminPlatformView.jsx';
import { AdminPlatformClient } from '../features/account-platform/admin-client.js';
import { adminPlatformCopy } from '../shared/i18n.js';
import { portalHTMLLanguage } from '../features/account-platform/portal-language.js';
import { initialAdminLanguagePreference, applyAdminLanguagePreference } from '../features/account-platform/AdminLanguageControl.jsx';

function AdminPortalApp() {
  const [{ language, feedback: languageFeedback }, setPreference] = useState(initialAdminLanguagePreference);
  useLayoutEffect(() => { document.documentElement.lang = portalHTMLLanguage(language); }, [language]);
  const client = useMemo(() => new AdminPlatformClient(), []);
  const updateLanguage = value => {
    const preference = applyAdminLanguagePreference(value);
    if (!preference) return;
    document.documentElement.lang = portalHTMLLanguage(preference.language);
    setPreference(preference);
    globalThis.dispatchEvent(new CustomEvent('pinvou-admin-language-change', { detail: { language: preference.language } }));
  };
  return <AdminPlatformView client={client} copy={adminPlatformCopy[language]} language={language} languageFeedback={languageFeedback} onLanguageChange={updateLanguage} />;
}

createRoot(document.getElementById('admin-root')).render(<AdminPortalApp />);
