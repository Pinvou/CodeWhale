import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, lstatSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, dirname, join, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import YAML from 'yaml';

const root = resolve(import.meta.dirname, '../../..');
const chart = join(root, 'account-platform/deploy/helm/account-platform');
const compose = join(root, 'account-platform/deploy/compose');
const permission = 'internal.auth.password-preparation.claim';
function isolated(run) {
  const home = mkdtempSync(join(tmpdir(), 'pinvou-password-preparation-render-'));
  const docker = join(home, 'docker'); mkdirSync(docker);
  const environment = { ...Object.fromEntries(Object.entries(process.env).filter(([name]) => ['PATH', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(name.toUpperCase()))),
    HOME: home, USERPROFILE: home, DOCKER_CONFIG: docker, COMPOSE_CONVERT_WINDOWS_PATHS: '1',
    HELM_CONFIG_HOME: join(home, 'config'), HELM_CACHE_HOME: join(home, 'cache'), HELM_DATA_HOME: join(home, 'data') };
  try { return run(environment); } finally {
    const target = resolve(home);
    assert.equal(dirname(target), resolve(tmpdir()));
    assert.ok(basename(target).startsWith('pinvou-password-preparation-render-') && !lstatSync(target).isSymbolicLink());
    rmSync(target, { recursive: true, force: true });
  }
}
function execute(binary, args, env) {
  const result = spawnSync(binary, args, { cwd: root, env, encoding: 'utf8', timeout: 30000, maxBuffer: 8 * 1024 * 1024, windowsHide: true });
  assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
  return result.stdout;
}
function grant(rules, subject) {
  assert.deepEqual(rules.filter(rule => rule.permissions.includes(permission)), [{ subject, service: 'identity-service', permissions: [permission] }]);
  assert.ok(rules.every(rule => !rule.permissions.includes('*')));
}
function checkNegativeControls(rules, subject) {
  grant(rules, subject);
  const entry = rules.find(rule => rule.service === 'identity-service');
  for (const altered of [{...entry, permissions: []}, {...entry, subject: `${subject}-other`}, {...entry, permissions: [permission, '*']}]) {
    assert.throws(() => grant(rules.map(rule => rule === entry ? altered : rule), subject));
  }
}
test('actual Helm preparation caller is the rendered Identity service account and source peers are explicit', {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1',
}, () => isolated(environment => {
  const release = 'preparation-contract', namespace = 'preparation-namespace';
  const raw = execute(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', ['template', release, chart, '--namespace', namespace,
    '-f', join(chart, 'values-model-token-test.yaml'), '--set-json', 'security.passwordPreparationTrustedProxyIPs=["192.0.2.10","192.0.2.11"]'], environment);
  const docs = YAML.parseAllDocuments(raw); assert.ok(docs.every(doc => doc.errors.length === 0));
  const resources = docs.map(doc => doc.toJSON()).filter(Boolean);
  const pod = name => resources.find(item => item.kind === 'Deployment' && item.spec.template.metadata.labels['app.kubernetes.io/component'] === name).spec.template.spec;
  const env = name => Object.fromEntries(pod(name).containers.find(item => item.name === name).env.map(item => [item.name, item.value]));
  const identity = pod('identity-service');
  assert.ok(resources.some(item => item.kind === 'ServiceAccount' && item.metadata.name === identity.serviceAccountName));
  checkNegativeControls(JSON.parse(env('account-service').WORKLOAD_IDENTITY_RULES_JSON), `system:serviceaccount:${namespace}:${identity.serviceAccountName}`);
  assert.equal(env('identity-service').ACCOUNT_HTTP_URL, `http://${release}-account-platform-account-service:8080`);
  assert.equal(env('identity-service').WORKLOAD_IDENTITY_TOKEN_FILE, '/var/run/secrets/pinvou/workload/token');
  assert.equal(env('identity-service').IDENTITY_PASSWORD_PREPARATION_TRUSTED_PROXY_IPS, '192.0.2.10,192.0.2.11');
  assert.equal(env('account-service').ACCOUNT_TRUSTED_SOURCE_PEER_IPS, '192.0.2.10,192.0.2.11');
}));
test('actual retained Compose preparation caller has one exact scope without changing the existing receivers', {
  skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1',
}, () => isolated(environment => {
  const binary = process.env.PINVOU_TEST_KMS_COMPOSE_BINARY;
  const raw = execute(binary || 'docker', [...(binary ? [] : ['compose']), '--env-file', join(compose, '.env.example'),
    '--env-file', join(compose, 'model-token.env.example'), '--env-file', join(root, 'account-platform/test/fixtures/compose-render.env'),
    '-f', join(compose, 'compose.yaml'), '-f', join(compose, 'compose.model-token.yaml'), '--profile', 'model-token', '--profile', 'real-newapi',
    'config', '--format', 'json'], environment);
  const services = JSON.parse(raw).services;
  checkNegativeControls(JSON.parse(services['account-service'].environment.WORKLOAD_IDENTITY_RULES_JSON), 'pinvou:service:identity-service');
  assert.equal(services['identity-service'].environment.ACCOUNT_HTTP_URL, 'http://account-service:8080');
  assert.equal(services['identity-service'].environment.IDENTITY_PASSWORD_PREPARATION_TRUSTED_PROXY_IPS, '');
}));
