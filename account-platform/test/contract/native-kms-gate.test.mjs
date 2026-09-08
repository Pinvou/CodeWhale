import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { existsSync, promises as fs } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import test from 'node:test';
import https from 'node:https';
import { once } from 'node:events';
import { nativeKmsConfiguration, prepareNativeRotation, verifyNativeRotation, validateNativeLease,
  ciphertextVersion, databaseQuery, readGateFile } from '../../../scripts/account-platform-native-kms-gate.mjs';
import { productionSecurityConfiguration, runProductionSecurityGate } from '../../../scripts/account-platform-production-security-gate.mjs';

const tokens = ['fixture-gateway-token-not-real', 'fixture-credential-token-not-real'];
const roles = ['pinvou-token-gateway', 'pinvou-credential-portal'];
const lease = index => ({ id: tokens[index], policies: [roles[index]], type: 'service', renewable: true,
  orphan: true, path: 'auth/approle/login', meta: { role_name: roles[index] }, ttl: 200,
  explicit_max_ttl: 900, creation_time: Math.floor(Date.now() / 1000) - 60,
  last_renewal: new Date(Date.now() - 10000).toISOString() });

test('native gate configuration separates fresh-install from legacy migration', () => {
  const root = path.resolve(os.tmpdir(), 'native-gate-config');
  const env = { TOKEN_SECRET_PROVIDER: 'vault-transit', PINVOU_SECURITY_INSTALL_MODE: 'fresh-install',
    TOKEN_SECRET_PROVIDER_URL: 'https://kms.example.com:8200', TOKEN_SECRET_KEY_ID: 'key',
    TOKEN_SECRET_PROVIDER_TOKEN_FILE: path.join(root, 'gateway'), PINVOU_SECURITY_CREDENTIAL_TOKEN_FILE: path.join(root, 'credentials'),
    TOKEN_SECRET_PROVIDER_CA_FILE: path.join(root, 'ca'), PINVOU_SECURITY_NATIVE_ROTATION_PROOF_FILE: path.join(root, 'proof'),
    PINVOU_SECURITY_FRESH_DATABASE_URL_FILE: path.join(root, 'db') };
  assert.equal(nativeKmsConfiguration(env).key, 'key');
  for (const mutation of [{ PINVOU_SECURITY_INSTALL_MODE: 'migration' }, { TOKEN_SECRET_PROVIDER_TOKEN: 'static-secret' },
    { PINVOU_SECRET_ROTATION_PREVIOUS_KEY_ID: 'old' }, { TOKEN_SECRET_PROVIDER_URL: 'http://kms.example.com' },
    { TOKEN_SECRET_PROVIDER_URL: 'https://kms.example.com/path' }, { TOKEN_SECRET_PROVIDER_URL: 'https://user:secret@kms.example.com' },
    { PINVOU_SECURITY_CREDENTIAL_TOKEN_FILE: env.TOKEN_SECRET_PROVIDER_TOKEN_FILE }, { TOKEN_SECRET_KEY_ID: "key';DROP" }]) {
    assert.throws(() => nativeKmsConfiguration({ ...env, ...mutation }), /native_kms_gate_/);
  }
  const publicEnv = { ...env, PINVOU_SECURITY_TOKEN_GATEWAY_READY_URL: 'http://127.0.0.1:18083/health/ready',
    PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON: JSON.stringify(['portal', 'identity', 'oidc', 'credentials'].map(name => ({ name, url: `https://${name}.example.com/`, expected_status: 200 }))) };
  assert.equal(productionSecurityConfiguration(publicEnv).provider, 'vault-transit');
  assert.throws(() => productionSecurityConfiguration({ ...publicEnv, PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON: JSON.stringify(JSON.parse(publicEnv.PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON).slice(0, 3)) }), /endpoints_incomplete/);
});

test('native gate requires distinct least-privilege renewed leases and strict versions', () => {
  validateNativeLease(lease(0), roles[0]);
  validateNativeLease({ ...lease(0), identity_policies: [] }, roles[0]);
  for (const mutation of [{ policies: ['root'] }, { policies: [roles[0], 'default'] }, { ttl: 301 }, { explicit_max_ttl: 3600 },
    { renewable: false }, { orphan: false }, { path: 'auth/token/create' }, { last_renewal: '' }, { meta: { role_name: roles[1] } },
    { identity_policies: ['fixture-extra-policy'] }, { identity_policies: null }, { identity_policies: '' }]) {
    assert.throws(() => validateNativeLease({ ...lease(0), ...mutation }, roles[0]), /native_kms_gate_/);
  }
  assert.equal(ciphertextVersion('vault:v2:YWJj'), 2);
  for (const value of ['vault:v0:YWJj', 'vault:v01:YWJj', 'vault:v999999999999999999999:YWJj', 'not-a-ciphertext']) {
    assert.throws(() => ciphertextVersion(value), /ciphertext_invalid/);
  }
  assert.match(databaseQuery('key'), /^BEGIN READ ONLY;/);
  assert.match(databaseQuery('key'), /usage_dispatch_intents/);
  assert.match(databaseQuery('key'), /public\.credential_isolation_sessions/);
  assert.match(databaseQuery('key'), /octet_length\(pkce_verifier_ciphertext\)>0/);
  assert.doesNotMatch(databaseQuery('key'), /UPDATE|DELETE|INSERT|DROP/);
  assert.throws(() => databaseQuery("key';DROP"), /key_invalid/);
});

test('native prepare and verify require real transitions and bound private state', async t => {
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), 'pinvou-native-gate-'));
  await fs.chmod(directory, 0o700);
  t.after(() => fs.rm(directory, { recursive: true, force: true }));
  const caFile = path.join(directory, 'ca.crt');
  const gitOpenSSL = process.platform === 'win32' && process.env.ProgramFiles
    ? path.join(process.env.ProgramFiles, 'Git', 'usr', 'bin', 'openssl.exe') : '';
  const openssl = process.env.PINVOU_TEST_OPENSSL_BINARY || (gitOpenSSL && existsSync(gitOpenSSL) ? gitOpenSSL : 'openssl');
  // Disposable test CA only. Generation never reads custody/production material.
  execFileSync(openssl, ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '30', '-subj', '/CN=fixture',
    '-addext', 'subjectAltName=DNS:localhost,IP:127.0.0.1',
    '-addext', 'basicConstraints=critical,CA:TRUE',
    '-keyout', path.join(directory, 'ca.key'), '-out', caFile], { stdio: 'ignore', windowsHide: true });
  const tokenFiles = [path.join(directory, 'gateway'), path.join(directory, 'credentials')];
  await Promise.all(tokenFiles.map((file, index) => fs.writeFile(file, tokens[index], { mode: 0o600 })));
  const configuration = { endpoint: 'https://kms.example.com:8200', key: 'key', tokenFiles, caFile,
    proofFile: path.join(directory, 'proof'), databaseFile: path.join(directory, 'db') };
  let version = 1, database = { identity: 'a'.repeat(64), total: 0, foreign: 0 }, wrongRole = false;
  const values = new Map();
  const fixtureRequest = async (_, __, endpoint, token, body) => {
    if (endpoint === 'sys/health') return { status: 200, body: { initialized: true, sealed: false } };
    const index = tokens.indexOf(token);
    assert.ok(index >= 0);
    if (endpoint === 'auth/token/lookup-self') return { status: 200, body: { data: lease(wrongRole ? 0 : index) } };
    if (endpoint.startsWith('transit/encrypt/')) {
      const ciphertext = `vault:v${version}:` + Buffer.from(String(values.size + 1)).toString('base64');
      values.set(ciphertext, body);
      return { status: 200, body: { data: { ciphertext } } };
    }
    const value = values.get(body.ciphertext);
    return value && value.associated_data === body.associated_data
      ? { status: 200, body: { data: { plaintext: value.plaintext } } } : { status: 400, body: {} };
  };
  const server = https.createServer({ key: await fs.readFile(path.join(directory, 'ca.key')), cert: await fs.readFile(caFile) }, async (req, res) => {
    const chunks = [];
    for await (const chunk of req) chunks.push(chunk);
    const payload = Buffer.concat(chunks);
    const result = await fixtureRequest(null, null, req.url.slice('/v1/'.length), req.headers['x-vault-token'], payload.length ? JSON.parse(payload) : undefined);
    res.writeHead(result.status, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(result.body));
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => new Promise(resolve => { server.closeAllConnections(); server.close(resolve); }));
  configuration.endpoint = `https://127.0.0.1:${server.address().port}`;
  const dependencies = { database: async () => database };
  database = { ...database, total: 1 };
  await assert.rejects(prepareNativeRotation(configuration, dependencies), /database_not_fresh/);
  database = { ...database, total: 0 };
  assert.equal((await prepareNativeRotation(configuration, dependencies)).decision, 'PREPARED_NOT_GO');
  const original = await fs.readFile(configuration.proofFile);
  await assert.rejects(prepareNativeRotation(configuration, dependencies), /EEXIST/);
  assert.deepEqual(await fs.readFile(configuration.proofFile), original);
  await assert.rejects(verifyNativeRotation(configuration, dependencies), /rotation_not_observed/);
  version = 2;
  const evidence = await verifyNativeRotation(configuration, dependencies);
  assert.equal(evidence.active_version, 2);
  assert.equal(evidence.previous_version, 1);
  assert.ok(!JSON.stringify(evidence).includes('vault:v') && !JSON.stringify(evidence).includes(tokens[0]));
  wrongRole = true;
  await assert.rejects(verifyNativeRotation(configuration, dependencies), /identity_or_lease_invalid/);
  wrongRole = false;
  database = { ...database, foreign: 1 };
  await assert.rejects(verifyNativeRotation(configuration, dependencies), /database_changed_or_legacy/);
  database = { ...database, foreign: 0, identity: 'b'.repeat(64) };
  await assert.rejects(verifyNativeRotation(configuration, dependencies), /database_changed_or_legacy/);
  database = { ...database, identity: 'a'.repeat(64) };
  const proof = JSON.parse(original);
  for (const mutation of [{ endpoint: 'https://foreign.example.com' }, { key: 'other' }, { ca_sha256: 'b'.repeat(64) },
    { version: 99 }, { prepared_at: '2000-01-01T00:00:00Z' }, { prepared_at: '2999-01-01T00:00:00Z' }]) {
    await fs.writeFile(configuration.proofFile, JSON.stringify({ ...proof, ...mutation }));
    await assert.rejects(verifyNativeRotation(configuration, dependencies), /native_kms_gate_/);
  }
  await fs.writeFile(configuration.proofFile, original);
  await fs.writeFile(tokenFiles[0], 'short');
  await assert.rejects(verifyNativeRotation(configuration, dependencies), /token_invalid/);
  if (process.platform !== 'win32') {
    await fs.chmod(configuration.proofFile, 0o644);
    await assert.rejects(readGateFile(configuration.proofFile), /file_unavailable_or_insecure/);
  }
});

test('native GO still requires public TLS and live readiness; oversized bodies fail closed', async () => {
  const configuration = { provider: 'vault-transit', endpoints: [{ name: 'portal', url: new URL('https://portal.example.com'), expectedStatus: 200 }],
    tokenReadyURL: new URL('https://token.example.com/health/ready'), native: {} };
  let invoked = false;
  const verifier = async () => { invoked = true; return { active_version: 2 }; };
  const tls = async () => ({ expiresAt: Date.now() + 30 * 86400000 });
  const fetcher = async url => new Response(String(url).includes('/health/ready') ? JSON.stringify({ status: 'ready' }) : '{}');
  assert.equal((await runProductionSecurityGate(configuration, fetcher, tls, verifier)).decision, 'GO');
  assert.equal(invoked, true);
  invoked = false;
  await assert.rejects(runProductionSecurityGate(configuration, async () => new Response('x'.repeat(65537)), tls, verifier), /response_too_large/);
  assert.equal(invoked, false);
  await assert.rejects(runProductionSecurityGate(configuration, async () => new Response('{'), tls, verifier), /response_invalid/);
});
