import assert from 'node:assert/strict';
import test from 'node:test';
import { evidenceTimestamp } from '../../../scripts/account-platform-evidence-time.mjs';

test('evidence times preserve valid RFC3339 offsets and fractional seconds but reject normalized calendar errors', () => {
  for (const value of ['2024-02-29T12:34:56Z', '2026-03-02T12:34:56.123Z', '2026-03-02t12:34:56z', '2026-03-02T12:34:56.1234567890Z', '2026-03-02T12:34:56+08:00', '2026-03-02T12:34:56-05:30']) assert.equal(evidenceTimestamp(value, 'controlled'), Date.parse(value));
  for (const value of ['2026-02-29T00:00:00Z', '2026-02-30T00:00:00Z', '2026-04-31T00:00:00Z', '2026-00-01T00:00:00Z', '2026-01-00T00:00:00Z', '2026-01-01T24:00:00Z', '2026-01-01T00:60:00Z', '2026-01-01T00:00:60Z', '2026-01-01T00:00:00+24:00', '2026-01-01T00:00:00+08:60', '2026-03-02', 'March 2, 2026', null, 0]) assert.throws(() => evidenceTimestamp(value, 'controlled'), /RFC3339/);
});
