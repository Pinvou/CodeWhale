import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import { validateApprovalRecoveryWire } from '../fixtures/approval-recovery-validator.mjs';

const load = async name => JSON.parse(await readFile(new URL(`../../contracts/openapi/${name}.json`, import.meta.url), 'utf8'));
const account = await load('approval-recovery');
const billing = await load('approval-recovery-billing');
const token = await load('approval-recovery-token');
const source = async path => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');

test('Account recovery contract binds existing exact routes, scope and dedicated workload', async () => {
  const routes = await source('internal/account/service.go');
  assert.deepEqual(Object.keys(account.paths).sort(), ['/internal/v1/approval-recovery/confirm', '/internal/v1/approval-recovery/plan', '/internal/v1/approval-recovery/zero-effect']);
  for (const [action, verb, method] of [['plan', 'GET', 'get'], ['confirm', 'POST', 'post'], ['zero-effect', 'POST', 'post']]) {
    const operation = account.paths[`/internal/v1/approval-recovery/${action}`][method];
    assert.ok(routes.includes(`internal.${verb}("/approval-recovery/${action}", serviceauth.Require("${operation['x-required-permission']}")`));
    assert.deepEqual(operation['x-required-workloads'], ['account-approval-recovery']);
    assert.deepEqual(operation.security, [{ WorkloadBearer: [] }]);
    for (const code of ['400', '401', '403', '503']) assert.ok(operation.responses[code]);
  }
  assert.ok(account.paths['/internal/v1/approval-recovery/plan'].get.responses['200']);
  for (const action of ['confirm', 'zero-effect']) {
    const operation = account.paths[`/internal/v1/approval-recovery/${action}`].post;
    assert.equal(operation['x-body-max-bytes'], 16384);
    assert.ok(operation.responses['204']); assert.ok(operation.responses['409']);
    assert.equal(operation.responses['204'].content, undefined);
  }
});

test('owner observations are separate existing service contracts and cannot grant replay', async () => {
  for (const [service, document, path] of [['billing', billing, 'internal/billing/service.go'], ['token', token, 'internal/token/management_routes.go']]) {
    const routes = await source(path);
    assert.equal(document['x-service'], service);
    assert.equal(Object.keys(document.paths).length, 2);
    for (const [url, item] of Object.entries(document.paths)) {
      const operation = item.get;
      const literal = url.replace('{approval_request_id}', ':approval_request_id');
      assert.ok(routes.includes(`"${service === 'token' ? literal.replace('/internal/v1', '') : literal}"`));
      assert.ok(routes.includes(`serviceauth.Require("${operation['x-required-permission']}")`));
      assert.deepEqual(operation['x-required-workloads'], ['account-service', 'account-approval-recovery']);
      assert.match(operation['x-conservative-result'], /never permission/);
    }
  }
  assert.notDeepEqual(billing.components.schemas.Result.properties, token.components.schemas.Result.properties);
});

test('closed schema rejects supplied terminal verdict and unknown candidate fields', () => {
  assert.equal(validateApprovalRecoveryWire('account', 'Confirmation', { candidate: {}, lease: {}, zero_effect_proven: true }), false);
  for (const service of ['billing', 'token']) {
    assert.equal(validateApprovalRecoveryWire(service, 'Result', { schema_version: 1, state: 'ABSENT' }), false);
    assert.equal(validateApprovalRecoveryWire(service, 'Effect', { state: 'UNCONFIRMED' }), false);
  }
});
