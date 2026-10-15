import React from 'react';
import { createRoot } from 'react-dom/client';
import '../../src/styles/base.css';
import { dict, ensureLanguage } from '../../src/shared/i18n.js';
import { ChatView } from '../../src/features/chat/ChatView.jsx';
import { bridge, useBridgeState } from '../../src/hooks/useBridge.js';
import { latestModelLifecycleRejection } from '../../src/features/account-platform/model-lifecycle.js';

// The HTML loads the exact installed bridge modules. The Node runner injects
// the existing ui_smoke Tauri producer before those scripts execute; this
// harness substitutes no ChatView callback, lifecycle helper or bridge reader.
const control = window.f05Lifecycle;
const root = createRoot(document.querySelector('#root'));
const none = () => {};
function Host() {
  const bs = useBridgeState(['chat', 'sessions', 'models', 'settings', 'voice', 'knowledge', 'platform', 'scheduled', 'interaction', 'computerUse', 'personas', 'memory', 'dependencies']);
  return <ChatView theme="light" t={dict[control.language]} bs={bs} prefill=""
    onPrefillConsumed={none} onOpenEditor={none} setJustInstalledTool={none}
    onGotoSettings={none} onGotoTools={none} />;
}
control.copy = () => dict[control.language];
control.currentMarker = () => latestModelLifecycleRejection(bridge.state.getMany(['chat']).turnTimeline);
control.activeId = () => bridge.state.getMany(['sessions']).activeSessionId;
control.defaultRef = () => bridge.state.getMany(['models']).defaultModelRef;
control.render = async () => { await ensureLanguage(control.language); root.render(<Host />); };
control.switchTo = id => bridge.sessions.switchToSession(id);
control.sendFailure = async () => {
  const result = await bridge.chat.sendMessageToSession('s1', 'Controlled preparation attempt');
  return result.completion ? result.completion.then(completion => ({ ok: completion.ok })) : result;
};
await control.render();
await bridge.lifecycle.init();
await control.switchTo('s1');
control.ready = true;
