import { integerMoney } from './finance-contract.js';

const microcreditsPerPoint = 10_000n;

// Plain decimal text only: no grouping, spaces, signs or exponent notation.
// Leading zeroes are harmless; the wire result is always a canonical integer.
export function ledgerPointsToMicrocredits(value) {
  if (typeof value !== 'string' || value.length > 64 || !/^\d+(?:\.\d{1,4})?$/.test(value)) {
    throw new Error('INVALID_LEDGER_POINTS_AMOUNT');
  }
  const [whole, fraction = ''] = value.split('.');
  const amount = integerMoney((BigInt(whole) * microcreditsPerPoint + BigInt(fraction.padEnd(4, '0'))).toString());
  if (amount <= 0n) throw new Error('INVALID_LEDGER_POINTS_AMOUNT');
  return amount.toString();
}

export function ledgerMicrocreditsToPoints(value) {
  const amount = integerMoney(value);
  if (amount <= 0n) throw new Error('INVALID_LEDGER_POINTS_AMOUNT');
  const fraction = (amount % microcreditsPerPoint).toString().padStart(4, '0').replace(/0+$/, '');
  const whole = (amount / microcreditsPerPoint).toString();
  return fraction ? `${whole}.${fraction}` : whole;
}
