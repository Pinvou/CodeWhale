import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { dict } from './helpers/i18n-all.js';
import {
  controlledTaskChatItems,
  isRemoteChatActive,
  mergeControlledTaskChatItems,
  remoteChatItems,
  remotePermissionFailureText,
} from '../src/features/remote-control/chat-transcript-projection.mjs';

const connection = { status: 'connected', sessionId: 'session-1' };
const projectControlled = tasks => {
  const sessionTasks = tasks.map(task => ({ sessionId: 'session-b', status: 'succeeded', ...task }));
  return controlledTaskChatItems(sessionTasks,
    { statusCancelled: 'Cancelled', statusFailed: 'Failed' }, 'session-b');
};

test('translated permission errors reconcile only the matching durable turn without discarding raw evidence', () => {
  const code = 'task_permission_mode_changed';
  const copy = dict.zh.remoteConversation;
  const history = [
    { id: 'old-user', type: 'user', text: 'same prompt', messageIndex: 0 },
    { id: 'old-error', type: 'assistant', text: code, messageIndex: 1 },
    { id: 'current-user', type: 'user', text: 'same prompt', messageIndex: 2 },
    { id: 'current-error', type: 'assistant', text: code, messageIndex: 3 },
  ];
  const snapshot = structuredClone(history);
  for (const errorText of ['', code]) {
    const projected = controlledTaskChatItems([{
      sessionId: 'session-1', taskId: 'current-task', status: 'failed',
      text: 'same prompt', messageIndex: 2, terminalReason: code, errorText,
    }], copy, 'session-1');
    const merged = mergeControlledTaskChatItems(history, projected);
    assert.equal(merged.length, 4);
    assert.deepEqual(merged.slice(0, 2), snapshot.slice(0, 2));
    assert.equal(merged[3].id, 'current-error');
    assert.equal(merged[3].controlledFailureReason, code);
    assert.ok(merged[3].text.includes(copy.permissionModeChanged));
    assert.ok(merged[3].text.includes(code));
    assert.deepEqual(history, snapshot);
  }
});

test('controlled permission failures show explicit local guidance in every language while retaining the evidence code', () => {
  const keys = {
    task_permission_mode_required: 'permissionModeRequired',
    task_permission_mode_changed: 'permissionModeChanged',
    task_permission_mode_unsupported: 'permissionModeUnsupported',
  };
  for (const language of ['zh', 'en', 'ja']) {
    const copy = dict[language].remoteConversation;
    for (const [code, key] of Object.entries(keys)) {
      assert.ok(Object.hasOwn(copy, key));
      assert.ok(copy[key]);
      for (const errorText of ['', code]) {
        const items = controlledTaskChatItems([{
          sessionId: 'session-1', taskId: 'permission-task', status: 'failed',
          text: 'accepted task', terminalReason: code, errorText,
        }], copy, 'session-1');
        const failure = items.find(item => /:terminal$|:error$/.test(item.id));
        assert.ok(failure.text.includes(copy[key]));
        assert.ok(failure.text.includes(code));
        if (!errorText) assert.equal(failure.controlledFailureReason, code);
      }
    }
    for (const unknown of ['constructor', '__proto__', 'provider_failure', null, {}]) {
      assert.equal(remotePermissionFailureText(unknown, copy), null);
    }
  }
});
const transcript = [
  { sessionId: 'session-1', epoch: 'e1', sequence: 1, role: 'user', text: 'hello' },
  { sessionId: 'session-1', epoch: 'e1', sequence: 2, role: 'assistant', text: 'a' },
  { sessionId: 'session-1', epoch: 'e1', sequence: 3, role: 'assistant', text: 'b' },
  { sessionId: 'session-old', epoch: 'e0', sequence: 4, role: 'assistant', text: 'ignored' },
];

test('only the live remote session projects into ChatView items', () => {
  assert.equal(isRemoteChatActive(connection), true);
  assert.equal(isRemoteChatActive({ status: 'idle' }), false);
  assert.deepEqual(remoteChatItems({ status: 'idle' }, transcript), []);

  const items = remoteChatItems(connection, transcript);
  assert.deepEqual(items.map(item => item.type), ['user', 'assistant']);
  assert.equal(items[0].text, 'hello');
  assert.equal(items[1].text, 'ab');
  assert.equal(items[1].remote, true);
  assert.equal(items[1].streaming, false);
});

test('B journal entries are matched by its authoritative local session ID', () => {
  const items = remoteChatItems(
    { status: 'connected', sessionId: 'wire-session-1' },
    [{ sessionId: 'wire-session-1', epoch: 'e1', sequence: 1, role: 'user', text: 'sent' }],
  );
  assert.equal(items.length, 1);
  assert.equal(items[0].text, 'sent');
});

test('a queued task supplies its prompt until the authoritative user journal arrives', () => {
  const tasks = [{
    taskId: 'task-1',
    sessionId: 'session-1',
    status: 'admitted',
    text: 'visible immediately',
  }];
  const fallback = remoteChatItems(connection, [], tasks);
  assert.equal(fallback.length, 1);
  assert.equal(fallback[0].id, 'remote-task-task-1');
  assert.equal(fallback[0].text, 'visible immediately');

  const authoritative = remoteChatItems(connection, [{
    sessionId: 'session-1',
    epoch: 'e1',
    sequence: 1,
    taskId: 'task-1',
    role: 'user',
    text: 'visible immediately',
  }], tasks);
  assert.equal(authoritative.length, 1, 'the journal must replace, not duplicate, the fallback');
  assert.equal(authoritative[0].id, 'remote-session-1:e1:task-1:user:1');
});

test('consecutive remote turns keep independent reasoning, answers, and terminal rendering state', () => {
  const entries = [
    { sessionId: 'session-1', epoch: 'e1', sequence: 1, taskId: 'task-1', role: 'user', text: 'first' },
    { sessionId: 'session-1', epoch: 'e1', sequence: 2, taskId: 'task-1', role: 'reasoning', text: 'think ' },
    { sessionId: 'session-1', epoch: 'e1', sequence: 3, taskId: 'task-1', role: 'reasoning', text: 'one' },
    { sessionId: 'session-1', epoch: 'e1', sequence: 4, taskId: 'task-1', role: 'assistant', text: '## Result 1' },
    { sessionId: 'session-1', epoch: 'e1', sequence: 5, taskId: 'task-2', role: 'user', text: 'second' },
    { sessionId: 'session-1', epoch: 'e1', sequence: 6, taskId: 'task-2', role: 'reasoning', text: 'think two' },
    { sessionId: 'session-1', epoch: 'e1', sequence: 7, taskId: 'task-2', role: 'assistant', text: '| A | B |' },
  ];
  const tasks = [
    { taskId: 'task-1', sessionId: 'session-1', status: 'succeeded', text: 'first' },
    { taskId: 'task-2', sessionId: 'session-1', status: 'running', text: 'second' },
  ];
  const items = remoteChatItems(connection, entries, tasks);
  assert.deepEqual(items.map(item => item.type), [
    'user', 'reasoning', 'assistant', 'user', 'reasoning', 'assistant',
  ]);
  assert.equal(items[1].text, 'think one');
  assert.equal(items[2].text, '## Result 1');
  assert.equal(items[2].streaming, false, 'an earlier answer remains renderable after turn two starts');
  assert.equal(items[4].streaming, true);
  assert.equal(items[5].streaming, true);

  const completed = remoteChatItems(connection, entries, [
    tasks[0],
    { ...tasks[1], status: 'succeeded' },
  ]);
  assert.equal(completed[5].streaming, false, 'terminal Markdown is rendered from text');
});

test('a prompt fallback is inserted before its task output instead of splitting another turn', () => {
  const items = remoteChatItems(connection, [
    { sessionId: 'session-1', epoch: 'e1', sequence: 1, taskId: 'task-1', role: 'user', text: 'first' },
    { sessionId: 'session-1', epoch: 'e1', sequence: 2, taskId: 'task-1', role: 'assistant', text: 'first result' },
    { sessionId: 'session-1', epoch: 'e1', sequence: 4, taskId: 'task-2', role: 'assistant', text: 'second result' },
  ], [
    { taskId: 'task-1', sessionId: 'session-1', status: 'succeeded', text: 'first' },
    { taskId: 'task-2', sessionId: 'session-1', status: 'succeeded', text: 'second' },
  ]);
  assert.deepEqual(items.map(item => item.text), [
    'first', 'first result', 'second', 'second result',
  ]);
});

test('controlled-side projection accepts its local session task without an A-side session filter', () => {
  const items = remoteChatItems(
    { status: 'connected' },
    [],
    [{ taskId: 'task-b', sessionId: 'local-b-session', status: 'running', text: 'from controller' }],
  );
  assert.equal(items.length, 1);
  assert.equal(items[0].text, 'from controller');
});

test('the controlled device keeps accepted prompts and terminal failures as conversation items', () => {
  const items = controlledTaskChatItems([
    {
      taskId: 'task-1',
      sessionId: 'session-b',
      status: 'failed',
      text: 'check the weather',
      terminalReason: 'task_engine_unavailable',
    },
  ], {
    statusFailed: '执行失败',
  }, 'session-b');
  assert.deepEqual(items.map(item => item.type), ['user', 'assistant']);
  assert.equal(items[0].text, 'check the weather');
  assert.equal(items[0].remote, true);
  assert.equal(items[1].text, '执行失败: task_engine_unavailable');
  assert.equal(items[1].streaming, false);
});

test('the controlled device projects assistant output for a completed task', () => {
  const items = controlledTaskChatItems([
    {
      taskId: 'task-2',
      sessionId: 'session-b',
      status: 'succeeded',
      text: 'hello',
      assistantText: 'world',
    },
  ], {}, 'session-b');
  assert.deepEqual(items.map(item => item.text), ['hello', 'world']);
  assert.equal(items[1].streaming, false);
});

test('controlled projection filters sessions, preserves real history, and keeps partial failure visible', () => {
  const projected = controlledTaskChatItems([
    {
      taskId: 'task-current',
      sessionId: 'session-b',
      status: 'failed',
      text: 'same prompt',
      assistantText: 'partial output',
      terminalReason: 'task_turn_failed',
    },
    {
      taskId: 'task-foreign',
      sessionId: 'other-session',
      status: 'failed',
      text: 'must not leak',
    },
  ], { statusFailed: '执行失败' }, 'session-b');
  assert.deepEqual(
    projected.map(item => item.text),
    ['same prompt', 'partial output', '执行失败: task_turn_failed'],
  );

  const merged = mergeControlledTaskChatItems([
    { id: 'local-history', type: 'assistant', text: 'older local history' },
    { id: 'engine-user', type: 'user', text: 'same prompt' },
    { id: 'engine-partial', type: 'assistant', text: 'partial output' },
    { id: 'persisted-failure', type: 'assistant', text: 'task_turn_failed' },
  ], projected);
  assert.deepEqual(
    merged.map(item => item.text),
    ['older local history', 'same prompt', 'partial output', '执行失败: task_turn_failed'],
  );
});

test('a repeated failure code is matched only inside its task turn', () => {
  const projected = controlledTaskChatItems([
    {
      taskId: 'task-current',
      sessionId: 'session-b',
      status: 'failed',
      text: 'current prompt',
      messageIndex: 2,
      terminalReason: 'task_engine_unavailable',
    },
  ], { statusFailed: '执行失败' }, 'session-b');
  const merged = mergeControlledTaskChatItems([
    { id: 'old-user', type: 'user', text: 'old prompt', messageIndex: 0 },
    { id: 'old-error', type: 'assistant', text: 'task_engine_unavailable', messageIndex: 1 },
    { id: 'current-user', type: 'user', text: 'current prompt', messageIndex: 2 },
  ], projected);
  assert.deepEqual(
    merged.map(item => item.text),
    ['old prompt', 'task_engine_unavailable', 'current prompt', '执行失败: task_engine_unavailable'],
  );
});

test('an immediately failed repeated prompt is appended when its durable turn is not hydrated yet', () => {
  const projected = controlledTaskChatItems([
    {
      taskId: 'task-repeat',
      sessionId: 'session-b',
      status: 'failed',
      text: 'same prompt',
      messageIndex: 2,
      terminalReason: 'task_engine_unavailable',
    },
  ], { statusFailed: '执行失败' }, 'session-b');
  const merged = mergeControlledTaskChatItems([
    { id: 'previous-user', type: 'user', text: 'same prompt', messageIndex: 0 },
    { id: 'previous-error', type: 'assistant', text: 'task_engine_unavailable', messageIndex: 1 },
  ], projected);
  assert.deepEqual(
    merged.map(item => item.text),
    [
      'same prompt',
      'task_engine_unavailable',
      'same prompt',
      '执行失败: task_engine_unavailable',
    ],
  );
});

test('controlled task overlays match the authoritative journal by task id', () => {
  const journalItems = remoteChatItems({ status: 'connected', sessionId: 'session-b' }, [
    {
      sessionId: 'session-b',
      epoch: 'epoch-1',
      sequence: 1,
      taskId: 'task-old',
      role: 'user',
      text: 'same prompt',
    },
    {
      sessionId: 'session-b',
      epoch: 'epoch-1',
      sequence: 2,
      taskId: 'task-current',
      role: 'user',
      text: 'same prompt',
    },
  ]);
  const projected = controlledTaskChatItems([{
    taskId: 'task-current',
    sessionId: 'session-b',
    status: 'failed',
    text: 'same prompt',
    messageIndex: 10,
    terminalReason: 'task_engine_unavailable',
  }], { statusFailed: '执行失败' }, 'session-b');

  const merged = mergeControlledTaskChatItems(journalItems, projected);
  assert.deepEqual(
    merged.map(item => item.text),
    ['same prompt', 'same prompt', '执行失败: task_engine_unavailable'],
  );
});

test('a multi-tool answer converges without a stale copy after later tasks', () => {
  const history = [
    { id: 'u1', type: 'user', text: 'first', messageIndex: 4 },
    { id: 'a1', type: 'assistant', text: 'Checking. ' },
    { id: 'tool1', type: 'tool', text: 'tool result' },
    { id: 'a2', type: 'assistant', text: 'Reading. ' },
    { id: 'think1', type: 'reasoning', text: 'reasoning' },
    { id: 'a3', type: 'assistant', text: 'Done.' },
    { id: 'u2', type: 'user', text: 'second', messageIndex: 12 },
    { id: 'a4', type: 'assistant', text: 'Next answer.' },
  ];
  const overlay = projectControlled([
    { taskId: 't1', text: 'first', messageIndex: 4, assistantText: 'Checking. Reading. Done.' },
    { taskId: 't2', text: 'second', messageIndex: 12, assistantText: 'Next answer.' },
  ]);
  assert.deepEqual(mergeControlledTaskChatItems(history, overlay), history);
  assert.equal(history.length, 8, 'the durable input is never mutated');
});

test('streaming overlays show only the uncovered suffix within the original task', () => {
  const history = [
    { type: 'user', text: 'first', messageIndex: 0 },
    { type: 'assistant', text: 'Check. ' },
    { type: 'tool', text: 'tool' },
    { type: 'reasoning', text: 'reasoning' },
    { type: 'assistant', text: 'Do' },
    { type: 'user', text: 'next', messageIndex: 7 },
    { type: 'assistant', text: 'next answer' },
  ];
  const overlay = projectControlled([
    { taskId: 't1', text: 'first', messageIndex: 0, status: 'running', assistantText: 'Check. Done.' },
  ]);
  const merged = mergeControlledTaskChatItems(history, overlay);
  assert.deepEqual(merged.map(item => item.text), [
    'first', 'Check. ', 'tool', 'reasoning', 'Do', 'ne.', 'next', 'next answer',
  ]);
  assert.equal(merged[5].streaming, true);
  assert.deepEqual(mergeControlledTaskChatItems([
    ...history.slice(0, 4), { type: 'assistant', text: 'Done.' }, ...history.slice(5),
  ], overlay).map(item => item.text), [
    'first', 'Check. ', 'tool', 'reasoning', 'Done.', 'next', 'next answer',
  ], 'reconnect/hydration replaces the suffix');
});

test('a durable stream ahead of task events is preserved without an older prefix copy', () => {
  const history = [{ type: 'user', text: 'first', messageIndex: 0 },
    { type: 'assistant', text: 'Complete result.' }];
  const overlay = projectControlled([{ taskId: 't1', text: 'first', messageIndex: 0,
    status: 'running', assistantText: 'Complete' }]);
  assert.deepEqual(mergeControlledTaskChatItems(history, overlay), history);
});

test('mid-turn steer bubbles do not cut off the same task output lane', () => {
  const history = [
    { type: 'user', text: 'first', messageIndex: 8 },
    { type: 'assistant', text: 'Checking. ' },
    { type: 'user', text: 'also check this', steeredMidTurn: true, messageIndex: 10 },
    { type: 'assistant', text: 'Done.' },
    { type: 'user', text: 'next', messageIndex: 14 },
  ];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'first', messageIndex: 8, assistantText: 'Checking. Done.' },
  ])), history);
});

test('repeated prompts and identical answers are reconciled independently by raw positions', () => {
  const history = [
    { type: 'user', text: 'same', messageIndex: 20 },
    { type: 'assistant', text: 'A' },
    { type: 'tool' },
    { type: 'assistant', text: 'B' },
    { type: 'user', text: 'same', messageIndex: 30 },
    { type: 'assistant', text: 'AB' },
  ];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'same', messageIndex: 20, assistantText: 'AB' },
    { taskId: 't2', text: 'same', messageIndex: 30, assistantText: 'AB' },
  ])), history);
});

test('an older task fallback cannot use an identical answer from a later task', () => {
  const history = [
    { type: 'user', text: 'old', messageIndex: 0 },
    { type: 'user', text: 'next', messageIndex: 3 },
    { type: 'assistant', text: 'identical' },
  ];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'old', messageIndex: 0, assistantText: 'identical' },
  ])).map(item => item.text), ['old', 'identical', 'next', 'identical']);
});

test('a missing earlier turn is inserted before the next raw-position admission', () => {
  const history = [{ type: 'user', text: 'same', messageIndex: 10 },
    { type: 'assistant', text: 'later' }];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'same', messageIndex: 5, assistantText: 'earlier' },
    { taskId: 't2', text: 'same', messageIndex: 10, assistantText: 'later' },
  ])).map(item => item.text), ['same', 'earlier', 'same', 'later']);
});

test('missing journal prompt and output stay before a later task without raw positions', () => {
  const history = [{ type: 'user', text: 'next', remoteTaskId: 't2' },
    { type: 'assistant', text: 'later', remoteTaskId: 't2' }];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'first', assistantText: 'earlier' },
    { taskId: 't2', text: 'next', assistantText: 'later' },
  ])).map(item => item.text), ['first', 'earlier', 'next', 'later']);
});

test('a task prompt arriving after its journal output is inserted before that output', () => {
  const history = [{ type: 'assistant', text: 'A', remoteTaskId: 't1' },
    { type: 'reasoning', text: 'reason', remoteTaskId: 't1' },
    { type: 'assistant', text: 'B', remoteTaskId: 't1' },
    { type: 'user', text: 'next', remoteTaskId: 't2' }];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'first', assistantText: 'AB' },
  ])).map(item => item.text), ['first', 'A', 'reason', 'B', 'next']);
});

test('partial output and terminal failures remain separate and task-local', () => {
  for (const status of ['failed', 'cancelled', 'interrupted', 'result_unknown']) {
    const history = [{ type: 'user', text: 'first', messageIndex: 0 },
      { type: 'assistant', text: 'Part' },
      { id: 'failure', type: 'assistant', text: 'failure_code' },
      { type: 'user', text: 'next', messageIndex: 6 }];
    const merged = mergeControlledTaskChatItems(history, projectControlled([
      { taskId: 't1', text: 'first', messageIndex: 0, status,
        assistantText: 'Partial', terminalReason: 'failure_code' },
    ]));
    const label = { failed: 'Failed', cancelled: 'Cancelled' }[status] || status;
    assert.deepEqual(merged.map(item => item.text), [
      'first', 'Part', 'ial', label + ': failure_code', 'next',
    ]);
    assert.equal(merged[3].id, 'failure');
    assert.equal(merged[2].streaming, false);
  }
});

test('explicit errors survive reconciliation when partial model text is not yet hydrated', () => {
  const history = [{ type: 'user', text: 'first', messageIndex: 0 },
    { type: 'assistant', text: 'Part' }, { type: 'assistant', text: 'bad request' }];
  const merged = mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'first', messageIndex: 0, status: 'failed',
      assistantText: 'Partial', errorText: 'bad request' },
  ]));
  assert.deepEqual(merged.map(item => item.text), ['first', 'Part', 'ial', 'bad request']);
});

test('different task IDs cannot satisfy output coverage despite matching text', () => {
  const history = [{ type: 'user', text: 'first', remoteTaskId: 't1' },
    { type: 'assistant', text: 'same answer', remoteTaskId: 'foreign' }];
  const merged = mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'first', assistantText: 'same answer' },
    { taskId: 'foreign-session', sessionId: 'elsewhere', text: 'hidden', assistantText: 'hidden' },
  ]));
  assert.deepEqual(merged.map(item => item.text), ['first', 'same answer', 'same answer']);
});

test('unrelated text in the same turn is preserved without substring de-duplication', () => {
  const history = [{ type: 'user', text: 'first', messageIndex: 0 },
    { type: 'assistant', text: 'prefix answer suffix' }];
  const merged = mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'first', messageIndex: 0, assistantText: 'answer' },
  ]));
  assert.deepEqual(merged.map(item => item.text), ['first', 'prefix answer suffix', 'answer']);
});

test('a task-labelled journal steer stays inside its owning task range', () => {
  const history = [{ type: 'user', text: 'first', remoteTaskId: 't1' },
    { type: 'assistant', text: 'A', remoteTaskId: 't1' },
    { type: 'user', text: 'steer', remoteTaskId: 't1' },
    { type: 'assistant', text: 'B', remoteTaskId: 't1' },
    { type: 'user', text: 'next', remoteTaskId: 't2' }];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'first', assistantText: 'AB' },
  ])), history);
});

test('a later task journal event bounds an earlier fallback before its prompt arrives', () => {
  const history = [{ type: 'user', text: 'first', remoteTaskId: 't1' },
    { type: 'assistant', text: 'later', remoteTaskId: 't2' }];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'first', assistantText: 'earlier' },
    { taskId: 't2', text: 'next', assistantText: 'later' },
  ])).map(item => item.text), ['first', 'earlier', 'next', 'later']);
});

test('live user bubbles with no raw position still reconcile after hidden transcript messages', () => {
  const history = [{ type: 'user', text: 'old', messageIndex: 0 },
    { type: 'assistant', text: 'old answer' },
    { type: 'user', text: 'new' }, { type: 'assistant', text: 'A' },
    { type: 'tool' }, { type: 'assistant', text: 'B' }];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'new', messageIndex: 12, assistantText: 'AB', status: 'running' },
  ])), history);
});

test('repeated live prompts follow their task order without display-to-raw comparisons', () => {
  const history = [{ type: 'user', text: 'same' }, { type: 'assistant', text: 'A' },
    { type: 'user', text: 'same' }, { type: 'assistant', text: 'B' }];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'same', messageIndex: 12, assistantText: 'A' },
    { taskId: 't2', text: 'same', messageIndex: 20, assistantText: 'B' },
  ])), history);
});

function liveAdmissionHarness(tasks, history = []) {
  const window = {};
  const sandbox = vm.createContext({ window, console, Date, String });
  vm.runInContext(readFileSync(new URL('../src/features/remote-control/remote-control-state.js', import.meta.url), 'utf8'), sandbox);
  vm.runInContext(readFileSync(new URL('../src/platform/tauri/bridge/chat-events.js', import.meta.url), 'utf8'), sandbox);
  const listeners = new Map();
  const state = { activeSessionId: 'session-b', chatItems: history.slice(),
    messages: [], thinking: { active: true }, controlledTasks: tasks };
  const buffer = { messages: [], remoteAdmissionKeys: [] };
  const context = {
    state,
    listen(name, handler) { listeners.set(name, handler); },
    notify() {},
    turnUsageDirty: {}, sessionStates: {},
    getBuffer() { return buffer; },
    userMessageDisplayText() { return ''; },
    markRemoteTurn() {},
    isInternalRuntimeUserMessage() { return false; },
    runSyncOnSession(_sid, callback) { callback(); },
    addChatItem(item) { state.chatItems.push(item); },
    timeStr() { return ''; },
    flushPendingStreamRender() {},
  };
  window.__PINVOU_TAURI_BRIDGE_FEATURES__['chat-events'](context);
  return {
    state, logic: window.__PINVOU_REMOTE_CONTROL_STATE__,
    admit(content) {
      return listeners.get('chat:user_message')({ payload: { session_id: 'session-b', content } });
    },
  };
}

test('the real live admission producer carries task identity across repeated prompt history', async () => {
  const tasks = [{ taskId: 't2', sessionId: 'session-b', text: 'same',
    messageIndex: 12, status: 'running', assistantText: 'AB' }];
  const harness = liveAdmissionHarness(tasks, [
    { type: 'user', text: 'same' }, { type: 'assistant', text: 'old answer' },
  ]);
  await harness.admit('same');
  assert.equal(harness.state.chatItems[2].remoteTaskId, 't2');
  assert.equal(harness.state.chatItems[2].messageIndex, 12);
  harness.state.chatItems.push({ type: 'assistant', text: 'A' },
    { type: 'tool' }, { type: 'assistant', text: 'B' });
  assert.deepEqual(mergeControlledTaskChatItems(harness.state.chatItems,
    projectControlled(tasks)), harness.state.chatItems);
});

test('live admission identity is absent for unrelated, terminal, or ambiguous tasks', () => {
  const active = { taskId: 't1', sessionId: 'session-b', text: 'prompt', status: 'admitted' };
  const { logic } = liveAdmissionHarness([]);
  assert.equal(logic.controlledTaskAdmissionIdentity([active], 'other-session', 'prompt'), null);
  assert.equal(logic.controlledTaskAdmissionIdentity([active], 'session-b', 'different'), null);
  assert.equal(logic.controlledTaskAdmissionIdentity([{ ...active, status: 'failed' }], 'session-b', 'prompt'), null);
  assert.equal(logic.controlledTaskAdmissionIdentity([active, { ...active, taskId: 't2' }], 'session-b', 'prompt'), null);
});

test('live attachment admission preserves identity across tools and repeated prompt history', async () => {
  const tasks = [{ taskId: 'attachment-task', sessionId: 'session-b', text: 'inspect',
    messageIndex: 12, status: 'running', assistantText: 'AB' }];
  const display = 'inspect\n\n📎 ["报告 · final.pdf","image.png"]';
  const harness = liveAdmissionHarness(tasks, [
    { type: 'user', text: display }, { type: 'assistant', text: 'old answer' },
  ]);
  await harness.admit(display);
  assert.equal(harness.state.chatItems[2].remoteTaskId, 'attachment-task');
  assert.equal(harness.state.chatItems[2].messageIndex, 12);
  harness.state.chatItems.push({ type: 'assistant', text: 'A' },
    { type: 'tool' }, { type: 'assistant', text: 'B' });
  assert.deepEqual(mergeControlledTaskChatItems(harness.state.chatItems,
    projectControlled(tasks)), harness.state.chatItems);
});

test('attachment fallback without positions follows task order across repeated prompts', () => {
  const display = 'inspect\n\n📎 ["报告.pdf"]';
  const history = [{ type: 'user', text: display }, { type: 'assistant', text: 'A' },
    { type: 'tool' }, { type: 'assistant', text: 'B' },
    { type: 'user', text: display }, { type: 'assistant', text: 'C' }];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'inspect', messageIndex: 12, assistantText: 'AB' },
    { taskId: 't2', text: 'inspect', messageIndex: 20, assistantText: 'C' },
  ])), history);
});

test('attachment-only UI prompt reconciles before and after admission identity', async () => {
  // ChatView supplies this nonempty prompt for an otherwise blank composer.
  const text = '请处理随任务提交的附件。';
  const display = `${text}\n\n📎 ["报告.pdf"]`;
  const tasks = [{ taskId: 't1', sessionId: 'session-b', text, status: 'running', assistantText: 'done' }];
  const history = [{ type: 'user', text: display }, { type: 'assistant', text: 'done' }];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled(tasks)), history);
  const harness = liveAdmissionHarness(tasks);
  await harness.admit(display);
  assert.equal(harness.state.chatItems[0].remoteTaskId, 't1');
});

test('attachment matching rejects incomplete, malformed, or extended display suffixes', () => {
  const task = { taskId: 't1', sessionId: 'session-b', text: 'inspect', status: 'running' };
  const { logic } = liveAdmissionHarness([]);
  for (const display of [
    'inspect more\n\n📎 ["a.pdf"]', 'inspect\n\n📎 ["a.pdf"]\nextra',
    'inspect\n\n📎 a.pdf', 'inspect\n\n📎 []', 'inspect\n\n📎 [""]',
    'inspect\n\n📎 [null]', 'inspect\n\n📎 ["a.pdf"',
    'inspect\n\n📎 ["a.pdf"] trailing', 'inspect\n\n📎 ["a.pdf"]\n',
  ]) {
    assert.equal(logic.controlledTaskAdmissionIdentity([task], 'session-b', display), null, display);
    const history = [{ type: 'user', text: display }];
    assert.equal(mergeControlledTaskChatItems(history, projectControlled([task])).length, 2, display);
  }
});

test('attachment text never overrides foreign identity, raw position, or steer boundaries', () => {
  const display = 'inspect\n\n📎 ["a.pdf"]';
  for (const fields of [{ remoteTaskId: 'foreign' }, { messageIndex: 9 }, { steeredMidTurn: true }]) {
    const history = [{ type: 'user', text: display, ...fields }];
    const merged = mergeControlledTaskChatItems(history, projectControlled([
      { taskId: 't1', text: 'inspect', messageIndex: 12 },
    ]));
    assert.equal(merged.length, 2);
    assert.equal(merged[0], history[0]);
  }
});

test('the full admitted prompt survives each state producer and is never a truncated identity', () => {
  const { logic } = liveAdmissionHarness([]);
  const prefix = 'x'.repeat(8192);
  const text = `${prefix} complete prompt`;
  const session = { sessionId: 'session-b', generation: 'g' };
  const initial = () => ({ ...logic.createState(), controlledSession: session });
  const event = { taskId: 't1', sessionId: 'session-b', generation: 'g', status: 'admitted', text };
  const direct = logic.applyControlledTaskEvent(initial(), event);
  const running = logic.applyControlledTaskEvent(initial(), { ...event, text: undefined, status: 'running' });
  const late = logic.applyControlledTaskEvent(running, event);
  const journal = logic.applyTaskContent(running, {
    taskId: 't1', sessionId: 'session-b', role: 'user', eventId: 'user-content', text,
  });
  for (const state of [direct, late, journal]) {
    assert.equal(state.controlledTasks[0].text, text);
    assert.equal(logic.controlledTaskAdmissionIdentity(state.controlledTasks, 'session-b', prefix), null);
    assert.equal(logic.controlledTaskAdmissionIdentity(state.controlledTasks, 'session-b', `${prefix} different`), null);
    assert.equal(logic.controlledTaskAdmissionIdentity(state.controlledTasks, 'session-b', text).remoteTaskId, 't1');
    const display = `${text}\n\n📎 ["file.pdf"]`;
    assert.equal(logic.controlledTaskAdmissionIdentity(state.controlledTasks, 'session-b', display).remoteTaskId, 't1');
    const history = [{ type: 'user', text: display }];
    assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled(state.controlledTasks)), history);
  }
  const maximum = logic.applyControlledTaskEvent(initial(), { ...event, text: 'x'.repeat(65536) });
  assert.equal(maximum.controlledTasks[0].text.length, 65536);
  const oversized = logic.applyControlledTaskEvent(initial(), { ...event, text: 'x'.repeat(65537) });
  assert.equal(oversized.controlledTasks[0].text, null, 'do not silently truncate an invalid prompt');
  for (const valid of ['界'.repeat(21845) + 'x', '😀'.repeat(16384)]) {
    assert.equal(Buffer.byteLength(valid, 'utf8'), 65536);
    assert.equal(logic.applyControlledTaskEvent(initial(), { ...event, text: valid }).controlledTasks[0].text, valid);
  }
  for (const invalid of ['界'.repeat(21846), '😀'.repeat(16384) + 'x', '\ud800']) {
    assert.equal(logic.applyControlledTaskEvent(initial(), { ...event, text: invalid }).controlledTasks[0].text, null);
  }
});

test('steer text cannot steal an attachment prompt anchor from the actual admission', () => {
  const display = 'inspect\n\n📎 ["a.pdf"]';
  const history = [{ type: 'user', text: display, steeredMidTurn: true },
    { type: 'assistant', text: 'earlier answer' },
    { type: 'user', text: display }, { type: 'assistant', text: 'correct answer' }];
  assert.deepEqual(mergeControlledTaskChatItems(history, projectControlled([
    { taskId: 't1', text: 'inspect', assistantText: 'correct answer' },
  ])), history);
});

test('a middle hydration gap preserves unproven output and converges when filled', () => {
  const task = projectControlled([{ taskId: 't1', text: 'inspect', messageIndex: 0, assistantText: 'ABC' }]);
  const partial = [{ type: 'user', text: 'inspect', messageIndex: 0 },
    { type: 'assistant', text: 'A' }, { type: 'tool' }, { type: 'assistant', text: 'C' }];
  assert.deepEqual(mergeControlledTaskChatItems(partial, task).map(item => item.text),
    ['inspect', 'A', undefined, 'C', 'ABC']);
  const hydrated = [...partial.slice(0, 3), { type: 'assistant', text: 'B' }, partial[3]];
  assert.deepEqual(mergeControlledTaskChatItems(hydrated, task), hydrated);
});

for (const type of ['assistant', 'tool']) {
  test(`an unanchored prompt ignores ${type} items as raw-position fallback anchors`, () => {
    const history = [
      { id: 'previous-user', type: 'user', text: 'previous prompt', messageIndex: 0 },
      { id: 'non-user', type, text: 'previous event', messageIndex: 11 },
      { id: 'next-user', type: 'user', text: 'next prompt', messageIndex: 20 },
    ];
    const snapshot = structuredClone(history);
    const projected = projectControlled([
      { taskId: 'missing-task', text: 'missing prompt', messageIndex: 10, status: 'running' },
    ]);
    const merged = mergeControlledTaskChatItems(history, projected);
    assert.deepEqual(merged.map(item => item.text),
      ['previous prompt', 'previous event', 'missing prompt', 'next prompt']);
    assert.equal(merged[1], history[1], 'non-user evidence retains its original position');
    assert.equal(merged[2].controlledTaskId, 'missing-task');
    assert.equal(merged[3], history[2], 'the next durable user is the fallback boundary');
    assert.deepEqual(history, snapshot);
  });
}

test('permission label enrichment preserves existing raw evidence and all durable identity fields', () => {
  const code = 'task_permission_mode_changed';
  const copy = dict.zh.remoteConversation;
  const failure = {
    id: 'durable-failure', type: 'assistant', text: code,
    messageIndex: 1, createdAt: 123, remoteTaskId: 't1',
    controlled: false, controlledFailureReason: 'original-durable-evidence',
  };
  const history = [{ type: 'user', text: 'first', messageIndex: 0 }, failure];
  const snapshot = structuredClone(history);
  const projected = controlledTaskChatItems([{
    sessionId: 'session-b', taskId: 't1', text: 'first', messageIndex: 0,
    status: 'failed', errorText: code,
  }], copy, 'session-b');
  const merged = mergeControlledTaskChatItems(history, projected);
  assert.equal(merged.length, 2);
  assert.deepEqual(merged[1], {
    ...failure, text: `${copy.permissionModeChanged} (${code})`,
  });
  assert.equal(merged[1].controlledContentLane, undefined);
  assert.equal(merged[1].controlledTaskId, undefined);
  assert.deepEqual(history, snapshot);
});

test('identical model output and failure text require distinct persisted lane coverage', () => {
  for (const failure of [{ errorText: 'failure_code' }, { terminalReason: 'failure_code' }]) {
    const projected = projectControlled([{ taskId: 't1', text: 'first', messageIndex: 0,
      status: 'failed', assistantText: 'failure_code', ...failure }]);
    const history = [{ type: 'user', text: 'first', messageIndex: 0 },
      { id: 'model-output', type: 'assistant', text: 'failure_code' }];
    const merged = mergeControlledTaskChatItems(history, projected);
    assert.equal(merged[1], history[1], 'model text is not rewritten as a failure');
    assert.equal(merged.length, 3, 'the still-unhydrated failure remains visible separately');
    const failureText = failure.errorText || 'Failed: failure_code';
    assert.deepEqual(merged.map(item => item.text), ['first', 'failure_code', failureText]);
    const hydrated = [...history, { id: 'failure-output', type: 'assistant', text: 'failure_code',
      createdAt: 123, remoteTaskId: 't1' }];
    const converged = mergeControlledTaskChatItems(hydrated, projected);
    assert.equal(converged.length, 3);
    assert.equal(converged[1], history[1]);
    assert.equal(converged[2].id, 'failure-output');
    assert.equal(converged[2].createdAt, 123);
    assert.equal(converged[2].remoteTaskId, 't1');
    assert.equal(converged[2].controlledContentLane, undefined, 'do not leak fallback metadata into durable items');
    assert.deepEqual(converged.map(item => item.text), ['first', 'failure_code', failureText]);
  }
});

test('newer durable multi-message model text cannot be reused as an identical failure', () => {
  const history = [{ type: 'user', text: 'first', messageIndex: 0 },
    { id: 'm1', type: 'assistant', text: 'bad' }, { type: 'tool' },
    { id: 'm2', type: 'assistant', text: 'XYZ' }];
  const projected = projectControlled([{ taskId: 't1', text: 'first', messageIndex: 0,
    status: 'failed', assistantText: 'badX', terminalReason: 'bad' }]);
  const merged = mergeControlledTaskChatItems(history, projected);
  assert.deepEqual(merged.map(item => item.text), ['first', 'bad', undefined, 'XYZ', 'Failed: bad']);
  assert.equal(merged[1], history[1]);
  assert.equal(merged[3], history[3]);
  const hydrated = [...history, { id: 'failure', type: 'assistant', text: 'bad' }];
  const converged = mergeControlledTaskChatItems(hydrated, projected);
  assert.deepEqual(converged.map(item => item.text), ['first', 'bad', undefined, 'XYZ', 'Failed: bad']);
  assert.equal(converged[1], history[1]);
  assert.equal(converged[4].id, 'failure');
});

test('unproven output fallback cannot consume a reserved identical failure', () => {
  const code = 'task_permission_mode_changed';
  const copy = dict.en.remoteConversation;
  const failure = { id: 'failure', type: 'assistant', text: code, createdAt: 103 };
  const history = [{ type: 'user', text: 'first' },
    { id: 'model-start', type: 'assistant', text: 'task_' },
    { id: 'model-gap', type: 'assistant', text: 'changed' }, failure];
  const before = structuredClone(history);
  const projected = controlledTaskChatItems([{ sessionId: 'session-b', taskId: 't1',
    status: 'failed', text: 'first', assistantText: code, errorText: code }], copy, 'session-b');
  const output = projected.find(item => item.controlledContentLane === 'output');
  const error = projected.find(item => item.controlledContentLane === 'error');
  const merged = mergeControlledTaskChatItems(history, projected);
  assert.equal(merged.length, 5, 'unproven output and its failure remain separate');
  assert.equal(merged[1], history[1]);
  assert.equal(merged[2], history[2]);
  assert.equal(merged[3], output);
  assert.deepEqual(merged[4], { ...failure, text: error.text, controlledFailureReason: code });
  assert.deepEqual(history, before);
});

test('lagging task output snapshots cannot turn identical model text into permission failures', () => {
  const codes = ['task_permission_mode_required', 'task_permission_mode_changed',
    'task_permission_mode_unsupported'];
  for (const language of ['zh', 'en', 'ja']) {
    const copy = dict[language].remoteConversation;
    for (const code of codes) {
      for (const errorText of ['', code]) {
        const model = { id: 'model', type: 'assistant', text: code, messageIndex: 1,
          remoteTaskId: 't1', createdAt: 101 };
        const failure = { id: 'failure', type: 'assistant', text: code, messageIndex: 2,
          remoteTaskId: 't1', createdAt: 102 };
        const history = [{ id: 'user', type: 'user', text: 'first', remoteTaskId: 't1' },
          model, failure, { id: 'next', type: 'user', text: 'next', remoteTaskId: 't2' },
          { id: 'later-model', type: 'assistant', text: code, remoteTaskId: 't2' }];
        const before = structuredClone(history);
        const projected = controlledTaskChatItems([{ sessionId: 'session-b', taskId: 't1',
          status: 'failed', text: 'first', errorText, terminalReason: code }], copy, 'session-b');
        assert.equal(projected.some(item => item.controlledContentLane === 'output'), false);
        const expectedFailure = projected.find(item => item.type === 'assistant');
        const merged = mergeControlledTaskChatItems(history, projected);
        assert.equal(merged.length, history.length);
        assert.equal(merged[1], model, 'the earlier model message stays byte-for-byte untouched');
        assert.deepEqual(merged[2], { ...failure, text: expectedFailure.text,
          controlledFailureReason: code });
        assert.equal(merged[3], history[3]);
        assert.equal(merged[4], history[4], 'a later task cannot supply failure coverage');
        assert.deepEqual(history, before, 'projection never mutates the durable input');
      }
    }
  }
});

test('repeated failure overlays keep one-to-one durable reservations', () => {
  const code = 'task_permission_mode_changed';
  const copy = dict.en.remoteConversation;
  const projected = controlledTaskChatItems([{ sessionId: 'session-b', taskId: 't1',
    status: 'failed', text: 'first', errorText: code }], copy, 'session-b');
  const error = projected.find(item => item.controlledContentLane === 'error');
  const model = { id: 'model', type: 'assistant', text: code };
  const failures = Array.from({ length: 2 }, (_, index) => ({
    id: `failure-${index}`, type: 'assistant', text: code, createdAt: 50 + index,
  }));
  const history = [{ type: 'user', text: 'first' }, model, ...failures];
  const before = structuredClone(history);
  const merged = mergeControlledTaskChatItems(history,
    [...projected, { ...error, id: 'second-failure' }]);
  assert.equal(merged[1], model, 'a missing failure must not borrow identical model text');
  assert.equal(merged.length, 4, 'each failure consumes its own reservation or stays visible');
  failures.forEach((failure, index) => {
    assert.deepEqual(merged[index + 2], { ...failure, text: error.text,
      controlledFailureReason: code });
  });
  assert.deepEqual(history, before);
});

test('failure reconciliation runs without Array.findLast on the Safari 14 baseline', () => {
  const source = readFileSync(new URL('../src/features/remote-control/chat-transcript-projection.mjs', import.meta.url), 'utf8');
  const sandbox = vm.createContext({});
  vm.runInContext('Array.prototype.findLast = undefined;', sandbox);
  // The projection has no imports; evaluate its real function bodies in the
  // isolated legacy runtime instead of changing Node's global prototypes.
  vm.runInContext(source.replace(/^export /gm, ''), sandbox);
  const actual = vm.runInContext(`mergeControlledTaskChatItems([
    { type: 'user', text: 'first', messageIndex: 0 },
    { id: 'model', type: 'assistant', text: 'failure_code' },
    { id: 'failure', type: 'assistant', text: 'failure_code' },
  ], controlledTaskChatItems([{
    taskId: 't1', sessionId: 'session-b', text: 'first', messageIndex: 0,
    status: 'failed', assistantText: 'failure_code', terminalReason: 'failure_code',
  }], { statusFailed: 'Failed' }, 'session-b'))`, sandbox);
  assert.deepEqual(Array.from(actual, item => item.text), ['first', 'failure_code', 'Failed: failure_code']);
  assert.equal(actual[1].id, 'model');
  assert.equal(actual[2].id, 'failure');
});
