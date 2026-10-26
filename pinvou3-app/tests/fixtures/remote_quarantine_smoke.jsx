import React from 'react';
import { createRoot } from 'react-dom/client';
import { dictZh } from '../../src/shared/i18n/zh.js';
import before from './remote_quarantine_before.json';
import after from './remote_quarantine_after.json';
import '../../src/shared/bridge-shared-helpers.js';
import '../../src/features/remote-control/remote-control-state.js';
import '../../src/platform/tauri/bridge/desktop-remote-control.js';
import facadeSource from '../../src/platform/tauri/bridge.js?raw';

// Real mounted panel + real public facade + real remote feature, native IPC mock.
// The two DTO fixtures are independently verified by the Rust producer test.
const logic = window.__PINVOU_REMOTE_CONTROL_STATE__;
const fixture = window.quarantineFixture = {
  state: { desktopRemoteControl: logic.projectRemoteProxyRead(logic.createState(), before) },
  sealed: false, calls: [], failure: null, cleanup: true, refreshFails: false,
};
async function nativeInvoke(command, args) {
    fixture.calls.push([command, args]);
    if (command === 'get_platform_capabilities') return { os: 'windows', desktopRemoteControlSupported: true };
    if (command === 'desktop_remote_proxy_quarantine') {
      if (fixture.failure) throw Object.assign(new Error(fixture.failure), { code: fixture.failure });
      fixture.sealed = true;
      return fixture.cleanup;
    }
    if (fixture.refreshFails && fixture.sealed) throw new Error('refresh_io_failure');
    if (command === 'desktop_remote_proxy_archive_candidates') return fixture.sealed ? [] : [
      { localSessionId: before.localSessionId, title: '待隔离旧会话', pendingCount: 2, projectedEntryCount: 0, hidden: true },
    ];
    if (command === 'desktop_remote_proxy_history_list') return [];
    if (command === 'desktop_remote_proxy_archive_list') return fixture.sealed ? [
      { localSessionId: before.localSessionId, title: '冻结旧会话', archived: true, archiveCursor: '1' },
    ] : [];
    if (command === 'desktop_remote_proxy_history_read') return fixture.sealed ? after : before;
    return {};
}
const factories = window.__PINVOU_TAURI_BRIDGE_FEATURES__;
const remoteFactory = factories['desktop-remote-control'];
const inertFeature = () => new Proxy({}, { get: () => function () {} });
window.__PINVOU_TAURI_BRIDGE_FEATURES__ = new Proxy({
  'desktop-remote-control': deps => {
    fixture.state = deps.state;
    deps.state.desktopRemoteControl = logic.projectRemoteProxyRead(logic.createState(), before);
    return remoteFactory(deps);
  },
}, { get: (object, key) => Object.hasOwn(object, key) ? object[key] : inertFeature });
window.__TAURI__ = { core: { invoke: nativeInvoke }, event: { listen: async () => () => {} }, dialog: {} };
// Suppress unrelated application-wide automatic startup, retaining the actual
// facade's registration, state, capability loading and remote public exports.
new Function('window', 'document', facadeSource)(window, { readyState: 'loading', addEventListener() {} });
await window.TauriBridge.platform.loadPlatformCapabilities();
const { default: Panel } = await import('../../src/features/remote-control/RemoteProxyArchivePanel.jsx');
createRoot(document.getElementById('root')).render(<Panel t={dictZh} />);
