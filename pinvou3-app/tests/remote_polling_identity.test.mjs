import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { remoteProjectionPollRevision } from '../src/features/remote-control/remote-session-adapter.mjs';
import { permissionPollingTaskIds, startPermissionPolling } from '../src/features/remote-control/controlled-device-panel-state.mjs';
import { dictEn } from '../src/shared/i18n/en.js';
import { dictZh } from '../src/shared/i18n/zh.js';
import { dictJa } from '../src/shared/i18n/ja.js';

test('message chunks do not retrigger projection polls; relevant lifecycle entries do', () => {
  const relevant = { kind: 'approval', journalEpoch: 'e1', sequence: 1, eventId: 'p1', taskId: 't1', body: 'pending' };
  const a = { entries: [relevant] };
  const b = { entries: [relevant, { kind: 'assistant_message', body: 'stream' }] };
  assert.equal(remoteProjectionPollRevision(a, ['approval', 'status']), remoteProjectionPollRevision(b, ['approval', 'status']));
  assert.notEqual(remoteProjectionPollRevision(a, ['approval']), remoteProjectionPollRevision({ entries: [{ ...relevant, sequence: 2, body: 'allowed' }] }, ['approval']));
  assert.notEqual(remoteProjectionPollRevision(a, ['approval']), remoteProjectionPollRevision({ entries: [] }, ['approval']));
});

test('B permission poll identity ignores chunks and ordering, not waiting membership', () => {
  const tasks = [{ taskId: 't2', status: 'waiting_permission' }, { taskId: 't1', status: 'waiting_permission' }];
  assert.equal(permissionPollingTaskIds(tasks), permissionPollingTaskIds(tasks.toReversed().map(t => ({ ...t, text: 'new chunk' }))));
  assert.notEqual(permissionPollingTaskIds(tasks), permissionPollingTaskIds([{ ...tasks[0], status: 'running' }, tasks[1]]));
});

test('artifact preview close uses a real nonempty three-language accessibility key', () => {
  const source = readFileSync(new URL('../src/features/chat/ChatView.jsx', import.meta.url), 'utf8');
  assert.match(source, /data-testid="chat-remote-artifact-preview-close"[^>]*aria-label=\{t.uiArtifacts.closePreviewAria\}/);
  for (const dict of [dictZh, dictEn, dictJa]) assert.ok(dict.uiArtifacts.closePreviewAria.trim());
});

test('same waiting task gains new approvals without message-triggered reads; no overlap or late replies', async () => {
  let tick; let release; let calls = 0; let cancelled = false;
  const seen = [];
  const stop = startPermissionPolling({
    read: () => { calls += 1; return new Promise(resolve => { release = resolve; }); },
    onSuccess: value => seen.push(value), onError: error => assert.fail(error),
    schedule: (callback, ms) => { tick = callback; assert.equal(ms, 2000); return 1; },
    cancel: id => { assert.equal(id, 1); cancelled = true; },
  });
  tick(); assert.equal(calls, 1);
  release([{ taskId: 't1', permissionRequestId: 'p1' }]);
  await new Promise(resolve => setImmediate(resolve));
  tick(); assert.equal(calls, 2);
  release([{ taskId: 't1', permissionRequestId: 'p1' }, { taskId: 't1', permissionRequestId: 'p2' }]);
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(seen.at(-1).length, 2);
  tick(); assert.equal(calls, 3);
  stop(); release([{ permissionRequestId: 'late' }]);
  await new Promise(resolve => setImmediate(resolve));
  tick(); assert.equal(calls, 3); assert.equal(seen.length, 2); assert.ok(cancelled);
});
