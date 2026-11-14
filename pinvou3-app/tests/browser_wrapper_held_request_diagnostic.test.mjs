import assert from 'node:assert/strict';
import test from 'node:test';
import { heldRequestResultDiagnostic, heldRequestAuthorizationDiagnostic } from './helpers/browser-wrapper-held-result.mjs';

test('held-request diagnostics distinguish closed result classes without reading text', () => {
  const forbidden = { get() { assert.fail('private diagnostic field was read'); } };
  const structured = {
    errorCode: 'browser/action-commit-unknown-after-authorization-loss',
    retryable: false,
  };
  for (const key of ['authorizationError', 'upstreamError', 'message', 'path']) {
    Object.defineProperty(structured, key, forbidden);
  }
  const result = { isError: true, structuredContent: structured };
  Object.defineProperty(result, 'content', forbidden);
  assert.deepEqual(heldRequestResultDiagnostic({ result }), {
    kind: 'tool_error',
    errorCode: 'browser/action-commit-unknown-after-authorization-loss',
    retryable: false,
  });
  const rpc = { code: -32000 };
  Object.defineProperty(rpc, 'message', forbidden);
  Object.defineProperty(rpc, 'data', forbidden);
  assert.deepEqual(heldRequestResultDiagnostic({ error: rpc }), { kind: 'rpc_error', rpcCode: -32000 });
  assert.deepEqual(heldRequestResultDiagnostic({ result: { content: 'ignored' } }), { kind: 'tool_result' });
});

const authorizationResponse = authorizationError => ({ result: {
  isError: true,
  structuredContent: {
    errorCode: 'browser/action-commit-unknown-after-authorization-loss', authorizationError,
  },
} });

test('held authorization diagnostics classify only reviewed producer messages', () => {
  for (const [message, expected] of [
    ['browser/host-request-timeout: refresh_agent_input exceeded 400ms',
      { reason: 'reported_host_timeout', operation: 'refresh_agent_input', timeoutMs: 400 }],
    ['browser/host-request-timeout: refresh_agent_operation exceeded 5000ms',
      { reason: 'reported_host_timeout', operation: 'refresh_agent_operation', timeoutMs: 5000 }],
    ['The host-visible tab changed before tool execution', { reason: 'reported_workspace_change' }],
    ['Browser tool call was cancelled', { reason: 'reported_cancellation' }],
  ]) {
    const response = authorizationResponse(message);
    Object.freeze(response.result.structuredContent); Object.freeze(response.result); Object.freeze(response);
    assert.deepEqual(heldRequestAuthorizationDiagnostic(response), expected);
  }
});

test('held authorization diagnostics reject other messages and malformed records', () => {
  for (const message of [
    'PRIVATE_REASON_SENTINEL', 'browser/host-request-timeout: refresh_agent_input exceeded 5000ms',
    'browser/host-request-timeout: refresh_agent_input exceeded 400ms\n',
    'PRIVATE_REASON_SENTINEL'.repeat(10000), null, 400, true,
    { toString() { assert.fail('private conversion must not run'); } },
  ]) assert.deepEqual(heldRequestAuthorizationDiagnostic(authorizationResponse(message)), { reason: 'unknown' });
  for (const response of [null, [], {}, { result: [] }, { result: { isError: false } },
    { ...authorizationResponse('Browser tool call was cancelled'), error: {} },
    { result: { isError: true, structuredContent: { errorCode: 'other', authorizationError: 'Browser tool call was cancelled' } } },
  ]) assert.deepEqual(heldRequestAuthorizationDiagnostic(response), { reason: 'unknown' });
});

test('held authorization diagnostics never invoke accessors or inherited private fields', () => {
  for (const [level, key] of [['response', 'result'], ['result', 'isError'],
    ['result', 'structuredContent'], ['structured', 'errorCode'], ['structured', 'authorizationError'],
    ['response', 'error']]) {
    const response = authorizationResponse('Browser tool call was cancelled');
    const records = { response, result: response.result, structured: response.result.structuredContent };
    let reads = 0;
    Object.defineProperty(records[level], key, { get() { reads++; throw Error('PRIVATE_GETTER_SENTINEL'); } });
    assert.deepEqual(heldRequestAuthorizationDiagnostic(response), { reason: 'unknown' });
    assert.equal(reads, 0, 'getters must remain untouched');
  }
  const inherited = Object.create({ authorizationError: 'Browser tool call was cancelled' });
  inherited.errorCode = 'browser/action-commit-unknown-after-authorization-loss';
  assert.deepEqual(heldRequestAuthorizationDiagnostic({ result: { isError: true, structuredContent: inherited } }),
    { reason: 'unknown' });
});

test('held authorization diagnostics reject proxies without activating their traps', () => {
  let traps = 0;
  const proxy = new Proxy({}, Object.fromEntries(['get', 'getPrototypeOf', 'getOwnPropertyDescriptor']
    .map(name => [name, () => { traps++; throw Error('PRIVATE_PROXY_SENTINEL'); }])));
  const revoked = Proxy.revocable({}, {}); revoked.revoke();
  for (const value of [proxy, revoked.proxy]) {
    for (const response of [value, { result: value }, { result: { isError: true, structuredContent: value } },
      authorizationResponse(value)]) {
      assert.deepEqual(heldRequestAuthorizationDiagnostic(response), { reason: 'unknown' });
    }
  }
  assert.equal(traps, 0);
});

test('held-request diagnostics never forward unknown or malformed values', () => {
  for (const code of ['private reason', {}, 2 ** 31, -(2 ** 31) - 1, NaN, Infinity, true]) {
    assert.deepEqual(heldRequestResultDiagnostic({ error: { code } }), { kind: 'rpc_error', rpcCode: 'unknown' });
  }
  assert.deepEqual(heldRequestResultDiagnostic({ result: {
    isError: true, structuredContent: { errorCode: 'private reason', retryable: 'private flag' },
  } }), { kind: 'tool_error', errorCode: 'unknown', retryable: 'unknown' });
  for (const response of [null, [], 'private reason', {}, { result: [] }]) {
    assert.deepEqual(heldRequestResultDiagnostic(response), { kind: 'unclassified' });
  }
  const response = { get error() { throw new Error('private getter'); } };
  assert.deepEqual(heldRequestResultDiagnostic(response), { kind: 'unclassified' });
});
