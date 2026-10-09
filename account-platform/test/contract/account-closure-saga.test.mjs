import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('account closure is a durable forward-only saga with a recoverable pre-quiet handoff gate', async () => {
  const [migration, saga, roles, service] = await Promise.all([
    read('migrations/account/000027_account_closure_saga.up.sql'),
    read('internal/account/account_closure.go'),
    read('internal/account/account_closure_roles.go'),
    read('internal/account/service.go'),
  ]);
  assert.match(migration, /account_closure_requests/);
  assert.match(migration, /account_closure_commands/);
  assert.match(migration, /account_closure_audit/);
  assert.match(migration, /enforce_account_closure_stage_progression/);
  assert.match(migration, /CLOSURE_REQUESTED.*QUIESCING.*FINANCIAL_CLEARANCE.*IDENTITY_CLEANUP.*ANONYMIZED.*COMPLETED/s);
  assert.match(saga, /accountClosureRecentAuth\s+= 5 \* time\.Minute/);
  assert.match(saga, /CLOSE_MY_ACCOUNT/);
  assert.match(saga, /EnterAccountClosureQuiescing/);
  assert.match(saga, /UPDATE users SET status='CLOSING'/);
  assert.match(roles, /LAST_ACTIVE_SUPER_ADMIN|ErrLastActiveSuperAdmin/);
  assert.match(service, /account-closure-requests/);
  assert.doesNotMatch(service, /admin\.DELETE\("\/users\/:user_id"/);
});

test('billing and token assessments cover unresolved money and usage while platform loss cannot debit the customer', async () => {
  const [billing, token] = await Promise.all([
    read('internal/billing/account_closure.go'),
    read('internal/token/account_closure.go'),
  ]);
  for (const marker of ['PERSONAL_BALANCE_NONZERO', 'RECEIVABLE_OUTSTANDING', 'UNSETTLED_HOLD', 'PAYMENT_PROCESSING', 'REFUND_UNRESOLVED', 'CHARGEBACK_UNRESOLVED', 'FINANCIAL_RECONCILIATION_OPEN', 'LATE_PAYMENT_REVIEW_OPEN', 'PAYMENT_ANOMALY_OPEN']) assert.match(billing, new RegExp(marker));
  for (const marker of ['UNSETTLED_USAGE_HOLD', 'MODEL_EXECUTION_ACTIVE', 'USAGE_RECONCILIATION_PENDING', 'late_usage_customer_debit_prohibited']) assert.match(token, new RegExp(marker));
});

test('public and operations contracts expose progress and bounded retry but no deletion bypass', async () => {
  const contract = YAML.parse(await read('contracts/openapi/account-closure.yaml'));
  const get = contract.paths['/v1/me/account-closure-request'].get;
  const create = contract.paths['/v1/me/account-closure-requests'].post;
  assert.ok(get.responses['200']);
  assert.deepEqual(create.requestBody.content['application/json'].schema.required, ['confirmation']);
  assert.equal(create.requestBody.content['application/json'].schema.properties.confirmation.const, 'CLOSE_MY_ACCOUNT');
  assert.ok(create.parameters.some(p => p.in === 'header' && p.name === 'Idempotency-Key' && p.required));
  assert.ok(contract.paths['/admin/v1/account-closure-requests'].get);
  const retry = contract.paths['/admin/v1/account-closure-requests/{closure_request_id}/actions/retry'].post;
  assert.ok(retry.parameters.some(p => p.in === 'header' && p.name === 'Idempotency-Key' && p.required));
  assert.deepEqual(retry.requestBody.content['application/json'].schema.required, ['reason']);
  assert.equal(Object.values(contract.paths).some(methods => methods.delete), false);
});

test('dedicated user and operations views expose closure progress, blockers and retry in all languages', async () => {
  const [view, closureFlow, adminView, personal, admin, copy, entry] = await Promise.all([
    read('../pinvou3-app/src/features/account-platform/AccountPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/account-closure-flow.js'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/AccountClosureWorkspace.jsx'),
    read('../pinvou3-app/src/features/account-platform/AdminAccountClosureWorkspace.jsx'),
    read('../pinvou3-app/src/shared/account-closure-i18n.js'),
    read('../pinvou3-app/src/shared/i18n.js'),
  ]);
  assert.match(view, /<AccountClosureWorkspace/);
  assert.match(closureFlow, /CLOSE_MY_ACCOUNT/);
  assert.match(adminView, /<AdminAccountClosureWorkspace/);
  assert.match(admin, /retryAccountClosure/);
  assert.match(personal, /accountClosureResumeAction/);
  assert.match(entry, /account-closure-i18n/);
  const { accountClosureCopy } = await import('../../../pinvou3-app/src/shared/account-closure-i18n.js');
  assert.deepEqual(Object.keys(accountClosureCopy).sort(), ['en', 'ja', 'zh']);
  const shape = value => typeof value === 'string' ? 'string' : Object.fromEntries(Object.keys(value).sort().map(key => [key, shape(value[key])]));
  const nonempty = value => typeof value === 'string' ? value.length > 0 : Object.values(value).every(nonempty);
  for (const language of ['zh', 'en', 'ja']) {
    assert.ok(nonempty(accountClosureCopy[language]));
    assert.deepEqual(shape(accountClosureCopy[language]), shape(accountClosureCopy.zh));
  }
  assert.match(copy, /accountClosureCopy/);
});
