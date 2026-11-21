import { isFreeTextPlaceholderOption } from '../../shared/user-input-shared.js';

export function remoteQuestionGroups(questions) {
  return (Array.isArray(questions) ? questions : []).map(question => ({
    id: question.id, header: question.header, question: question.question,
    options: question.options.filter(option => !isFreeTextPlaceholderOption(option)),
    multiSelect: question.multi_select, allowOther: true, required: !question.multi_select,
  }));
}

export function remoteQuestionAnswers(groups) {
  return groups.map(group => {
    const custom = group.answers.find(answer => answer.other);
    return {
      question_id: group.questionId,
      option_ids: group.answers.filter(answer => !answer.other).map(answer => String(answer.value)),
      ...(custom ? { custom_text: String(custom.value) } : {}),
    };
  });
}

export function remoteQuestionProjectionKeys(items, taskId) {
  const pending = [];
  const retired = [];
  if (taskId) {
    for (const item of items) {
      if (item.type !== 'tool' || item.remoteTaskId !== taskId || item.name !== 'request_user_input') continue;
      const destination = item.state === 'running' ? pending : retired;
      destination.push(JSON.stringify([item.remoteJournalEpoch, item.remoteToolId]));
    }
  }
  return {
    pendingKey: pending.length ? JSON.stringify(pending) : '',
    // Adding a tool can refresh the list without clearing an existing draft.
    // A retirement invalidates the preceding list even if another tool remains.
    retiredKey: JSON.stringify(retired),
  };
}

export function advanceRemoteQuestionScope(scope, pendingKey, retiredKey) {
  if (scope.pendingKey === pendingKey && scope.retiredKey === retiredKey) return scope;
  const pending = new Set(JSON.parse(pendingKey || '[]'));
  const invalidated = scope.retiredKey !== retiredKey
    || JSON.parse(scope.pendingKey || '[]').some(id => !pending.has(id));
  return { pendingKey, retiredKey, revision: scope.revision + Number(invalidated) };
}

export function remoteQuestionOwner(binding, connection, taskId) {
  if (!binding || !connection || !taskId) return '';
  return JSON.stringify([binding.localSessionId, binding.remoteRef, taskId,
    connection.connectionId, connection.controllerLeaseId, connection.leaseEpoch,
    connection.relayEpoch, connection.generation]);
}
