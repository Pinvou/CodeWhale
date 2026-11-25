import assert from 'node:assert/strict';
import test from 'node:test';
import {
  advanceRemoteQuestionScope, remoteQuestionProjectionKeys, remoteQuestionAnswers, remoteQuestionGroups,
  remoteQuestionOwner,
} from '../src/features/remote-control/remote-user-input.mjs';
import { activeRemoteTaskId, remoteProxyChatItems } from '../src/features/remote-control/remote-session-adapter.mjs';
import { dict } from './helpers/i18n-all.js';

test('remote questions retain opaque choices, multi-select and independent custom replies', () => {
  const questions = remoteQuestionGroups([{
    id: '0', header: 'Health', question: 'Choose', multi_select: true,
    options: [{ value: '0', label: 'HTML', description: 'Report' },
      { value: '1', label: 'Text', description: 'Report' }, { value: '2', label: '其他', description: 'Custom' }],
  }]);
  assert.equal(questions[0].multiSelect, true);
  assert.equal(questions[0].allowOther, true);
  assert.equal(questions[0].required, false);
  assert.deepEqual(questions[0].options.map(x => x.value), ['0', '1']);
  assert.deepEqual(remoteQuestionAnswers([{ questionId: '0', answers: [
    { value: '0', label: 'HTML', other: false }, { value: '1', label: 'Text', other: false },
  ] }]), [{ question_id: '0', option_ids: ['0', '1'] }]);
  assert.deepEqual(remoteQuestionAnswers([{ questionId: '0', answers: [
    { value: 'My own choice', label: 'Other', other: true },
  ] }]), [{ question_id: '0', option_ids: [], custom_text: 'My own choice' }]);
});

test('question ownership changes with lease/session/task but not capability refresh', () => {
  const binding = { localSessionId: 'local', remoteRef: 'remote' };
  const connection = { connectionId: 'c', controllerLeaseId: 'l', leaseEpoch: '1', relayEpoch: 'e', generation: 'g' };
  const key = remoteQuestionOwner(binding, connection, 'task-1');
  assert.equal(key, remoteQuestionOwner(binding, { ...connection, capabilityRevision: '999' }, 'task-1'));
  for (const field of Object.keys(connection)) assert.notEqual(key, remoteQuestionOwner(binding, { ...connection, [field]: 'changed' }, 'task-1'));
  for (const field of Object.keys(binding)) assert.notEqual(key, remoteQuestionOwner({ ...binding, [field]: 'changed' }, connection, 'task-1'));
  assert.notEqual(key, remoteQuestionOwner(binding, connection, 'task-2'));
  assert.equal(remoteQuestionOwner(null, connection, 'task-1'), '');
});

test('actual journal projection retires unanswered question tools for every terminal state', () => {
  const entry = (sequence, kind, body, taskId = 'task-1') => ({
    journalEpoch: 'e1', sequence, eventId: `event-${sequence}`, kind, body, taskId, attachments: [],
  });
  const start = entry(1, 'tool', JSON.stringify({ schema: 'remote_tool_display/v1', toolId: 'call-1', toolName: 'request_user_input', toolState: 'running' }));
  const render = entries => remoteProxyChatItems('local', { localSessionId: 'local', entries });
  const firstKeys = remoteQuestionProjectionKeys(render([start]), 'task-1');
  assert.notEqual(firstKeys.pendingKey, '');
  assert.equal(remoteQuestionProjectionKeys(render([start]), 'other-task').pendingKey, '');
  for (const state of ['succeeded', 'failed', 'cancelled', 'interrupted', 'result_unknown']) {
    const entries = [start, entry(2, 'status', state)];
    assert.equal(remoteQuestionProjectionKeys(render(entries), 'task-1').pendingKey, '', state);
    assert.equal(activeRemoteTaskId('local', { localSessionId: 'local', entries }), null, state);
  }
  const ended = entry(2, 'tool', JSON.stringify({ schema: 'remote_tool_display/v1', toolId: 'call-1', toolName: 'request_user_input', toolState: 'completed' }));
  assert.equal(remoteQuestionProjectionKeys(render([start, ended]), 'task-1').pendingKey, '');
  const next = entry(3, 'tool', JSON.stringify({ schema: 'remote_tool_display/v1', toolId: 'call-2', toolName: 'request_user_input', toolState: 'running' }));
  const nextKeys = remoteQuestionProjectionKeys(render([start, ended, next]), 'task-1');
  assert.notEqual(nextKeys.pendingKey, '');
  assert.notEqual(nextKeys.pendingKey, firstKeys.pendingKey);
  assert.notEqual(nextKeys.retiredKey, firstKeys.retiredKey);

  const overlapKeys = remoteQuestionProjectionKeys(render([start, next]), 'task-1');
  assert.notEqual(overlapKeys.pendingKey, firstKeys.pendingKey);
  assert.equal(overlapKeys.retiredKey, firstKeys.retiredKey, 'adding a projected tool preserves the current draft');
  const nextEnded = entry(4, 'tool', JSON.stringify({ schema: 'remote_tool_display/v1', toolId: 'call-2', toolName: 'request_user_input', toolState: 'completed' }));
  const afterOverlap = remoteQuestionProjectionKeys(render([start, next, nextEnded]), 'task-1');
  assert.equal(afterOverlap.pendingKey, firstKeys.pendingKey);
  assert.notEqual(afterOverlap.retiredKey, firstKeys.retiredKey, 'retiring a second tool invalidates prior snapshots');

  const firstScope = { ...firstKeys, revision: 0 };
  const overlapScope = advanceRemoteQuestionScope(firstScope, overlapKeys.pendingKey, overlapKeys.retiredKey);
  assert.equal(overlapScope.revision, firstScope.revision);
  const afterRetirement = advanceRemoteQuestionScope(overlapScope, afterOverlap.pendingKey, afterOverlap.retiredKey);
  const afterTrim = advanceRemoteQuestionScope(afterRetirement, firstKeys.pendingKey, firstKeys.retiredKey);
  assert.ok(afterRetirement.revision > overlapScope.revision);
  assert.ok(afterTrim.revision > afterRetirement.revision, 'trimming retired events cannot restore an old snapshot');
  const afterReplacement = advanceRemoteQuestionScope(firstScope, nextKeys.pendingKey, firstKeys.retiredKey);
  assert.ok(afterReplacement.revision > firstScope.revision, 'replacement invalidates even after the retirement is trimmed');
});

test('remote-question UI copy exists independently in all three languages', () => {
  for (const language of ['zh', 'en', 'ja']) {
    for (const key of ['userInputTitle', 'userInputWaiting', 'userInputCancel', 'userInputUnavailable', 'userInputUnknown']) {
      assert.equal(typeof dict[language].remoteConversation[key], 'string', `${language}.${key}`);
      assert.ok(dict[language].remoteConversation[key].length > 0);
    }
  }
});
