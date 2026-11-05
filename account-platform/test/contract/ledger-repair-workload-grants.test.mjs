import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../../..');
const read = name => readFileSync(path.join(root, name), 'utf8');
const proof = 'internal.account.ledger-repair-proof.read';

function assertBillingOnly(rules, expectedSubject) {
  assert.ok(Array.isArray(rules));
  assert.equal(new Set(rules.map(r => r.service)).size, rules.length, 'duplicate workload');
  for (const r of rules) {
    assert.ok(Array.isArray(r.permissions));
    assert.equal(new Set(r.permissions).size, r.permissions.length, 'duplicate permission');
    assert.ok(r.permissions.every(p => !p.includes('*')), 'wildcard forbidden');
  }
  const holders = rules.filter(r => r.permissions.includes(proof));
  assert.equal(holders.length, 1, 'exactly one proof recipient');
  assert.equal(holders[0].service, 'billing-service');
  assert.equal(holders[0].subject, expectedSubject);
}
function accountRulesFromHelmTemplate(source) {
  const branch = source.match(/\{\{- if eq \$app\.name "account-service" \}\}\s*(- name: WORKLOAD_IDENTITY_RULES_JSON[\s\S]*?)\{\{- else if eq \$app\.name "identity-service" \}\}/);
  assert.ok(branch, 'Account Helm workload branch missing');
  const expression = branch[1].match(/^\s*- name: WORKLOAD_IDENTITY_RULES_JSON\s+value: \{\{ toJson \(list ([\s\S]*)\) \| quote \}\}\s*$/);
  assert.ok(expression, 'Account workload list must be closed');
  const pattern = /\(dict "subject" \(printf "system:serviceaccount:%s:%s-([a-z-]+)" \$root\.Release\.Namespace \$fullname\) "service" "([a-z-]+)" "permissions" \(list ((?:"[^"]+"\s*)+)\)\)/g;
  const parsed = [...expression[1].matchAll(pattern)].map(m => {
    return { service: m[2], subject: `service-account:${m[1]}`, permissions: [...m[3].matchAll(/"([^"]+)"/g)].map(p => p[1]) };
  });
  assert.equal(expression[1].replace(pattern, '').trim(), '', 'unparsed Account workload rule');
  return parsed;
}

test('production, Compose and Helm grant historical repair proof only to Billing', () => {
  const production = JSON.parse(read('account-platform/deploy/production/workload-account-rules.json.template'));
  const compose = YAML.parse(read('account-platform/deploy/compose/compose.model-token.yaml'));
  const model = JSON.parse(compose.services['account-service'].environment.WORKLOAD_IDENTITY_RULES_JSON);
  const helm = accountRulesFromHelmTemplate(read('account-platform/deploy/helm/account-platform/templates/app-deployments.yaml'));
  for (const [rules, subject] of [[production, '__BILLING_WORKLOAD_SUBJECT__'], [model, 'pinvou:service:billing-service'], [helm, 'service-account:billing-service']]) {
    assertBillingOnly(rules, subject);
    const missing = structuredClone(rules); missing.find(r => r.service === 'billing-service').permissions = missing.find(r => r.service === 'billing-service').permissions.filter(p => p !== proof);
    assert.throws(() => assertBillingOnly(missing, subject));
    const overgrant = structuredClone(rules); overgrant.find(r => r.service === 'token-gateway').permissions.push(proof);
    assert.throws(() => assertBillingOnly(overgrant, subject));
    const wildcard = structuredClone(rules); wildcard.find(r => r.service === 'token-gateway').permissions.push('internal.*');
    assert.throws(() => assertBillingOnly(wildcard, subject));
    const wrongSubject = structuredClone(rules); wrongSubject.find(r => r.service === 'billing-service').subject = 'wrong';
    assert.throws(() => assertBillingOnly(wrongSubject, subject));
  }
});

test('rendered remote and native Helm grant the proof only to the deployed Billing identity', {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1',
}, () => {
  const chart = path.join(root, 'account-platform/deploy/helm/account-platform');
  const namespace = 'public-repair-proof-fixture';
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
    const objects = documents.map(document => document.toJSON()).filter(Boolean);
    const deployment = name => objects.find(item => item.kind === 'Deployment'
      && item.spec.template.metadata.labels['app.kubernetes.io/component'] === name);
    const account = deployment('account-service').spec.template.spec.containers.find(container => container.name === 'account-service');
    const billing = deployment('billing-service');
    const rules = JSON.parse(account.env.find(item => item.name === 'WORKLOAD_IDENTITY_RULES_JSON').value);
    const subject = `system:serviceaccount:${namespace}:${billing.spec.template.spec.serviceAccountName}`;
    assert.ok(objects.some(item => item.kind === 'ServiceAccount' && item.metadata.name === billing.spec.template.spec.serviceAccountName));
    assertBillingOnly(rules, subject);
    const changed = structuredClone(rules); changed.find(r => r.service === 'token-gateway').permissions.push(proof);
    assert.throws(() => assertBillingOnly(changed, subject));
  }
});
