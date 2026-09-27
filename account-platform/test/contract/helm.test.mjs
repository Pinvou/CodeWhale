import assert from 'node:assert/strict';
import { readFile, readdir } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';
import Ajv from 'ajv';
import addFormats from 'ajv-formats';

const root = path.resolve(import.meta.dirname, '../..');
const chart = path.join(root, 'deploy/helm/account-platform');

test('New API Compose and both Helm profiles pin the approved rc.26 index', async () => {
  const compose = YAML.parse(await readFile(path.join(root, 'deploy/compose/compose.yaml'), 'utf8'));
  const digest = 'sha256:7b82c866e9c0fb02faca7d5068e9d1261e47edf52462d63d9db7b614e7f1027d';
  for (const file of ['values.yaml', 'values-production-new.yaml']) {
    const values = YAML.parse(await readFile(path.join(chart, file), 'utf8'));
    assert.equal(values.images.newApi.repository, 'calciumion/new-api', file);
    assert.equal(values.images.newApi.tag, 'v1.0.0-rc.26', file);
    assert.equal(values.images.newApi.digest, digest, file);
    assert.equal(compose.services.newapi.image, `${values.images.newApi.repository}:${values.images.newApi.tag}@${digest}`, file);
  }
});

test('Helm production-new values contain references, not secret material or simulators', async () => {
  const raw = await readFile(path.join(chart, 'values-production-new.yaml'), 'utf8');
  const values = YAML.parse(raw);
  assert.equal(values.global.environment, 'production-new');
  assert.match(values.global.runtimeSecret, /production-new/);
  assert.equal(values.tokenGateway.secretProvider, 'remote');
  assert.equal(values.externalSecrets.enabled, true);
  assert.match(values.externalSecrets.secretStoreName, /production/);
  assert.match(values.externalSecrets.remoteKey, /production/);
  assert.ok(values.tokenGateway.secretRotationIntervalSeconds >= 60);
  assert.equal(values.keycloak.adminHostname, 'http://127.0.0.1:18081');
  assert.equal(values.keycloak.oidcClientId, 'pinvou-desktop');
  assert.equal(values.ingress.className, 'nginx');
  assert.equal(values.tokenGateway.providerAllowedHosts.includes('newapi'), true);
  assert.equal(values.availability.enabled, true);
	assert.equal(values.security.authRiskEnabled, true);
	assert.equal(values.sms.enabled, true);
  assert.equal(values.redis.enabled, false);
  assert.equal(values.nats.enabled, false);
  assert.match(values.dependencies.redisUrl, /^rediss:\/\//);
  assert.match(values.dependencies.natsUrl, /^tls:\/\//);
  for (const key of ['accountService', 'billingService', 'tokenGateway', 'tokenPortal', 'eventRelay']) assert.ok(values.replicas[key] >= 2);
  assert.match(values.images.newApi.digest, /^sha256:[a-f0-9]{64}$/);
  assert.ok(values.tokenGateway.providerAllowedHosts.every(host => !/mock|localhost|127\.0\.0\.1/i.test(host)));
  for (const cidrs of Object.values(values.external)) {
    assert.ok(cidrs.length > 0);
    assert.ok(cidrs.every(cidr => cidr !== '0.0.0.0/0' && cidr !== '::/0'));
  }
  assert.doesNotMatch(raw, /(password|clientSecret|accessToken|refreshToken|apiKey|api_key):\s*[^\s]+/i);
  assert.doesNotMatch(raw, /payment-simulator|mock-newapi/i);
});

test('Helm values schema rejects unsafe production-new overrides', async () => {
  const schema = JSON.parse(await readFile(path.join(chart, 'values.schema.json'), 'utf8'));
  const base = YAML.parse(await readFile(path.join(chart, 'values.yaml'), 'utf8'));
  const production = YAML.parse(await readFile(path.join(chart, 'values-production-new.yaml'), 'utf8'));
  const merge = (left, right) => Object.fromEntries(new Set([...Object.keys(left), ...Object.keys(right)]).values().map(key => [key, left[key] && right[key] && typeof left[key] === 'object' && typeof right[key] === 'object' && !Array.isArray(left[key]) ? merge(left[key], right[key]) : right[key] ?? left[key]]));
  const values = merge(base, production);
  const ajv = new Ajv({ allErrors: true, strict: true });
  addFormats(ajv);
  const validate = ajv.compile(schema);
  assert.equal(validate(values), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...values, networkPolicy: { ...values.networkPolicy, enabled: false } }), false);
  assert.equal(validate({ ...values, tokenGateway: { ...values.tokenGateway, secretProvider: 'local' } }), false);
  assert.equal(validate({ ...values, externalSecrets: { ...values.externalSecrets, enabled: false } }), false);
  assert.equal(validate({ ...values, ingress: { ...values.ingress, tlsSecretName: '' } }), false);
  assert.equal(validate({ ...values, public: { ...values.public, portalUrl: 'http://tokens.example.com' } }), false);
  assert.equal(validate({ ...values, newApi: { ...values.newApi, enabled: false } }), false);
  assert.equal(validate({ ...values, images: { ...values.images, newApi: { ...values.images.newApi, digest: '' } } }), false);
  assert.equal(validate({ ...values, external: { ...values.external, postgresCidrs: ['0.0.0.0/0'] } }), false);
  assert.equal(validate({ ...values, keycloak: { hostname: values.keycloak.hostname } }), false);
  assert.equal(validate({ ...values, keycloak: { ...values.keycloak, realmUpgradeEnabled: false } }), false);
  assert.equal(validate({ ...values, keycloak: { ...values.keycloak, oidcClientId: 'pinvou-desktop-dev' } }), false);
  assert.equal(validate({ ...values, availability: { ...values.availability, enabled: false } }), false);
  assert.equal(validate({ ...values, redis: { ...values.redis, enabled: true } }), false);
  assert.equal(validate({ ...values, dependencies: { ...values.dependencies, natsUrl: 'nats://nats.example.com:4222' } }), false);
  assert.equal(validate({ ...values, replicas: { ...values.replicas, billingService: 1 } }), false);
});

test('Helm chart includes required workloads, migration hooks, and deny-by-default policy', async () => {
  const templateDirectory = path.join(chart, 'templates');
  const files = await readdir(templateDirectory);
  const combined = (await Promise.all(files.map(name => readFile(path.join(templateDirectory, name), 'utf8')))).join('\n');
  for (const component of ['account-service', 'identity-service', 'billing-service', 'token-gateway', 'token-portal', 'new-api', 'keycloak', 'redis', 'nats']) {
    assert.match(combined, new RegExp(component));
  }
  assert.match(combined, /pre-install,pre-upgrade/);
  assert.match(combined, /default-deny/);
  assert.match(combined, /pinvou\.io\/newapi-data-plane/);
  assert.match(combined, /app\.kubernetes\.io\/component: token-gateway/);
  assert.match(combined, /newapi-database-url/);
  assert.match(combined, /newapi-session-secret/);
  assert.match(combined, /SYSTEM_PROBES_JSON/);
  assert.match(combined, /PORTAL_SESSION_HANDOFF/);
  assert.match(combined, /PORTAL_HANDOFF_OIDC_CLIENT_ID/);
  assert.match(combined, /portal-handoff-oidc-client-secret/);
  assert.match(combined, /kind: ExternalSecret/);
  assert.match(combined, /creationPolicy: Owner/);
  assert.match(combined, /TOKEN_SECRET_ROTATION_INTERVAL_SECONDS/);
  assert.match(combined, /range \$source := list "account" "billing" "token"/);
  assert.match(combined, /kind: Service[\s\S]*name: \{\{ \$fullname \}\}-\{\{ \$source \}\}-event-relay/);
  assert.match(combined, /name: monitor, port: 8222/);
  assert.match(combined, /name: KC_HOSTNAME_ADMIN/);
  assert.match(combined, /name: management, containerPort: 9000/);
  assert.match(combined, /startupProbe:[\s\S]*path: \/health\/started, port: management, scheme: HTTP/);
  assert.match(combined, /readinessProbe:[\s\S]*path: \/health\/ready, port: management, scheme: HTTP/);
  assert.match(combined, /kind: PodDisruptionBudget/);
  assert.match(combined, /topologySpreadConstraints/);
  assert.match(combined, /maxUnavailable: 0/);
  assert.match(combined, /pinvou\.natsUrl/);
  assert.doesNotMatch(combined, /kind:\s*Secret/);
});

test('Helm exposes only public authentication paths and keeps Keycloak administration tunneled', async () => {
  const ingress = await readFile(path.join(chart, 'templates/ingress.yaml'), 'utf8');
  const documents = ingress.split(/^---$/m);
  const sticky = documents.find(document => document.includes('-keycloak-session'));
  const shared = documents.find(document => !document.includes('-keycloak-session') && !document.includes('-token-admin'));
  assert.match(sticky, /nginx\.ingress\.kubernetes\.io\/affinity: "cookie"/);
  assert.match(ingress, /fail "ingress\.className must be nginx/);
  assert.match(sticky, /nginx\.ingress\.kubernetes\.io\/affinity-mode: "persistent"/);
  assert.match(sticky, /nginx\.ingress\.kubernetes\.io\/session-cookie-name: "pinvou-keycloak-route"/);
  assert.match(sticky, /nginx\.ingress\.kubernetes\.io\/session-cookie-path: "\/"/);
  assert.match(sticky, /^\s*- path: \/realms\s*$/m);
  assert.match(sticky, /^\s*- path: \/resources\s*$/m);
  assert.doesNotMatch(sticky, /identity-service|portalHost|identityHost|\/admin/);
	assert.match(shared, /path: \/realms\/pinvou\/protocol\/openid-connect\/token[\s\S]*pathType: Exact[\s\S]*identity-service/);
  assert.doesNotMatch(shared, /^\s*- path: \/(?:realms|resources)\s*$/m);
  assert.doesNotMatch(shared, /nginx\.ingress\.kubernetes\.io\/affinity/);
  assert.match(ingress, /path: \/v1\/registration\/password-prechecks/);
});

test('CI validates rendered Keycloak probes and session affinity', async () => {
  const [workflow, verifier] = await Promise.all([
    readFile(path.join(root, '../.github/workflows/pr-check.yml'), 'utf8'),
    readFile(path.join(root, '../scripts/account-platform-keycloak-helm-check.mjs'), 'utf8'),
  ]);
  assert.match(workflow, /helm:4\.2\.3 template pinvou[\s\S]*account-platform-keycloak-helm-check\.mjs \/tmp\/account-platform-production\.yaml/);
  assert.match(verifier, /\/health\/started[\s\S]*\/health\/ready/);
  assert.match(verifier, /pinvou-keycloak-route/);
  assert.match(verifier, /\[\['\/realms', 'Prefix'\], \['\/resources', 'Prefix'\]\]/);
  assert.match(verifier, /shared\.metadata\.annotations\?\.\['nginx\.ingress\.kubernetes\.io\/affinity'\]/);
});
