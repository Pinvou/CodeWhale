import assert from 'node:assert/strict';
import fs, { existsSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { syncBuiltinESMExports } from 'node:module';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { RetentionFacts, retentionDay } from '../retention-store.js';
import { TelemetryStore, buildBehaviorOverview } from '../telemetry-service.js';
import { backfillRetention } from '../scripts/backfill-retention.mjs';
import { FailureReportStore } from '../failure-diagnostics.js';

const at = s => Date.parse(s + '+08:00');
const DAY = 86400000;
const now = at('2026-10-20T10:00:00');
const device = (id = 'fixture') => ({ device_id: id, hardware_hash:`fixture-hash-${id}`, device_token:`fixture-token-${id}`, hardware_source: 'installation_id', first_seen_at: at('2026-09-01T08:00:00') });
const success = (time, id = 'fixture', extra = {}) => ({ device_id: id, event_id: `${id}_${time}`, attempt_id: `${id}_${time}`, event_name: 'task_finished', status: 'success', initiator_type: 'user', mode: 'assistant', occurred_at: at(time), received_at: at(time), ...extra });
const fixtureStores = new Map();
function testStore(dir, pepper, options) {
  const store = new TelemetryStore(dir, pepper, options);
  fixtureStores.get(dir).add(store);
  return store;
}
const facts = (events, devices = [device()], time = now) => {
  const result = new RetentionFacts(null, at('2026-08-01T00:00:00'));
  result.observe(events, devices, time);
  return result;
};
function fixture(t, action) {
  const dir = mkdtempSync(join(tmpdir(), 'pinvou-retention-'));
  fixtureStores.set(dir, new Set());
  t.after(() => {
    // saveNow persists immediately but does not cancel saveSoon's pending timer.
    // Close every store, including reopened instances, before deleting its files.
    for (const store of fixtureStores.get(dir)) {
      clearTimeout(store.saveTimer);
      store.saveTimer = null;
    }
    fixtureStores.delete(dir);
    // Windows can briefly retain a just-closed file during directory teardown.
    // Retry only cleanup, after all store timers have been cancelled.
    rmSync(dir, { recursive: true, force: true, maxRetries: 3, retryDelay: 50 });
  });
  return action(dir);
}
function registeredStore(dir) {
  const store = testStore(dir, 'fixture-pepper-at-least-24-characters');
  const credential = store.register({ hardware_claim: 'fixture-installation-retention', registration_secret: 'fixture-registration-secret-at-least-32', hardware_source: 'installation_id' });
  const registered = store.authenticate(credential.device_id, credential.device_token);
  store.saveNow();
  return { store, registered };
}

function ingressSuccess(collectionPeriodId, eventId = 'retention_ingress_event_001') {
  return { schema_version:1, event_id:eventId, event_name:'task_finished', event_source:'client', occurred_at:now,
    collection_period_id:collectionPeriodId, producer:'pinvou_agent', producer_version:'0.10.12', app_version:'0.10.12',
    platform:'windows', arch:'x86_64', channel:'dev', locale:'zh-CN', session_id:'retention_session_001',
    attempt_id:eventId, turn_id:'retention_turn_001', status:'success', duration_ms:1, mode:'assistant',
    initiator_type:'user', input_type:'text', has_attachment:false };
}

for (const damage of ['json', 'shape', 'row', 'duplicate', 'missing', 'read']) {
  test(`unknown device inventory (${damage}) cannot erase persisted retention on restart`, t => fixture(t, dir => {
    t.mock.method(Date, 'now', () => now);
    const { store, registered } = registeredStore(dir);
    const collection = store.startBehaviorCollection(registered, { request_id:'start_inventory_boundary_001' });
    assert.equal(store.appendBehaviorEvents(registered, [ingressSuccess(collection.collection_period_id)]).results[0].status, 'accepted');
    store.saveNow();
    clearTimeout(store.saveTimer); store.saveTimer = null;
    const inventory = readFileSync(store.devicesPath);
    const files = [store.retentionFacts.path, store.retentionFacts.marker, store.behaviorEventsPath];
    const before = files.map(path => readFileSync(path));
    if (damage === 'json') writeFileSync(store.devicesPath, '{broken');
    if (damage === 'shape') writeFileSync(store.devicesPath, '{"devices":null}');
    if (damage === 'row') writeFileSync(store.devicesPath, '{"devices":[null]}');
    if (damage === 'duplicate') writeFileSync(store.devicesPath, JSON.stringify({ devices:[registered, registered] }));
    if (damage === 'missing') rmSync(store.devicesPath);
    const corrupt = existsSync(store.devicesPath) ? readFileSync(store.devicesPath) : null;
    const originalRead = fs.readFileSync;
    const fault = damage === 'read' ? t.mock.method(fs, 'readFileSync', (path, ...args) => {
      if (path === store.devicesPath) throw Object.assign(new Error('fixture read denied'), { code:'EACCES' });
      return originalRead(path, ...args);
    }) : null;
    if (fault) syncBuiltinESMExports();
    try {
      assert.throws(() => testStore(dir, store.pepper), error => error.status === 503 && error.message === 'telemetry_storage_unavailable');
    } finally {
      if (fault) { fault.mock.restore(); syncBuiltinESMExports(); }
    }
    files.forEach((path, index) => assert.deepEqual(readFileSync(path), before[index]));
    if (corrupt) assert.deepEqual(readFileSync(store.devicesPath), corrupt);
    else assert.equal(existsSync(store.devicesPath), false);
    writeFileSync(store.devicesPath, inventory);
    const recovered = testStore(dir, store.pepper);
    assert.equal(recovered.retentionFacts.rows.size, 1);
    assert.equal(recovered.behaviorOverview('30').retention.d1.eligible_installations, 0);
    recovered.deleteBehaviorData(recovered.devices.get(registered.device_id), { request_id:'delete_inventory_boundary_001' });
    assert.equal(new RetentionFacts(dir).rows.size, 0);
  }));
}

test('known empty inventory still reconciles retention while a fresh startup anchors inventory first', t => fixture(t, dir => {
  const { store, registered } = registeredStore(dir);
  store.retentionFacts.observe([success('2026-09-01T08:00:00', registered.device_id)], [registered], now);
  store.retentionFacts.flush();
  writeFileSync(store.devicesPath, JSON.stringify({ devices:[] }));
  assert.equal(testStore(dir, store.pepper).retentionFacts.rows.size, 0);
  assert.equal(new RetentionFacts(dir).rows.size, 0);
}));

for (const remaining of [[], ['kept']]) {
  test(`online missing inventory warns counts only before pruning (${remaining.length} kept)`, t => fixture(t, dir => {
    const { store, registered } = registeredStore(dir);
    const devices = [registered, device('kept')];
    store.retentionFacts.observe(devices.map(d => success('2026-09-01T08:00:00', d.device_id)), devices, now);
    store.retentionFacts.flush();
    writeFileSync(store.devicesPath, JSON.stringify({ devices:devices.filter(d => remaining.includes(d.device_id)) }));
    const warnings = [];
    t.mock.method(console, 'warn', value => warnings.push(JSON.parse(value)));
    const reopened = testStore(dir, store.pepper);
    assert.deepEqual(warnings, [{ event:'retention_inventory_omission', rows:2 - remaining.length }]);
    assert.deepEqual([...reopened.retentionFacts.rows.keys()], remaining);
    assert.deepEqual([...new RetentionFacts(dir).rows.keys()], remaining);
    reopened.persistRetentionFacts(now);
    assert.equal(warnings.length, 1);
  }));
}

test('inventory warnings exclude known deletions and known ineligible identities', t => fixture(t, dir => {
  const warnings = [];
  t.mock.method(console, 'warn', value => warnings.push(value));
  const { store, registered } = registeredStore(dir);
  store.retentionFacts.observe([success('2026-09-01T08:00:00', registered.device_id)], [registered], now);
  store.retentionFacts.flush();
  store.deleteBehaviorData(registered, { request_id:'delete_no_inventory_warning_001' });
  assert.equal(warnings.length, 0);
  const state = facts([success('2026-09-01T08:00:00')]);
  state.observe([], [{ ...device(), hardware_source:'hardware_serial' }], now);
  assert.equal(state.rows.size, 0);
  assert.equal(warnings.length, 0);
}));

test('offline dry run and apply share strict inventory validation and preserve every authority on rejection', async t => fixture(t, async dir => {
  const { store, registered } = registeredStore(dir);
  store.retentionFacts.observe([success('2026-09-01T08:00:00', registered.device_id)], [registered], now);
  store.retentionFacts.flush();
  writeFileSync(store.behaviorEventsPath, `${JSON.stringify(success('2026-10-01T08:00:00', registered.device_id))}\n`);
  writeFileSync(store.deletionTombstonesPath, '{"records":[]}');
  const validInventory = readFileSync(store.devicesPath);
  const files = [store.retentionFacts.path, store.retentionFacts.marker, store.behaviorEventsPath, store.deletionTombstonesPath];
  const before = files.map(path => readFileSync(path));
  const second = { ...registered, device_id:'second' };
  for (const devices of [[{ device_id:registered.device_id }], [registered, registered], [registered, second],
    [{ ...registered, device_token:'' }], [null]]) {
    const corrupt = JSON.stringify({ devices });
    writeFileSync(store.devicesPath, corrupt);
    for (const apply of [false, true]) {
      await assert.rejects(backfillRetention({ dataDir:dir, now, apply }), /invalid device inventory/);
      files.forEach((path, index) => assert.deepEqual(readFileSync(path), before[index]));
      assert.equal(readFileSync(store.devicesPath, 'utf8'), corrupt);
    }
  }
  writeFileSync(store.devicesPath, validInventory);
  writeFileSync(store.deletionTombstonesPath, '{"records":[null]}');
  assert.throws(() => testStore(dir, store.pepper), /telemetry_storage_unavailable/);
  for (const apply of [false, true]) await assert.rejects(backfillRetention({ dataDir:dir, now, apply }), /invalid deletion inventory/);
  assert.deepEqual(readFileSync(store.retentionFacts.path), before[0]);
  writeFileSync(store.deletionTombstonesPath, before[3]);
  const applied = await backfillRetention({ dataDir:dir, now, apply:true });
  assert.equal(applied.after.d30.retained_installations, 1);
  assert.equal(testStore(dir, store.pepper).retentionFacts.rows.size, 1);
}));

test('query reconciliation matures deferred events and fails closed when its authority cannot persist', t => fixture(t, dir => {
  let clock = now;
  t.mock.method(Date, 'now', () => clock);
  const { store, registered } = registeredStore(dir);
  // Isolate the query's reconciliation from the periodic compactor: removing
  // behaviorOverview's persistRetentionFacts call must make this test fail.
  t.mock.method(store, 'compactExpiredEventsIfDue', () => false);
  store.behaviorEvents = [success('2026-10-21T08:00:00', registered.device_id)];
  store.persistRetentionFacts(clock);
  assert.equal(store.retentionFacts.rows.size, 0);
  const original = readFileSync(store.retentionFacts.path);
  clock = at('2026-10-22T08:00:00');
  const flush = t.mock.method(store.retentionFacts, 'flush', () => { throw new Error('fixture disk full'); });
  assert.throws(() => store.behaviorOverview('30'), error => error.status === 503 && error.message === 'retention_storage_unavailable');
  assert.deepEqual(readFileSync(store.retentionFacts.path), original);
  assert.equal(store.retentionFacts.dirty, true);
  flush.mock.restore();
  assert.equal(store.behaviorOverview('30').retention.d1.eligible_installations, 1);
  assert.equal(new RetentionFacts(dir).rows.get(registered.device_id).first_success_day, retentionDay(at('2026-10-21T08:00:00')));
  store.devices.delete(registered.device_id);
  assert.equal(store.behaviorOverview('30').retention.d1.eligible_installations, 0);
  assert.equal(new RetentionFacts(dir).rows.size, 0);
}));

test('metric activation is recorded on successful authority persistence and survives restart', t => fixture(t, dir => {
  let clock = now;
  t.mock.method(Date, 'now', () => clock);
  const { store } = registeredStore(dir);
  const legacy = JSON.parse(readFileSync(store.retentionFacts.path, 'utf8'));
  legacy.initialized_at = at('2026-08-01T00:00:00');
  delete legacy.metric_version_started_at;
  writeFileSync(store.retentionFacts.path, JSON.stringify(legacy));
  clock += DAY;
  const migrated = testStore(dir, store.pepper);
  const report = migrated.behaviorOverview('30');
  assert.equal(report.metric_version, 'behavior-v1.5');
  assert.equal(report.metric_version_started_at, clock);
  assert.equal(migrated.retentionFacts.initializedAt, legacy.initialized_at);
  clock += DAY;
  assert.equal(testStore(dir, store.pepper).behaviorOverview('30').metric_version_started_at, report.metric_version_started_at);
  assert.equal(buildBehaviorOverview([], [], [], '30', clock).metric_version_started_at, null);
  assert.equal(buildBehaviorOverview([], [], [], '30', clock).metric_version, 'behavior-v1.5');
}));

test('loader rejects invalid facts, rows and marker without rewriting the authority', t => fixture(t, dir => {
  const path = join(dir, 'retention-facts.json');
  const marker = join(dir, 'retention-facts.initialized');
  const row = { device_id:'fixture', first_success_day:20_000, successful_days:[20_000], history_complete:true };
  const valid = { version:1, initialized_at:now, rows:[row] };
  const badRows = [null, [], {}, { ...row, device_id:'' }, { ...row, device_id:'x'.repeat(129) },
    { ...row, first_success_day:0 }, { ...row, first_success_day:1.5 }, { ...row, history_complete:'true' },
    { ...row, first_success_day:100_000_001, successful_days:[100_000_001] },
    { ...row, first_success_day:Number.MAX_SAFE_INTEGER + 1, successful_days:[Number.MAX_SAFE_INTEGER + 1] },
    { ...row, first_success_day:100_000_000, successful_days:[100_000_000, 100_000_001] },
    { ...row, successful_days:null }, { ...row, successful_days:[] }, { ...row, successful_days:[20_000, 19_999] },
    { ...row, successful_days:[20_000, 20_031] }, { ...row, successful_days:[20_000, 20_000.5] },
    { ...row, successful_days:Array(32).fill(20_000) }];
  for (const value of [null, [], { ...valid, version:2 }, { ...valid, initialized_at:0 },
    { ...valid, metric_version_started_at:null }, { ...valid, rows:[row, row] },
    ...badRows.map(row => ({ ...valid, rows:[row] }))]) {
    const bytes = JSON.stringify(value);
    writeFileSync(path, bytes); writeFileSync(marker, '1\n');
    assert.throws(() => new RetentionFacts(dir), /invalid retention (facts|row)/, bytes);
    assert.equal(readFileSync(path, 'utf8'), bytes);
  }
  writeFileSync(path, JSON.stringify({ ...valid, rows:[{ ...row, successful_days:Array.from({length:31}, (_, index) => 20_000 + index) }] }));
  assert.equal(new RetentionFacts(dir).rows.get('fixture').successful_days.length, 31);
  // The upper representable day remains valid even when it is ahead of now.
  writeFileSync(path, JSON.stringify({ ...valid, rows:[{ ...row, first_success_day:100_000_000, successful_days:[100_000_000] }] }));
  assert.equal(new RetentionFacts(dir).rows.get('fixture').first_success_day, 100_000_000);
  for (const value of ['', '1', '2\n', '1\nextra']) {
    writeFileSync(marker, value);
    assert.throws(() => new RetentionFacts(dir), /missing or invalid retention authority/);
    assert.equal(readFileSync(marker, 'utf8'), value);
  }
}));

for (const [label, days] of [['duplicate', [20_000, 20_000, 20_001]], ['unordered', [20_002, 20_000, 20_001]]]) {
  test(`loader normalizes and persists ${label} successful dates under an existing marker`, t => fixture(t, dir => {
    const path = join(dir, 'retention-facts.json');
    const marker = join(dir, 'retention-facts.initialized');
    const expected = label === 'duplicate' ? [20_000, 20_001] : [20_000, 20_001, 20_002];
    writeFileSync(path, JSON.stringify({ version:1, initialized_at:now, metric_version_started_at:now,
      rows:[{ device_id:'fixture', first_success_day:20_000, successful_days:days, history_complete:true }] }));
    writeFileSync(marker, '1\n');
    const state = new RetentionFacts(dir);
    assert.deepEqual(state.rows.get('fixture').successful_days, expected);
    assert.equal(state.dirty, true);
    state.flush();
    assert.deepEqual(JSON.parse(readFileSync(path, 'utf8')).rows[0].successful_days, expected);
    const reopened = new RetentionFacts(dir);
    assert.deepEqual(reopened.rows.get('fixture').successful_days, expected);
    assert.equal(reopened.dirty, false);
  }));
}

for (const faultAt of ['facts-rename', 'marker-write', 'marker-rename']) {
  test(`first activation survives ${faultAt} without a torn marker`, t => fixture(t, dir => {
    const state = new RetentionFacts(dir, now);
    const originalRename = fs.renameSync, originalWrite = fs.writeFileSync;
    let markerTemporary;
    const originalOpen = fs.openSync;
    const openFault = t.mock.method(fs, 'openSync', (path, ...args) => {
      const fd = originalOpen(path, ...args);
      if (String(path).startsWith(state.marker + '.')) markerTemporary = fd;
      return fd;
    });
    const writeFault = t.mock.method(fs, 'writeFileSync', (fd, ...args) => {
      if (faultAt === 'marker-write' && fd === markerTemporary) {
        originalWrite(fd, '1');
        throw Object.assign(new Error('fixture marker write failed'), { code:'EIO' });
      }
      return originalWrite(fd, ...args);
    });
    const renameFault = t.mock.method(fs, 'renameSync', (source, target) => {
      if ((faultAt === 'facts-rename' && target === state.path) || (faultAt === 'marker-rename' && target === state.marker)) {
        throw Object.assign(new Error('fixture rename failed'), { code:'EIO' });
      }
      if (target === state.marker) assert.equal(JSON.parse(readFileSync(state.path, 'utf8')).version, 1);
      return originalRename(source, target);
    });
    syncBuiltinESMExports();
    try {
      assert.throws(() => state.flush(), { code:'EIO' });
      assert.equal(state.dirty, true);
      assert.equal(state.metricVersionStartedAt, null);
      assert.equal(existsSync(state.marker), false);
      assert.equal(existsSync(state.path), faultAt !== 'facts-rename');
      if (existsSync(state.path)) assert.equal(JSON.parse(readFileSync(state.path, 'utf8')).version, 1);
      assert.equal(readdirSync(dir).some(name => name.endsWith('.tmp')), false);
    } finally {
      openFault.mock.restore(); writeFault.mock.restore(); renameFault.mock.restore(); syncBuiltinESMExports();
    }
    const reopened = new RetentionFacts(dir, now);
    reopened.flush();
    assert.equal(readFileSync(state.marker, 'utf8'), '1\n');
    assert.equal(new RetentionFacts(dir).dirty, false);
  }));
}

test('a later earlier-success batch preserves the exact D30 target across reload', t => fixture(t, dir => {
  const initial = new RetentionFacts(dir, at('2026-08-01T00:00:00'));
  initial.observe([success('2026-09-02T08:00:00'), success('2026-10-01T08:00:00')], [device()], now);
  initial.flush();
  const next = new RetentionFacts(dir);
  next.observe([success('2026-09-01T08:00:00')], [device()], now);
  next.flush();
  assert.equal(next.metrics(now).d30.retained_installations, 1);
  assert.equal(new RetentionFacts(dir).metrics(now).d30.retained_installations, 1);
}));

test('unresolved installation counts include missing history and legacy comparison keeps its distinct anchor', async t => fixture(t, async dir => {
  const devices = [device('legacy'), { ...device('new'), first_seen_at:at('2026-09-03T00:00:00') },
    { ...device('missing'), first_seen_at:null }, { ...device('hardware'), hardware_source:'hardware_serial' }];
  const state = new RetentionFacts(dir, at('2026-09-02T00:00:00'));
  state.observe([], devices, now);
  assert.equal(state.metrics(now).d30.history_unresolved_installations, 2);
  state.observe([success('2026-09-02T08:00:00', 'new')], devices, now);
  assert.equal(state.metrics(now).d30.history_unresolved_installations, 3);
  state.flush();
  writeFileSync(join(dir, 'devices.json'), JSON.stringify({ devices }));
  const events = [{ ...success('2026-09-01T08:00:00', 'legacy'), event_name:'analytics_cohort_started' },
    success('2026-09-02T08:00:00', 'legacy'), success('2026-10-01T08:00:00', 'legacy'),
    success('2026-10-21T08:00:00', 'legacy'), success('2026-09-01T08:00:00', 'excluded'),
    success('2026-09-01T08:00:00', 'new', { identity_id:'legacy' })];
  writeFileSync(join(dir, 'behavior-events.jsonl'), events.map(value => JSON.stringify(value)).join('\n'));
  const report = await backfillRetention({ dataDir:dir, now });
  assert.deepEqual(report.legacy_cohort_start.d30, { eligible_installations:1, retained_installations:1 });
  assert.equal(report.after.d30.retained_installations, 0);
  assert.deepEqual(report.excluded_events, { ineligible_identity:1, future_timestamp:1, identity_mismatch:1 });
}));

test('a failed close retains its original error and cleans its temporary file', t => fixture(t, dir => {
  const state = new RetentionFacts(dir);
  const close = fs.closeSync;
  const fault = t.mock.method(fs, 'closeSync', descriptor => {
    close(descriptor);
    throw Object.assign(new Error('fixture primary close error'), { code:'EIO' });
  });
  syncBuiltinESMExports();
  try {
    assert.throws(() => state.flush(), error => error.code === 'EIO' && error.message === 'fixture primary close error');
    assert.equal(state.dirty, true);
    assert.equal(state.metricVersionStartedAt, null);
    assert.equal(readdirSync(dir).some(name => name.endsWith('.tmp')), false);
  } finally { fault.mock.restore(); syncBuiltinESMExports(); }
  state.flush();
  assert.ok(state.metricVersionStartedAt > 0);
}));

test('day zero is rejected before persistence and seconds are normalized exactly once', t => fixture(t, dir => {
  assert.equal(retentionDay(57), 0);
  const state = new RetentionFacts(dir);
  state.observe([success('2026-09-01T08:00:00', 'fixture', { occurred_at:57 })], [device()], now);
  assert.equal(state.rows.size, 0);
  state.observe([success('2026-09-01T08:00:00', 'fixture', { occurred_at:57_600 })], [device()], now);
  assert.equal(state.rows.get('fixture').first_success_day, 1);
  state.flush();
  assert.equal(new RetentionFacts(dir).rows.get('fixture').first_success_day, 1);
}));

test('backfill reports rows dropped by device reconciliation before applying them', async t => fixture(t, async dir => {
  const state = new RetentionFacts(dir);
  state.observe(['kept', 'missing'].map(id => success('2026-09-01T08:00:00', id)), ['kept', 'missing'].map(device), now);
  state.flush();
  writeFileSync(join(dir, 'devices.json'), JSON.stringify({ devices:[device('kept')] }));
  writeFileSync(join(dir, 'behavior-events.jsonl'), '');
  const bytes = readFileSync(state.path);
  const warnings = [];
  t.mock.method(console, 'warn', value => warnings.push(JSON.parse(value)));
  const dry = await backfillRetention({ dataDir:dir, now });
  assert.deepEqual(warnings, [{ event:'retention_inventory_omission', rows:1 }]);
  assert.equal(dry.before.d30.eligible_installations, 2);
  assert.equal(dry.after.d30.eligible_installations, 1);
  assert.equal(dry.rows_dropped, 1);
  assert.deepEqual(readFileSync(state.path), bytes);
  const applied = await backfillRetention({ dataDir:dir, now, apply:true });
  assert.deepEqual(applied, { ...dry, applied:true });
  assert.deepEqual([...new RetentionFacts(dir).rows.keys()], ['kept']);
}));

test('backfill skips invalid JSON values and day zero consistently in dry run and apply', async t => fixture(t, async dir => {
  const { store, registered } = registeredStore(dir);
  const source = join(dir, 'backup.jsonl');
  const event = success('2026-09-01T08:00:00', registered.device_id);
  const lines = [null, 42, 'text', [], { ...event, occurred_at:57 }, event, event];
  writeFileSync(source, lines.map(value => JSON.stringify(value)).join('\n') + '\n{broken\n');
  const rawExists = existsSync(store.behaviorEventsPath);
  const before = readFileSync(store.retentionFacts.path);
  const dry = await backfillRetention({ dataDir:dir, sources:[source, source], now });
  assert.equal(dry.scanned_events, 7); // Parsed input lines, including duplicates; source paths are deduplicated.
  assert.equal(dry.malformed_lines, 1);
  assert.equal(dry.invalid_events, 4);
  assert.equal(dry.invalid_timestamp_events, 1);
  assert.equal(dry.after.d30.eligible_installations, 1);
  assert.deepEqual(readFileSync(store.retentionFacts.path), before);
  const applied = await backfillRetention({ dataDir:dir, sources:[source], now, apply:true });
  assert.deepEqual(applied, { ...dry, applied:true });
  assert.equal(existsSync(store.behaviorEventsPath), rawExists);
  assert.equal(testStore(dir, store.pepper).retentionFacts.rows.size, 1);
}));

test('a fresh deployment has an empty read-only backfill but explicit missing sources fail', async t => fixture(t, async dir => {
  writeFileSync(join(dir, 'devices.json'), JSON.stringify({ devices:[] }));
  const files = readdirSync(dir);
  const dry = await backfillRetention({ dataDir:dir, now });
  assert.equal(dry.scanned_events, 0);
  assert.equal(dry.after.d30.eligible_installations, 0);
  assert.deepEqual(readdirSync(dir), files);
  await assert.rejects(backfillRetention({ dataDir:dir, sources:[join(dir, 'missing-backup.jsonl')], now }), { code:'ENOENT' });
  await assert.rejects(backfillRetention({ dataDir:dir, sources:[join(dir, 'behavior-events.jsonl')], now }), { code:'ENOENT' });
}));

for (const operation of ['writeFileSync', 'renameSync']) {
  test(`failed retention ${operation} removes its temporary file and can retry`, t => fixture(t, dir => {
    const state = new RetentionFacts(dir);
    state.flush();
    const before = readFileSync(state.path);
    state.observe([success('2026-09-01T08:00:00')], [device()], now);
    const original = fs[operation];
    const failure = t.mock.method(fs, operation, (...args) => {
      if (operation === 'writeFileSync') original(args[0], '{partial');
      throw Object.assign(new Error('fixture disk failure'), { code:'EIO' });
    });
    syncBuiltinESMExports();
    try {
      assert.throws(() => state.flush(), /fixture disk failure/);
      assert.deepEqual(readFileSync(state.path), before);
      assert.equal(state.dirty, true);
      assert.equal(readdirSync(dir).some(name => name.endsWith('.tmp')), false);
    } finally {
      failure.mock.restore();
      syncBuiltinESMExports();
    }
    state.flush();
    assert.equal(new RetentionFacts(dir).rows.size, 1);
    assert.equal(state.dirty, false);
  }));
}

test('behavior ingress observes accepted increments without rescanning retained history', t => fixture(t, dir => {
  t.mock.method(Date, 'now', () => now);
  const { store, registered } = registeredStore(dir);
  const collection = store.startBehaviorCollection(registered, { request_id:'start_increment_retention_001' });
  store.compactExpiredEventsIfDue(now);
  store.behaviorEvents = Array.from({ length:50_000 }, (_, index) => ({ event_id:`historical_${index}`, event_name:'scene_impression' }));
  const lengths = [];
  const observe = store.retentionFacts.observe.bind(store.retentionFacts);
  store.retentionFacts.observe = (events, ...args) => { lengths.push(events.length); return observe(events, ...args); };
  const event = ingressSuccess(collection.collection_period_id);
  assert.equal(store.appendBehaviorEvents(registered, [event]).results[0].status, 'accepted');
  assert.deepEqual(lengths, [1]);
  assert.equal(store.appendBehaviorEvents(registered, [event]).results[0].status, 'duplicate');
  assert.deepEqual(lengths, [1]);
  assert.equal(new RetentionFacts(dir).rows.size, 1);
  store.saveNow();
}));

test('ABC real-time D30 keeps A and lets C join both denominator and numerator today', () => {
  const devices = ['A','B','C'].map(device);
  const events = [success('2026-09-18T08:00:00','A'), success('2026-09-19T08:00:00','B'), success('2026-09-20T08:00:00','C'), success('2026-10-18T08:00:00','A')];
  const state = facts(events, devices);
  assert.equal(state.metrics(now).d30.eligible_installations, 3);
  assert.equal(state.metrics(now).d30.retained_installations, 1);
  assert.equal(state.metrics(now).d30.provisional_installations, 1);
  state.observe([success('2026-10-20T09:00:00','C')], devices, now);
  assert.equal(state.metrics(now).d30.retained_installations, 2);
  assert.equal(state.metrics(now + DAY).d30.retained_installations, 2);
  assert.equal(state.metrics(now + DAY).d30.provisional_installations, 0);
  for (const range of ['today','7','30']) {
    const report = buildBehaviorOverview(devices, [], events, range, now);
    assert.equal(report.retention.d30.retained_installations, 1);
  }
});

test('first success crosses Beijing midnight and replaces cohort-start attribution', () => {
  const events = [{ event_name:'analytics_cohort_started', device_id:'fixture', occurred_at:at('2026-09-01T23:50:00') }, success('2026-09-02T00:05:00'), success('2026-10-01T08:00:00')];
  const state = facts(events);
  assert.equal(state.metrics(now).d30.retained_installations, 0);
  state.observe([success('2026-10-02T08:00:00')], [device()], now);
  assert.equal(state.metrics(now).d30.retained_installations, 1);
});

test('failed, submitted, scheduled, system, noninstallation and future events do not activate', () => {
  const events = [
    success('2026-09-01T08:00:00', 'fixture', { status:'failed' }),
    success('2026-09-01T08:00:00', 'fixture', { event_name:'task_submitted' }),
    success('2026-09-01T08:00:00', 'fixture', { initiator_type:'system' }),
    success('2026-09-01T08:00:00', 'fixture', { mode:'scheduled' }),
    success('2026-09-01T08:00:00', 'unknown'),
    success('2026-09-01T08:00:00', 'hardware'),
    success('2026-09-01T08:00:00', 'fixture', { identity_id:'other' }),
    success('2026-10-21T00:00:00'),
  ];
  const state = facts(events, [device(), { ...device('hardware'), hardware_source:'hardware_serial' }]);
  assert.equal(state.rows.size, 0);
  state.observe(events, [device()], at('2026-11-21T00:00:00'));
  assert.equal(state.metrics(at('2026-11-21T00:00:00')).d30.eligible_installations, 1);
});

test('duplicates and out-of-order earlier success recalculate exact horizons with bounded facts', () => {
  const events = [success('2026-09-02T08:00:00'), success('2026-10-02T08:00:00'), success('2026-10-01T08:00:00'), success('2026-09-01T08:00:00')];
  const a = facts(events), b = facts([...events].reverse());
  a.observe(events, [device()], now);
  assert.deepEqual(a.metrics(now), b.metrics(now));
  assert.equal(a.metrics(now).d30.retained_installations, 1);
  assert.equal(a.rows.get('fixture').successful_days.length, 3);
  const many = Array.from({length:1000}, (_,i) => ({...success('2026-09-01T08:00:00'), occurred_at: at('2026-09-01T08:00:00') + i*DAY}));
  a.observe(many, [device()], at('2030-01-01T00:00:00'));
  assert.equal(a.rows.get('fixture').successful_days.length, 31);
});

test('durable facts survive age compaction and restart', t => fixture(t, dir => {
  t.mock.method(Date, 'now', () => now);
  const { store, registered } = registeredStore(dir);
  store.behaviorEvents = ['2026-09-01T08:00:00','2026-09-02T08:00:00','2026-09-08T08:00:00','2026-10-01T08:00:00'].map(time => success(time,registered.device_id));
  store.compactBehaviorEvents(at('2026-12-01T00:00:00'));
  assert.equal(store.behaviorEvents.length, 0);
  for (const h of [1,7,30]) assert.equal(store.retentionFacts.metrics(now)[`d${h}`].retained_installations, 1);
  const reopened = testStore(dir, store.pepper);
  for (const h of [1,7,30]) assert.equal(reopened.behaviorOverview('today').retention[`d${h}`].retained_installations, 1);
  assert.equal(reopened.behaviorOverview('today').retention.d30.history_status, 'partial');
}));

test('capacity compaction also persists facts before removing successful events', t => fixture(t, dir => {
  const { store, registered } = registeredStore(dir);
  store.maxEvents=2;
  store.eventCompactionTarget=1;
  store.behaviorEvents=[success('2026-09-01T08:00:00',registered.device_id),success('2026-10-01T08:00:00',registered.device_id),{device_id:registered.device_id,event_name:'scene_impression',received_at:at('2026-10-01T09:00:00')}];
  store.compactBehaviorEvents(at('2026-10-02T00:00:00'));
  assert.equal(store.behaviorEvents.length,1);
  assert.equal(store.behaviorEvents[0].event_name,'scene_impression');
  assert.equal(new RetentionFacts(dir).metrics(now).d30.retained_installations,1);
}));

test('ingress retries cannot acknowledge duplicates before pending retention facts persist', t => fixture(t, dir => {
  t.mock.method(Date,'now',()=>now);
  const { store, registered } = registeredStore(dir);
  const collection=store.startBehaviorCollection(registered,{request_id:'start_retention_ingress_001'});
  store.compactExpiredEventsIfDue(now);
  const event={schema_version:1,event_id:'retention_ingress_event_001',event_name:'task_finished',event_source:'client',occurred_at:now,collection_period_id:collection.collection_period_id,producer:'pinvou_agent',producer_version:'0.10.12',app_version:'0.10.12',platform:'windows',arch:'x86_64',channel:'dev',locale:'zh-CN',session_id:'retention_session_001',attempt_id:'retention_attempt_001',turn_id:'retention_turn_001',status:'success',duration_ms:1,mode:'assistant',initiator_type:'user',input_type:'text',has_attachment:false};
  const flush=store.retentionFacts.flush.bind(store.retentionFacts);
  store.retentionFacts.flush=()=>{if(store.behaviorEvents.length) throw new Error('fixture disk failure'); flush();};
  const result=store.appendBehaviorEvents(registered,[event]);
  assert.equal(result.results[0].status,'retryable');
  assert.match(readFileSync(store.behaviorEventsPath,'utf8'),/retention_ingress_event_001/);
  assert.throws(()=>store.appendBehaviorEvents(registered,[event]),/retention_storage_unavailable/);
  store.retentionFacts.flush=flush;
  assert.equal(store.appendBehaviorEvents(registered,[event]).results[0].status,'duplicate');
  assert.equal(new RetentionFacts(dir).rows.size,1);
}));

test('startup migration observes old successes before pruning the raw file', t => fixture(t, dir => {
  t.mock.method(Date, 'now', () => now);
  const { store, registered } = registeredStore(dir);
  const old = ['2025-01-01T08:00:00','2025-01-31T08:00:00'].map(time => success(time,registered.device_id));
  writeFileSync(store.behaviorEventsPath, old.map(e=>JSON.stringify(e)).join('\n')+'\n');
  const migrated = testStore(dir, store.pepper);
  assert.equal(migrated.behaviorEvents.length, 0);
  assert.equal(migrated.behaviorOverview('30').retention.d30.retained_installations, 1);
}));

test('an installation returning after 36 days keeps its original identity and first registration', t => fixture(t, dir => {
  let clock=at('2026-09-01T08:00:00');
  t.mock.method(Date,'now',()=>clock);
  const { store, registered } = registeredStore(dir);
  const first=registered.first_seen_at;
  store.behaviorEvents=[success('2026-09-01T08:00:00',registered.device_id)];
  clock+=36*DAY;
  store.compactExpiredData(clock);
  assert.equal(store.behaviorEvents.length,0);
  const reopened=testStore(dir,store.pepper);
  const returned=reopened.register({hardware_claim:'fixture-installation-retention',registration_secret:'fixture-registration-secret-at-least-32',hardware_source:'installation_id'});
  assert.equal(returned.device_id,registered.device_id);
  assert.equal(reopened.devices.size,1);
  assert.equal(reopened.devices.get(returned.device_id).first_seen_at,first);
  const today=reopened.overview().usage_trend_hourly_today;
  assert.equal(today.reduce((n,row)=>n+(row.new_devices||0),0),0);
  reopened.saveNow();
}));

test('deletion removes retention and a restored facts backup cannot resurrect the subject', t => fixture(t, dir => {
  const { store, registered } = registeredStore(dir);
  store.behaviorEvents = [success('2026-09-01T08:00:00',registered.device_id),success('2026-10-01T08:00:00',registered.device_id)];
  store.persistRetentionFacts(now);
  const backup = readFileSync(store.retentionFacts.path);
  store.deleteBehaviorData(registered, { request_id:'delete_retention_fixture_001' });
  assert.equal(new RetentionFacts(dir).rows.size, 0);
  writeFileSync(store.retentionFacts.path, backup);
  const reopened = testStore(dir, store.pepper);
  assert.equal(reopened.behaviorOverview('30').retention.d30.eligible_installations, 0);
  assert.equal(new RetentionFacts(dir).rows.size, 0);
}));

for (const recovery of ['retry', 'restart']) {
  test(`deletion waits for retention and diagnostic cleanup before ${recovery} completes it`, t => fixture(t, dir => {
    const { store, registered } = registeredStore(dir);
    const period = store.startBehaviorCollection(registered, { request_id:'start_joint_cleanup_fixture' }).collection_period_id;
    store.behaviorEvents = [success('2026-09-01T08:00:00', registered.device_id)];
    store.persistRetentionFacts(now);
    const incident = { device_id:registered.device_id, event_id:'joint_cleanup_incident', diagnostic_id:'joint_cleanup_incident',
      collection_period_id:period, diagnostic_code:'tool_failed', diagnostic_component:'tool', stage:'tool' };
    store.failureReports.append(registered, { report_id:'joint_cleanup_report', incident_id:incident.diagnostic_id,
      collection_period_id:period, incident_period_id:period, notice_version:'failure-diagnostics-v1',
      confirmed_at:Date.now(), notes:'error_code=EACCES', attachments:[] }, incident);
    store.failureReports.detail(incident, 'fixture-reviewer');
    const paths = [store.retentionFacts.path, store.retentionFacts.marker, store.failureReports.path, store.failureReports.metadataPath];
    const before = paths.map(path => readFileSync(path));
    const fault = t.mock.method(store.retentionFacts, 'flush', () => { throw new Error('fixture disk failure'); });
    const response = store.deleteBehaviorData(registered, { request_id:'delete_pending_retention_001' });
    assert.equal(response.status, 'processing');
    assert.equal(response.completed_at, null);
    assert.equal(store.deletionStatus(response.deletion_request_id, registered.device_token).status, 'processing');
    assert.equal(JSON.parse(readFileSync(store.deletionTombstonesPath, 'utf8')).records.at(-1).status, 'processing');
    paths.forEach((path, index) => assert.deepEqual(readFileSync(path), before[index]));
    let recovered;
    if (recovery === 'retry') {
      fault.mock.restore();
      assert.equal(store.deletionStatus(response.deletion_request_id, registered.device_token).status, 'completed');
      recovered = store;
    } else {
      recovered = testStore(dir, store.pepper);
      assert.equal(recovered.deletionStatus(response.deletion_request_id, registered.device_token).status, 'completed');
    }
    assert.equal(new RetentionFacts(dir).rows.size, 0);
    const reports = new FailureReportStore(dir);
    assert.equal(reports.state.reports.length, 0);
    // Access audit keeps its existing bounded retention; deleting report
    // material must not silently erase that separate security ledger.
    assert.deepEqual(reports.state.audit, JSON.parse(before[3].toString('utf8')).audit);
    assert.equal(recovered.deletionTombstones.at(-1).status, 'completed');
  }));
}

for (const failStream of [false, true]) {
  test(`pending deletion rewrites only dirty streams and retries failed disk commits (${failStream})`, t => fixture(t, dir => {
    const { store, registered } = registeredStore(dir);
    const other = store.register({ hardware_claim:'other-retention-fixture', registration_secret:'other-retention-secret-at-least-32', hardware_source:'installation_id' });
    const rows = [registered.device_id, other.device_id].map((device_id, index) => ({
      device_id, event_id:`dirty_stream_${index}`, event_name:'app_opened', occurred_at:now, received_at:now,
    }));
    store.events = rows.slice(); store.behaviorEvents = rows.slice(); store.behaviorPipelineEvents = rows.slice();
    const streams = [store.eventsPath, store.behaviorEventsPath, store.behaviorPipelinePath];
    for (const path of streams) writeFileSync(path, rows.map(row => JSON.stringify(row)).join('\n') + '\n');
    const writes = [], rename = fs.renameSync;
    let failNext = failStream;
    const fault = t.mock.method(fs, 'renameSync', (source, target) => {
      if (streams.includes(target)) writes.push(target);
      if (failNext && target === store.behaviorEventsPath) { failNext = false; throw new Error('fixture stream disk failure'); }
      return rename(source, target);
    });
    const prune = failStream ? null : t.mock.method(store.failureReports, 'prune', () => { throw new Error('fixture late cleanup failure'); });
    syncBuiltinESMExports();
    try {
      const response = store.deleteBehaviorData(registered, { request_id:'delete_dirty_streams_fixture' });
      assert.equal(response.status, 'processing');
      if (failStream) {
        assert.equal(store.behaviorEvents.length, 2, 'failed disk commit must not mark memory clean');
        assert.match(readFileSync(store.behaviorEventsPath, 'utf8'), new RegExp(registered.device_id));
      } else {
        for (let count = 0; count < 3; count++) {
          assert.equal(store.deletionStatus(response.deletion_request_id, registered.device_token).status, 'processing');
        }
        assert.deepEqual(writes, streams, 'late cleanup failure must not amplify already-clean stream writes');
        prune.mock.restore();
      }
      assert.equal(store.deletionStatus(response.deletion_request_id, registered.device_token).status, 'completed');
      assert.equal(writes.filter(path => path === store.eventsPath).length, 1);
      for (const path of streams) {
        const saved = readFileSync(path, 'utf8').trim().split('\n').map(line => JSON.parse(line));
        assert.deepEqual(saved.map(row => row.device_id), [other.device_id]);
      }
    } finally {
      fault.mock.restore(); if (prune) prune.mock.restore(); syncBuiltinESMExports();
    }
  }));
}

for (const marker of [false, null, 'true']) {
  test(`online and offline inventory reject invalid required-ledger marker ${JSON.stringify(marker)}`, async t => fixture(t, async dir => {
    const { store } = registeredStore(dir);
    const inventory = JSON.parse(readFileSync(store.devicesPath, 'utf8'));
    inventory.deletion_ledger_required = marker;
    const bytes = JSON.stringify(inventory);
    writeFileSync(store.devicesPath, bytes);
    const files = [store.retentionFacts.path, store.retentionFacts.marker, store.deletionTombstonesPath];
    const before = files.map(path => readFileSync(path));
    assert.throws(() => testStore(dir, store.pepper), error => error.status === 503 && error.message === 'telemetry_storage_unavailable');
    for (const apply of [false, true]) await assert.rejects(backfillRetention({ dataDir:dir, now, apply }), /invalid device inventory/);
    files.forEach((path, index) => assert.deepEqual(readFileSync(path), before[index]));
    assert.equal(readFileSync(store.devicesPath, 'utf8'), bytes);
  }));
}

test('offline reconciliation refuses a missing required ledger before touching retention authority', async t => fixture(t, async dir => {
  const { store } = registeredStore(dir);
  assert.equal(JSON.parse(readFileSync(store.devicesPath, 'utf8')).deletion_ledger_required, true);
  const paths = [store.devicesPath, store.retentionFacts.path, store.retentionFacts.marker];
  const before = paths.map(path => readFileSync(path));
  rmSync(store.deletionTombstonesPath);
  for (const apply of [false, true]) {
    await assert.rejects(backfillRetention({ dataDir:dir, now, apply }), /missing required deletion inventory/);
    paths.forEach((path, index) => assert.deepEqual(readFileSync(path), before[index]));
    assert.equal(existsSync(store.deletionTombstonesPath), false);
  }
}));

test('failed retention persistence prevents raw compaction and damaged authority fails closed', t => fixture(t, dir => {
  const { store, registered } = registeredStore(dir);
  store.behaviorEvents = [success('2025-01-01T08:00:00',registered.device_id)];
  store.retentionFacts.flush = () => { throw new Error('fixture disk failure'); };
  assert.throws(()=>store.compactBehaviorEvents(now), /retention_storage_unavailable/);
  assert.equal(store.behaviorEvents.length, 1);
  writeFileSync(store.retentionFacts.path, '{broken');
  assert.throws(()=>testStore(dir,store.pepper));
  assert.equal(readFileSync(store.retentionFacts.path,'utf8'),'{broken');
}));

test('an initialized but missing facts file is not mistaken for first deployment', t => fixture(t, dir => {
  const { store }=registeredStore(dir);
  rmSync(store.retentionFacts.path);
  assert.throws(()=>testStore(dir,store.pepper),/missing or invalid retention authority/);
}));

test('backfill dry run is read-only; apply deduplicates snapshots and preserves the live raw log', async t => fixture(t, async dir => {
  const { store, registered } = registeredStore(dir);
  const current = [success('2026-10-01T08:00:00',registered.device_id)];
  writeFileSync(store.behaviorEventsPath,current.map(e=>JSON.stringify(e)).join('\n')+'\n');
  const source = join(dir,'backup.jsonl');
  const older = success('2026-09-01T08:00:00',registered.device_id);
  writeFileSync(source,[older,older,...current,success('2026-09-01T08:00:00','removed')].map(e=>JSON.stringify(e)).join('\n')+'\n');
  const rawBefore=readFileSync(store.behaviorEventsPath), factsBefore=readFileSync(store.retentionFacts.path);
  const dry=await backfillRetention({dataDir:dir,sources:[source,source],now});
  assert.equal(dry.after.d30.retained_installations,1);
  assert.equal(dry.after.d30.history_status,'partial');
  assert.deepEqual(readFileSync(store.retentionFacts.path),factsBefore);
  const applied=await backfillRetention({dataDir:dir,sources:[source],now,apply:true});
  assert.deepEqual(applied.after,dry.after);
  assert.deepEqual(readFileSync(store.behaviorEventsPath),rawBefore);
  const reopened=testStore(dir,store.pepper);
  assert.equal(reopened.retentionFacts.metrics(now).d30.retained_installations,1);
  assert.equal(reopened.retentionFacts.rows.size,1);
}));

test('daily timestamps normalize seconds, confidence starts at 30, and state stays private and minimal', t => fixture(t, dir => {
  const state = new RetentionFacts(dir,at('2026-08-01T00:00:00'));
  const devices = Array.from({length:30},(_,i)=>device(`device_${i}`));
  const events = devices.flatMap(d=>[success('2026-09-01T08:00:00',d.device_id),success('2026-10-01T08:00:00',d.device_id,{occurred_at:at('2026-10-01T08:00:00')/1000,private_body:'must-not-persist'})]);
  state.observe(events,devices,now);
  assert.equal(state.metrics(now).d30.retention_rate,1);
  assert.equal(state.metrics(now).d30.history_status,'complete');
  assert.equal(state.metrics(now).d30.history_incomplete_installations, 0);
  assert.equal(state.metrics(now).d30.observation, 'realtime');
  for (const horizon of [1, 7, 30]) assert.equal(state.metrics(now)[`d${horizon}`].horizon_days, horizon);
  state.flush();
  const raw=readFileSync(state.path,'utf8');
  assert.equal(raw.includes('must-not-persist'),false);
  assert.deepEqual(new RetentionFacts(dir).metrics(now),state.metrics(now));
}));

test('clock skew cannot silently restore a cohort with uncertain history to complete', t => fixture(t, dir => {
  const initializedAt = at('2026-09-01T08:00:00');
  const state = new RetentionFacts(dir, initializedAt);
  const registered = { ...device(), first_seen_at:initializedAt + 1_000 };
  state.observe([success('2026-09-01T08:00:00')], [registered], now);
  assert.equal(state.rows.get('fixture').history_complete, false);
  state.observe([success('2026-10-01T08:00:00')], [registered], now);
  state.flush();
  const reopened = new RetentionFacts(dir);
  reopened.observe([], [registered], now);
  assert.equal(reopened.metrics(now).d30.retained_installations, 1);
  assert.equal(reopened.metrics(now).d30.history_status, 'partial');
  assert.equal(reopened.metrics(now).d30.history_incomplete_installations, 1);
}));

test('offline deletion filtering shares online trimming of subject IDs', async t => fixture(t, async dir => {
  const { store, registered } = registeredStore(dir);
  const padded = `  ${registered.device_id}  `;
  const deviceRow = { ...registered, device_id:padded };
  store.retentionFacts.observe([success('2026-09-01T08:00:00', padded)], [deviceRow], now);
  store.retentionFacts.flush();
  writeFileSync(store.devicesPath, JSON.stringify({ devices:[deviceRow] }));
  writeFileSync(store.deletionTombstonesPath, JSON.stringify({ records:[{ subject_hash:store.deletionSubjectHash(registered.device_id) }] }));
  const warnings = [];
  t.mock.method(console, 'warn', value => warnings.push(value));
  const result = await backfillRetention({ dataDir:dir, now, pepper:store.pepper, apply:true });
  assert.deepEqual(warnings, []);
  assert.equal(result.rows_dropped, 1);
  assert.equal(result.after.d30.eligible_installations, 0);
  assert.equal(new RetentionFacts(dir).rows.size, 0);
  assert.equal(testStore(dir, store.pepper).devices.size, 0);
}));

test('backfill excludes a deleted identity while preserving a fresh registration on the same claim', async t => fixture(t, async dir => {
  const {store,registered}=registeredStore(dir);
  store.deleteBehaviorData(registered,{request_id:'delete_backfill_subject_001'});
  const fresh=store.register({hardware_claim:'fixture-installation-retention',registration_secret:'different-fixture-registration-secret-32',hardware_source:'installation_id'});
  store.saveNow();
  const source=join(dir,'backup.jsonl');
  writeFileSync(source,[success('2026-09-01T08:00:00',registered.device_id),success('2026-09-01T08:00:00',fresh.device_id),success('2026-10-01T08:00:00',fresh.device_id)].map(e=>JSON.stringify(e)).join('\n'));
  await assert.rejects(backfillRetention({dataDir:dir,sources:[source],now,pepper:''}),/deletion filtering requires/);
  const result=await backfillRetention({dataDir:dir,sources:[source],now,pepper:store.pepper,apply:true});
  assert.equal(result.after.d30.eligible_installations,1);
  assert.equal(result.after.d30.retained_installations,1);
  assert.deepEqual([...new RetentionFacts(dir).rows.keys()],[fresh.device_id]);
}));

test('empty backfill preserves the documented before-row and after-inventory history baselines', async t => fixture(t, async dir => {
  const state = new RetentionFacts(dir, now);
  state.flush();
  writeFileSync(join(dir, 'devices.json'), JSON.stringify({ devices:[device()] }));
  const result = await backfillRetention({ dataDir:dir, now });
  assert.equal(result.scanned_events, 0);
  assert.equal(result.rows_dropped, 0);
  assert.equal(result.before.d30.history_status, 'complete');
  assert.equal(result.before.d30.history_unresolved_installations, 0);
  assert.equal(result.after.d30.history_status, 'partial');
  assert.equal(result.after.d30.history_unresolved_installations, 1);
  assert.equal(result.after.d30.eligible_installations, 0);
  assert.deepEqual(JSON.parse(readFileSync(state.path, 'utf8')).rows, []);
}));
