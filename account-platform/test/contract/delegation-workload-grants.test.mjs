import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../../..');
const read = file => readFileSync(path.join(root, file), 'utf8');
const permission = 'internal.authorization.delegate';

function checkDelegation(rules, tokenSubject) {
  const delegated = rules.filter(rule => rule.permissions.includes(permission) || rule.permissions.includes('internal.*'));
  assert.equal(delegated.length, 1, 'only the installed Token caller may request human delegations');
  assert.equal(delegated[0].service, 'token-gateway');
  assert.equal(delegated[0].subject, tokenSubject);
  assert.equal(delegated[0].permissions.includes('internal.*'), false);
  const billing = rules.filter(rule => rule.service === 'billing-service');
  assert.equal(billing.length, 1);
  assert.equal(billing[0].permissions.includes(permission), false);
  assert.ok(billing[0].permissions.includes('internal.authorization.check'));
  assert.ok(billing[0].permissions.includes('internal.approval-execution.begin'));
}

test('production and Compose grant delegation only to the actual Token caller', () => {
  const production = JSON.parse(read('account-platform/deploy/production/workload-account-rules.json.template'));
  const compose = YAML.parse(read('account-platform/deploy/compose/compose.model-token.yaml'));
  const rules = JSON.parse(compose.services['account-service'].environment.WORKLOAD_IDENTITY_RULES_JSON);
  for (const [items, subject] of [[production, '__TOKEN_WORKLOAD_SUBJECT__'], [rules, 'pinvou:service:token-gateway']]) {
    checkDelegation(items, subject);
    const overgrant = structuredClone(items);
    overgrant.find(rule => rule.service === 'billing-service').permissions.push(permission);
    assert.throws(() => checkDelegation(overgrant, subject), /only the installed Token caller/);
    const missing = structuredClone(items);
    missing.find(rule => rule.service === 'token-gateway').permissions = [];
    assert.throws(() => checkDelegation(missing, subject), /only the installed Token caller/);
  }
});

test('rendered remote and native Helm keep exact caller/subject delegation grants', {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1',
}, () => {
  const chart = path.join(root, 'account-platform/deploy/helm/account-platform');
  const namespace = 'public-grant-fixture';
  for (const native of [false, true]) {
    const env = Object.fromEntries(Object.entries(process.env).filter(([key]) =>
      ['PATH', 'HOME', 'USERPROFILE', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(key.toUpperCase())));
    const result = spawnSync(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm',
      ['template', 'fixture', chart, '--namespace', namespace, '-f', path.join(chart, 'values-model-token-test.yaml'),
        ...(native ? ['-f', path.join(chart, 'values-kms-native-test.yaml')] : [])],
      { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024, env, windowsHide: true });
    assert.equal(result.error, undefined);
    assert.equal(result.status, 0, result.stderr);
    const documents = YAML.parseAllDocuments(result.stdout, { uniqueKeys: true });
    assert.ok(documents.every(document => document.errors.length === 0));
    const items = documents.map(document => document.toJSON()).filter(Boolean);
    const deployment = name => items.find(item => item.kind === 'Deployment'
      && item.spec.template.metadata.labels['app.kubernetes.io/component'] === name);
    const receiver = deployment('account-service').spec.template.spec.containers.find(container => container.name === 'account-service');
    const rules = JSON.parse(receiver.env.find(item => item.name === 'WORKLOAD_IDENTITY_RULES_JSON').value);
    const subject = `system:serviceaccount:${namespace}:${deployment('token-gateway').spec.template.spec.serviceAccountName}`;
    assert.ok(items.some(item => item.kind === 'ServiceAccount' && item.metadata.name === deployment('token-gateway').spec.template.spec.serviceAccountName));
    checkDelegation(rules, subject);
    const mutated = structuredClone(rules);
    mutated.find(rule => rule.service === 'billing-service').permissions.push(permission);
    assert.throws(() => checkDelegation(mutated, subject), /only the installed Token caller/);
  }
});
