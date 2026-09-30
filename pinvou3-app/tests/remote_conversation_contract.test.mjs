// DEV-31: the connection shell is not a second conversation owner. Remote
// conversation rendering and dispatch are confined to the active ChatView ID.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { dict } from './helpers/i18n-all.js';
import { isTaskTerminal, taskStatusKey } from '../src/features/remote-control/remote-control-logic.mjs';

const shell = readFileSync(new URL('../src/features/remote-control/RemoteConnectionShell.jsx', import.meta.url), 'utf8');
const chat = readFileSync(new URL('../src/features/chat/ChatView.jsx', import.meta.url), 'utf8');
const main = readFileSync(new URL('../src/app/main.jsx', import.meta.url), 'utf8');
const navigation = readFileSync(new URL('../src/components/layout/NavigationComponents.jsx', import.meta.url), 'utf8');

test('frozen task status copy remains available for the unified ChatView', () => {
  const mapping = {
    admitted: 'statusAdmitted', running: 'statusRunning', waiting_permission: 'statusWaitingPermission',
    succeeded: 'statusSucceeded', failed: 'statusFailed', cancelled: 'statusCancelled',
    interrupted: 'statusInterrupted', result_unknown: 'statusResultUnknown',
  };
  for (const [status, key] of Object.entries(mapping)) {
    assert.equal(taskStatusKey(status), key);
    for (const language of ['zh', 'en', 'ja']) assert.ok(dict[language].remoteConversation[key]);
  }
  assert.equal(isTaskTerminal('running'), false);
  for (const status of ['succeeded', 'failed', 'cancelled', 'interrupted', 'result_unknown']) {
    assert.equal(isTaskTerminal(status), true);
  }
});

test('A connection shell contains no second transcript, task list, history or composer', () => {
  assert.ok(!/RemoteConversationView|transcript|remoteHistories|remote-tasks|remote-conversation-composer|<form/.test(shell));
  assert.ok(shell.includes('onRequestDisconnect'));
  assert.ok(shell.includes('onClose'));
  assert.ok(chat.includes('remoteProxyChatItems(activeSessionId, remoteProxyHistory)'));
  assert.match(chat, /submitRemoteTask\(\s*remoteBinding\.localSessionId, remoteBinding\.remoteRef, intent/);
  assert.match(chat, /readRemoteProxyHistory\(remoteBinding\.localSessionId\)[\s\S]*?recordRemoteDispatch\(/);
  assert.match(chat, /if \(outcome\.state !== 'queued'\) return 'held'/);
  assert.ok(!chat.includes('bs ? bs.transcript : []'));
  assert.ok(!chat.includes('bs ? bs.tasks : []'));
});

test('remote and offline labels are localized at the single conversation boundary', () => {
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(dict[language].remoteConversation.remoteBadge);
    assert.ok(dict[language].remoteConversation.offlineCopy);
    assert.ok(dict[language].remoteConversation.resultUnknownHint);
  }
  assert.ok(chat.includes('t.remoteConversation.offlineCopy'));
});

test('RemoteProxy sidebar offers only supported delete, not local metadata or folder actions', () => {
  assert.ok(main.includes("chat.sessionKind === 'RemoteProxy' ? undefined : handleRenameSession"));
  assert.ok(main.includes("chat.sessionKind === 'RemoteProxy' ? undefined : handleToggleSessionPinned"));
  assert.ok(main.includes("chat.sessionKind === 'RemoteProxy' ? undefined : handleArchiveSession"));
  assert.ok(main.includes("chat.sessionKind !== 'RemoteProxy' && can('externalSystemOpen')"));
  assert.ok(navigation.includes('{onTogglePinned && <button'));
  assert.ok(navigation.includes('{onRename && <button'));
});
