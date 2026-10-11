import test from 'node:test';
import assert from 'node:assert/strict';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { reconciliationCommand, reconciliationDayRange, reconciliationImportItems, reconciliationCaseLabel, requireReconciliationDate } from '../src/features/account-platform/reconciliation-contract.js';
import { reconciliationCopy } from '../src/shared/i18n.js';

const ID = '11111111-1111-4111-8111-111111111111';
test('date evidence gaps have explicit labels in all three languages and preserve unknown types', () => {
  for (const language of ['zh', 'en', 'ja']) {
    for (const type of ['REFUND_DATE_UNVERIFIED', 'CHARGEBACK_DATE_UNVERIFIED']) {
      assert.equal(typeof reconciliationCopy[language].caseTypes[type], 'string');
      assert.notEqual(reconciliationCaseLabel(type, reconciliationCopy[language]), type);
    }
    assert.equal(reconciliationCaseLabel('FUTURE_PROVIDER_EVIDENCE', reconciliationCopy[language]), 'FUTURE_PROVIDER_EVIDENCE');
  }
});
test('published import row limit and UTF8 field budget agree with the server', () => {
  assert.equal(reconciliationImportItems(JSON.stringify(Array.from({ length: 100000 }, () => ({ amount_fen: 1 })))).length, 100000);
  assert.throws(() => reconciliationImportItems(JSON.stringify(Array.from({ length: 100001 }, () => ({ amount_fen: 1 })))), { message: 'INVALID_RECONCILIATION_INPUT' });
  assert.throws(() => reconciliationImportItems(JSON.stringify([{ merchant_order_no: '界'.repeat(86) }])), { message: 'INVALID_RECONCILIATION_INPUT' });
});
test('import preserves int64 source digits and rejects invalid dates', () => {
  assert.equal(reconciliationImportItems('[{"amount_fen":9007199254740993}]')[0].amount_fen, '9007199254740993');
  for (const value of ['2026-02-29', '10/07/2026', '2026-13-01', '2026-99-99']) assert.throws(() => requireReconciliationDate(value), { message: 'INVALID_RECONCILIATION_INPUT' });
  assert.equal(requireReconciliationDate('2024-02-29'), '2024-02-29');
});
test('Shanghai selected days use the next calendar date as exclusive end', () => {
  for (const [from, to] of [['2026-10-07','2026-10-08'],['2026-12-31','2027-01-01'],['2024-02-28','2024-02-29'],['2024-02-29','2024-03-01'],['2026-02-28','2026-03-01']]) {
    assert.deepEqual(reconciliationDayRange(from), { business_date_from: from, business_date_to: to });
  }
  for (const invalid of ['2026-02-29','2026-13-01','10/07/2026','9999-12-31']) assert.throws(() => reconciliationDayRange(invalid), { message: 'INVALID_RECONCILIATION_INPUT' });
});
test('case commands require actual owner/review fields without future financial repair actions', () => {
  const fields = { reason: 'controlled review reason', owner: ID, repair: 'controlled plan', review: 'controlled finding' };
  assert.deepEqual(reconciliationCommand('ASSIGN', fields), { action: 'ASSIGN', reason: fields.reason, owner_id: ID });
  assert.deepEqual(reconciliationCommand('RESOLVE', fields), { action: 'RESOLVE', reason: fields.reason, review_result: fields.review });
  assert.throws(() => reconciliationCommand('CONFIRM_FORCED_REFUND', fields));
  assert.throws(() => reconciliationCommand('ASSIGN', { ...fields, owner: '' }));
});
test('actual client rejects unknown success or wrong run identity facts', async t => {
  const saved = globalThis.fetch; t.after(() => { globalThis.fetch = saved; });
  const client = new AdminPlatformClient(); client.csrfToken = 'public-fixture-csrf';
  const facts = { channel: 'ALIPAY', business_date: '2026-10-07' };
  const run = { reconciliation_run_id: ID, ...facts, status: 'COMPLETED' };
  const calls = [];
  globalThis.fetch = async (url, options) => { calls.push({url, options}); return new Response(JSON.stringify(run), { status: 200 }); };
  assert.deepEqual(await client.runReconciliation(facts), run);
  assert.equal(calls[0].url, '/api/admin/billing/admin/v1/reconciliation-runs');
  assert.deepEqual(JSON.parse(calls[0].options.body), facts);
  globalThis.fetch = async () => new Response(JSON.stringify(run), { status: 202 });
  await assert.rejects(client.runReconciliation(facts));
  globalThis.fetch = async () => new Response(JSON.stringify({ ...run, business_date: '2026-10-08' }), { status: 200 });
  await assert.rejects(client.runReconciliation(facts));
});
test('case writes preserve the same key and exact money response, reject false ACK', async t => {
  const saved = globalThis.fetch; t.after(() => { globalThis.fetch = saved; });
  const client = new AdminPlatformClient(); client.csrfToken = 'public-fixture-csrf';
  const calls = []; const facts = { action: 'CLAIM', reason: 'controlled claim reason' };
  globalThis.fetch = async (url, options) => { calls.push({url, options}); return new Response(`{"case_id":"${ID}","provider_snapshot":{"amount_fen":9007199254740993}}`, { status: 200 }); };
  for (let i=0;i<2;i++) assert.equal((await client.reconciliationCaseCommand(ID, facts, 'controlled-stable-key')).provider_snapshot.amount_fen, '9007199254740993');
  assert.equal(calls[0].options.headers['Idempotency-Key'], calls[1].options.headers['Idempotency-Key']);
  assert.equal(calls[0].options.body, calls[1].options.body);
  globalThis.fetch = async () => new Response(JSON.stringify({ case_id: '22222222-2222-4222-8222-222222222222' }), { status: 200 });
  await assert.rejects(client.reconciliationCaseCommand(ID, facts, 'controlled-stable-key'));
});

import * as reconciliation from '../src/features/account-platform/reconciliation-contract.js';
import fs from 'node:fs';
const GENERATED_CASE_TYPES = ['UNSUPPORTED_PROVIDER_ENTRY', 'DUPLICATE_TRANSACTION', 'INTERNAL_PAID_PROVIDER_MISSING', 'INTERNAL_REFUND_PROVIDER_MISSING', 'INTERNAL_CHARGEBACK_PROVIDER_MISSING', 'AMOUNT_MISMATCH', 'REFUND_AMOUNT_MISMATCH', 'REFUND_DATE_UNVERIFIED', 'CHARGEBACK_DATE_UNVERIFIED', 'PAYMENT_LEDGER_INVALID', 'REFUND_LEDGER_INVALID', 'CHARGEBACK_LEDGER_INVALID', 'PAYMENT_PACKAGE_INVALID', 'PAYMENT_LEDGER_MISSING', 'REFUND_LEDGER_MISSING', 'CHARGEBACK_LEDGER_MISSING', 'PROVIDER_PAID_INTERNAL_MISSING', 'PROVIDER_REFUND_INTERNAL_MISSING', 'PROVIDER_CHARGEBACK_INTERNAL_MISSING'];
test('all nineteen current producer case types have explicit three-language labels; future remains raw', () => {
  for (const language of ['zh', 'en', 'ja']) {
    assert.deepEqual(Object.keys(reconciliationCopy[language].caseTypes).sort(), [...GENERATED_CASE_TYPES].sort());
    for (const type of GENERATED_CASE_TYPES) {
      assert.ok(reconciliationCopy[language].caseTypes[type].trim());
      assert.notEqual(reconciliationCaseLabel(type, reconciliationCopy[language]), type);
    }
    assert.equal(reconciliationCaseLabel('FUTURE_PROVIDER_EVIDENCE', reconciliationCopy[language]), 'FUTURE_PROVIDER_EVIDENCE');
  }
});
test('only declared validation codes are input errors; state conflicts and unknown writes retain attribution', () => {
  assert.equal(typeof reconciliation.reconciliationReadError, 'function');
  for (const code of ['INVALID_REQUEST', 'INVALID_FILTER']) {
    assert.equal(reconciliation.reconciliationReadError({ status: 400, code }), 'invalid');
    assert.equal(reconciliation.reconciliationWriteError({ status: 400, code }), 'invalid');
  }
  assert.equal(reconciliation.reconciliationReadError({ status: 403 }), 'permissions');
  assert.equal(reconciliation.reconciliationReadError({ status: 503 }), 'unavailable');
  assert.equal(reconciliation.reconciliationReadError({ status: 422, code: 'PAGINATION_FILTER_REQUIRED' }), 'narrowFilters');
  assert.equal(reconciliation.reconciliationReadError({ status: 422, code: 'UNRECOGNIZED_ERROR' }), 'unavailable');
  for (const [status, code] of [[400, 'PAGINATION_CURSOR_INVALID'], [400, 'UNRECOGNIZED_ERROR'], [409, 'BILLING_CONFLICT'], [409, 'IDEMPOTENCY_CONFLICT']]) {
    assert.equal(reconciliation.reconciliationReadError({ status, code }), 'sourceChanged');
    assert.equal(reconciliation.reconciliationWriteError({ status, code }), 'sourceChanged');
  }
  for (const error of [{}, {status:503}, {status:400, code:'COMMAND_RESULT_UNKNOWN'}]) assert.equal(reconciliation.reconciliationWriteError(error), 'runUnknown');
  assert.equal(reconciliation.reconciliationWriteError({ message:'INVALID_RECONCILIATION_INPUT' }), 'invalid');
});
test('OpenAPI describes actual list filters, snapshot cursors and distinct input/state/size responses', () => {
  const contract=fs.readFileSync(new URL('../../account-platform/contracts/openapi/reconciliation.yaml', import.meta.url),'utf8').replace(/\r\n/g,'\n');
  const operation = route => {
    const marker=`  ${route}:\n`;
    assert.ok(contract.includes(marker), `Missing route ${route}`);
    return contract.split(marker)[1].split('\n  /')[0];
  };
  const parameter = name => {
    const block=contract.split(`    ${name}:`)[1]?.split(/\n {4}(?=\S)/)[0];
    assert.ok(block, `Missing parameter ${name}`);
    return block;
  };
  const producer=fs.readFileSync(new URL('../../account-platform/internal/billing/admin_pagination.go',import.meta.url),'utf8').replace(/\r\n/g,'\n');
  const definition = name => producer.split(`\t${name}: {`)[1].split('\n\t},')[0];
  const names = name => [...definition(name).matchAll(/^\t\t"([a-z_]+)":/gm)].map(match=>match[1]);
  const queryNames = route => [...operation(route).split('    post:')[0].matchAll(/#\/components\/parameters\/([A-Za-z]+)/g)].map(match=>parameter(match[1]).match(/name: ([a-z_]+)/)[1]);
  for (const [route, resource, scope] of [['/admin/v1/reconciliation-runs','adminPageReconciliationRuns',[]], ['/admin/v1/reconciliation-cases','adminPageReconciliationCases',['run_id']], ['/admin/v1/reconciliation-runs/{run_id}/cases','adminPageReconciliationCases',[]]]) {
    const get=operation(route).split('    post:')[0];
    // Compare the entire actual backend whitelist, separately accounting for path/query run scope.
    assert.deepEqual(queryNames(route).sort(), ['limit','cursor',...scope,...names(resource)].sort());
    assert.match(parameter('Limit'),/minimum: 1, maximum: 200, default: 50/);
    for(const status of ['400','410','422','429','503']) assert.ok(get.includes(`'${status}':`));
  }
  for(const name of ['CreatedFrom','CreatedTo']) assert.match(parameter(name),/format: date-time/);
  assert.match(parameter('CreatedFrom'),/Inclusive RFC3339.*366 days/);
  assert.match(parameter('CreatedTo'),/Exclusive RFC3339/);
  assert.match(producer,/validAdminTimeRange\(request.Filters, "created_from", "created_to"\)/);
  assert.match(producer,/created_at>=NULLIF\(\$7,''\)::timestamptz/);
  assert.match(producer,/created_at<NULLIF\(\$8,''\)::timestamptz/);
  const imports=operation('/admin/v1/reconciliation-imports');
  assert.match(imports,/'422':[^\n]*PAYMENT_CAPABILITY_UNSUPPORTED[^\n]*100000/);
  assert.match(imports,/'413':[^\n]*64 MiB/);
  assert.ok(imports.includes("'400':"));assert.ok(imports.includes("'409':"));
  assert.ok(operation('/admin/v1/reconciliation-cases/{case_id}/commands').includes("'409':"));
  // The independent server filter definitions are the authority, including all nineteen case enums.
  for(const type of GENERATED_CASE_TYPES) assert.ok(definition('adminPageReconciliationCases').includes(`"${type}"`));
  const declaredCases=parameter('CaseType').match(/enum: \[([^\]]+)\]/)[1].split(',').map(value=>value.trim());
  assert.deepEqual(declaredCases.sort(),[...GENERATED_CASE_TYPES].sort());
});

test('case receipt conflicts use the real handler BILLING_CONFLICT contract', () => {
  const contract=fs.readFileSync(new URL('../../account-platform/contracts/openapi/reconciliation.yaml',import.meta.url),'utf8').replace(/\r\n/g,'\n');
  const handler=fs.readFileSync(new URL('../../account-platform/internal/billing/admin_reconciliation.go',import.meta.url),'utf8').replace(/\r\n/g,'\n');
  const errors=fs.readFileSync(new URL('../../account-platform/internal/billing/service.go',import.meta.url),'utf8').replace(/\r\n/g,'\n');
  assert.match(handler,/existingHash != requestHash \|\| existingActor != command.ActorID[\s\S]*?return AdminReconciliationCase\{\}, ErrConflict/);
  assert.match(handler.split('func (s *Service) writeReconciliationError')[1],/s.writeError\(w, r, err\)/);
  assert.match(errors,/errors.Is\(err, ErrConflict\)[\s\S]*?"BILLING_CONFLICT"/);
  const conflict=contract.split('    StateConflict:')[1].split(/\n {4}(?=\S)/)[0];
  assert.match(conflict,/BILLING_CONFLICT[\s\S]*key.*actor.*request facts/);
  assert.equal(conflict.includes('IDEMPOTENCY_CONFLICT'),false);


});
