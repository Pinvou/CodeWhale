import React, { useRef, useState } from 'react';
import { createRoot } from 'react-dom/client';
import '../../src/styles/base.css';
import '../../src/styles/tailwind.css';
import { ModelAccessGate } from '../../src/features/settings/ModelAccessGate.jsx';
import { ModelAccessController } from '../../src/features/settings/ModelAccessController.jsx';
import { useModalIsolation } from '../../src/hooks/useModalIsolation.js';
import { dict, ensureLanguage } from '../../src/shared/i18n.js';
import { modelAccessAppliesToView } from '../../src/shared/model-access-scope.js';

const params = new URLSearchParams(location.search);
const language = params.get('language') || 'zh';
await ensureLanguage(language);
const carrier = { id: 'builtin_account_platform', kind: 'builtin_account_platform' };
const local = { id: 'local', model: 'local-model', preset: 'local_vllm' };
const mode = params.get('mode');
let authenticated = mode === 'authenticated';
let fail = mode === 'error';
let delayed = mode === 'delayed';
let release;
const control = { reads: 0, writes: 0 };
let snapshot = { modelsLoaded: true, savedModels: [carrier], effectiveModelConfig: {} };
if (mode === 'local') snapshot = { ...snapshot, savedModels: [carrier, local], effectiveModelConfig: local };
if (mode === 'history') snapshot.activeSessionId = 'existing';
const modelsBridge = {
  loadModels: async () => { control.reads++; return !fail; },
  loadEffectiveModelConfig: async () => !fail,
};
const transport = {
  session: async () => {
    if (delayed) { delayed = false; return new Promise(resolve => { release = () => resolve({ authenticated: false }); }); }
    if (fail) throw new Error('CONTROLLED_ACCOUNT_FAILURE');
    return { authenticated };
  },
  login: async () => { authenticated = true; },
  loginSms: async () => { authenticated = true; },
  logout: async () => { authenticated = false; },
  modelCatalog: async () => ({ items: [{ model_id: 'platform-a' }] }),
};
function Harness() {
  const [dismissed, setDismissed] = useState(false);
  const [view, setView] = useState(params.get('view') || 'chat');
  const [nested, setNested] = useState(false);
  const [revision, setRevision] = useState(0);
  const [client, setClient] = useState(null);
  const [access, setAccess] = useState({ resolved: { kind: 'checking' } });
  const modal = useRef(null);
  const resolved = access.resolved;
  const applies = modelAccessAppliesToView(snapshot, view, true, mode === 'remote');
  const gate = !dismissed && !snapshot.activeSessionId && applies && resolved.kind === 'first_use';
  const notice = !gate && applies && !['ready', 'checking'].includes(resolved.kind);
  useModalIsolation(gate, modal);
  Object.assign(control, {
    client, kind: resolved.kind, gate, revision, release: () => release?.(),
    recover: () => { fail = false; return access.refresh(); },
    chooseLocal: () => { snapshot = { ...snapshot, savedModels: [carrier, local], effectiveModelConfig: local }; setRevision(value => value + 1); },
    nested: () => setNested(true),
  });
  const props = { t: dict[language], language, theme: params.get('theme') || 'light', access: resolved, onLogin: () => client.login('synthetic', 'synthetic'), onUseExternalModels: () => setView('settings'), onRetry: () => access.refresh(), onDismiss: () => setDismissed(true) };
  return <>
    <ModelAccessController modelsBridge={modelsBridge} accountTransport={transport} enabled sessionId={snapshot.activeSessionId} selectionKey={JSON.stringify(snapshot.defaultModelRef)} modelState={snapshot} onChange={setAccess} onClient={setClient} />
    <main data-testid="workspace"><button id="sidebar-login" disabled={!client} onClick={() => client.login('synthetic', 'synthetic')}>Fixture login</button><article>Existing workspace content</article><output>{view}</output></main>
    {notice && <ModelAccessGate {...props} variant="notice" />}
    {gate && <div ref={modal} role="dialog" aria-modal="true" tabIndex={-1} className="fixed inset-0 flex items-center justify-center overflow-auto bg-white p-4" onKeyDown={event => {
      if (event.key === 'Escape' && event.target.closest('[role="dialog"]') === event.currentTarget) setDismissed(true);
    }}>
      <ModelAccessGate {...props} />
      {nested && <div role="dialog" aria-modal="true" aria-label="Fixture captcha" className="fixed bg-white p-4"><button id="captcha-first">First</button><button id="captcha-last" onClick={() => setNested(false)}>Close</button></div>}
    </div>}
  </>;
}
window.modelAccessFixture = control;
createRoot(document.getElementById('root')).render(<Harness />);
