import assert from 'node:assert/strict';
import test from 'node:test';
import {
  controlledTaskChatItems,
  isRemoteChatActive,
  mergeControlledTaskChatItems,
  remoteChatItems,
} from '../src/features/remote-control/chat-transcript-projection.mjs';

const connection = { status: 'connected', sessionId: 'session-1' };
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
