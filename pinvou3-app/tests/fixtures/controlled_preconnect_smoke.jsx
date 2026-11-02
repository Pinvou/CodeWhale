import React from 'react';
import { createRoot } from 'react-dom/client';
import { dictZh } from '/src/shared/i18n/zh.js';
import { dictEn } from '/src/shared/i18n/en.js';
import { dictJa } from '/src/shared/i18n/ja.js';
import '/src/features/remote-control/remote-control-state.js';

// Renderer regression: real reducer and React component, mock bridge/IPC.
// No Rust, actual Relay, provider, store or native WebView is exercised.
const language = new URLSearchParams(location.search).get('lang') || 'zh';
const dictionary = { zh: dictZh, en: dictEn, ja: dictJa }[language];
if (!dictionary) throw new Error('unsupported_fixture_language');
const logic = window.__PINVOU_REMOTE_CONTROL_STATE__;
const root = createRoot(document.getElementById('root'));
let Panel;
let projection = logic.createState();
const fixture = window.preconnectFixture = {
  calls: [], sequence: 1, ready: false,
  status() { return projection.controlledDevice; },
};
const waitingWire = (sequence) => ({
  status: 'waiting_for_pairing', statusIntent: String(sequence),
  generation: `generation-renderer-${sequence}`,
  registrationRequestId: `registration-renderer-${sequence}`,
  pairingCode: sequence === 1 ? '2345678' : sequence === 2 ? 'ABCDEFG' : 'HJKMNPQ',
  pairingRemainingMs: 300_000, workspaceCount: 0, codeModeAllowed: false,
});
function render() {
  if (!Panel) return;
  const device = projection.controlledDevice;
  root.render(<Panel t={dictionary} bs={projection} supported
    secondsRemaining={device.pairingRemainingMs == null ? 0 : 300} nowMs={Date.now()} />);
}
function publishWire(wire, options = {}) {
  const previous = projection;
  projection = logic.applyControlledStatus(projection, { ...wire, ...options });
  if (projection === previous) throw new Error(`fixture_projection_rejected:${wire.status}`);
  render();
}
projection = logic.beginControlledIntent(projection, true);
publishWire(waitingWire(1), { intent: projection.intents.controlled, replaceBinding: true });
fixture.expire = (code) => {
  if (!['desktop_remote_pairing_expired', 'desktop_remote_handshake_timeout'].includes(code)) {
    throw new Error('unsupported_fixture_error');
  }
  const current = projection.controlledDevice;
  if (code === 'desktop_remote_handshake_timeout') {
    publishWire({ status: 'handshake_pending', generation: current.generation,
      statusIntent: current.statusIntent, workspaceCount: 0 });
  }
  publishWire({ status: 'error', generation: current.generation,
    statusIntent: current.statusIntent, errorCode: code, workspaceCount: 0 });
};
window.TauriBridge = { available: true, desktopRemoteControl: {
  async controlledSync() { fixture.calls.push(['sync']); return projection.controlledDevice; },
  async controlledPermissions() { fixture.calls.push(['permissions']); return { chat: 'yolo', code: 'plan' }; },
  async controlledListWorkspaces() { fixture.calls.push(['workspaces']); return []; },
  async controlledStart(deviceName, workspaces) {
    fixture.calls.push(['start', deviceName, workspaces]);
    if (projection.controlledDevice.status !== 'error') throw new Error('start_requires_terminal');
    projection = logic.beginControlledIntent(projection, true);
    const intent = projection.intents.controlled;
    const next = waitingWire(++fixture.sequence);
    publishWire({ status: 'registering', statusIntent: next.statusIntent,
      generation: next.generation }, { intent, replaceBinding: true });
    await Promise.resolve();
    publishWire(next, { intent, replaceBinding: true });
    return next;
  },
  async controlledRefresh() {
    fixture.calls.push(['refresh']);
    throw new Error('refresh_must_remain_unavailable_after_terminal');
  },
  async controlledStop() {
    fixture.calls.push(['stop']);
    throw new Error('terminal_recovery_must_start_not_stop');
  },
} };
// useBridge captures window.TauriBridge at module evaluation, so import last.
({ default: Panel } = await import('/src/features/remote-control/ControlledDevicePanel.jsx'));
render();
fixture.ready = true;
