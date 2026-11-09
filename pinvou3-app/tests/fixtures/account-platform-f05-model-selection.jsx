import React from 'react';
import { createRoot } from 'react-dom/client';
import { dict, ensureLanguage } from '../../src/shared/i18n.js';
const control = { language: 'en', calls: [], selected: false, fail: false, refreshFail: false, authenticated: true, retired: false, retirementMarker: true, epoch: 0 };
const state = { savedModels: [{ id: 'builtin_account_platform', kind: 'builtin_account_platform', system: true, model: '' }], modelsLoaded: true, effectiveModelConfig: null, platformCapabilities: {}, settings: {} };
const subscribers = new Set();
const publishModels = () => {
 for (const subscriber of subscribers) {
  if (subscriber.domains.includes('models')) subscriber.callback({ ...state });
 }
};
window.PinvouPlatform = { kind: 'desktop', capabilities: new Proxy({}, { get: (_, key) => key === 'modelManagement' }) };
window.TauriBridge = { available: true, models: {
 loadModels: async () => { control.calls.push('list'); if (control.refreshFail) throw new Error('controlled refresh unavailable'); state.defaultModelRef = control.selected ? { source: 'ACCOUNT_PLATFORM', model_id: 'public-model-a' } : null; publishModels(); return true; },
 loadEffectiveModelConfig: async () => { control.calls.push('effective'); if (control.refreshFail) throw new Error('controlled refresh unavailable'); state.effectiveModelConfig = control.selected ? { model: 'public-model-a', requires_user_api_key: false, credential_state: 'backend_managed' } : null; publishModels(); return true; },
}, state: { getMany: () => ({ ...state }), subscribeMany: (domains, callback) => { const subscriber = { domains, callback }; subscribers.add(subscriber); return () => subscribers.delete(subscriber); } }, lifecycle: { init: async () => {} } };
const { SettingsView } = await import('../../src/features/settings/SettingsView.jsx');
const { shouldShowModelAccessGate, useBridgeState } = await import('../../src/hooks/useBridge.js');
const client = { session: async () => ({ authenticated: control.authenticated }), modelCatalog: async () => { const retiredNow = control.retired && control.selected; if (control.retired) control.selected = false; return ({ selected_model_id: control.selected ? 'public-model-a' : null, default_model_retired: retiredNow && control.retirementMarker, catalog_version: 1, items: [{ model_id: 'public-model-a', display_name: 'Public selectable model', runtime_protocol: 'OPENAI_CHAT_V1', context_eligible: true, lifecycle_status: control.retired ? 'DEPRECATED' : 'PUBLISHED' }, { model_id: 'deprecated-future', display_name: 'Deprecated future model', runtime_protocol: 'OPENAI_CHAT_V1', context_eligible: true, lifecycle_status: 'DEPRECATED', new_session_cutoff_at: '2099-01-01T00:00:00Z' }, { model_id: 'removed-model', display_name: 'Removed model', runtime_protocol: 'OPENAI_CHAT_V1', context_eligible: true, lifecycle_status: 'REMOVED' }, { model_id: 'public-model-b', display_name: 'Public unavailable model', context_eligible: false, lifecycle_status: 'PUBLISHED' }] }); }, selectCatalogModel: async id => { control.calls.push(`select:${id}`); if (control.fail) throw new Error('controlled unavailable'); control.selected = true; } };
const root = createRoot(document.querySelector('#root'));
function SelectionHarness() {
 const snapshot = useBridgeState(['models']);
 return <SettingsView language={control.language} t={dict[control.language]} activeTheme="light" colorScheme="light" initialSection="model" savedModels={snapshot.savedModels} bs={snapshot} defaultModelRef={snapshot.defaultModelRef} accountPlatformClient={client} />;
}
control.render = async () => { await ensureLanguage(control.language); root.render(<SelectionHarness key={control.epoch} />); };
control.seedExistingDefault = () => { control.selected = true; state.effectiveModelConfig = { model: 'public-model-a', requires_user_api_key: false, credential_state: 'backend_managed' }; state.defaultModelRef = { source: 'ACCOUNT_PLATFORM', model_id: 'public-model-a' }; publishModels(); };
control.retire = async () => { control.retired = true; control.epoch++; await control.render(); };
control.gate = () => shouldShowModelAccessGate(state, 'chat', true);
control.defaultModelRef = () => state.defaultModelRef ?? null;
control.copy = () => dict[control.language].uiSettingsDetail;
window.f05 = control; await control.render();
