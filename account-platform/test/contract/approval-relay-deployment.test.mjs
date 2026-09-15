import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import YAML from 'yaml';
import { checkProductionBusiness } from '../../../scripts/account-platform-business-compose-check.mjs';
import { businessComposeFixture, interpolateBusinessFixtureScalar } from '../helpers/business-compose-fixture.mjs';

const read = name => readFileSync(new URL(`../../${name}`, import.meta.url), 'utf8');

test('production previous identity requires explicit paired configuration without a fallback', () => {
  const keys = ['APPROVAL_RELAY_NATS_PREVIOUS_PUBLIC_KEYS_FILE', 'APPROVAL_RELAY_NATS_PREVIOUS_PUBLIC_KEYS_SHA256'];
  const template = YAML.parse(read('deploy/production/compose.business.yaml'), { merge: true }).services['account-event-relay'].environment;
  for (const key of keys) {
    assert.ok(template[key].startsWith('${' + key + '?'));
    assert.throws(() => interpolateBusinessFixtureScalar(template[key], {}));
    assert.throws(() => interpolateBusinessFixtureScalar(template[key], { [key]: undefined }));
    assert.equal(interpolateBusinessFixtureScalar(template[key], { [key]: '' }), '');
  }
  const valid = {
    [keys[0]]: '/var/run/public/pinvou/nats-authorization/previous-v1.json',
    [keys[1]]: 'a'.repeat(64),
  };
  const fixture = businessComposeFixture();
  checkProductionBusiness(fixture.config, fixture.inventory);
  for (const key of keys) {
    assert.equal(fixture.config.services['account-event-relay'].environment[key], '');
    const half = structuredClone(fixture.config);
    half.services['account-event-relay'].environment[key] = valid[key];
    assert.throws(() => checkProductionBusiness(half, fixture.inventory));
    const missing = structuredClone(fixture.config);
    delete missing.services['account-event-relay'].environment[key];
    assert.throws(() => checkProductionBusiness(missing, fixture.inventory));
  }
  for (const key of keys) {
    const rendered = interpolateBusinessFixtureScalar(template[key], valid);
    assert.equal(rendered, valid[key]);
    fixture.config.services['account-event-relay'].environment[key] = rendered;
  }
  checkProductionBusiness(fixture.config, fixture.inventory);
  for (const [key, value] of [[keys[0], '/var/run/secrets/pinvou/approval-nats/client.creds'], [keys[1], 'invalid-digest']]) {
    const invalid = structuredClone(fixture.config);
    invalid.services['account-event-relay'].environment[key] = value;
    assert.throws(() => checkProductionBusiness(invalid, fixture.inventory));
  }
});

test('production gate rejects approval identity reuse, weakened TLS and mount leakage', () => {
  for (const mutate of [
    services => { services['account-event-relay'].environment.APPROVAL_RELAY_DATABASE_URL = services['account-event-relay'].environment.DATABASE_URL; },
    services => { services['account-event-relay'].environment.APPROVAL_RELAY_DATABASE_URL = services['account-event-relay'].environment.APPROVAL_RELAY_DATABASE_URL.replace('sslmode=verify-full', 'sslmode=disable'); },
    services => { services['billing-event-relay'].environment.APPROVAL_RELAY_DATABASE_URL = services['account-event-relay'].environment.APPROVAL_RELAY_DATABASE_URL; },
    services => { services['account-event-relay'].environment.APPROVAL_RELAY_NATS_NKEY_SEED_FILE = services['account-event-relay'].environment.NATS_NKEY_SEED_FILE; },
    services => { delete services['account-event-relay'].environment.APPROVAL_RELAY_DATABASE_URL; },
    services => { services['account-event-relay'].volumes.find(volume => volume.target === '/var/run/secrets/pinvou/approval-nats').read_only = false; },
    services => {
      const volumes = services['account-event-relay'].volumes;
      volumes.find(volume => volume.target === '/var/run/secrets/pinvou/approval-nats').source = volumes.find(volume => volume.target === '/var/run/secrets/pinvou/nats').source;
    },
    services => { delete services['account-event-relay'].environment.APPROVAL_RELAY_NATS_CURRENT_PUBLIC_KEYS_SHA256; },
    services => { services['account-event-relay'].volumes.find(volume => volume.target === '/var/run/public/pinvou/nats-authorization').read_only = false; },
    services => {
      const volumes = services['account-event-relay'].volumes;
      volumes.find(volume => volume.target === '/var/run/public/pinvou/nats-authorization').source = volumes.find(volume => volume.target === '/var/run/secrets/pinvou/approval-nats').source;
    },
  ]) {
    const { config, inventory } = businessComposeFixture();
    checkProductionBusiness(config, inventory);
    mutate(config.services);
    assert.throws(() => checkProductionBusiness(config, inventory));
  }
});

test('approval Relay database and broker credentials are Account-relay-only', () => {
  for (const path of ['deploy/compose/compose.yaml', 'deploy/production/compose.business.yaml']) {
    const { services } = YAML.parse(read(path), { merge: true });
    assert.ok(services['account-event-relay'].environment.APPROVAL_RELAY_DATABASE_URL, path);
    for (const [name, service] of Object.entries(services)) {
      const keys = Object.keys(service.environment || {}).filter(key => key.startsWith('APPROVAL_RELAY_'));
      if (name !== 'account-event-relay') assert.deepEqual(keys, [], `${path}:${name}`);
    }
  }
  const { services } = YAML.parse(read('deploy/production/compose.business.yaml'), { merge: true });
  const relay = services['account-event-relay'];
  const dedicated = new URL(relay.environment.APPROVAL_RELAY_DATABASE_URL);
  assert.equal(dedicated.username, 'account_approval_relay');
  assert.equal(dedicated.password, '');
  assert.equal(dedicated.searchParams.get('sslmode'), 'verify-full');
  assert.notEqual(relay.environment.APPROVAL_RELAY_DATABASE_URL, relay.environment.DATABASE_URL);
  assert.notEqual(relay.environment.APPROVAL_RELAY_NATS_NKEY_SEED_FILE, relay.environment.NATS_NKEY_SEED_FILE);
  const mount = relay.volumes.filter(volume => volume.target === '/var/run/secrets/pinvou/approval-nats');
  assert.equal(mount.length, 1);
  assert.equal(mount[0].read_only, true);
  assert.equal(mount[0].bind.create_host_path, false);
  assert.match(mount[0].source, /ACCOUNT_APPROVAL_RELAY_NATS_DIRECTORY/);
  const publicMount = relay.volumes.filter(volume => volume.target === '/var/run/public/pinvou/nats-authorization');
  assert.equal(publicMount.length, 1);
  assert.equal(publicMount[0].read_only, true);
  assert.equal(publicMount[0].bind.create_host_path, false);
  assert.notEqual(publicMount[0].source, mount[0].source);
  assert.equal(relay.environment.APPROVAL_RELAY_NATS_CURRENT_PUBLIC_KEYS_FILE, '/var/run/public/pinvou/nats-authorization/current-v2.json');
  for (const suffix of ['CURRENT_PUBLIC_KEYS_SHA256', 'PREVIOUS_PUBLIC_KEYS_FILE', 'PREVIOUS_PUBLIC_KEYS_SHA256']) {
    assert.ok(Object.hasOwn(relay.environment, `APPROVAL_RELAY_NATS_${suffix}`));
  }
  assert.equal(relay.environment.HEALTHCHECK_URL, 'http://127.0.0.1:9090/health/ready');
});

test('development NATS overlay and Helm keep authorization identity separate', () => {
  const { services } = YAML.parse(read('deploy/compose/compose.model-token.yaml'), { merge: true });
  assert.ok(services['account-event-relay'].environment.APPROVAL_RELAY_NATS_CREDENTIALS_FILE);
  for (const [name, service] of Object.entries(services)) {
    if (name !== 'account-event-relay') {
      assert.equal(service.environment?.APPROVAL_RELAY_NATS_CREDENTIALS_FILE, undefined, name);
    }
  }
  // This checks the template contract, not an actual Helm render/deployment.
  const helm = read('deploy/helm/account-platform/templates/event-relays.yaml');
  assert.match(helm, /if eq \$source "account"[\s\S]*?- name: APPROVAL_RELAY_DATABASE_URL[\s\S]*?key: account-approval-relay-database-url/);
  assert.match(helm, /if eq \$source "account"[\s\S]*?name: approval-nats-credentials, mountPath: \/var\/run\/secrets\/pinvou\/approval-nats, readOnly: true/);
  assert.match(helm, /key: account-approval-relay\.creds, path: account-approval-relay\.creds/);
  assert.match(helm, /natsPublicArtifactConfigMap/);
  assert.match(helm, /name: approval-nats-public-artifacts, mountPath: \/var\/run\/public\/pinvou\/nats-authorization\/current-v2\.json, subPath: current-v2\.json, readOnly: true/);
  assert.match(helm, /previousV1Enabled[\s\S]*?name: APPROVAL_RELAY_NATS_PREVIOUS_PUBLIC_KEYS_FILE[\s\S]*?name: APPROVAL_RELAY_NATS_PREVIOUS_PUBLIC_KEYS_SHA256/);
  assert.match(helm, /subPath: previous-v1\.json, readOnly: true/);
  assert.doesNotMatch(helm, /mountPath: \/var\/run\/public\/pinvou\/nats-authorization,/);
  assert.match(helm, /readinessProbe: \{ httpGet: \{ path: \/health\/ready,/);
});

test('authorization Relay startup has explicit independent credentials and a partitioned generic store', () => {
  const main = read('cmd/event-relay/main.go');
  assert.match(main, /source == "account"[\s\S]*?events\.NewPartitionedPostgresStore\(ctx, os\.Getenv\("DATABASE_URL"\), \[\]string\{account\.ApprovalAuthorizationEventType\}\)/);
  assert.match(main, /account\.NewApprovalRelayStore\(ctx, os\.Getenv\("APPROVAL_RELAY_DATABASE_URL"\)\)/);
  assert.match(main, /events\.NewNATSPublishOnlyBroker\(ctx, os\.Getenv\("NATS_URL"\), "account-approval-relay"/);
  assert.match(main, /approvalSeedFile := os\.Getenv\("APPROVAL_RELAY_NATS_NKEY_SEED_FILE"\)/);
  assert.match(main, /approvalCredentialsFile := os\.Getenv\("APPROVAL_RELAY_NATS_CREDENTIALS_FILE"\)/);
  assert.match(main, /SeedFile: approvalSeedFile, CredentialsFile: approvalCredentialsFile, RequireExplicitAuthentication: true/);
  for (const suffix of ['CURRENT_PUBLIC_KEYS_FILE', 'CURRENT_PUBLIC_KEYS_SHA256', 'PREVIOUS_PUBLIC_KEYS_FILE', 'PREVIOUS_PUBLIC_KEYS_SHA256']) {
    assert.ok(main.includes(`os.Getenv("APPROVAL_RELAY_NATS_${suffix}")`));
  }
  assert.match(main, /return errors\.Join\(approvalArtifacts\.Validate\(time\.Now\(\)\.UTC\(\)\), approvalStore\.Ready\(ctx\), approvalBroker\.Ping\(ctx\)\)/);
  assert.match(main, /events\.FrozenRelay\{Store: approvalStore, Broker: approvalBroker/);
  assert.match(main, /serverWithApproval\(/);
  const metrics = read('cmd/event-relay/metrics.go');
  assert.match(metrics, /router\.GET\("\/health\/ready\/approval-authorization"/);
  for (const metric of ['pending_events', 'active_claims', 'reclaimable_claims', 'unclassified_dead_letters']) {
    assert.ok(metrics.includes(`pinvou_authorization_outbox_${metric}`), metric);
  }
  assert.ok(metrics.includes('pinvou_authorization_relay_active_handlers'));
});

test('authorization Relay runtime is allowed only over TLS in production HBA', () => {
  const lines = read('deploy/production/pg_hba.conf').split(/\r?\n/)
    .map(line => line.trim()).filter(line => line && !line.startsWith('#') && line.includes('account_approval_relay'));
  assert.equal(lines.length, 1);
  const columns = lines[0].split(/\s+/);
  assert.equal(columns[0], 'hostssl');
  assert.equal(columns[1], 'account_platform_account');
  assert.ok(columns[2].split(',').includes('account_approval_relay'));
  assert.equal(columns[4], 'scram-sha-256');
});

test('authorization replay remains a CLI prohibition without an HTTP action', () => {
  const inventory = JSON.parse(read('deploy/authorization-entrypoints.json'));
  assert.deepEqual(inventory.http_policies.filter(policy => /(?:outbox|event).*replay|replay.*(?:outbox|event)/i.test(policy.path)), []);
  for (const file of ['internal/account/service.go', 'internal/billing/service.go', 'internal/token/service.go', 'internal/portal/service.go']) {
    assert.doesNotMatch(read(file), /\.(?:POST|PUT|PATCH|DELETE)\("[^"\n]*(?:outbox|event)[^"\n]*replay/i, file);
  }
  const source = read('internal/platform/events/postgres.go');
  const prohibition = source.indexOf('return ReplayResult{}, ErrAuthorizationEventReplayForbidden');
  const idempotencyLookup = source.indexOf('SELECT replay_id,event_id,request_hash');
  assert.ok(prohibition >= 0 && idempotencyLookup > prohibition);
  assert.match(source, /candidate\.Replayable = false/);
  assert.match(source, /candidate\.LastError = ""/);
});
