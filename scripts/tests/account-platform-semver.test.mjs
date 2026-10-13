import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile, mkdtemp, writeFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createServer } from 'node:http';
import { execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { createHash, generateKeyPairSync, sign } from 'node:crypto';
import { compareSemVer, parseSemVer } from '../account-platform-semver.mjs';

const vectors = JSON.parse(await readFile(new URL('../../account-platform/internal/platform/clientcompat/testdata/semver-vectors.json', import.meta.url), 'utf8'));

test('shared SemVer 2 valid inputs preserve all core and suffix syntax', async t => {
  assert.equal(vectors.valid.length, 22);
  for (const version of vectors.valid) await t.test(version, () => assert.doesNotThrow(() => parseSemVer(version)));
});

test('shared SemVer 2 invalid inputs fail on both comparison sides', async t => {
  assert.equal(vectors.invalid.length, 40);
  for (const version of vectors.invalid) await t.test(JSON.stringify(version), () => {
    assert.throws(() => parseSemVer(version), /release_gate_version_invalid/);
    assert.throws(() => compareSemVer(version, '1.0.0'), /release_gate_version_invalid/);
    assert.throws(() => compareSemVer('1.0.0', version), /release_gate_version_invalid/);
  });
});

test('shared SemVer 2 precedence is precise in both directions and reflexive', async t => {
  assert.equal(vectors.comparisons.length, 31);
  for (const { left, right, expected } of vectors.comparisons) await t.test(`${left} : ${right}`, () => {
    assert.equal(Math.sign(compareSemVer(left, right)), expected);
    assert.equal(Math.sign(compareSemVer(right, left)), expected === 0 ? 0 : -expected);
    assert.equal(compareSemVer(left, left), 0);
    assert.equal(compareSemVer(right, right), 0);
  });
});

test('sorting keeps build-only equal versions stable without coercing input types', () => {
  const versions = ['1.0.0+b', '1.0.0+a', '1.0.0-rc.2', '1.0.0-rc.1'];
  assert.deepEqual(versions.toSorted(compareSemVer), ['1.0.0-rc.1', '1.0.0-rc.2', '1.0.0+b', '1.0.0+a']);
  for (const value of [undefined, null, 123, {}, ['1.0.0']]) {
    assert.throws(() => parseSemVer(value), /release_gate_version_invalid/);
  }
});

test('actual release CLI applies SemVer bounds while retaining signed raw version identity', async t => {
  // Real child CLI, real Ed25519 signatures, controlled loopback metadata.
  // This exercises the release command, not a deployed issuer/provider/E2E.
  const directory = await mkdtemp(join(tmpdir(), 'pinvou-semver-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  const artifact = Buffer.from('public synthetic semver release artifact');
  const artifactPath = join(directory, 'artifact.bin');
  const attestationPath = join(directory, 'attestation.json');
  await writeFile(artifactPath, artifact);
  const digest = createHash('sha256').update(artifact).digest('hex');
  const { publicKey, privateKey } = generateKeyPairSync('ed25519');
  const rawPublic = publicKey.export({ format: 'der', type: 'spki' }).subarray(-32);
  let minimum = '1.0.0';
  let maximum = '1.0.0';
  let requests = 0;
  let origin;
  const server = createServer((request, response) => {
    requests++;
    response.setHeader('Content-Type', 'application/json');
    const body = request.url === '/compatibility' ? {
      attestation_available: true, environment: 'test', attestation_key_id: 'synthetic-semver-key',
      contract_version: 'client-readiness-v1', service_version: 'controlled-service',
      minimum_client_version: minimum, maximum_client_version: maximum,
    } : { issuer: origin, jwks_uri: `${origin}/jwks` };
    response.end(JSON.stringify(body));
  });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  origin = `http://127.0.0.1:${server.address().port}`;
  t.after(() => new Promise((resolve, reject) => server.close(error => error ? reject(error) : resolve())));
  const execute = promisify(execFile);
  const gate = fileURLToPath(new URL('../account-platform-release-gate.mjs', import.meta.url));
  const root = fileURLToPath(new URL('../../', import.meta.url));
  const run = async (version, low, high, signedVersion = version) => {
    minimum = low; maximum = high;
    const now = Date.now();
    const claims = {
      version: 1, environment: 'test', artifact_sha256: digest, client_version: signedVersion,
      protocol_code: 'CHAT', catalog_version: 9, contract_version: 'client-readiness-v1',
      service_version: 'controlled-service', issued_at: new Date(now).toISOString(),
      expires_at: new Date(now + 5 * 60_000).toISOString(),
      check_codes: ['GATEWAY_CONTRACT:PASS', 'ACCOUNT_CONTRACT:PASS', 'OIDC_METADATA:PASS',
        'MODEL_CATALOG:PASS', 'MANAGED_ROUTE:PASS', 'PRICING_FORMULA:PASS'],
    };
    await writeFile(attestationPath, JSON.stringify({ algorithm: 'Ed25519', key_id: 'synthetic-semver-key', claims,
      signature_base64url: sign(null, Buffer.from(JSON.stringify(claims)), privateKey).toString('base64url') }));
    return execute(process.execPath, [gate], { cwd: root, timeout: 20_000, env: {
      ...process.env, PINVOU_RELEASE_ARTIFACT: artifactPath, PINVOU_RELEASE_ATTESTATION: attestationPath,
      PINVOU_CLIENT_COMPATIBILITY_URL: `${origin}/compatibility`, PINVOU_ACCOUNT_OIDC_ISSUER: origin,
      PINVOU_ACCOUNT_SERVICE_URL: origin, PINVOU_TOKEN_GATEWAY_URL: origin, PINVOU_ACCOUNT_PORTAL_URL: origin,
      PINVOU_ACCOUNT_ENVIRONMENT: 'test', PINVOU_CLIENT_READINESS_KEY_ID: 'synthetic-semver-key',
      PINVOU_CLIENT_READINESS_PUBLIC_KEY: rawPublic.toString('base64url'), PINVOU_RELEASE_CLIENT_VERSION: version,
    } });
  };
  await t.test('prerelease cannot satisfy a stable minimum', () => assert.rejects(run('1.0.0-rc.1', '1.0.0', '2.0.0'), /release_gate_compatibility_mismatch/));
  await t.test('stable cannot satisfy an earlier prerelease maximum', () => assert.rejects(run('1.0.0', '0.0.0', '1.0.0-rc.1'), /release_gate_compatibility_mismatch/));
  await t.test('prerelease with build is admitted only inside its exact bounds', async () => {
    const result = await run('1.0.0-rc.2+001', '1.0.0-rc.1', '1.0.0-rc.10');
    assert.match(result.stdout, /"decision":"GO"/);
    assert.match(result.stdout, /"client_version":"1\.0\.0-rc\.2\+001"/);
  });
  await t.test('build does not affect order but output retains the original string', async () => {
    const result = await run('1.0.0+client.001', '1.0.0+other', '1.0.0+another');
    assert.match(result.stdout, /"client_version":"1\.0\.0\+client\.001"/);
  });
  await t.test('equal-precedence build strings cannot substitute a signed client identity', () => assert.rejects(run('1.0.0+client.002', '1.0.0', '1.0.0', '1.0.0+client.001'), /release_gate_attestation_binding_invalid/));
  await t.test('invalid lower and upper metadata bounds both fail closed', async () => {
    await assert.rejects(run('1.0.0', '1.0.0-01', '2.0.0'), /release_gate_version_invalid/);
    await assert.rejects(run('1.0.0', '0.0.0', '1.0.0+'), /release_gate_version_invalid/);
  });
  await t.test('whitespace client input is rejected before files or metadata fetch', async () => {
    const before = requests;
    await rm(artifactPath);
    await assert.rejects(run(' 1.0.0 ', '1.0.0', '1.0.0', '1.0.0'), /release_gate_version_invalid/);
    assert.equal(requests, before);
  });
});
