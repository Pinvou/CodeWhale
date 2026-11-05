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
const compose = join(root, 'account-platform/deploy/compose');
const read = p => readFileSync(join(root, p), 'utf8');
const expectedSources = ['account', 'billing'];
const expectedRelays = expectedSources.map(source => `${source}-event-relay`);
const missingExpression = '(count(count by (source) (pinvou_outbox_pending_events{source=~"account|billing"})) or vector(0)) < 2';
const env = container => Object.fromEntries((container.env || []).map(item => [item.name, item.value]));
const component = item => item.spec?.template?.metadata?.labels?.['app.kubernetes.io/component']
  || item.metadata?.labels?.['app.kubernetes.io/component'];

function isolatedEnvironment(directory) {
  const docker = join(directory, 'docker-config'); mkdirSync(docker); writeFileSync(join(docker, 'config.json'), '{"auths":{}}\n');
  return { ...Object.fromEntries(Object.entries(process.env).filter(([key]) => ['PATH', 'SYSTEMROOT', 'TEMP', 'TMP'].includes(key.toUpperCase()))),
    HOME: directory, USERPROFILE: directory, DOCKER_CONFIG: docker, COMPOSE_CONVERT_WINDOWS_PATHS: '1',
    HELM_CONFIG_HOME: join(directory, 'helm-config'), HELM_CACHE_HOME: join(directory, 'helm-cache'), HELM_DATA_HOME: join(directory, 'helm-data') };
}
function dispose(directory) {
  const parent = resolve(tmpdir()), target = resolve(directory), child = relative(parent, target);
  assert.ok(isAbsolute(target) && dirname(target) === parent && child !== '' && !child.startsWith('..')
    && !isAbsolute(child) && basename(target).startsWith('pinvou-relay-runtime-'));
  assert.ok(lstatSync(target).isDirectory() && !lstatSync(target).isSymbolicLink());
  rmSync(target, { recursive: true, force: true });
}
function execute(binary, args, environment) {
  const result = spawnSync(binary, args, { cwd: root, env: environment, encoding: 'utf8', timeout: 30000,
    maxBuffer: 4 * 1024 * 1024, windowsHide: true, shell: false });
  assert.equal(result.error, undefined, 'real config renderer must execute');
  assert.equal(result.status, 0, `real config renderer failed: ${result.stderr}`);
  return result.stdout;
}
function renderHelm(native, environment, directory = chart) {
  const text = execute(process.env.PINVOU_TEST_KMS_HELM_BINARY || 'helm', ['template', 'ordinary-relay-contract', directory,
    '--namespace', 'ordinary-relay-contract', '-f', join(directory, 'values-model-token-test.yaml'),
    ...(native ? ['-f', join(directory, 'values-kms-native-test.yaml')] : []), '--set', 'observability.prometheusRulesEnabled=true', '--set', 'availability.enabled=true'], environment);
  const documents = YAML.parseAllDocuments(text, { uniqueKeys: true });
  assert.ok(documents.every(document => document.errors.length === 0));
  return documents.map(document => document.toJSON()).filter(Boolean);
}
function checkProbes(raw, names) {
  const probes = JSON.parse(raw).filter(item => item.kind === 'relay');
  assert.deepEqual(probes.map(item => item.key).sort(), expectedRelays);
  for (const probe of probes) assert.equal(probe.url, `http://${names[probe.key]}:9090`);
}
function checkHelm(items) {
  const deployments = items.filter(item => item.kind === 'Deployment');
  const relays = deployments.filter(item => env(item.spec.template.spec.containers[0]).EVENT_SOURCE != null);
  assert.deepEqual(relays.map(item => env(item.spec.template.spec.containers[0]).EVENT_SOURCE).sort(), expectedSources,
    'ordinary relay sources must be the two installed runtime sources');
  const names = {};
  for (const item of relays) {
    const name = component(item); assert.ok(expectedRelays.includes(name)); names[name] = item.metadata.name;
    const pod = item.spec.template.spec, container = pod.containers[0];
    assert.ok(items.some(resource => resource.kind === 'ServiceAccount' && resource.metadata.name === pod.serviceAccountName));
    assert.ok(items.some(resource => resource.kind === 'Service' && component(resource) === name));
    assert.ok(items.some(resource => resource.kind === 'PodDisruptionBudget' && resource.spec.selector.matchLabels['app.kubernetes.io/component'] === name));
    assert.equal(container.readinessProbe.httpGet.path, '/health/ready');
    assert.equal(container.livenessProbe.httpGet.path, '/health/live');
    assert.equal(pod.automountServiceAccountToken, false);
  }
  for (const kind of ['Deployment', 'Service', 'ServiceAccount', 'PodDisruptionBudget']) {
    assert.ok(!items.some(item => item.kind === kind && (component(item) === 'token-event-relay'
      || item.metadata.name.endsWith('-token-event-relay'))), `retired ordinary relay ${kind} remains`);
  }
  for (const policy of items.filter(item => item.kind === 'NetworkPolicy')) {
    assert.ok(!JSON.stringify(policy.spec).includes('token-event-relay'), 'retired ordinary relay remains a network caller');
  }
  assert.ok(deployments.some(item => component(item) === 'token-gateway'));
  assert.ok(deployments.some(item => component(item) === 'token-command-executor'));
  checkProbes(env(deployments.find(item => component(item) === 'token-portal').spec.template.spec.containers[0]).SYSTEM_PROBES_JSON, names);
  const rules = items.filter(item => item.kind === 'PrometheusRule').flatMap(item => item.spec.groups).flatMap(group => group.rules)
    .filter(rule => rule.alert === 'PinvouAccountPlatformEventRelayMissing');
  assert.equal(rules.length, 1); assert.equal(rules[0].expr, missingExpression, 'missing relay alert must require exactly two supported sources');
  assert.equal(rules[0].for, '5m'); assert.equal(rules[0].labels.severity, 'p1');
}
function renderCompose(native, environment, files = ['compose.yaml', 'compose.model-token.yaml']) {
  const standalone = process.env.PINVOU_TEST_KMS_COMPOSE_BINARY;
  return JSON.parse(execute(standalone || 'docker', [...(standalone ? [] : ['compose']),
    '--env-file', join(compose, '.env.example'), '--env-file', join(compose, 'model-token.env.example'),
    '--env-file', join(root, 'account-platform/test/fixtures/compose-render.env'),
    ...(native ? ['--env-file', join(compose, 'kms-native.env.example')] : []),
    ...files.flatMap(file => ['-f', isAbsolute(file) ? file : join(compose, file)]),
    ...(native ? ['-f', join(compose, 'compose.kms-native.yaml')] : []),
    '--profile', 'model-token', '--profile', 'governance-operations', '--profile', 'real-newapi', 'config', '--format', 'json'], environment));
}
function checkCompose(config) {
  const services = config.services;
  assert.ok(!Object.hasOwn(services, 'token-event-relay'), 'retired ordinary relay service remains');
  assert.deepEqual(Object.values(services).map(service => service.environment?.EVENT_SOURCE).filter(Boolean).sort(), expectedSources,
    'ordinary relay sources must be the two installed runtime sources');
  for (const name of expectedRelays) assert.equal(services[name].environment.EVENT_SOURCE, name.split('-')[0]);
  assert.ok(services['token-gateway']);
  checkProbes(services['token-portal'].environment.SYSTEM_PROBES_JSON, Object.fromEntries(expectedRelays.map(name => [name, name])));
}

test('ordinary relay inventory matches installed runtime and preserves retired historical resources', () => {
  const main = read('account-platform/cmd/event-relay/main.go');
  assert.ok(main.includes('source != "account" && source != "billing"'));
  for (const path of ['account-platform/deploy/runbooks/backup-restore.md', 'account-platform/deploy/compose/README.md', 'account-platform/docs/architecture.md']) {
    assert.ok(!read(path).includes('三个 Relay'), 'current delivery instructions must not require the retired third relay');
  }
  const policy = JSON.parse(read('account-platform/deploy/ha-policy.json'));
  assert.deepEqual(policy.required_replicated_workloads.filter(name => name.endsWith('-event-relay')).sort(), expectedRelays);
  for (const name of ['account-service', 'billing-service', 'token-gateway', 'token-portal', 'keycloak', 'new-api']) assert.ok(policy.required_replicated_workloads.includes(name));
  const historical = JSON.parse(read('account-platform/deploy/authorization-entrypoints.json'));
  assert.ok(JSON.stringify(historical).includes('account|billing|token.>'), 'historical authorization inventory must not be deleted');
  const fixtures = YAML.parse(read('account-platform/test/observability/event-relay-missing.test.yaml'));
  assert.equal(fixtures.tests.length, 8);
  for (const fixture of fixtures.tests) {
    const sources = new Set((fixture.input_series || []).map(item => item.series.match(/source="([^"]+)"/)[1]));
    const expectedAlert = fixture.alert_rule_test[0].eval_time === '6m' && !expectedSources.every(source => sources.has(source));
    assert.equal(fixture.alert_rule_test[0].exp_alerts.length > 0, expectedAlert, fixture.name);
  }
});
test('current rollout instructions separate retired Token writer fencing from active ordinary relays', () => {
  const composeInstructions = read('account-platform/deploy/compose/README.md');
  assert.ok(composeInstructions.includes('本代 gateway 使用 `TOKEN_GATEWAY_RELEASE_DATABASE_URL`')
    && composeInstructions.includes('credential 使用不同本代 `TOKEN_CREDENTIAL_RELEASE_DATABASE_URL`')
    && composeInstructions.includes('不配置或重启普通 Token Relay'), 'Compose must not allocate the current gateway database identity to a live Token Relay');
  assert.ok(composeInstructions.includes('历史 Token Relay') && composeInstructions.includes('旧角色 NOLOGIN/fence 和存量会话清退'),
    'Compose must retain historical Token writer login and session fencing');
  assert.ok(!composeInstructions.includes('gateway 与 Token Relay 共用本代'));
  const helmInstructions = read('account-platform/deploy/helm/account-platform/README.md');
  assert.ok(helmInstructions.includes('No ordinary Token Relay is deployed or restarted.')
    && helmInstructions.includes('Historical Token') && helmInstructions.includes('NOLOGIN/session fence'),
    'Helm must distinguish a retired Token writer fence from a current deployment');
  assert.ok(!helmInstructions.includes('The Token Relay uses the same release-scoped gateway database Secret'));
  const release = read('account-platform/deploy/runbooks/model-token-release.md');
  assert.ok(release.includes('本代只启用 gateway/credential 两个数据库身份，普通 Token Relay 不部署或重启。')
    && release.includes('`--candidate-roles` 恰为 credential/gateway 两角色，不含普通 Token Relay'),
    'G2 and candidate role instructions must not restart the retired Token Relay');
  assert.ok(release.includes('历史 Token Relay') && release.includes('NOLOGIN/会话 fence')
    && release.includes('不删除角色、成员关系、历史数据或密码') && release.includes('--terminate-sessions'),
    'release instructions must retain the historical writer inventory and login/session fences');
  assert.ok(!release.includes('Token Relay 与 gateway 同代 writer 共同停止/fence/恢复')
    && !release.includes('gateway/credential/Relay') && !release.includes('本代 writer/Relay'));
  const ha = read('account-platform/deploy/runbooks/high-availability-failover.md');
  assert.ok(ha.includes('Account/Billing 两个普通 Relay') && !ha.includes('三类 Relay'),
    'current HA instructions must require only the two live ordinary Relay sources');
  const requirements = read('account-platform/docs/requirements.md');
  assert.ok(requirements.includes('既有 Token Relay 描述仅保留历史迁移上下文')
    && requirements.includes('G2 历史交付记录不要求当前部署普通 Token Relay'),
    'historical requirements must identify the current ordinary notification retirement boundary');
});
for (const native of [false, true]) test(`actual Helm ordinary relay runtime, probes and missing-source alert (${native ? 'native' : 'remote'})`, {
  skip: process.env.PINVOU_TEST_KMS_HELM !== '1',
}, () => {
  const directory = mkdtempSync(join(tmpdir(), 'pinvou-relay-runtime-'));
  try {
    const environment = isolatedEnvironment(directory); checkHelm(renderHelm(native, environment));
    const copy = join(directory, 'chart'); cpSync(chart, copy, { recursive: true });
    const path = join(copy, 'templates/event-relays.yaml'), original = readFileSync(path, 'utf8');
    assert.ok(original.includes('list "account" "billing"'));
    writeFileSync(path, original.replace('list "account" "billing"', 'list "account" "billing" "token"'));
    assert.throws(() => checkHelm(renderHelm(native, environment, copy)), /ordinary relay sources/);
    writeFileSync(path, original);
    const alertPath = join(copy, 'templates/prometheus-rules.yaml'), rules = readFileSync(alertPath, 'utf8');
    assert.ok(rules.includes('source=~"account|billing"'));
    writeFileSync(alertPath, rules.replace('source=~"account|billing"', 'source=~"account|billing|token"').replace('or vector(0)) < 2', 'or vector(0)) < 3'));
    assert.throws(() => checkHelm(renderHelm(native, environment, copy)), /missing relay alert/);
  } finally { dispose(directory); }
});
for (const mode of ['base', 'remote', 'native']) test(`actual Compose ordinary relay runtime and Portal probes (${mode})`, {
  skip: process.env.PINVOU_TEST_KMS_COMPOSE !== '1',
}, () => {
  const directory = mkdtempSync(join(tmpdir(), 'pinvou-relay-runtime-'));
  try {
    const environment = isolatedEnvironment(directory);
    const files = mode === 'base' ? ['compose.yaml'] : ['compose.yaml', 'compose.model-token.yaml'];
    checkCompose(renderCompose(mode === 'native', environment, files));
    const retired = join(directory, 'retired-runtime.yaml');
    writeFileSync(retired, 'services:\n  token-event-relay:\n    image: controlled-retired-event-relay\n    environment: { EVENT_SOURCE: token }\n');
    assert.throws(() => checkCompose(renderCompose(mode === 'native', environment, [...files, retired])), /retired ordinary relay/);
  } finally { dispose(directory); }
});
