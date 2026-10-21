// Shared telemetry identity and metric rules for online aggregation and offline repair.
import crypto from 'node:crypto';

export const DAY_MS = 86_400_000;
export const STATS_TIMEZONE_OFFSET_MS = 28_800_000;
export const MIN_RELIABLE_SAMPLE = 30;

export function boundedText(value, max = 128) {
  return String(value || '').trim().slice(0, max);
}

export function normalizeUsageOccurredAt(value) {
  const occurredAt = Number(value);
  if (!Number.isFinite(occurredAt) || occurredAt <= 0) return Number.NaN;
  return occurredAt >= 1_000_000_000_000 ? occurredAt : occurredAt * 1_000;
}

export function statsDay(timestamp) {
  return statsDayFromMilliseconds(normalizeUsageOccurredAt(timestamp));
}

export function statsDayFromMilliseconds(timestamp) {
  return Math.floor((timestamp + STATS_TIMEZONE_OFFSET_MS) / DAY_MS);
}

export function isCoreUserTask(event, eventName) {
  return event?.event_name === eventName
    && event.initiator_type === 'user'
    && ['assistant', 'code', 'team'].includes(event.mode);
}

export function isSuccessfulCoreUserTask(event) {
  return isCoreUserTask(event, 'task_finished') && event.status === 'success';
}

export function deletionSubjectHash(deviceId, pepper) {
  return crypto.createHmac('sha256', pepper)
    .update(`deletion-subject:${boundedText(deviceId, 96)}`).digest('hex');
}
