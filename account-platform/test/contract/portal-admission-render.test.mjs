import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const chart = path.resolve(import.meta.dirname, '../../deploy/helm/account-platform');
const mapping = [
  ['PORTAL_OPERATION_RPS', 'operationRps', '100', '9'],
  ['PORTAL_OPERATION_BURST', 'operationBurst', '200', '17'],
  ['PORTAL_LOGIN_FLOW_CAPACITY', 'loginFlowCapacity', '65536', '123'],
  ['PORTAL_SESSION_HANDOFF_CAPACITY', 'sessionHandoffCapacity', '8192', '23'],
];

function render(extra = []) {
  return spawnSync(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', [
    'template', 'portal-capacity', chart, '-f', path.join(chart, 'values-model-token-test.yaml'),
    '--set-json', 'workloadIdentity.kubernetesApiServerPorts=[443]',
    '--set-json', 'external.workloadJwksCidrs=["192.0.2.40/32"]', ...extra,
  ], { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024 });
}

test('Portal Compose exposes approved site defaults and overrides', () => {
  const root = new URL('../../deploy/compose/', import.meta.url);
  for (const file of [new URL('compose.yaml', root), new URL('../production/compose.personal.yaml', root)]) {
    const compose = YAML.parse(readFileSync(file, 'utf8'), { merge: true });
    const env = compose.services['token-portal'].environment;
    for (const [name, , expected] of mapping) assert.equal(env[name], '${' + name + ':-' + expected + '}');
  }
});

test('Portal actual Helm deployment receives defaults and all four overrides', { skip: process.env.PINVOU_TEST_KMS_HELM !== '1' }, () => {
  for (const override of [false, true]) {
    const result = render(override ? mapping.flatMap(([, key, , value]) => ['--set', 'portal.' + key + '=' + value]) : []);
    assert.equal(result.error, undefined);
    assert.equal(result.status, 0, result.stderr);
    const deployment = YAML.parseAllDocuments(result.stdout).map(x => x.toJSON()).find(x => x?.kind === 'Deployment' && x.metadata.labels?.['app.kubernetes.io/component'] === 'token-portal');
    assert.ok(deployment, 'the shipped Portal deployment must exist');
    const env = deployment.spec.template.spec.containers[0].env;
    for (const [name, , expected, configured] of mapping) {
      const fields = env.filter(x => x.name === name);
      assert.equal(fields.length, 1);
      assert.equal(fields[0].value, override ? configured : expected);
    }
  }
  for (const input of ['portal.operationRps=0', 'portal.operationBurst=1.5', 'portal.loginFlowCapacity=1000001', 'portal.sessionHandoffCapacity=-1']) {
    const result = render(['--set', input]);
    assert.equal(result.error, undefined);
    assert.notEqual(result.status, 0, 'invalid admission value reached a deployment');
  }
});
