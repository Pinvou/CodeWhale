import assert from 'node:assert/strict';
import { cpSync, lstatSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, dirname, isAbsolute, join, relative, resolve } from 'node:path';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import test from 'node:test';
import YAML from 'yaml';

const root = fileURLToPath(new URL('../../../', import.meta.url));
const chart = join(root, 'account-platform/deploy/helm/account-platform');
const composeBase = join(root, 'account-platform/deploy/compose');
const release = 'closure-contract-release';
const namespace = 'closure-contract-namespace';
// Independent anchors: the closure runbook's exact receiver contract and the
// corresponding installed receiver Require, rather than another config copy.
const bindings = [
  { receiver: 'account-service', caller: 'billing-service', permission: 'internal.account.closure.read', retired: 'internal.account-closure-state.read', source: 'account-platform/internal/account/service.go' },
  { receiver: 'billing-service', caller: 'account-service', permission: 'internal.billing.account-closure.assess', retired: 'internal.billing.account-closure-assessment.read', source: 'account-platform/internal/billing/service.go' },
  { receiver: 'billing-service', caller: 'account-service', permission: 'internal.billing.account-closure.isolate', retired: 'internal.billing.account-closure-grants.isolate', source: 'account-platform/internal/billing/service.go' },
  { receiver: 'billing-service', caller: 'token-gateway', permission: 'internal.billing.closed-grant.reclaim', retired: 'internal.billing.account-closed-grant.reclaim', source: 'account-platform/internal/billing/organization_routes.go' },
];
const ownerPermission = 'internal.account.token-owner-authority.check';
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
    && !child.startsWith('..') && !isAbsolute(child) && basename(target).startsWith('pinvou-closure-render-'),
  'recursive cleanup must remain inside the owned temporary prefix');
  assert.ok(lstatSync(target).isDirectory() && !lstatSync(target).isSymbolicLink());
  rmSync(target, { recursive: true, force: true });
}

test('closure grants follow the independent runbook and owner retains its frozen contract', () => {
  const documented = new Set([...read('account-platform/deploy/runbooks/account-closure.md').matchAll(/`(internal\.[^`]+)`/g)]
    .flatMap(([, value]) => {
      const [first, ...suffixes] = value.split('/');
      return [first, ...suffixes.map(suffix => first.slice(0, first.lastIndexOf('.') + 1) + suffix)];
    }));
  for (const binding of bindings) {
    assert.ok(documented.has(binding.permission), `closure runbook does not specify ${binding.permission}`);
    assert.ok(read(binding.source).includes(`serviceauth.Require("${binding.permission}")`), `receiver no longer enforces ${binding.permission}`);
  }
  for (const path of ['account-platform/docs/requirements.md', 'account-platform/docs/architecture.md']) {
    assert.ok(read(path).includes('/internal/v1/token-account-authority/snapshot'));
  }
  assert.ok(read('account-platform/docs/requirements.md').includes(ownerPermission));
  assert.ok(read('account-platform/internal/billing/organization_routes.go')
    .includes('s.requireHumanDelegation("org.grants.reclaim_closed"'), 'internal reclaim scope must not replace human delegation');
});

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
  for (const name of ['account-service', 'billing-service', 'token-gateway']) {
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
    assert.equal(typeof value?.value, 'string');
    rules[name] = JSON.parse(value.value);
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
  for (const name of ['account-service', 'billing-service', 'token-gateway']) {
    assert.ok(config.services[name]);
    rules[name] = JSON.parse(config.services[name].environment.WORKLOAD_IDENTITY_RULES_JSON);
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

function check(actual) {
  for (const [receiver, rules] of Object.entries(actual.rules)) {
    assert.equal(new Set(rules.map(rule => rule.service)).size, rules.length, `duplicate caller on ${receiver}`);
    for (const rule of rules) {
      assert.equal(new Set(rule.permissions).size, rule.permissions.length, 'duplicate exact permission');
      assert.ok(rule.permissions.every(permission => !permission.includes('*')), 'wildcard must not replace exact grants');
    }
  }
  for (const binding of bindings) {
    const rule = ruleFor(actual, binding.receiver, binding.caller);
    assert.ok(rule.permissions.includes(binding.permission), `missing exact closure grant ${binding.permission} for ${binding.caller} on ${binding.receiver}`);
    assert.ok(!rule.permissions.includes(binding.retired), `ineffective retired closure grant ${binding.retired}`);
  }
  const owner = ruleFor(actual, 'account-service', 'token-gateway');
  assert.ok(owner.permissions.includes(ownerPermission), 'preserve normative combined owner grant');
  assert.ok(!owner.permissions.includes('internal.token-owner-authority.read'), 'do not follow the incomplete old owner receiver');
}

function rejectChangedRenderedGrants(actual) {
  for (const mutation of [
    changed => { const rule = ruleFor(changed, 'account-service', 'billing-service'); rule.permissions = rule.permissions.filter(p => p !== bindings[0].permission); },
    changed => { const rule = ruleFor(changed, 'account-service', 'billing-service'); rule.permissions = rule.permissions.filter(p => p !== bindings[0].permission); ruleFor(changed, 'account-service', 'token-gateway').permissions.push(bindings[0].permission); },
    changed => ruleFor(changed, 'billing-service', 'account-service').permissions.push('internal.*'),
    changed => { ruleFor(changed, 'account-service', 'billing-service').subject += '-foreign'; },
    changed => ruleFor(changed, 'billing-service', 'account-service').permissions.push(bindings[1].permission),
    changed => { const rule = ruleFor(changed, 'account-service', 'token-gateway'); rule.permissions = rule.permissions.filter(p => p !== ownerPermission); rule.permissions.push('internal.token-owner-authority.read'); },
  ]) {
    const changed = structuredClone(actual);
    mutation(changed);
    assert.throws(() => check(changed), assert.AssertionError);
  }
}

for (const kind of ['helm', 'compose']) for (const native of [false, true]) {
  test(`retained ${kind} closure receiver grants actually render (${native ? 'native' : 'remote'})`, {
    skip: process.env[kind === 'helm' ? 'PINVOU_TEST_KMS_HELM' : 'PINVOU_TEST_KMS_COMPOSE'] !== '1',
  }, () => {
    const render = kind === 'helm' ? renderHelm : renderCompose;
    const directory = mkdtempSync(join(tmpdir(), 'pinvou-closure-render-'));
    try {
      const dockerConfig = join(directory, 'docker-config');
      mkdirSync(dockerConfig);
      writeFileSync(join(dockerConfig, 'config.json'), '{"auths":{}}\n');
      const actual = render(native, undefined, dockerConfig);
      check(actual);
      rejectChangedRenderedGrants(actual);
      // Each originally wrong source grant must be caught after rerendering
      // a private source copy; a renderer error cannot satisfy this.
      const input = kind === 'helm' ? join(directory, 'chart') : join(directory, 'compose.model-token.yaml');
      if (kind === 'helm') cpSync(chart, input, { recursive: true });
      const sourcePath = kind === 'helm' ? join(input, 'templates/app-deployments.yaml') : input;
      const original = kind === 'helm' ? readFileSync(sourcePath, 'utf8')
        : readFileSync(join(composeBase, 'compose.model-token.yaml'), 'utf8');
      for (const binding of bindings) {
        assert.equal(original.split(binding.permission).length, 2, 'source mutation must target one grant');
        writeFileSync(sourcePath, original.replace(binding.permission, binding.retired));
        const changed = render(native, input, dockerConfig);
        assert.throws(() => check(changed), /missing exact closure grant/);
      }
    } finally { disposeOwnedDirectory(directory); }
  });
}
