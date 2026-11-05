import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { createRequire } from 'node:module';
import { currentRenderFixture } from '../fixtures/production-render.mjs';
import { workloads } from '../../../scripts/account-platform-production-render.mjs';
const require = createRequire(new URL('../../package.json', import.meta.url));
const YAML = require('yaml');
const current = ['internal.token.account-closure.assess', 'internal.token.account-closure.replay-clear'];
const retired = ['internal.token.account-closure-assessment.read', 'internal.token.replay.delete'];
function check(rules) {
  const account = rules.filter(rule => rule.service === 'account-service');
  assert.equal(account.length, 1);
  for (const scope of current) assert.ok(account[0].permissions.includes(scope), `missing actual Token route scope ${scope}`);
  for (const scope of retired) assert.ok(!account[0].permissions.includes(scope), `retired ineffective scope ${scope}`);
  assert.equal(new Set(account[0].permissions).size, account[0].permissions.length);
  assert.ok(account[0].permissions.every(permission => !permission.includes('*')));
  assert.ok(rules.find(rule => rule.service === 'billing-service').permissions.includes('internal.token.usage.waive'));
  assert.ok(rules.find(rule => rule.service === 'token-command-executor').permissions.includes('internal.token.credential-revoke.execute'));
}
test('retained Compose receiver policy binds only the current exact closure scopes', () => {
  const actual = YAML.parse(readFileSync(new URL('../../deploy/compose/compose.model-token.yaml', import.meta.url), 'utf8'));
  check(JSON.parse(actual.services['token-gateway'].environment.WORKLOAD_IDENTITY_RULES_JSON));
});
for (const kind of ['compose', 'helm']) test(`actual current ${kind} render retains exact authenticated Token closure scopes`, {
  skip: process.env[kind === 'compose' ? 'PINVOU_TEST_KMS_COMPOSE' : 'PINVOU_TEST_KMS_HELM'] !== '1',
}, () => {
  const f = currentRenderFixture(kind, 'models');
  try {
    const token = workloads(f.render, f.render.secrets).find(workload => (kind === 'compose' ? workload.id : workload.service.name) === 'token-gateway');
    assert.ok(token);
    check(JSON.parse(token.environment.WORKLOAD_IDENTITY_RULES_JSON));
  } finally { f.dispose(); }
});
