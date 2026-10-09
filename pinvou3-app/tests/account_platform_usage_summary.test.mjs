import test from 'node:test';
import assert from 'node:assert/strict';
import { summarizeUsageRecords, usageRecordReference } from '../src/features/account-platform/usage-summary.js';

test('Web personal center groups model calls by logical conversation turn', () => {
  const records = [
    { usage_request_id: 'request-2', logical_turn_id: 'turn-a', model: 'model-a', display_status: 'SETTLED', execution_status: 'SUCCEEDED', delivery_status: 'COMPLETED', settlement_status: 'SETTLED', actual_microcredits: 5_000, charged_microcredits: 5_000, input_tokens: 20, output_tokens: 4, cached_input_tokens: 1, started_at: '2026-08-28T08:01:00Z', completed_at: '2026-08-28T08:02:00Z' },
    { usage_request_id: 'request-1', logical_turn_id: 'turn-a', model: 'model-a', display_status: 'SETTLED', execution_status: 'SUCCEEDED', delivery_status: 'COMPLETED', settlement_status: 'SETTLED', actual_microcredits: 3_317, charged_microcredits: 3_317, input_tokens: 10, output_tokens: 2, cached_input_tokens: 0, started_at: '2026-08-28T08:00:00Z', completed_at: '2026-08-28T08:01:30Z' },
    { usage_request_id: 'legacy-request', model: 'model-a', display_status: 'RELEASED', execution_status: 'FAILED', delivery_status: 'COMPLETED', settlement_status: 'RELEASED', actual_microcredits: null, charged_microcredits: 0, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-08-27T08:00:00Z' },
  ];
  const summaries = summarizeUsageRecords(records);
  assert.equal(summaries.length, 2);
  assert.equal(summaries[0].logical_turn_id, 'turn-a');
  assert.equal(summaries[0].usage_request_count, 2);
  assert.equal(summaries[0].actual_microcredits, 8_317);
  assert.equal(summaries[0].charged_microcredits, 8_317);
  assert.equal(summaries[0].execution_status, '');
  assert.equal(summaries[0].settlement_status, '');
  assert.equal(summaries[0].input_tokens, 30);
  assert.equal(summaries[0].output_tokens, 6);
  assert.equal(summaries[0].cached_input_tokens, 1);
  assert.equal(summaries[0].started_at, '2026-08-28T08:00:00Z');
  assert.equal(summaries[0].completed_at, '2026-08-28T08:02:00Z');
  assert.equal(summaries[1].usage_request_count, 1);
  assert.equal(summaries[1].actual_microcredits, null);
  assert.equal(summaries[1].charged_microcredits, 0);
});

test('Web usage summary never merges a legacy request id with an equal logical turn id', () => {
  const collision = '0199-usage-collision';
  const summaries = summarizeUsageRecords([
    { usage_request_id: 'request-a', logical_turn_id: collision, display_status: 'RELEASED', settlement_status: 'RELEASED', charged_microcredits: 0 },
    { usage_request_id: collision, display_status: 'RELEASED', settlement_status: 'RELEASED', charged_microcredits: 0 },
  ]);
  assert.equal(summaries.length, 2);
});

test('Web usage summary keeps an unfinished request actionable within a grouped turn', () => {
  const [summary] = summarizeUsageRecords([
    { usage_request_id: 'request-complete', logical_turn_id: 'turn-active', display_status: 'SETTLED', execution_status: 'SUCCEEDED', delivery_status: 'COMPLETED', settlement_status: 'SETTLED', actual_microcredits: 1_000, charged_microcredits: 1_000, input_tokens: 10, output_tokens: 2, cached_input_tokens: 0, started_at: '2026-08-28T08:00:00Z', completed_at: '2026-08-28T08:01:00Z' },
    { usage_request_id: 'request-active', logical_turn_id: 'turn-active', display_status: 'IN_PROGRESS', execution_status: 'UPSTREAM_DISPATCHING', delivery_status: 'DELIVERING', settlement_status: 'HOLD_ACTIVE', actual_microcredits: null, charged_microcredits: null, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-08-28T08:02:00Z' },
  ]);
  assert.equal(summary.cancel_usage_request_id, 'request-active');
  assert.equal(summary.display_status, 'IN_PROGRESS');
  assert.equal(summary.execution_status, '');
  assert.equal(summary.settlement_status, '');
  assert.equal(summary.actual_microcredits, null);
  assert.equal(summary.charged_microcredits, null);
  assert.equal(summary.completed_at, '');
});

test('Web usage summary derives charge status from every request in the logical turn', () => {
  const summaries = summarizeUsageRecords([
    { usage_request_id: 'released-retry', logical_turn_id: 'turn-mixed', display_status: 'RELEASED', execution_status: 'FAILED', delivery_status: 'DELIVERED', settlement_status: 'RELEASED', actual_microcredits: 56_300, charged_microcredits: 0, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-09-15T11:54:01Z', completed_at: '2026-09-15T12:00:00Z' },
    { usage_request_id: 'settled-call', logical_turn_id: 'turn-mixed', display_status: 'SETTLED', execution_status: 'SUCCEEDED', delivery_status: 'DELIVERED', settlement_status: 'SETTLED', actual_microcredits: 36_800, charged_microcredits: 36_800, input_tokens: 14_199, output_tokens: 1_053, cached_input_tokens: 128, started_at: '2026-09-15T11:54:00Z', completed_at: '2026-09-15T11:55:00Z' },
    { usage_request_id: 'released-only', logical_turn_id: 'turn-released', display_status: 'RELEASED', execution_status: 'FAILED', delivery_status: 'DELIVERED', settlement_status: 'RELEASED', actual_microcredits: 49_900, charged_microcredits: 0, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-09-15T06:51:00Z', completed_at: '2026-09-15T06:52:00Z' },
  ]);
  assert.equal(summaries[0].display_status, 'SETTLED');
  assert.equal(summaries[0].execution_status, '');
  assert.equal(summaries[0].settlement_status, '');
  assert.equal(summaries[0].actual_microcredits, 93_100);
  assert.equal(summaries[0].charged_microcredits, 36_800);
  assert.equal(summaries[0].input_tokens, null);
  assert.equal(summaries[0].output_tokens, null);
  assert.equal(summaries[0].cached_input_tokens, null);
  assert.equal(summaries[0].cancel_usage_request_id, '');
  assert.equal(summaries[1].display_status, 'RELEASED');
  assert.equal(summaries[1].actual_microcredits, 49_900);
  assert.equal(summaries[1].charged_microcredits, 0);
});

test('Web usage summary treats missing Tokens as zero only before dispatch', () => {
  const [summary] = summarizeUsageRecords([
    { usage_request_id: 'cancelled', logical_turn_id: 'turn-cancelled', display_status: 'CANCELLED_BEFORE_DISPATCH', settlement_status: 'RELEASED', actual_microcredits: null, charged_microcredits: 0, input_tokens: null, output_tokens: null, cached_input_tokens: null },
    { usage_request_id: 'settled', logical_turn_id: 'turn-cancelled', display_status: 'SETTLED', settlement_status: 'SETTLED', actual_microcredits: 1_000, charged_microcredits: 1_000, input_tokens: 10, output_tokens: 2, cached_input_tokens: 1 },
  ]);
  assert.equal(summary.input_tokens, 10);
  assert.equal(summary.output_tokens, 2);
  assert.equal(summary.cached_input_tokens, 1);
});

test('Web usage summary consumes a bounded server-side turn projection', () => {
  const [summary] = summarizeUsageRecords([{
    usage_request_id: 'head-request', logical_turn_id: 'large-turn', usage_request_count: 10_000,
    cancel_usage_request_id: 'active-request', display_status: 'MANUAL_REVIEW', execution_status: '', delivery_status: '', settlement_status: '',
    actual_microcredits: null, charged_microcredits: null, input_tokens: null, output_tokens: null, cached_input_tokens: null,
  }]);
  assert.equal(summary.usage_request_count, 10_000);
  assert.equal(summary.cancel_usage_request_id, 'active-request');
  assert.equal(summary.display_status, 'MANUAL_REVIEW');
});

test('Web usage summary does not offer cancellation after execution enters settlement or review', () => {
  const summaries = summarizeUsageRecords([
    { usage_request_id: 'settlement-pending', logical_turn_id: 'turn-pending', display_status: 'SETTLEMENT_PENDING', execution_status: 'SETTLEMENT_PENDING', delivery_status: 'DELIVERED', settlement_status: 'USAGE_PENDING', actual_microcredits: 30_700, charged_microcredits: null, input_tokens: 14_196, output_tokens: 293, cached_input_tokens: 128, started_at: '2026-09-15T09:40:00Z' },
    { usage_request_id: 'review', logical_turn_id: 'turn-review', display_status: 'MANUAL_REVIEW', execution_status: 'REVIEW', delivery_status: 'DELIVERED', settlement_status: 'REVIEW', actual_microcredits: null, charged_microcredits: null, input_tokens: null, output_tokens: null, cached_input_tokens: null, started_at: '2026-09-15T05:45:00Z', completed_at: '2026-09-15T05:46:00Z' },
  ]);
  for (const summary of summaries) {
    assert.equal(summary.charged_microcredits, null);
    assert.equal(summary.cancel_usage_request_id, '');
  }
});

test('Web usage summary keeps manual review visible beside an active request', () => {
  const [summary] = summarizeUsageRecords([
    { usage_request_id: 'active', logical_turn_id: 'turn-review-active', display_status: 'IN_PROGRESS', execution_status: 'STREAMING', delivery_status: 'DELIVERING', settlement_status: 'HOLD_ACTIVE', actual_microcredits: null, charged_microcredits: null, started_at: '2026-09-15T05:45:01Z' },
    { usage_request_id: 'review', logical_turn_id: 'turn-review-active', display_status: 'MANUAL_REVIEW', execution_status: 'REVIEW', delivery_status: 'DELIVERED', settlement_status: 'REVIEW', actual_microcredits: 12_300, charged_microcredits: null, started_at: '2026-09-15T05:45:00Z' },
  ]);
  assert.equal(summary.display_status, 'MANUAL_REVIEW');
  assert.equal(summary.cancel_usage_request_id, 'active');
  assert.equal(summary.execution_status, '');
  assert.equal(summary.delivery_status, '');
  assert.equal(summary.settlement_status, '');
  assert.equal(summary.charged_microcredits, null);
});
