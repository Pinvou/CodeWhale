import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import YAML from 'yaml';
import Ajv from 'ajv';
import addFormats from 'ajv-formats';
import { checkGovernedComposeConfiguration, checkGovernedHelmResources, matchesPodSelector } from '../../../scripts/account-platform-model-token-deployment-check.mjs';

const read = name => readFile(new URL(`../../${name}`, import.meta.url), 'utf8');
function credentialEnv() {
  return { APP_ENV: 'staging', PORTAL_PUBLIC_URL: 'https://account-uat.pinvou.com',
    CREDENTIAL_PORTAL_PUBLIC_URL: 'https://credentials-uat.pinvou.com', OIDC_ISSUER: 'https://identity.example.invalid/realms/pinvou',
    OIDC_JWKS_URL: 'https://identity.example.invalid/certs', CREDENTIAL_OIDC_AUTHORIZATION_URL: 'https://identity.example.invalid/auth',
    CREDENTIAL_OIDC_TOKEN_URL: 'https://identity.example.invalid/token', CREDENTIAL_OIDC_CLIENT_ID: 'credential-client', CREDENTIAL_OIDC_AUDIENCE: 'credential-audience' };
}
function composeFixture() {
  const services = {};
  for (const name of ['account-service', 'billing-service', 'token-gateway', 'account-event-relay', 'billing-event-relay', 'token-event-relay', 'token-command-executor']) {
    services[name] = { environment: { NATS_URL: 'tls://event.example.invalid:4222', NATS_CREDENTIALS_FILE: '/nats/client.creds' } };
  }
  services['token-gateway'].environment.DATABASE_URL = 'postgresql://gateway_release@db/token';
  services['token-event-relay'].environment.DATABASE_URL = services['token-gateway'].environment.DATABASE_URL;
  services['token-credential-portal'] = { environment: { ...credentialEnv(), DATABASE_URL: 'postgresql://credential_release@db/token' }, ports: [{ host_ip: '127.0.0.1', published: '18108', target: 8080, protocol: 'tcp' }] };
  for (const name of ['token-command-executor', 'token-credential-portal']) {
    Object.assign(services[name], { read_only: true, healthcheck: { test: ['CMD', '/healthcheck'] }, networks: { control: {} },
      volumes: [{ type: 'bind', target: '/var/run/secrets/pinvou/workload', read_only: true, bind: { create_host_path: false } }] });
  }
  services['token-portal'] = { ports: [{ host_ip: '127.0.0.1', published: '18105', target: 8080, protocol: 'tcp' }] };
  services['token-governance-check'] = { profiles: ['governance-operations'], restart: 'no', command: [], healthcheck: { disable: true } };
  for (const name of ['token-gateway', 'token-credential-portal']) Object.assign(services[name].environment, {
    TOKEN_SECRET_PROVIDER: 'remote', TOKEN_SECRET_PROVIDER_URL: 'https://kms.example.invalid',
    TOKEN_SECRET_KEY_ID: 'fixture-key', TOKEN_SECRET_PROVIDER_TOKEN: `fixture-only-${name}`,
  });
  return { services };
}
function helmFixture() {
  const names = ['account-service', 'billing-service', 'token-gateway', 'account-event-relay', 'billing-event-relay', 'token-event-relay', 'token-command-executor', 'token-credential-portal'];
  const resources = names.map(name => ({ kind: 'Deployment', metadata: { name }, spec: { template: { metadata: { labels: { 'app.kubernetes.io/name': 'test', 'app.kubernetes.io/instance': 'test', 'app.kubernetes.io/component': name } }, spec: {
    automountServiceAccountToken: false, containers: [{ name, env: [{ name: 'NATS_URL', value: 'tls://event.example.invalid:4222' }, { name: 'NATS_CREDENTIALS_FILE', value: '/nats/creds' }],
      readinessProbe: { httpGet: { path: '/health/ready' } }, livenessProbe: { httpGet: { path: '/health/live' } } }],
    volumes: [{ projected: { sources: [{ serviceAccountToken: { audience: 'internal' } }] } }],
  } } } }));
  const setEnv = (name, values) => { resources.find(item => item.metadata.name === name).spec.template.spec.containers[0].env.push(...Object.entries(values).map(([key, value]) => typeof value === 'string' ? { name: key, value } : { name: key, valueFrom: value })); };
  for (const name of ['token-gateway', 'token-event-relay']) setEnv(name, { DATABASE_URL: { secretKeyRef: { name: 'gateway-release', key: 'database' } } });
  setEnv('token-credential-portal', { ...credentialEnv(), DATABASE_URL: { secretKeyRef: { name: 'credential-release', key: 'database' } } });
  for (const name of ['token-command-executor', 'token-credential-portal']) {
    resources.find(item => item.metadata.name === name).spec.template.metadata.labels['pinvou.io/restricted-token-control'] = 'true';
    resources.push({ kind: 'NetworkPolicy', metadata: { name: `test-${name}` }, spec: { policyTypes: ['Ingress', 'Egress'], podSelector: { matchLabels: { 'app.kubernetes.io/component': name } } } });
  }
  resources.push({ kind: 'NetworkPolicy', metadata: { name: 'test-internal-and-approved-egress' }, spec: { egress: [{}], podSelector: { matchExpressions: [{ key: 'pinvou.io/restricted-token-control', operator: 'DoesNotExist' }] } } });
  resources.push({ kind: 'Job', metadata: { name: 'control' }, spec: { suspend: true, backoffLimit: 0, template: { metadata: { labels: { 'app.kubernetes.io/name': 'test', 'app.kubernetes.io/instance': 'test', 'app.kubernetes.io/component': 'token-governance-check', 'pinvou.io/restricted-token-control': 'true' } }, spec: { containers: [{ command: ['/service'], args: [], env: ['TOKEN_MIGRATOR_DATABASE_URL', 'ACCOUNT_POLICY_DATABASE_URL'].map(name => ({ name, valueFrom: { secretKeyRef: { name: 'operations', key: name } } })) }] } } } });
  resources.push({ kind: 'NetworkPolicy', metadata: { name: 'test-token-governance-check' }, spec: { policyTypes: ['Ingress', 'Egress'], podSelector: { matchLabels: { 'app.kubernetes.io/component': 'token-governance-check' } }, egress: [{ to: [{ ipBlock: { cidr: '192.0.2.20/32' } }], ports: [{ protocol: 'TCP', port: 5432 }] }] } });
  return resources;
}

test('render checker detects NetworkPolicy permission union, missing readiness, secret leakage and auto Job', () => {
  assert.equal(checkGovernedHelmResources(helmFixture()).environment_status, 'ENV_PENDING');
  for (const mutate of [items => { items.find(item => item.metadata.name.endsWith('internal-and-approved-egress')).spec.podSelector = {}; },
    items => { items.find(item => item.metadata.name === 'token-command-executor').spec.template.spec.containers[0].env.push({ name: 'DATABASE_URL', value: 'not-a-real-secret' }); },
    items => { items.find(item => item.kind === 'Job').spec.suspend = false; },
    items => { items.find(item => item.kind === 'Job').spec.template.spec.containers[0].args.push('--apply'); },
    items => { items.find(item => item.metadata.name === 'token-credential-portal').spec.template.spec.containers[0].readinessProbe = {}; }]) {
    const fixture = helmFixture(); mutate(fixture); assert.throws(() => checkGovernedHelmResources(fixture), /model_token_deployment_/);
  }
  assert.equal(matchesPodSelector({ restricted: 'true' }, { matchExpressions: [{ key: 'restricted', operator: 'DoesNotExist' }] }), false);
  assert.equal(matchesPodSelector({ restricted: 'true' }, {}), true);
});

test('all selecting policies enforce bounded peers and ports including trusted names and control Job', () => {
  const peer = name => ({ podSelector: { matchLabels: { 'app.kubernetes.io/name': 'test', 'app.kubernetes.io/instance': 'test', 'app.kubernetes.io/component': name } } });
  const cidr = value => ({ ipBlock: { cidr: value } });
  const rule = (target, port) => ({ to: [target], ports: [{ protocol: 'TCP', port }] });
  const positive = helmFixture();
  positive.find(item => item.metadata.name === 'test-token-command-executor').spec.egress = [rule(peer('account-service'), 8080), rule(cidr('192.0.2.30/32'), 4222)];
  assert.equal(checkGovernedHelmResources(positive).environment_status, 'ENV_PENDING');
  for (const invalid of [{}, { to: [peer('account-service')] }, { ports: [{ port: 8080 }] },
    rule({}, 8080), rule(cidr('0.0.0.0/0'), 4222), rule(cidr('::/0'), 443),
    rule(cidr('192.0.2.20/32'), 5432), rule(peer('postgres'), 5432), rule(peer('kms'), 443), rule(peer('new-api'), 8080),
    rule({ namespaceSelector: {} }, 443), rule({ podSelector: {} }, 8080)]) {
    const fixture = helmFixture(); fixture.find(item => item.metadata.name === 'test-token-command-executor').spec.egress = [invalid];
    assert.throws(() => checkGovernedHelmResources(fixture), /network_/);
  }
  for (const name of ['test-token-credential-portal', 'test-token-governance-check']) {
    const fixture = helmFixture(); fixture.find(item => item.metadata.name === name).spec.egress = [{}];
    assert.throws(() => checkGovernedHelmResources(fixture), /network_rule_unbounded/);
  }
  const extra = helmFixture(); extra.push({ kind: 'NetworkPolicy', metadata: { name: 'unrelated-looking-policy' }, spec: { podSelector: {}, egress: [{}] } });
  assert.throws(() => checkGovernedHelmResources(extra), /network_rule_unbounded/);
});

test('Compose rejects operations credentials in every runtime and overlapping wildcard ports', () => {
  for (const name of Object.keys(composeFixture().services).filter(name => name !== 'token-governance-check')) {
    for (const key of ['TOKEN_MIGRATOR_DATABASE_URL', 'ACCOUNT_POLICY_DATABASE_URL']) {
      const fixture = composeFixture(); (fixture.services[name].environment ||= {})[key] = 'not-a-real-secret';
      assert.throws(() => checkGovernedComposeConfiguration(fixture), /operations_secret_in_runtime/);
    }
  }
  for (const host of ['0.0.0.0', '::', '0:0:0:0:0:0:0:0', '::ffff:0.0.0.0', '::FFFF:0000:0000', '::ffff:127.0.0.1', '0:0:0:0:0:FFFF:7F00:0001', '::FFFF:7f00:1', undefined]) {
    const fixture = composeFixture(); Object.assign(fixture.services['token-portal'].ports[0], { host_ip: host, published: '18108' });
    assert.throws(() => checkGovernedComposeConfiguration(fixture), /port_collision/);
  }
  const separateIP = composeFixture(); Object.assign(separateIP.services['token-portal'].ports[0], { host_ip: '127.0.0.2', published: '18108' });
  assert.equal(checkGovernedComposeConfiguration(separateIP).deployment_status, 'NOT_DEPLOYED');
  const separateProtocol = composeFixture(); Object.assign(separateProtocol.services['token-portal'].ports[0], { host_ip: '0.0.0.0', published: '18108', protocol: 'udp' });
  assert.equal(checkGovernedComposeConfiguration(separateProtocol).deployment_status, 'NOT_DEPLOYED');
  const differentIPv6 = composeFixture(); Object.assign(differentIPv6.services['token-portal'].ports[0], { host_ip: '2001:db8::1', published: '18108' });
  assert.equal(checkGovernedComposeConfiguration(differentIPv6).deployment_status, 'NOT_DEPLOYED');
  for (const protocol of ['TCP', '', null, 'quic', 1]) {
    const fixture = composeFixture(); fixture.services['token-portal'].ports[0].protocol = protocol;
    assert.throws(() => checkGovernedComposeConfiguration(fixture), /port_protocol_invalid/);
  }
  const range = composeFixture(); range.services['token-portal'].ports[0].published = '18107-18109';
  assert.throws(() => checkGovernedComposeConfiguration(range), /port_collision/);
});

test('read-only control execution rejects alternate entrypoints, shell wrappers and extra execution paths', () => {
  for (const command of [undefined, [], ['/service', '--apply'], ['/bin/sh', '-c', '/service'], ['/different-service']]) {
    const fixture = helmFixture(); fixture.find(item => item.kind === 'Job').spec.template.spec.containers[0].command = command;
    assert.throws(() => checkGovernedHelmResources(fixture), /governance_auto_execution/);
  }
  for (const field of ['containers', 'initContainers', 'ephemeralContainers']) {
    const fixture = helmFixture(); const spec = fixture.find(item => item.kind === 'Job').spec.template.spec;
    (spec[field] ||= []).push({ command: ['/service', '--apply'] });
    assert.throws(() => checkGovernedHelmResources(fixture), /governance_auto_execution/);
    for (const key of ['TOKEN_MIGRATOR_DATABASE_URL', 'ACCOUNT_POLICY_DATABASE_URL']) {
      const runtime = helmFixture(); const runtimeSpec = runtime.find(item => item.metadata.name === 'account-service').spec.template.spec;
      (runtimeSpec[field] ||= []).push({ env: [{ name: key, value: 'not-a-real-secret' }] });
      assert.throws(() => checkGovernedHelmResources(runtime), /operations_secret_in_runtime/);
    }
  }
  for (const field of ['lifecycle', 'readinessProbe', 'livenessProbe', 'startupProbe']) {
    const fixture = helmFixture(); fixture.find(item => item.kind === 'Job').spec.template.spec.containers[0][field] = { exec: { command: ['/service', '--apply'] } };
    assert.throws(() => checkGovernedHelmResources(fixture), /governance_auto_execution/);
  }
  for (const command of ['--apply', ['/service', '--apply'], ['--apply'], '/bin/sh -c /service']) {
    const fixture = composeFixture(); fixture.services['token-governance-check'].command = command;
    assert.throws(() => checkGovernedComposeConfiguration(fixture), /governance_auto_execution/);
  }
  for (const entrypoint of [['/service', '--apply'], ['/bin/sh', '-c', '/service'], '/service --apply', [], ['/different-service']]) {
    const fixture = composeFixture(); fixture.services['token-governance-check'].entrypoint = entrypoint;
    assert.throws(() => checkGovernedComposeConfiguration(fixture), /governance_auto_execution/);
  }
  for (const field of ['post_start', 'pre_stop']) {
    const fixture = composeFixture(); fixture.services['token-governance-check'][field] = [{ command: ['/service', '--apply'] }];
    assert.throws(() => checkGovernedComposeConfiguration(fixture), /governance_auto_execution/);
  }
  const probe = composeFixture(); probe.services['token-governance-check'].healthcheck = { test: ['CMD', '/service', '--apply'] };
  assert.throws(() => checkGovernedComposeConfiguration(probe), /governance_auto_execution/);
  for (const entrypoint of [undefined, null, '/service', ['/service']]) {
    const fixture = composeFixture(); fixture.services['token-governance-check'].entrypoint = entrypoint;
    assert.equal(checkGovernedComposeConfiguration(fixture).deployment_status, 'NOT_DEPLOYED');
  }
});

test('Compose render checker rejects shared port, old relay login, split NATS, development and unsafe origin', () => {
  assert.equal(checkGovernedComposeConfiguration(composeFixture()).deployment_status, 'NOT_DEPLOYED');
  for (const mutate of [s => { s['token-credential-portal'].ports[0].published = '18105'; },
    s => { s['token-event-relay'].environment.DATABASE_URL = 'postgresql://retired@db/token'; },
    s => { s['account-event-relay'].environment.NATS_URL = 'tls://other.example.invalid:4222'; },
    s => { s['token-credential-portal'].environment.OIDC_ISSUER = 'http://identity.example.invalid/realms/pinvou'; },
    s => { s['token-credential-portal'].environment.PORTAL_PUBLIC_URL = 'https://account.pinvou.com'; },
    s => { s['token-credential-portal'].environment.OIDC_JWKS_URL = 'http://keycloak:8080/certs'; },
    s => { s['token-credential-portal'].environment.APP_ENV = 'development'; },
    s => { s['token-command-executor'].networks.backend = {}; }]) {
    const fixture = composeFixture(); mutate(fixture.services); assert.throws(() => checkGovernedComposeConfiguration(fixture), /model_token_deployment_/);
  }
  for (const key of ['OIDC_ISSUER', 'OIDC_JWKS_URL', 'CREDENTIAL_OIDC_AUTHORIZATION_URL', 'CREDENTIAL_OIDC_TOKEN_URL']) {
    const fixture = composeFixture();
    const environment = fixture.services['token-credential-portal'].environment;
    environment[key] = environment[key].replace('identity.example.invalid/', 'identity.example.invalid:443/');
    assert.throws(() => checkGovernedComposeConfiguration(fixture), /oidc_invalid/);
  }
});

test('new Helm settings and gates are closed typed inputs and default off', async () => {
  const values = YAML.parse(await read('deploy/helm/account-platform/values.yaml'));
  const schema = JSON.parse(await read('deploy/helm/account-platform/values.schema.json'));
  const ajv = new Ajv({ strict: true }); addFormats(ajv); const valid = ajv.compile(schema);
  assert.equal(valid(values), true, JSON.stringify(valid.errors));
  for (const key of ['tokenCredentialPortal', 'tokenCommandExecutor', 'tokenGovernanceCheck']) {
    assert.equal(values[key].enabled, false);
    const wrong = structuredClone(values); wrong[key].enabled = 'false'; assert.equal(valid(wrong), false);
    const unknown = structuredClone(values); unknown[key].apply = true; assert.equal(valid(unknown), false);
  }
  const roles = structuredClone(values); roles.tokenCredentialPortal.egressCidrs = ['0.0.0.0/0']; assert.equal(valid(roles), false);
});

test('Helm declarations retain image pull credentials, isolated control Job and same-generation Token Relay', async () => {
  const workloads = await read('deploy/helm/account-platform/templates/token-governed-workloads.yaml');
  const network = await read('deploy/helm/account-platform/templates/networkpolicies.yaml');
  const helpers = await read('deploy/helm/account-platform/templates/_token-governance.tpl');
  const relay = await read('deploy/helm/account-platform/templates/event-relays.yaml');
  assert.match(workloads, /serviceAccountName:.*token-governance-check[\s\S]*global.imagePullSecrets/);
  assert.match(workloads, /suspend: true/); assert.match(workloads, /args: \[\]/);
  assert.doesNotMatch(workloads, /helm.sh\/hook|--apply/);
  assert.match(network, /restricted-token-control, operator: DoesNotExist/);
  assert.match(network, /app.kubernetes.io\/component: token-credential-portal/);
  assert.match(workloads, /OIDC_JWKS_URL, value: \{\{ \$app.settings.oidcJwksUrl/);
  assert.match(helpers, /ne \$issuer.scheme "https"/);
  assert.match(helpers, /or .Values.nats.enabled \(not \(hasPrefix "tls:\/\/" .Values.dependencies.natsUrl\)\)/);
  assert.match(helpers, /Credential Portal origin does not match/);
  assert.match(relay, /tokenGateway.runtimeSecret[\s\S]*token-gateway-database-url/);
});

test('Compose overlay uses one event cluster, per-service rotated credentials and no default new workers', async () => {
  const source = await read('deploy/compose/compose.model-token.yaml');
  const overlay = YAML.parse(source, { merge: true });
  for (const name of ['token-credential-portal', 'token-command-executor']) {
    assert.deepEqual(overlay.services[name].profiles, ['model-token']);
    assert.equal(overlay.services[name].read_only, true);
    assert.ok(overlay.services[name].volumes.every(volume => volume.read_only === true && volume.bind.create_host_path === false));
  }
  assert.equal(overlay.services['token-command-executor'].environment.DATABASE_URL, undefined);
  assert.equal(overlay.services['token-credential-portal'].environment.NATS_URL, '');
  assert.equal(overlay.services['token-credential-portal'].ports[0], '127.0.0.1:${CREDENTIAL_PORTAL_BIND_PORT:-18108}:8080');
  for (const name of ['account-service', 'billing-service', 'token-gateway', 'account-event-relay', 'billing-event-relay', 'token-event-relay', 'token-command-executor']) {
    assert.equal(overlay.services[name].environment.NATS_URL, '${MODEL_TOKEN_NATS_URL:?set the single authenticated event cluster}');
    assert.equal(overlay.services[name].environment.NATS_CREDENTIALS_FILE, '/var/run/secrets/pinvou/nats/client.creds');
  }
  assert.deepEqual(overlay.services['token-governance-check'].command, []);
  assert.deepEqual(overlay.services['token-governance-check'].profiles, ['governance-operations']);
  assert.doesNotMatch(source, /--apply|SERVICE: (?:lifecycle-worker|account-provisioning-worker)/);
});

test('workload roles preserve existing permissions and narrowly register credential authority/activation', async () => {
  const overlay = YAML.parse(await read('deploy/compose/compose.model-token.yaml'), { merge: true });
  const account = JSON.parse(overlay.services['account-service'].environment.WORKLOAD_IDENTITY_RULES_JSON);
  assert.deepEqual(account.find(rule => rule.service === 'token-credential-portal').permissions, ['internal.authorization.check']);
  const token = JSON.parse(overlay.services['token-gateway'].environment.WORKLOAD_IDENTITY_RULES_JSON);
  assert.deepEqual(token.find(rule => rule.service === 'token-command-executor').permissions, [
    'internal.token.model-lifecycle.execute', 'internal.token.catalog-rollback.execute', 'internal.token.credential-revoke.execute', 'internal.authorization.check',
  ]);
  assert.ok(token.find(rule => rule.service === 'account-service').permissions.includes('internal.token.approval-snapshot.read'));
  assert.ok(token.find(rule => rule.service === 'account-service').permissions.includes('internal.authorization.check'));
  for (const rules of [account, token]) assert.ok(rules.every(rule => rule.subject === `pinvou:service:${rule.service}` && rule.permissions.every(permission => !permission.includes('*'))));
});
