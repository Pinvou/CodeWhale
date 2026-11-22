import test from 'node:test';
import assert from 'node:assert/strict';
import { generateKeyPairSync, sign } from 'node:crypto';
import { validateReadinessBindings, verifyReadinessAttestation } from '../../scripts/account-platform-readiness-attestation.mjs';

const now = Date.parse('2026-10-08T00:00:00Z');
const binding = { environment: 'test', digest: 'public-synthetic-artifact-digest', clientVersion: '1.0.0', now };
const claims = {
  version: 1, environment: binding.environment, artifact_sha256: binding.digest,
  client_version: binding.clientVersion, protocol_code: 'CHAT', catalog_version: 9,
  issued_at: new Date(now).toISOString(), expires_at: new Date(now + 5 * 60_000).toISOString(),
};

test('readiness accepts the existing signed-claim binding and finite permitted clock skew', () => {
  assert.doesNotThrow(() => validateReadinessBindings(claims, binding));
  assert.doesNotThrow(() => validateReadinessBindings({ ...claims, issued_at: new Date(now + 60_000).toISOString() }, binding));
});

test('readiness rejects malformed time and catalog authority instead of treating NaN as valid', async t => {
  for (const change of [
    { issued_at: 'invalid' }, { expires_at: 'invalid' }, { issued_at: null }, { expires_at: null },
    { catalog_version: 'invalid' }, { catalog_version: '9' }, { catalog_version: null }, { catalog_version: 0 },
    { catalog_version: 1.5 }, { catalog_version: Number.MAX_SAFE_INTEGER + 1 },
    { expires_at: new Date(now).toISOString() }, { issued_at: new Date(now + 60_001).toISOString() },
    { expires_at: new Date(now + 15 * 60_000 + 1).toISOString() },
    { issued_at: new Date(now + 30_000).toISOString(), expires_at: new Date(now + 20_000).toISOString() },
  ]) await t.test(JSON.stringify(change), () => assert.throws(() => validateReadinessBindings({ ...claims, ...change }, binding), /binding_invalid/));
});

test('real Ed25519 signatures cannot authorize malformed claims or a different release context', async t => {
  const { publicKey, privateKey } = generateKeyPairSync('ed25519');
  const rawPublicKey = publicKey.export({ format: 'der', type: 'spki' }).subarray(-32);
  const complete = { ...claims, check_codes: ['GATEWAY_CONTRACT:PASS', 'ACCOUNT_CONTRACT:PASS', 'OIDC_METADATA:PASS', 'MODEL_CATALOG:PASS', 'MANAGED_ROUTE:PASS', 'PRICING_FORMULA:PASS'] };
  const signed = value => ({ algorithm: 'Ed25519', key_id: 'synthetic-key', claims: value,
    signature_base64url: sign(null, Buffer.from(JSON.stringify(value)), privateKey).toString('base64url') });
  assert.doesNotThrow(() => verifyReadinessAttestation(signed(complete), binding, 'synthetic-key', rawPublicKey));
  for (const change of [{ issued_at: 'invalid' }, { expires_at: 'invalid' }, { catalog_version: 'invalid' },
    { environment: 'another-context' }, { expires_at: new Date(now - 1).toISOString() }, { check_codes: [] }]) {
    await t.test(JSON.stringify(change), () => assert.throws(() => verifyReadinessAttestation(signed({ ...complete, ...change }), binding, 'synthetic-key', rawPublicKey)));
  }
  assert.throws(() => verifyReadinessAttestation(signed(complete), binding, 'another-key', rawPublicKey), /signature_invalid/);
  const forged = signed(complete); forged.claims = { ...complete, client_version: '2.0.0' };
  assert.throws(() => verifyReadinessAttestation(forged, { ...binding, clientVersion: '2.0.0' }, 'synthetic-key', rawPublicKey), /signature_invalid/);
});

test('readiness preserves all original feature/artifact/client bindings', async t => {
  for (const change of [{ version: 2 }, { environment: 'production' }, { artifact_sha256: 'another' },
    { client_version: '2.0.0' }, { protocol_code: 'EMBEDDINGS' }]) {
    await t.test(JSON.stringify(change), () => assert.throws(() => validateReadinessBindings({ ...claims, ...change }, binding), /binding_invalid/));
  }
});
