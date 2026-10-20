// Durable, minimal retention facts. Raw telemetry can expire independently.
import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { MIN_RELIABLE_SAMPLE, isSuccessfulCoreUserTask, normalizeUsageOccurredAt, statsDay as retentionDay, statsDayFromMilliseconds } from './telemetry-primitives.js';
import { atomicWriteJson, enforcePrivateFile } from './telemetry-storage.js';
export { statsDay as retentionDay } from './telemetry-primitives.js';

export class RetentionFacts {
  constructor(dataDir = null, now = Date.now()) {
    this.path = dataDir ? join(dataDir, 'retention-facts.json') : null;
    this.marker = dataDir ? join(dataDir, 'retention-facts.initialized') : null;
    this.rows = new Map();
    this.initializedAt = now;
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
    if (value.version !== 1 || !Number.isFinite(value.initialized_at)
      || value.initialized_at <= 0 || !Array.isArray(value.rows)) throw new Error('invalid retention facts');
    this.initializedAt = value.initialized_at;
    for (const row of value.rows) {
      if (typeof row.device_id !== 'string' || !row.device_id || row.device_id.length > 128
        || this.rows.has(row.device_id) || !Number.isInteger(row.first_success_day)
        || row.first_success_day <= 0 || typeof row.history_complete !== 'boolean'
        || !Array.isArray(row.successful_days) || row.successful_days.length > 31
        || !row.successful_days.includes(row.first_success_day)
        || row.successful_days.some(day => !Number.isInteger(day)
          || day < row.first_success_day || day > row.first_success_day + 30)) throw new Error('invalid retention row');
      this.rows.set(row.device_id, { ...row, successful_days: [...new Set(row.successful_days)] });
    }
    this.unresolvedInstallations = [...this.rows.values()].filter(row => !row.history_complete).length;
  }

  observe(events, devices, now = Date.now()) {
    const eligible = new Map(devices.filter(d => d.hardware_source === 'installation_id').map(d => [d.device_id, d]));
    for (const id of this.rows.keys()) {
      if (!eligible.has(id)) { this.rows.delete(id); this.dirty = true; }
    }
    for (const event of events) {
      const id = event.identity_id || event.device_id;
      const device = eligible.get(id);
      if (!device || (event.identity_id && event.device_id && event.identity_id !== event.device_id)
        || !isSuccessfulCoreUserTask(event)) continue;
      const timestamp = normalizeUsageOccurredAt(event.occurred_at);
      const day = statsDayFromMilliseconds(timestamp);
      if (!Number.isInteger(day) || day < 1 || timestamp > now) continue;
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
    atomicWriteJson(this.path, { version: 1, initialized_at: this.initializedAt, rows: [...this.rows.values()] });
    if (!existsSync(this.marker)) writeFileSync(this.marker, '1\n', { mode: 0o600, flag: 'wx' });
    enforcePrivateFile(this.marker);
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
