import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../../..');
const chart = path.join(root, 'account-platform/deploy/helm/account-platform');
const compose = path.join(root, 'account-platform/deploy/compose/compose.yaml');
const domains = ['account', 'billing', 'token'];
const publicEnv = () => Object.fromEntries(Object.entries(process.env).filter(([key]) =>
  ['PATH', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(key.toUpperCase())));

test('migration budgets reach every supported base deployment entry', () => {
  const config = YAML.parse(readFileSync(compose, 'utf8'));
  for (const domain of [...domains, 'payment-simulator']) {
    assert.equal(config.services[`${domain}-migrate`].environment.MIGRATION_TIMEOUT, '${MIGRATION_TIMEOUT:-2m}');
  }
  assert.equal(YAML.parse(readFileSync(path.join(chart, 'values.yaml'), 'utf8')).migrations.timeout, '2m');
});

test('actual Helm renders default and selected budgets for all three domains', {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1',
}, () => {
  for (const budget of ['2m', '15m']) {
    const args = ['template', 'budget-fixture', chart, '-f', path.join(chart, 'values-model-token-test.yaml')];
    if (budget !== '2m') args.push('--set-string', `migrations.timeout=${budget}`);
    const result = spawnSync(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', args, {
      encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024, env: publicEnv(), windowsHide: true,
    });
    assert.equal(result.error, undefined);
    assert.equal(result.status, 0, 'public config-only Helm render failed');
    const resources = YAML.parseAllDocuments(result.stdout).map(document => document.toJSON()).filter(Boolean);
    for (const domain of domains) {
      const job = resources.find(item => item.kind === 'Job'
        && item.spec.template.metadata.labels['app.kubernetes.io/component'] === `migrate-${domain}`);
      assert.ok(job, `missing ${domain} migration Job`);
      assert.equal(job.spec.template.spec.containers[0].env.find(item => item.name === 'MIGRATION_TIMEOUT')?.value, budget);
    }
  }
});

test('actual Compose renders default and selected budgets for all four domains', {
  skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1',
}, () => {
  for (const budget of ['2m', '15m']) {
    const env = publicEnv();
    if (budget !== '2m') env.MIGRATION_TIMEOUT = budget;
    const result = spawnSync(process.env.PINVOU_TEST_KMS_COMPOSE_BINARY || 'docker-compose', [
      '--env-file', path.join(root, 'account-platform/test/fixtures/compose-render.env'),
      '-f', compose, 'config', '--format', 'json',
    ], { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024, env, windowsHide: true });
    assert.equal(result.error, undefined);
    assert.equal(result.status, 0, 'public config-only Compose render failed');
    const config = JSON.parse(result.stdout);
    for (const domain of [...domains, 'payment-simulator']) {
      assert.equal(config.services[`${domain}-migrate`].environment.MIGRATION_TIMEOUT, budget);
    }
  }
});
