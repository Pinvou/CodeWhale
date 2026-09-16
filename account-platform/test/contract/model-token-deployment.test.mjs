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

const nativeNATSConsumers = ['billing-service', 'account-event-relay', 'billing-event-relay', 'token-event-relay', 'token-command-executor'];
function nativeNATSFixture(operator = false) {
  const config = composeFixture();
  for (const [name, service] of Object.entries(config.services)) {
    for (const key of ['NATS_URL', 'NATS_CREDENTIALS_FILE']) delete (service.environment ||= {})[key];
    for (const volume of service.volumes || []) volume.source ||= `/fixture/workload/${name}`;
  }
  const names = operator ? [...nativeNATSConsumers, 'event-resources'] : nativeNATSConsumers;
  if (operator) config.services['event-resources'] = { environment: {}, profiles: ['event-operations'], restart: 'no',
    entrypoint: ['/service'], command: [], healthcheck: { disable: true } };
  for (const name of names) {
    const service = config.services[name];
    Object.assign(service, { user: '65532:65532', read_only: true });
    Object.assign(service.environment, { NATS_URL: 'tls://nats.example.invalid:4222',
      NATS_NKEY_SEED_FILE: '/var/run/secrets/pinvou/nats/nkey.seed', NATS_CA_FILE: '/var/run/secrets/pinvou/nats-ca/ca.crt' });
    (service.volumes ||= []).push(
      { type: 'bind', source: `/fixture/nats/${name}`, target: '/var/run/secrets/pinvou/nats', read_only: true, bind: { create_host_path: false } },
      { type: 'bind', source: '/fixture/nats-public-ca', target: '/var/run/secrets/pinvou/nats-ca', read_only: true, bind: { create_host_path: false } });
  }
  return config;
}
const nativeSeed = (config, name = 'billing-service') => config.services[name].volumes.find(volume => volume.target === '/var/run/secrets/pinvou/nats');

test('native NKey Compose uses only five real broker clients and optional explicit resource tool', () => {
  for (const operator of [false, true]) {
    const config = nativeNATSFixture(operator);
    assert.equal(checkGovernedComposeConfiguration(config).deployment_status, 'NOT_DEPLOYED');
    for (const name of ['account-service', 'token-gateway', 'token-portal', 'token-credential-portal']) {
      assert.equal(config.services[name].environment.NATS_NKEY_SEED_FILE, undefined);
    }
  }
  // A present but empty native setting must not silently select legacy JWT credentials.
  const legacy = composeFixture(); legacy.services['billing-service'].environment.NATS_NKEY_SEED_FILE = '';
  assert.throws(() => checkGovernedComposeConfiguration(legacy), /model_token_deployment_nats_/);
});

test('native NKey Compose rejects missing identities, mixed modes and unbounded clusters', () => {
  for (const name of nativeNATSConsumers) {
    for (const key of ['NATS_URL', 'NATS_NKEY_SEED_FILE', 'NATS_CA_FILE']) {
      const config = nativeNATSFixture(); delete config.services[name].environment[key];
      assert.throws(() => checkGovernedComposeConfiguration(config), /model_token_deployment_nats_/);
    }
  }
  for (const url of ['nats://nats.example.invalid:4222', 'tls://a:4222,tls://b:4222', 'tls://user:password@nats:4222',
    'tls://nats:4222/path', 'tls://nats:4222?token=fixture', 'tls://nats:4222#fragment', 'tls://nats', ' tls://nats:4222']) {
    const config = nativeNATSFixture(); for (const name of nativeNATSConsumers) config.services[name].environment.NATS_URL = url;
    assert.throws(() => checkGovernedComposeConfiguration(config), /model_token_deployment_nats_/);
  }
  for (const mutate of [
    c => { c.services['billing-service'].environment.NATS_CREDENTIALS_FILE = '/fixture/old.creds'; },
    c => { c.services['billing-service'].environment.NATS_URL = 'tls://other:4222'; },
    c => { c.services['billing-service'].environment.NATS_NKEY_SEED_FILE = ''; },
    c => { c.services['billing-service'].user = '0:0'; },
    c => { c.services['billing-service'].read_only = false; },
    c => { c.services['billing-service'].pid = 'host'; },
  ]) {
    const config = nativeNATSFixture(); mutate(config);
    assert.throws(() => checkGovernedComposeConfiguration(config), /model_token_deployment_nats_/);
  }
});

test('native NKey Compose rejects shared, unsafe, shadowed or leaked seed mounts', () => {
  for (const mutate of [
    c => { nativeSeed(c).source = nativeSeed(c, 'account-event-relay').source; },
    c => { nativeSeed(c).source = '/fixture/nats'; },
    c => { nativeSeed(c).source = '/fixture/nats-public-ca/seed'; },
    c => { nativeSeed(c).source = '/'; },
    c => { nativeSeed(c).source = '/fixture/../nats'; },
    c => { nativeSeed(c).read_only = false; },
    c => { nativeSeed(c).bind.create_host_path = true; },
    c => { c.services['billing-service'].volumes.push({ ...nativeSeed(c) }); },
    c => { c.services['billing-service'].volumes.push({ type: 'bind', source: '/fixture/other', target: '/var/run/secrets/pinvou' }); },
    c => { c.services['billing-service'].volumes[1].source = '/fixture/other-ca'; },
    c => { c.services['token-portal'].volumes = [{ type: 'bind', source: '/fixture', target: '/leaked', read_only: true }]; },
    c => { c.services['token-portal'].volumes_from = ['billing-service']; },
    c => { c.services['token-portal'].pid = 'service:billing-service'; },
    c => { c.services['account-service'].environment.NATS_URL = 'tls://nats.example.invalid:4222'; },
    c => { c.services['identity-service'] = { environment: { NATS_NKEY_SEED_FILE: '/fixture/seed' } }; },
    c => { c.services['token-portal'].environment.NATS_CREDENTIALS_FILE = '/fixture/legacy.creds'; },
  ]) {
    const config = nativeNATSFixture(); mutate(config);
    assert.throws(() => checkGovernedComposeConfiguration(config), /model_token_deployment_nats_/);
  }
});

test('native NKey resource operator cannot start implicitly, run hooks or share a seed', () => {
  for (const mutate of [
    c => { delete c.services['event-resources'].profiles; },
    c => { c.services['event-resources'].profiles.push('default'); },
    c => { c.services['event-resources'].restart = 'always'; },
    c => { c.services['event-resources'].command = ['-create-only']; },
    c => { c.services['event-resources'].entrypoint = ['/wrapper']; },
    c => { c.services['event-resources'].healthcheck = { test: ['CMD', '/service', '-create-only'] }; },
    c => { c.services['event-resources'].post_start = [{ command: '/service -create-only' }]; },
    c => { c.services['event-resources'].pre_stop = [{ command: '/service -create-only' }]; },
    c => { c.services['account-service'].depends_on = { 'event-resources': { condition: 'service_completed_successfully' } }; },
    c => { c.services['account-service'].network_mode = 'service:event-resources'; },
    c => { c.services['account-service'].links = ['event-resources:resource-tool']; },
    c => { c.services['account-service'].volumes_from = ['event-resources']; },
    c => { nativeSeed(c, 'event-resources').source = nativeSeed(c).source; },
  ]) {
    const config = nativeNATSFixture(true); mutate(config);
    assert.throws(() => checkGovernedComposeConfiguration(config), /model_token_deployment_nats_/);
  }
});

test('native NKey follows named bind volumes and Compose file resources without rejecting unrelated files', () => {
  const volumeFixture = source => {
    const config = nativeNATSFixture();
    config.volumes = { mapped: { driver: 'local', driver_opts: { type: 'none', o: 'bind', device: source } } };
    config.services['token-portal'].volumes = [{ type: 'volume', source: 'mapped', target: '/unrelated', read_only: true }];
    return config;
  };
  assert.equal(checkGovernedComposeConfiguration(volumeFixture('/fixture/unrelated-data')).deployment_status, 'NOT_DEPLOYED');
  for (const source of ['/fixture/nats/billing-service', '/fixture/nats/billing-service/nkey.seed', '/fixture']) {
    assert.throws(() => checkGovernedComposeConfiguration(volumeFixture(source)), /nats_identity_leaked/);
  }
  for (const declared of [{ external: true }, {}, { driver: 'unknown' }, { driver: 'local', driver_opts: { type: 'none', o: 'bind', device: '/fixture/unknown', extra: 'unproven' } }]) {
    const config = volumeFixture('/fixture/unrelated-data'); config.volumes.mapped = declared;
    assert.throws(() => checkGovernedComposeConfiguration(config), /nats_volume_source_unproven/);
  }
  for (const kind of ['secrets', 'configs']) {
    for (const source of ['/fixture/unrelated/public.conf', '/fixture/nats/billing-service/nkey.seed', '/fixture']) {
      const config = nativeNATSFixture(); config[kind] = { public_file: { file: source } };
      config.services['token-portal'][kind] = [{ source: 'public_file', target: '/unrelated.conf' }];
      if (source.includes('unrelated')) assert.equal(checkGovernedComposeConfiguration(config).deployment_status, 'NOT_DEPLOYED');
      else assert.throws(() => checkGovernedComposeConfiguration(config), /nats_identity_leaked/);
    }
    for (const target of ['/var/run/secrets/pinvou/nats/nkey.seed', '/var/run/secrets/pinvou/nats-ca/ca.crt', '/var/run/secrets/pinvou']) {
      const config = nativeNATSFixture(); config[kind] = { public_file: { file: '/fixture/unrelated/public.conf' } };
      config.services['billing-service'][kind] = [{ source: 'public_file', target }];
      assert.throws(() => checkGovernedComposeConfiguration(config), /nats_mount_shadowed/);
    }
    const config = nativeNATSFixture(); config[kind] = { unknown: { external: true } }; config.services['token-portal'][kind] = ['unknown'];
    assert.throws(() => checkGovernedComposeConfiguration(config), /nats_file_source_unproven/);
  }
  const tmpfs = nativeNATSFixture(); tmpfs.services['billing-service'].volumes.push({ type: 'tmpfs', target: '/unrelated-cache' });
  assert.equal(checkGovernedComposeConfiguration(tmpfs).deployment_status, 'NOT_DEPLOYED');
  tmpfs.services['billing-service'].volumes.at(-1).target = '/var/run/secrets/pinvou/nats';
  assert.throws(() => checkGovernedComposeConfiguration(tmpfs), /model_token_deployment_nats_/);
  const shortTmpfs = nativeNATSFixture(); shortTmpfs.services['billing-service'].tmpfs = ['/tmp:rw,noexec,size=16m'];
  assert.equal(checkGovernedComposeConfiguration(shortTmpfs).deployment_status, 'NOT_DEPLOYED');
  for (const target of ['/var/run/secrets/pinvou/nats', '/var/run/secrets/pinvou/nats-ca', '/var/run']) {
    shortTmpfs.services['billing-service'].tmpfs = [`${target}:rw,size=16m`];
    assert.throws(() => checkGovernedComposeConfiguration(shortTmpfs), /nats_mount_shadowed/);
  }
});

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
  const billing = JSON.parse(overlay.services['billing-service'].environment.WORKLOAD_IDENTITY_RULES_JSON);
  assert.deepEqual(token.find(rule => rule.service === 'token-command-executor').permissions, [
    'internal.token.model-lifecycle.execute', 'internal.token.catalog-rollback.execute', 'internal.token.credential-revoke.execute', 'internal.authorization.check',
  ]);
  assert.ok(token.find(rule => rule.service === 'account-service').permissions.includes('internal.token.approval-snapshot.read'));
  assert.ok(token.find(rule => rule.service === 'account-service').permissions.includes('internal.token.approval-effect.read'));
  assert.ok(token.find(rule => rule.service === 'account-service').permissions.includes('internal.authorization.check'));
  assert.ok(!token.find(rule => rule.service === 'token-command-executor').permissions.includes('internal.token.approval-effect.read'));
  assert.ok(billing.find(rule => rule.service === 'account-service').permissions.includes('internal.billing.approval-activation.read'));
  assert.ok(billing.find(rule => rule.service === 'account-service').permissions.includes('internal.billing.approval-effect.read'));
  assert.ok(!billing.find(rule => rule.service === 'token-gateway').permissions.includes('internal.billing.approval-activation.read'));
  assert.ok(!billing.find(rule => rule.service === 'token-gateway').permissions.includes('internal.billing.approval-effect.read'));
  for (const rules of [account, billing, token]) assert.ok(rules.every(rule => rule.subject === `pinvou:service:${rule.service}` && rule.permissions.every(permission => !permission.includes('*'))));
});

const billingWorkloadPermissions = {
  'account-service': [
    'internal.billing.wallet.ensure', 'internal.billing.account-closure-assessment.read',
    'internal.billing.account-closure-grants.isolate', 'internal.billing.organization.lifecycle',
    'internal.billing.organization-unit.lifecycle', 'internal.billing.organization-member.lifecycle',
    'internal.billing.approval-activation.read', 'internal.billing.approval-effect.read',
  ],
  'token-gateway': [
    'internal.billing.wallet.ensure', 'internal.billing.wallet.read', 'internal.billing.entitlement.read',
    'internal.billing.hold.reserve', 'internal.billing.hold.extend', 'internal.billing.hold.commit',
    'internal.billing.hold.release', 'internal.billing.account-closed-grant.reclaim',
    'internal.billing.pricing.read', 'internal.billing.pricing.bind', 'internal.billing.quota.read',
    'internal.billing.quota.allocate', 'internal.billing.quota.revoke', 'internal.billing.product.read',
    'internal.billing.payment-capability.read', 'internal.billing.recharge-order.create',
    'internal.billing.recharge-order.read', 'internal.billing.recharge-order.close',
    'internal.billing.payment-claim.create', 'internal.billing.token-account-wallet-provision.execute',
    'internal.billing.token-account-wallet-provision.read',
  ],
};

function assertBillingWorkloadPermissions(rules) {
  assert.deepEqual(rules.map(rule => rule.service).sort(), Object.keys(billingWorkloadPermissions).sort());
  for (const rule of rules) {
    assert.deepEqual([...rule.permissions].sort(), [...billingWorkloadPermissions[rule.service]].sort());
  }
}

// Inspect the real template's complete closed list, not a hand-written rendered
// fixture. Reject unparsed template syntax so additional rules cannot be ignored.
function billingRulesFromHelmTemplate(source) {
  const branch = source.match(/\{\{- else if eq \$app\.name "billing-service" \}\}([\s\S]*?)\{\{- else if eq \$app\.name "token-gateway" \}\}/);
  assert.ok(branch, 'Billing Helm workload branch is missing');
  const expression = branch[1].match(/^\s*- name: WORKLOAD_IDENTITY_RULES_JSON\s+value: \{\{ toJson \(list ([\s\S]*)\) \| quote \}\}\s*$/);
  assert.ok(expression, 'Billing Helm workload list must remain explicit and closed');
  const rulePattern = /\(dict "subject" \(printf "system:serviceaccount:%s:%s-([a-z-]+)" \$root\.Release\.Namespace \$fullname\) "service" "([a-z-]+)" "permissions" \(list ((?:"[^"]+"\s*)+)\)\)/g;
  const rules = [...expression[1].matchAll(rulePattern)].map(match => {
    assert.equal(match[1], match[2], 'workload subject does not match service');
    return { service: match[2], permissions: [...match[3].matchAll(/"([^"]+)"/g)].map(item => item[1]) };
  });
  assert.equal(expression[1].replace(rulePattern, '').trim(), '', 'unparsed Billing workload rule');
  return rules;
}

test('Billing Compose and real Helm template preserve exact least-privilege workload sets', async () => {
  const overlay = YAML.parse(await read('deploy/compose/compose.model-token.yaml'), { merge: true });
  assertBillingWorkloadPermissions(JSON.parse(overlay.services['billing-service'].environment.WORKLOAD_IDENTITY_RULES_JSON));
  const source = await read('deploy/helm/account-platform/templates/app-deployments.yaml');
  const rules = billingRulesFromHelmTemplate(source);
  assertBillingWorkloadPermissions(rules);
  // Regression oracles: omission, wildcard, wrong recipient, extra workload,
  // duplicate grants and loss of any existing permission must fail the check.
  for (const mutate of [
    rules => { rules[0].permissions.pop(); },
    rules => { rules[0].permissions.push('internal.billing.*'); },
    rules => { rules[1].permissions.push(rules[0].permissions.pop()); },
    rules => { rules.push({ service: 'token-portal', permissions: ['internal.billing.approval-activation.read'] }); },
    rules => { rules[0].permissions.push('internal.billing.approval-activation.read'); },
    rules => { rules[1].permissions.shift(); },
  ]) {
    const changed = structuredClone(rules);
    mutate(changed);
    assert.throws(() => assertBillingWorkloadPermissions(changed));
  }
  assert.throws(() => billingRulesFromHelmTemplate(source.replace('"service" "account-service" "permissions" (list "internal.billing.wallet.ensure"', '"service" "token-portal" "permissions" (list "internal.billing.wallet.ensure"')));
});
