// Offline migration: never restore raw backup events into the live event log.
import { createReadStream, existsSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { createInterface } from 'node:readline';
import { RetentionFacts, retentionDay } from '../retention-store.js';
import { deletionSubjectHash, isSuccessfulCoreUserTask, normalizeUsageOccurredAt, statsDayFromMilliseconds } from '../telemetry-primitives.js';
import { isDirectCliEntry } from '../cli-entrypoint.js';
import { validDeviceInventory } from '../telemetry-storage.js';

export async function backfillRetention({ dataDir, sources = [], apply = false, now = Date.now(), pepper = process.env.PINVOU_TELEMETRY_DEVICE_PEPPER }) {
  const saved = JSON.parse(readFileSync(join(dataDir, 'devices.json'), 'utf8'));
  if (!validDeviceInventory(saved)) throw new Error('invalid device inventory');
  // Pending deletions must also be excluded when the service has been stopped.
  let tombstones;
  try { tombstones = JSON.parse(readFileSync(join(dataDir, 'behavior-deletion-tombstones.json'), 'utf8')); }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
  if (tombstones !== undefined && (!tombstones || !Array.isArray(tombstones.records)
    || tombstones.records.some(row => !row || typeof row !== 'object'
      || typeof row.subject_hash !== 'string' || !row.subject_hash))) throw new Error('invalid deletion inventory');
  const removedSubjects = new Set((tombstones?.records || []).map(row => row.subject_hash).filter(Boolean));
  if (removedSubjects.size && !pepper) throw new Error('deletion filtering requires PINVOU_TELEMETRY_DEVICE_PEPPER');
  const devices = (saved.devices || []).filter(row => !removedSubjects.has(
    deletionSubjectHash(row.device_id, pepper || ''),
  ));
  const facts = new RetentionFacts(dataDir, now);
  const before = facts.metrics(now);
  const beforeRows = facts.rows.size;
  facts.observe([], devices, now);
  const rowsDropped = beforeRows - facts.rows.size;
  const legacyCohorts = new Map();
  const legacyActivity = new Map();
  const eligibleIds = new Set(devices.filter(d => d.hardware_source === 'installation_id').map(d => d.device_id));
  let malformed = 0, scanned = 0, invalidEvents = 0, invalidTimestamps = 0;
  const excluded = { ineligible_identity:0, future_timestamp:0, identity_mismatch:0 };
  const livePath = resolve(dataDir, 'behavior-events.jsonl');
  const sourcePaths = new Set(sources.map(path => resolve(path)));
  for (const path of new Set([livePath, ...sourcePaths])) {
    // Like online startup, a fresh deployment need not have a live event log.
    // Explicit backup sources must exist, even if they name that same path.
    if (path === livePath && !sourcePaths.has(path) && !existsSync(path)) continue;
    const stream = createReadStream(path, { encoding: 'utf8' });
    const lines = createInterface({ input: stream, crlfDelay: Infinity });
    try {
      let chunk = [];
      for await (const line of lines) {
        if (!line.trim()) continue;
        let event;
        try { event = JSON.parse(line); } catch { malformed += 1; continue; }
        scanned += 1;
        if (!event || typeof event !== 'object' || Array.isArray(event)) { invalidEvents += 1; continue; }
        const id = event.identity_id || event.device_id;
        const timestamp = normalizeUsageOccurredAt(event.occurred_at);
        const day = statsDayFromMilliseconds(timestamp);
        if (!Number.isInteger(day) || day < 1) { invalidTimestamps += 1; continue; }
        if (!eligibleIds.has(id)) { excluded.ineligible_identity += 1; continue; }
        if (timestamp > now) { excluded.future_timestamp += 1; continue; }
        if (event.identity_id && event.device_id && event.identity_id !== event.device_id) { excluded.identity_mismatch += 1; continue; }
        if (event.event_name === 'analytics_cohort_started') {
          legacyCohorts.set(id, Math.min(day, legacyCohorts.get(id) ?? day));
        }
        if (isSuccessfulCoreUserTask(event)) {
          if (!legacyActivity.has(id)) legacyActivity.set(id, new Set());
          legacyActivity.get(id).add(day);
          chunk.push(event);
        }
        if (chunk.length >= 1000) { facts.observe(chunk, devices, now); chunk = []; }
      }
      facts.observe(chunk, devices, now);
    } finally { lines.close(); stream.destroy(); }
  }
  const after = facts.metrics(now);
  const legacy = Object.fromEntries([1, 7, 30].map(h => {
    const rows = [...legacyCohorts].filter(([, day]) => day + h <= retentionDay(now));
    return [`d${h}`, { eligible_installations: rows.length, retained_installations: rows.filter(([id, day]) => legacyActivity.get(id)?.has(day + h)).length }];
  }));
  if (apply) facts.flush();
  return {
    applied:apply, scanned_events:scanned, malformed_lines:malformed,
    invalid_events:invalidEvents, invalid_timestamp_events:invalidTimestamps,
    excluded_events:excluded,
    rows_dropped:rowsDropped, before, after, legacy_cohort_start:legacy,
  };
}

const USAGE = 'usage: node backfill-retention.mjs --data-dir DIR [--source JSONL] [--apply --offline]';
function argumentError(message) {
  return Object.assign(new Error(message), { cliMessage:`${message}\n${USAGE}` });
}

function parseArguments(args) {
  const sources = [];
  let dataDir, apply = false, offline = false;
  if (args.includes('--help')) return { help:true };
  for (let i = 0; i < args.length; i++) {
    const option = args[i].split('=', 1)[0];
    if (option === '--data-dir' || option === '--source') {
      const inline = args[i].startsWith(`${option}=`);
      const value = inline ? args[i].slice(option.length + 1) : args[++i];
      if (!value || (!inline && value.startsWith('--'))) throw argumentError(`${option} requires a path`);
      if (option === '--source') sources.push(value);
      else {
        if (dataDir) throw argumentError('--data-dir may only be supplied once');
        dataDir = value;
      }
    } else if (args[i] === '--apply') apply = true;
    else if (args[i] === '--offline') offline = true;
    else throw argumentError('unknown argument');
  }
  if (!dataDir) throw argumentError('--data-dir is required');
  if (apply && !offline) throw argumentError('--apply requires --offline to confirm the service is stopped');
  return { dataDir, sources, apply };
}

try {
  if (isDirectCliEntry(import.meta.url)) {
    const options = parseArguments(process.argv.slice(2));
    if (options.help) {
      console.log(`${USAGE}\nDefault: read-only dry run. --source may be repeated.\nUse --data-dir=DIR or --source=JSONL for paths beginning with --.\nDeletion filtering requires PINVOU_TELEMETRY_DEVICE_PEPPER when tombstones exist.`);
    } else {
      console.log(JSON.stringify(await backfillRetention(options), null, 2));
    }
  }
} catch (error) {
  // Do not echo raw JSON or filesystem contents from a failed backup parse.
  const known = ['deletion filtering requires PINVOU_TELEMETRY_DEVICE_PEPPER', 'missing or invalid retention authority',
    'invalid retention facts', 'invalid retention row', 'invalid device inventory', 'invalid deletion inventory', 'unsafe telemetry storage file'];
  console.error(error.cliMessage || (known.includes(error.message) ? error.message
    : `retention backfill failed (${error.code || (error instanceof SyntaxError ? 'invalid_json' : 'storage_error')})`));
  process.exitCode = 1;
}
