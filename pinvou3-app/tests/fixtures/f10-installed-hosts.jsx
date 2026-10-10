import { entitlementHostFacts } from './entitlement-host-facts.js';
import React from 'react';
import { createRoot } from 'react-dom/client';
import { AccountPlatformView } from '../../src/features/account-platform/AccountPlatformView.jsx';
import { AdminPlatformView } from '../../src/features/account-platform/AdminPlatformView.jsx';
import { AccountPlatformBffClient } from '../../src/features/account-platform/bff-client.js';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { accountPlatformCopy, adminPlatformCopy, accountClosureCopy, entitlementCorrectionCopy, accountClosedGrantCopy, financeCopy, organizationCopy } from '../../src/shared/i18n.js';
const q = new URLSearchParams(location.search), language = q.get('language') || 'zh', mode = q.get('mode') || 'personal';
const id = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const receipt = { closure_request_id:id(9), status:'IN_PROGRESS', stage:'CLOSURE_REQUESTED', blockers:[] };
const grant = { grant_id:id(4), organization_id:id(2), member_user_id:id(3), package_id:id(5), component_id:id(6), status:'SUSPENDED', suspension_reason:'ACCOUNT_CLOSED', original_microcredits:'9007199254740993', available_microcredits:'9007199254740993', reserved_microcredits:0, used_microcredits:0, expired_microcredits:0, revoked_microcredits:0, membership_version:1, allocation_version:1 };
const ent = { facts:entitlementHostFacts(),created:q.has('approve-only'),status:q.has('approve-only')?'SUBMITTED':'DRAFT',version:1,precheckError:q.has('precheck-error') };
const entDetail = () => ({approval_request_id:id(9),command_type:'ENTITLEMENT_COMPONENT_ADJUSTMENT',status:ent.status,version:ent.version,action_control_version:1,detail_availability:'AVAILABLE',allowed_actions:ent.status==='DRAFT'?['EDIT','SUBMIT']:['APPROVE','REJECT'],target:{resource_type:'ENTITLEMENT_PACKAGE',resource_id:id(5)},command:ent.facts.normalized_command,reason:'Verified payment product split evidence'});
const state = window.installed = { calls:[], language, mode, id, ent, entDetail, adminPlatformCopy, receipt, grant, accountClosureCopy, entitlementCorrectionCopy, accountClosedGrantCopy, financeCopy, organizationCopy, user:id(1), delayed:q.has('delay'), queryError:q.has('error'), denied:q.has('deny') };
const permissions = () => mode === 'admin' ? ['platform.account-closures.read'] : mode === 'ent' ? q.has('approve-only') ? ['platform.billing.repairs.approve'] : ['platform.billing.repairs.request'] : [];
const profile = () => ({ user_id:state.user, status:'ACTIVE', personal_permissions:state.denied ? [] : mode === 'personal' ? ['personal.account.close.read', 'personal.account.close.request'] : [], platform_permissions:state.denied?[]:permissions(), active_context:{ context_type:mode === 'org' ? 'ORGANIZATION':'PERSONAL', organization_id:mode === 'org'?id(2):null, context_version:1, organization_authorization_version:1, permissions:mode==='org'&&!state.denied?['org.grants.reclaim_closed','org.quota.read.self']:[] }, platform_authorization_version:1 });
const original = window.fetch.bind(window);
window.fetch = async (url, options = {}) => {
 if (!String(url).startsWith('/api/')) return original(url, options);
 state.calls.push({url:String(url),method:options.method||'GET',body:options.body,headers:options.headers});
 if (url==='/api/session') return Response.json({authenticated:true,csrf_token:'controlled-csrf'});
 if (url==='/api/account/v1/account/bootstrap') return Response.json(profile());
 if (url==='/api/account/v1/me/organizations') return Response.json({items:[]});
 if (url==='/api/account/v1/me/account-closure-request') {
  if (state.delayed) { state.delayed=false; return new Promise(resolve=>{state.resolve=()=>resolve(Response.json(receipt));}); }
  return Response.json({code:state.queryError?'UNAVAILABLE':'NOT_FOUND'},{status:state.queryError?503:404});
 }
 if (url==='/api/account/v1/me/account-closure-requests') return Response.json(receipt,{status:202});
 if (url==='/api/admin/account/admin/v1/account-closure-requests') return Response.json({items:[{...receipt,status:'REPAIR_REQUIRED'}]});
 if (url===`/api/token/v1/organizations/${id(2)}/quota-grants`) return Response.json({items:[grant]});
 if (url===`/api/token/v1/organizations/${id(2)}/account-closed-grants/${id(4)}/reclaim`) return Response.json({grant:{...grant,status:'RECLAIMED_ACCOUNT_CLOSED',available_microcredits:0},amount_microcredits:'9007199254740993',subject_lifecycle_version:2,replayed:false});
 if (url==='/api/admin/approvals/capability') return Response.json({session_approval_wire_version:2,control:{version:1,epoch:1,authorization_relay_epoch:1}});
 if (url==='/api/admin/approvals/directory') return Response.json({schema_version:1,platform_authorization_version:1,items:[{command_type:'ENTITLEMENT_COMPONENT_ADJUSTMENT',policy_version:2,risk_class:'FINANCIAL_FACT_CHANGE',requestable:true},{command_type:'USAGE_SETTLEMENT_REPAIR',policy_version:1,risk_class:'FINANCIAL_FACT_CHANGE',requestable:true}]},{headers:{'X-Platform-Authorization-Version':'1'}});
 if (url==='/api/admin/account/admin/v2/approval-command-prechecks') return ent.precheckError ? Response.json({code:'OWNER_UNAVAILABLE'},{status:503}) : Response.json(ent.facts);
 if (url==='/api/admin/approvals/requests'&&options.method==='POST') {ent.created=true;return Response.json({approval_request_id:id(9)},{status:201});}
 if (url===`/api/admin/approvals/requests/${id(9)}`&&options.method==='PUT') {ent.version++;return new Response(null,{status:204});}
 if (url===`/api/admin/approvals/requests/${id(9)}/actions/decide`) {ent.status='APPROVED';ent.version++;return new Response(null,{status:204});}
 if (url===`/api/admin/approvals/requests/${id(9)}/actions/submit`) {ent.status='SUBMITTED';ent.version++;return new Response(null,{status:204});}
 if (url===`/api/admin/approvals/requests/${id(9)}`) return Response.json(entDetail());
 if (String(url).startsWith('/api/admin/approvals/requests?')) return Response.json({items:ent.created?[entDetail()]:[],has_more:false});
 throw new Error(`Unexpected controlled route: ${url}`);
};
window.prompt=()=> 'CLOSE_MY_ACCOUNT'; window.confirm=()=>true;
const root = createRoot(document.getElementById('root'));
const makeClient = () => { const value = ['admin','ent'].includes(mode) ? new AdminPlatformClient() : new AccountPlatformBffClient(); value.csrfToken='controlled-csrf';return value; };
state.client=makeClient();
state.render=()=>root.render(['admin','ent'].includes(mode) ? <AdminPlatformView client={state.client} copy={adminPlatformCopy[language]} language={language} onLanguageChange={()=>{}} /> : <AccountPlatformView client={state.client} copy={accountPlatformCopy[language]} language={language} onLanguageChange={()=>{}} portal />);
state.switchUser=()=>{state.user=id(8);state.client=makeClient();state.render();};
state.render();
