import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const chart = path.join(root, 'deploy/helm/account-platform');
const production = path.join(chart, 'values-production-new.yaml');
const executable = process.env.PINVOU_HELM_EXECUTABLE || 'helm';

function helm(args) {
  const result = spawnSync(executable, args, { encoding: 'utf8', shell: false });
  assert.equal(result.error, undefined, 'the real Helm executable must be available');
  return result;
}

function render(overrides = []) {
  return helm(['template', 'fixture', chart, '-f', production, ...overrides]);
}

test('actual Helm lint accepts default and production TLS configurations', async (t) => {
  for (const [name, args] of [['default', []], ['production-new', ['-f', production]]]) {
    await t.test(name, () => assert.equal(helm(['lint', chart, ...args]).status, 0, 'Helm lint failed'));
  }
});

test('actual production render confines TLS, CA, routes and network access', () => {
  const result = render();
  assert.equal(result.status, 0, 'production Helm template failed');
  const objects = YAML.parseAllDocuments(result.stdout).map(document => document.toJSON()).filter(Boolean);
  const object = (kind, name) => objects.find(item => item.kind === kind && item.metadata.name === name);
  const name = 'fixture-account-platform';
  const api = object('Deployment', `${name}-new-api`);
  const gateway = object('Deployment', `${name}-token-gateway`);
  const front = api.spec.template.spec.containers.find(item => item.name === 'newapi-tls');
  const upstream = api.spec.template.spec.containers.find(item => item.name === 'new-api');
  assert.deepEqual(front.ports, [{ name: 'https', containerPort: 8443 }]);
  assert.equal(front.readinessProbe.httpGet.scheme, 'HTTPS');
  assert.equal(front.livenessProbe.httpGet.scheme, 'HTTPS');
  assert.deepEqual(front.volumeMounts, [{ name: 'newapi-server-tls', mountPath: '/etc/newapi-tls', readOnly: true }]);
  assert.equal(upstream.volumeMounts.some(item => item.name === 'newapi-server-tls'), false);
  const serverTLS = api.spec.template.spec.volumes.find(item => item.name === 'newapi-server-tls').secret;
  assert.equal(serverTLS.secretName, 'pinvou-account-platform-production-new-newapi-tls');
  assert.equal(serverTLS.defaultMode, 288);
  assert.deepEqual(serverTLS.items.map(item => item.key).sort(), ['tls.crt', 'tls.key']);
  assert.notEqual(serverTLS.optional, true);
  const gatewayContainer = gateway.spec.template.spec.containers[0];
  const environment = Object.fromEntries(gatewayContainer.env.filter(item => item.value !== undefined).map(item => [item.name, item.value]));
  assert.equal(environment.SSL_CERT_DIR, '/etc/ssl/certs:/etc/newapi-ca');
  assert.equal(environment.TOKEN_DEFAULT_PROVIDER_BASE_URL, 'https://newapi:8443');
  assert.equal(environment.TOKEN_RECONCILIATION_NEWAPI_BASE_URL, 'https://newapi:8443');
  const ca = gateway.spec.template.spec.volumes.find(item => item.name === 'newapi-ca').secret;
  assert.equal(ca.secretName, 'pinvou-account-platform-production-new-newapi-ca');
  assert.deepEqual(ca.items, [{ key: 'ca.crt', path: 'ca.crt' }]);
  assert.notEqual(ca.optional, true);
  assert.equal(gateway.spec.template.spec.volumes.some(item => item.secret?.secretName === serverTLS.secretName), false);
  assert.deepEqual(object('Service', 'newapi').spec.ports, [{ name: 'https', port: 8443, targetPort: 'https' }]);
  const policy = object('NetworkPolicy', `${name}-newapi-data-plane`);
  assert.equal(policy.spec.ingress.length, 1);
  assert.deepEqual(policy.spec.ingress[0].ports, [{ protocol: 'TCP', port: 8443 }]);
  assert.equal(policy.spec.ingress[0].from[0].podSelector.matchLabels['app.kubernetes.io/component'], 'token-gateway');
  assert.equal(policy.spec.ingress[0].from[0].podSelector.matchLabels['app.kubernetes.io/instance'], 'fixture');
  const publicBackends = objects.filter(item => item.kind === 'Ingress').flatMap(item => item.spec.rules.flatMap(rule => rule.http.paths.map(item => item.backend.service.name)));
  assert.equal(publicBackends.includes('newapi'), false, 'New API must not gain an ingress');
});

test('actual production Helm render rejects missing TLS references and incompatible URLs', async (t) => {
  for (const [name, override] of [
    ['HTTP-provider', 'tokenGateway.defaultProviderBaseUrl=http://newapi:8080'],
    ['HTTP-reconciliation', 'tokenGateway.reconciliationNewApiBaseUrl=http://newapi:8080'],
    ['different-provider-host', 'tokenGateway.defaultProviderBaseUrl=https://other:8443'],
    ['missing-server-secret', 'newApi.tls.serverSecretName='],
    ['missing-CA-secret', 'newApi.tls.caSecretName='],
  ]) {
    await t.test(name, () => assert.notEqual(render(['--set-string', override]).status, 0, 'unsafe configuration rendered successfully'));
  }
});
