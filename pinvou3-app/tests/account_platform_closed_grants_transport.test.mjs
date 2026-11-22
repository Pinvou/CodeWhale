import test from 'node:test';
import assert from 'node:assert/strict';
import { OrganizationClient } from '../src/features/account-platform/organization-client.js';
const id = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const grant={grant_id:id(2),organization_id:id(1),member_user_id:id(3),package_id:id(4),component_id:id(5),status:'RECLAIMED_ACCOUNT_CLOSED',membership_version:1,allocation_version:1,original_microcredits:'9007199254740993',available_microcredits:0,reserved_microcredits:0,used_microcredits:0,expired_microcredits:0,revoked_microcredits:0};
const receipt={grant,amount_microcredits:'9007199254740993',subject_lifecycle_version:2,replayed:true};
test('single reclaim preserves the original key and strict lossless lifecycle receipt',async()=>{
 const calls=[];const api=new OrganizationClient({request:async(...values)=>{calls.push(values);return receipt;}});
 assert.deepEqual(await api.reclaimClosedGrant(id(1),id(2),'ACCOUNT_CLOSED_FULL_RECLAIM','controlled-reclaim-key'),receipt);
 const [area,path,options]=calls[0];assert.equal(area,'token');assert.equal(path,`/v1/organizations/${id(1)}/account-closed-grants/${id(2)}/reclaim`);assert.equal(options.method,'POST');assert.equal(options.idempotencyKey,'controlled-reclaim-key');assert.equal(options.headers['Idempotency-Key'],'controlled-reclaim-key');assert.deepEqual(options.body,{reason:'ACCOUNT_CLOSED_FULL_RECLAIM'});
});
test('single receipt cannot substitute another org/grant or a nonterminal lifecycle',async()=>{
 for(const value of [{...receipt,grant:{...grant,organization_id:id(9)}},{...receipt,grant:{...grant,grant_id:id(9)}},{...receipt,subject_lifecycle_version:1},{...receipt,grant:{...grant,status:'SUSPENDED'}},{...receipt,amount_microcredits:0}]){
  await assert.rejects(new OrganizationClient({request:async()=>value}).reclaimClosedGrant(id(1),id(2),'reason','controlled-reclaim-key'),/INVALID_ORGANIZATION_ACK/);
 }
});
test('batch uses canonical 207 route and preserves per-item rejection without invented receipt',async()=>{
 let options;const api=new OrganizationClient({request:async(area,path,input)=>{assert.equal(path,`/v1/organizations/${id(1)}/account-closed-grants:reclaim-batch`);options=input;return {items:[{grant_id:id(2),status:'SUCCEEDED',result:receipt},{grant_id:id(6),status:'FAILED',code:'ACCOUNT_CLOSED_GRANT_RECLAIM_REJECTED'}]};}});
 const value=await api.reclaimClosedGrantsBatch(id(1),[id(2),id(6)],'reason','controlled-batch-key');assert.equal(value.items.length,2);assert.deepEqual(options.expectedStatus,[207]);assert.equal(options.idempotencyKey,'controlled-batch-key');
 await assert.rejects(api.reclaimClosedGrantsBatch(id(1),[id(2),id(2)],'reason','controlled-batch-key'),/INVALID_ORGANIZATION_INPUT/);
});

test('a rejected batch item cannot carry a financial receipt', async () => {
 const api = new OrganizationClient({ request: async () => ({ items: [{ grant_id: id(2), status: 'FAILED', code: 'ACCOUNT_CLOSED_GRANT_RECLAIM_REJECTED', result: receipt }] }) });
 await assert.rejects(api.reclaimClosedGrantsBatch(id(1), [id(2)], 'reason', 'controlled-batch-key'), /INVALID_ORGANIZATION_ACK/);
});

test('an unknown batch item cannot acknowledge or redispatch the original command', async () => {
 const calls = [];
 const api = new OrganizationClient({ request: async (...args) => {
  calls.push(args);
  return { items: [{ grant_id: id(2), status: 'FAILED', code: 'COMMAND_RESULT_UNKNOWN' }] };
 } });
 await assert.rejects(api.reclaimClosedGrantsBatch(id(1), [id(2)], 'reason', 'controlled-batch-key'), error => error.code === 'COMMAND_RESULT_UNKNOWN' && error.status === 503);
 assert.equal(calls.length, 1);
 assert.equal(calls[0][2].idempotencyKey, 'controlled-batch-key');
});
