// Offline migration: never restore raw backup events into the live event log.
import { createReadStream, readFileSync } from 'node:fs';
import crypto from 'node:crypto';
import { join, resolve } from 'node:path';
import { createInterface } from 'node:readline';
import { pathToFileURL } from 'node:url';
import { RetentionFacts, retentionDay } from '../retention-store.js';

export async function backfillRetention({ dataDir, sources = [], apply = false, now = Date.now(), pepper = process.env.PINVOU_TELEMETRY_DEVICE_PEPPER }) {
  const saved = JSON.parse(readFileSync(join(dataDir, 'devices.json'), 'utf8'));
  // Pending deletions must also be excluded when the service has been stopped.
  let tombstones;
  try { tombstones = JSON.parse(readFileSync(join(dataDir, 'behavior-deletion-tombstones.json'), 'utf8')); }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
  const removedSubjects = new Set((tombstones?.records || []).map(row => row.subject_hash).filter(Boolean));
  if (removedSubjects.size && !pepper) throw new Error('deletion filtering requires PINVOU_TELEMETRY_DEVICE_PEPPER');
  const devices = (saved.devices || []).filter(row => !removedSubjects.has(
    crypto.createHmac('sha256', pepper || '').update(`deletion-subject:${String(row.device_id).slice(0,96)}`).digest('hex'),
  ));
  const facts = new RetentionFacts(dataDir, now);
  facts.observe([], devices, now);
  const before = facts.metrics(now);
  const legacyCohorts = new Map();
  const legacyActivity = new Map();
  const eligibleIds = new Set(devices.filter(d => d.hardware_source === 'installation_id').map(d => d.device_id));
  let malformed = 0, scanned = 0;
  for (const path of new Set([join(dataDir, 'behavior-events.jsonl'), ...sources].map(p => resolve(p)))) {
    const stream = createReadStream(path, { encoding: 'utf8' });
    const lines = createInterface({ input: stream, crlfDelay: Infinity });
    try {
      let chunk = [];
      for await (const line of lines) {
        if (!line.trim()) continue;
        let event;
        try { event = JSON.parse(line); } catch { malformed += 1; continue; }
        scanned += 1;
        const id = event.identity_id || event.device_id;
        const timestamp = Number(event.occurred_at) < 1e12 ? Number(event.occurred_at) * 1000 : Number(event.occurred_at);
        const day = retentionDay(timestamp);
        if (!eligibleIds.has(id) || !Number.isInteger(day) || timestamp > now
          || (event.identity_id && event.device_id && event.identity_id !== event.device_id)) continue;
        if (event.event_name === 'analytics_cohort_started') {
          legacyCohorts.set(id, Math.min(day, legacyCohorts.get(id) ?? day));
        }
        if (event.event_name === 'task_finished' && event.status === 'success'
          && event.initiator_type === 'user' && ['assistant', 'code', 'team'].includes(event.mode)) {
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
  return { applied: apply, scanned_events: scanned, malformed_lines: malformed, before, after, legacy_cohort_start: legacy };
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  const args = process.argv.slice(2);
  const sources = [];
  let dataDir, apply = false, offline = false;
  for (let i = 0; i < args.length; i++) {
    if (args[i] === '--data-dir') dataDir = args[++i];
    else if (args[i] === '--source') sources.push(args[++i]);
    else if (args[i] === '--apply') apply = true;
    else if (args[i] === '--offline') offline = true;
    else throw new Error('usage: --data-dir DIR [--source JSONL] [--apply --offline]');
  }
  if (!dataDir || sources.some(p => !p) || (apply && !offline)) throw new Error('apply requires an offline service: --data-dir DIR [--source JSONL] [--apply --offline]');
  console.log(JSON.stringify(await backfillRetention({ dataDir, sources, apply }), null, 2));
}
