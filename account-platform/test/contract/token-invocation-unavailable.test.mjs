import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import Ajv from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';

const document = JSON.parse(await readFile(new URL('../../contracts/openapi/token-invocation.yaml', import.meta.url), 'utf8'));
const schema = document.paths['/v1/chat/completions'].post.responses['503'].content['application/json'].schema;
const ajv = new Ajv({ strict: true, allErrors: true });
addFormats(ajv);
const validateResponse = ajv.compile(schema);
const credentialReceipt = ajv.compile(document.components.schemas.CredentialUnavailableReceipt);
const unconfirmedReceipt = ajv.compile(document.components.schemas.UsageStateUnconfirmedReceipt);
const usageID = '01920000-0000-7000-8000-000000000001';
const details = { usage_request_id: usageID, status_url: `/v1/usage-requests/${usageID}` };

test('existing service-unavailable api.Error envelopes remain valid', () => {
  const validate = validateResponse;
  for (const code of ['MODEL_CATALOG_UNAVAILABLE', 'MODEL_POLICY_UNAVAILABLE', 'BILLING_UNAVAILABLE', 'PROVIDER_CIRCUIT_OPEN', 'HOLD_RENEWAL_FAILED', 'RESULT_PERSISTENCE_FAILED', 'SETTLEMENT_PENDING']) {
    assert.equal(validate({ code, message: 'unavailable' }), true, `${code}: ${ajv.errorsText(validate.errors)}`);
    assert.equal(validate({ code, message: 'unavailable', request_id: 'request', details: { retry_after: 1 } }), true, code);
  }
  // Provider errors are not guaranteed to use the gateway error envelope.
  assert.equal(validate({ error: { code: 'unavailable', message: 'retry' } }), true);
});

test('exact predispatch failure receipt retains required identity and terminal state', () => {
  const validate = credentialReceipt;
  const receipt = { code: 'PROVIDER_CREDENTIAL_UNAVAILABLE', message: 'unavailable', details: { ...details, execution_status: 'FAILED', settlement_status: 'RELEASED' } };
  assert.equal(validate(receipt), true, ajv.errorsText(validate.errors));
  for (const field of ['usage_request_id', 'status_url', 'execution_status', 'settlement_status']) {
    const bad = structuredClone(receipt); delete bad.details[field];
    assert.equal(validate(bad), false, `missing ${field} must not use generic error branch`);
  }
  for (const replacement of [{ usage_request_id: 'invalid' }, { execution_status: 'UPSTREAM_DISPATCHING' }, { settlement_status: 'HOLD_ACTIVE' }]) {
    assert.equal(validate({ ...receipt, details: { ...receipt.details, ...replacement } }), false);
  }
  assert.equal(validate({ code: receipt.code, message: receipt.message }), false);
});

test('unconfirmed receipt definition does not infer terminal or no-effect meaning', () => {
  const validate = unconfirmedReceipt;
  const receipt = { code: 'USAGE_STATE_UNCONFIRMED', message: 'unconfirmed', details };
  assert.equal(validate(receipt), true, ajv.errorsText(validate.errors));
  assert.equal(validate({ code: receipt.code, message: receipt.message }), false);
  assert.equal(validate({ ...receipt, details: { ...details, settlement_status: 'RELEASED' } }), false);
  assert.equal(validate({ ...receipt, details: { ...details, usage_request_id: 'invalid' } }), false);
  assert.equal(validate({ code: '', message: 'unavailable' }), false);
  assert.equal(validate({ message: 'unavailable' }), false);
});

test('upstream503 JSON and media are passthrough, not a gateway receipt discriminator', () => {
  for (const value of [null, false, 1, 'upstream unavailable', [], ['retry'], { error: { message: 'busy' } }, { code: 'PROVIDER_CREDENTIAL_UNAVAILABLE' }]) {
    assert.equal(validateResponse(value), true, JSON.stringify(value));
  }
  assert.equal(credentialReceipt({ code: 'PROVIDER_CREDENTIAL_UNAVAILABLE' }), false);
  assert.equal(unconfirmedReceipt({ code: 'USAGE_STATE_UNCONFIRMED' }), false);
  const wildcard = document.paths['/v1/chat/completions'].post.responses['503'].content['*/*'];
  assert.ok(wildcard, 'original non-JSON provider media are retained');
  assert.equal(ajv.compile(wildcard.schema)('upstream plain text'), true);
});
