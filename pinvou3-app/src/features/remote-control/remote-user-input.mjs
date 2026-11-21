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

export function hasPendingRemoteQuestion(items, taskId) {
  return !!taskId && items.some(item => item.type === 'tool' && item.remoteTaskId === taskId
    && item.name === 'request_user_input' && item.state === 'running');
}

export function remoteQuestionOwner(binding, connection, taskId) {
  if (!binding || !connection || !taskId) return '';
  return JSON.stringify([binding.localSessionId, binding.remoteRef, taskId,
    connection.connectionId, connection.controllerLeaseId, connection.leaseEpoch,
    connection.relayEpoch, connection.generation]);
}
