import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { RetentionFacts } from '../retention-store.js';
import { TelemetryStore, buildBehaviorOverview } from '../telemetry-service.js';
import { backfillRetention } from '../scripts/backfill-retention.mjs';

const at = s => Date.parse(s + '+08:00');
const DAY = 86400000;
const now = at('2026-10-20T10:00:00');
const device = (id = 'fixture') => ({ device_id: id, hardware_source: 'installation_id', first_seen_at: at('2026-09-01T08:00:00') });
const success = (time, id = 'fixture', extra = {}) => ({ device_id: id, event_id: `${id}_${time}`, attempt_id: `${id}_${time}`, event_name: 'task_finished', status: 'success', initiator_type: 'user', mode: 'assistant', occurred_at: at(time), received_at: at(time), ...extra });
const facts = (events, devices = [device()], time = now) => {
  const result = new RetentionFacts(null, at('2026-08-01T00:00:00'));
  result.observe(events, devices, time);
  return result;
};
function fixture(t, action) {
  const dir = mkdtempSync(join(tmpdir(), 'pinvou-retention-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  return action(dir);
}
function registeredStore(dir) {
  const store = new TelemetryStore(dir, 'fixture-pepper-at-least-24-characters');
  const credential = store.register({ hardware_claim: 'fixture-installation-retention', registration_secret: 'fixture-registration-secret-at-least-32', hardware_source: 'installation_id' });
  const registered = store.authenticate(credential.device_id, credential.device_token);
  store.saveNow();
  return { store, registered };
}

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
  const { store, registered } = registeredStore(dir);
  store.behaviorEvents = ['2026-09-01T08:00:00','2026-09-02T08:00:00','2026-09-08T08:00:00','2026-10-01T08:00:00'].map(time => success(time,registered.device_id));
  store.compactBehaviorEvents(at('2026-12-01T00:00:00'));
  assert.equal(store.behaviorEvents.length, 0);
  for (const h of [1,7,30]) assert.equal(store.retentionFacts.metrics(now)[`d${h}`].retained_installations, 1);
  const reopened = new TelemetryStore(dir, store.pepper);
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
  const { store, registered } = registeredStore(dir);
  const old = ['2025-01-01T08:00:00','2025-01-31T08:00:00'].map(time => success(time,registered.device_id));
  writeFileSync(store.behaviorEventsPath, old.map(e=>JSON.stringify(e)).join('\n')+'\n');
  const migrated = new TelemetryStore(dir, store.pepper);
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
  const reopened=new TelemetryStore(dir,store.pepper);
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
  const reopened = new TelemetryStore(dir, store.pepper);
  assert.equal(reopened.behaviorOverview('30').retention.d30.eligible_installations, 0);
  assert.equal(new RetentionFacts(dir).rows.size, 0);
}));

test('processing deletion is recovered before retention becomes visible', t => fixture(t, dir => {
  const { store, registered } = registeredStore(dir);
  store.behaviorEvents = [success('2026-09-01T08:00:00',registered.device_id)];
  store.persistRetentionFacts(now);
  store.retentionFacts.flush = () => { throw new Error('fixture disk failure'); };
  assert.throws(()=>store.deleteBehaviorData(registered,{request_id:'delete_pending_retention_001'}), /retention_storage_unavailable/);
  const reopened = new TelemetryStore(dir, store.pepper);
  assert.equal(reopened.retentionFacts.rows.size, 0);
  assert.equal(reopened.deletionTombstones.at(-1).status, 'completed');
}));

test('failed retention persistence prevents raw compaction and damaged authority fails closed', t => fixture(t, dir => {
  const { store, registered } = registeredStore(dir);
  store.behaviorEvents = [success('2025-01-01T08:00:00',registered.device_id)];
  store.retentionFacts.flush = () => { throw new Error('fixture disk failure'); };
  assert.throws(()=>store.compactBehaviorEvents(now), /retention_storage_unavailable/);
  assert.equal(store.behaviorEvents.length, 1);
  writeFileSync(store.retentionFacts.path, '{broken');
  assert.throws(()=>new TelemetryStore(dir,store.pepper));
  assert.equal(readFileSync(store.retentionFacts.path,'utf8'),'{broken');
}));

test('an initialized but missing facts file is not mistaken for first deployment', t => fixture(t, dir => {
  const { store }=registeredStore(dir);
  rmSync(store.retentionFacts.path);
  assert.throws(()=>new TelemetryStore(dir,store.pepper),/missing or invalid retention authority/);
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
  const reopened=new TelemetryStore(dir,store.pepper);
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
  state.flush();
  const raw=readFileSync(state.path,'utf8');
  assert.equal(raw.includes('must-not-persist'),false);
  assert.deepEqual(new RetentionFacts(dir).metrics(now),state.metrics(now));
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
