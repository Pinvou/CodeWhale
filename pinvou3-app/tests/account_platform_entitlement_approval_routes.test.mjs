import assert from 'node:assert/strict';
import test from 'node:test';
import { approvalCommands, readApprovalRoute, approvalBusinessRoute, approvalQuery } from '../src/features/account-platform/admin-approval-actions.js';
import { currentApprovalEditDraft } from '../src/features/account-platform/admin-finance.js';
import { entitlementHostFacts, entitlementHostID as id } from './fixtures/entitlement-host-facts.js';
const command='ENTITLEMENT_COMPONENT_ADJUSTMENT';
const detail=()=>({approval_request_id:id(9),status:'DRAFT',allowed_actions:['EDIT'],command_type:command,detail_availability:'AVAILABLE',target:{resource_type:'ENTITLEMENT_PACKAGE',resource_id:id(5)},command:entitlementHostFacts().normalized_command,critical_resource_versions:{wallet_version:1, facts_digest:'old'}});
test('installed ENT joins existing four-view filter and exact package business route without future commands',()=>{
 assert.ok(approvalCommands.includes(command));assert.equal(approvalCommands.includes('RECONCILIATION_FINANCIAL_REPAIR'),false);
 const route=readApprovalRoute(`?approval_view=mine&approval_command_type=${command}&approval_target=${id(5)}`);
 assert.deepEqual(approvalQuery(route,id(1)),{command_type:command,target:id(5),requester_user_id:id(1)});
 assert.deepEqual(approvalBusinessRoute(detail()),{section:'finance',tab:'correction'});
 for(const target of [{resource_type:'PAYMENT_ORDER',resource_id:id(5)},{resource_type:'ENTITLEMENT_PACKAGE',resource_id:id(6)},{resource_type:'ENTITLEMENT_PACKAGE',resource_id:'bad'}])assert.equal(approvalBusinessRoute({...detail(),target}),null);
});
test('ENT EDIT reads current owner proof for fixed original intent and preserves exact int64 tokens',async()=>{
 const calls=[]; const facts=entitlementHostFacts('9223372036854775807');
 const draft=await currentApprovalEditDraft({entitlementCorrectionPrecheck:async(...args)=>{calls.push(args);return facts;}},detail(),'Verified original product split evidence');
 assert.deepEqual(calls,[[id(5),id(6),'PRODUCT_SPLIT_CORRECTION']]);
 assert.equal(draft.critical_resource_versions.wallet_version,'9223372036854775807');assert.equal(draft.critical_resource_versions.facts_digest,'b'.repeat(64));
 assert.deepEqual(draft.normalized_command,detail().command);assert.equal(draft.idempotency_key,`approval:edit:${id(9)}`);
 assert.deepEqual(detail().critical_resource_versions,{wallet_version:1,facts_digest:'old'});
});
test('ENT EDIT rejects changed owner package/order/reason and incomplete projection',async()=>{
 for(const changes of [{package_id:id(15)},{payment_order_id:id(16)},{reason_code:'OTHER'}]){
  const facts=entitlementHostFacts();Object.assign(facts.normalized_command,changes);
  await assert.rejects(currentApprovalEditDraft({entitlementCorrectionPrecheck:async()=>facts},detail(),'Verified original product split evidence'));
 }
 await assert.rejects(currentApprovalEditDraft({entitlementCorrectionPrecheck:async()=>({})},detail(),'Verified original product split evidence'));
});
test('wrong ENT target and noneditable state fail before any owner read',async()=>{
 let calls=0;const client={entitlementCorrectionPrecheck:async()=>{calls++;return entitlementHostFacts();}};
 for(const value of [{...detail(),target:{resource_type:'PAYMENT_ORDER',resource_id:id(5)}},{...detail(),command:{...detail().command,payment_order_id:'bad'}},{...detail(),status:'SUBMITTED'},{...detail(),allowed_actions:[]}])await assert.rejects(currentApprovalEditDraft(client,value,'Verified original product split evidence'));
 assert.equal(calls,0);
});
