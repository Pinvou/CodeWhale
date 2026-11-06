import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const chart = path.resolve(import.meta.dirname, '../../deploy/helm/account-platform');
const enabled = { skip: process.env.PINVOU_TEST_KMS_HELM !== '1' };
const caVolume = 'redis-ca';
const caPath = '/run/pinvou/redis-trust/ca.crt';
const caConfigMap = 'redis-fixture-issuer';
const redisURL = 'rediss://redis.fixture.invalid:6379/0';

function render(...args) {
  const result = spawnSync(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', [
    'template', 'fixture', chart, '-f', path.join(chart, 'values-model-token-test.yaml'),
    '--set-json', 'workloadIdentity.kubernetesApiServerPorts=[443]',
    '--set-json', 'external.workloadJwksCidrs=["192.0.2.40/32"]', ...args,
  ], { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024 });
  assert.equal(result.error, undefined);
  return result;
}

function deployments(...args) {
  const result = render(...args);
  assert.equal(result.status, 0, result.stderr);
  const pods = YAML.parseAllDocuments(result.stdout).map(doc => doc.toJSON())
    .filter(doc => doc?.kind === 'Deployment').map(doc => doc.spec.template);
  return new Map(pods.map(template => [template.metadata.labels['app.kubernetes.io/component'], template.spec]));
}

function assertSMSRefs(pod, enabledSMS, provider) {
  const env = new Map(pod.containers[0].env.map(item => [item.name, item]));
  const aliyunRequired = enabledSMS && provider === 'aliyun-managed';
  const httpRequired = enabledSMS && provider === 'http-generated';
  const expected = [
    ['REGISTRATION_QUERY_HMAC_KEY_BASE64', 'registration-query-hmac-key-base64', enabledSMS],
    ['REGISTRATION_BONUS_FINGERPRINT_HMAC_KEY_BASE64', 'registration-bonus-fingerprint-hmac-key-base64', enabledSMS],
    ['SMS_VERIFICATION_HMAC_KEY_BASE64', 'sms-verification-hmac-key-base64', httpRequired],
    ['SMS_HTTP_BEARER_TOKEN', 'sms-http-bearer-token', false],
    ['ALIYUN_SMS_ACCESS_KEY_ID', 'aliyun-sms-access-key-id', aliyunRequired],
    ['ALIYUN_SMS_ACCESS_KEY_SECRET', 'aliyun-sms-access-key-secret', aliyunRequired],
    ['ALIYUN_SMS_SIGN_NAME', 'aliyun-sms-sign-name', aliyunRequired],
    ['ALIYUN_SMS_REGISTER_TEMPLATE', 'aliyun-sms-register-template', aliyunRequired],
    ['ALIYUN_SMS_LOGIN_TEMPLATE', 'aliyun-sms-login-template', aliyunRequired],
  ];
  const secretName = env.get('AUDIT_EXPORT_MASTER_KEY_BASE64').valueFrom.secretKeyRef.name;
  assert.equal(env.get('MOBILE_AUTH_ENABLED').value, String(enabledSMS));
  assert.equal(env.get('SMS_PROVIDER').value, provider);
  for (const [name, key, required] of expected) {
    assert.deepEqual(env.get(name)?.valueFrom.secretKeyRef, { name: secretName, key, optional: !required }, name);
  }
}

for (const enabledSMS of [false, true]) {
  for (const provider of ['aliyun-managed', 'http-generated']) {
    test(`Helm renders only consumed SMS Secret keys as required (${provider}, enabled=${enabledSMS})`, enabled, () => {
      const pods = deployments('--set', `sms.enabled=${enabledSMS}`, '--set-string', `sms.provider=${provider}`,
        '--set-string', 'sms.httpEndpoint=https://sms.fixture.invalid/send');
      assertSMSRefs(pods.get('account-service'), enabledSMS, provider);
      const identity = new Map(pods.get('identity-service').containers[0].env.map(item => [item.name, item]));
      assert.equal(identity.get('MOBILE_AUTH_ENABLED').value, String(enabledSMS));
      assert.deepEqual(identity.get('IDENTITY_PASSWORD_HMAC_KEY_BASE64').valueFrom.secretKeyRef, {
        name: identity.get('KEYCLOAK_PROVISIONER_CLIENT_ID').valueFrom.secretKeyRef.name,
        key: 'identity-password-hmac-key-base64', optional: !enabledSMS,
      });
    });
  }
}

function assertRedisCA(pods, authRiskEnabled, expectedComponents) {
  const identity = pods.get('identity-service');
  assert.ok(identity);
  for (const [component, pod] of pods) {
    const container = pod.containers[0];
    const entries = (container.env || []).filter(item => ['AUTH_RISK_REDIS_CA_FILE', 'REDIS_CA_FILE'].includes(item.name));
    const mounts = (container.volumeMounts || []).filter(item => item.name === caVolume);
    const volumes = (pod.volumes || []).filter(item => item.name === caVolume);
    if (expectedComponents.includes(component)) {
      const envName = component === 'identity-service' ? 'AUTH_RISK_REDIS_CA_FILE' : 'REDIS_CA_FILE';
      assert.deepEqual(entries, [{ name: envName, value: caPath }]);
      assert.deepEqual(mounts, [{ name: caVolume, mountPath: caPath, subPath: 'ca.crt', readOnly: true }]);
      assert.deepEqual(volumes, [{ name: caVolume, configMap: {
        name: caConfigMap, defaultMode: 292, items: [{ key: 'ca.crt', path: 'ca.crt' }],
      } }]);
    } else {
      assert.deepEqual(entries, [], component);
      assert.deepEqual(mounts, [], component);
      assert.deepEqual(volumes, [], component);
    }
  }
  const riskURL = identity.containers[0].env.find(item => item.name === 'AUTH_RISK_REDIS_URL');
  assert.equal(riskURL?.value, authRiskEnabled ? redisURL : undefined);
  assert.equal(pods.get('token-portal').containers[0].env.find(item => item.name === 'REDIS_URL')?.value, redisURL);
}

test('Helm scopes the configured Redis CA to clients of the shared endpoint', enabled, () => {
  const site = ['--set-string', `dependencies.redisUrl=${redisURL}`];
  const ca = ['--set-string', `dependencies.redisCAConfigMap=${caConfigMap}`];
  const risk = ['--set', 'sms.enabled=true,security.authRiskEnabled=true'];
  assertRedisCA(deployments(...site, '--set', 'security.authRiskEnabled=false', ...ca), false, ['token-portal']);
  assertRedisCA(deployments(...site, '--set', 'sms.enabled=false,security.authRiskEnabled=true', ...ca), true, ['token-portal']);
  assertRedisCA(deployments(...site, ...risk), true, []);
  assertRedisCA(deployments(...site, ...risk, ...ca), true, ['identity-service', 'token-portal']);
  const plaintext = render(...ca,
    '--set-string', 'dependencies.redisUrl=redis://redis.fixture.invalid:6379/0');
  assert.notEqual(plaintext.status, 0);
  assert.match(plaintext.stderr, /redisCAConfigMap requires a rediss:\/\/ Redis URL/);
});
