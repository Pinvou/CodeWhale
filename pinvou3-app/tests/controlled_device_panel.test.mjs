import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import { fileURLToPath } from 'node:url';
import { dict } from './helpers/i18n-all.js';
import {
  countdownAnnouncement, countdownSeconds, currentControlledTasks, normalizeSelectedWorkspaces,
  permissionOperationCopyKey, permissionRedactionCopyKey,
} from '../src/features/remote-control/controlled-device-panel-state.mjs';

const app = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const read = relative => fs.readFileSync(path.join(app, 'src', relative), 'utf8');

test('B panel announces countdown sparsely while the visible clock still ticks each second', () => {
  assert.equal(countdownAnnouncement(300), 300);
  assert.equal(countdownAnnouncement(299), null);
  assert.equal(countdownAnnouncement(30), 30);
  assert.equal(countdownAnnouncement(29), null);
  assert.equal(countdownAnnouncement(10), 10);
  assert.equal(countdownAnnouncement(1), 1);
  assert.equal(countdownAnnouncement(0), null);
  // A Rust monotonic deadline such as 30_000 is not a JS wall-clock epoch.
  assert.equal(countdownSeconds(30_000, 1_800_000_000_000, 1_800_000_000_000), 30);
  assert.equal(countdownSeconds(30_000, 1_800_000_000_000, 1_800_000_001_001), 29);
  assert.equal(countdownSeconds(30_000, 1_800_000_000_000, 1_800_000_031_000), 0);
  assert.equal(countdownSeconds(undefined, 1000, 1000), 0);
  const panel = read('features/remote-control/ControlledDevicePanel.jsx');
  const settings = read('features/remote-control/RemoteControlSettingsPage.jsx');
  assert.match(settings, /setInterval\(\(\) => setControlledNow\(Date\.now\(\)\), 1000\)/);
  assert.match(settings, /controlledDevice\.pairingRemainingMs/);
  assert.ok(!settings.includes('controlledDevice.expiresAtMs'));
  assert.ok(!panel.includes('device.expiresAtMs'));
  assert.match(panel, /aria-live="polite"/);
  assert.match(panel, /countdownAnnouncement\(seconds\)/);
  assert.ok(!panel.includes("replace(/\\s+/g, '')"), 'B must display and copy only the exact seven-character code');
});

test('workspace preauthorization is deduplicated and passed only to authenticated B start', () => {
  assert.deepEqual(normalizeSelectedWorkspaces([
    'C:\\Work\\Alpha', 'C:\\Work\\Alpha', 'D:\\Beta', '', null,
  ]), [
    { path: 'C:\\Work\\Alpha', displayName: 'Alpha', codeAllowed: true },
    { path: 'D:\\Beta', displayName: 'Beta', codeAllowed: true },
  ]);
  assert.equal(normalizeSelectedWorkspaces(Array.from({ length: 20 }, (_, index) => `C:\\Folder${index}`)).length, 16,
    'the picker must not stage more workspaces than the B manager can admit');
  assert.equal([...normalizeSelectedWorkspaces(`C:\\${'長'.repeat(121)}`)[0].displayName].length, 120,
    'the displayed folder label must meet the B manager character limit');
  const panel = read('features/remote-control/ControlledDevicePanel.jsx');
  assert.match(panel, /controlledStart\(null, stagedWorkspaces\)/);
  assert.match(panel, /controlledListWorkspaces\(\)/);
  assert.match(panel, /setWorkspaceReadError\(active && !!device\.generation && !remote\?\.controlledListWorkspaces\)/);
  assert.match(panel, /if \(!Array\.isArray\(list\)\)/);
  assert.ok(!panel.includes('controlledAuthorizeWorkspace('), 'cannot grant new scope after pairing');
  assert.ok(!panel.includes('localStorage'));
});

test('task and permission cards use only the B authoritative session and generation', () => {
  const tasks = [
    { taskId: 'same', sessionId: 'b-1', status: 'running', modelSource: 'controlled_device' },
    { taskId: 'foreign', sessionId: 'b-2', status: 'running', modelSource: 'controller_temporary' },
    { taskId: 'done', sessionId: 'b-1', status: 'succeeded' },
  ];
  assert.deepEqual(currentControlledTasks(
    { status: 'connected', generation: 'g-1' }, { sessionId: 'b-1', generation: 'g-1' }, tasks,
  ).map(task => task.taskId), ['same']);
  assert.deepEqual(currentControlledTasks(
    { status: 'connected', generation: 'g-2' }, { sessionId: 'b-1', generation: 'g-1' }, tasks,
  ), []);
  for (const recovery of ['reconnecting', 'resume_provisional', 'resume_activated_pending_liveness']) {
    assert.deepEqual(currentControlledTasks(
      { status: recovery, generation: 'g-1' }, { sessionId: 'b-1', generation: 'g-1' }, tasks,
    ).map(task => task.taskId), ['same'], `${recovery} retains the bound task for local stop`);
  }
  const panel = read('features/remote-control/ControlledDevicePanel.jsx');
  assert.match(panel, /controlledPermissionList\(task\.taskId\)/);
  assert.doesNotMatch(panel, /remote\.permissionList\(/);
  assert.match(panel, /setPermissionReadStatus\(waiting\.length > 0 \? 'error' : 'idle'\)/);
  assert.match(panel, /controlledStopTask\(task\.taskId\)/);
  assert.match(panel, /aria-label=\{copy\.stopTaskForTask\(task\.taskId\)\}/);
  assert.match(panel, /controlledDisconnectNow\(\)/);
  assert.match(panel, /CONTROLLED_BOUND_STATUSES\.has\(status\)/);
  assert.ok(!/permissionDecide|\bAllow\b|\bApprove\b/.test(panel));
});

test('permission redaction sentinels become localized, readable copy and unknown values never echo', () => {
  assert.equal(permissionRedactionCopyKey('[PATH]'), 'permissionPathHidden');
  assert.equal(permissionRedactionCopyKey('[REDACTED]'), 'permissionSensitiveHidden');
  assert.equal(permissionRedactionCopyKey(''), null);
  assert.equal(permissionRedactionCopyKey(null), null);
  assert.equal(permissionRedactionCopyKey(undefined), null);
  for (const unexpected of ['[path]', ' [PATH]', '[PATH]suffix', '[REDACTED]\nsecret', 'C:\\private', 42, {}]) {
    assert.equal(permissionRedactionCopyKey(unexpected), 'permissionDetailHidden');
  }
  for (const language of ['zh', 'en', 'ja']) {
    const copy = dict[language].controlledDevice;
    for (const key of ['permissionPathHidden', 'permissionSensitiveHidden', 'permissionDetailHidden',
      'permissionTargetScopeLabel', 'permissionPreviewLabel']) {
      assert.ok(typeof copy[key] === 'string' && copy[key].length > 0, `${language}: ${key}`);
      assert.ok(!copy[key].includes('[PATH]') && !copy[key].includes('[REDACTED]'));
    }
  }
  const panel = read('features/remote-control/ControlledDevicePanel.jsx');
  assert.match(panel, /targetScopeCopyKey: permissionRedactionCopyKey\(item\.targetScope\)/);
  assert.match(panel, /previewCopyKey: permissionRedactionCopyKey\(item\.preview\)/);
  assert.match(panel, /className="sr-only">\{copy\.permissionTargetScopeLabel\}: /);
  assert.match(panel, /className="sr-only">\{copy\.permissionPreviewLabel\}: /);
  assert.match(panel, /copy\[permission\.targetScopeCopyKey\]/);
  assert.match(panel, /copy\[permission\.previewCopyKey\]/);
  assert.ok(!/\{permission\.(?:targetScope|preview)\}/.test(panel), 'raw backend sentinels must never reach the DOM');
  assert.ok(!/String\(item\.(?:targetScope|preview)/.test(panel), 'raw unexpected details must not be stored');
});

test('B permission operations render only six localized enum labels and a safe unknown fallback', () => {
  const cases = [
    ['file_write', 'permissionOperationFileWrite'],
    ['file_delete', 'permissionOperationFileDelete'],
    ['shell_command', 'permissionOperationShellCommand'],
    ['network_request', 'permissionOperationNetworkRequest'],
    ['tool_call', 'permissionOperationToolCall'],
    ['other', 'permissionOperationOther'],
  ];
  for (const [operation, key] of cases) assert.equal(permissionOperationCopyKey(operation), key);
  for (const unknown of ['', null, undefined, 'FILE_WRITE', 'file_write ', 'credential_dump', {}, 1]) {
    assert.equal(permissionOperationCopyKey(unknown), 'permissionOperationUnknown');
  }
  for (const language of ['zh', 'en', 'ja']) {
    const copy = dict[language].controlledDevice;
    assert.ok(copy.permissionOperationLabel, `${language}: accessible operation label`);
    for (const [, key] of cases) {
      assert.ok(typeof copy[key] === 'string' && copy[key].length > 0, `${language}: ${key}`);
      assert.ok(!copy[key].includes('_'), `${language}: no raw snake_case enum`);
    }
    assert.ok(copy.permissionOperationUnknown, `${language}: unknown operation`);
  }
  const panel = read('features/remote-control/ControlledDevicePanel.jsx');
  assert.match(panel, /operationTypeCopyKey: permissionOperationCopyKey\(item\.operationType\)/);
  assert.match(panel, /className="sr-only">\{copy\.permissionOperationLabel\}: /);
  assert.match(panel, /copy\[permission\.operationTypeCopyKey\]/);
  assert.ok(!/\{permission\.operationType\}/.test(panel), 'raw operation type must not reach the DOM');
  assert.ok(!panel.includes('String(item.operationType'), 'raw operation type must not be retained in panel state');
});

test('B controls have trilingual, keyboard, focus, high-contrast and reduced-motion affordances', () => {
  const panel = read('features/remote-control/ControlledDevicePanel.jsx');
  assert.match(panel, /role="switch"|<Toggle/);
  assert.match(panel, /focus-visible:/);
  assert.match(panel, /motion-reduce:/);
  assert.match(panel, /forced-colors:/);
  assert.match(panel, /headingRef\.current\?\.focus\(\)/);
  for (const language of ['zh', 'en', 'ja']) {
    const copy = dict[language].controlledDevice;
    for (const key of ['disconnectNow', 'stopTask', 'connectedDuration', 'modelSourceUnknown',
      'permissionDetails', 'workspaceStageHint', 'taskStatusRunning']) {
      assert.ok(copy[key], `${language}: ${key}`);
    }
    assert.equal(typeof copy.stopTaskForTask, 'function');
  }
  assert.ok(!panel.includes('window.__TAURI__'));
});
