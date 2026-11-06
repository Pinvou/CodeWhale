import test from 'node:test';
import assert from 'node:assert/strict';
import { ledgerPointsToMicrocredits, ledgerMicrocreditsToPoints } from '../src/features/account-platform/ledger-amount-input.js';

test('ledger points preserve independent scale, precision and int64 boundary oracles', () => {
  for (const [input, microcredits, restored] of [
    ['0.0001', '1', '0.0001'],
    ['1', '10000', '1'],
    ['1.2345', '12345', '1.2345'],
    ['0001.2500', '12500', '1.25'],
    ['900719925474.0993', '9007199254740993', '900719925474.0993'],
    ['922337203685477.5807', '9223372036854775807', '922337203685477.5807'],
  ]) {
    assert.equal(ledgerPointsToMicrocredits(input), microcredits);
    assert.equal(ledgerMicrocreditsToPoints(microcredits), restored);
  }
});

test('ledger amount input rejects ambiguous, nonpositive, overprecision and overflow values', () => {
  for (const input of ['', '0', '0.0000', '-1', '+1', '1e2', 'NaN', 'Infinity', '.1', '1.', '1.00001', '0.00001',
    '1,000', '1,25', ' 1', '1 ', '１', '922337203685477.5808', '1'.repeat(65), 1, null]) {
    assert.throws(() => ledgerPointsToMicrocredits(input), String(input));
  }
  for (const value of ['0', '-1', '9223372036854775808', 9007199254740992]) {
    assert.throws(() => ledgerMicrocreditsToPoints(value));
  }
});

test('representative positive int64 values round-trip without losing a microcredit', () => {
  const maximum = 9_223_372_036_854_775_807n;
  let amount = 1n;
  for (let index = 0; index < 100; index++) {
    const original = amount.toString();
    assert.equal(ledgerPointsToMicrocredits(ledgerMicrocreditsToPoints(original)), original);
    amount = (amount * 6_364_136_223_846_793_005n + 1n) % maximum + 1n;
  }
});
