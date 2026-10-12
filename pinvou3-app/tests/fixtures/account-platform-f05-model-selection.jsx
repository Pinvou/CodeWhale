import React from 'react';
import { createRoot } from 'react-dom/client';
import { dict, ensureLanguage } from '../../src/shared/i18n.js';
const control = { language: 'en', calls: [], selected: false, fail: false, authenticated: true };
const state = { savedModels: [{ id: 'builtin_account_platform', kind: 'builtin_account_platform', system: true, model: '' }], modelsLoaded: true, effectiveModelConfig: null, platformCapabilities: {}, settings: {} };
window.PinvouPlatform = { kind: 'desktop', capabilities: new Proxy({}, { get: (_, key) => key === 'modelManagement' }) };
window.TauriBridge = { available: true, models: {
 loadModels: async () => { control.calls.push('list'); state.defaultModelRef = control.selected ? { source: 'ACCOUNT_PLATFORM', model_id: 'public-model-a' } : null; return true; },
 loadEffectiveModelConfig: async () => { control.calls.push('effective'); state.effectiveModelConfig = control.selected ? { model: 'public-model-a', requires_user_api_key: false, credential_state: 'backend_managed' } : null; return true; },
}, state: { getMany: () => state, subscribeMany: () => () => {} }, lifecycle: { init: async () => {} } };
const { SettingsView } = await import('../../src/features/settings/SettingsView.jsx');
const { shouldShowModelAccessGate } = await import('../../src/hooks/useBridge.js');
const client = { session: async () => ({ authenticated: control.authenticated }), modelCatalog: async () => ({ catalog_version: 1, items: [{ model_id: 'public-model-a', display_name: 'Public selectable model', context_eligible: true, lifecycle_status: 'PUBLISHED' }, { model_id: 'public-model-b', display_name: 'Public unavailable model', context_eligible: false, lifecycle_status: 'PUBLISHED' }] }), selectCatalogModel: async id => { control.calls.push(`select:${id}`); if (control.fail) throw new Error('controlled unavailable'); control.selected = true; } };
const root = createRoot(document.querySelector('#root'));
control.render = async () => { await ensureLanguage(control.language); root.render(<SettingsView language={control.language} t={dict[control.language]} activeTheme="light" colorScheme="light" initialSection="model" savedModels={state.savedModels} bs={state} defaultModelRef={state.defaultModelRef} accountPlatformClient={client} />); };
control.gate = () => shouldShowModelAccessGate(state, 'chat', true);
control.copy = () => dict[control.language].uiSettingsDetail;
window.f05 = control; await control.render();
