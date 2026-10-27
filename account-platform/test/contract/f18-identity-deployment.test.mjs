import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import YAML from 'yaml';
import { matchesPodSelector } from '../../../scripts/account-platform-model-token-deployment-check.mjs';

const root = path.resolve(import.meta.dirname, '../../..');
const chart = path.join(root, 'account-platform/deploy/helm/account-platform');
const publicEnvironment = () => Object.fromEntries(Object.entries(process.env).filter(([key]) =>
  ['PATH', 'HOME', 'USERPROFILE', 'SYSTEMROOT', 'TEMP', 'TMP', 'DOCKER_CONFIG'].includes(key.toUpperCase())));
const newApi = ['newApi.tlsSecretName=fixture-only-newapi', 'newApi.caConfigMap=fixture-only-newapi-ca',
  'images.newApiTLS.repository=registry.example.invalid/config-only', `images.newApiTLS.digest=sha256:${'0'.repeat(64)}`];
function render(overrides = [], production = true) {
  const args = ['template', 'fixture', chart,
    ...(production ? ['-f', path.join(chart, 'values-production-new.yaml')] : []),
    // The base profile also uses strict provisioned NATS consumers. Keep this
    // Identity fixture valid without reviving the unauthenticated legacy server.
    ...(!production ? ['--set', 'nats.enabled=false', '--set-string',
      'dependencies.natsUrl=tls://nats.example.invalid:4222'] : []),
    // Config-only fixture member; production values require real deployed on-call users.
    ...(production ? ['--set-json', 'global.manualReviewOnCallUserIds=["00000000-0000-4000-8000-000000000707"]'] : []),
    ...[...newApi, ...overrides].flatMap(value =>
      [/^(keycloak|sms)\.enabled=(true|false)$/.test(value) ? '--set' : '--set-string', value])];
  return spawnSync(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', args,
    { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024, env: publicEnvironment() });
}
function documents(result) {
  assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
  return YAML.parseAllDocuments(result.stdout).map(d => d.toJSON()).filter(Boolean);
}
const deployment = (items, component) => items.find(item => item.kind === 'Deployment'
  && item.spec.template.metadata.labels['app.kubernetes.io/component'] === component);
const environment = container => Object.fromEntries(container.env.map(item => [item.name, item.value]));

test('remote overlay freezes the same public issuer for Portal and token verifiers', () => {
  const overlay = YAML.parse(readFileSync(path.join(root, 'account-platform/deploy/compose/compose.remote.yaml'), 'utf8'));
  const issuer = overlay.services['account-service'].environment.OIDC_ISSUER;
  assert.equal(overlay.services['token-portal'].environment.PORTAL_OIDC_ISSUER, issuer);
  assert.equal(overlay.services['billing-service'].environment.OIDC_ISSUER, issuer);
  assert.equal(overlay.services['token-gateway'].environment.OIDC_ISSUER, issuer);
});

test('production Identity chart satisfies its strict Keycloak input and network contracts', {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1',
}, async t => {
  const trust = ['keycloak.tlsSecretName=fixture-only-keycloak', 'keycloak.caConfigMap=fixture-only-keycloak-ca'];
  for (const external of [false, true]) await t.test(external ? 'external_direct_https' : 'bundled_direct_https', () => {
    const direct = external ? ['keycloak.enabled=false', 'keycloak.tlsSecretName=',
      'keycloak.adminApiUrl=https://keycloak-backchannel.example.invalid',
      'keycloak.provisionerTokenUrl=https://keycloak-backchannel.example.invalid/realms/pinvou/protocol/openid-connect/token',
      'oidc.jwksUrl=https://issuer.example.invalid/certs', 'oidc.tokenUrl=https://issuer.example.invalid/token'] : [];
    const items = documents(render([...trust, ...direct, 'external.workloadJwksCidrs[0]=203.0.113.128/25']));
    const identity = deployment(items, 'identity-service');
    const pod = identity.spec.template.spec, main = pod.containers[0], env = environment(main);
    assert.equal(env.APP_ENV, 'production-new'); assert.equal(env.MOBILE_AUTH_ENABLED, 'true');
    assert.equal(main.env.some(item => item.name === 'KEYCLOAK_PROVISIONER_CLIENT_SECRET'), false);
    const admin = new URL(env.KEYCLOAK_ADMIN_API_URL), token = new URL(env.KEYCLOAK_PROVISIONER_TOKEN_URL);
    assert.equal(admin.protocol, 'https:'); assert.equal(admin.origin, token.origin);
    assert.equal(token.pathname, '/realms/pinvou/protocol/openid-connect/token');
    assert.notEqual(token.origin, 'https://accounts.example.com');
    assert.equal(pod.securityContext.runAsUser, 65532); assert.equal(pod.securityContext.fsGroup, 65532);
    const init = pod.initContainers.find(item => item.name === 'prepare-keycloak-secret');
    assert.equal(init.image, main.image); assert.deepEqual(init.command, ['/service']);
    assert.equal(init.args[0], 'prepare-keycloak-secret'); assert.equal(init.args[2], env.KEYCLOAK_PROVISIONER_CLIENT_SECRET_FILE);
    assert.equal(init.securityContext.runAsUser, 65532); assert.equal(init.securityContext.readOnlyRootFilesystem, true);
    const privateMount = main.volumeMounts.find(item => env.KEYCLOAK_PROVISIONER_CLIENT_SECRET_FILE.startsWith(`${item.mountPath}/`));
    assert.equal(privateMount.readOnly, true);
    assert.equal(pod.volumes.find(item => item.name === privateMount.name).emptyDir.medium, 'Memory');
    assert.equal(main.volumeMounts.some(item => item.name === 'keycloak-provisioner-source'), false);
    const source = pod.volumes.find(item => item.name === 'keycloak-provisioner-source').secret;
    assert.equal(source.defaultMode, 0o440); assert.deepEqual(source.items, [{ key: 'keycloak-provisioner-client-secret', path: 'client-secret' }]);
    assert.equal(init.volumeMounts.find(item => item.name === 'keycloak-provisioner-source').readOnly, true);
    const ca = main.volumeMounts.find(item => item.mountPath === env.KEYCLOAK_CA_FILE);
    assert.equal(ca.subPath, 'ca.crt'); assert.equal(ca.readOnly, true);
    assert.equal(pod.volumes.find(item => item.name === ca.name).configMap.name, 'fixture-only-keycloak-ca');
    if (!external) {
      const keycloak = deployment(items, 'keycloak').spec.template.spec;
      const server = keycloak.containers[0], serverEnv = environment(server);
      assert.equal(serverEnv.KC_HTTP_MANAGEMENT_SCHEME, 'http');
      assert.equal(server.readinessProbe.httpGet.scheme, 'HTTP');
      assert.ok(server.ports.some(item => item.name === 'https' && item.containerPort === 8443));
      assert.equal(keycloak.securityContext.fsGroup, 1000);
      assert.equal(keycloak.securityContext.runAsUser, 1000);
      assert.equal(keycloak.volumes.find(item => item.name === 'keycloak-tls').secret.secretName, 'fixture-only-keycloak');
      const service = items.find(item => item.kind === 'Service' && item.metadata.name.endsWith('-keycloak'));
      assert.ok(service.spec.ports.some(item => item.port === 8443 && item.targetPort === 'https'));
      assert.ok(service.spec.ports.some(item => item.port === 8080));
    } else assert.equal(deployment(items, 'keycloak'), undefined);
    const jwks = items.find(item => item.kind === 'NetworkPolicy' && item.metadata.name.endsWith('-workload-verifiers'));
    assert.equal(matchesPodSelector(identity.spec.template.metadata.labels, jwks.spec.podSelector), true);
    const otherRelease = { ...identity.spec.template.metadata.labels, 'app.kubernetes.io/instance': 'another-release' };
    assert.equal(matchesPodSelector(otherRelease, jwks.spec.podSelector), false);
    assert.deepEqual(jwks.spec.egress, [{ to: [{ ipBlock: { cidr: '203.0.113.128/25' } }], ports: [{ protocol: 'TCP', port: 443 }] }]);
  });
  await t.test('missing_or_invalid_production_trust_is_rejected', () => {
    for (const changes of [[], ['keycloak.caConfigMap=fixture-only-ca'], ['keycloak.tlsSecretName=fixture-only-tls'],
      [...trust, 'keycloak.enabled=false', 'keycloak.adminApiUrl=http://unsafe.invalid',
        'keycloak.provisionerTokenUrl=http://unsafe.invalid/realms/pinvou/protocol/openid-connect/token'],
      [...trust, 'keycloak.enabled=false', 'keycloak.adminApiUrl=https://one.invalid',
        'keycloak.provisionerTokenUrl=https://two.invalid/realms/pinvou/protocol/openid-connect/token'],
      [...trust, 'keycloak.enabled=false', 'keycloak.adminApiUrl=https://accounts.example.com',
        'keycloak.provisionerTokenUrl=https://accounts.example.com/realms/pinvou/protocol/openid-connect/token'],
      [...trust, 'keycloak.enabled=false', 'keycloak.adminApiUrl=https://ACCOUNTS.example.com:443',
        'keycloak.provisionerTokenUrl=https://ACCOUNTS.example.com:443/realms/pinvou/protocol/openid-connect/token'],
      [...trust, 'keycloak.enabled=false', 'keycloak.adminApiUrl=https://direct.invalid',
        'keycloak.provisionerTokenUrl=https://direct.invalid/realms/other/protocol/openid-connect/token'],
      [...trust, 'keycloak.enabled=false', 'keycloak.adminApiUrl=https://direct.invalid',
        'keycloak.provisionerTokenUrl=https://direct.invalid/realms/pinvou/protocol/openid-connect/token?'],
    ]) {
      const result = render(changes); assert.equal(result.error, undefined); assert.notEqual(result.status, 0);
      assert.match(result.stderr, /Identity .*Keycloak|Identity external Keycloak/);
    }
  });
  await t.test('development_http_and_environment_secret_remain_supported', () => {
    const items = documents(render(['global.environment=development', 'sms.enabled=true'], false));
    const pod = deployment(items, 'identity-service').spec.template.spec;
    const env = environment(pod.containers[0]);
    assert.ok(env.KEYCLOAK_ADMIN_API_URL.startsWith('http://')); assert.equal(pod.initContainers, undefined);
    assert.equal(env.KEYCLOAK_CA_FILE, undefined);
    assert.ok(pod.containers[0].env.find(item => item.name === 'KEYCLOAK_PROVISIONER_CLIENT_SECRET').valueFrom.secretKeyRef);
  });
});

test('real remote Compose keeps Portal issuer aligned with its authorization and Keycloak issuer', {
  skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1',
}, () => {
  const binary = process.env.PINVOU_TEST_KMS_COMPOSE_BINARY;
  const base = 'account-platform/deploy/compose/';
  const result = spawnSync(binary || 'docker', [...(binary ? [] : ['compose']),
    '--env-file', `${base}.env.example`, '--env-file', `${base}model-token.env.example`,
    '--env-file', 'account-platform/test/fixtures/compose-render.env',
    '-f', `${base}compose.yaml`, '-f', `${base}compose.remote.yaml`, 'config', '--format', 'json'],
  { cwd: root, encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024,
    env: { ...publicEnvironment(), PUBLIC_BASE_URL: 'https://identity.example.invalid',
      PORTAL_PUBLIC_URL: 'https://portal.example.invalid', COMPOSE_CONVERT_WINDOWS_PATHS: '1' } });
  assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
  const services = JSON.parse(result.stdout).services;
  const issuer = services.keycloak.environment.PINVOU_OIDC_ISSUER;
  assert.equal(issuer, 'https://identity.example.invalid/realms/pinvou');
  assert.equal(services['token-portal'].environment.PORTAL_OIDC_ISSUER, issuer);
  for (const name of ['account-service', 'billing-service', 'token-gateway']) assert.equal(services[name].environment.OIDC_ISSUER, issuer);
  assert.equal(services['token-portal'].environment.PORTAL_OIDC_AUTHORIZATION_URL, `${issuer}/protocol/openid-connect/auth`);
});
