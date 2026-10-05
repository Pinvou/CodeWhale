// Controlled public desktop transport. No identity provider or Gateway is contacted.
import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { dict, ensureLanguage } from '../../src/shared/i18n.js';

const language = new URLSearchParams(location.search).get('language') || 'en';
await ensureLanguage(language);
const calls = [];
let pending;
let identityPending;
let outcome = 'success';
let wire = 'wire-a';
const view = () => ({ available_models: ['wire-a', 'wire-b'], default_model: wire });
window.__builtinReview = {
  calls,
  copy: dict[language],
  setOutcome(value) { outcome = value; },
  get pending() { return !!pending; },
  get identityPending() { return !!identityPending; },
  settle() { const accept = pending; pending = null; accept(view()); },
  identityChange(method, fail) {
    outcome = fail ? 'identity-failed' : 'identity-delayed';
    const transport = window.TauriBridge.accountPlatform;
    return (method.startsWith('scope')
      ? transport.request({ area: 'account', resource: method === 'scope-normalized'
        ? '/v1/sessions/current/ignored/../active-context/' : '/v1/sessions/current/active-context',
      method: 'POST', body: { context_type: 'PERSONAL' } })
      : transport[method]({ account: 'public-fixture', password: 'public-fixture-password' })).catch(() => {});
  },
  settleIdentity() { const accept = identityPending; identityPending = null; accept(); },
};
window.PinvouPlatform = { kind: 'desktop', capabilities: {
  modelManagement: true, pet: false, dependencyInstall: false, desktopNotifications: false,
  superPermission: false, appUpdate: false, hostFilePicker: false,
} };
window.TauriBridge = {
  available: true,
  accountPlatform: {
    async getModelCatalog() {
      calls.push({ method: 'getModelCatalog' });
      if (outcome === 'delayed') return new Promise(resolve => { pending = resolve; });
      if (outcome === 'empty') return { available_models: [], default_model: '' };
      if (outcome === 'malformed') return { available_models: ['wire-a'], default_model: false };
      if (outcome === 'denied') return Promise.reject('authentication_required: PUBLIC_PROVIDER_DETAIL');
      return view();
    },
    async setDefaultModel(model) {
      calls.push({ method: 'setDefaultModel', model });
      if (outcome === 'denied') return Promise.reject('authentication_required: PUBLIC_PROVIDER_DETAIL');
      if (outcome === 'delayed') return new Promise(resolve => { pending = value => { wire = model; resolve({ ...value, default_model: model }); }; });
      wire = model;
      return view();
    },
  },
  models: { async loadModels() { calls.push({ method: 'loadModels' }); } },
};
const controlledModels = window.TauriBridge.accountPlatform;
await import('../../src/platform/tauri/bridge/account-platform.js');
window.TauriBridge.accountPlatform = window.__PINVOU_TAURI_BRIDGE_FEATURES__['account-platform']({
  async invoke(command, args) {
    if (command === 'account_platform_models') return controlledModels.getModelCatalog();
    if (command === 'account_platform_set_default_model') return controlledModels.setDefaultModel(args.model);
    if (['account_platform_login', 'account_platform_login_sms', 'account_platform_logout', 'account_platform_request'].includes(command)) {
      if (outcome === 'identity-failed') return Promise.reject('credential_store_failed: PUBLIC_PROVIDER_DETAIL');
      await new Promise(resolve => { identityPending = resolve; });
      return command === 'account_platform_request' ? { status: 204, body: {} }
        : { enabled: true, configured: true, authenticated: command !== 'account_platform_logout', errorCode: null };
    }
    throw new Error('Unsupported public fixture command');
  },
});
const { SettingsView } = await import('../../src/features/settings/SettingsView.jsx');
const models = [
  { id: 'builtin_account_platform', kind: 'builtin_account_platform', readonly: true, system: true,
    preset: 'openai_compatible', name: 'Builtin fixture', model: 'wire-a', base_url: 'https://account.invalid' },
  { id: 'ordinary-custom', preset: 'openai_compatible', name: 'Ordinary custom', model: 'ordinary-wire',
    base_url: 'https://custom.invalid', credential_state: 'configured' },
];
const noOp = () => {};
function Harness() {
  const [mounted, setMounted] = useState(true);
  const [instance, setInstance] = useState(0);
  return <>
    <button data-testid="fixture-toggle" onClick={() => { setMounted(value => !value); setInstance(value => value + 1); }}>Toggle fixture</button>
    {mounted && <SettingsView key={instance} initialSection="model" activeTheme="light" colorScheme="light"
      language={language} setLanguage={noOp} onColorSchemeChange={noOp} t={dict[language]}
      superPerm={false} setSuperPerm={noOp} taskCompletedNotif={false} setTaskCompletedNotif={noOp}
      searchProvider="bing" setSearchProvider={noOp} savedModels={models} activeModelId="builtin_account_platform"
      onSaveModel={noOp} onDeleteModel={noOp} onSetActiveModel={noOp}
      bs={{ settings: { language: language === 'zh' ? 'zh-Hans' : language, advanced: { account_platform_default_model: 'wire-a' } },
        platformCapabilities: {}, savedModels: models }} />}
  </>;
}
createRoot(document.getElementById('settings-root')).render(<Harness />);
