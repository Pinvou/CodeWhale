import test from 'node:test';
import assert from 'node:assert/strict';
import {financialRepairPrecheckBody,financialRepairCommandTypes} from '../src/features/account-platform/admin-financial-repair-form.js';
import {financialRepairApprovalDraft} from '../src/features/account-platform/admin-financial-repair-drafts.js';
import {currentApprovalEditDraft} from '../src/features/account-platform/admin-finance.js';
import {AdminPlatformClient} from '../src/features/account-platform/admin-client.js';
import {parseFinanceJSON,encodeFinanceJSON} from '../src/features/account-platform/finance-contract.js';
import {ledgerRepairCopy} from '../src/shared/ledger-repair-i18n.js';
const id=n=>`00000000-0000-4000-8000-${String(n).padStart(12,'0')}`;
const values={target_resource_id:id(1),write_off_microcredits:'9223372036854775807',expected_version:'9007199254740993'};
const raw=()=>`{"command_type":"RECEIVABLE_WRITE_OFF","policy_version":1,"snapshot_schema_version":1,"target_resource_type":"RECEIVABLE","target_resource_id":"${id(1)}","normalized_command":{"receivable_id":"${id(1)}","write_off_microcredits":9223372036854775807,"expected_receivable_version":9007199254740993,"reason_code":"APPROVED_RECEIVABLE_WRITE_OFF"},"critical_resource_versions":{"receivable_version":9007199254740993,"facts_digest":"${'a'.repeat(64)}","policy_version":1},"financial_snapshot":{"write_off_microcredits":9223372036854775807,"outstanding_microcredits":9223372036854775807,"settled_microcredits":0,"original_microcredits":9223372036854775807,"source_type":"USAGE_OVERAGE","source_reference":"usage:controlled","wallet_receivable_microcredits":9223372036854775807}}`;
test('shared Ledger and WriteOff final prefixed request key respects exact UTF8 byte boundary', () => {
  const ledger = { command_type: 'LEDGER_REPAIR', policy_version: 1, snapshot_schema_version: 1, target_resource_type: 'LEDGER_POSTING', target_resource_id: id(1), normalized_command: { target_posting_id: id(1), target_journal_entry_id: id(2), wrong_ledger_account_id: id(3), correct_ledger_account_id: id(4), amount_microcredits: '1' }, critical_resource_versions: {}, financial_snapshot: {} };
  for (const projection of [parseFinanceJSON(raw()), ledger]) {
    for (const key of ['a'.repeat(111), '中'.repeat(37), '😀'.repeat(27) + 'abc']) {
      const draft = financialRepairApprovalDraft(projection, 'Controlled key boundary reason', key);
      assert.equal(draft.idempotency_key, `approval:${key}`);
      assert.equal(new TextEncoder().encode(draft.idempotency_key).length, 120);
    }
    for (const key of ['a'.repeat(112), '中'.repeat(38), '😀'.repeat(28)]) {
      assert.throws(() => financialRepairApprovalDraft(projection, 'Controlled key boundary reason', key), error => error.code === 'FINANCIAL_REPAIR_PRECHECK_INVALID');
    }
  }
});
test('closed F11 input preserves existing Ledger plus only write-off',()=>{assert.deepEqual(financialRepairCommandTypes,['LEDGER_REPAIR','RECEIVABLE_WRITE_OFF']);assert.deepEqual(financialRepairPrecheckBody('RECEIVABLE_WRITE_OFF',values).body,{command_type:'RECEIVABLE_WRITE_OFF',...values,reason_code:'APPROVED_RECEIVABLE_WRITE_OFF'});for(const key of ['write_off_microcredits','expected_version'])for(const value of ['0','-1','9223372036854775808','1e3',' 1',9007199254740992,true,null])assert.ok(financialRepairPrecheckBody('RECEIVABLE_WRITE_OFF',{...values,[key]:value}).error);assert.ok(financialRepairPrecheckBody('PAYMENT_REPAIR_CREDIT',values).error);});
test('real precheck client emits exact numeric int64 tokens and reads original CAS losslessly',async()=>{const old=globalThis.fetch;let call;globalThis.fetch=async(url,options)=>{call={url,options};return new Response(raw(),{status:200});};try{const client=new AdminPlatformClient('https://portal.example');client.csrfToken='controlled-f11';const projection=await client.approvalCommandPrecheck(financialRepairPrecheckBody('RECEIVABLE_WRITE_OFF',values).body);assert.equal(call.url,'https://portal.example/api/admin/account/admin/v2/approval-command-prechecks');assert.ok(call.options.body.includes('"expected_version":9007199254740993'));assert.ok(call.options.body.includes('"write_off_microcredits":9223372036854775807'));assert.equal(call.options.headers['X-CSRF-Token'],'controlled-f11');assert.equal(call.options.cache,'no-store');assert.equal(projection.normalized_command.expected_receivable_version,'9007199254740993');assert.equal(projection.critical_resource_versions.receivable_version,'9007199254740993');const draft=financialRepairApprovalDraft(projection,'Controlled write-off justification','controlled-f11-key');const wire=encodeFinanceJSON(draft);assert.ok(wire.includes('"expected_receivable_version":9007199254740993'));assert.ok(wire.includes('"receivable_version":9007199254740993'));}finally{globalThis.fetch=old;}});
test('typed EDIT preserves original version/amount while getting new owner facts',async()=>{let request;const p=parseFinanceJSON(raw());const edited=await currentApprovalEditDraft({approvalCommandPrecheck:async b=>{request=b;return p;}},{approval_request_id:id(9),command_type:'RECEIVABLE_WRITE_OFF',status:'DRAFT',allowed_actions:['EDIT'],target:{resource_type:'RECEIVABLE',resource_id:id(1)},command:{receivable_id:id(1),write_off_microcredits:values.write_off_microcredits,expected_receivable_version:values.expected_version,reason_code:'APPROVED_RECEIVABLE_WRITE_OFF'}},'Updated write-off justification');assert.equal(request.expected_version,values.expected_version);assert.equal(request.write_off_microcredits,values.write_off_microcredits);assert.equal(edited.normalized_command.expected_receivable_version,values.expected_version);assert.equal(edited.target_resource_id,id(1));});
test('only published int64 version shapes can reach numeric wire',()=>{for(const key of ['expected_version','expected_receivable_version','receivable_version','receivable_version_before','receivable_version_after']){assert.equal(encodeFinanceJSON({[key]:'9223372036854775807'}),`{"${key}":9223372036854775807}`);for(const value of ['9223372036854775808','1e3',' 1',9007199254740992,true])assert.throws(()=>encodeFinanceJSON({[key]:value}),/INVALID_FINANCE_AMOUNT/);}});
test('three language exact labels are complete without fallback',()=>{for(const lang of ['zh','en','ja'])for(const key of ['receivable_id','write_off_microcredits','expected_version','expected_receivable_version'])assert.ok(ledgerRepairCopy[lang].fields[key]);for(const lang of ['zh','en','ja'])assert.ok(ledgerRepairCopy[lang].targets.RECEIVABLE_WRITE_OFF);});

import { financialRepairPrecheck } from '../src/features/account-platform/admin-financial-repair-drafts.js';
import { approvalCommands, readApprovalRoute, approvalBusinessRoute } from '../src/features/account-platform/admin-approval-actions.js';
test('WriteOff closed center/filter/drilldown accepts only its installed matching receivable',()=>{
 assert.ok(approvalCommands.includes('RECEIVABLE_WRITE_OFF'));assert.equal(approvalCommands.includes('RECONCILIATION_FINANCIAL_REPAIR'),false);
 assert.equal(readApprovalRoute('?approval_command_type=RECEIVABLE_WRITE_OFF').commandType,'RECEIVABLE_WRITE_OFF');
 const detail={detail_availability:'AVAILABLE',command_type:'RECEIVABLE_WRITE_OFF',target:{resource_type:'RECEIVABLE',resource_id:id(1)},command:{receivable_id:id(1)}};
 assert.deepEqual(approvalBusinessRoute(detail),{section:'finance',tab:'writeoff'});
 for(const patch of [{target:{resource_type:'LEDGER_POSTING',resource_id:id(1)}},{command:{receivable_id:id(2)}},{detail_availability:'DETAIL_PRESENTATION_UNAVAILABLE'},{command_type:'RECONCILIATION_FINANCIAL_REPAIR'}])assert.equal(approvalBusinessRoute({...detail,...patch}),null);
});
test('WriteOff current EDIT refuses automatic amount/version or owner-target revision',async()=>{
 const detail={approval_request_id:id(9),command_type:'RECEIVABLE_WRITE_OFF',status:'DRAFT',allowed_actions:['EDIT'],target:{resource_type:'RECEIVABLE',resource_id:id(1)},command:parseFinanceJSON(raw()).normalized_command};
 for(const change of [p=>{p.normalized_command.expected_receivable_version='9007199254740994';p.critical_resource_versions.receivable_version='9007199254740994';},p=>{p.normalized_command.write_off_microcredits='1';p.financial_snapshot.write_off_microcredits='1';},p=>{p.target_resource_id=id(2);p.normalized_command.receivable_id=id(2);},p=>{p.normalized_command.reason_code='DIFFERENT_REASON';}]){
  const p=parseFinanceJSON(raw());change(p);await assert.rejects(currentApprovalEditDraft({approvalCommandPrecheck:async()=>p},detail,'Updated controlled write-off justification'));
 }
 for(const command of [{...detail.command,reason_code:'OTHER'},{...detail.command,receivable_id:id(2)}])await assert.rejects(currentApprovalEditDraft({approvalCommandPrecheck:async()=>{assert.fail('invalid intent must not reach owner');}}, {...detail,command},'Updated controlled write-off justification'));
});
test('WriteOff owner critical and financial safety reject incomplete inconsistent projections',()=>{
 for(const change of [p=>{p.critical_resource_versions.receivable_version='1';},p=>{p.critical_resource_versions.facts_digest='';},p=>{p.financial_snapshot.source_type='LEGACY_TOPUP';},p=>{p.financial_snapshot.wallet_receivable_microcredits='1';},p=>{p.financial_snapshot.settled_microcredits='1';},p=>{p.financial_snapshot.write_off_microcredits=9007199254740992;},p=>{p.policy_version=2;}]){const p=parseFinanceJSON(raw());change(p);assert.equal(financialRepairPrecheck(p),null);}
});
test('real current and mixed legacy readers retain only installed WriteOff int64 versions and existing command semantics',async()=>{
 const old=globalThis.fetch;const rw={command_type:'RECEIVABLE_WRITE_OFF',normalized_command:{expected_receivable_version:'9007199254740993'},critical_resource_versions:{receivable_version:'9223372036854775807'}};
 const rawPayload=`{"items":[{"command_type":"WALLET_ADJUSTMENT","financial_snapshot":{"wallet_version":7}},{"command_type":"RECEIVABLE_WRITE_OFF","normalized_command":{"expected_receivable_version":9007199254740993},"critical_resource_versions":{"receivable_version":9223372036854775807}}]}`;
 globalThis.fetch=async url=>new Response(String(url).includes('/capability')?JSON.stringify({session_approval_wire_version:2,control:{version:1,epoch:1,authorization_relay_epoch:1}}):rawPayload,{status:200});
 try{const client=new AdminPlatformClient('https://portal.example');for(const [read,expected] of [[()=>client.approvalRequests(),rw],[()=>client.governedApprovalRequests(),{...rw,allowed_actions:[]}]]){const p=await read();assert.deepEqual(p.items[1],expected);assert.equal(p.items[0].financial_snapshot.wallet_version,7);}}finally{globalThis.fetch=old;}
});
