import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { validateControllerRuntimePayload } from '../protocol-controller.js';

const fixture = JSON.parse(readFileSync(new URL('./fixtures/controller-user-input.json', import.meta.url), 'utf8'));
const lease = { connection_id: 'connection-0001', controller_lease_id: 'lease-0001', lease_epoch: '1',
  relay_epoch: 'epoch-test-1', remote_control_generation: 'generation-0001' };
const context = { lease, negotiated_capabilities: ['permission_v1', 'task_v1'] };
function packet(body, result) {
  return { v: 1, type: result ? 'remote_action_result' : 'remote_action_request', ...lease,
    request_id: 'request-input-1', session_id: 'session-ref-1',
    operation: result ? 'user_input_list' : 'user_input_respond', body,
    ...(result ? { ok: true } : {}),
  };
}
const validate = (body, result) => validateControllerRuntimePayload(packet(body, result), result ? 'B' : 'A', context);

test('shared native/relay user-question vectors reject malformed and excess authority fields', () => {
  assert.doesNotThrow(() => validate({ requests: fixture.requests }, true));
  for (const mutation of fixture.invalid_request_mutations) {
    const requests = structuredClone(fixture.requests);
    const path = mutation.path.slice(1).split('/');
    const field = path.pop();
    path.reduce((object, key) => object[key], requests)[field] = mutation.value;
    assert.throws(() => validate({ requests }, true), mutation.path);
  }
  for (const [key, valid] of [['valid_answers', true], ['invalid_answers', false]]) {
    for (const answers of fixture[key]) {
      const body = { task_id: 'task-1', input_id: 'input-fixture-1', cancel: false, answers };
      if (valid) assert.doesNotThrow(() => validate(body, false));
      else assert.throws(() => validate(body, false), JSON.stringify(answers));
    }
  }
  const extra = structuredClone(fixture.requests);
  extra[0].questions[0].extra = true;
  assert.throws(() => validate({ requests: extra }, true));
});

test('user-question limits count Unicode scalars and reject unpaired surrogates', () => {
  for (const [length, valid] of [[64, true], [65, false]]) {
    const requests = structuredClone(fixture.requests);
    requests[0].questions[0].header = '🙂'.repeat(length);
    if (valid) assert.doesNotThrow(() => validate({ requests }, true));
    else assert.throws(() => validate({ requests }, true));
  }
  for (const custom_text of ['x'.repeat(4097), '\u0085', '\ud800']) {
    assert.throws(() => validate({ task_id: 'task-1', input_id: 'input-1', cancel: false,
      answers: [{ question_id: '0', option_ids: [], custom_text }] }, false));
  }
});

test('new user-question actions still require authenticated task and permission families', () => {
  for (const [body, result] of [[{ requests: fixture.requests }, true],
    [{ task_id: 'task-1', input_id: 'input-1', cancel: true, answers: [] }, false]]) {
    const value = packet(body, result);
    for (const capabilities of [['task_v1'], ['permission_v1']]) {
      assert.throws(() => validateControllerRuntimePayload(value, result ? 'B' : 'A', { lease, negotiated_capabilities: capabilities }));
    }
    assert.throws(() => validateControllerRuntimePayload(value, result ? 'A' : 'B', context));
    assert.throws(() => validateControllerRuntimePayload(value, result ? 'B' : 'A', {
      ...context, lease: { ...lease, lease_epoch: '2' },
    }));
  }
});
