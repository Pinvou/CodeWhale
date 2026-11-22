import assert from 'node:assert/strict';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import YAML from 'yaml';
import { checkHelmBillingApprovalArtifacts } from '../../../scripts/account-platform-billing-approval-artifacts-check.mjs';

const chart = path.resolve(import.meta.dirname, '../../deploy/helm/account-platform');
const enabled = { skip: process.env.PINVOU_TEST_KMS_HELM !== '1' };
function render(profile) {
  const args = ['base', 'staging'].includes(profile) ? [
    ...(profile === 'staging' ? ['-f', path.join(chart, 'values-staging.yaml')] : []),
    '--set', 'nats.enabled=false', '--set-string', 'dependencies.natsUrl=tls://nats.fixture.invalid:4222',
    '--set-string', 'newApi.tlsSecretName=fixture-only-newapi', '--set-string', 'newApi.caConfigMap=fixture-only-ca',
    '--set-string', 'images.newApiTLS.repository=registry.example.invalid/config-only',
    '--set-string', `images.newApiTLS.digest=sha256:${'0'.repeat(64)}`,
  ] : ['-f', path.join(chart, 'values-model-token-test.yaml'),
    ...(profile === 'native' ? ['-f', path.join(chart, 'values-kms-native-test.yaml')] : [])];
  const result = spawnSync(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', ['template', 'fixture', chart, ...args, '--set-json', 'workloadIdentity.kubernetesApiServerPorts=[443]', '--set-json', 'external.workloadJwksCidrs=["192.0.2.40/32"]'],
    { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024,
      env: Object.fromEntries(Object.entries(process.env).filter(([key]) =>
        ['PATH', 'HOME', 'USERPROFILE', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(key.toUpperCase()))) });
  assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
  return YAML.parseAllDocuments(result.stdout).map(doc => doc.toJSON()).filter(Boolean);
}
const billing = items => items.find(item => item.kind === 'Deployment'
  && item.spec.template.metadata.labels['app.kubernetes.io/component'] === 'billing-service').spec.template.spec;
function trustedDigests(items) {
  const secrets = {};
  for (const entry of billing(items).containers[0].env.filter(v => v.name.startsWith('BILLING_APPROVAL_CONSUMER_') && v.name.endsWith('_SHA256'))) {
    const ref = entry.valueFrom.secretKeyRef;
    // Config-only independent Secret fixture, not hashes of real deployment
    // artifacts and not proof that resources have been initialized.
    secrets[`fixture/${ref.name}/${ref.key}`] = 'abc'[Object.keys(secrets).length].repeat(64);
  }
  return secrets;
}

test('actual Helm supplies all six required Billing approval consumer bootstrap inputs', enabled, () => {
  for (const profile of ['base', 'staging', 'remote', 'native']) {
    const items = render(profile), pod = billing(items), main = pod.containers[0];
    for (const suffix of ['PUBLIC_KEYS_FILE', 'PUBLIC_KEYS_SHA256', 'MANIFEST_FILE', 'MANIFEST_SHA256', 'RECEIPT_FILE', 'RECEIPT_SHA256']) {
      assert.ok(main.env.find(v => v.name === `BILLING_APPROVAL_CONSUMER_${suffix}`), `${profile}: required ${suffix}`);
    }
    assert.doesNotThrow(() => checkHelmBillingApprovalArtifacts(items));
    assert.doesNotThrow(() => checkHelmBillingApprovalArtifacts(items, trustedDigests(items), 'fixture'));
  }
});

test('rendered Billing artifact check rejects missing env, digest, public file wiring and aliases', enabled, () => {
  const original = render('native');
  const suffixes = ['PUBLIC_KEYS_FILE', 'PUBLIC_KEYS_SHA256', 'MANIFEST_FILE', 'MANIFEST_SHA256', 'RECEIPT_FILE', 'RECEIPT_SHA256'];
  for (const suffix of suffixes) {
    const items = structuredClone(original), main = billing(items).containers[0];
    main.env = main.env.filter(v => v.name !== `BILLING_APPROVAL_CONSUMER_${suffix}`);
    assert.throws(() => checkHelmBillingApprovalArtifacts(items), /billing_approval_artifacts_/, suffix);
  }
  for (const [name, mutate] of [
    ['wrong public key', pod => { pod.volumes.find(v => v.name === 'billing-approval-public-artifacts').configMap.items[0].key = 'private.creds'; }],
    ['missing public key', pod => { pod.volumes.find(v => v.name === 'billing-approval-public-artifacts').configMap.items.pop(); }],
    ['all ConfigMap keys', pod => { delete pod.volumes.find(v => v.name === 'billing-approval-public-artifacts').configMap.items; }],
    ['writable public file mode', pod => { pod.volumes.find(v => v.name === 'billing-approval-public-artifacts').configMap.defaultMode = 438; }],
    ['optional public evidence', pod => { pod.volumes.find(v => v.name === 'billing-approval-public-artifacts').configMap.optional = true; }],
    ['Secret carries public files', pod => { const v = pod.volumes.find(v => v.name === 'billing-approval-public-artifacts'); v.secret = v.configMap; delete v.configMap; }],
    ['main writable mount', pod => { pod.containers[0].volumeMounts.find(v => v.subPath === 'current-v2.json').readOnly = false; }],
    ['whole projection directory', pod => { const v = pod.containers[0].volumeMounts.find(v => v.subPath === 'current-v2.json'); v.mountPath = '/run/pinvou/approval-identities'; delete v.subPath; }],
    ['wrong subPath', pod => { pod.containers[0].volumeMounts.find(v => v.subPath === 'current-v2.json').subPath = 'manifest.json'; }],
    ['expanded subPath', pod => { pod.containers[0].volumeMounts.find(v => v.subPath === 'current-v2.json').subPathExpr = '$(OTHER)'; }],
    ['extra identity directory entry', pod => { pod.containers[0].volumeMounts.push({ name: 'billing-approval-public-artifacts', mountPath: '/run/pinvou/approval-identities/manifest.json', subPath: 'manifest.json', readOnly: true }); }],
    ['directory alias shadows identity', pod => { pod.containers[0].volumeMounts.push({ name: 'nats-private', mountPath: '/run/pinvou/approval-identities', readOnly: true }); }],
    ['private-directory artifact alias', pod => { pod.containers[0].env.find(v => v.name === 'BILLING_APPROVAL_CONSUMER_MANIFEST_FILE').value = '/run/pinvou/nats/private/manifest.json'; }],
    ['noncanonical file path', pod => { pod.containers[0].env.find(v => v.name === 'BILLING_APPROVAL_CONSUMER_MANIFEST_FILE').value = '/run/pinvou/approval-manifest/../approval-manifest/manifest.json'; }],
    ['untrusted literal digest', pod => { const e = pod.containers[0].env.find(v => v.name === 'BILLING_APPROVAL_CONSUMER_MANIFEST_SHA256'); delete e.valueFrom; e.value = 'a'.repeat(64); }],
    ['wrong digest key', pod => { pod.containers[0].env.find(v => v.name === 'BILLING_APPROVAL_CONSUMER_MANIFEST_SHA256').valueFrom.secretKeyRef.key = 'billing-approval-consumer-public-keys-sha256'; }],
    ['wrong digest authority', pod => { pod.containers[0].env.find(v => v.name === 'BILLING_APPROVAL_CONSUMER_MANIFEST_SHA256').valueFrom.secretKeyRef.name = 'unreviewed'; }],
    ['optional digest', pod => { pod.containers[0].env.find(v => v.name === 'BILLING_APPROVAL_CONSUMER_MANIFEST_SHA256').valueFrom.secretKeyRef.optional = true; }],
  ]) {
    const items = structuredClone(original);
    mutate(billing(items));
    assert.throws(() => checkHelmBillingApprovalArtifacts(items), /billing_approval_artifacts_/, name);
  }
  for (const key of Object.keys(trustedDigests(original))) for (const value of [undefined, '', 'invalid', 'A'.repeat(64)]) {
    const secrets = trustedDigests(original);
    if (value === undefined) delete secrets[key]; else secrets[key] = value;
    assert.throws(() => checkHelmBillingApprovalArtifacts(original, secrets, 'fixture'), /billing_approval_artifacts_digest_value/);
  }
});
