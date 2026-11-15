import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../../..');
const chart = path.join(root, 'account-platform/deploy/helm/account-platform');
const compose = path.join(root, 'account-platform/deploy/compose/compose.yaml');
const keys = [
  ['ALIYUN_SMS_PROFILE_MOBILE_TEMPLATE', 'aliyun-sms-profile-mobile-template'],
  ['ALIYUN_SMS_PASSWORD_RESET_TEMPLATE', 'aliyun-sms-password-reset-template'],
];
const publicEnv = () => Object.fromEntries(Object.entries(process.env).filter(([key]) =>
  ['PATH', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(key.toUpperCase())));

test('base Compose exposes both optional purpose-template inputs', () => {
  const config = YAML.parse(readFileSync(compose, 'utf8'));
  for (const [key] of keys) assert.equal(config.services['account-service'].environment[key], '${' + key + ':-}');
});

test('actual Compose retains empty compatibility and forwards independent purpose overrides', {
  skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1',
}, () => {
  for (const overrides of [{}, { ALIYUN_SMS_PROFILE_MOBILE_TEMPLATE: 'fixture-profile' },
    { ALIYUN_SMS_PASSWORD_RESET_TEMPLATE: 'fixture-reset' },
    { ALIYUN_SMS_PROFILE_MOBILE_TEMPLATE: 'fixture-profile', ALIYUN_SMS_PASSWORD_RESET_TEMPLATE: 'fixture-reset' }]) {
    const result = spawnSync(process.env.PINVOU_TEST_KMS_COMPOSE_BINARY || 'docker-compose', [
      '--env-file', path.join(root, 'account-platform/test/fixtures/compose-render.env'), '-f', compose, 'config', '--format', 'json',
    ], { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024, env: { ...publicEnv(), ...overrides }, windowsHide: true });
    assert.equal(result.error, undefined);
    assert.equal(result.status, 0, 'public fixture render failed');
    const account = JSON.parse(result.stdout).services['account-service'];
    for (const [key] of keys) assert.equal(account.environment[key], overrides[key] || '');
  }
});

test('actual Helm keeps both purpose Secret references optional for old sites in each SMS mode', {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1',
}, () => {
  for (const [enabled, provider] of [[true, 'aliyun-managed'], [true, 'http-generated'], [false, 'aliyun-managed']]) {
    const result = spawnSync(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', [
      'template', 'sms-fixture', chart, '-f', path.join(chart, 'values-model-token-test.yaml'),
      '--set', `sms.enabled=${enabled}`, '--set-string', `sms.provider=${provider}`,
    ], { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024, env: publicEnv(), windowsHide: true });
    assert.equal(result.error, undefined);
    assert.equal(result.status, 0, 'public fixture render failed');
    const resources = YAML.parseAllDocuments(result.stdout).map(document => document.toJSON()).filter(Boolean);
    const account = resources.find(item => item.kind === 'Deployment'
      && item.spec.template.metadata.labels['app.kubernetes.io/component'] === 'account-service');
    assert.ok(account);
    const env = account.spec.template.spec.containers[0].env;
    assert.equal(new Set(env.map(item => item.name)).size, env.length, 'duplicate environment name');
    const login = env.find(item => item.name === 'ALIYUN_SMS_LOGIN_TEMPLATE').valueFrom.secretKeyRef;
    for (const [name, key] of keys) {
      assert.deepEqual(env.find(item => item.name === name)?.valueFrom.secretKeyRef,
        { name: login.name, key, optional: true });
    }
    assert.equal(login.optional, !(enabled && provider === 'aliyun-managed'));
  }
});
