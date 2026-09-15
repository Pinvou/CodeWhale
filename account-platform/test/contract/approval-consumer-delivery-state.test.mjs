import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import YAML from 'yaml';

const contract = YAML.parse(await readFile(new URL('../../contracts/openapi/account.yaml', import.meta.url), 'utf8'));
const validate = new Ajv2020({ allErrors: true, strict: false }).compile(contract.components.schemas.ApprovalAuthorizationDeliveryState);
const validateFailure = new Ajv2020({ allErrors: true, strict: false }).compile(contract.components.schemas.ApprovalAuthorizationDeliveryFailure);

test('approval delivery state exposes only closed crash-recovery facts', () => {
  for (const candidate of [
    { domain_attempt_count: 0, state: 'READY', version: 1 },
    { domain_attempt_count: 3, state: 'DOMAIN_INFLIGHT', version: 8, execution_status: 'EXECUTING' },
    { domain_attempt_count: 12, state: 'DOMAIN_INFLIGHT', version: 30, last_error_code: 'DOMAIN_PRECONDITION_REJECTED', execution_status: 'FAILED', execution_error_class: 'DOMAIN_PRECONDITION_REJECTED' },
  ]) assert.equal(validate(candidate), true, JSON.stringify(validate.errors));

  for (const candidate of [
    { domain_attempt_count: 0, state: 'READY', version: 1, payload: {} },
    { domain_attempt_count: 13, state: 'DOMAIN_INFLIGHT', version: 1 },
    { domain_attempt_count: 1, state: 'DOMAIN_INFLIGHT', version: 1, execution_status: 'FAILED' },
    { domain_attempt_count: 1, state: 'DOMAIN_INFLIGHT', version: 1, execution_status: 'EXECUTING', execution_error_class: 'DOMAIN_PRECONDITION_REJECTED' },
    { domain_attempt_count: 1, state: 'DOMAIN_INFLIGHT', version: 1, last_error_code: 'SECRET_LEAK' },
    { domain_attempt_count: 1, state: 'DOMAIN_INFLIGHT', version: 1, execution_status: 'FAILED', execution_error_class: 'TOKEN_EXPIRED' },
  ]) assert.equal(validate(candidate), false, `open state admitted: ${JSON.stringify(candidate)}`);
});

test('failure CAS accepts only stable non-sensitive error classes', () => {
  assert.equal(validateFailure({ expected_version: 3, error_code: 'DOMAIN_PRECONDITION_REJECTED' }), true, JSON.stringify(validateFailure.errors));
  for (const error_code of ['', 'domain_failure', 'ACCESS_TOKEN_REJECTED', 'PASSWORD_INVALID']) {
    assert.equal(validateFailure({ expected_version: 3, error_code }), false, error_code);
  }
});

test('delivery state reader remains service-authenticated and non-cacheable', () => {
  const operation = contract.paths['/internal/v1/approval-authorization/events/{event_id}/deliveries/{durable}'].get;
  assert.deepEqual(operation.security, [{ serviceToken: [] }]);
  assert.equal(operation.responses['200'].content['application/json'].schema.$ref, '#/components/schemas/ApprovalAuthorizationDeliveryState');
  assert.equal(operation.responses['200'].headers['Cache-Control'].schema.const, 'no-store');
  for (const code of ['200', '400', '403', '409', '503']) assert.ok(operation.responses[code]);
});
