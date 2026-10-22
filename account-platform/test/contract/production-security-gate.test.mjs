import assert from 'node:assert/strict';
import test from 'node:test';
import { execFile, execFileSync } from 'node:child_process';
import { promisify } from 'node:util';
import { existsSync, promises as fs } from 'node:fs';
import https from 'node:https';
import { once } from 'node:events';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
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
  assert.throws(() => productionSecurityConfiguration({ ...environment, NODE_TLS_REJECT_UNAUTHORIZED:'0' }), /tls_verification_required/);
  assert.doesNotThrow(() => productionSecurityConfiguration({ ...environment, PINVOU_SECURITY_TOKEN_GATEWAY_READY_URL:'http://127.0.0.1:18083/health/ready' }));
  assert.throws(() => productionSecurityConfiguration({ ...environment, PINVOU_SECURITY_TOKEN_GATEWAY_READY_URL:'http://token.internal/health/ready' }), /endpoint_invalid/);
  assert.throws(() => productionSecurityConfiguration({ ...environment, TOKEN_SECRET_KEY_ID:'kms-key-v1' }), /rotation_key_unchanged/);
  assert.throws(() => productionSecurityConfiguration({ ...environment, PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON:'[]' }), /endpoints_incomplete/);
});

test('production security gate proves TLS reachability, live readiness, and retired remote KMS key', async () => {
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
  let reads = 0;
  const fetcher = async url => {
    reads++;
    return new Response(JSON.stringify(String(url).includes('vault.internal')
      ? { status:'ready', active_key_id:'kms-key-v2', key_states:{ 'kms-key-v2':'ENCRYPT_DECRYPT', 'kms-key-v1':'DISABLED' } }
      : { status:'ready' }), { status:200 });
  };
  await assert.rejects(() => runProductionSecurityGate(configuration, fetcher, tlsInspector), /certificate_expiring/);
  assert.equal(reads, 0);
});


test('public evidence rejects ambiguous input, invalid dates and malformed readiness or KMS JSON', async () => {
  for (const raw of [environment.PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON.replace('"name":"portal"', '"name":"portal","name":"identity"'),
    environment.PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON.replace('"expected_status":200', '"expected_status":200,"extra":true')]) {
    assert.throws(() => productionSecurityConfiguration({ ...environment, PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON: raw }), /configuration_invalid|endpoint_invalid/);
  }
  const configuration = productionSecurityConfiguration(environment);
  for (const expiresAt of [NaN, Infinity, undefined]) {
    await assert.rejects(runProductionSecurityGate(configuration, async () => new Response('{}'), async () => ({ expiresAt })), /certificate_expiring/);
  }
  const tls = async () => ({ expiresAt: Date.now() + 30 * 86400000 });
  for (const raw of ['null', '[]', '{"status":"ready","status":"ready"}', Buffer.from([0xc0, 0xaf])]) {
    await assert.rejects(runProductionSecurityGate(configuration, async url => new Response(String(url).includes('token-gateway') ? raw : '{}'), tls), /response_invalid|rotation_not_ready/);
  }
  for (const raw of ['null', '[]', '{"status":"ready","active_key_id":"kms-key-v2","key_states":null}',
    '{"status":"ready","active_key_id":"kms-key-v2","key_states":{"kms-key-v1":"DISABLED","kms-key-v1":"ENCRYPT_DECRYPT"}}']) {
    await assert.rejects(runProductionSecurityGate(configuration, async url => new Response(String(url).includes('vault.internal') ? raw : '{"status":"ready"}'), tls), /response_invalid|kms_rotation_unverified/);
  }
});

test('actual CLI verifies trusted local TLS, exact HTTP statuses and remote key evidence without printing secrets', async t => {
  const directory = await fs.mkdtemp(path.join(os.tmpdir(), 'pinvou-security-gate-'));
  t.after(async () => {
    const relative = path.relative(path.resolve(os.tmpdir()), path.resolve(directory));
    assert.ok(relative.startsWith('pinvou-security-gate-') && !relative.includes(path.sep) && !path.isAbsolute(relative));
    await fs.rm(directory, { recursive: true, force: true });
  });
  const caFile = path.join(directory, 'ca.crt'), keyFile = path.join(directory, 'ca.key');
  const gitOpenSSL = process.platform === 'win32' && process.env.ProgramFiles
    ? path.join(process.env.ProgramFiles, 'Git', 'usr', 'bin', 'openssl.exe') : '';
  const openssl = process.env.PINVOU_TEST_OPENSSL_BINARY || (gitOpenSSL && existsSync(gitOpenSSL) ? gitOpenSSL : 'openssl');
  execFileSync(openssl, ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-days', '30', '-subj', '/CN=fixture',
    '-addext', 'subjectAltName=DNS:localhost,IP:127.0.0.1', '-addext', 'basicConstraints=critical,CA:TRUE',
    '-keyout', keyFile, '-out', caFile], { stdio: 'ignore', windowsHide: true });
  const canary = 'fixture-secret-never-emit-8ce7d6a', seen = [];
  let endpointStatus = 200, previousState = 'DISABLED', readyBody = '{"status":"ready"}';
  const server = https.createServer({ key: await fs.readFile(keyFile), cert: await fs.readFile(caFile) }, (req, res) => {
    seen.push({ method: req.method, url: req.url, authorization: req.headers.authorization });
    res.writeHead(['/portal', '/identity', '/oidc'].includes(req.url) ? endpointStatus : 200, { 'Content-Type': 'application/json' });
    if (req.url === '/ready') res.end(readyBody);
    else if (req.url === '/kms') res.end(JSON.stringify({ status:'ready', active_key_id:'kms-key-v2', key_states:{ 'kms-key-v2':'ENCRYPT_DECRYPT', 'kms-key-v1':previousState }, ignored: canary }));
    else res.end('{}');
  });
  server.listen(0, '127.0.0.1');
  await once(server, 'listening');
  t.after(() => new Promise(resolve => { server.closeAllConnections(); server.close(resolve); }));
  const origin = `https://127.0.0.1:${server.address().port}`;
  const env = { ...process.env, ...environment, NODE_EXTRA_CA_CERTS: caFile,
    PINVOU_SECURITY_PUBLIC_ENDPOINTS_JSON: JSON.stringify(['portal', 'identity', 'oidc'].map(name => ({ name, url: origin + '/' + name, expected_status:200 }))),
    PINVOU_SECURITY_TOKEN_GATEWAY_READY_URL: origin + '/ready', PINVOU_SECURITY_KMS_HEALTH_URL: origin + '/kms',
    TOKEN_SECRET_PROVIDER_TOKEN: canary, TOKEN_SECRET_PROVIDER: 'remote', PINVOU_SECURITY_INSTALL_MODE: 'migration', NODE_TLS_REJECT_UNAUTHORIZED:'1' };
  const cli = fileURLToPath(new URL('../../../scripts/account-platform-production-security-gate.mjs', import.meta.url));
  const run = args => promisify(execFile)(process.execPath, [cli, ...args], { env, windowsHide: true, timeout:15000, maxBuffer:65536 });
  await t.test('trusted TLS and actual remote GET chain produces bounded digest evidence', async () => {
    const result = await run([]), evidence = JSON.parse(result.stdout);
    assert.equal(evidence.decision, 'GO'); assert.equal(evidence.public_endpoints.length, 3);
    assert.match(evidence.evidence_sha256, /^[a-f0-9]{64}$/);
    assert.equal((result.stdout + result.stderr).includes(canary), false);
    assert.deepEqual(seen.map(item => item.url), ['/portal', '/identity', '/oidc', '/ready', '/kms']);
    assert.ok(seen.every(item => item.method === 'GET'));
    assert.ok(seen.slice(0, 4).every(item => item.authorization === undefined));
    assert.equal(seen.at(-1).authorization, 'Bearer ' + canary);
  });
  await t.test('untrusted certificate fails before any HTTP request', async () => {
    const before = seen.length, untrustedEnv = { ...env }; delete untrustedEnv.NODE_EXTRA_CA_CERTS;
    await assert.rejects(promisify(execFile)(process.execPath, [cli], { env:untrustedEnv, windowsHide:true, timeout:15000, maxBuffer:65536 }), error => error.code === 1 && error.stdout === '' && error.stderr.trim() === 'production_security_check_failed');
    assert.equal(seen.length, before);
  });
  await t.test('wrong status fails closed', async () => {
    endpointStatus = 503;
    await assert.rejects(run([]), error => error.code === 1 && error.stdout === '' && /endpoint_status_mismatch:portal:503/.test(error.stderr) && !error.stderr.includes(canary));
    endpointStatus = 200;
  });
  await t.test('remote prior key remains strictly disabled', async () => {
    previousState = 'DECRYPT_ONLY';
    await assert.rejects(run([]), error => error.code === 1 && error.stdout === '' && /kms_rotation_unverified/.test(error.stderr) && !error.stderr.includes(canary));
    previousState = 'DISABLED';
  });
  await t.test('ambiguous actual readiness response is rejected', async () => {
    readyBody = '{"status":"ready","status":"not-ready"}';
    await assert.rejects(run([]), error => error.code === 1 && error.stdout === '' && /response_invalid/.test(error.stderr));
    readyBody = '{"status":"ready"}';
  });
  await t.test('invalid arguments and module import never trigger endpoint probes', async () => {
    const before = seen.length;
    await assert.rejects(run(['--unknown-' + canary]), error => error.code === 1 && error.stdout === '' && error.stderr.trim() === 'production_security_arguments_invalid');
    const imported = await promisify(execFile)(process.execPath, ['--input-type=module', '-e', `await import(${JSON.stringify(new URL('../../../scripts/account-platform-production-security-gate.mjs', import.meta.url).href)})`], { env, windowsHide: true, timeout:5000 });
    assert.equal(imported.stdout + imported.stderr, ''); assert.equal(seen.length, before);
  });
});


test('package exposes both manual entry points and runbooks distinguish runtime duties', async () => {
  const pkg = JSON.parse(await fs.readFile(new URL('../../package.json', import.meta.url), 'utf8'));
  assert.equal(pkg.scripts['security:production-readiness'], 'node ../scripts/account-platform-production-security-gate.mjs');
  assert.equal(pkg.scripts['security:prepare-native-rotation'], 'node ../scripts/account-platform-production-security-gate.mjs --prepare-native-rotation');
  const instructions = await fs.readFile(new URL('../../deploy/runbooks/native-kms-security-gate.md', import.meta.url), 'utf8');
  for (const command of ['security:production-readiness', 'security:prepare-native-rotation', 'kms-agent-check', 'kms-monitor', 'kms-operator']) assert.ok(instructions.includes(command));
  assert.ok(instructions.includes('/run/pinvou-kms-agents/pinvou-token-gateway/token'));
  assert.ok(instructions.includes('/run/pinvou-kms-agents/pinvou-credential-portal/token'));
  assert.ok(!instructions.includes('account-platform-native-gate-fixture.mjs'));
});
