import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { readFileSync, mkdtempSync, writeFileSync, rmSync, lstatSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import { productionImageDigestArgs } from '../fixtures/production-image-digests.mjs';
import YAML from 'yaml';
import Ajv from 'ajv';
import addFormats from 'ajv-formats';

const root = path.resolve(import.meta.dirname, '../../..');
const chart = path.join(root, 'account-platform/deploy/helm/account-platform');
const helper = path.join(root, 'account-platform/deploy/keycloak/test/provision_mobile_auth_test.py');
const isolatedEnvironment = () => Object.fromEntries(Object.entries(process.env).filter(([key]) =>
  ['PATH', 'HOME', 'USERPROFILE', 'SYSTEMROOT', 'TEMP', 'TMP', 'PINVOU_TEST_BASH'].includes(key.toUpperCase())));
const handoffSecretPath = '/var/run/secrets/pinvou/portal-handoff/client-secret';
const bridge = String.raw`
import json, runpy, sys
module = runpy.run_path(sys.argv[1])
facts = json.load(sys.stdin)
expected, hook = facts["expected"], facts["hook"]
fixture = module["ProvisioningContractTest"]()
names = ("PINVOU_OIDC_CLIENT_ID", "PINVOU_OIDC_AUDIENCE", "PORTAL_OIDC_CLIENT_ID", "PORTAL_PUBLIC_URL",
         "PORTAL_HANDOFF_OIDC_CLIENT_ID", "PORTAL_SESSION_HANDOFF", "PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE")
changes = {name: hook.get(name) for name in names}
# Only the verified container path maps to the fixture's controlled private file.
if changes["PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE"] == facts["secretPath"]:
    del changes["PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE"]
changes.update(CONTROL_DESKTOP_CLIENT=expected["desktop"], CONTROL_PORTAL_CLIENT=expected["portal"],
               CONTROL_HANDOFF_CLIENT=expected["handoff"])
secret = "controlled-handoff-value-for-local-tests"
result, arguments, bodies = fixture.run_producer(changes, existing=facts["existing"], handoff_secret="  " + secret + "  ")
fixture.assertEqual(0, result.returncode, result.stderr.decode())
fixture.assert_hardening(arguments)
clients = {body["clientId"]: body for body in bodies if "clientId" in body}
portal, handoff = clients[expected["portal"]], clients[expected["handoff"]]
fixture.assertEqual([expected["origin"] + "/auth/callback"], portal["redirectUris"])
fixture.assertFalse(handoff["publicClient"])
for field in ("standardFlowEnabled", "directAccessGrantsEnabled", "implicitFlowEnabled", "serviceAccountsEnabled"):
    fixture.assertFalse(handoff[field])
fixture.assertEqual([], handoff["redirectUris"])
fixture.assertEqual([], handoff["optionalClientScopes"])
fixture.assertEqual("true", handoff["attributes"]["standard.token.exchange.enabled"])
fixture.assertEqual("false", handoff["attributes"]["standard.token.exchange.enableRefreshRequestedTokenType"])
for owner, name, audience in (("portal-id", "pinvou-account-audience", expected["audience"]),
                              ("desktop-id", "pinvou-portal-handoff-audience", expected["handoff"]),
                              ("handoff-id", "pinvou-account-audience", expected["audience"])):
    action = "update" if facts["existing"] else "create"
    writes = [line for line in arguments.splitlines()
              if line.startswith(action + " clients/" + owner + "/protocol-mappers/models")
              and " -s name=" + name + " " in line]
    fixture.assertEqual(1, len(writes))
    fixture.assertIn('config."included.client.audience"=' + audience + ' ', writes[0])
if facts["existing"]:
    fixture.assertNotIn("create clients ", arguments)
    fixture.assertIn("update clients/handoff-id ", arguments)
else:
    fixture.assertEqual(3, sum(line.startswith("create clients ") for line in arguments.splitlines()))
fixture.assertEqual("controlled-handoff-value-for-local-tests", handoff["secret"])
for client in clients.values():
    if "secret" in client:
        fixture.assertNotIn(client["secret"], arguments)
        fixture.assertNotIn(client["secret"].encode(), result.stdout + result.stderr)
print(json.dumps({"handoff": handoff["clientId"], "audience": expected["audience"]}))
`;
function helmResult(release, { hookEnabled = true, keycloakEnabled = true } = {}) {
  // Use YAML values rather than --set-string so backslashes/newlines are not
  // consumed by Helm's CLI value parser before schema validation sees them.
  const directory = mkdtempSync(path.join(tmpdir(), 'pinvou-realm-hook-'));
  const valuesFile = path.join(directory, 'release.yaml');
  writeFileSync(valuesFile, YAML.stringify({ keycloak: { oidcClientId: release.desktop, enabled: keycloakEnabled, realmUpgradeEnabled: hookEnabled,
      ...(!keycloakEnabled ? { adminApiUrl: 'https://keycloak-direct.example.test', provisionerTokenUrl: 'https://keycloak-direct.example.test/realms/pinvou/protocol/openid-connect/token' } : {}) },
    ...(hookEnabled && keycloakEnabled ? {} : { global: { environment: 'staging' } }),
    oidc: { portalClientId: release.portal, audience: release.audience,
      ...(!keycloakEnabled ? { tokenUrl: 'https://accounts.example.com/realms/pinvou/protocol/openid-connect/token', jwksUrl: 'https://accounts.example.com/realms/pinvou/protocol/openid-connect/certs' } : {}),
      ...(release.handoff === undefined ? {} : { portalHandoffClientId: release.handoff }) }, public: { portalUrl: release.origin } }));
  try {
    const args = ['template', 'hook-contract', chart, '-f', path.join(chart, 'values-production-new.yaml'),
      '-f', path.resolve(import.meta.dirname, '../fixtures/production-site-network.yaml'),
      '-f', valuesFile, '--set-json', 'global.manualReviewOnCallUserIds=["00000000-0000-4000-8000-000000000707"]',
      ...productionImageDigestArgs, ...[
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
function provision(hook, expected, existing = true) {
  const python = process.env.PINVOU_TEST_PYTHON || (process.platform === 'win32' ? 'python' : 'python3');
  return spawnSync(python, ['-B', '-c', bridge, helper], { input: JSON.stringify({ hook, expected, existing, secretPath: handoffSecretPath }),
    encoding: 'utf8', timeout: 90000, maxBuffer: 1024 * 1024, env: { ...isolatedEnvironment(), PYTHONDONTWRITEBYTECODE: '1' } });
}
function verifyHandoffProjection(pod, container, secretName) {
  const env = environment(container);
  assert.equal(env.PORTAL_SESSION_HANDOFF, 'true');
  assert.equal(env.PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE, handoffSecretPath);
  const mount = container.volumeMounts.find(item => item.mountPath === path.posix.dirname(handoffSecretPath));
  assert.ok(mount, 'handoff secret mount is required');
  assert.equal(mount.readOnly, true);
  const volume = pod.volumes.find(item => item.name === mount.name);
  assert.deepEqual(volume.secret, { secretName, items: [{ key: 'portal-handoff-oidc-client-secret', path: 'client-secret' }] });
}

test('real Helm hook provisions and updates the exact Portal handoff client and audiences', {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1', timeout: 180000,
}, async t => {
  const releases = [
    { desktop: 'pinvou-desktop', portal: 'pinvou-token-portal', audience: 'pinvou-desktop', origin: 'https://tokens.example.com' },
    { desktop: 'pinvou-desktop', portal: 'pinvou-portal', audience: 'account-prod', handoff: 'handoff-custom', origin: 'https://portal.example.test' },
    { desktop: 'pinvou-desktop', portal: '_Portal.Release-V2', audience: '.Account_Prod-V2', handoff: '.Handoff_Release-V2', origin: 'https://portal-safe.example.test' },
    { desktop: 'pinvou-desktop', portal: 'portal-production-alt', audience: 'account-prod-alt', handoff: 'handoff-production-alt', origin: 'https://portal-alt.example.test' },
  ];
  for (const release of releases) await t.test(release.portal, () => {
    const expected = { ...release, handoff: release.handoff ?? 'pinvou-token-portal-handoff' };
    const resources = render(release);
    const job = resources.find(item => item.kind === 'Job' && item.metadata.name.endsWith('-keycloak-realm-upgrade'));
    assert.equal(job.metadata.annotations['helm.sh/hook'], 'post-install,post-upgrade');
    const pod = job.spec.template.spec;
    assert.equal(pod.automountServiceAccountToken, false);
    const hookContainer = pod.containers[0];
    assert.deepEqual(hookContainer.command, ['/opt/keycloak/bin/pinvou-provision-mobile-auth.sh']);
    const hook = environment(hookContainer);
    const deployments = resources.filter(item => item.kind === 'Deployment');
    const container = name => deployments.find(item => item.spec.template.metadata.labels['app.kubernetes.io/component'] === name).spec.template.spec.containers[0];
    for (const name of ['account-service', 'billing-service', 'token-gateway']) assert.equal(environment(container(name)).OIDC_AUDIENCE, release.audience);
    const portalDeployment = deployments.find(item => item.metadata.name.endsWith('-token-portal'));
    const portal = container('token-portal');
    assert.equal(environment(portal).PORTAL_OIDC_CLIENT_ID, release.portal);
    for (const consumer of [hookContainer, portal]) assert.equal(environment(consumer).PORTAL_HANDOFF_OIDC_CLIENT_ID, expected.handoff);
    const runtimeSecret = 'pinvou-account-platform-production-new';
    verifyHandoffProjection(pod, hookContainer, runtimeSecret);
    verifyHandoffProjection(portalDeployment.spec.template.spec, portal, runtimeSecret);
    for (const existing of [false, true]) {
      const result = provision(hook, expected, existing);
      assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
      assert.equal(JSON.parse(result.stdout).handoff, expected.handoff);
    }
    if (release.portal === 'portal-production-alt') {
      for (const missing of ['PINVOU_OIDC_AUDIENCE', 'PORTAL_OIDC_CLIENT_ID', 'PORTAL_HANDOFF_OIDC_CLIENT_ID', 'PORTAL_SESSION_HANDOFF', 'PORTAL_HANDOFF_OIDC_CLIENT_SECRET_FILE']) {
        const altered = { ...hook }; delete altered[missing];
        const negative = provision(altered, expected);
        assert.equal(negative.error, undefined); assert.notEqual(negative.status, 0, missing + ' omission was not detected');
      }
      for (const mutation of ['mount', 'readonly', 'key', 'path', 'secret']) {
        const altered = structuredClone(pod);
        const target = altered.containers[0];
        const mount = target.volumeMounts.find(item => item.name === 'portal-handoff-oidc');
        const volume = altered.volumes.find(item => item.name === 'portal-handoff-oidc');
        if (mutation === 'mount') target.volumeMounts = target.volumeMounts.filter(item => item !== mount);
        if (mutation === 'readonly') mount.readOnly = false;
        if (mutation === 'key') volume.secret.items[0].key = 'unrelated-key';
        if (mutation === 'path') volume.secret.items[0].path = 'wrong-path';
        if (mutation === 'secret') volume.secret.secretName = 'unrelated-secret';
        assert.throws(() => verifyHandoffProjection(altered, target, runtimeSecret), undefined, mutation);
      }
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


test('real Helm handoff identifier check applies only to the automatic realm upgrade hook', {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1', timeout: 180000,
}, () => {
  const release = { desktop: 'pinvou-desktop', portal: 'Portal_Release.v2', audience: 'Account_Prod.v2', origin: 'https://portal.example.test' };
  for (const [label, handoff] of invalidIdentifiers) {
    const result = helmResult({ ...release, handoff });
    assert.equal(result.error, undefined); assert.notEqual(result.status, 0, label);
    assert.equal(result.stdout, '');
    assert.match(result.stderr, /portalHandoffClientId/);
  }
  for (const handoff of safeIdentifiers) {
    const result = helmResult({ ...release, handoff });
    assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
  }
  for (const configuration of [{ hookEnabled: false }, { keycloakEnabled: false }]) {
    const result = helmResult({ ...release, handoff: 'handoff:custom' }, configuration);
    assert.equal(result.error, undefined); assert.equal(result.status, 0, result.stderr);
    assert.doesNotMatch(result.stdout, /component: keycloak-realm-upgrade/);
  }
});
