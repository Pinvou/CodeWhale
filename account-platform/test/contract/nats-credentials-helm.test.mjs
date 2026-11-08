import assert from 'node:assert/strict';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import YAML from 'yaml';
import { checkHelmNATSCredentials } from '../../../scripts/account-platform-nats-helm-check.mjs';

const chart = path.resolve(import.meta.dirname, '../../deploy/helm/account-platform');
const enabled = { skip: process.env.PINVOU_TEST_KMS_HELM !== '1' };
function render(governed, native = false) {
  const args = governed ? ['-f', path.join(chart, 'values-model-token-test.yaml'),
    ...(native ? ['-f', path.join(chart, 'values-kms-native-test.yaml')] : [])] :
    ['-f', path.join(chart, 'values-staging.yaml'), '--set-string', 'dependencies.natsUrl=tls://nats.fixture.invalid:4222',
      '--set-string', 'newApi.tlsSecretName=fixture-only-newapi', '--set-string', 'newApi.caConfigMap=fixture-only-ca',
      '--set-string', 'images.newApiTLS.repository=registry.example.invalid/config-only',
      '--set-string', `images.newApiTLS.digest=sha256:${'0'.repeat(64)}`];
  const result = spawnSync(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', ['template', 'fixture', chart, ...args, '--set-json', 'workloadIdentity.kubernetesApiServerPorts=[443]', '--set-json', 'external.workloadJwksCidrs=["192.0.2.40/32"]'],
    { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024,
      env: Object.fromEntries(Object.entries(process.env).filter(([key]) =>
        ['PATH', 'HOME', 'USERPROFILE', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(key.toUpperCase()))) });
  assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
  return YAML.parseAllDocuments(result.stdout).map(doc => doc.toJSON()).filter(Boolean);
}
const get = (items, name) => items.find(item => item.kind === 'Deployment'
  && item.spec.template.metadata.labels['app.kubernetes.io/component'] === name).spec.template.spec;

test('real Helm wires private same-image non-root NATS preparation across all existing clients', enabled, () => {
  for (const [governed, native] of [[false, false], [true, false], [true, true]]) {
    const items = render(governed, native);
    assert.doesNotThrow(() => checkHelmNATSCredentials(items));
    const inits = items.flatMap(item => item.kind === 'Deployment'
      ? item.spec.template.spec.initContainers || [] : []).filter(init => init.args?.[0] === 'prepare-nats-credentials');
    const clients = ['billing-command-executor', 'account-event-relay', 'account-approval-relay', 'billing-event-relay',
      ...(governed ? ['account-service', 'token-gateway', 'token-command-executor'] : [])];
    assert.deepEqual(inits.map(init => init.args[1]).sort(), clients.sort());
    assert.equal(get(items, 'account-event-relay').initContainers.length, 2);
    assert.equal(get(items, 'billing-event-relay').initContainers.length, 1);
    assert.equal(items.filter(item => item.kind === 'Job' && item.metadata.annotations?.['helm.sh/hook']).length, 3);
  }
});

test('NATS rendered boundary rejects credential leaks, widened init and unsafe memory/identity changes', enabled, () => {
  const original = render(true, true);
  for (const [name, mutate] of [
    ['raw Secret reaches application', pod => pod.containers[0].volumeMounts.push({ ...pod.initContainers[0].volumeMounts[0] })],
    ['raw Secret alias reaches application', pod => {
      const source = pod.volumes.find(v => v.name.startsWith('nats-source-'));
      pod.volumes.push({ name: 'stolen', secret: { secretName: source.secret.secretName } });
      pod.containers[0].volumeMounts.push({ name: 'stolen', mountPath: '/stolen', readOnly: true });
    }],
    ['projected Secret alias reaches application', pod => {
      const source = pod.volumes.find(v => v.name.startsWith('nats-source-'));
      pod.volumes.push({ name: 'stolen', projected: { sources: [{ secret: { name: source.secret.secretName } }] } });
      pod.containers[0].volumeMounts.push({ name: 'stolen', mountPath: '/stolen', readOnly: true });
    }],
    ['main may write snapshot', pod => { pod.containers[0].volumeMounts.find(v => v.name === 'nats-private').readOnly = false; }],
    ['disk output', pod => { delete pod.volumes.find(v => v.name === 'nats-private').emptyDir.medium; }],
    ['root init', pod => { pod.initContainers[0].securityContext.runAsUser = 0; }],
    ['main overrides pod identity', pod => { pod.containers[0].securityContext.runAsUser = 0; }],
    ['wrong fsGroup', pod => { pod.securityContext.fsGroup = 0; }],
    ['unreviewed init image', pod => { pod.initContainers[0].image = 'other:latest'; }],
    ['arbitrary source path argument', pod => { pod.initContainers[0].args.push('/other-secret'); }],
    ['whole Secret projected to init', pod => { delete pod.volumes.find(v => v.name.startsWith('nats-source-')).secret.items; }],
    ['extra Secret key', pod => { pod.volumes.find(v => v.name.startsWith('nats-source-')).secret.items.push({ key: 'database-url', path: 'database-url' }); }],
    ['init may read workload token', pod => { pod.initContainers[0].volumeMounts.push({ name: 'workload-identity', mountPath: '/stolen', readOnly: true }); }],
    ['runtime path bypasses prepared file', pod => { pod.containers[0].env.find(v => v.name === 'NATS_CREDENTIALS_FILE').value = '/var/run/bootstrap/pinvou/nats/token-gateway.creds'; }],
    ['governed producer loses both NATS settings', pod => { pod.containers[0].env = pod.containers[0].env.filter(v => !['NATS_URL', 'NATS_CREDENTIALS_FILE'].includes(v.name)); }],
    ['missing init', pod => { pod.initContainers = []; }],
    ['extra init', pod => { pod.initContainers.push({ name: 'unapproved', image: 'other:latest' }); }],
    ['KMS sidecar reads prepared identity', pod => { pod.containers[1].volumeMounts.push({ name: 'nats-private', mountPath: '/run/pinvou/nats', readOnly: true }); }],
  ]) {
    const items = structuredClone(original);
    mutate(get(items, 'token-gateway'));
    assert.throws(() => checkHelmNATSCredentials(items), /model_token_deployment_nats_/, name);
  }
  const missingApproval = structuredClone(original);
  get(missingApproval, 'account-event-relay').initContainers.pop();
  assert.throws(() => checkHelmNATSCredentials(missingApproval), /model_token_deployment_nats_init_scope/);
});
