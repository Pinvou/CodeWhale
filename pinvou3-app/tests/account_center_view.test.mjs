import assert from 'node:assert/strict';
import { test } from 'node:test';
import { pointsToMicrocredits, productPointFacts, quotaSources, formatRMB, packageAllocatedMicrocredits } from '../src/features/account-platform/account-center-view.js';
import { accountCenterCopy } from '../src/shared/account-center-i18n.js';

test('point input preserves four decimals and the int64 boundary without Number conversion', () => {
  assert.equal(pointsToMicrocredits('1'), '10000');
  assert.equal(pointsToMicrocredits('0.0001'), '1');
  assert.equal(pointsToMicrocredits('12.3456'), '123456');
  assert.equal(pointsToMicrocredits('922337203685477.5807'), '9223372036854775807');
  for (const value of ['922337203685477.5808', '0', '-1', '1.23456', '1e3', '01', '', 'NaN', '1,000', 1]) assert.throws(() => pointsToMicrocredits(value));
});

test('only known active, unexpired source components with positive precise credit are offered', () => {
  const now = Date.parse('2026-11-05T00:00:00Z');
  const component = { component_id: 'source', component_type: 'BASE', status: 'ACTIVE', available_microcredits: '9007199254740993' };
  const pack = { status: 'ACTIVE', package_id: 'package', components: [component] };
  assert.equal(quotaSources([pack], now)[0].component, component);
  for (const overrides of [{ status: 'FROZEN' }, { valid_from: '2026-11-06T00:00:00Z' }, { expires_at: '2026-11-04T00:00:00Z' }, { expires_at: 'bad-date' }, { components: {} }, { components: null }]) assert.deepEqual(quotaSources([{ ...pack, ...overrides }], now), []);
  for (const overrides of [{ status: 'FROZEN' }, { component_type: 'PLAN' }, { available_microcredits: 9007199254740992 }, { available_microcredits: '0' }, { component_id: '' }, { expires_at: '2026-11-05T00:00:00Z' }]) assert.deepEqual(quotaSources([{ ...pack, components: [{ ...component, ...overrides }] }], now), []);
});

test('product facts never infer points or discounts from a low price', () => {
  assert.equal(productPointFacts({ amount_fen: '10' }), null);
  assert.deepEqual(productPointFacts({ base_points: '9007199254740993', bonus_points: '2' }), { base: 9007199254740993n, bonus: 2n, total: 9007199254740995n });
  assert.deepEqual(productPointFacts({ display_points: '100' }), { total: 100n });
  assert.equal(productPointFacts({ base_points: '-1' }), null);
  assert.equal(formatRMB('10', 'en'), '¥0.10');
});

test('allocated credits include active and suspended grants without inventing absent totals', () => {
  assert.equal(packageAllocatedMicrocredits({}), null);
  assert.equal(packageAllocatedMicrocredits({ components: [{ active_grant_microcredits: '1' }] }), null);
  assert.equal(packageAllocatedMicrocredits({ components: [{ active_grant_microcredits: '9007199254740993', suspended_grant_microcredits: '2' }] }), '9007199254740995');
});

test('account center translations have matching keys and no missing leaf values', () => {
  const leaves = value => Object.entries(value).flatMap(([key, item]) => item && typeof item === 'object' ? leaves(item).map(child => `${key}.${child}`) : [key]).sort();
  for (const language of ['en', 'ja']) assert.deepEqual(leaves(accountCenterCopy[language]), leaves(accountCenterCopy.zh));
  for (const copy of Object.values(accountCenterCopy)) assert.ok(copy.usageSummary(2, '1.0001').includes('1.0001'));
});
