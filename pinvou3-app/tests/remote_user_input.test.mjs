import assert from 'node:assert/strict';
import test from 'node:test';
import {
  hasPendingRemoteQuestion, remoteQuestionAnswers, remoteQuestionGroups, remoteQuestionOwner,
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
  assert.equal(hasPendingRemoteQuestion(render([start]), 'task-1'), true);
  assert.equal(hasPendingRemoteQuestion(render([start]), 'other-task'), false);
  for (const state of ['succeeded', 'failed', 'cancelled', 'interrupted', 'result_unknown']) {
    const entries = [start, entry(2, 'status', state)];
    assert.equal(hasPendingRemoteQuestion(render(entries), 'task-1'), false, state);
    assert.equal(activeRemoteTaskId('local', { localSessionId: 'local', entries }), null, state);
  }
  const ended = entry(2, 'tool', JSON.stringify({ schema: 'remote_tool_display/v1', toolId: 'call-1', toolName: 'request_user_input', toolState: 'completed' }));
  assert.equal(hasPendingRemoteQuestion(render([start, ended]), 'task-1'), false);
});

test('remote-question UI copy exists independently in all three languages', () => {
  for (const language of ['zh', 'en', 'ja']) {
    for (const key of ['userInputTitle', 'userInputWaiting', 'userInputCancel', 'userInputUnavailable', 'userInputUnknown']) {
      assert.equal(typeof dict[language].remoteConversation[key], 'string', `${language}.${key}`);
      assert.ok(dict[language].remoteConversation[key].length > 0);
    }
  }
});
