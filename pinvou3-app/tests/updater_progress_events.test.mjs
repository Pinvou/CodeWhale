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
  test(`${platform} download progress events remain available until the download receipt`, async () => {
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
    assert.equal(state.updateProgress, 37);
    rejectDownload(new Error('fixture'));
    assert.equal(await pending, false);
  });
}

// community #860 回流补钉（语义按 Official 实现适配）：下载失败路径同样必须
// 清掉挂起的 200ms 合并定时器，失败后晚到的进度事件不得再改变状态或触发通知
// （失败复位 updateDownloading 与 windows 门控，监听器直接忽略）。
test('a failed download clears pending progress work and ignores late events', async () => {
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
    listen: (name, callback) => { listener = callback; },
    invoke: (name) => name === 'download_update'
      ? new Promise((resolve, reject) => { rejectDownload = reject; })
      : Promise.resolve(),
  });
  const pending = updater.downloadAndInstallUpdate();
  listener({ payload: { downloaded: 250, total: 1000 } });
  assert.equal(state.updateProgress, 25);
  assert.equal(timers.size, 1, 'a progress event must leave one pending publication');

  rejectDownload(new Error('network unavailable'));
  assert.equal(await pending, false);
  assert.equal(timers.size, 0, 'failure must remove the pending progress publication');
  assert.equal(state.updateDownloading, false);

  const notificationsAfterFailure = notifications;
  listener({ payload: { downloaded: 500, total: 1000 } });
  assert.equal(state.updateProgress, 25, 'late progress after failure must be ignored');
  assert.equal(notifications, notificationsAfterFailure, 'late progress after failure must not notify');
});

// community #860 回流补钉（语义按 Official 实现适配）：无效 payload 必须整体
// 忽略而不是归零；百分比向下取整（24.9% → 24），超出总量的 downloaded 按
// Math.min(99, …) 封顶 99（100 只属于下载完成并校验通过后的完成路径）；
// 重复百分比不得重复排定通知定时器。
test('invalid progress payloads are ignored and out-of-range values clamp to the 99 cap', async () => {
  const source = readFileSync(new URL('../src/platform/tauri/bridge/updater.js', import.meta.url), 'utf8');
  let listener, rejectDownload;
  const timers = new Map(); let nextTimer = 0;
  const window = {
    setTimeout(callback) { timers.set(++nextTimer, callback); return nextTimer; },
    clearTimeout(id) { timers.delete(id); },
    PinvouBridgeShared: { create: () => ({}) },
  };
  vm.runInNewContext(source, { window });
  const state = { updateInfo: { available: true, platform: 'windows' } };
  const updater = window.__PINVOU_TAURI_BRIDGE_FEATURES__.updater({
    state, notify() {}, listen: (name, callback) => { listener = callback; },
    invoke: (name) => name === 'download_update'
      ? new Promise((resolve, reject) => { rejectDownload = reject; })
      : Promise.resolve(),
  });
  const pending = updater.downloadAndInstallUpdate();

  listener({ payload: { total: 0, downloaded: 10 } });
  listener({ payload: { total: 'x', downloaded: 10 } });
  listener({ payload: { total: 100, downloaded: -5 } });
  listener({ payload: {} });
  assert.equal(state.updateProgress, 0, 'an invalid payload must be ignored, not coerced to zero');
  assert.equal(timers.size, 0, 'invalid payloads must not schedule a publication');

  listener({ payload: { downloaded: 249, total: 1000 } });
  assert.equal(state.updateProgress, 24, 'progress must floor instead of rounding 24.9 up to 25');

  listener({ payload: { downloaded: 1500, total: 1000 } });
  assert.equal(state.updateProgress, 99, 'an over-total payload must clamp to the 99 cap (100 stays on the completion path)');
  assert.equal(timers.size, 1);
  [...timers.values()][0](); timers.clear();

  listener({ payload: { downloaded: 1500, total: 1000 } });
  assert.equal(timers.size, 0, 'an unchanged percentage must not schedule a redundant publication');

  rejectDownload(new Error('fixture'));
  assert.equal(await pending, false);
});
