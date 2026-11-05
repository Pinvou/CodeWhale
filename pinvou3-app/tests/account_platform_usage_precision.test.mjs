import test from 'node:test';
import assert from 'node:assert/strict';
import { parseFinanceJSON } from '../src/features/account-platform/finance-contract.js';
import { summarizeUsageRecords } from '../src/features/account-platform/usage-summary.js';
import { formatUsagePoints } from '../src/features/account-platform/credit-units.js';

test('usage summary preserves the actual int64 source digits supplied by the finance transport', () => {
  const page = parseFinanceJSON('{"items":[{"usage_request_id":"a","logical_turn_id":"turn-a","display_status":"SETTLED","settlement_status":"SETTLED","actual_microcredits":9007199254740993,"charged_microcredits":9007199254740993,"input_tokens":1,"output_tokens":0,"cached_input_tokens":0}]}');
  const [summary] = summarizeUsageRecords(page.items);
  assert.equal(BigInt(String(summary.actual_microcredits)), 9007199254740993n);
  assert.equal(BigInt(String(summary.charged_microcredits)), 9007199254740993n);
  const [grouped] = summarizeUsageRecords([...page.items, { ...page.items[0], usage_request_id: 'b', actual_microcredits: '2', charged_microcredits: '2' }]);
  assert.equal(BigInt(String(grouped.actual_microcredits)), 9007199254740995n);
  assert.equal(BigInt(String(grouped.charged_microcredits)), 9007199254740995n);
});

test('usage display rounds the exact int64 quantity to the existing two decimal places', () => {
  assert.equal(formatUsagePoints('9223372036854775807', 'en'), '922,337,203,685,477.58');
  assert.equal(formatUsagePoints(8317, 'en'), '0.83');
  assert.equal(formatUsagePoints('9950', 'en'), '1.00');
  assert.equal(formatUsagePoints(0, 'en'), '0.00');
  assert.equal(formatUsagePoints(null, 'en'), '—');
});

test('unknown or already-rounded quantities never become an exact charge', () => {
  const [unknown] = summarizeUsageRecords([{ usage_request_id: 'a', actual_microcredits: null, charged_microcredits: null }]);
  assert.equal(unknown.actual_microcredits, null);
  assert.equal(unknown.charged_microcredits, null);
  const [rounded] = summarizeUsageRecords([{ usage_request_id: 'b', actual_microcredits: 9007199254740992, charged_microcredits: 9007199254740992 }]);
  assert.equal(rounded.actual_microcredits, null);
  assert.equal(rounded.charged_microcredits, null);
});
