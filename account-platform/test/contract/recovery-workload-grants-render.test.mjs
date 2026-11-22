import assert from 'node:assert/strict';
import { cpSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, dirname, isAbsolute, join, relative, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { createRequire } from 'node:module';
import test from 'node:test';
import { currentRenderFixture } from '../fixtures/production-render.mjs';
import { workloads } from '../../../scripts/account-platform-production-render.mjs';
const require = createRequire(new URL('../../package.json', import.meta.url));
const YAML = require('yaml');
const root = fileURLToPath(new URL('../../../', import.meta.url));
const chart = join(root, 'account-platform/deploy/helm/account-platform');
const composeBase = join(root, 'account-platform/deploy/compose');
const release = 'grant-contract-release';
const namespace = 'grant-contract-namespace';
const audience = 'pinvou-account-platform-internal';
const bindings = [
  { receiver: 'token-gateway', caller: 'billing-service', permission: 'internal.token.hold-expiry.prove', source: 'account-platform/internal/token/management_routes.go' },
];
const read = path => readFileSync(join(root, path), 'utf8');
function publicEnvironment(dockerConfig) {
  assert.ok(isAbsolute(dockerConfig));
  const home = dirname(dockerConfig);
  return { ...Object.fromEntries(Object.entries(process.env).filter(([key]) =>
    ['PATH', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(key.toUpperCase()))),
  HOME: home, USERPROFILE: home, DOCKER_CONFIG: dockerConfig,
  HELM_CONFIG_HOME: join(home, 'helm-config'), HELM_CACHE_HOME: join(home, 'helm-cache'), HELM_DATA_HOME: join(home, 'helm-data') };
}
function disposeOwnedDirectory(directory) {
  const parent = resolve(tmpdir()), target = resolve(directory), child = relative(parent, target);
  assert.ok(isAbsolute(target) && dirname(target) === parent && child !== ''
    && !child.startsWith('..') && !isAbsolute(child) && basename(target).startsWith('pinvou-workload-grants-'),
  'recursive cleanup must remain inside the owned temporary prefix');
  assert.ok(lstatSync(target).isDirectory() && !lstatSync(target).isSymbolicLink());
  rmSync(target, { recursive: true, force: true });
}

function rendered(binary, args, environment) {
  const result = spawnSync(binary, args, { cwd: root, env: environment, encoding: 'utf8',
    timeout: 30000, maxBuffer: 4 * 1024 * 1024, windowsHide: true, shell: false });
  assert.equal(result.error, undefined, 'native config renderer failed to execute');
  assert.equal(result.status, 0, 'public config-only renderer must succeed before grant assertions');
  return result.stdout;
}

function renderHelm(native, chartPath = chart, dockerConfig) {
  const args = ['template', release, chartPath, '--namespace', namespace,
    '-f', join(chartPath, 'values-model-token-test.yaml'),
    ...(native ? ['-f', join(chartPath, 'values-kms-native-test.yaml')] : [])];
  // A local Helm executable only. HELM_IMAGE never switches this test to docker run.
  const text = rendered(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', args, publicEnvironment(dockerConfig));
  const documents = YAML.parseAllDocuments(text, { uniqueKeys: true });
  assert.ok(documents.every(document => document.errors.length === 0));
  const resources = documents.map(document => document.toJSON()).filter(Boolean);
  const rules = {}, subjects = {};
  for (const name of ['billing-service', 'token-gateway']) {
    const deployment = resources.find(item => item.kind === 'Deployment'
      && item.spec.template.metadata.labels['app.kubernetes.io/component'] === name);
    assert.ok(deployment, `missing actual Helm caller/receiver ${name}`);
    const pod = deployment.spec.template.spec;
    const serviceAccount = resources.find(item => item.kind === 'ServiceAccount' && item.metadata.name === pod.serviceAccountName);
    assert.ok(serviceAccount, `caller lacks its actual rendered ServiceAccount: ${name}`);
    subjects[name] = `system:serviceaccount:${namespace}:${serviceAccount.metadata.name}`;
    assert.ok(serviceAccount.metadata.name.startsWith(`${release}-account-platform-`));
    const main = pod.containers.find(container => container.name === name);
    const value = main.env.find(env => env.name === 'WORKLOAD_IDENTITY_RULES_JSON');
    if (name !== 'token-command-executor') { assert.equal(typeof value?.value, 'string'); rules[name] = JSON.parse(value.value); }
    if (name !== 'token-command-executor') assert.equal(main.env.find(env => env.name === 'WORKLOAD_IDENTITY_AUDIENCE')?.value, audience);
    const allProjected = pod.volumes.flatMap(volume => volume.projected?.sources || []).map(source => source.serviceAccountToken).filter(Boolean);
    const projected = allProjected.filter(token => token.audience === audience);
    const api = allProjected.filter(token => !Object.hasOwn(token, 'audience'));
    const nativeJWKS = main.env.find(env => env.name === 'WORKLOAD_IDENTITY_JWKS_AUTH_MODE')?.value === 'kubernetes';
    assert.equal(allProjected.length, nativeJWKS ? 2 : 1, 'no unaccounted caller credential');
    assert.equal(api.length, nativeJWKS ? 1 : 0, 'API credential remains distinct from the business token');
    if (nativeJWKS) assert.equal(api[0].expirationSeconds, 600);
    assert.equal(projected.length, 1, `one projected caller token for ${name}`);
    assert.equal(projected[0].audience, audience);
    assert.ok(projected[0].expirationSeconds > 0 && projected[0].expirationSeconds <= 900);
  }
  return { rules, subjects };
}

function renderCompose(native, overlay = join(composeBase, 'compose.model-token.yaml'), dockerConfig) {
  const standalone = process.env.PINVOU_TEST_KMS_COMPOSE_BINARY;
  const args = [...(standalone ? [] : ['compose']),
    '--env-file', join(composeBase, '.env.example'), '--env-file', join(composeBase, 'model-token.env.example'),
    '--env-file', join(root, 'account-platform/test/fixtures/compose-render.env'),
    ...(native ? ['--env-file', join(composeBase, 'kms-native.env.example')] : []),
    '-f', join(composeBase, 'compose.yaml'), '-f', overlay,
    ...(native ? ['-f', join(composeBase, 'compose.kms-native.yaml')] : []),
    '--profile', 'model-token', '--profile', 'governance-operations', '--profile', 'real-newapi',
    'config', '--format', 'json'];
  // The actual retained owner composition, not deploy/production's other stack.
  const config = JSON.parse(rendered(standalone || 'docker', args,
    { ...publicEnvironment(dockerConfig), COMPOSE_CONVERT_WINDOWS_PATHS: '1' }));
  const rules = {}, subjects = {};
  for (const name of ['billing-service', 'token-gateway']) {
    assert.ok(config.services[name]);
    if (name !== 'token-command-executor') rules[name] = JSON.parse(config.services[name].environment.WORKLOAD_IDENTITY_RULES_JSON);
    if (name !== 'token-command-executor') assert.equal(config.services[name].environment.WORKLOAD_IDENTITY_AUDIENCE, audience);
    assert.equal(config.services[name].environment.WORKLOAD_IDENTITY_TOKEN_FILE, '/var/run/secrets/pinvou/workload/token');
    assert.ok(config.services[name].volumes.some(volume => volume.target === '/var/run/secrets/pinvou/workload' && volume.read_only));
    subjects[name] = `pinvou:service:${name}`;
  }
  return { rules, subjects };
}

function ruleFor(actual, receiver, caller) {
  const matches = actual.rules[receiver].filter(rule => rule.service === caller);
  assert.equal(matches.length, 1, `ambiguous/missing caller ${caller} on ${receiver}`);
  assert.equal(matches[0].subject, actual.subjects[caller], `wrong subject for ${caller} on ${receiver}`);
  return matches[0];
}


function check(actual, selected = bindings) {
  for (const [receiver, rules] of Object.entries(actual.rules)) {
    assert.equal(new Set(rules.map(rule => rule.service)).size, rules.length, `duplicate caller on ${receiver}`);
    assert.equal(new Set(rules.map(rule => rule.subject)).size, rules.length, `ambiguous identity on ${receiver}`);
    for (const rule of rules) {
      assert.equal(new Set(rule.permissions).size, rule.permissions.length);
      assert.ok(rule.permissions.every(permission => !permission.includes('*')), 'no wildcard grant');
    }
  }
  for (const binding of selected) {
    const rule = ruleFor(actual, binding.receiver, binding.caller);
    assert.ok(rule.permissions.includes(binding.permission), `missing exact workload grant ${binding.permission} for ${binding.caller} on ${binding.receiver}`);
  }
}
function withoutGrant(actual, binding) {
  const changed = structuredClone(actual);
  const rule = ruleFor(changed, binding.receiver, binding.caller);
  rule.permissions = rule.permissions.filter(permission => permission !== binding.permission);
  return changed;
}
function removeSourceGrant(original, kind, binding) {
  if (kind === 'compose') {
    const value = YAML.parse(original);
    const rules = JSON.parse(value.services[binding.receiver].environment.WORKLOAD_IDENTITY_RULES_JSON);
    const rule = rules.find(rule => rule.service === binding.caller);
    assert.ok(rule.permissions.includes(binding.permission));
    rule.permissions = rule.permissions.filter(permission => permission !== binding.permission);
    value.services[binding.receiver].environment.WORKLOAD_IDENTITY_RULES_JSON = JSON.stringify(rules);
    return YAML.stringify(value);
  }
  // Bind the one Helm dict to its existing service, not every repeated scope.
  const blocks = [...original.matchAll(new RegExp(`"service" "${binding.caller}" "permissions" \\(list [^)]*\\)`, 'g'))]
    .filter(match => match[0].includes(`"${binding.permission}"`));
  assert.equal(blocks.length, 1, 'one exact caller grant must be reverted');
  const match = blocks[0];
  return original.slice(0, match.index) + match[0].replace(` "${binding.permission}"`, '') + original.slice(match.index + match[0].length);
}

test('workload grants bind installed receiver routes and existing production callers', () => {
  const production = JSON.parse(read('account-platform/deploy/production/workload-token-rules.json.template'));
  const billing = production.filter(rule => rule.service === 'billing-service');
  assert.equal(billing.length, 1);
  assert.equal(billing[0].subject, '__BILLING_WORKLOAD_SUBJECT__');
  assert.ok(billing[0].permissions.includes(bindings[0].permission));
  for (const binding of bindings) assert.ok(read(binding.source).includes(`serviceauth.Require("${binding.permission}")`));
  assert.ok(read('account-platform/cmd/billing-service/main.go').includes('NewExpiredHoldRecoveryWorker(repository, expiredHoldClient'));
  assert.ok(read('account-platform/cmd/billing-service/main.go').includes('NewHTTPExpiredHoldProofClient(endpoint, credential, client)'));
  assert.ok(read('account-platform/internal/billing/expired_hold_client.go').includes('c.call(ctx, "/internal/v1/holds/expiry-proof", i, &p)'));
  assert.ok(read('account-platform/internal/billing/expired_hold_client.go').includes('serviceauth.SetAuthorization(req, c.credential)'));

});

for (const kind of ['helm', 'compose']) for (const native of [false, true]) {
  test(`actual retained ${kind} exact workload grants render (${native ? 'native' : 'remote'})`, {
    skip: process.env[kind === 'helm' ? 'PINVOU_TEST_KMS_HELM' : 'PINVOU_TEST_KMS_COMPOSE'] !== '1',
  }, () => {
    const directory = mkdtempSync(join(tmpdir(), 'pinvou-workload-grants-'));
    try {
      const dockerConfig = join(directory, 'docker-config'); mkdirSync(dockerConfig);
      writeFileSync(join(dockerConfig, 'config.json'), '{"auths":{}}\n');
      const render = kind === 'helm' ? renderHelm : renderCompose;
      const actual = render(native, undefined, dockerConfig); check(actual);
      // All required bindings fail individually when removed from rendered rules.
      for (const binding of bindings) assert.throws(() => check(withoutGrant(actual, binding)), /missing exact workload grant/);
      const input = kind === 'helm' ? join(directory, 'chart') : join(directory, 'compose.model-token.yaml');
      if (kind === 'helm') cpSync(chart, input, { recursive: true });
      const sourcePath = kind === 'helm' ? join(input, 'templates/app-deployments.yaml') : input;
      const original = kind === 'helm' ? readFileSync(sourcePath, 'utf8') : readFileSync(join(composeBase, 'compose.model-token.yaml'), 'utf8');
      // Revert the original missing grant in each retained carrier.
      for (const binding of bindings) {
        writeFileSync(sourcePath, removeSourceGrant(original, kind, binding));
        const changed = render(native, input, dockerConfig);
        assert.throws(() => check(changed), /missing exact workload grant/);
      }
    } finally { disposeOwnedDirectory(directory); }
  });
}

for (const profile of ['models']) test(`actual production Compose ${profile} keeps exact receiver/caller grants`, {
  skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1',
}, () => {
  const directory = mkdtempSync(join(tmpdir(), 'pinvou-workload-grants-'));
  const environmentKeys = ['HOME', 'USERPROFILE', 'DOCKER_CONFIG'];
  const prior = Object.fromEntries(environmentKeys.map(key => [key, process.env[key]]));
  let fixture;
  try {
    const dockerConfig = join(directory, 'docker-config'); mkdirSync(dockerConfig);
    writeFileSync(join(dockerConfig, 'config.json'), '{"auths":{}}\n');
    Object.assign(process.env, { HOME: directory, USERPROFILE: directory, DOCKER_CONFIG: dockerConfig });
    fixture = currentRenderFixture('compose', profile);
    const selected = bindings;
    const actual = () => {
      const rules = {}, subjects = { ...fixture.subjects };
      for (const workload of workloads(fixture.render, fixture.render.secrets)) {
        if (!['billing-service', 'token-gateway'].includes(workload.id)) continue;
        const env = workload.environment;
        if (env.WORKLOAD_IDENTITY_RULES_JSON) rules[workload.id] = JSON.parse(env.WORKLOAD_IDENTITY_RULES_JSON);
        if (env.WORKLOAD_IDENTITY_AUDIENCE) assert.equal(env.WORKLOAD_IDENTITY_AUDIENCE, audience);
        subjects[workload.id] = fixture.subjects[workload.id];
      }
      return { rules, subjects };
    };
    check(actual(), selected);
    const original = fixture.raw.TOKEN_GATEWAY_PRIVATE_ENV_FILE.WORKLOAD_IDENTITY_RULES_JSON;
    const binding = bindings[0];
    const rules = JSON.parse(original); const executor = rules.find(rule => rule.service === binding.caller);
    assert.ok(executor.permissions.includes(binding.permission));
    executor.permissions = executor.permissions.filter(permission => permission !== binding.permission);
    fixture.updatePrivate('TOKEN_GATEWAY_PRIVATE_ENV_FILE', 'WORKLOAD_IDENTITY_RULES_JSON', JSON.stringify(rules));
    fixture.rerender();
    assert.throws(() => check(actual(), selected), /missing exact workload grant/);
  } finally {
    if (fixture) {
      const target = resolve(fixture.directory);
      assert.equal(dirname(target), resolve(tmpdir()));
      assert.ok(basename(target).startsWith('pinvou-public-render-'));
      assert.ok(lstatSync(target).isDirectory() && !lstatSync(target).isSymbolicLink());
      fixture.dispose();
    }
    for (const key of environmentKeys) if (prior[key] === undefined) delete process.env[key]; else process.env[key] = prior[key];
    disposeOwnedDirectory(directory);
  }
});
