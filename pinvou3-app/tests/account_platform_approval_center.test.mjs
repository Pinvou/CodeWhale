import assert from 'node:assert/strict';
import test from 'node:test';
import { approvalQuery, approvalBusinessRoute, readApprovalRoute, approvalCenterVisible } from '../src/features/account-platform/admin-approval-actions.js';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';
import { loadFinancePage } from '../src/features/account-platform/admin-finance-pagination.js';
import { adminPlatformCopy } from '../src/shared/i18n.js';
const id='11111111-1111-4111-8111-111111111111';
test('mine binds current requester while pending/execution use distinct server filters',()=>{
 assert.deepEqual(approvalQuery({view:'mine',commandType:'WALLET_ADJUSTMENT',target:id,status:'DRAFT'},id),{command_type:'WALLET_ADJUSTMENT',target:id,requester_user_id:id,status:'DRAFT'});
 assert.deepEqual(approvalQuery({view:'pending',status:'EXECUTED'},id),{status:'SUBMITTED'});
 assert.deepEqual(approvalQuery({view:'execution',status:'DRAFT'},id),{status:'EXECUTION_FAILED'});
 assert.throws(()=>approvalQuery({view:'mine'},''));
});
test('deep-link fields do not enable future command or invalid status/view',()=>{
 assert.deepEqual(readApprovalRoute('?approval_view=repair&approval_command_type=LEDGER_POSTING_RECLASSIFY&approval_status=FUTURE'),{view:'mine',commandType:'',target:'',status:'',requestID:''});
 const route=readApprovalRoute(`?approval_view=execution&approval_command_type=PAYMENT_PRODUCT_RESUME&approval_target=${id}&approval_status=EXECUTED&approval_request_id=${id}`);
 assert.equal(route.status,'EXECUTED');assert.equal(route.requestID,id);
});
test('drilldown maps only matching authoritative current business targets',()=>{
 const detail={detail_availability:'AVAILABLE',command_type:'WALLET_ADJUSTMENT',target:{resource_type:'MANUAL_ADJUSTMENT',resource_id:id},command:{adjustment_id:id}};
 assert.deepEqual(approvalBusinessRoute(detail),{section:'finance',tab:'wallets'});
 for(const [command_type,resource_type,field] of [['PAYMENT_PROVIDER_RESUME','PAYMENT_PROVIDER_CONTROL','provider_id'],['PAYMENT_PRODUCT_RESUME','PAYMENT_PRODUCT_CONTROL','product_id']]) assert.deepEqual(approvalBusinessRoute({...detail,command_type,target:{resource_type,resource_id:'controlled'},command:{[field]:'controlled'}}),{section:'finance',tab:'controls'});
 for(const value of [{...detail,detail_availability:'DETAIL_PRESENTATION_UNAVAILABLE'},{...detail,command:{adjustment_id:'wrong'}},{...detail,command_type:'TOKEN_CREDENTIAL_REVOKE'}])assert.equal(approvalBusinessRoute(value),null);
});
test('center visibility preserves requester/approver/read scope without future permission',()=>{
 assert.ok(approvalCenterVisible(['platform.billing.payment-controls.resume.approve']));assert.ok(approvalCenterVisible(['platform.billing.repairs.request']));assert.ok(approvalCenterVisible(['platform.billing.wallets.read']));assert.equal(approvalCenterVisible(['platform.models.lifecycle.approve']),false);assert.equal(approvalCenterVisible([]),false);
});
test('current client sends actual directory and mine filters with control and signal',async()=>{
 const original=globalThis.fetch;const calls=[];const client=new AdminPlatformClient();let bad=false;
 globalThis.fetch=async(url,options)=>{calls.push({url,options});if(url.endsWith('/capability'))return new Response(JSON.stringify({control:{version:1,epoch:1,authorization_relay_epoch:1},session_approval_wire_version:2}));if(url.endsWith('/directory'))return new Response(JSON.stringify({schema_version:1,platform_authorization_version:1,items:[]}),{headers:{'X-Platform-Authorization-Version':bad?'2':'1'}});return new Response('{"items":[]}');};
 try{const controller=new AbortController();await client.governedApprovalRequests(approvalQuery({view:'mine',target:id,status:'DRAFT'},id),controller.signal);const call=calls.find(c=>c.url.includes('/requests?'));const query=new URL(call.url,'https://controlled.invalid').searchParams;assert.equal(query.get('requester_user_id'),id);assert.equal(query.get('target'),id);assert.equal(call.options.credentials,'same-origin');assert.ok(call.options.signal instanceof AbortSignal);assert.deepEqual((await client.governedApprovalDirectory(controller.signal)).items,[]);bad=true;await assert.rejects(client.governedApprovalDirectory());}finally{globalThis.fetch=original;}
});
test('expired snapshot retains target, command, requester and status for actual client restart',async()=>{
 const original=globalThis.fetch;const queries=[];const client=new AdminPlatformClient();let restarted=0;
 globalThis.fetch=async(url)=>{if(url.endsWith('/capability'))return new Response(JSON.stringify({control:{version:1,epoch:1,authorization_relay_epoch:1},session_approval_wire_version:2}));const query=new URL(url,'https://controlled.invalid').searchParams;queries.push(query);return query.get('cursor')?new Response('{"code":"PAGINATION_CURSOR_EXPIRED"}',{status:410}):new Response('{"items":[]}');};
 try{await loadFinancePage({...approvalQuery({view:'mine',commandType:'WALLET_ADJUSTMENT',target:id,status:'DRAFT'},id),cursor:'expired'},filters=>client.governedApprovalRequests(filters),()=>restarted++);assert.equal(restarted,1);assert.equal(queries.length,2);for(const query of queries){assert.equal(query.get('target'),id);assert.equal(query.get('requester_user_id'),id);assert.equal(query.get('status'),'DRAFT');assert.equal(query.get('command_type'),'WALLET_ADJUSTMENT');}assert.equal(queries[1].get('cursor'),null);}finally{globalThis.fetch=original;}
});
test('three locale center labels are explicit',()=>{for(const language of ['zh','en','ja']){assert.equal(typeof adminPlatformCopy[language].nav.approvals,'string');for(const key of ['mine','pending','execution','directory','target','openBusinessArea'])assert.equal(typeof adminPlatformCopy[language].approvalCenter[key],'string');}});
