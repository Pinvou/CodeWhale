import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { handoffBuildEvidence, probeCredentialOrigins, validateHandoffResponse, validateCredentialResponse, validateCredentialDiscovery } from '../../../scripts/account-platform-credential-origin-probe.mjs';
const config = JSON.parse(await readFile(new URL('../../deploy/nginx/credential-edge.example.json', import.meta.url), 'utf8'));
const build = await handoffBuildEvidence();
function headers(referrer) {
  return { 'cache-control': 'no-store', 'referrer-policy': referrer, 'cross-origin-opener-policy': 'same-origin', 'x-content-type-options': 'nosniff', 'x-frame-options': 'DENY', 'strict-transport-security': 'max-age=31536000; includeSubDomains' };
}
function handoff() {
  return { status: 200, certificate_sha256: 'a'.repeat(64), headers: { ...headers('origin'), 'content-type': 'text/html; charset=utf-8', 'content-security-policy': `default-src 'none'; script-src 'sha256-${build.digest}'; form-action ${config.credential_origin}; worker-src 'none'; frame-ancestors 'none'; base-uri 'none'` }, body: Buffer.concat([Buffer.from(`<!doctype html><html><head><meta charset="utf-8"><meta name="referrer" content="origin"><meta name="pinvou-credential-origin" content="${config.credential_origin}"><title>Credential handoff</title></head><body><script>`), build.script, Buffer.from('</script></body></html>')]) };
}
function credential(asset = false) {
  const origin = new URL(config.oidc.issuer).origin;
  return { status: asset ? 200 : 401, certificate_sha256: 'b'.repeat(64), headers: { ...headers('no-referrer'), 'content-security-policy': `default-src 'self'; connect-src 'self' ${origin}; form-action 'self' ${origin}; frame-ancestors 'none'; base-uri 'none'` }, body: Buffer.from(asset ? 'fixture-asset' : '{"code":"CREDENTIAL_SESSION_REQUIRED"}') };
}
function discovery() {
  return { status: 200, certificate_sha256: 'c'.repeat(64), body: Buffer.from(JSON.stringify({ issuer: config.oidc.issuer, authorization_endpoint: config.oidc.authorization_url, token_endpoint: config.oidc.token_url, jwks_uri: config.oidc.jwks_url, response_types_supported: ['code'], code_challenge_methods_supported: ['S256'] })) };
}
test('origin probe default does not resolve or request and cannot mark environment verified', async () => {
  const forbidden = () => { throw new Error('NETWORK_WAS_CALLED'); };
  const result = await probeCredentialOrigins(config, { resolve: forbidden, request: forbidden });
  assert.equal(result.decision, 'PREFLIGHT_PLAN_VALID'); assert.equal(result.environment_status, 'ENV_PENDING'); assert.deepEqual(result.checks, []);
});
test('origin probe strictly verifies all handoff bytes, hash, staged headers and no cookie', () => {
  validateHandoffResponse(handoff(), config.credential_origin, build);
  for (const mutate of [r => r.status = 302, r => r.headers['set-cookie'] = ['session=private'], r => r.headers['referrer-policy'] = 'no-referrer', r => r.headers['content-security-policy'] += "; script-src 'self'", r => r.headers['strict-transport-security'] = 'max-age=0', r => r.body = Buffer.concat([r.body, Buffer.from('<img src="https://other.invalid">')]), r => r.body = Buffer.from(r.body.toString().replace('<script>', '<script src="/malicious.js">'))]) {
    const value = handoff(); mutate(value); assert.throws(() => validateHandoffResponse(value, config.credential_origin, build), /^Error: CREDENTIAL_EDGE_/);
  }
});
test('origin probe requires cookie-free 401 and exact OIDC metadata instead of login redirects', () => {
  validateCredentialResponse(credential(), config); validateCredentialDiscovery(discovery(), config);
  for (const mutate of [r => r.status = 200, r => r.status = 303, r => r.body = Buffer.from('{"code":"CREDENTIAL_SESSION_INVALID"}'), r => r.headers['referrer-policy'] = 'origin']) {
    const value = credential(); mutate(value); assert.throws(() => validateCredentialResponse(value, config), /^Error: CREDENTIAL_EDGE_/);
  }
  for (const key of ['issuer', 'authorization_endpoint', 'token_endpoint', 'jwks_uri', 'response_types_supported', 'code_challenge_methods_supported']) {
    const value = discovery(); const body = JSON.parse(value.body); body[key] = key.endsWith('supported') ? [] : 'https://other.invalid'; value.body = Buffer.from(JSON.stringify(body));
    assert.throws(() => validateCredentialDiscovery(value, config), /^Error: CREDENTIAL_EDGE_/);
  }
});
test('simulated read-only preflight requests only seven fixed public URLs and retains incomplete evidence', async () => {
  const targets = []; const names = [];
  const request = async target => {
    targets.push(target);
    if (target.endsWith('/credential-handoff')) return handoff();
    if (target.endsWith('/session')) return credential();
    if (target.includes('/assets/')) return credential(true);
    if (target.endsWith('/.well-known/openid-configuration')) return discovery();
    if (target === config.workload.jwks_url) return { status: 200, certificate_sha256: 'd'.repeat(64), body: Buffer.from('{"keys":[{"kty":"RSA"}]}') };
    throw new Error('UNEXPECTED_TARGET');
  };
  const result = await probeCredentialOrigins(config, { probe: true, resolve: async name => names.push(name), request });
  assert.equal(targets.length, 7); assert.equal(names.length, 2); assert.ok(targets.every(value => !/[?#]/.test(value) && !value.endsWith('/launch') && !value.endsWith('/credentials')));
  assert.equal(result.decision, 'PREFLIGHT_PASSED'); assert.equal(result.environment_status, 'ENV_PENDING'); assert.equal(result.deployment_status, 'NOT_DEPLOYED');
  assert.ok(result.incomplete.includes('AUTHENTICATED_HANDSHAKE')); assert.ok(result.incomplete.includes('ALL_LAYER_LOG_SCAN'));
});
