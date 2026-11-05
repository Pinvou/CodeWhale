import assert from 'node:assert/strict';
import { test } from 'node:test';
import { usagePeriodQuery, readUsagePeriodBatch } from '../src/features/account-platform/usage-period.js';

const filters = { from: '2026-11-01', to: '2026-11-07', model: '', status: '' };
const record = (id, extra = {}) => ({ usage_request_id: id, started_at: '2026-11-04T12:00:00Z', model: 'A', display_status: 'SETTLED', settlement_status: 'SETTLED', execution_status: 'SUCCEEDED', charged_microcredits: '10000', ...extra });

test('period includes the 21st record and only completes after cursor exhaustion', async () => {
  const pages = [], query = usagePeriodQuery(filters);
  const value = await readUsagePeriodBatch({ query, readPage: async cursor => { pages.push(cursor); return cursor ? { items: [record('last')], next_cursor: null } : { items: Array.from({ length: 20 }, (_, i) => record(String(i))), next_cursor: 'older' }; } });
  assert.deepEqual(pages, ['', 'older']); assert.equal(value.charged, '210000'); assert.equal(value.cursor, null); assert.equal(value.scanned, 21);
});

test('bounded batches continue from their checkpoint without page-boundary-dependent legacy filtering', async () => {
  const readPage = async cursor => cursor ? { items: [record('b', { logical_turn_id: 'one-turn', model: 'B', charged_microcredits: '9007199254740993' })], next_cursor: null } : { items: [record('a', { logical_turn_id: 'one-turn', model: 'A' })], next_cursor: 'older' };
  const query = usagePeriodQuery({ ...filters, model: 'B' });
  const first = await readUsagePeriodBatch({ readPage, query, maxPages: 1 });
  assert.equal(first.cursor, 'older'); assert.equal(first.charged, '0');
  const final = await readUsagePeriodBatch({ readPage, query, checkpoint: first });
  assert.equal(final.cursor, null); assert.equal(final.charged, '9007199254740993'); assert.equal(first.cursor, 'older');
});

test('unknown charges are excluded explicitly, zero-charge failures count as known, and dates are inclusive local days', async () => {
  const query = usagePeriodQuery(filters);
  const value = await readUsagePeriodBatch({ query, readPage: async () => ({ items: [record('unknown', { charged_microcredits: null }), record('released', { settlement_status: 'RELEASED', display_status: 'RELEASED', execution_status: 'FAILED', charged_microcredits: '0' }), record('outside', { started_at: new Date(query.until).toISOString() }), record('start', { started_at: new Date(query.from).toISOString() })], next_cursor: null }) });
  assert.equal(value.unknown, 1); assert.equal(value.charged, '10000');
  for (const value of [{ ...filters, from: '' }, { ...filters, to: '2026-11-00' }, { ...filters, from: '2026-11-08' }]) assert.throws(() => usagePeriodQuery(value));
});

test('a failed batch does not mutate its checkpoint; retry cannot double count', async () => {
  const checkpoint = { cursor: 'older', charged: '10000', scanned: 1, unknown: 0 }, query = usagePeriodQuery(filters);
  await assert.rejects(readUsagePeriodBatch({ query, checkpoint, readPage: async cursor => { if (cursor === 'older') return { items: [record('two')], next_cursor: 'last' }; throw new Error('READ_FAILED'); } }));
  assert.deepEqual(checkpoint, { cursor: 'older', charged: '10000', scanned: 1, unknown: 0 });
  const final = await readUsagePeriodBatch({ query, checkpoint, readPage: async () => ({ items: [record('two')], next_cursor: null }) });
  assert.equal(final.charged, '20000');
});

test('cancellation, stale owner and cyclic/malformed pages cannot deliver a total or dispatch a successor', async () => {
  const query = usagePeriodQuery(filters); let current = true, calls = 0;
  await assert.rejects(readUsagePeriodBatch({ query, isCurrent: () => current, readPage: async () => { calls++; current = false; return { items: [record('one')], next_cursor: 'older' }; } }), { name: 'AbortError' });
  assert.equal(calls, 1);
  const controller = new AbortController(); controller.abort();
  await assert.rejects(readUsagePeriodBatch({ query, signal: controller.signal, readPage: async () => { throw new Error('DISPATCHED'); } }), { name: 'AbortError' });
  for (const page of [{ items: [] }, { items: [], next_cursor: '' }, { items: [record('bad', { started_at: 'invalid' })], next_cursor: null }, { items: [], next_cursor: 'same' }]) await assert.rejects(readUsagePeriodBatch({ query, checkpoint: { cursor: 'same' }, readPage: async () => page }));
});
