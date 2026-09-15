import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import YAML from 'yaml';

const read = name => readFile(new URL(`../../${name}`, import.meta.url), 'utf8');
const consume = 'internal.approval-authorization.consume';
const expected = {
  'billing-service': ['internal.authorization.check', consume, 'internal.approval-execution.begin', 'internal.approval-execution.complete', 'internal.account-closure-state.read'],
  'token-gateway': ['internal.authorization.check', 'internal.authorization.delegate', 'internal.client-compatibility.read', 'internal.token-owner-candidates.read', 'internal.account.token-owner-authority.check'],
  'token-command-executor': ['internal.authorization.check', consume, 'internal.approval-execution.begin', 'internal.approval-execution.complete'],
  'token-credential-portal': ['internal.authorization.check'],
  'keycloak-mobile-authenticator': ['internal.auth.sms.verify', 'internal.authorization.check'],
};

function exactRules(rules) {
  assert.deepEqual(rules.map(rule => rule.service).sort(), Object.keys(expected).sort());
  for (const rule of rules) assert.deepEqual([...rule.permissions].sort(), [...expected[rule.service]].sort());
}

function accountHelmRules(source) {
  const branch = source.match(/\{\{- if eq \$app\.name "account-service" \}\}(\s*- name: WORKLOAD_IDENTITY_RULES_JSON[\s\S]*?)\{\{- else if eq \$app\.name "identity-service" \}\}/);
  assert.ok(branch, 'Account workload branch missing');
  const expression = branch[1].match(/^\s*- name: WORKLOAD_IDENTITY_RULES_JSON\s+value: \{\{ toJson \(list ([\s\S]*)\) \| quote \}\}\s*$/);
  assert.ok(expression, 'Account workload rules must stay closed');
  const pattern = /\(dict "subject" \(printf "system:serviceaccount:%s:%s-([a-z-]+)" \$root\.Release\.Namespace \$fullname\) "service" "([a-z-]+)" "permissions" \(list ((?:"[^"]+"\s*)+)\)\)/g;
  const rules = [...expression[1].matchAll(pattern)].map(match => {
    assert.equal(match[1], match[2] === 'keycloak-mobile-authenticator' ? 'keycloak' : match[2]);
    return { service: match[2], permissions: [...match[3].matchAll(/"([^"]+)"/g)].map(item => item[1]) };
  });
  assert.equal(expression[1].replace(pattern, '').trim(), '', 'unparsed Account workload rule');
  return rules;
}

test('approval consumer grants target only existing Billing and Token workload identities', async () => {
  const overlay = YAML.parse(await read('deploy/compose/compose.model-token.yaml'), { merge: true });
  const composeRules = JSON.parse(overlay.services['account-service'].environment.WORKLOAD_IDENTITY_RULES_JSON);
  exactRules(composeRules);
  for (const rule of composeRules) assert.equal(rule.subject, `pinvou:service:${rule.service}`);
  exactRules(accountHelmRules(await read('deploy/helm/account-platform/templates/app-deployments.yaml')));
  const edge = JSON.parse(await read('deploy/nginx/credential-edge.example.json'));
  assert.deepEqual(edge.workload.identities.filter(rule => rule.permissions.includes(consume)).map(rule => rule.service), ['token-command-executor']);
  assert.deepEqual(edge.workload.identities.find(rule => rule.service === 'token-credential-portal').permissions, ['internal.authorization.check']);
  // Reject omission, excess grants, duplicates and widening to other services.
  for (const mutate of [
    rules => { rules[0].permissions = rules[0].permissions.filter(p => p !== consume); },
    rules => { rules[0].permissions.push(consume); },
    rules => { rules[1].permissions.push(consume); },
    rules => { rules[0].permissions.push('internal.*'); },
    rules => { rules.push({ service: 'unexpected', permissions: [consume] }); },
  ]) {
    const candidate = structuredClone(composeRules);
    mutate(candidate);
    assert.throws(() => exactRules(candidate));
  }
});

test('all eight consumer HTTP operations require the exact service permission', async () => {
  const source = await read('internal/account/service.go');
  const routes = source.split('\n').filter(line => /internal\.(GET|POST)\("\/approval-authorization\//.test(line));
  assert.equal(routes.length, 8);
  for (const line of routes) assert.ok(line.includes(`serviceauth.Require("${consume}")`));
  const suffixes = ['binding', 'deliveries/:durable', 'observe', 'attempts/grant', 'attempts/failure', 'result', 'dlq-puback', 'terminal'];
  for (const suffix of suffixes) assert.equal(routes.filter(line => line.includes(`/${suffix}"`)).length, 1);
});
