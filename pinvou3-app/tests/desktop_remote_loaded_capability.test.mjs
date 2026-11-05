import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';
import test from 'node:test';

const sourceRoot = new URL('../src/', import.meta.url);

test('app startup loads remote history from the loaded semantic capability on Linux', () => {
  const source = fs.readFileSync(new URL('app/main.jsx', sourceRoot), 'utf8');
  const wiring = source.match(/const remoteProxyHistoryAvailable =[\s\S]*?\}, \[remoteProxyHistoryAvailable\]\);/);
  assert.ok(wiring, 'exercise the production availability expression and startup effect together');
  for (const [label, platformCapabilities, available, expected] of [
    ['Linux supported', { os: 'linux', loaded: true, desktopRemoteControl: true }, true, 1],
    ['Windows supported', { os: 'windows', loaded: true, desktopRemoteControl: true }, true, 1],
    ['future supported host', { os: 'future-host', loaded: true, desktopRemoteControl: true }, true, 1],
    ['not loaded', { os: 'linux', loaded: false, desktopRemoteControl: true }, true, 0],
    ['unsupported', { os: 'linux', loaded: true, desktopRemoteControl: false }, true, 0],
    ['missing capabilities', undefined, true, 0],
    ['bridge unavailable', { os: 'linux', loaded: true, desktopRemoteControl: true }, false, 0],
  ]) {
    let calls = 0;
    vm.runInNewContext(wiring[0], {
      bs: { platformCapabilities },
      bridge: {
        available,
        desktopRemoteControl: { listRemoteProxyHistory() { calls += 1; return Promise.resolve([]); } },
      },
      useEffect(effect) { effect(); },
    });
    assert.equal(calls, expected, label);
  }
});

function loadDesktopBridge() {
  const calls = [];
  let response = null;
  let capabilityCheck;
  const storage = { getItem: () => null, setItem() {}, removeItem() {} };
  const host = {
    __TAURI__: {
      core: { invoke: async (command) => {
        calls.push(command);
        if (command === 'get_platform_capabilities') {
          if (response instanceof Error) throw response;
          return response;
        }
        return { status: 'idle' };
      } },
      event: { listen: async () => () => {} },
      dialog: { open: async () => null },
    },
    performance: { now: () => 0 }, addEventListener() {},
    localStorage: storage, setTimeout, clearTimeout,
  };
  const context = vm.createContext({
    window: host, navigator: {}, localStorage: storage,
    document: { addEventListener() {}, createElement: () => ({ style: {} }), body: { appendChild() {} } },
    console: { ...console, warn() {} }, setTimeout, clearTimeout,
    setInterval: () => 0, clearInterval() {}, structuredClone,
  });
  const html = fs.readFileSync(new URL('index.html', sourceRoot), 'utf8');
  const scripts = [...html.matchAll(/<script src="%BASE_URL%(shared\/(?:bridge-messages|bridge-shared-helpers)\.js|features\/remote-control\/remote-control-state\.js|platform\/tauri\/bridge(?:\.js|\/[^" ]+))"[^>]*><\/script>/g)]
    .map((match) => match[1]);
  assert.ok(scripts.includes('platform/tauri/bridge.js'));
  for (const path of scripts) {
    vm.runInContext(fs.readFileSync(new URL(path, sourceRoot), 'utf8'), context, { filename: path });
    if (path === 'platform/tauri/bridge/desktop-remote-control.js') {
      const factory = host.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control'];
      host.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control'] = (ports) => {
        capabilityCheck = ports.can;
        return factory(ports);
      };
    }
  }
  vm.runInContext(fs.readFileSync(new URL('shared/platform.js', sourceRoot), 'utf8')
    .replace(/\bexport\s+/g, ''), context);
  return {
    api: host.TauriBridge, calls,
    setCapabilities(value) { response = value; },
    canRemote: () => vm.runInContext('can("desktopRemoteControl")', context),
    bridgeCan: (name) => capabilityCheck(name),
  };
}

test('the full desktop bridge and shared UI gate agree before load, after failure and on every OS', async () => {
  const host = loadDesktopBridge();
  await new Promise(resolve => setTimeout(resolve, 0));
  const platformSnapshots = [];
  const settingsSnapshots = [];
  const unsubscribePlatform = host.api.state.subscribe('platform', snapshot => platformSnapshots.push(snapshot));
  const unsubscribeSettings = host.api.state.subscribe('settings', snapshot => settingsSnapshots.push(snapshot));
  const available = (expected) => {
    assert.equal(host.canRemote(), expected);
    assert.equal(host.bridgeCan('desktopRemoteControl'), expected);
    assert.equal(host.api.desktopRemoteControl.isAvailable(), expected);
  };
  available(false);
  assert.throws(() => host.api.desktopRemoteControl.controllerStatus(), /desktop_remote_control_unsupported/);
  assert.equal(host.calls.includes('desktop_remote_controller_status'), false);
  for (const [os, supported] of [['linux', true], ['macos', false], ['windows', true], ['unknown', false]]) {
    host.setCapabilities({ os, desktopRemoteControlSupported: supported });
    await host.api.platform.loadPlatformCapabilities();
    assert.equal(platformSnapshots.at(-1).platformCapabilities.os, os);
    assert.equal(platformSnapshots.at(-1).platformCapabilities.loaded, true);
    available(supported);
    if (supported) {
      await host.api.desktopRemoteControl.controllerStatus();
      await host.api.desktopRemoteControl.controlledStatus();
      assert.ok(host.calls.includes('desktop_remote_controlled_status'));
    }
  }
  // The backend owns platform support; an adapter must not second-guess it.
  host.setCapabilities({ os: 'future-host', desktopRemoteControlSupported: true });
  await host.api.platform.loadPlatformCapabilities();
  available(true);
  host.setCapabilities({ os: 'windows', desktopRemoteControlSupported: false });
  await host.api.platform.loadPlatformCapabilities();
  available(false);
  host.setCapabilities({ os: 'windows', desktopRemoteControl: true });
  await host.api.platform.loadPlatformCapabilities();
  available(false);
  host.setCapabilities(new Error('unavailable'));
  await host.api.platform.loadPlatformCapabilities();
  available(false);
  assert.equal(platformSnapshots.at(-1).platformCapabilities.loaded, false);
  assert.ok(settingsSnapshots.length > 0, 'precise publication still delivers subscribers');
  assert.ok(settingsSnapshots.every(snapshot => snapshot === settingsSnapshots[0]),
    'platform and remote publication must not invalidate unchanged settings');
  assert.equal(host.bridgeCan('unknownCapability'), false, 'unrelated capabilities must not acquire a permissive fallback');
  unsubscribePlatform();
  unsubscribeSettings();
});
