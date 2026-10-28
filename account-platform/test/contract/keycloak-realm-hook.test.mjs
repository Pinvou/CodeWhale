import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync, mkdtempSync, writeFileSync, rmSync, lstatSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';
import Ajv from 'ajv';
import addFormats from 'ajv-formats';

const root = path.resolve(import.meta.dirname, '../../..');
const chart = path.join(root, 'account-platform/deploy/helm/account-platform');
const helper = path.join(root, 'account-platform/deploy/keycloak/test/provision_mobile_auth_test.py');
const isolatedEnvironment = () => Object.fromEntries(Object.entries(process.env).filter(([key]) =>
  ['PATH', 'HOME', 'USERPROFILE', 'SYSTEMROOT', 'TEMP', 'TMP', 'PINVOU_TEST_BASH'].includes(key.toUpperCase())));
const bridge = [
  'import json, runpy, sys',
  'module = runpy.run_path(sys.argv[1])',
  'facts = json.load(sys.stdin)',
  'expected, hook = facts["expected"], facts["hook"]',
  'fixture = module["ProvisioningContractTest"]()',
  'names = ("PINVOU_OIDC_CLIENT_ID", "PINVOU_OIDC_AUDIENCE", "PORTAL_OIDC_CLIENT_ID", "PORTAL_PUBLIC_URL")',
  'changes = {name: hook.get(name) for name in names}',
  'changes.update({"CONTROL_DESKTOP_CLIENT": expected["desktop"], "CONTROL_PORTAL_CLIENT": expected["portal"], "PORTAL_SESSION_HANDOFF": None})',
  'result, arguments, bodies = fixture.run_producer(changes, existing=True)',
  'fixture.assertEqual(0, result.returncode, result.stderr.decode())',
  'fixture.assert_hardening(arguments)',
  'fixture.assertNotIn("create clients ", arguments)',
  'portal = [body for body in bodies if body.get("clientId") == expected["portal"]]',
  'fixture.assertEqual(1, len(portal))',
  'fixture.assertEqual([expected["origin"] + "/auth/callback"], portal[0]["redirectUris"])',
  'writes = [line for line in arguments.splitlines() if line.startswith("update clients/portal-id/protocol-mappers/models/account-id ")]',
  'fixture.assertEqual(1, len(writes))',
  "fixture.assertIn(\"config.\" + chr(34) + \"included.client.audience\" + chr(34) + \"=\" + expected[\"audience\"] + \" \", writes[0])",
  'fixture.assertFalse(any(body.get("clientId") == "handoff-test" for body in bodies))',
  'print(json.dumps({"portal": portal[0]["clientId"], "audience": expected["audience"], "existing_mapper_updated": True}))',
].join('\n');
function helmResult(release) {
  // Use YAML values rather than --set-string so backslashes/newlines are not
  // consumed by Helm's CLI value parser before schema validation sees them.
  const directory = mkdtempSync(path.join(tmpdir(), 'pinvou-realm-hook-'));
  const valuesFile = path.join(directory, 'release.yaml');
  writeFileSync(valuesFile, YAML.stringify({ keycloak: { oidcClientId: release.desktop },
    oidc: { portalClientId: release.portal, audience: release.audience }, public: { portalUrl: release.origin } }));
  try {
    const args = ['template', 'hook-contract', chart, '-f', path.join(chart, 'values-production-new.yaml'),
      '-f', valuesFile, '--set-json', 'global.manualReviewOnCallUserIds=["00000000-0000-4000-8000-000000000707"]',
      ...[
        'newApi.tlsSecretName=fixture-only-newapi', 'newApi.caConfigMap=fixture-only-newapi-ca',
        'images.newApiTLS.repository=registry.example.invalid/config-only', 'images.newApiTLS.digest=sha256:' + '0'.repeat(64),
        'keycloak.tlsSecretName=fixture-only-keycloak', 'keycloak.caConfigMap=fixture-only-keycloak-ca',
      ].flatMap(value => ['--set-string', value])];
    return spawnSync(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', args,
      { encoding: 'utf8', timeout: 30000, maxBuffer: 4 * 1024 * 1024, env: { ...isolatedEnvironment(),
        HELM_CONFIG_HOME: path.join(directory, 'config'), HELM_CACHE_HOME: path.join(directory, 'cache'), HELM_DATA_HOME: path.join(directory, 'data') } });
  } finally {
    const owned = path.resolve(directory);
    assert.equal(path.dirname(owned), path.resolve(tmpdir()));
    assert.ok(path.basename(owned).startsWith('pinvou-realm-hook-') && !lstatSync(owned).isSymbolicLink());
    rmSync(owned, { recursive: true, force: true });
  }
}
function render(release) {
  const result = helmResult(release);
  assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
  return YAML.parseAllDocuments(result.stdout).map(document => { assert.deepEqual(document.errors, []); return document.toJSON(); }).filter(Boolean);
}
const environment = container => Object.fromEntries(container.env.filter(item => Object.hasOwn(item, 'value')).map(item => [item.name, item.value]));
function provision(hook, expected) {
  const python = process.env.PINVOU_TEST_PYTHON || (process.platform === 'win32' ? 'python' : 'python3');
  return spawnSync(python, ['-B', '-c', bridge, helper], { input: JSON.stringify({ hook, expected }),
    encoding: 'utf8', timeout: 90000, maxBuffer: 1024 * 1024, env: { ...isolatedEnvironment(), PYTHONDONTWRITEBYTECODE: '1' } });
}
test('real Helm hook preserves release client and audience through the actual existing-client provisioner', {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1', timeout: 180000,
}, async t => {
  // The release inputs are the oracle, independently of both rendered consumers.
  const releases = [
    { desktop: 'pinvou-desktop', portal: 'pinvou-token-portal', audience: 'pinvou-desktop', origin: 'https://tokens.example.com' },
    { desktop: 'pinvou-desktop', portal: 'pinvou-portal', audience: 'account-prod', origin: 'https://portal.example.test' },
    { desktop: 'pinvou-desktop', portal: '_Portal.Release-V2', audience: '.Account_Prod-V2', origin: 'https://portal-safe.example.test' },
    { desktop: 'pinvou-desktop', portal: 'portal-production-alt', audience: 'account-prod-alt', origin: 'https://portal-alt.example.test' },
  ];
  for (const release of releases) await t.test(release.portal, () => {
    const resources = render(release);
    const job = resources.find(item => item.kind === 'Job' && item.metadata.name.endsWith('-keycloak-realm-upgrade'));
    assert.equal(job.metadata.annotations['helm.sh/hook'], 'post-install,post-upgrade');
    const hookContainer = job.spec.template.spec.containers[0];
    assert.deepEqual(hookContainer.command, ['/opt/keycloak/bin/pinvou-provision-mobile-auth.sh']);
    const hook = environment(hookContainer);
    const deployments = resources.filter(item => item.kind === 'Deployment');
    const env = name => environment(deployments.find(item => item.spec.template.metadata.labels['app.kubernetes.io/component'] === name).spec.template.spec.containers[0]);
    for (const name of ['account-service', 'billing-service', 'token-gateway']) assert.equal(env(name).OIDC_AUDIENCE, release.audience);
    assert.equal(env('token-portal').PORTAL_OIDC_CLIENT_ID, release.portal);
    const result = provision(hook, release);
    assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
    assert.equal(JSON.parse(result.stdout).existing_mapper_updated, true);
    assert.equal(hook.PORTAL_OIDC_CLIENT_ID, release.portal); assert.equal(hook.PINVOU_OIDC_AUDIENCE, release.audience);
    // Each omitted mapping must independently fail the same real-script contract.
    if (release.portal === 'portal-production-alt') for (const missing of ['PINVOU_OIDC_AUDIENCE', 'PORTAL_OIDC_CLIENT_ID']) {
      const altered = { ...hook }; delete altered[missing];
      const negative = provision(altered, release);
      assert.equal(negative.error, undefined); assert.notEqual(negative.status, 0, missing + ' omission was not detected');
    }
  });
});

const invalidIdentifiers = [
  ['empty', ''], ['colon', 'account:prod'], ['urn', 'urn:pinvou:account'],
  ['space', 'account prod'], ['tab', 'account\tprod'], ['leading-lf', '\naccount-prod'],
  ['trailing-lf', 'account-prod\n'], ['cr', 'account-prod\r'], ['crlf', 'account-prod\r\n'],
  ['double-quote', 'account"prod'], ['single-quote', "account'prod"],
  ['backslash', 'account\\prod'], ['unicode', '账户-prod'],
];
const safeIdentifiers = ['Account_Prod.v2-Release', '.Account_Prod-v2', '_Portal.Release-V2', '-Release', '9', 'A'.repeat(80)];
test('OIDC updater schema accepts only its safe alphabet without importing generator-only restrictions', () => {
  const read = name => readFileSync(path.join(chart, name), 'utf8');
  const ajv = new Ajv({ strict: true }); addFormats(ajv);
  const validate = ajv.compile(JSON.parse(read('values.schema.json')));
  for (const field of ['audience', 'portalClientId']) {
    for (const value of safeIdentifiers) {
      const values = YAML.parse(read('values.yaml')); values.oidc[field] = value;
      assert.equal(validate(values), true, field + ': ' + JSON.stringify(validate.errors));
    }
    for (const [label, value] of invalidIdentifiers) {
      const values = YAML.parse(read('values.yaml')); values.oidc[field] = value;
      assert.equal(validate(values), false, field + ': ' + label);
      assert.deepEqual([...new Set(validate.errors.map(error => error.instancePath))], ['/oidc/' + field]);
    }
  }
  // These independent fields keep their existing contracts.
  const unchanged = YAML.parse(read('values.yaml'));
  unchanged.workloadIdentity.audience = 'urn:workload:custom';
  unchanged.oidc.portalHandoffClientId = 'handoff:custom';
  assert.equal(validate(unchanged), true, JSON.stringify(validate.errors));
});
test('real Helm rejects invalid updater identifiers at only the supplied field before rendering', {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1', timeout: 180000,
}, async t => {
  const valid = { desktop: 'pinvou-desktop', portal: 'Portal_Release.v2', audience: 'Account_Prod.v2', origin: 'https://portal.example.test' };
  for (const [field, member] of [['audience', 'audience'], ['portalClientId', 'portal']]) {
    for (const [label, value] of invalidIdentifiers) await t.test(field + ': ' + label, () => {
      const result = helmResult({ ...valid, [member]: value });
      assert.equal(result.error, undefined); assert.notEqual(result.status, 0);
      assert.equal(result.stdout, '', 'invalid values must produce no rendered resources');
      const paths = [...result.stderr.matchAll(/^\s*- ([A-Za-z][A-Za-z0-9_.]*):/gm)].map(match => match[1]);
      assert.deepEqual([...new Set(paths)], ['oidc.' + field], result.stderr);
    });
  }
  // The actual Helm validator also preserves leading punctuation and >64 chars.
  for (const value of ['.Account_Prod-v2', '_Portal.Release-V2', 'A'.repeat(80)]) {
    const result = helmResult({ ...valid, audience: value, portal: value });
    assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
    assert.notEqual(result.stdout, '');
  }
});
