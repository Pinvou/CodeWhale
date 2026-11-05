import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { createRequire } from 'node:module';
import { resolve } from 'node:path';
import test from 'node:test';
import { currentRenderFixture } from '../fixtures/production-render.mjs';
import { repositoryRoot, workloads } from '../../../scripts/account-platform-production-render.mjs';

const require = createRequire(new URL('../../package.json', import.meta.url));
const YAML = require('yaml');
const read = path => readFileSync(resolve(repositoryRoot, path), 'utf8');
const recovery = 'account-approval-recovery';
const permissions = ['internal.token.approval-effect.read', 'internal.token.approval-result.read'];
function check(rules, subject) {
  const matches = rules.filter(rule => rule.service === recovery);
  assert.equal(matches.length, 1, 'one dedicated recovery identity');
  assert.equal(matches[0].subject, subject, 'recovery must use its own issued subject');
  assert.deepEqual([...matches[0].permissions].sort(), permissions, 'recovery receives only the two installed observation scopes');
  assert.equal(rules.filter(rule => rule.subject === subject).length, 1, 'no other service borrows recovery subject');
}
function negativeBindings(rules, subject) {
  for (const permission of permissions) {
    const changed = structuredClone(rules);
    const rule = changed.find(rule => rule.service === recovery);
    rule.permissions = rule.permissions.filter(value => value !== permission);
    assert.throws(() => check(changed, subject));
  }
  const changed = structuredClone(rules);
  changed.find(rule => rule.service === recovery).subject = changed.find(rule => rule.service === 'account-service').subject;
  assert.throws(() => check(changed, subject));
}

test('production Token recovery grant matches the existing workload and owner contracts', () => {
  const identity = JSON.parse(read('account-platform/deploy/production/workload-approval-recovery.json.template'));
  const rules = JSON.parse(read('account-platform/deploy/production/workload-token-rules.json.template'));
  assert.equal(identity.subject, '__APPROVAL_RECOVERY_WORKLOAD_SUBJECT__');
  assert.equal(identity.audience, 'pinvou-account-platform-internal');
  check(rules, identity.subject); negativeBindings(rules, identity.subject);
  const paths = JSON.parse(read('account-platform/contracts/openapi/approval-recovery-token.json')).paths;
  for (const permission of permissions) {
    const operations = Object.values(paths).flatMap(path => Object.values(path))
      .filter(operation => operation['x-required-permission'] === permission);
    assert.equal(operations.length, 1);
    assert.ok(operations[0]['x-required-workloads'].includes(recovery));
    assert.ok(read('account-platform/internal/token/management_routes.go').includes(`serviceauth.Require("${permission}")`));
  }
  const account = rules.find(rule => rule.service === 'account-service');
  for (const permission of ['internal.token.client-readiness.read', 'internal.token.client-readiness.attest']) {
    assert.ok(account.permissions.includes(permission), 'preserve the explicit invocation readiness caller contract');
  }
});

test('actual production Compose resolves Token recovery to the dedicated workload subject', {
  skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1',
}, () => {
  const fixture = currentRenderFixture('compose', 'models');
  try {
    const gateway = workloads(fixture.render, fixture.render.secrets).find(workload => workload.id === 'token-gateway');
    const rules = JSON.parse(gateway.environment.WORKLOAD_IDENTITY_RULES_JSON);
    check(rules, fixture.subjects[recovery]); negativeBindings(rules, fixture.subjects[recovery]);
  } finally { fixture.dispose(); }
});

for (const native of [false, true]) test(`actual retained Helm/Compose preserve Token recovery identity (${native ? 'native' : 'remote'})`, {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1' || process.env.PINVOU_TEST_KMS_COMPOSE !== '1',
}, () => {
  const environment = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
    ['PATH', 'HOME', 'USERPROFILE', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(key.toUpperCase())));
  function render(binary, args) {
    const result = spawnSync(binary, args, { cwd: repositoryRoot, env: { ...environment, COMPOSE_CONVERT_WINDOWS_PATHS: '1' },
      encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024, windowsHide: true, shell: false });
    assert.equal(result.error, undefined); assert.equal(result.status, 0, 'actual config renderer must succeed');
    return result.stdout;
  }
  const chart = resolve(repositoryRoot, 'account-platform/deploy/helm/account-platform');
  const items = YAML.parseAllDocuments(render(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm',
    ['template', 'token-recovery-fixture', chart, '--namespace', 'recovery-fixture', '-f', resolve(chart, 'values-model-token-test.yaml'),
      ...(native ? ['-f', resolve(chart, 'values-kms-native-test.yaml')] : [])]), { uniqueKeys: true });
  assert.ok(items.every(item => item.errors.length === 0));
  const resources = items.map(item => item.toJSON()).filter(Boolean);
  const account = resources.find(item => item.kind === 'ServiceAccount'
    && item.metadata.labels?.['app.kubernetes.io/component'] === recovery);
  assert.ok(account, 'the existing dedicated recovery ServiceAccount must be rendered');
  const deployment = resources.find(item => item.kind === 'Deployment'
    && item.spec.template.metadata.labels['app.kubernetes.io/component'] === 'token-gateway');
  const rules = JSON.parse(deployment.spec.template.spec.containers[0].env.find(value => value.name === 'WORKLOAD_IDENTITY_RULES_JSON').value);
  const subject = `system:serviceaccount:recovery-fixture:${account.metadata.name}`;
  check(rules, subject); negativeBindings(rules, subject);

  const base = resolve(repositoryRoot, 'account-platform/deploy/compose');
  const config = JSON.parse(render(process.env.PINVOU_TEST_KMS_COMPOSE_BINARY || 'docker',
    [...(process.env.PINVOU_TEST_KMS_COMPOSE_BINARY ? [] : ['compose']), '--env-file', resolve(base, '.env.example'),
      '--env-file', resolve(base, 'model-token.env.example'), '--env-file', resolve(repositoryRoot, 'account-platform/test/fixtures/compose-render.env'),
      ...(native ? ['--env-file', resolve(base, 'kms-native.env.example')] : []),
      '-f', resolve(base, 'compose.yaml'), '-f', resolve(base, 'compose.model-token.yaml'),
      ...(native ? ['-f', resolve(base, 'compose.kms-native.yaml')] : []),
      '--profile', 'model-token', '--profile', 'governance-operations', '--profile', 'real-newapi', 'config', '--format', 'json']));
  const composeRules = JSON.parse(config.services['token-gateway'].environment.WORKLOAD_IDENTITY_RULES_JSON);
  check(composeRules, `pinvou:service:${recovery}`); negativeBindings(composeRules, `pinvou:service:${recovery}`);
});
