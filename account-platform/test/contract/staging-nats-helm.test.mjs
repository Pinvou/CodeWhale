import assert from 'node:assert/strict';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import YAML from 'yaml';

const chart = path.resolve(import.meta.dirname, '../../deploy/helm/account-platform');
const enabled = { skip: process.env.PINVOU_TEST_KMS_HELM !== '1' };
const publicEnvironment = () => Object.fromEntries(Object.entries(process.env).filter(([key]) =>
  ['PATH', 'HOME', 'USERPROFILE', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(key.toUpperCase())));

function render(overrides = [], staging = true) {
  const inputs = ['newApi.tlsSecretName=fixture-only-newapi', 'newApi.caConfigMap=fixture-only-newapi-ca',
    'images.newApiTLS.repository=registry.example.invalid/config-only',
    `images.newApiTLS.digest=sha256:${'0'.repeat(64)}`];
  return spawnSync(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm',
    ['template', 'fixture', chart, ...(staging ? ['-f', path.join(chart, 'values-staging.yaml')] : []),
      ...inputs.flatMap(value => ['--set-string', value]), '--set-json', 'workloadIdentity.kubernetesApiServerPorts=[443]', '--set-json', 'external.workloadJwksCidrs=["192.0.2.40/32"]', ...overrides],
    { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024, env: publicEnvironment() });
}

test('base and staging Helm reject unusable built-in NATS with governed workloads off', enabled, async t => {
  for (const staging of [false, true]) await t.test(staging ? 'staging' : 'base', () => {
    const result = render([], staging);
    assert.equal(result.error, undefined);
    assert.notEqual(result.status, 0, 'must reject plaintext built-in NATS before deployment');
    assert.match(result.stderr, /Billing and event relays require authenticated external TLS NATS/);
  });
});

test('staging Helm requires a runtime-compatible external NATS URL', enabled, async t => {
  for (const url of ['', 'nats://nats.example.invalid:4222', 'tls://', 'tls://:4222', 'tls://[]:4222',
    'tls://user:password@nats.example.invalid:4222', 'tls://nats.example.invalid:4222/path',
    'tls://@nats.example.invalid:4222', 'tls://:@nats.example.invalid:4222',
    'tls://nats.example.invalid:4222?token=fixture', 'tls://nats.example.invalid:4222#fragment',
    'tls://one.example.invalid:4222,tls://two.example.invalid:4222']) {
    await t.test(url || 'missing', () => {
      const result = render(['--set', 'nats.enabled=false', '--set-literal', `dependencies.natsUrl=${url}`]);
      assert.equal(result.error, undefined);
      assert.notEqual(result.status, 0);
      assert.match(result.stderr, /Billing and event relays require authenticated external TLS NATS/);
    });
  }
});

test('valid external staging NATS reaches Billing and both relays without an internal server', enabled, () => {
  for (const url of ['tls://nats.fixture.invalid:4222', 'tls://nats.fixture.invalid', 'tls://[2001:db8::7]:4222']) {
    const result = render(['--set', 'nats.enabled=false', '--set-string', `dependencies.natsUrl=${url}`]);
    assert.equal(result.error, undefined);
    assert.equal(result.status, 0, result.stderr);
    const items = YAML.parseAllDocuments(result.stdout).map(document => document.toJSON()).filter(Boolean);
    assert.equal(items.some(item => item.kind === 'StatefulSet' && item.metadata.name.endsWith('-nats')), false);
    const consumers = items.filter(item => item.kind === 'Deployment'
      && ['billing-service', 'account-event-relay', 'billing-event-relay']
        .includes(item.spec.template.metadata.labels['app.kubernetes.io/component']));
    assert.equal(consumers.length, 3);
    for (const consumer of consumers) {
      const container = consumer.spec.template.spec.containers[0];
      const env = Object.fromEntries(container.env.map(item => [item.name, item.value]));
      assert.equal(env.NATS_URL, url);
      assert.ok(env.NATS_CREDENTIALS_FILE?.startsWith('/run/pinvou/nats/private/'));
      assert.ok(consumer.spec.template.spec.volumes.some(volume => volume.secret));
    }
    for (const name of ['token-command-executor', 'token-credential-portal']) {
      assert.equal(items.some(item => item.kind === 'Deployment'
        && item.spec.template.metadata.labels['app.kubernetes.io/component'] === name), false);
    }
  }
});
