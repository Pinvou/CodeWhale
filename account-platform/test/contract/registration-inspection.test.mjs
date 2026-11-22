import assert from 'node:assert/strict';
import test from 'node:test';
import { createServer } from 'node:http';
import { resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const workspace = fileURLToPath(new URL('../../../', import.meta.url));
const source = path => pathToFileURL(resolve(workspace, 'pinvou3-app/src', path));
const { registrationRepairActions, registrationInspectionFilters, registrationOperationAge } = await import(source('features/account-platform/registration-repair.js'));
const { AdminPlatformClient } = await import(source('features/account-platform/admin-client.js'));
const { registrationRepairCopy } = await import(source('shared/i18n.js'));
const id = '00000000-0000-4000-8000-000000000701';

test('incomplete unknown identity and future registration states never expose an action even with a misleading compensation hint', () => {
  for (const status of ['IDENTITY_CREATING', 'SMS_PENDING', 'IDENTITY_CREATED', 'ACTIVE', 'COMPENSATED', 'FUTURE_STATE', '']) {
    assert.deepEqual(registrationRepairActions({ status, compensation_allowed: true }), { query: false, retry: false, compensate: false });
  }
  assert.deepEqual(registrationRepairActions(null), { query: false, retry: false, compensate: false });
});

test('a durable unknown identity offers only the existing manual lookup action and never compensation', () => {
  const unknown = { status: 'IDENTITY_CREATING', stage: 'IDENTITY_CREATING', identity_create_attempted_at: '2026-10-24T00:00:01Z', account_created: false, compensation_allowed: true };
  assert.deepEqual(registrationRepairActions(unknown), { query: true, retry: true, compensate: false });
  for (const changes of [
    { status: 'FUTURE_STATE' }, { stage: 'IDENTITY_CREATED' }, { stage: undefined },
    { identity_create_attempted_at: undefined }, { identity_create_attempted_at: '' },
    { account_created: true }, { account_created: undefined }, { account_created: 'false' },
  ]) {
    assert.deepEqual(registrationRepairActions({ ...unknown, ...changes }), { query: false, retry: false, compensate: false });
  }
});

test('existing repair retries and proof-qualified compensation keep their distinct gates', () => {
  assert.deepEqual(registrationRepairActions({ status: 'REPAIR_REQUIRED', compensation_allowed: false }), { query: false, retry: true, compensate: false });
  assert.deepEqual(registrationRepairActions({ status: 'REPAIR_REQUIRED', compensation_allowed: true }), { query: false, retry: true, compensate: true });
  assert.deepEqual(registrationRepairActions({ status: 'COMPENSATION_PENDING', compensation_allowed: true }), { query: false, retry: false, compensate: true });
  assert.equal(registrationRepairActions({ status: 'COMPENSATION_PENDING', compensation_allowed: 'true' }).compensate, false);
});

test('exact operation filter and existing updated-age filter reject invalid UI inputs without creating a new age policy', () => {
  assert.deepEqual(registrationInspectionFilters({ operation_id: '  ' + id.toUpperCase() + ' ', status: 'IDENTITY_CREATING', stage: 'IDENTITY_CREATING', responsibility: 'IDENTITY', sla: '', older_than_minutes: '60' }), { operation_id: id, status: 'IDENTITY_CREATING', stage: 'IDENTITY_CREATING', responsibility: 'IDENTITY', older_than_minutes: '60' });
  for (const value of ['wrong', '00000000-0000-0000-0000-000000000000']) assert.equal(registrationInspectionFilters({ operation_id: value }), null);
  for (const value of ['1.5', '-1', '43201', 'Infinity']) assert.equal(registrationInspectionFilters({ older_than_minutes: value }), null);
  assert.deepEqual(registrationInspectionFilters({ older_than_minutes: '0' }), {});
  assert.equal(registrationOperationAge('2026-10-24T00:00:00Z', Date.parse('2026-10-24T02:00:00Z')), 120);
  assert.equal(registrationOperationAge('not-a-date'), null);
  assert.equal(registrationOperationAge('2099-01-01T00:00:00Z', 0), null);
});

test('actual client preserves all registration query filters over HTTP and reads the public unknown projection', async () => {
  const calls = [];
  const server = createServer((req, res) => {
    const url = new URL(req.url, 'http://public.example'); calls.push({ method: req.method, path: url.pathname, query: Object.fromEntries(url.searchParams) });
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ items: [{ registration_operation_id: id, status: 'IDENTITY_CREATING', stage: 'IDENTITY_CREATING', responsibility: 'IDENTITY', account_created: false, compensation_allowed: false, identity_create_attempted_at: '2026-10-24T00:00:01Z', created_at: '2026-10-24T00:00:00Z', updated_at: '2026-10-24T02:00:00Z', next_retry_at: '2026-10-24T02:00:05Z', attempts: 2, reason: 'IDENTITY_UNAVAILABLE' }] }));
  });
  await new Promise(done => server.listen(0, '127.0.0.1', done));
  try {
    const client = new AdminPlatformClient(`http://127.0.0.1:${server.address().port}`);
    const filters = { operation_id: id, status: 'IDENTITY_CREATING', stage: 'IDENTITY_CREATING', responsibility: 'IDENTITY', sla: 'WITHIN', older_than_minutes: '60' };
    const page = await client.registrationRepairs(filters);
    assert.deepEqual(calls, [{ method: 'GET', path: '/api/admin/account/admin/v1/registration-operations', query: { limit: '100', ...filters } }]);
    assert.equal(page.items[0].registration_operation_id, id);
    assert.equal(Object.hasOwn(page.items[0], 'sla_expires_at'), false, 'do not invent an SLA for unknown identity creation');
    assert.deepEqual(registrationRepairActions(page.items[0]), { query: true, retry: true, compensate: false });
  } finally { server.closeAllConnections(); await new Promise(done => server.close(done)); }
});

test('inspection fields and the unknown-result explanation are complete in all three existing languages', () => {
  const keys = ['operationID', 'status', 'stage', 'responsibility', 'olderThan', 'sla', 'createdAt', 'updatedAt', 'attemptAt', 'nextRetryAt', 'age', 'identityUnknown', 'readOnly', 'invalidFilters'];
  for (const language of ['zh', 'en', 'ja']) {
    const words = registrationRepairCopy[language];
    for (const key of keys) assert.equal(typeof words[key] === 'string' && words[key].length > 0, true, `${language}.${key}`);
    assert.equal(typeof words.minutes(120), 'string');
    assert.equal(typeof words.stages.IDENTITY_CREATING, 'string');
    assert.equal(typeof words.responsibilities.IDENTITY, 'string');
  }
});
