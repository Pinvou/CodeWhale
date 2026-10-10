import test from 'node:test';
import assert from 'node:assert/strict';
import { financialRepairCommands, financialRepairApprovalDraft, financialRepairPrecheck } from '../src/features/account-platform/admin-financial-repair-drafts.js';
import { financialRepairInputs, financialRepairPrecheckBody } from '../src/features/account-platform/admin-financial-repair-form.js';
const command = "CHARGEBACK_FINANCIAL_REPAIR";
const target = "11111111-1111-4111-8111-111111111111";
const normalized = {"chargeback_case_id": "11111111-1111-4111-8111-111111111111", "payment_evidence_id": "22222222-2222-4222-8222-222222222222", "expected_case_version": "9007199254740993", "reason_code": "BASE_SHORTFALL_COMPENSATION"};
const projection = { command_type: command, policy_version: 1, snapshot_schema_version: 1,
 target_resource_type: financialRepairCommands[command].targetResourceType, target_resource_id: target,
 normalized_command: normalized, critical_resource_versions: { chargeback_case_version: '9007199254740993', facts_digest: 'a'.repeat(64), policy_version: 1 },
 financial_snapshot: { base_shortfall_microcredits: '9007199254740993', wallet_id: '33333333-3333-4333-8333-333333333333', wallet_version: '9223372036854775807', package_id: '44444444-4444-4444-8444-444444444444', component_digest: 'b'.repeat(64), provider_chargeback_id: 'controlled-provider-chargeback' } };

test('F14 financial descriptor set exposes only delivered operations', () => {
 assert.deepEqual(Object.keys(financialRepairCommands).sort(), ["LEDGER_REPAIR", "RECEIVABLE_WRITE_OFF", "PAYMENT_FINANCIAL_REPAIR", "REFUND_EXCEPTION_REPAIR", "CHARGEBACK_FINANCIAL_REPAIR", "RECONCILIATION_FINANCIAL_REPAIR"].sort());
 assert.deepEqual(Object.keys(financialRepairInputs).sort(), ["LEDGER_REPAIR", "RECEIVABLE_WRITE_OFF", "PAYMENT_FINANCIAL_REPAIR", "REFUND_EXCEPTION_REPAIR", "CHARGEBACK_FINANCIAL_REPAIR", "RECONCILIATION_FINANCIAL_REPAIR"].sort());
 assert.equal(financialRepairPrecheck({ ...projection, command_type: 'UNKNOWN_REPAIR' }), null);
});

test('F14 owner target and complete normalized facts bind the request', () => {
 assert.ok(financialRepairPrecheck(projection));
 assert.equal(financialRepairPrecheck({ ...projection, target_resource_id: '33333333-3333-4333-8333-333333333333' }), null);
 for (const field of financialRepairCommands[command].fields) {
  const facts = { ...normalized }; delete facts[field.key];
  assert.equal(financialRepairPrecheck({ ...projection, normalized_command: facts }), null);
 }
 const input = { target_resource_id: target, payment_evidence_id: "22222222-2222-4222-8222-222222222222", business_date: '2026-10-07', usage_request_id: "22222222-2222-4222-8222-222222222222", loss_review_id: "22222222-2222-4222-8222-222222222222", untrusted_amount: '10' };
 const accepted = financialRepairPrecheckBody(command, input);
 assert.ok(accepted.body);
 assert.ok(!Object.hasOwn(accepted.body, 'untrusted_amount'));
 assert.ok(financialRepairPrecheckBody(command, { ...input, target_resource_id: 'not-an-owner-id' }).error);
});

test('F14 draft preserves int64 owner documents and closed authority', () => {
 const polluted = { ...projection, normalized_command: { ...normalized, attacker_amount: '10' } };
 const draft = financialRepairApprovalDraft(polluted, 'A legitimate operator explanation', 'f14-logical-attempt');
 assert.equal(draft.command_type, command);
 assert.equal(draft.target_resource_id, target);
 assert.strictEqual(draft.critical_resource_versions, projection.critical_resource_versions);
 assert.strictEqual(draft.financial_snapshot, projection.financial_snapshot);
 assert.equal(draft.financial_snapshot.base_shortfall_microcredits, '9007199254740993');
 assert.equal(draft.normalized_command[financialRepairCommands[command].reasonKey], financialRepairCommands[command].reasonCode);
 assert.ok(!Object.hasOwn(draft.normalized_command, 'attacker_amount'));
 assert.throws(() => financialRepairApprovalDraft(projection, 'short', 'f14-logical-attempt'));
});

import { currentApprovalEditDraft } from '../src/features/account-platform/admin-finance.js';
import { approvalCommands, readApprovalRoute, approvalBusinessRoute } from '../src/features/account-platform/admin-approval-actions.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { parseFinanceJSON, encodeFinanceJSON } from '../src/features/account-platform/finance-contract.js';
import { ledgerRepairCopy } from '../src/shared/ledger-repair-i18n.js';
const raw = () => encodeFinanceJSON(projection);
const detail = () => ({ approval_request_id: '99999999-9999-4999-8999-999999999999', command_type: command, status: 'DRAFT', allowed_actions: ['EDIT'], target: { resource_type: 'CHARGEBACK_CASE', resource_id: target }, command: { ...normalized } });


test('F14 complete owner projection fails closed on changed or missing authority', () => {
 for (const change of [p=>{p.normalized_command.reason_code='OTHER';},p=>{p.critical_resource_versions.facts_digest='';},p=>{p.critical_resource_versions.policy_version=2;},p=>{p.critical_resource_versions.chargeback_case_version='9007199254740994';},p=>{p.normalized_command.expected_case_version=9007199254740992;},p=>{p.financial_snapshot.component_digest='';},p=>{p.financial_snapshot.wallet_version=9007199254740992;},p=>{p.financial_snapshot.base_shortfall_microcredits='0';},p=>{p.financial_snapshot.package_id='bad';},p=>{p.financial_snapshot.provider_chargeback_id=' padded ';},p=>{p.snapshot_schema_version=2;}]) {
  const p=parseFinanceJSON(raw());change(p);assert.equal(financialRepairPrecheck(p),null);
 }
});

test('F14 fresh EDIT retains original two UUIDs and refuses changed frozen CAS', async () => {
 let input;const client={approvalCommandPrecheck:async b=>{input=b;return parseFinanceJSON(raw());}};const d=await currentApprovalEditDraft(client,detail(),'Controlled original chargeback reason');assert.deepEqual(Object.keys(input),['command_type','target_resource_id','payment_evidence_id','reason_code']);assert.equal(input.target_resource_id,target);assert.equal(input.payment_evidence_id,normalized.payment_evidence_id);assert.equal(d.normalized_command.expected_case_version,normalized.expected_case_version);
 for(const change of [p=>{p.normalized_command.payment_evidence_id='33333333-3333-4333-8333-333333333333';},p=>{p.target_resource_id='33333333-3333-4333-8333-333333333333';p.normalized_command.chargeback_case_id=p.target_resource_id;},p=>{p.normalized_command.expected_case_version='9007199254740994';p.critical_resource_versions.chargeback_case_version='9007199254740994';}]){const p=parseFinanceJSON(raw());change(p);await assert.rejects(currentApprovalEditDraft({approvalCommandPrecheck:async()=>p},detail(),'Controlled original chargeback reason'));}
 for(const change of [{payment_evidence_id:'bad'},{expected_case_version:'0'},{reason_code:'OTHER'}])await assert.rejects(currentApprovalEditDraft({approvalCommandPrecheck:()=>assert.fail('invalid original intent reached authority')},{...detail(),command:{...normalized,...change}},'Controlled original chargeback reason'));
});
test('F14 directory/filter/drilldown and three-language targets expose only installed Chargeback', () => {
 assert.ok(approvalCommands.includes(command));assert.equal(approvalCommands.includes('USAGE_SETTLEMENT_REPAIR'),false);assert.equal(readApprovalRoute('?approval_command_type='+command).commandType,command);
 const d={...detail(),detail_availability:'AVAILABLE'};assert.deepEqual(approvalBusinessRoute(d),{section:'finance',tab:'chargebackrepair'});for(const patch of [{target:{resource_type:'REFUND',resource_id:target}},{command:{chargeback_case_id:normalized.payment_evidence_id}},{detail_availability:'DETAIL_PRESENTATION_UNAVAILABLE'},{command_type:'RECONCILIATION_FINANCIAL_REPAIR'}])assert.equal(approvalBusinessRoute({...d,...patch}),null);
 for(const lang of ['zh','en','ja']){assert.ok(ledgerRepairCopy[lang].chargebackRepairTitle);assert.ok(ledgerRepairCopy[lang].targets[command]);for(const key of ['chargeback_case_id','payment_evidence_id','expected_case_version','chargeback_case_version','base_shortfall_microcredits','component_digest','provider_chargeback_id'])assert.ok(ledgerRepairCopy[lang].fields[key]);}
 for(const key of ['a'.repeat(111),'中'.repeat(37)])assert.equal(financialRepairApprovalDraft(projection,'Controlled Chargeback reason',key).idempotency_key,'approval:'+key);for(const key of ['a'.repeat(112),'中'.repeat(38)])assert.throws(()=>financialRepairApprovalDraft(projection,'Controlled Chargeback reason',key));
});
test('F14 mixed SDK legacy and governed readers retain current versions without changing old Wallet types',async()=>{
 const original=globalThis.fetch;const payload=`{"items":[{"command_type":"WALLET_ADJUSTMENT","financial_snapshot":{"wallet_version":7}},{"command_type":"CHARGEBACK_FINANCIAL_REPAIR","command":{"expected_case_version":9007199254740993},"critical_resource_versions":{"chargeback_case_version":9223372036854775807},"financial_snapshot":{"wallet_version":9223372036854775807}}]}`;
 globalThis.fetch=async url=>new Response(String(url).includes('/capability')?JSON.stringify({session_approval_wire_version:2,control:{version:1,epoch:1,authorization_relay_epoch:1}}):payload,{status:200});try{const c=new AdminPlatformClient('https://portal.example');for(const read of [()=>c.approvalRequests(),()=>c.governedApprovalRequests()]){const p=await read();assert.equal(p.items[0].financial_snapshot.wallet_version,7);assert.equal(p.items[1].command.expected_case_version,'9007199254740993');assert.equal(p.items[1].critical_resource_versions.chargeback_case_version,'9223372036854775807');assert.equal(p.items[1].financial_snapshot.wallet_version,'9223372036854775807');}}finally{globalThis.fetch=original;}
});
