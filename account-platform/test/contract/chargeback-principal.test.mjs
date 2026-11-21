import test from 'node:test';
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { validateChargebackPrincipalWire } from '../fixtures/chargeback-principal-validator.mjs';

test('principal schema keeps unknown cash null and exact int64 cash as text', () => {
  const fact = { status: 'CONFIRMED', amount_fen: '9007199254740993', currency: 'CNY', observation_count: 1, evidence_digest: 'a'.repeat(64), manual_review_required: true };
  assert.ok(validateChargebackPrincipalWire('CashPrincipal', fact));
  assert.equal(validateChargebackPrincipalWire('CashPrincipal', { ...fact, amount_fen: 9007199254740993 }), false);
  for (const status of ['MISSING', 'CONFLICT']) {
    assert.ok(validateChargebackPrincipalWire('CashPrincipal', { ...fact, status, amount_fen: null }));
    assert.equal(validateChargebackPrincipalWire('CashPrincipal', { ...fact, status }), false);
    assert.equal(validateChargebackPrincipalWire('CashPrincipal', { ...fact, status, amount_fen: null, manual_review_required: false }), false);
  }
});

test('registered write and bounded read share the existing exact supplier permission', async () => {
  const document = JSON.parse(await readFile(new URL('../../contracts/openapi/chargeback-principal.json', import.meta.url), 'utf8'));
  const source = await readFile(new URL('../../internal/billing/service.go', import.meta.url), 'utf8');
  for (const [path, method] of [['/internal/v1/chargebacks/confirmed', 'post'], ['/internal/v1/chargebacks/{case_id}/principal-evidence', 'get']]) {
    const operation = document.paths[path][method];
    assert.equal(operation['x-required-permission'], 'internal.billing.chargeback.record');
    assert.deepEqual(operation.security, [{ WorkloadBearer: [] }]);
    assert.ok(source.includes(`protected.${method.toUpperCase()}("${path.replace('{case_id}', ':case_id')}", serviceauth.Require("${operation['x-required-permission']}")`));
  }
  const limit = document.paths['/internal/v1/chargebacks/{case_id}/principal-evidence'].get.parameters.find(p => p.name === 'limit');
  assert.deepEqual(limit.schema, { type: 'integer', minimum: 1, maximum: 100, default: 20 });
  assert.equal(document.components.schemas.Observation.properties.payload_snapshot, undefined);
});
