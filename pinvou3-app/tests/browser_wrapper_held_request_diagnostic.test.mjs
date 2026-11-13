import assert from 'node:assert/strict';
import test from 'node:test';
import { heldRequestResultDiagnostic } from './helpers/browser-wrapper-held-result.mjs';

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
