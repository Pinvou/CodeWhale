import React from 'react';
import { createRoot } from 'react-dom/client';
import '../../src/styles/tailwind.css';
import '../../src/features/account-platform/account-platform.css';
import { AccountLogin } from '../../src/features/account-platform/AccountLogin.jsx';
import { accountPlatformCopy } from '../../src/shared/i18n.js';

const language = new URLSearchParams(location.search).get('language') || 'zh';
const client = { registrationStatus: async () => null };
createRoot(document.getElementById('root')).render(<div className="ap-popover-backdrop"><div className="ap-shell ap-login"><AccountLogin client={client} language={language} copy={accountPlatformCopy[language]} onAuthenticated={() => {}} /></div></div>);
