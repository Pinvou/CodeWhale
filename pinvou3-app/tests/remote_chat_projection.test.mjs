import assert from 'node:assert/strict';
import test from 'node:test';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import {
  controlledTaskChatItems,
  isRemoteChatActive,
  mergeControlledTaskChatItems,
  remoteChatItems,
} from '../src/features/remote-control/chat-transcript-projection.mjs';

const connection = { status: 'connected', sessionId: 'session-1' };
const projectControlled = tasks => {
  const sessionTasks = tasks.map(task => ({ sessionId: 'session-b', status: 'succeeded', ...task }));
  return controlledTaskChatItems(sessionTasks,
    { statusCancelled: 'Cancelled', statusFailed: 'Failed' }, 'session-b');
};
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
    const hydrated = [...history, { id: 'failure-output', type: 'assistant', text: 'failure_code' }];
    const converged = mergeControlledTaskChatItems(hydrated, projected);
    assert.equal(converged.length, 3);
    assert.equal(converged[1], history[1]);
    assert.equal(converged[2].id, 'failure-output');
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
