// DEV-23 contract: the remote conversation view consumes only the DEV-20 bridge
// API, keeps the remote context visible, and never touches credentials or
// private model configuration.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { dict } from '../src/shared/i18n-all.js';
import { isTaskTerminal, taskStatusKey } from '../src/features/remote-control/remote-control-logic.mjs';

const view = readFileSync(
  new URL('../src/features/remote-control/RemoteConversationView.jsx', import.meta.url),
  'utf8',
);
const viewCode = view.replace(/\/\/[^\n]*|\/\*[\s\S]*?\*\//g, '');

test('task statuses map to the frozen copy keys and terminals are detected', () => {
  const mapping = {
    admitted: 'statusAdmitted',
    running: 'statusRunning',
    waiting_permission: 'statusWaitingPermission',
    succeeded: 'statusSucceeded',
    failed: 'statusFailed',
    cancelled: 'statusCancelled',
    interrupted: 'statusInterrupted',
    result_unknown: 'statusResultUnknown',
  };
  for (const [status, key] of Object.entries(mapping)) {
    assert.equal(taskStatusKey(status), key);
    for (const language of ['zh', 'en', 'ja']) {
      assert.ok(dict[language].remoteConversation[key], `${language}.remoteConversation.${key}`);
    }
  }
  assert.equal(isTaskTerminal('running'), false);
  for (const status of ['succeeded', 'failed', 'cancelled', 'interrupted']) {
    assert.equal(isTaskTerminal(status), true, `${status} must be terminal`);
  }
  assert.equal(isTaskTerminal('result_unknown'), true);
  assert.equal(isTaskTerminal('admitted'), false);
  assert.equal(isTaskTerminal('waiting_permission'), false);
});

test('the view only uses the desktop bridge API', () => {
  const invokes = [...view.matchAll(/remote\.([A-Za-z]+)\(/g)].map((match) => match[1]);
  // E2E-07：A 端正文/任务路径已切到主控端专用命令（E2E-03/04 的 session、
  // capability 与加密发送入口），仍只能经 bridge 访问。
  invokes.push(
    ...[...view.matchAll(/remote\s*\.?\s*controller([A-Za-z]+)\(/g)].map((match) => `controller${match[1]}`),
  );
  assert.ok(invokes.length >= 4);
  for (const method of invokes) {
    assert.ok(
      [
        'permissionDecide',
        'artifactIndex',
        'artifactDownloadBegin',
        'controllerSessionOpen',
        'controllerCapabilitySend',
        'controllerTaskSend',
        'controllerTaskCancelSend',
        'controllerProfile',
      ].includes(method),
      `unexpected bridge method used by the remote view: ${method}`,
    );
  }
  // 旧路径（前端自建 payload 的 submitRemoteTask / cancelRemoteTask）不得再出现。
  assert.ok(!/submitRemoteTask|cancelRemoteTask/.test(view));
  assert.ok(!/window\.__TAURI__|invoke\(/.test(view));
});

test('credentials and host paths stay out of the view', () => {
  assert.ok(!/apiKey|api_key|credential_ref|credentialRef|savedModels|prefs|credential/i.test(viewCode));
  assert.ok(!/['"`]sk-[A-Za-z0-9_-]{8,}/.test(viewCode), 'no literal API key may appear in the view');
});

test('remote context, offline copies and unknown results are explicit', () => {
  assert.ok(view.includes('remoteBadge'));
  assert.ok(view.includes('offlineCopy'));
  assert.ok(view.includes('offlineHint'));
  assert.ok(view.includes('resultUnknownHint'));
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(dict[language].remoteConversation.remoteBadge);
    assert.ok(dict[language].remoteConversation.offlineHint);
    assert.ok(dict[language].remoteConversation.resultUnknownHint);
  }
});

test('the view is keyboard reachable with labelled controls', () => {
  assert.ok(view.includes('aria-labelledby'));
  assert.ok(view.includes('role="alert"'));
  assert.ok(view.includes('aria-live'));
  assert.ok(view.includes('<fieldset'));
  assert.ok(view.includes('<button'));
  assert.ok(view.includes('type="radio"'));
});
