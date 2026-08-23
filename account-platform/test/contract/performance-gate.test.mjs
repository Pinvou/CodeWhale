import assert from 'node:assert/strict';
import test from 'node:test';
import { validateEvidence, validatePolicy } from '../../../scripts/account-platform-performance-gate.mjs';

const policy = {
  schema_version: 1,
  baseline: { steady_minutes: 30, minimum_samples: 10000, minimum_stream_samples: 1000, maximum_declared_capacity_percent: 70 },
  api: { read_p95_ms: 300, read_p99_ms: 1000, write_p95_ms: 500, write_p99_ms: 1500, unexpected_error_percent: 0.1 },
  external: { end_to_end_p95_ms: 3000, end_to_end_p99_ms: 8000, platform_p95_ms: 500, platform_p99_ms: 1500 },
  stream: { dispatch_p95_ms: 500, dispatch_p99_ms: 1500, content_p95_ms: 5000, content_p99_ms: 15000, concurrent: 200, unexpected_error_percent: 0.5 },
  holds: { active: 100000, steady_tps: 200, burst_tps: 500, burst_seconds: 60, lock_wait_p99_ms: 500 },
  payments: { orders: 1000000, transactions: 3000000, order_create_tps: 50, evidence_merge_tps: 200 },
  events: { retained_unacked_per_stream: 1000000, backlog: 100000, drain_seconds: 900, realtime_peak_percent: 50 },
};

function evidence(now) {
  return {
    schema_version: 1, environment: 'production-equivalent', generated_at: new Date(now).toISOString(),
    release_sha256: 'a'.repeat(64), dataset_sha256: 'b'.repeat(64), report_sha256: 'c'.repeat(64), steady_minutes: 30, maximum_declared_capacity_percent: 70,
    api: { samples: 10000, ...policy.api }, external: { ...policy.external },
    stream: { samples: 1000, ...policy.stream },
    holds: { ...policy.holds, conservation_violations: 0, duplicate_effects: 0 },
    payments: { ...policy.payments, duplicate_financial_effects: 0 },
    events: { ...policy.events, lost_events: 0, high_risk_gate_triggered: true },
    timing_segments: { platform_ms: 1, provider_ms: 1, queue_ms: 1, client_network_ms: 1 },
  };
}

test('performance evidence enforces samples, SLOs, capacity, conservation, and backlog drain', () => {
  const now = Date.parse('2026-08-24T00:00:00Z');
  assert.doesNotThrow(() => validatePolicy(policy));
  assert.doesNotThrow(() => validateEvidence(policy, evidence(now), now));
  const slow = evidence(now); slow.api.read_p95_ms = 301;
  assert.throws(() => validateEvidence(policy, slow, now), /api.read_p95_ms/);
  const duplicate = evidence(now); duplicate.payments.duplicate_financial_effects = 1;
  assert.throws(() => validateEvidence(policy, duplicate, now), /duplicate financial/);
});
