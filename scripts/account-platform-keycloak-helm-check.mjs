import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import YAML from '../account-platform/node_modules/yaml/dist/index.js';

export function verifyRenderedKeycloak(source) {
  const parsed = YAML.parseAllDocuments(source);
  for (const document of parsed) assert.equal(document.errors.length, 0, document.errors.join('\n'));
  const resources = parsed.map(document => document.toJSON()).filter(Boolean);
  const deployment = resources.find(resource => resource.kind === 'Deployment'
    && resource.metadata?.name?.endsWith('-keycloak'));
  assert.ok(deployment, 'rendered Keycloak Deployment is required');
  const container = deployment.spec?.template?.spec?.containers?.find(item => item.name === 'keycloak');
  assert.ok(container, 'rendered Keycloak container is required');
  assert.ok(container.ports?.some(port => port.name === 'management' && port.containerPort === 9000));
  assert.deepEqual(container.startupProbe?.httpGet,
    { path: '/health/started', port: 'management', scheme: 'HTTP' });
  assert.deepEqual(container.readinessProbe?.httpGet,
    { path: '/health/ready', port: 'management', scheme: 'HTTP' });

  const ingresses = resources.filter(resource => resource.kind === 'Ingress');
  const sticky = ingresses.find(resource => resource.metadata?.name?.endsWith('-keycloak-session'));
  assert.ok(sticky, 'dedicated Keycloak session ingress is required');
  assert.equal(sticky.spec.ingressClassName, 'nginx');
  assert.deepEqual(sticky.metadata.annotations, {
    'nginx.ingress.kubernetes.io/affinity': 'cookie',
    'nginx.ingress.kubernetes.io/affinity-mode': 'persistent',
    'nginx.ingress.kubernetes.io/session-cookie-name': 'pinvou-keycloak-route',
    'nginx.ingress.kubernetes.io/session-cookie-path': '/',
    'nginx.ingress.kubernetes.io/session-cookie-samesite': 'Lax',
    'nginx.ingress.kubernetes.io/session-cookie-secure': 'true',
    'nginx.ingress.kubernetes.io/session-cookie-change-on-failure': 'true',
  });
  assert.equal(sticky.spec.rules.length, 1);
  const stickyPaths = sticky.spec.rules[0].http.paths;
  assert.deepEqual(stickyPaths.map(item => [item.path, item.pathType]),
    [['/realms', 'Prefix'], ['/resources', 'Prefix']]);
  assert.ok(stickyPaths.every(item => item.backend.service.name.endsWith('-keycloak')
    && item.backend.service.port.number === 8080));

  const tokenPath = '/realms/pinvou/protocol/openid-connect/token';
  const shared = ingresses.find(resource => resource !== sticky && resource.spec?.rules?.some(rule =>
    rule.http?.paths?.some(item => item.path === tokenPath)));
  assert.ok(shared, 'shared ingress must retain the exact Identity token proxy');
  assert.equal(shared.spec.ingressClassName, 'nginx');
  const keycloakHostRule = shared.spec.rules.find(rule => rule.http.paths.some(item => item.path === tokenPath));
  assert.deepEqual(keycloakHostRule.http.paths.map(item => [item.path, item.pathType]), [[tokenPath, 'Exact']]);
  assert.ok(keycloakHostRule.http.paths[0].backend.service.name.endsWith('-identity-service'));
  assert.equal(shared.metadata.annotations?.['nginx.ingress.kubernetes.io/affinity'], undefined,
    'cookie affinity must not affect the shared ingress');
}

if (pathToFileURL(process.argv[1] || '').href === import.meta.url) {
  const file = process.argv[2];
  assert.ok(file, 'usage: node scripts/account-platform-keycloak-helm-check.mjs <rendered-helm.yaml>');
  verifyRenderedKeycloak(await readFile(file, 'utf8'));
  console.log('rendered Keycloak health and sticky-session contract passed');
}
