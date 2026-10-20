import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import YAML from 'yaml';
import { checkComposeKMS } from '../../../scripts/account-platform-kms-compose-check.mjs';
import { checkGovernedComposeConfiguration } from '../../../scripts/account-platform-model-token-deployment-check.mjs';
import { isReadOnlyDirectoryBind } from '../../../scripts/account-platform-compose-bind.mjs';

const names = ['token-gateway', 'token-credential-portal'];
const tokenTarget = '/var/run/secrets/pinvou/kms';
const caTarget = '/var/run/secrets/pinvou/kms-ca';
function fixture() {
  return { networks: { 'kms-transit': { external: true, name: 'fixture-only-private-kms' } },
    services: Object.fromEntries(names.map((name, index) => [name, {
      user: '65532:65532', read_only: true, networks: { 'kms-transit': {} },
      environment: { TOKEN_SECRET_PROVIDER: 'vault-transit', TOKEN_SECRET_PROVIDER_URL: 'https://openbao.example.invalid:8200',
        TOKEN_SECRET_KEY_ID: 'fixture-key', TOKEN_SECRET_PROVIDER_TOKEN: '', TOKEN_LOCAL_KEK_BASE64: '',
        TOKEN_SECRET_PROVIDER_TOKEN_FILE: `${tokenTarget}/token`, TOKEN_SECRET_PROVIDER_CA_FILE: `${caTarget}/ca.crt` },
      volumes: [tokenTarget, caTarget].map((target, slot) => ({ type: 'bind',
        source: slot ? '/opt/fixture/public-ca' : `/run/fixture/identity-${index}`, target,
        read_only: true, bind: { create_host_path: false } })),
    }])) };
}
test('native Compose keeps separate renewable directory identities and rejects unsafe configuration', () => {
  checkComposeKMS(fixture());
  const changes = [
    c => { c.services[names[1]].environment.TOKEN_SECRET_PROVIDER = 'remote'; },
    c => { c.services[names[0]].environment.TOKEN_SECRET_PROVIDER_URL = 'http://openbao'; },
    c => { c.services[names[0]].environment.TOKEN_SECRET_PROVIDER_URL += '/transit'; },
    c => { c.services[names[0]].environment.TOKEN_SECRET_PROVIDER_URL += '/?'; },
    c => { c.services[names[0]].environment.TOKEN_SECRET_PROVIDER_URL += '/%2f'; },
    c => { c.services[names[0]].environment.TOKEN_SECRET_PROVIDER_TOKEN = 'fixture-static'; },
    c => { c.services[names[0]].environment.TOKEN_LOCAL_KEK_BASE64 = 'fixture-local'; },
    c => { c.services[names[0]].environment.TOKEN_SECRET_PROVIDER_TOKEN_FILE += '-other'; },
    c => { c.services[names[0]].environment.TOKEN_SECRET_PROVIDER_CA_FILE = '/etc/ssl/ca.crt'; },
    c => { for (const name of names) c.services[name].environment.TOKEN_SECRET_KEY_ID = '../other'; },
    c => { for (const name of names) c.services[name].environment.TOKEN_SECRET_KEY_ID = '-wrong'; },
    c => { for (const name of names) c.services[name].environment.TOKEN_SECRET_KEY_ID = 'x'.repeat(129); },
    c => { c.services[names[0]].user = '0:0'; },
    c => { c.services[names[0]].read_only = false; },
    c => { c.services[names[0]].privileged = true; },
    c => { c.services[names[0]].network_mode = 'host'; },
    c => { c.services[names[0]].volumes[0].target += '/token'; },
    c => { c.services[names[0]].volumes[0].read_only = false; },
    c => { c.services[names[0]].volumes[0].bind.create_host_path = true; },
    c => { c.services[names[0]].volumes[0].source = '/run/fixture'; },
    c => { c.services[names[0]].volumes[0].source = '/'; },
    c => { c.services[names[0]].volumes[0].source = '/run/fixture/../identity'; },
    c => { c.services[names[0]].volumes[0].source = c.services[names[1]].volumes[0].source; },
    c => { c.services[names[0]].volumes.push({ type: 'volume', target: `${tokenTarget}/token`, source: 'old-token' }); },
    c => { c.services[names[0]].volumes.push({ type: 'bind', target: '/elsewhere', source: '/run/fixture' }); },
    c => { c.services[names[1]].volumes[1].source = '/opt/other-ca'; },
    c => { c.networks['kms-transit'].external = false; },
    c => { c.networks.alias = { ...c.networks['kms-transit'] }; },
    c => { delete c.services[names[0]].networks['kms-transit']; },
    c => { c.services.executor = { networks: { 'kms-transit': {} } }; },
    c => { c.services.executor = { network_mode: `service:${names[0]}` }; },
    c => { c.services.executor = { volumes_from: [names[0]] }; },
    c => { c.services.executor = { volumes: [{ type: 'bind', source: '/run/fixture', target: '/stolen' }] }; },
    c => { c.services.executor = { environment: { TOKEN_SECRET_PROVIDER_TOKEN_FILE: '/some-token' } }; },
  ];
  for (const [index, change] of changes.entries()) {
    const c = fixture(); change(c); assert.throws(() => checkComposeKMS(c), /model_token_deployment_kms_/, `case ${index}`);
  }
});

test('remote remains supported but empty/shared static credentials and native-only CA are rejected', () => {
  function remote() {
    const c = fixture();
    for (const name of names) Object.assign(c.services[name].environment, { TOKEN_SECRET_PROVIDER: 'remote',
      TOKEN_SECRET_PROVIDER_TOKEN: `fixture-${name}`, TOKEN_SECRET_PROVIDER_TOKEN_FILE: '', TOKEN_SECRET_PROVIDER_CA_FILE: '' });
    return c;
  }
  checkComposeKMS(remote());
  for (const [key, value] of [['TOKEN_SECRET_PROVIDER_TOKEN', ''], ['TOKEN_SECRET_PROVIDER_TOKEN', '   '],
    ['TOKEN_SECRET_PROVIDER_TOKEN', `fixture-${names[1]}`], ['TOKEN_SECRET_PROVIDER_CA_FILE', '/ca.crt'],
    ['TOKEN_SECRET_PROVIDER_TOKEN_FILE', '/token']]) {
    const c = remote(); c.services[names[0]].environment[key] = value;
    assert.throws(() => checkComposeKMS(c), /model_token_deployment_kms_/);
  }
});

test('native KMS accepts normalized omitted false but rejects unsafe or untyped bind options', () => {
  const normalized = fixture();
  for (const service of Object.values(normalized.services)) {
    for (const volume of service.volumes) delete volume.bind.create_host_path;
  }
  checkComposeKMS(normalized);
  for (const options of [undefined, null, [], Object.create(null), { create_host_path: true },
    { create_host_path: 'false' }, { create_host_path: null }, { create_host_path: 0 }, { create_host_path: undefined }]) {
    const c = fixture(); c.services[names[0]].volumes[0].bind = options;
    assert.throws(() => checkComposeKMS(c), /model_token_deployment_kms_mount_unsafe/);
  }
});

test('native overlay has only two consumers and no bootstrap or KMS service', () => {
  const c = YAML.parse(readFileSync(new URL('../../deploy/compose/compose.kms-native.yaml', import.meta.url), 'utf8'), { merge: true });
  assert.deepEqual(Object.keys(c.services), names);
  for (const service of Object.values(c.services)) {
    assert.equal(service.environment.TOKEN_SECRET_PROVIDER, 'vault-transit');
    assert.equal(service.environment.TOKEN_SECRET_PROVIDER_TOKEN, '');
    assert.equal(service.volumes.length, 2);
    assert.ok(service.volumes.every(v => v.read_only === true && v.bind.create_host_path === false));
  }
});

test('real Docker Compose merges native without static tokens and validates both provider configurations', {
  skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1',
}, () => {
  const root = fileURLToPath(new URL('../../../', import.meta.url));
  const base = 'account-platform/deploy/compose/';
  const standalone = process.env.PINVOU_TEST_KMS_COMPOSE_BINARY;
  const environment = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
    ['PATH', 'HOME', 'USERPROFILE', 'SYSTEMROOT', 'TEMP', 'TMP', 'DOCKER_CONFIG'].includes(key.toUpperCase())));
  const render = (native, overrides = {}) => spawnSync(standalone || 'docker', [...(standalone ? [] : ['compose']),
    '--env-file', `${base}.env.example`, '--env-file', `${base}model-token.env.example`,
    '--env-file', 'account-platform/test/fixtures/compose-render.env',
    ...(native ? ['--env-file', `${base}kms-native.env.example`] : []),
    '-f', `${base}compose.yaml`, '-f', `${base}compose.model-token.yaml`,
    ...(native ? ['-f', `${base}compose.kms-native.yaml`] : []),
    '--profile', 'model-token', '--profile', 'governance-operations', '--profile', 'real-newapi',
    'config', '--format', 'json'], { cwd: root, encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024,
    // Never inherit site variables or print config; this test only uses public fixtures.
    // The checker validates Linux mounts, including when Compose runs on Windows.
    env: { ...environment, COMPOSE_CONVERT_WINDOWS_PATHS: '1', ...overrides } });
  for (const native of [false, true]) {
    const rendered = render(native);
    assert.equal(rendered.error, undefined);
    assert.equal(rendered.status, 0, rendered.stderr);
    const c = JSON.parse(rendered.stdout);
    assert.equal(checkGovernedComposeConfiguration(c).deployment_status, 'NOT_DEPLOYED');
    for (const name of names) assert.equal(c.services[name].environment.TOKEN_SECRET_PROVIDER, native ? 'vault-transit' : 'remote');
    if (native) for (const name of names) assert.equal(c.services[name].environment.TOKEN_SECRET_PROVIDER_TOKEN, '');
    for (const key of ['INTERNAL_SERVICE_TOKEN', 'ACCOUNT_DB_PASSWORD',
      'NATS_PUBLIC_AUTHORIZATION_DIRECTORY', 'APPROVAL_RELAY_NATS_CURRENT_PUBLIC_KEYS_SHA256']) {
      const missing = render(native, { [key]: '' });
      assert.equal(missing.error, undefined);
      assert.notEqual(missing.status, 0, key);
      assert.match(missing.stderr, new RegExp(key), key);
    }
  }
  // Prove the renderer's representation instead of patching its output in the
  // test: explicit false omits the bool; true and short syntax must stay unsafe.
  const probe = spawnSync(standalone || 'docker', [...(standalone ? [] : ['compose']),
    '--project-name', 'public-bind-config-probe', '-f', '-', 'config', '--format', 'json'], {
    cwd: root, encoding: 'utf8', timeout: 30000,
    env: { ...environment, COMPOSE_CONVERT_WINDOWS_PATHS: '1' },
    input: YAML.stringify({ services: { probe: {
      image: 'registry.example.invalid/config-only:never-run',
      volumes: [
        { type: 'bind', source: '/opt/fixture-bind-false', target: '/false', read_only: true, bind: { create_host_path: false } },
        { type: 'bind', source: '/opt/fixture-bind-true', target: '/true', read_only: true, bind: { create_host_path: true } },
        '/opt/fixture-bind-short:/short:ro',
      ],
    } } }),
  });
  assert.equal(probe.error, undefined); assert.equal(probe.status, 0, probe.stderr);
  const volumes = JSON.parse(probe.stdout).services.probe.volumes;
  const find = target => volumes.find(volume => volume.target === target);
  assert.equal(isReadOnlyDirectoryBind(find('/false')), true);
  for (const target of ['/true', '/short']) {
    assert.equal(find(target).bind.create_host_path, true);
    assert.equal(isReadOnlyDirectoryBind(find(target)), false);
    const rejected = fixture(); rejected.services[names[0]].volumes[0].bind = find(target).bind;
    assert.throws(() => checkComposeKMS(rejected), /model_token_deployment_kms_mount_unsafe/);
  }
});
