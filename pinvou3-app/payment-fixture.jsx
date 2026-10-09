import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminFinanceWorkspace } from './src/features/account-platform/AdminFinanceWorkspace.jsx';
import { AdminPlatformClient } from './src/features/account-platform/admin-client.js';
import { financeCopy, ledgerRepairCopy } from './src/shared/i18n.js';
const id = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const projection = {"command_type": "PAYMENT_FINANCIAL_REPAIR", "policy_version": 1, "snapshot_schema_version": 1, "target_resource_type": "PAYMENT_ORDER", "target_resource_id": "00000000-0000-4000-8000-000000000001", "normalized_command": {"payment_order_id": "00000000-0000-4000-8000-000000000001", "payment_evidence_id": "00000000-0000-4000-8000-000000000002", "reason_code": "CONFIRMED_PAYMENT_NOT_CREDITED"}, "critical_resource_versions": {"payment_order_updated_at": "2026-10-01T00:00:00Z", "facts_digest": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "policy_version": 1}, "financial_snapshot": {"amount_fen": "9007199254740993", "credit_microcredits": "9223372036854775807", "provider_transaction_id": "controlled-provider-tx", "wallet_id": "00000000-0000-4000-8000-000000000003", "wallet_version": "9007199254740993", "applied_to_receivables": "0"}};
const params=new URLSearchParams(location.search),language=params.get('language')||'zh';
const state=window.f10={calls:[],projection,id,language,financeCopy,ledgerRepairCopy,status:'DRAFT',version:1,loseCreate:params.has('lost'), created:false};
const client=new AdminPlatformClient();client.csrfToken='controlled-csrf';
const response=(body,status=200)=>new Response(JSON.stringify(body),{status,headers:{'Content-Type':'application/json'}});
const original=window.fetch.bind(window);
window.fetch=async(url,options={})=>{
 if(!String(url).startsWith('/api/'))return original(url,options);
 state.calls.push({url:String(url),method:options.method||'GET',body:options.body,headers:options.headers,cache:options.cache,credentials:options.credentials});
 if(url==='/api/admin/account/admin/v2/approval-command-prechecks')return response(projection);
 if(url==='/api/admin/approvals/capability')return response({session_approval_wire_version:2,control:{version:1,epoch:1,authorization_relay_epoch:1}});
 if(url==='/api/admin/approvals/requests'&&options.method==='POST'){state.created=true;if(state.loseCreate){state.loseCreate=false;throw new TypeError('controlled lost ACK');}return response({approval_request_id:id(9)},201);}
 if(String(url).startsWith('/api/admin/approvals/requests?'))return response({items:state.created?[{approval_request_id:id(9),command_type:'PAYMENT_FINANCIAL_REPAIR',status:state.status,version:state.version}]:[],has_more:false});
 if(url===`/api/admin/approvals/requests/${id(9)}/actions/submit`){state.status='SUBMITTED';state.version++;return new Response(null,{status:204});}
 if(url===`/api/admin/approvals/requests/${id(9)}`)return response({approval_request_id:id(9),command_type:'PAYMENT_FINANCIAL_REPAIR',status:state.status,version:state.version,action_control_version:1,detail_availability:'AVAILABLE',allowed_actions:state.status==='DRAFT'?['SUBMIT']:[],target:{resource_type:'PAYMENT_ORDER',resource_id:id(1)},command:projection.normalized_command,reason:'Controlled ledger correction justification'});
 return response({items:[],has_more:false});
};
createRoot(document.getElementById('root')).render(<AdminFinanceWorkspace client={client} language={language} me={{user_id:id(8),platform_permissions:params.has('deny')?[]:['platform.billing.repairs.request','platform.billing.repairs.approve']}}/>);
