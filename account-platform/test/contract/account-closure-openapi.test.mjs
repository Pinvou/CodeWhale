import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = readFileSync(new URL('../../contracts/openapi/account-closure.yaml', import.meta.url), 'utf8');

function operation(path, method) {
  const start = source.indexOf(`  ${path}:`);
  assert.notEqual(start, -1, `missing OpenAPI path ${path}`);
  const end = source.indexOf('\n  /', start + 1);
  const pathBlock = source.slice(start, end === -1 ? source.length : end);
  const methodStart = pathBlock.indexOf(`    ${method}:`);
  assert.notEqual(methodStart, -1, `missing ${method.toUpperCase()} operation for ${path}`);
  return pathBlock.slice(methodStart);
}

test('account-closure OpenAPI declares every deterministic handler status', () => {
  const expected = {
    '/v1/me/account-closure-request': ['401', '403', '404', '503'],
    '/v1/me/account-closure-requests': ['400', '401', '403', '404', '409', '503'],
    '/admin/v1/account-closure-requests': ['401', '403', '503'],
    '/admin/v1/account-closure-requests/{closure_request_id}/actions/retry': ['400', '401', '403', '404', '409', '503'],
  };
  for (const [path, statuses] of Object.entries(expected)) {
    const method = path.includes('actions/retry') || path === '/v1/me/account-closure-requests' ? 'post' : 'get';
    const block = operation(path, method);
    for (const status of statuses) assert.match(block, new RegExp(`^        "${status}":`, 'm'), `${path} missing ${status}`);
  }
});
