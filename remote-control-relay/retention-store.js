// Durable, minimal retention facts. Raw telemetry can expire independently.
import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { MIN_RELIABLE_SAMPLE, isSuccessfulCoreUserTask, retentionOccurredAt, statsDay as retentionDay, statsDayFromMilliseconds } from './telemetry-primitives.js';
import { atomicWriteJson, atomicWritePrivateFile, enforcePrivateFile } from './telemetry-storage.js';
export { statsDay as retentionDay } from './telemetry-primitives.js';

// The last Beijing calendar day containing a representable JavaScript Date.
// Do not compare stored days to today's clock: offline restores and clock
// rollback must not invalidate an otherwise representable authority.
const MAX_RETENTION_DAY = statsDayFromMilliseconds(8_640_000_000_000_000);
const validRetentionDay = day => Number.isSafeInteger(day) && day > 0 && day <= MAX_RETENTION_DAY;

export class RetentionFacts {
  constructor(dataDir = null, now = Date.now()) {
    this.path = dataDir ? join(dataDir, 'retention-facts.json') : null;
    this.marker = dataDir ? join(dataDir, 'retention-facts.initialized') : null;
    this.rows = new Map();
    this.initializedAt = now;
    this.metricVersionStartedAt = null;
    this.dirty = Boolean(this.marker && !existsSync(this.marker));
    this.unresolvedInstallations = 0;
    if (this.marker && existsSync(this.marker)) {
      enforcePrivateFile(this.marker);
      if (readFileSync(this.marker, 'utf8') !== '1\n' || !existsSync(this.path)) throw new Error('missing or invalid retention authority');
    }
    if (!this.path || !existsSync(this.path)) {
      this.dirty = true;
      return;
    }
    enforcePrivateFile(this.path);
    // A damaged authority must never be replaced with an empty successful metric.
    const value = JSON.parse(readFileSync(this.path, 'utf8'));
    if (!value || typeof value !== 'object' || Array.isArray(value)
      || value.version !== 1 || !Number.isFinite(value.initialized_at)
      || value.initialized_at <= 0 || !Array.isArray(value.rows)) throw new Error('invalid retention facts');
    this.initializedAt = value.initialized_at;
    if (value.metric_version_started_at === undefined) this.dirty = true;
    else {
      if (!Number.isFinite(value.metric_version_started_at) || value.metric_version_started_at <= 0) throw new Error('invalid retention facts');
      this.metricVersionStartedAt = value.metric_version_started_at;
    }
    for (const row of value.rows) {
      if (!row || typeof row !== 'object' || Array.isArray(row)
        || typeof row.device_id !== 'string' || !row.device_id || row.device_id.length > 128
        || this.rows.has(row.device_id) || !validRetentionDay(row.first_success_day)
        || typeof row.history_complete !== 'boolean'
        || !Array.isArray(row.successful_days) || row.successful_days.length > 31
        || !row.successful_days.includes(row.first_success_day)
        || row.successful_days.some(day => !validRetentionDay(day)
          || day < row.first_success_day || day > row.first_success_day + 30)) throw new Error('invalid retention row');
      const days = [...new Set(row.successful_days)].sort((a, b) => a - b);
      if (days.length !== row.successful_days.length || days.some((day, index) => day !== row.successful_days[index])) this.dirty = true;
      this.rows.set(row.device_id, { ...row, successful_days: days });
    }
    this.unresolvedInstallations = [...this.rows.values()].filter(row => !row.history_complete).length;
  }

  observe(events, devices, now = Date.now(), wasDeleted = () => false) {
    const inventoryIds = new Set(devices.map(device => device.device_id));
    const unexplained = [...this.rows.keys()].filter(id => !inventoryIds.has(id) && !wasDeleted(id)).length;
    // Preserve inventory-authoritative reconciliation, but never hide an
    // unmatched facts restore. Counts only: no subject IDs or hashes in logs.
    if (unexplained) console.warn(JSON.stringify({ event:'retention_inventory_omission', rows:unexplained }));
    const eligible = new Map(devices.filter(d => d.hardware_source === 'installation_id').map(d => [d.device_id, d]));
    for (const id of this.rows.keys()) {
      if (!eligible.has(id)) { this.rows.delete(id); this.dirty = true; }
    }
    for (const event of events) {
      const id = event.identity_id || event.device_id;
      const device = eligible.get(id);
      if (!device || (event.identity_id && event.device_id && event.identity_id !== event.device_id)
        || !isSuccessfulCoreUserTask(event)) continue;
      const timestamp = retentionOccurredAt(event);
      const day = statsDayFromMilliseconds(timestamp);
      if (!validRetentionDay(day) || timestamp > now) continue;
      let row = this.rows.get(id);
      if (!row) {
        row = {
          device_id: id, first_success_day: day, successful_days: [],
          // Existing installations may have successes older than the raw window.
          history_complete: Number(device.first_seen_at) >= this.initializedAt,
        };
        this.rows.set(id, row);
        this.dirty = true;
      }
      if (day < row.first_success_day) {
        row.first_success_day = day;
        row.successful_days = row.successful_days.filter(d => d <= day + 30);
        this.dirty = true;
      }
      if (row.history_complete && timestamp < Number(device.first_seen_at)) {
        row.history_complete = false;
        this.dirty = true;
      }
      if (day <= row.first_success_day + 30 && !row.successful_days.includes(day)) {
        row.successful_days.push(day);
        row.successful_days.sort((a, b) => a - b);
        this.dirty = true;
      }
    }
    this.unresolvedInstallations = [...eligible.values()].filter(device => (
      Number(device.first_seen_at) < this.initializedAt || !Number(device.first_seen_at)
      || this.rows.get(device.device_id)?.history_complete === false
    )).length;
  }

  flush() {
    if (!this.path || !this.dirty) return;
    // This instance's first successful activation of the new metric, not a
    // guessed deployment date or the older history-completeness boundary.
    const metricVersionStartedAt = this.metricVersionStartedAt ?? Date.now();
    atomicWriteJson(this.path, { version: 1, initialized_at: this.initializedAt,
      metric_version_started_at: metricVersionStartedAt, rows: [...this.rows.values()] });
    if (!existsSync(this.marker)) atomicWritePrivateFile(this.marker, '1\n');
    enforcePrivateFile(this.marker);
    this.metricVersionStartedAt = metricVersionStartedAt;
    this.dirty = false;
  }

  metrics(now = Date.now()) {
    const today = retentionDay(now);
    return Object.fromEntries([1, 7, 30].map(horizon => {
      let eligible = 0, retained = 0, provisional = 0, incomplete = 0;
      for (const row of this.rows.values()) {
        const target = row.first_success_day + horizon;
        if (target > today) continue;
        eligible += 1;
        if (row.successful_days.includes(target)) retained += 1;
        if (target === today) provisional += 1;
        if (!row.history_complete) incomplete += 1;
      }
      return [`d${horizon}`, {
        horizon_days: horizon, eligible_installations: eligible, retained_installations: retained,
        retention_rate: eligible >= MIN_RELIABLE_SAMPLE ? retained / eligible : null,
        confidence: eligible >= MIN_RELIABLE_SAMPLE ? 'sufficient' : 'low',
        basis: 'first_success_beijing_day', observation: 'realtime',
        provisional_installations: provisional, history_incomplete_installations: incomplete,
        history_status: this.unresolvedInstallations ? 'partial' : 'complete',
        history_unresolved_installations: this.unresolvedInstallations,
      }];
    }));
  }
}

export function retentionFromEvents(devices, events, now) {
  const facts = new RetentionFacts(null, now);
  facts.observe(events, devices, now);
  return facts.metrics(now);
}
