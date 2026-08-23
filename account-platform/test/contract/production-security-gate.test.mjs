import assert from 'node:assert/strict';
import test from 'node:test';
import {
  productionSecurityConfiguration,
  runProductionSecurityGate,
} from '../../../scripts/account-platform-production-security-gate.mjs';

const environment = {
  PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON: JSON.stringify([
    { name:'portal', url:'https://tokens.example.com/health', expected_status:200 },
    { name:'identity', url:'https://identity.example.com/health', expected_status:200 },
    { name:'oidc', url:'https://accounts.example.com/realms/pinvou/.well-known/openid-configuration', expected_status:200 },
  ]),
  PINVOU_SECURITY_TOKEN_GATEWAY_READY_URL: 'https://token-gateway.internal/health/ready',
  PINVOU_SECURITY_KMS_HEALTH_URL: 'https://vault.internal/v1/health',
  TOKEN_SECRET_PROVIDER_TOKEN: 'workload-token-not-real-but-long-enough',
  TOKEN_SECRET_KEY_ID: 'kms-key-v2',
  PINVOU_SECRET_ROTATION_PREVIOUS_KEY_ID: 'kms-key-v1',
};

test('production security gate requires HTTPS public endpoints and a real key transition', () => {
  assert.doesNotThrow(() => productionSecurityConfiguration(environment));
  assert.doesNotThrow(() => productionSecurityConfiguration({ ...environment, PINVOU_SECURITY_TOKEN_GATEWAY_READY_URL:'http://127.0.0.1:18083/health/ready' }));
  assert.throws(() => productionSecurityConfiguration({ ...environment, PINVOU_SECURITY_TOKEN_GATEWAY_READY_URL:'http://token.internal/health/ready' }), /endpoint_invalid/);
  assert.throws(() => productionSecurityConfiguration({ ...environment, TOKEN_SECRET_KEY_ID:'kms-key-v1' }), /rotation_key_unchanged/);
  assert.throws(() => productionSecurityConfiguration({ ...environment, PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON:'[]' }), /endpoints_incomplete/);
});

test('production security gate proves TLS reachability, zero old ciphertext, and retired KMS key', async () => {
  const configuration = productionSecurityConfiguration(environment);
  const fetchImpl = async url => {
    const value = String(url);
    if (value.includes('token-gateway')) return new Response(JSON.stringify({ status:'ready' }), { status:200 });
    if (value.includes('vault.internal')) return new Response(JSON.stringify({ status:'ready', active_key_id:'kms-key-v2', key_states:{ 'kms-key-v2':'ENCRYPT_DECRYPT', 'kms-key-v1':'DISABLED' } }), { status:200 });
    return new Response('{}', { status:200 });
  };
  const tlsInspector = async () => ({ expiresAt:Date.now() + 30 * 24 * 60 * 60 * 1000 });
  const evidence = await runProductionSecurityGate(configuration, fetchImpl, tlsInspector);
  assert.equal(evidence.decision, 'GO');
  assert.equal(evidence.public_endpoints.length, 3);
  assert.match(evidence.evidence_sha256, /^[a-f0-9]{64}$/);
  assert.equal(JSON.stringify(evidence).includes(environment.TOKEN_SECRET_PROVIDER_TOKEN), false);
});

test('production security gate fails before the old KMS key is disabled', async () => {
  const configuration = productionSecurityConfiguration(environment);
  const fetchImpl = async url => String(url).includes('vault.internal')
    ? new Response(JSON.stringify({ status:'ready', active_key_id:'kms-key-v2', key_states:{ 'kms-key-v2':'ENCRYPT_DECRYPT', 'kms-key-v1':'DECRYPT_ONLY' } }), { status:200 })
    : new Response(JSON.stringify({ status:'ready' }), { status:200 });
  const tlsInspector = async () => ({ expiresAt:Date.now() + 30 * 24 * 60 * 60 * 1000 });
  await assert.rejects(() => runProductionSecurityGate(configuration, fetchImpl, tlsInspector), /kms_rotation_unverified/);
});

test('production security gate rejects a public certificate inside the renewal window', async () => {
  const configuration = productionSecurityConfiguration(environment);
  const tlsInspector = async () => ({ expiresAt:Date.now() + 13 * 24 * 60 * 60 * 1000 });
  await assert.rejects(() => runProductionSecurityGate(configuration, async () => new Response('{}', { status:200 }), tlsInspector), /certificate_expiring/);
});
