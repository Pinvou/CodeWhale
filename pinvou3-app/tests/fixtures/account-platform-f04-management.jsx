import { AdminModelTokenWorkspace } from '../../src/features/account-platform/AdminModelTokenWorkspace.jsx';
import React from 'react';
import '../../src/features/account-platform/admin-token-management.css';
import { createRoot } from 'react-dom/client';
import { PricingLifecycleWorkspace } from '../../src/features/account-platform/PricingLifecycleWorkspace.jsx';
import { AdminManagementWorkspace } from '../../src/features/account-platform/AdminManagementWorkspace.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { adminPlatformCopy, tokenManagementCopy } from '../../src/shared/i18n.js';
import { parseFinanceJSON } from '../../src/features/account-platform/finance-contract.js';
import { AdminHighRiskActionDialog } from '../../src/features/account-platform/AdminHighRiskActionDialog.jsx';
import { AdminTokenAccountDetailWorkspace } from '../../src/features/account-platform/AdminTokenAccountDetailWorkspace.jsx';
import { requireGovernedTokenCommandResult } from '../../src/features/account-platform/admin-high-risk-action-state.js';
const id = '11111111-1111-4111-8111-111111111111';
const other = '22222222-2222-4222-8222-222222222222';
const root = createRoot(document.querySelector('#root'));
const client = new AdminPlatformClient();client.csrfToken='CONTROLLED_SYNTHETIC_CSRF';
const unit = { unit_definition_id: id, unit_code: 'INPUT', version: 1, minimum_precision: '1', price_denominator: '1000000', rounding_rule: 'CEILING', status: 'ACTIVE' };
const formula = { formula_version_id: id, protocol_code: 'CONTROLLED', status: 'PUBLISHED', version: 1, required_unit_versions: { INPUT: id } };
const price = { price_version_id: id, scope_type: 'GLOBAL', scope_id: null, protocol_code: 'CONTROLLED', formula_version_id: id, credit_unit: 'MICROCREDIT', lifecycle_status: 'DRAFT', derived_status: 'DRAFT', effective_from: '2099-01-01T00:00:00+08:00', effective_to: null, space_etag: '9007199254740993', version: 1, units: [{ unit_definition_id: id, config_type: 'FIXED_PRICE', fixed_price_per_denominator: '9007199254740993' }] };
const control = { id, other, client, owner: id, language: 'en', surface: 'pricing', prices: [], units: [unit], calls: [], unknownOnce: false, delayOnce: false, canManage: true, receipts: new Map(), approvalStatus: 'DRAFT', approvalVersion: 1 };
// The v2 presenter sends decimal-string command versions. A response fixture
// must preserve that DTO rather than use the numeric request-body encoder.
const json = (value, status = 200) => new Response(status===204 ? null : JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
const approval = () => ({ approval_request_id:id,command_type:'TOKEN_MODEL_LIFECYCLE_CHANGE',status:control.approvalStatus,version:control.approvalVersion,action_control_version:1,detail_availability:'AVAILABLE',allowed_actions:control.approvalStatus==='DRAFT'?['EDIT','SUBMIT']:[],target:{resource_type:'MODEL',resource_id:id},command:{model_id:id,target_lifecycle:'DEPRECATED',effective_at:'2099-01-01T00:00:00+08:00',existing_session_cutoff_at:'2099-01-02T00:00:00+08:00',replacement_model_id:null,lifecycle_plan_id:id,lifecycle_plan_version:'9007199254740993'} });
globalThis.fetch = async (target,options={}) => {
 const url=new URL(target,globalThis.location.href);const path=url.pathname;const method=options.method||'GET';
 if(control.surface==='governance'||control.surface==='governance-disabled'){
  (control.httpCalls||=[]).push({path,method,query:url.search,headers:options.headers});
  try {
   if(path.endsWith(`/admin/v2/token-accounts/${id}`))return json(await governanceBackend.tokenAccountSafeDetail());
   if(path.endsWith(`/token-accounts/${id}/routes`))return json(await governanceBackend.tokenAccountRoutes(id,{cursor:url.searchParams.get('cursor')||'',...(url.searchParams.has('health_status')?{health_status:url.searchParams.get('health_status')}: {})}));
   if(path.endsWith('/models'))return json(await governanceBackend.catalogModels());
   if(path.endsWith('/commands:precheck'))return json(await governanceBackend.governedTokenCommandPrecheck(JSON.parse(options.body)));
   if(path.endsWith('/commands/by-idempotency-key'))return json(await governanceBackend.governedTokenCommandByIdempotencyKey(url.searchParams.get('command_type'),options.headers['Idempotency-Key']));
   if(path.endsWith('/commands'))return json(await governanceBackend.createGovernedTokenCommand(JSON.parse(options.body),options.headers['Idempotency-Key']));
   throw new Error('unexpected governance HTTP route');
  }catch(error){return json({code:error.code||'UPSTREAM_UNAVAILABLE',request_id:id},error.status||503);}
 }

 control.calls.push({path,method,body:options.body,key:options.headers?.['Idempotency-Key']});
 if(path.endsWith('/commands') || path.endsWith('/commands/by-idempotency-key')) {
  if(method==='POST'&&control.expiredEntryOnce){control.expiredEntryOnce=false;return json({code:'CREDENTIAL_REQUEST_DEADLINE_EXCEEDED',request_id:id},503);}
  const type=control.commandType||'TOKEN_CREDENTIAL_VALIDATE';
  const receipt={command_reference:id,command_type:type,policy_version:1,actor_user_id:id,actor_permission:'platform.tokens.credentials.validate',authorization_version:1,idempotency_fingerprint:'a'.repeat(64),target_resource_type:type==='TOKEN_MODEL_CATALOG_SYNC'?'MODEL_CATALOG':type==='TOKEN_MODEL_PUBLISH'?'MODEL':'PROVIDER_CREDENTIAL',target_resource_id:type==='TOKEN_MODEL_CATALOG_SYNC'?'CURRENT':id,status:type==='TOKEN_CREDENTIAL_VALIDATE'?'FAILED':'SUCCEEDED',redacted_result:{},stable_error_code:type==='TOKEN_CREDENTIAL_VALIDATE'?'CREDENTIAL_PROBE_OUTCOME_UNKNOWN':null,approval_request_id:null,audit_request_id:id,created_at:'2026-10-08T00:00:00Z',completed_at:'2026-10-08T00:00:01Z'};
  if(method==='POST'&&control.unknownOnce){control.unknownOnce=false;throw new TypeError('controlled lost receipt');}
  return json(receipt);
 }
 if(path.endsWith('/api/admin/approvals/capability'))return json({session_approval_wire_version:2,control:{version:1,epoch:1,authorization_relay_epoch:1}});
 if(path.endsWith('/api/admin/approvals/requests')&&method==='GET')return json({items:[approval()]});
 if(path.endsWith(`/api/admin/approvals/requests/${id}`)){if(method==='PUT'){control.approvalVersion++;return json(null,204);}return json(approval());}
 if(path.endsWith(`/api/admin/approvals/requests/${id}/actions/submit`)){control.approvalStatus='SUBMITTED';control.approvalVersion++;return json(null,204);}
 if(method==='GET'){
  if(path.endsWith(`/models/${id}`))return json({model:control.models[0],draft:null,protocol:{},formula:null,as_of:'2026-10-08T00:00:00Z'});
  if(path.endsWith('/pricing'))return json({items:control.prices,current_space_etag:'9007199254740993',has_more:false});
  if(path.endsWith('/metering-units'))return json({items:control.units});
  if(path.endsWith('/protocols'))return json({protocols:[],formulas:[formula]});
  if(path.endsWith('/models'))return json({items:control.models||[],limit:50,has_more:false,next_cursor:null,as_of:'2099-01-01T00:00:00Z'});
  if(path.endsWith('/formulas'))return json({items:[formula],limit:50,has_more:false,next_cursor:null});
 }
 const input=parseFinanceJSON(options.body||'{}');const key=options.headers?.['Idempotency-Key'];
 let receipt=control.receipts.get(key);
 if(receipt){if(receipt.body!==options.body||receipt.path!==path||receipt.method!==method)return json({code:'IDEMPOTENCY_CONFLICT'},409);return json(receipt.item,receipt.status);}
 let item,status=200;
 if(path.endsWith('/pricing/drafts')){item={...price,...input};control.prices=[item];status=201;}
 else if(path.endsWith('/draft')){item={...price,...input};control.prices=[item];}
 else if(path.endsWith('/publish')){item={...price,lifecycle_status:'PUBLISHED',derived_status:'PUBLISHED'};control.prices=[item];}
 else if(path.endsWith('/transition')){item={...price,lifecycle_status:'CANCELLED',derived_status:'CANCELLED'};control.prices=[item];}
 else if(path.endsWith('/metering-units')){item={...unit,...input,unit_definition_id:other};control.units.push(item);status=201;}
 else if(path.endsWith('/deactivate')){item={...unit,status:'INACTIVE'};control.units=[item];}
 else throw new Error('UNEXPECTED_CONTROLLED_ROUTE');
 if(!key)throw new Error('MISSING_PRICING_INVOCATION_KEY');
 receipt={body:options.body,path,method,item,status};control.receipts.set(key,receipt);
 if(control.unknownOnce){control.unknownOnce=false;return json({code:'UPSTREAM_UNAVAILABLE'},503);}
 if(control.delayOnce){control.delayOnce=false;return new Promise(resolve=>{control.release=()=>resolve(json(item,status));});}
 return json(item,status);
};
const credentialClient={ tokenAccountSafeDetail:async()=>({token_account_id:id,name:'Controlled account',status:'ACTIVE',owner_type:'USER',owner_id:id,share_scope:'PRIVATE',account_version:1,active_credential_count:0,route_count:0,updated_at:'2026-10-08T00:00:00Z'}),tokenAccountCredentials:async()=>({items:[],has_more:false}) };

const governedAccount = { token_account_id:id,name:'Controlled account',status:'ACTIVE',owner_type:'USER',owner_id:id,share_scope:'PRIVATE',account_version:1,active_credential_count:0,route_count:1,updated_at:'2026-10-08T00:00:00Z' };
const governedRoute = { route_id:other, token_account_id:id, model_id:id, protocol_version_id:id, protocol_code:'CONTROLLED', priority:100, default_scope:'NONE',health_status:'HEALTHY',health_diagnostic_code:'HEALTHY',health_observed_at:'2026-10-08T00:00:00Z',health_expires_at:'2099-01-01T00:00:00Z',route_version:1,route_generation:1,account_version:1,updated_at:'2026-10-08T00:00:00Z' };
const governanceBackend = {
 tokenAccountSafeDetail:async()=>({...governedAccount}),
 tokenAccountRoutes:async(account,filters)=>{
  control.calls.push({method:'GET',path:'routes',filters});
  const page=(items,has_more=false,next_cursor=null)=>({items,has_more,next_cursor,as_of:'2026-10-08T00:00:00Z'});
  if(Object.keys(filters).length===1){
   if(control.lookupMode==='error')throw Object.assign(new Error('controlled route source unavailable'),{status:503,code:'QUERY_TIMEOUT'});
   if(control.lookupMode==='cycle')return page([],true,'controlled-loop');
   if(control.lookupMode==='asof')return filters.cursor?{...page([]),as_of:'2026-10-08T00:00:01Z'}:page([],true,'lookup-second');
   if(control.lookupMode==='foreign')return page([{...governedRoute,token_account_id:other}]);
   if(control.lookupMode==='absent')return page([]);
   const existing={...governedRoute,priority:77,default_scope:'UNIT'};
   if(control.lookupMode==='outside')return filters.cursor?page([existing]):page([{...governedRoute,model_id:other}],true,'lookup-second');
   if(control.lookupMode==='late'){
    if(!control.lookupStarted){control.lookupStarted=true;return new Promise(resolve=>{control.releaseLookup=()=>resolve(page([existing]));});}
    return page([existing,{...governedRoute,model_id:other,priority:23,default_scope:'ORG'}]);
   }
   return page([governedRoute],true,'controlled-next');
  }
  return page([control.lookupMode==='outside'?{...governedRoute,model_id:other}:governedRoute],true,'controlled-next');
 },
 catalogModels:async()=>({items:[{model_id:id,display_name:'Controlled model',protocol_definition_id:id,protocol_code:'CONTROLLED',protocol_version:1,lifecycle_status:'PUBLISHED',source_available:true},...(control.lookupMode==='late'?[{model_id:other,display_name:'Controlled second model',protocol_definition_id:id,protocol_code:'CONTROLLED',protocol_version:1,lifecycle_status:'PUBLISHED',source_available:true}]:[])],has_more:false}),
 governedTokenCommandPrecheck:async(input)=>{control.calls.push({method:'PRE',...input});return {action:input.command_type,precheck_fingerprint:'a'.repeat(64),requires_cas_revalidation:true,snapshot_semantics:'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS',blockers:[],token_observed_at:'2026-10-08T00:00:00Z',expected_versions:{token_account_id:id,expected_account_version:1,expected_route_version:control.lookupMode==='absent'?0:1},evidence:{resource_type:input.target_resource_type,resource_id:input.target_resource_id,current_status:'ACTIVE',current_version:{evidence_etag:'"impact-'+ 'a'.repeat(64)+'"',evidence_set_sha256:'a'.repeat(64)},dependencies:Object.fromEntries(['routes','healthy_routes','provisionings','ready_provisionings','active_leases','inflight_requests','active_sessions','alternative_credentials','alternative_accounts','affected_coverage_tuples','remaining_ready_coverage','coverage_gaps'].map(k=>[k,0]))}};},
 createGovernedTokenCommand:async(input,key)=>{
  control.calls.push({method:'POST',path:'commands',body:JSON.stringify(input),key});
  const result=input.command_type==='TOKEN_ROUTE_UPSERT'?{...governedRoute,route_id:id,model_id:input.command.model_id,priority:input.command.priority,default_scope:input.command.default_scope,route_version:control.lookupMode==='absent'?1:2,account_version:2}:input.command_type==='TOKEN_ROUTE_DELETE'?{route_id:other,token_account_id:id,model_id:id,protocol_version_id:id,deleted:true,deleted_route_version:1,route_version:2,route_generation:2,account_version:2,deleted_at:'2026-10-08T00:00:01Z'}:{};
  const receipt={command_reference:id,command_type:input.command_type,policy_version:1,actor_user_id:id,actor_permission:'platform.tokens.manage',authorization_version:1,idempotency_fingerprint:'a'.repeat(64),target_resource_type:input.target_resource_type,target_resource_id:input.target_resource_id,status:'SUCCEEDED',redacted_result:result,stable_error_code:null,approval_request_id:null,audit_request_id:id,created_at:'2026-10-08T00:00:00Z',completed_at:'2026-10-08T00:00:01Z'};
  control.receipts.set(key,receipt);
  if(control.unknownOnce){control.unknownOnce=false;throw Object.assign(new Error('controlled lost ACK'),{status:503,code:'UPSTREAM_UNAVAILABLE'});}
  return receipt;
 },
 governedTokenCommandByIdempotencyKey:async(type,key)=>{control.calls.push({method:'GET',path:'by-key',type,key});return control.receipts.get(key);},
};
const governanceClient = client;
control.governanceWords=()=>tokenManagementCopy[control.language].modelToken;
function ControlledCommand({copy}) {
 const type=control.commandType||'TOKEN_CREDENTIAL_VALIDATE';const expected={commandType:type,targetResourceType:type==='TOKEN_MODEL_CATALOG_SYNC'?'MODEL_CATALOG':type==='TOKEN_MODEL_PUBLISH'?'MODEL':'PROVIDER_CREDENTIAL',targetResourceID:type==='TOKEN_MODEL_CATALOG_SYNC'?'CURRENT':id};
 return <AdminHighRiskActionDialog key={type} copy={copy} action={{key:'controlled-command',impactAction:type,title:copy.validateCredential}} target={{label:'Controlled source'}} impactRequired={false} initialReason="Controlled verification reason" onClose={()=>{control.closed=true;root.render(<p>Controlled dialog closed</p>);}} execute={async(payload,key,signal)=>requireGovernedTokenCommandResult(await client.createGovernedTokenCommand({command_type:type,reason:payload.reason},key,signal),expected)} recoverByIdempotencyKey={async(key,signal)=>requireGovernedTokenCommandResult(await client.governedTokenCommandByIdempotencyKey(type,key,signal),expected)} />;
}
function render(){
 const shared=tokenManagementCopy[control.language];const copy={...adminPlatformCopy[control.language],modelToken:shared.modelToken,billing:{pricingLifecycle:shared.pricingLifecycle}};
 const permissions=['platform.pricing.read','platform.models.read','platform.models.lifecycle.request',...(control.canManage?['platform.pricing.manage']:[])];
 root.render(control.surface==='governance-disabled'?<AdminModelTokenWorkspace section="tokenResources" client={governanceClient} copy={copy} permissions={['platform.tokens.read','platform.tokens.manage','platform.models.read']} tokenDetailsEnabled modelTokenActionsEnabled={false} routeVersion={0} onRouteChange={()=>{}} />:control.surface==='governance'?<AdminManagementWorkspace key={control.language} client={governanceClient} copy={copy} permissions={control.governancePermissions||['platform.tokens.read','platform.tokens.manage','platform.models.read']} language={control.language} owner={id}/>:control.surface==='command'?<ControlledCommand copy={shared.modelToken}/>:control.surface==='credentials'?<AdminTokenAccountDetailWorkspace accountID={id} client={credentialClient} copy={shared.modelToken} permissions={[]} routeVersion={0} onRouteChange={()=>{}} onQueryChange={()=>{}} cursorHistory={{current:{}}} actionsEnabled={false}/>:control.surface==='pricing'?<PricingLifecycleWorkspace key={`${control.owner}:${client.financeSessionEpoch}`} {...{client,copy,permissions}} owner={control.owner}/>:<AdminManagementWorkspace key={`${control.owner}:${client.financeSessionEpoch}`} {...{client,copy,permissions}} owner={control.owner} language={control.language} credentialOrigin="https://credentials-uat.pinvou.com"/>);
}
control.sourceMissingPolicyLabel=()=>tokenManagementCopy[control.language].modelToken.sourceMissingPolicy;control.modelStatusLabels=()=>tokenManagementCopy[control.language].modelToken.enumLabels;control.expiredEntryLabel=()=>tokenManagementCopy[control.language].modelToken.commandErrors.CREDENTIAL_REQUEST_DEADLINE_EXCEEDED;control.credentialProbeLabel=()=>tokenManagementCopy[control.language].modelToken.commandErrors.CREDENTIAL_PROBE_OUTCOME_UNKNOWN;control.render=render;control.labels=()=>tokenManagementCopy[control.language].pricingLifecycle;control.price=price;window.f04=control;render();
