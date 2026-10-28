import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';
import Ajv from 'ajv';
import addFormats from 'ajv-formats';

const chart = path.resolve(import.meta.dirname, '../../deploy/helm/account-platform');
const apiURL = 'https://kubernetes.default.svc/openid/v1/jwks';
const receivers = ['account-service', 'identity-service', 'billing-service', 'token-gateway'];
const cidrs = ['192.0.2.40/32', '2001:db8::40/128'];
const ports = [443, 6443];
const publicEnvironment = () => Object.fromEntries(Object.entries(process.env).filter(([key]) =>
  ['PATH', 'HOME', 'USERPROFILE', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(key.toUpperCase())));
const enabled = { skip: process.env.PINVOU_TEST_KMS_HELM !== '1' };
function render(profile = 'base', changes = []) {
  const args = ['template', 'kube-contract', chart, '--namespace', 'kube-test',
    ...(profile === 'base' ? [] : ['-f', path.join(chart, 'values-' + profile + '.yaml')]),
    ...(profile === 'production-new' ? ['--set-string', 'newApi.tlsSecretName=fixture-newapi', '--set-string', 'newApi.caConfigMap=fixture-newapi-ca', '--set-string', 'images.newApiTLS.repository=registry.example.invalid/fixture', '--set-string', 'images.newApiTLS.digest=sha256:' + '0'.repeat(64)] : ['--set', 'newApi.enabled=false']), '--set', 'nats.enabled=false', '--set-string', 'dependencies.natsUrl=tls://nats.fixture.invalid:4222',
    '--set-json', 'external.workloadJwksCidrs=' + JSON.stringify(cidrs),
    '--set-json', 'workloadIdentity.kubernetesApiServerPorts=' + JSON.stringify(ports),
    ...(profile === 'production-new' ? ['--set-json', 'global.manualReviewOnCallUserIds=["00000000-0000-4000-8000-000000000707"]',
      '--set-string', 'keycloak.tlsSecretName=fixture-keycloak', '--set-string', 'keycloak.caConfigMap=fixture-keycloak-ca'] : []),
    ...changes];
  return spawnSync(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', args,
    { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024, env: publicEnvironment() });
}
function resources(result) {
  assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
  return YAML.parseAllDocuments(result.stdout).map(document => { assert.deepEqual(document.errors, []); return document.toJSON(); }).filter(Boolean);
}
const environment = container => Object.fromEntries(container.env.filter(item => Object.hasOwn(item, 'value')).map(item => [item.name, item.value]));
function assertNative(items, expectedPorts = ports) {
  const deployments = items.filter(item => item.kind === 'Deployment');
  for (const component of receivers) {
    const deployment = deployments.find(item => item.spec.template.metadata.labels['app.kubernetes.io/component'] === component);
    const pod = deployment.spec.template.spec, main = pod.containers[0], env = environment(main);
    assert.equal(env.WORKLOAD_IDENTITY_JWKS_URL, apiURL);
    assert.equal(env.WORKLOAD_IDENTITY_JWKS_AUTH_MODE, 'kubernetes');
    assert.equal(env.WORKLOAD_IDENTITY_JWKS_API_SUBJECT, 'system:serviceaccount:kube-test:' + pod.serviceAccountName);
    assert.notEqual(env.WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE, env.WORKLOAD_IDENTITY_TOKEN_FILE);
    const ca = main.volumeMounts.find(mount => mount.mountPath === env.WORKLOAD_IDENTITY_CA_FILE);
    assert.equal(ca.readOnly, true); assert.equal(ca.subPath, 'ca.crt');
    assert.equal(pod.volumes.find(volume => volume.name === ca.name).configMap.name, 'kube-root-ca.crt');
    const mount = main.volumeMounts.find(item => env.WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE.startsWith(item.mountPath + '/'));
    assert.equal(mount.readOnly, true); assert.equal(mount.subPath, undefined);
    const projection = pod.volumes.find(volume => volume.name === mount.name).projected;
    assert.equal(projection.defaultMode, 0o440);
    assert.deepEqual(projection.sources, [{ serviceAccountToken: { path: 'token', expirationSeconds: 600 } }]);
    const business = pod.volumes.find(volume => volume.name === 'workload-identity').projected.sources[0].serviceAccountToken;
    assert.equal(business.audience, env.WORKLOAD_IDENTITY_AUDIENCE);
    assert.equal(pod.automountServiceAccountToken, false);
    assert.equal(pod.securityContext.fsGroup, 65532); assert.equal(pod.securityContext.runAsUser, 65532);
  }
  for (const deployment of deployments.filter(item => !receivers.includes(item.spec.template.metadata.labels['app.kubernetes.io/component']))) {
    assert.ok(deployment.spec.template.spec.containers.every(container => !environment(container).WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE));
    assert.ok(!deployment.spec.template.spec.volumes?.some(volume => volume.name.startsWith("kubernetes-jwks")));
  }
  const role = items.find(item => item.kind === 'ClusterRole');
  assert.deepEqual(role.rules, [{ nonResourceURLs: ['/openid/v1/jwks'], verbs: ['get'] }]);
  const binding = items.find(item => item.kind === 'ClusterRoleBinding');
  assert.equal(binding.roleRef.name, role.metadata.name);
  assert.deepEqual(binding.subjects, receivers.map(component => ({ kind: 'ServiceAccount', name: 'kube-contract-account-platform-' + component, namespace: 'kube-test' })));
  const policy = items.find(item => item.kind === 'NetworkPolicy' && item.metadata.name.endsWith('-workload-verifiers'));
  assert.deepEqual([...policy.spec.podSelector.matchExpressions[0].values].sort(), [...receivers].sort());
  assert.deepEqual(policy.spec.egress, cidrs.map(cidr => ({ to: [{ ipBlock: { cidr } }], ports: expectedPorts.map(port => ({ protocol: 'TCP', port })) })));
}

test('Kubernetes JWKS API port inputs are typed without changing external workload audience', () => {
  const ajv = new Ajv({ strict: true }); addFormats(ajv);
  const validate = ajv.compile(JSON.parse(readFileSync(path.join(chart, 'values.schema.json'))));
  const defaults = YAML.parse(readFileSync(path.join(chart, 'values.yaml'), 'utf8'));
  assert.equal(validate(defaults), true);
  for (const invalid of [[0], [65536], ['443'], [443, 443]]) {
    const values = structuredClone(defaults); values.workloadIdentity.kubernetesApiServerPorts = invalid;
    assert.equal(validate(values), false);
  }
  const external = structuredClone(defaults); external.workloadIdentity.audience = 'urn:cluster:business';
  assert.equal(validate(external), true);
});
test('real base staging and production native receivers get CA separate API token exact RBAC and explicit egress', enabled, async t => {
  for (const profile of ['base', 'staging', 'production-new']) await t.test(profile, () => {
    assertNative(resources(render(profile, ['--set-string', 'workloadIdentity.jwksUrl=' + apiURL,
      '--set-string', 'workloadIdentity.issuer=https://actual-cluster-issuer.fixture.invalid'])));
  });
  const altered = resources(render());
  const identity = altered.find(item => item.kind === 'Deployment' && item.spec.template.metadata.labels['app.kubernetes.io/component'] === 'identity-service');
  identity.spec.template.spec.containers[0].env = identity.spec.template.spec.containers[0].env.filter(item => item.name !== 'WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE');
  assert.throws(() => assertNative(altered));
  assertNative(resources(render('base', ['--set-json', 'workloadIdentity.kubernetesApiServerPorts=[7443]'])), [7443]);
});
test('real Helm native mode rejects missing egress rather than guessing cluster addresses or ports', enabled, () => {
  for (const change of [['--set-json', 'external.workloadJwksCidrs=[]'], ['--set-json', 'workloadIdentity.kubernetesApiServerPorts=[]'],
    ['--set-json', 'external.workloadJwksCidrs=["0.0.0.0/0"]'], ['--set-json', 'external.workloadJwksCidrs=["::/0"]'], ['--set-json', 'external.workloadJwksCidrs=["0.0.0.0/00"]'], ['--set-json', 'external.workloadJwksCidrs=["0.0.0.0/+0"]'],
    ...['999.0.2.1/32', '2001:::1/128', '2001:db8::1::2/128', '2001:db8:1/64', '12345::1/64', '2001:db8::1/129', 'invalid/24'].map(cidr => ['--set-json', 'external.workloadJwksCidrs=' + JSON.stringify([cidr])]),
    ['--set-json', 'workloadIdentity.kubernetesApiServerPorts=[0]']]) {
    const result = render('base', change); assert.equal(result.error, undefined); assert.notEqual(result.status, 0);
    assert.equal(result.stdout, '');
  }
});
test('real native CIDR validation retains IPv4 IPv6 and mapped IPv4 forms', enabled, () => {
  for (const cidr of ['192.0.2.0/24', '2001:0DB8:0000:0000:0000:0000:0000:0040/64', '::ffff:192.0.2.40/128', '1:2:3:4:5:6:7:8/128']) resources(render('base', ['--set-json', 'external.workloadJwksCidrs=' + JSON.stringify([cidr])]));
});
test('real external JWKS mode does not receive API credentials CA or Kubernetes RBAC', enabled, () => {
  const items = resources(render('production-new', ['--set-json', 'workloadIdentity.kubernetesApiServerPorts=[]']));
  assert.equal(items.some(item => ['ClusterRole', 'ClusterRoleBinding'].includes(item.kind)), false);
  for (const item of items.filter(item => item.kind === 'Deployment')) {
    assert.ok(!item.spec.template.spec.volumes?.some(volume => volume.name.startsWith('kubernetes-jwks')));
    for (const container of item.spec.template.spec.containers) {
      const env = environment(container);
      assert.equal(env.WORKLOAD_IDENTITY_JWKS_AUTH_MODE, undefined);
      assert.equal(env.WORKLOAD_IDENTITY_JWKS_API_TOKEN_FILE, undefined);
      assert.equal(env.WORKLOAD_IDENTITY_CA_FILE, undefined);
    }
  }
});
