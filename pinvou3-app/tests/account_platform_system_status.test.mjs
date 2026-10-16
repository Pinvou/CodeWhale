import test from 'node:test';
import assert from 'node:assert/strict';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { formatSystemCount, parseSystemStatusJSON, requireSystemStatus, systemDiagnosticMessage } from '../src/features/account-platform/admin-system-status.js';
import { adminPlatformCopy } from '../src/shared/i18n.js';

test('actual client uses same-origin readonly status route, preserves raw int64 and float age', async () => {
  const original = globalThis.fetch;
  const calls = [];
  globalThis.fetch = async (url, options) => {
    calls.push({ url, options });
    return new Response('{"services":[{"key":"portal","status":"HEALTHY","latency_ms":9223372036854775807}],"relays":[{"key":"token-event-relay","status":"DEGRADED","pending_events":9007199254740993,"dead_lettered_events":0,"oldest_pending_seconds":1.25}],"reconciliation":{"pending":9223372036854775807,"stopped":0},"recent_errors":[]}');
  };
  try {
    const payload = requireSystemStatus(await new AdminPlatformClient().systemStatus());
    assert.equal(calls[0].url, '/api/admin/system/status');
    assert.equal(calls[0].options.method, undefined);
    assert.equal(calls[0].options.cache, 'no-store');
    assert.equal(calls[0].options.credentials, 'same-origin');
    assert.equal(payload.services[0].latency_ms, '9223372036854775807');
    assert.equal(payload.relays[0].pending_events, '9007199254740993');
    assert.equal(payload.relays[0].oldest_pending_seconds, 1.25);
    assert.equal(payload.reconciliation.pending, '9223372036854775807');
  } finally { globalThis.fetch = original; }
});
for (const status of [401, 403, 503]) test(`actual client rejects HTTP ${status}`, async () => {
  const original = globalThis.fetch;
  globalThis.fetch = async () => new Response('{"code":"PERMISSION_DENIED"}', { status });
  try { await assert.rejects(new AdminPlatformClient().systemStatus(), error => error.status === status); }
  finally { globalThis.fetch = original; }
});
test('missing counts stay unavailable rather than displayed as healthy zero', () => {
  for (const value of [null, undefined, true, {}, '', '1.5', -1, 9007199254740992, '9223372036854775808']) assert.equal(formatSystemCount(value, 'en'), '—');
  assert.equal(formatSystemCount('0', 'en'), '0');
  assert.equal(formatSystemCount('9007199254740993', 'en'), '9,007,199,254,740,993');
});
test('decoder keeps numeric-looking messages and escaped keys intact', () => {
  const result = parseSystemStatusJSON('{"message":"pending: 9007199254740993", "resource":"\\\"pending\\\":9007199254740993","pending":12}');
  assert.equal(result.message, 'pending: 9007199254740993');
  assert.equal(result.resource, '"pending":9007199254740993');
  assert.equal(result.pending, '12');
});
test('malformed or missing response shape is rejected', () => {
  for (const value of [null, {}, { services: [], relays: [], reconciliation: {}, recent_errors: [null] }, { services: [{}], relays: [], reconciliation: {}, recent_errors: [] }]) assert.throws(() => requireSystemStatus(value));
  assert.throws(() => parseSystemStatusJSON('{'));
});
test('malformed optional display values are rejected before React rendering', () => {
  const valid = { services: [], relays: [], reconciliation: {}, recent_errors: [] };
  for (const patch of [
    { services: [{ key: 'portal', status: 'HEALTHY', message: {} }] },
    { relays: [{ key: 'token-event-relay', status: 'HEALTHY', message: [] }] },
    { reconciliation: [] },
    { recent_errors: [{ source: 'token', message: 'controlled', resource: {} }] },
    { recent_errors: [{ source: 'token', message: 'controlled', updated_at: 1 }] },
  ]) assert.throws(() => requireSystemStatus({ ...valid, ...patch }), /INVALID_SYSTEM_STATUS/);
});
for (const language of ['zh', 'en', 'ja']) test(`existing system/runbook labels available in ${language}`, () => {
  const copy = adminPlatformCopy[language];
  for (const key of ['services', 'relays', 'recentErrors', 'service', 'latency', 'message', 'pending', 'deadLetters', 'oldest', 'source', 'resource', 'updated', 'healthyServices', 'pendingReconciliation', 'stoppedReconciliation']) assert.ok(copy.system[key], `missing system label: ${language}.${key}`);
  for (const key of ['title', 'lead', 'bootstrap', 'bootstrapHelp', 'idp', 'idpHelp', 'noExecution']) assert.ok(copy.runbooks[key]);
  assert.ok(copy.nav.system);
});

test('unavailable reconciliation counts and diagnostic codes retain their read-only three-language presentation', () => {
  const payload = requireSystemStatus(parseSystemStatusJSON('{"services":[],"relays":[],"reconciliation":{"pending":null,"stopped":null},"recent_errors":[{"source":"token-reconciliation","code":"RECONCILIATION_STATUS_UNAVAILABLE","message":"reconciliation status unavailable"}]}'));
  for (const language of ['zh', 'en', 'ja']) {
    const labels = adminPlatformCopy[language].system;
    assert.equal(formatSystemCount(payload.reconciliation.pending, language), '—');
    assert.equal(formatSystemCount(payload.reconciliation.stopped, language), '—');
    assert.equal(systemDiagnosticMessage(payload.recent_errors[0], labels), labels.reconciliationUnavailable);
    assert.ok(labels.reconciliationUnavailable); assert.ok(labels.reconciliationErrorsUnavailable);
    assert.equal(systemDiagnosticMessage({code: 'RECONCILIATION_ERRORS_UNAVAILABLE'}, labels), labels.reconciliationErrorsUnavailable);
    assert.equal(systemDiagnosticMessage({message: 'existing controlled diagnostic'}, labels), 'existing controlled diagnostic');
  }
  assert.throws(() => requireSystemStatus({...payload, recent_errors: [{source:'token-reconciliation', message:'controlled', code:{}}]}), /INVALID_SYSTEM_STATUS/);
});
