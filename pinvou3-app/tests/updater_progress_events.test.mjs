import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';

test('download progress is displayed, throttled, and ignored after cancellation', async () => {
  const source = readFileSync(new URL('../src/platform/tauri/bridge/updater.js', import.meta.url), 'utf8');
  let listener, rejectDownload, notifications = 0;
  const timers = new Map(); let nextTimer = 0;
  const window = {
    setTimeout(callback) { timers.set(++nextTimer, callback); return nextTimer; },
    clearTimeout(id) { timers.delete(id); },
    PinvouBridgeShared: { create: () => ({}) },
  };
  vm.runInNewContext(source, { window });
  const state = { updateInfo: { available: true, platform: 'windows' } };
  const updater = window.__PINVOU_TAURI_BRIDGE_FEATURES__.updater({
    state, notify: () => { notifications++; },
    listen: (name, callback) => { assert.equal(name, 'update:progress'); listener = callback; },
    invoke: (name) => name === 'download_update'
      ? new Promise((resolve, reject) => { rejectDownload = reject; })
      : Promise.resolve(),
  });
  const pending = updater.downloadAndInstallUpdate();
  listener({ payload: { downloaded: 37, total: 100 } });
  listener({ payload: { downloaded: 40, total: 100 } });
  assert.equal(state.updateProgress, 40);
  assert.equal(timers.size, 1);
  [...timers.values()][0](); timers.clear();
  assert.equal(notifications, 2);
  listener({ payload: { downloaded: 100, total: 100 } });
  assert.equal(state.updateProgress, 99);
  updater.cancelUpdate();
  listener({ payload: { downloaded: 99, total: 100 } });
  rejectDownload(new Error('cancelled'));
  assert.equal(await pending, false);
  assert.equal(state.updateProgress, 0);
  assert.equal(timers.size, 0);
});

for (const platform of ['linux', 'macos']) {
  test(`${platform} progress events retain the existing behavior`, async () => {
    const source = readFileSync(new URL('../src/platform/tauri/bridge/updater.js', import.meta.url), 'utf8');
    let listener, rejectDownload;
    const window = { setTimeout, clearTimeout, PinvouBridgeShared: { create: () => ({}) } };
    vm.runInNewContext(source, { window });
    const state = { updateInfo: { available: true, platform } };
    const updater = window.__PINVOU_TAURI_BRIDGE_FEATURES__.updater({
      state, notify() {}, listen: (name, callback) => { listener = callback; },
      invoke: (name) => name === 'download_update'
        ? new Promise((resolve, reject) => { rejectDownload = reject; })
        : Promise.resolve(),
    });
    const pending = updater.downloadAndInstallUpdate();
    listener({ payload: { downloaded: 37, total: 100 } });
    assert.equal(state.updateProgress, 0);
    rejectDownload(new Error('fixture'));
    assert.equal(await pending, false);
  });
}
