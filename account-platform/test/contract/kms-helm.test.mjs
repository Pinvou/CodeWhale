import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import YAML from 'yaml';
import Ajv from 'ajv';
import addFormats from 'ajv-formats';
import { checkGovernedHelmResources } from '../../../scripts/account-platform-model-token-deployment-check.mjs';

const chart = new URL('../../deploy/helm/account-platform/', import.meta.url);
const read = name => readFileSync(new URL(name, chart), 'utf8');
const parse = name => YAML.parse(read(name));
function merge(a, b) {
  for (const [key, value] of Object.entries(b)) {
    if (value && typeof value === 'object' && !Array.isArray(value)) a[key] = merge(a[key] || {}, value);
    else a[key] = value;
  }
  return a;
}
test('native KMS Helm values are typed, closed and fail before missing required inputs', () => {
  const ajv = new Ajv({ strict: true }); addFormats(ajv);
  const validate = ajv.compile(JSON.parse(read('values.schema.json')));
  const defaults = parse('values.yaml'); assert.equal(validate(defaults), true);
  assert.equal(defaults.tokenGateway.secretProvider, 'remote');
  const native = () => merge(merge(parse('values.yaml'), parse('values-model-token-test.yaml')), parse('values-kms-native-test.yaml'));
  assert.equal(validate(native()), true, JSON.stringify(validate.errors));
  for (const [field, value] of [['port', '8200'], ['port', 80], ['endpoint', 'http://kms'], ['endpoint', 'https://user:pw@kms'],
    ['endpoint', 'https://kms/?query'], ['gatewayBootstrapSecret', ''], ['caConfigMap', ''], ['cidrs', []],
    ['cidrs', ['0.0.0.0/0']], ['cidrs', ['192.0.2.40/24']], ['cidrs', ['999.0.0.1/32']], ['token', 'not-allowed']]) {
    const valueSet = native(); valueSet.nativeKms[field] = value; assert.equal(validate(valueSet), false, field);
  }
  const missing = native(); delete missing.nativeKms; assert.equal(validate(missing), false);
  const invalidKey = native(); invalidKey.tokenGateway.secretKeyId = '../other'; assert.equal(validate(invalidKey), false);
});

test('Helm native Agent configuration retains fixed source-verified identity and no proxy', () => {
  const helpers = read('templates/_native-kms.tpl'); const config = read('templates/native-kms.yaml');
  assert.match(helpers, /command: \["\/usr\/bin\/bao"\]/);
  assert.match(helpers, /openbao@sha256:11fd73a2102cda9c55d5d881a8c3210303146a7ec1e8ac76f526e175c6d24641/);
  assert.match(helpers, /medium: Memory/); assert.doesNotMatch(helpers, /hostPath:|kind: Secret|subPath:|TOKEN_SECRET_PROVIDER_TOKEN,/);
  assert.match(config, /remove_secret_id_file_after_reading = false/); assert.match(config, /mode = 384/);
  assert.doesNotMatch(config, /listener\s+"|cache\s*\{|api_proxy\s*\{|tls_skip_verify/);
});

test('real Helm native and remote rendering preserve isolation and reject invalid values', {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1',
}, () => {
  const image = process.env.PINVOU_TEST_KMS_HELM_IMAGE;
  const local = process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm';
  const root = fileURLToPath(chart);
  function run(action, native, overrides = []) {
    const directory = image ? '/chart' : root;
    const files = ['values-model-token-test.yaml', ...(native ? ['values-kms-native-test.yaml'] : [])];
    const args = [action, ...(action === 'template' ? ['fixture'] : []), directory,
      ...files.flatMap(f => ['-f', `${directory}/${f}`]), ...overrides.flatMap(value => ['--set-string', value])];
    const result = spawnSync(image ? 'docker' : local, image ? ['run', '--rm', '--network', 'none', '--read-only',
      '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges', '--memory', '512m', '--cpus', '1',
      '--tmpfs', '/tmp:rw,nosuid,nodev,size=16m', '-e', 'HELM_CACHE_HOME=/tmp/cache',
      '-v', `${root}:/chart:ro`, image, ...args] : args,
    { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024 });
    assert.equal(result.error, undefined); return result;
  }
  for (const native of [false, true]) {
    const lint = run('lint', native); assert.equal(lint.status, 0, lint.stderr || lint.stdout);
    const rendered = run('template', native); assert.equal(rendered.status, 0, rendered.stderr);
    const items = YAML.parseAllDocuments(rendered.stdout).map(d => d.toJSON()).filter(Boolean);
    assert.equal(checkGovernedHelmResources(items).deployment_status, 'NOT_DEPLOYED');
    const deployments = items.filter(v => v.kind === 'Deployment');
    const consumer = name => deployments.find(w => w.spec.template.metadata.labels['app.kubernetes.io/component'] === name);
    assert.equal(deployments.flatMap(w => w.spec.template.spec.containers).filter(c => c.name === 'kms-agent').length, native ? 2 : 0);
    if (!native) continue;
    const rules = items.find(v => v.kind === 'NetworkPolicy' && v.metadata.name.endsWith('-native-kms'));
    assert.deepEqual(rules.spec.podSelector.matchExpressions[0].values, ['token-gateway', 'token-credential-portal']);
    assert.deepEqual(rules.spec.egress, [{ to: [{ ipBlock: { cidr: '192.0.2.40/32' } }], ports: [{ protocol: 'TCP', port: 8200 }] }]);
    const badMutations = [
      w => { w.spec.hostPID = true; },
      w => { w.spec.shareProcessNamespace = true; },
      w => { w.spec.containers[0].volumeMounts.push({ name: 'kms-bootstrap', mountPath: '/stolen', readOnly: true }); },
      w => { w.spec.containers[0].volumeMounts.find(v => v.name === 'kms-token').readOnly = false; },
      w => { w.spec.containers[0].volumeMounts.find(v => v.name === 'kms-token').subPath = 'token'; },
      w => { w.spec.containers[0].volumeMounts.push({ name: 'tmp', mountPath: '/var/run/secrets' }); },
      w => { w.spec.volumes.find(v => v.name === 'kms-token').emptyDir.medium = ''; },
      w => { w.spec.containers[1].volumeMounts.push({ name: 'workload-identity', mountPath: '/stolen', readOnly: true }); },
      w => { w.spec.containers[1].image = 'openbao:latest'; },
      w => { w.spec.containers[1].securityContext.privileged = true; },
      w => { w.spec.volumes.find(v => v.name === 'kms-bootstrap').secret.defaultMode = 420; },
      w => { w.spec.volumes.push({ name: 'alias', projected: { sources: [{ secret: { name: 'fixture-kms-gateway-approle' } }] } }); },
      w => { w.spec.containers[0].env.push({ name: 'TOKEN_SECRET_PROVIDER_TOKEN', value: 'fixture-only' }); },
    ];
    for (const mutate of badMutations) {
      const clone = structuredClone(items);
      const gw = clone.find(v => v.kind === 'Deployment' && v.spec.template.metadata.labels['app.kubernetes.io/component'] === 'token-gateway');
      mutate(gw.spec.template); assert.throws(() => checkGovernedHelmResources(clone), /model_token_deployment_kms_/);
    }
    assert.ok(consumer('token-command-executor').spec.template.spec.containers.every(v => v.name !== 'kms-agent'));
  }
  for (const override of ['nativeKms.credentialBootstrapSecret=fixture-kms-gateway-approle',
    'nativeKms.gatewayBootstrapSecret=fixture-token-gateway-release', 'nativeKms.endpoint=http://kms',
    'nativeKms.endpoint=https://kms:8443', 'nativeKms.endpoint=https://kms:443', 'nativeKms.credentialBootstrapSecret=',
    'nativeKms.caConfigMap=', 'nativeKms.token=forbidden', 'nativeKms.cidrs[0]=0.0.0.0/0']) {
    const result = run('template', true, [override]); assert.notEqual(result.status, 0, override);
  }
});
