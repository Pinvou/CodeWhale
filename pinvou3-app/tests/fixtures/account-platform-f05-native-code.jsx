import React from 'react';
import { createRoot } from 'react-dom/client';
import { dict, ensureLanguage } from '../../src/shared/i18n.js';
import * as native from '../../src/features/codex/native-session-handoff.js';
const control = { language: 'en', calls: [], owner: 'code-a', authenticated: true, busy: false, fail: false,
  modelRef: { source: 'ACCOUNT_PLATFORM', model_id: 'public-a' } };
const state = { activeSessionId: 'chat-other', currentSessionModelRef: { source: 'ACCOUNT_PLATFORM', model_id: 'chat-only' },
  savedModels: [{ id: 'builtin_account_platform', kind: 'builtin_account_platform', system: true }, { id: 'local-a', model: 'Local A', preset: 'custom', name: 'Local A' }] };
window.PinvouPlatform = { kind: 'desktop', capabilities: { sessionModelSwitch: true, modelManagement: true } };
window.TauriBridge = { available: true, models: { switchModelRef: async (...args) => { control.calls.push(['wrong-chat-bridge', ...args]); } },
  state: { getMany: () => state, subscribeMany: () => () => {} }, lifecycle: { init: async () => {} } };
const { ComposerModelSelector } = await import('../../src/features/settings/composer-shared.jsx');
const client = { session: async () => ({ authenticated: control.authenticated }), modelCatalog: async () => ({ items: [
  { model_id: 'public-a', display_name: 'Platform A', context_eligible: true, lifecycle_status: 'PUBLISHED' },
  { model_id: 'public-b', display_name: 'Platform B', context_eligible: true, lifecycle_status: 'PUBLISHED' },
  { model_id: 'blocked', display_name: 'Platform blocked', context_eligible: true, lifecycle_status: 'DEPRECATED', new_session_cutoff_at: '2000-01-01T00:00:00Z' },
] }) };
const root = createRoot(document.querySelector('#root'));
control.render = async () => { await ensureLanguage(control.language); root.render(<ComposerModelSelector t={dict[control.language]} bs={state}
 sessionId={control.owner} sessionModelId={control.modelRef?.saved_model_id || 'builtin_account_platform'} sessionModelRef={control.modelRef}
 busy={control.busy} accountPlatformClient={client} multiAgentAvailable={false}
 onSwitchModelRef={async (sessionId, ref) => { await native.persistNativeSessionModelRef(async (command, args) => {
   control.calls.push([command, args]); if (control.fail) throw new Error('controlled unavailable');
 }, sessionId, ref); control.modelRef = ref; await control.render(); }} />); };
control.copy = () => dict[control.language]; window.nativeCode = control; await control.render();
