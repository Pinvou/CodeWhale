import assert from 'node:assert/strict';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { mkdtemp, writeFile } from 'node:fs/promises';
import http from 'node:http';
import os from 'node:os';
import path from 'node:path';
import { spawn } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const repoRoot = path.resolve(appRoot, '..');
const gateScript = path.join(repoRoot, 'scripts', 'account-platform-release-gate.mjs');

const temporary = await mkdtemp(path.join(os.tmpdir(), 'pinvou-client-switch-'));
const artifactPath = path.join(temporary, 'pinvou.bin');
const attestationPath = path.join(temporary, 'attestation.json');
await writeFile(artifactPath, 'signed release artifact');
const digest = createHash('sha256').update('signed release artifact').digest('hex');
const { publicKey, privateKey } = generateKeyPairSync('ed25519');
const rawPublicKey = publicKey.export({ format: 'der', type: 'spki' }).subarray(-32).toString('base64url');
const now = new Date();
const claims = {
  version: 1,
  attestation_id: '018f47b4-e124-7dca-9c85-6f7688436600',
  environment: 'test',
  artifact_sha256: digest,
  client_version: '0.8.0',
  service_version: '0.8.0',
  contract_version: 'client-readiness-v1',
  account_contract_version: 'account-v1',
  catalog_version: 9,
  protocol_code: 'CHAT',
  check_codes: ['GATEWAY_CONTRACT:PASS', 'ACCOUNT_CONTRACT:PASS', 'OIDC_METADATA:PASS', 'MODEL_CATALOG:PASS', 'MANAGED_ROUTE:PASS', 'PRICING_FORMULA:PASS'],
  issued_at: now.toISOString(),
  expires_at: new Date(now.getTime() + 5 * 60_000).toISOString(),
};
const attestation = {
  key_id: 'test-release-key',
  algorithm: 'Ed25519',
  claims,
  signature_base64url: sign(null, Buffer.from(JSON.stringify(claims)), privateKey).toString('base64url'),
};
await writeFile(attestationPath, JSON.stringify(attestation));

const server = http.createServer((request, response) => {
  response.setHeader('content-type', 'application/json');
  if (request.url === '/v1/client-compatibility') {
    response.end(JSON.stringify({
      environment: 'test', service_version: '0.8.0', contract_version: 'client-readiness-v1',
      minimum_client_version: '0.8.0', maximum_client_version: '0.8.0',
      attestation_key_id: 'test-release-key', attestation_available: true,
    }));
    return;
  }
  if (request.url === '/oidc/.well-known/openid-configuration') {
    response.end(JSON.stringify({ issuer: `http://127.0.0.1:${server.address().port}/oidc`, jwks_uri: `http://127.0.0.1:${server.address().port}/jwks` }));
    return;
  }
  response.statusCode = 404;
  response.end('{}');
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
const env = {
  ...process.env,
  PINVOU_RELEASE_ARTIFACT: artifactPath,
  PINVOU_RELEASE_ATTESTATION: attestationPath,
  PINVOU_CLIENT_COMPATIBILITY_URL: `${origin}/v1/client-compatibility`,
  PINVOU_ACCOUNT_OIDC_ISSUER: `${origin}/oidc`,
  PINVOU_ACCOUNT_SERVICE_URL: `${origin}/account`,
  PINVOU_TOKEN_GATEWAY_URL: `${origin}/token`,
  PINVOU_ACCOUNT_PORTAL_URL: `${origin}/portal`,
  PINVOU_ACCOUNT_ENVIRONMENT: 'test',
  PINVOU_CLIENT_READINESS_KEY_ID: 'test-release-key',
  PINVOU_CLIENT_READINESS_PUBLIC_KEY: rawPublicKey,
  PINVOU_RELEASE_CLIENT_VERSION: '0.8.0',
};

function runGate() {
  return new Promise(resolve => {
    const child = spawn(process.execPath, [gateScript], { env });
    let stdout = ''; let stderr = '';
    child.stdout.on('data', chunk => { stdout += chunk; });
    child.stderr.on('data', chunk => { stderr += chunk; });
    child.on('close', status => resolve({ status, stdout, stderr }));
  });
}

try {
  const accepted = await runGate();
  assert.equal(accepted.status, 0, accepted.stderr);
  assert.equal(JSON.parse(accepted.stdout).decision, 'GO');

  await writeFile(artifactPath, 'tampered artifact');
  const rejected = await runGate();
  assert.notEqual(rejected.status, 0, 'tampered artifact must fail the release gate');
} finally {
  await new Promise(resolve => server.close(resolve));
}
