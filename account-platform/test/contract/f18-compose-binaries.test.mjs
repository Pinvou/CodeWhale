import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import YAML from 'yaml';

// Private counterfactual input is only a test carrier; production never reads it.
const file = process.env.F18_TEST_COMPOSE_FILE || new URL('../../deploy/compose/compose.yaml', import.meta.url);
const document = YAML.parseDocument(readFileSync(file, 'utf8'));
assert.deepEqual(document.errors, []);
const compose = document.toJS({ maxAliasCount: 1000 });
function validate(services) {
  for (const domain of ['account', 'billing', 'token', 'payment-simulator']) {
    const job = services[`${domain}-migrate`];
    assert.equal(job.build.args.SERVICE, 'migrate');
    assert.deepEqual(job.command, ['--dir', `/migrations/${domain}`]);
  }
  const job = services['token-policy-bootstrap'];
  assert.equal(job.build.args.SERVICE, 'token-policy-bootstrap');
  assert.deepEqual(job.command, ['--mode', 'bootstrap']);
  assert.equal(job.entrypoint, undefined);
}
test('Compose migrations and policy bootstrap resolve to their own actual binaries and arguments', () => validate(compose.services));
test('Compose rejects an Account migration using the policy bootstrap binary', () => {
  const services = structuredClone(compose.services);
  services['account-migrate'].build.args.SERVICE = 'token-policy-bootstrap';
  assert.throws(() => validate(services));
});
test('Compose rejects policy bootstrap using migrate or an obsolete standalone entrypoint', () => {
  for (const wrong of ['binary', 'entrypoint']) {
    const services = structuredClone(compose.services);
    if (wrong === 'binary') services['token-policy-bootstrap'].build.args.SERVICE = 'migrate';
    else services['token-policy-bootstrap'].entrypoint = ['/token-policy-bootstrap'];
    assert.throws(() => validate(services));
  }
});
