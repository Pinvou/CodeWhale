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

const mobileSupplierValues = [
  'mobileAuth.enabled=true', 'mobileAuth.smsProvider=http',
  'mobileAuth.smsEndpoint=https://sms.example.test/adapter',
  'mobileAuth.keycloakAdminApiUrl=https://identity.example.test',
  'mobileAuth.keycloakProvisionerTokenUrl=https://identity.example.test/realms/pinvou/protocol/openid-connect/token',
  'images.keycloak.repository=pinvou-account-platform-keycloak-mobile',
  'external.smsProviderCidrs[0]=203.0.113.1/32',
  'commerce.enabled=true', 'commerce.baseUrl=https://commerce.example.test',
  'external.commerceProviderCidrs[0]=203.0.113.2/32', 'billing.paymentProvider=h3c_mall',
];
const setStrings = (values) => values.flatMap(value => [/=(true|false)$/.test(value) ? '--set' : '--set-string', value]);

test('actual mobile and commerce render preserves component-scoped supplier access and secret references', () => {
  const result = render(setStrings(mobileSupplierValues));
  assert.equal(result.status, 0, 'mobile supplier configuration did not render');
  const objects = YAML.parseAllDocuments(result.stdout).map(document => document.toJSON()).filter(Boolean);
  const name = 'fixture-account-platform';
  const object = (kind, suffix) => objects.find(item => item.kind === kind && item.metadata.name === `${name}-${suffix}`);
  const environment = (component) => Object.fromEntries(object('Deployment', component).spec.template.spec.containers[0].env.map(item => [item.name, item]));
  const account = environment('account-service');
  const billing = environment('billing-service');
  const keycloak = environment('keycloak');
  assert.equal(account.MOBILE_AUTH_ENABLED.value, 'true');
  assert.equal(account.SMS_HTTP_ENDPOINT.value, 'https://sms.example.test/adapter');
  assert.equal(account.SMS_HTTP_SERVICE_TOKEN.valueFrom.secretKeyRef.key, 'sms-http-service-token');
  assert.equal(account.KEYCLOAK_PROVISIONER_CLIENT_SECRET.valueFrom.secretKeyRef.key, 'keycloak-provisioner-client-secret');
  assert.equal(billing.BILLING_PAYMENT_PROVIDER.value, 'h3c_mall');
  assert.equal(billing.BILLING_ALLOW_TENTATIVE_PRICES.value, 'false');
  assert.equal(keycloak.PINVOU_SMS_VERIFY_URL.value, `http://${name}-account-service:8080/internal/v1/auth/sms/verify`);
  assert.equal(keycloak.PINVOU_MOBILE_IDENTITY_URL.value, `http://${name}-account-service:8080/internal/v1/auth/mobile/identity`);
  assert.equal(keycloak.PINVOU_INTERNAL_SERVICE_TOKEN.valueFrom.secretKeyRef.key, 'internal-service-token');
  for (const [supplier, components, cidr] of [['sms', ['account-service'], '203.0.113.1/32'], ['commerce', ['account-service', 'billing-service'], '203.0.113.2/32']]) {
    const policy = object('NetworkPolicy', `${supplier}-egress`);
    assert.deepEqual(policy.spec.podSelector.matchExpressions, [{ key: 'app.kubernetes.io/component', operator: 'In', values: components }]);
    assert.deepEqual(policy.spec.egress, [{ to: [{ ipBlock: { cidr } }], ports: [{ protocol: 'TCP', port: 443 }] }]);
  }
  const general = object('NetworkPolicy', 'internal-and-approved-egress');
  assert.equal(JSON.stringify(general).includes('203.0.113.1/32'), false);
  assert.equal(JSON.stringify(general).includes('203.0.113.2/32'), false);
  assert.equal(environment('token-portal').SMS_HTTP_SERVICE_TOKEN, undefined);
  assert.equal(environment('token-portal').H3CMALL_APP_SECRET, undefined);
});

test('actual production supplier configuration fails closed on incomplete or insecure wiring', async (t) => {
  for (const [name, override] of [
    ['HTTP-SMS', 'mobileAuth.smsEndpoint=http://sms.example.test'],
    ['HTTP-provisioner-admin', 'mobileAuth.keycloakAdminApiUrl=http://identity.example.test'],
    ['HTTP-provisioner-token', 'mobileAuth.keycloakProvisionerTokenUrl=http://identity.example.test/token'],
    ['official-image-without-SPI', 'images.keycloak.repository=quay.io/keycloak/keycloak'],
    ['missing-SMS-CIDR', 'external.smsProviderCidrs=[]'],
    ['HTTP-commerce', 'commerce.baseUrl=http://commerce.example.test'],
    ['missing-commerce-CIDR', 'external.commerceProviderCidrs=[]'],
    ['H3C-without-commerce', 'commerce.enabled=false'],
  ]) {
    await t.test(name, () => {
      const index = mobileSupplierValues.findIndex(value => value.split('=')[0] === override.split('=')[0]);
      const values = mobileSupplierValues.filter((_, position) => position !== index);
      const base = override.endsWith('=[]') ? values.filter(value => !value.startsWith(override.split('=')[0] + '[')) : values;
      assert.notEqual(render([...setStrings(base), ...(override.endsWith('=[]') ? ['--set-json', override] : setStrings([override]))]).status, 0, 'unsafe supplier settings rendered');
    });
  }
});

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

  const billing = object('Deployment', `${name}-billing-service`).spec.template.spec.containers[0];
  assert.equal(billing.env.find(item => item.name === 'TOKEN_GATEWAY_HTTP_URL').value, `http://${name}-token-gateway:8080`);
  assert.ok(billing.env.some(item => item.name === 'INTERNAL_SERVICE_TOKEN' && item.valueFrom.secretKeyRef.key === 'internal-service-token'));
  assert.ok(gatewayContainer.env.some(item => item.name === 'INTERNAL_SERVICE_TOKEN' && item.valueFrom.secretKeyRef.key === 'internal-service-token'));
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
