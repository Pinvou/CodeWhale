import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync, rmSync, readdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';
import { spawnSync } from 'node:child_process';
import { developmentNATSFixture } from '../fixtures/development-nats.mjs';
import { developmentNATSConfiguration, newDevelopmentNATSProject, removeDevelopmentNATSProject, retainDevelopmentNATSEvidence, sha256 } from '../../../scripts/account-platform-dev-nats.mjs';
import { guardExistingDevelopmentNATSStorage } from '../../../scripts/account-platform-dev-nats-storage.mjs';
import { initializeFreshDevelopmentNATS, developmentTestPaymentEnvironment } from '../../../scripts/account-platform-dev-nats-runner.mjs';
function fixture(action) {
  const parent = path.resolve(tmpdir()), directory = mkdtempSync(path.join(parent, 'pinvou-dev-nats-contract-'));
  try { return action(developmentNATSFixture(directory)); }
  finally { assert.equal(path.dirname(path.resolve(directory)), parent); assert.ok(path.basename(directory).startsWith('pinvou-dev-nats-contract-')); rmSync(directory, { recursive: true }); }
}

test('development declaration supplies isolated TLS and five separate explicit identities without issuing privileges', () => fixture(f => {
  const result = developmentNATSConfiguration(f.environment, { fresh: true });
  assert.equal(result.DEV_NATS_UID, String(f.config.uid));
  assert.equal(result.DEV_NATS_PUBLIC_KEYS_SHA256, f.config.currentPublicKeysSHA256);
  assert.equal(Object.keys(result).filter(key => /_DIRECTORY$/.test(key) && /OUTBOX|EXECUTOR|APPROVAL_RELAY|OPERATOR/.test(key)).length, 5);
  assert.equal(result.DEV_NATS_RECEIPT_SHA256, '');
  assert.equal(result.NATS_URL, undefined); // Endpoint/roles cannot be selected by the declaration.
}));

test('missing, wrong-hash, reused, overlapping and unknown inputs fail closed without reflecting material', () => fixture(f => {
  const original = structuredClone(f.config);
  for (const change of [
    c => { delete c.caFile; }, c => { c.authorizationSHA256 = '0'.repeat(64); },
    c => { c.seeds['billing-command-executor'] = c.seeds['account-outbox-relay']; },
    c => { c.manifestFile = c.currentPublicKeysFile; }, c => { c.NATS_URL = 'nats://anonymous:4222'; },
    c => { c.uid = 0; }, c => { c.serverKeyFile = 'relative-secret.key'; },
    c => { c.seeds['unknown-role'] = 'sensitive-seed'; },
  ]) {
    f.config = structuredClone(original); change(f.config); writeFileSync(f.declaration, JSON.stringify(f.config));
    assert.throws(() => developmentNATSConfiguration(f.environment, { fresh: true }), error => error.message === 'DEV_NATS_MATERIALS_INVALID: see account-platform/deploy/runbooks/development-nats.md');
  }
}));

test('existing dev up requires an independently trusted receipt, while down does not read expired/missing materials', () => fixture(f => {
  assert.throws(() => developmentNATSConfiguration(f.environment));
  const raw = Buffer.from(JSON.stringify({ schema: 'approval-consumer-cutover-receipt-v1', schema_version: 1, identity_sha256: f.config.currentPublicKeysSHA256, manifest_sha256: f.config.manifestSHA256, stream: 'ACCOUNT_PLATFORM', control: 'ADMIN_APPROVAL', captured_mode: 'QUIESCING', captured_relay_state: 'FENCED', control_version: '3', approval_epoch: '2', authorization_relay_epoch: '2', captured_at: new Date().toISOString(), high_watermark: '0', billing_starts: [{ durable: 'fixture-only-durable', start_sequence: '1' }] })); writeFileSync(f.config.receiptFile, raw); f.config.receiptSHA256 = sha256(raw); f.save();
  assert.equal(developmentNATSConfiguration(f.environment).DEV_NATS_RECEIPT_SHA256, sha256(raw));
  rmSync(f.config.caFile); rmSync(f.config.receiptFile);
  assert.doesNotThrow(() => developmentNATSConfiguration(f.environment, { cleanup: true }));
  assert.throws(() => developmentNATSConfiguration(f.environment));
}));

function freshDriver(f, { occupied = false, occupiedNetwork = false, badReceipt = false, failAction = '', remote = false } = {}) {
  const owned = newDevelopmentNATSProject(developmentNATSConfiguration(f.environment, { fresh: true }));
  const commands = [];
  const capture = (command, args) => {
    commands.push(args);
    if (args[0] === 'context') return JSON.stringify(remote ? 'tcp://remote.example.invalid:2376' : 'unix:///var/run/docker.sock');
    if (args[0] === 'network' && occupiedNetwork) return 'controlled-existing-network';
    if (args[0] !== 'compose') return occupied ? 'existing-resource' : '';
    const action = args[args.indexOf('-action') + 1];
    if (action === failAction) throw new Error('controlled operator failure');
    const version = Number(args[args.indexOf('-expected-version') + 1]);
    if (action === 'initialize-consumers') writeFileSync(path.join(owned.directory, 'receipt', 'receipt.json'), JSON.stringify({ schema: 'approval-consumer-cutover-receipt-v1', schema_version: 1, identity_sha256: f.config.currentPublicKeysSHA256, manifest_sha256: f.config.manifestSHA256, stream: 'ACCOUNT_PLATFORM', control: 'ADMIN_APPROVAL', captured_at: new Date().toISOString(), captured_mode: badReceipt ? 'LEGACY' : 'QUIESCING', captured_relay_state: 'FENCED', control_version: '3', approval_epoch: '2', authorization_relay_epoch: '2', high_watermark: '4', billing_starts: [{ durable: 'fixture-only-durable', start_sequence: '5' }] }), { mode: 0o600 });
    return JSON.stringify({ ControlID: 'ADMIN_APPROVAL', Version: version + (['quiesce', 'fence'].includes(action) ? 1 : 0), Mode: action === 'status' ? 'LEGACY' : 'QUIESCING', AuthorizationRelayState: ['status', 'quiesce'].includes(action) ? 'RUNNING' : 'FENCED', ApprovalEpoch: action === 'status' ? 1 : 2, AuthorizationRelayEpoch: ['status', 'quiesce'].includes(action) ? 1 : 2 });
  };
  return { owned, commands, capture, run: (_, args) => commands.push(args) };
}

test('owned test initializer orders actual CLI calls before Billing, preserves fences, and binds the produced receipt', () => fixture(f => {
  const d = freshDriver(f);
  try {
    initializeFreshDevelopmentNATS({ ...d, args: ['compose', '-f', 'base', '-f', 'fixed-dev-overlay'] });
    assert.equal(d.owned.mayCleanCompose, true);
    assert.match(d.owned.environment.DEV_NATS_RECEIPT_SHA256, /^[a-f0-9]{64}$/);
    assert.deepEqual(d.commands.filter(args => args.includes('-action')).map(args => args[args.indexOf('-action') + 1]), ['status', 'quiesce', 'fence', 'initialize-consumers']);
    assert.ok(d.commands.some(args => args.includes('-create-only')));
    assert.ok(d.commands.every(args => !args.includes('billing-service') && !args.includes('activate') && !args.includes('resume')));
    assert.match(d.owned.project, /^pinvou-account-platform-test-\d+-[a-f0-9]{32}$/);
  } finally { removeDevelopmentNATSProject(d.owned); }
}));

test('occupied projects, failed initializer and unbound receipt never admit business startup', () => fixture(f => {
  for (const options of [{ occupied: true }, { occupiedNetwork: true }, { remote: true }, { failAction: 'fence' }, { badReceipt: true }]) {
    const d = freshDriver(f, options);
    try {
      assert.throws(() => initializeFreshDevelopmentNATS({ ...d, args: ['compose'] }));
      assert.equal(d.owned.environment.DEV_NATS_RECEIPT_SHA256, '');
      if (options.occupied || options.occupiedNetwork || options.remote) assert.equal(d.owned.mayCleanCompose, undefined);
      assert.ok(d.commands.every(args => !args.includes('billing-service')));
    } finally { removeDevelopmentNATSProject(d.owned); }
  }
}));

test('unified up/test and fault restart retain the fixed dev overlay; original acceptance assertions remain executable', () => {
  const runner = readFileSync(new URL('../../../scripts/account-platform.mjs', import.meta.url), 'utf8');
  const fault = readFileSync(new URL('../integration/compose-fault.mjs', import.meta.url), 'utf8');
  assert.match(runner, /initializeFreshDevelopmentNATS/);
  assert.equal([...runner.matchAll(/'-f', composeFile, '-f', composeNATSFile/g)].length, 3);
  assert.match(fault, /path\.resolve\(composeNATSFile\)/);
  assert.match(fault, /'-f', composeFile, '-f', composeNATSFile/);
  for (const name of ['compose-smoke.mjs', 'compose-acceptance.mjs', 'compose-fault.mjs']) assert.ok(runner.includes(name));
  assert.doesNotMatch(runner, /SKIP.*ACCEPTANCE|test\.skip/);
});


test('dev NATS retains pure broker monitoring only inside the original internal network', () => {
  const production = readFileSync(new URL('../../deploy/production/nats.conf', import.meta.url), 'utf8');
  const development = readFileSync(new URL('../../deploy/compose/nats.dev.conf', import.meta.url), 'utf8');
  assert.equal(development, production.replace('http: "127.0.0.1:8222"', 'http: "0.0.0.0:8222"'));
  assert.match(production, /http: "127\.0\.0\.1:8222"/);
  const base = readFileSync(new URL('../../deploy/compose/compose.yaml', import.meta.url), 'utf8');
  const overlay = readFileSync(new URL('../../deploy/compose/compose.dev-nats.yaml', import.meta.url), 'utf8');
  assert.match(base, /backend:\n    internal: true/);
  assert.match(base, /"key":"event-bus","kind":"http","url":"http:\/\/nats:8222\/healthz"/);
  assert.doesNotMatch(overlay, /ports:|SYSTEM_PROBES_JSON/);
});


test('cleanup refuses changed ownership and retains only bounded public initialization evidence', () => fixture(f => {
  const d = freshDriver(f);
  let evidence;
  try {
    initializeFreshDevelopmentNATS({ ...d, args: ['compose'] });
    evidence = retainDevelopmentNATSEvidence(d.owned, false);
    assert.deepEqual(readdirSync(evidence).sort(), ['receipt.json', 'result.json']);
    assert.equal(JSON.parse(readFileSync(path.join(evidence, 'result.json'))).formally_activated, false);
    assert.equal(sha256(readFileSync(path.join(evidence, 'receipt.json'))), d.owned.environment.DEV_NATS_RECEIPT_SHA256);
    writeFileSync(path.join(d.owned.directory, 'owner.json'), JSON.stringify({ project: 'different-project' }));
    assert.throws(() => removeDevelopmentNATSProject(d.owned));
    writeFileSync(path.join(d.owned.directory, 'owner.json'), JSON.stringify({ project: d.owned.project }));
  } finally {
    removeDevelopmentNATSProject(d.owned);
    if (evidence) { assert.equal(path.dirname(path.resolve(evidence)), d.owned.parent); assert.ok(path.basename(evidence).startsWith('pinvou-account-nats-evidence-')); rmSync(evidence, { recursive: true }); }
  }
}));

test('manual developer operations do not expose formal activation or arbitrary CLI arguments', () => {
  for (const action of ['activate', 'resume', '--unknown']) {
    const result = spawnSync(process.execPath, [path.resolve(import.meta.dirname, '../../../scripts/account-platform.mjs'), 'dev', 'nats', action, '--expected-version', '3'], { encoding: 'utf8', shell: false });
    assert.equal(result.status, 1);
    assert.match(result.stderr, /usage: dev nats/);
    assert.doesNotMatch(result.stderr, /Docker|password|nkey.seed/);
  }
});


test('parsed dev overlay binds current runtime identities and isolates all operator authority', () => {
  const { services } = YAML.parse(readFileSync(new URL('../../deploy/compose/compose.dev-nats.yaml', import.meta.url), 'utf8'));
  assert.deepEqual(Object.keys(services).sort(), ['account-event-relay', 'billing-event-relay', 'billing-service', 'dev-approval-resources', 'dev-event-resources', 'nats']);
  for (const [service, variable] of [['account-event-relay', 'ACCOUNT_OUTBOX_RELAY'], ['billing-event-relay', 'BILLING_OUTBOX_RELAY'], ['billing-service', 'BILLING_COMMAND_EXECUTOR']]) {
    assert.equal(services[service].environment.NATS_URL, 'tls://nats:4222');
    assert.equal(services[service].environment.NATS_CREDENTIALS_FILE, '');
    const privateMount = services[service].volumes.find(volume => volume.target === '/run/pinvou/nats');
    assert.ok(privateMount.source.includes(`DEV_NATS_${variable}_DIRECTORY:`));
    assert.equal(privateMount.read_only, true);
    assert.ok(services[service].volumes.every(volume => volume.bind.create_host_path === false));
    assert.ok(services[service].volumes.every(volume => !volume.source.includes('EVENT_RESOURCE_OPERATOR')));
  }
  assert.equal(Object.keys(services['billing-service'].environment).filter(key => key.startsWith('BILLING_APPROVAL_CONSUMER_')).length, 6);
  assert.ok(services['account-event-relay'].volumes.find(volume => volume.target === '/run/pinvou/approval-nats').source.includes('ACCOUNT_APPROVAL_RELAY'));
  for (const name of ['dev-event-resources', 'dev-approval-resources']) {
    assert.deepEqual(services[name].profiles, ['dev-nats-operations']);
    assert.equal(services[name].restart, 'no');
    assert.equal(services[name].read_only, true);
    assert.equal(services[name].volumes.filter(volume => volume.source.includes('EVENT_RESOURCE_OPERATOR')).length, 1);
    assert.equal(services[name].environment.NATS_URL, 'tls://nats:4222');
  }
  const initializer = services['dev-approval-resources'];
  assert.deepEqual(initializer.command, ['-action', 'status', '-expected-version', '1']);
  assert.match(initializer.environment.APPROVAL_INITIALIZER_DATABASE_URL, /^postgresql:\/\/account_migrator:/);
  assert.equal(initializer.volumes.find(volume => volume.target === '/run/pinvou/approval-receipt').read_only, undefined);
  assert.ok(services.nats.volumes.every(volume => !/SEED|RELAY|OPERATOR|CUSTODY/.test(volume.source)));
  assert.equal(services.nats.ports, undefined);
});


test('existing developer bus cannot silently switch old named data or a different bind', () => fixture(f => {
  for (const state of ['new', 'same-bind', 'old-volume', 'unlabelled-old-volume', 'volume-mounted', 'different-bind', 'uninspectable', 'remote']) {
    const calls = [];
    const capture = (_, args) => {
      calls.push(args);
      if (args[0] === 'context') return JSON.stringify(state === 'remote' ? 'ssh://remote.example.invalid' : 'unix:///var/run/docker.sock');
      if (args.includes('config')) return JSON.stringify({ name: 'controlled-dev', services: { nats: { volumes: [{ type: 'bind', target: '/data', source: f.config.dataDirectory }] } } });
      if (args[0] === 'volume') return state === 'old-volume' || state === 'unlabelled-old-volume' && args.some(value => value.startsWith('name=')) ? 'controlled-dev_nats-data' : '';
      if (args.includes('ps')) return state === 'new' ? '' : 'a'.repeat(64);
      if (args[0] === 'inspect') {
        if (state === 'uninspectable') throw Error('controlled inspect failure');
        return JSON.stringify([{ Type: state === 'volume-mounted' ? 'volume' : 'bind', Destination: '/data', Source: state === 'different-bind' ? path.join(f.config.dataDirectory, 'different') : f.config.dataDirectory }]);
      }
      throw Error('unexpected command');
    };
    const run = () => guardExistingDevelopmentNATSStorage({ args: ['compose'], environment: { DEV_NATS_DATA_DIRECTORY: f.config.dataDirectory }, capture });
    if (['new', 'same-bind'].includes(state)) assert.doesNotThrow(run);
    else assert.throws(run, /DEV_NATS_EXISTING_STORAGE_UNPROVEN/);
    assert.ok(calls.every(args => !args.includes('up') && !args.includes('down') && !args.includes('rm')));
  }
  const runner = readFileSync(new URL('../../../scripts/account-platform.mjs', import.meta.url), 'utf8');
  assert.equal([...runner.matchAll(/guardExistingDevelopmentNATSStorage\(\{ args, environment/g)].length, 2);
}));


test('only fresh simulator acceptance enables the existing refund fixture and explicit conflicts fail before startup', () => {
  assert.deepEqual(developmentTestPaymentEnvironment(['compose-smoke.mjs'], {}), {});
  assert.deepEqual(developmentTestPaymentEnvironment(['compose-smoke.mjs', 'compose-fault.mjs'], {}), { BILLING_PAYMENT_PROVIDER: 'development_simulator' });
  assert.deepEqual(developmentTestPaymentEnvironment(['compose-smoke.mjs', 'compose-acceptance.mjs'], {}), { BILLING_PAYMENT_PROVIDER: 'development_simulator', BILLING_NORMAL_REFUND_CREATION_ENABLED: 'true' });
  assert.deepEqual(developmentTestPaymentEnvironment(['compose-acceptance.mjs'], { BILLING_PAYMENT_PROVIDER: 'development_simulator', BILLING_NORMAL_REFUND_CREATION_ENABLED: 'true' }), { BILLING_PAYMENT_PROVIDER: 'development_simulator', BILLING_NORMAL_REFUND_CREATION_ENABLED: 'true' });
  for (const value of ['false', '', 'invalid']) assert.throws(() => developmentTestPaymentEnvironment(['compose-acceptance.mjs'], { BILLING_NORMAL_REFUND_CREATION_ENABLED: value }), /DEV_NATS_TEST_NORMAL_REFUND_REQUIRED/);
  for (const file of ['compose-acceptance.mjs', 'compose-fault.mjs']) {
    assert.throws(() => developmentTestPaymentEnvironment([file], { BILLING_PAYMENT_PROVIDER: 'h3c_mall' }), /DEV_NATS_TEST_SIMULATOR_REQUIRED/);
    const environment = { ...process.env, BILLING_PAYMENT_PROVIDER: 'h3c_mall' };
    const result = spawnSync(process.execPath, [path.resolve(import.meta.dirname, '../../../scripts/account-platform.mjs'), 'test', file === 'compose-acceptance.mjs' ? 'e2e' : 'fault'], { env: environment, encoding: 'utf8', shell: false });
    assert.equal(result.status, 1); assert.match(result.stderr, /DEV_NATS_TEST_SIMULATOR_REQUIRED/);
    assert.doesNotMatch(result.stderr, /Docker is required|COMPOSE_TEST_CONFIGURATION_REQUIRED/);
  }
  const result = spawnSync(process.execPath, [path.resolve(import.meta.dirname, '../../../scripts/account-platform.mjs'), 'test', 'e2e'], { env: { ...process.env, BILLING_PAYMENT_PROVIDER: 'development_simulator', BILLING_NORMAL_REFUND_CREATION_ENABLED: 'false' }, encoding: 'utf8', shell: false });
  assert.equal(result.status, 1); assert.match(result.stderr, /DEV_NATS_TEST_NORMAL_REFUND_REQUIRED/);
});
