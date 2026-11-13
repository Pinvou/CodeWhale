import React from 'react';
import { AccountClosureWorkspace } from '../../src/features/account-platform/AccountClosureWorkspace.jsx';
import { PersonalFinanceWorkspace } from '../../src/features/account-platform/PersonalFinanceWorkspace.jsx';

// The desktop center now hands closure off to the Web center. This explicit
// test host preserves the native coordinator's protected-operation regression
// coverage; the production desktop presentation is tested by desktop_ui_smoke.
export function DesktopAccountCenter({ client, me, language, onPaid }) {
  return <main className="ap-shell ap-app">
    <PersonalFinanceWorkspace client={client} me={me} language={language} onPaid={onPaid} />
    <AccountClosureWorkspace client={client} me={me} language={language} />
  </main>;
}
