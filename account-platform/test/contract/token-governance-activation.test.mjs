import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const contract = YAML.parse(await readFile(new URL('../../contracts/openapi/token-gateway.yaml', import.meta.url), 'utf8'));
const accountContract = YAML.parse(await readFile(new URL('../../contracts/openapi/account.yaml', import.meta.url), 'utf8'));
const ajv = new Ajv2020({ strict: false, allErrors: true });
addFormats(ajv);
const validate = ajv.compile(contract.components.schemas.TokenGovernanceActivation);
const pre = { state: 'PRE_ACTIVATION', epoch: 0, policy_set_digest: '', handler_set_digest: '', approved_runtime_roles: [], updated_at: '2026-09-06T00:00:00Z' };
const active = { ...pre, state: 'ACTIVE', epoch: 1, policy_set_digest: 'a'.repeat(64), handler_set_digest: 'b'.repeat(64), approved_runtime_roles: ['release_fixture'] };

test('APT-138 activation is service-only and has no public mutation operation', () => {
  const endpoint = contract.paths['/internal/v1/token-governance/activation'];
  assert.deepEqual(Object.keys(endpoint), ['get']);
  assert.deepEqual(endpoint.get.security, [{ serviceToken: [] }]);
  assert.match(endpoint.get.description, /internal\.authorization\.check/);
  assert.equal(endpoint.get.responses['200'].headers['Cache-Control'].schema.const, 'no-store');
  for (const status of ['401', '403', '503']) assert.ok(endpoint.get.responses[status]);
});

test('APT-138 activation snapshots reject mixed states and extra fields', () => {
  for (const valid of [pre, active, { ...active, state: 'SAFE_ROLLBACK', epoch: 2 }]) {
    assert.equal(validate(valid), true, JSON.stringify(validate.errors));
    for (const field of Object.keys(valid)) {
      const missing = { ...valid };
      delete missing[field];
      assert.equal(validate(missing), false, `missing ${field}`);
    }
  }
  for (const invalid of [
    { ...pre, epoch: 1 }, { ...pre, approved_runtime_roles: ['old_writer'] },
    { ...pre, policy_set_digest: active.policy_set_digest }, { ...active, epoch: 0 },
    { ...active, state: 'UNKNOWN' }, { ...active, handler_set_digest: '' },
    { ...active, approved_runtime_roles: [] }, { ...active, approved_runtime_roles: ['same', 'same'] },
    { ...active, approved_runtime_roles: [''] }, { ...active, password: 'must-not-appear' },
    { ...active, updated_at: 'invalid' }, { ...active, policy_set_digest: 'x'.repeat(64) },
  ]) assert.equal(validate(invalid), false, JSON.stringify(invalid));
});

test('APT-138 approved command reads use separate workload permissions and unknown-only GET recovery', () => {
  for (const command of ['model-lifecycle', 'catalog-rollback', 'credential-revoke']) {
    const precheck = contract.paths[`/internal/v1/commands/${command}/precheck`];
    const result = contract.paths[`/internal/v1/commands/${command}/result`];
    assert.deepEqual(Object.keys(precheck), ['post']);
    assert.deepEqual(Object.keys(result), ['get']);
    for (const operation of [precheck.post, result.get]) {
      assert.deepEqual(operation.security, [{ serviceToken: [] }]);
      assert.ok(operation.description.includes(`internal.token.${command}.execute`));
      assert.match(operation.description, /token-command-executor/);
      assert.equal(operation.responses['200'].headers['Cache-Control'].schema.const, 'no-store');
    }
    assert.deepEqual(result.get.parameters.map((item) => [item.name, item.in, item.required]), [
      ['approval_request_id', 'query', true], ['command_fingerprint', 'query', true],
    ]);
    assert.match(result.get.description, /SAFE_ROLLBACK/);
    assert.match(result.get.description, /404 is unconfirmed/);
    assert.ok(result.get.responses['404']);
  }
});

test('APT-138 execution lease makes first durable claim explicit without accepting aliases', () => {
  const schema = accountContract.components.schemas.ApprovalExecutionLease;
  assert.equal(schema.additionalProperties, false);
  for (const field of ['requester_authorization_version', 'requester_reason', 'first_claim']) {
    assert.ok(schema.required.includes(field));
    assert.ok(schema.properties[field]);
  }
  assert.equal(schema.properties.first_claim.type, 'boolean');
  assert.match(schema.properties.first_claim.description, /created the durable execution record/);
  assert.match(schema.properties.first_claim.description, /Replays.*false/);
  for (const alias of ['First_Claim', 'FirstClaim', 'firstClaim']) assert.equal(schema.properties[alias], undefined);
});

test('APT-138 approved execution requires a frozen positive activation epoch', () => {
  const schema = contract.components.schemas.ApprovedTokenCommandExecution;
  assert.ok(schema.required.includes('activation_epoch'));
  assert.deepEqual({ type: schema.properties.activation_epoch.type, minimum: schema.properties.activation_epoch.minimum }, { type: 'integer', minimum: 1 });
  assert.match(schema.description, /transaction fence must match that epoch/);
  assert.match(schema.description, /SAFE_ROLLBACK and reactivation/);
  for (const alias of ['ActivationEpoch', 'activationEpoch']) assert.equal(schema.properties[alias], undefined);
});
