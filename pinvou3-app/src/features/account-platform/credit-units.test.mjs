import assert from 'node:assert/strict';
import test from 'node:test';
import { formatMoney, formatPoints, subtractMicrocredits } from './credit-units.js';

test('money preserves every cent beyond the Number safe integer boundary', () => {
  for (const language of ['zh', 'en', 'ja']) {
    assert.equal(formatMoney('9007199254740993', language), '¥90,071,992,547,409.93');
    assert.equal(formatMoney('9223372036854775807', language), '¥92,233,720,368,547,758.07');
    assert.equal(formatMoney('-1', language), '¥-0.01');
    assert.equal(formatMoney(0, language), '¥0.00');
  }
});

test('points retain all four fractional digits and negative adjustments', () => {
  assert.equal(formatPoints('9007199254740993', 'en'), '900,719,925,474.0993');
  assert.equal(formatPoints('-9223372036854775808', 'en'), '-922,337,203,685,477.5808');
  assert.equal(formatPoints('-1', 'ja'), '-0.0001');
  assert.equal(formatPoints('10000', 'zh'), '1');
  assert.equal(formatPoints(7969994, 'en'), '796.9994');
});

test('available balance subtraction stays exact even when both amounts exceed Number precision', () => {
  const available = subtractMicrocredits('9007199254740993', '9007199254740992');
  assert.equal(available, '1');
  assert.equal(formatPoints(available, 'en'), '0.0001');
  assert.equal(subtractMicrocredits('0', '1'), '-1');
});

test('unknown, fractional and already rounded amounts never become a fabricated financial value', () => {
  for (const value of [null, undefined, '', 'bad', '1.5', Number.NaN, Number.POSITIVE_INFINITY, 1.5, 9007199254740992]) {
    assert.equal(formatMoney(value, 'en'), '—');
    assert.equal(formatPoints(value, 'en'), '—');
    assert.equal(subtractMicrocredits(value, '0'), null);
  }
});
