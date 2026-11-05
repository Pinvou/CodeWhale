import test from 'node:test'; import assert from 'node:assert/strict'; import fs from 'node:fs';
import {financialRepairPrecheckBody,financialRepairCommandTypes} from '../src/features/account-platform/admin-financial-repair-form.js';
import {financialRepairApprovalDraft,financialRepairPrecheck} from '../src/features/account-platform/admin-financial-repair-drafts.js';
import {currentApprovalEditDraft} from '../src/features/account-platform/admin-finance.js';
import {AdminPlatformClient} from '../src/features/account-platform/admin-client.js';
import {parseFinanceJSON,encodeFinanceJSON} from '../src/features/account-platform/finance-contract.js';
import {ledgerRepairCopy} from '../src/shared/ledger-repair-i18n.js';
const raw=()=>"{\"command_type\":\"RECONCILIATION_FINANCIAL_REPAIR\",\"policy_version\":1,\"snapshot_schema_version\":1,\"target_resource_type\":\"RECONCILIATION_CASE\",\"target_resource_id\":\"00000000-0000-4000-8000-000000000001\",\"normalized_command\":{\"reconciliation_case_id\":\"00000000-0000-4000-8000-000000000001\",\"payment_evidence_id\":\"00000000-0000-4000-8000-000000000002\",\"business_date\":\"2026-10-07\",\"resolution_code\":\"PROVIDER_REFUND_CONFIRMED\"},\"critical_resource_versions\":{\"run_id\":\"00000000-0000-4000-8000-000000000003\",\"case_id\":\"00000000-0000-4000-8000-000000000001\",\"provider_snapshot_digest\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\",\"facts_digest\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"policy_version\":1},\"financial_snapshot\":{\"amount_fen\":9007199254740993,\"currency\":\"CNY\",\"provider_transaction_id\":\"controlled-provider-transaction\",\"ledger_effect\":\"NONE\"}}";
const id=n=>`00000000-0000-4000-8000-${String(n).padStart(12,'0')}`;
const values={target_resource_id:id(1),payment_evidence_id:id(2),business_date:'2026-10-07'};
test('F15 closed descriptor input and remaining future deny',()=>{assert.ok(financialRepairCommandTypes.includes('RECONCILIATION_FINANCIAL_REPAIR'));assert.ok(!financialRepairCommandTypes.includes('UNKNOWN_REPAIR'));assert.deepEqual(financialRepairPrecheckBody('RECONCILIATION_FINANCIAL_REPAIR',values).body,{command_type:'RECONCILIATION_FINANCIAL_REPAIR',...values,reason_code:'PROVIDER_REFUND_CONFIRMED'});for(const v of ['',null,true,'00000000-0000-0000-0000-000000000000'])assert.ok(financialRepairPrecheckBody('RECONCILIATION_FINANCIAL_REPAIR',{...values,payment_evidence_id:v}).error)});
test('F15 actual client preserves raw owner amount, CSRF, signal and complete frozen facts',async()=>{const old=globalThis.fetch;let call;const controller=new AbortController();globalThis.fetch=async(url,options)=>{call={url,options};return new Response(raw(),{status:200})};try{const c=new AdminPlatformClient('https://portal.example');c.csrfToken='controlled-f15';const p=await c.approvalCommandPrecheck(financialRepairPrecheckBody('RECONCILIATION_FINANCIAL_REPAIR',values).body,controller.signal);assert.equal(call.options.headers['X-CSRF-Token'],'controlled-f15');assert.equal(call.options.signal,controller.signal);assert.equal(p.financial_snapshot.amount_fen,'9007199254740993');const draft=financialRepairApprovalDraft(p,'Controlled reconciliation correction reason','controlled-f15-key');assert.ok(encodeFinanceJSON(draft).includes('"amount_fen":9007199254740993'));assert.equal(draft.normalized_command.resolution_code,'PROVIDER_REFUND_CONFIRMED');}finally{globalThis.fetch=old}});
test('F15 frozen typed detail to current EDIT precheck to draft binds original evidence/date',async()=>{const owner=parseFinanceJSON(raw());const detail=process.env.F15_TYPED_DETAIL?parseFinanceJSON(fs.readFileSync(process.env.F15_TYPED_DETAIL,'utf8')):{command_type:owner.command_type,target:{resource_type:owner.target_resource_type,resource_id:owner.target_resource_id},command:{...owner.normalized_command,run_id:owner.critical_resource_versions.run_id,amount_fen:owner.financial_snapshot.amount_fen,currency:owner.financial_snapshot.currency,ledger_effect:owner.financial_snapshot.ledger_effect,facts_digest:owner.critical_resource_versions.facts_digest}};detail.approval_request_id=id(9);detail.status='DRAFT';detail.allowed_actions=['EDIT'];let request;const draft=await currentApprovalEditDraft({approvalCommandPrecheck:async b=>{request=b;return parseFinanceJSON(process.env.F15_OWNER_PROJECTION?fs.readFileSync(process.env.F15_OWNER_PROJECTION,'utf8'):raw())}},detail,'Edited reconciliation correction reason');assert.equal(request.payment_evidence_id,id(2));assert.equal(request.business_date,'2026-10-07');assert.equal(request.target_resource_id,id(1));assert.equal(draft.normalized_command.payment_evidence_id,id(2));assert.equal(draft.financial_snapshot.amount_fen,'9007199254740993');assert.ok(encodeFinanceJSON(draft).includes('"amount_fen":9007199254740993'));});
test('F15 three locale command/evidence/date fields and exact int64 range',()=>{for(const lang of ['zh','en','ja']){assert.ok(ledgerRepairCopy[lang].targets.RECONCILIATION_FINANCIAL_REPAIR);for(const key of ['reconciliation_case_id','payment_evidence_id','business_date'])assert.ok(ledgerRepairCopy[lang].fields[key])}for(const n of ['9007199254740993','9223372036854775807'])assert.equal(encodeFinanceJSON(parseFinanceJSON(`{"amount_fen":${n}}`)),`{"amount_fen":${n}}`);for(const n of ['9223372036854775808','1e3','true','{}','[]'])assert.throws(()=>parseFinanceJSON(`{"amount_fen":${n}}`));});

test('Recon calendar date and five-field public input remain closed',()=>{
  const command='RECONCILIATION_FINANCIAL_REPAIR';
  for(const business_date of ['2026-02-30','2026-13-01','2026-00-10','2026-10-07 ','2026-1-07',true])assert.ok(financialRepairPrecheckBody(command,{...values,business_date}).error);
  assert.deepEqual(Object.keys(financialRepairPrecheckBody(command,{...values,amount_fen:'10',expected_version:'1',anomaly_case_id:id(4)}).body).sort(),['command_type','target_resource_id','payment_evidence_id','business_date','reason_code'].sort());
});
test('Recon calendar preserves four-digit years and exact leap-day validity',()=>{
  const command='RECONCILIATION_FINANCIAL_REPAIR';
  for(const business_date of ['0000-02-29','0001-01-01','0099-10-07','0100-10-07','2000-02-29']){
    assert.equal(financialRepairPrecheckBody(command,{...values,business_date}).body.business_date,business_date);
    const p=parseFinanceJSON(raw());p.normalized_command.business_date=business_date;
    assert.equal(financialRepairPrecheck(p).normalized.business_date,business_date);
  }
  for(const business_date of ['0001-02-29','0099-02-29','0100-02-29','2026-02-30']){
    assert.ok(financialRepairPrecheckBody(command,{...values,business_date}).error);
    const p=parseFinanceJSON(raw());p.normalized_command.business_date=business_date;
    assert.equal(financialRepairPrecheck(p),null);
  }
});
test('Recon owner NONE, digests, run/case and exact positive int64 fail closed',()=>{
  for(const mutate of [p=>{p.critical_resource_versions.case_id=id(5)},p=>{p.critical_resource_versions.run_id='bad'},p=>{p.critical_resource_versions.facts_digest='bad'},p=>{p.critical_resource_versions.provider_snapshot_digest='bad'},p=>{p.financial_snapshot.ledger_effect='CREDIT'},p=>{p.financial_snapshot.amount_fen=9007199254740992},p=>{p.financial_snapshot.amount_fen='0'},p=>{p.financial_snapshot.amount_fen='9223372036854775808'},p=>{p.financial_snapshot.currency=''},p=>{p.financial_snapshot.provider_transaction_id=' padded '},p=>{p.normalized_command.business_date='2026-02-30'},p=>{p.normalized_command.resolution_code='OTHER'}]){const p=parseFinanceJSON(raw());mutate(p);assert.equal(financialRepairPrecheck(p),null)}
  for(const section of ['critical_resource_versions','financial_snapshot'])for(const key of Object.keys(parseFinanceJSON(raw())[section])){const p=parseFinanceJSON(raw());delete p[section][key];assert.equal(financialRepairPrecheck(p),null)}
  const p=parseFinanceJSON(raw());p.financial_snapshot.amount_fen='9223372036854775807';assert.ok(financialRepairPrecheck(p));assert.equal(financialRepairApprovalDraft(p,'Controlled maximum amount facts','recon-key').financial_snapshot.amount_fen,'9223372036854775807');
});
test('Recon prefixed request key uses exact UTF8 120byte bound',()=>{
  for(const key of ['a'.repeat(111),'中'.repeat(37)])assert.equal(financialRepairApprovalDraft(parseFinanceJSON(raw()),'Controlled request-key boundary',key).idempotency_key,`approval:${key}`);
  for(const key of ['a'.repeat(112),'中'.repeat(38)])assert.throws(()=>financialRepairApprovalDraft(parseFinanceJSON(raw()),'Controlled request-key boundary',key));
});
test('Recon fresh EDIT preserves original tuple/date and replaces only authoritative frozen documents',async()=>{
  const p=parseFinanceJSON(raw());const detail={approval_request_id:id(9),status:'DRAFT',allowed_actions:['EDIT'],command_type:p.command_type,target:{resource_type:p.target_resource_type,resource_id:id(1)},command:p.normalized_command};let request;
  const fresh=parseFinanceJSON(raw());fresh.critical_resource_versions.facts_digest='c'.repeat(64);
  const d=await currentApprovalEditDraft({approvalCommandPrecheck:async b=>{request=b;return fresh}},detail,'Controlled current original-intent EDIT');
  assert.deepEqual(request,{command_type:p.command_type,...values,reason_code:'PROVIDER_REFUND_CONFIRMED'});assert.equal(d.critical_resource_versions.facts_digest,'c'.repeat(64));assert.deepEqual(d.normalized_command,detail.command);
  for(const field of ['reconciliation_case_id','payment_evidence_id','business_date']){const changed=parseFinanceJSON(raw());changed.normalized_command[field]=field==='business_date'?'2026-10-08':id(5);await assert.rejects(currentApprovalEditDraft({approvalCommandPrecheck:async()=>changed},detail,'Controlled current original-intent EDIT'));}
});
