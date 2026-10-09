import {parseFinanceJSON} from './src/features/account-platform/finance-contract.js';
import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminFinanceWorkspace } from './src/features/account-platform/AdminFinanceWorkspace.jsx';
import { AdminPlatformClient } from './src/features/account-platform/admin-client.js';
import { financeCopy, ledgerRepairCopy } from './src/shared/i18n.js';
const id = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const projection = parseFinanceJSON("{\"command_type\":\"USAGE_SETTLEMENT_REPAIR\",\"policy_version\":1,\"snapshot_schema_version\":1,\"target_resource_type\":\"USAGE_HOLD\",\"target_resource_id\":\"00000000-0000-4000-8000-000000000001\",\"normalized_command\":{\"usage_request_id\":\"00000000-0000-4000-8000-000000000002\",\"hold_id\":\"00000000-0000-4000-8000-000000000001\",\"evidence_kind\":\"LOSS_REVIEW\",\"loss_review_id\":\"00000000-0000-4000-8000-000000000003\",\"reason_code\":\"MISSING_USAGE_SETTLEMENT\"},\"critical_resource_versions\":{\"hold_updated_at\":\"2026-10-07T00:00:00.123456789Z\",\"loss_review_updated_at\":\"2026-10-07T00:00:00.123456789Z\",\"facts_digest\":\"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\",\"policy_version\":1},\"financial_snapshot\":{\"expected_microcredits\":9223372036854775807,\"settled_microcredits\":9007199254740993,\"settlement_microcredits\":9007199254740993,\"overage_microcredits\":9214364837600034814,\"wallet_id\":\"00000000-0000-4000-8000-000000000004\",\"wallet_version\":9007199254740993,\"allocations_digest\":\"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\"}}");
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
 if(String(url).startsWith('/api/admin/approvals/requests?'))return response({items:state.created?[{approval_request_id:id(9),command_type:'USAGE_SETTLEMENT_REPAIR',status:state.status,version:state.version}]:[],has_more:false});
 if(url===`/api/admin/approvals/requests/${id(9)}/actions/submit`){state.status='SUBMITTED';state.version++;return new Response(null,{status:204});}
 if(url===`/api/admin/approvals/requests/${id(9)}`)return response({approval_request_id:id(9),command_type:'USAGE_SETTLEMENT_REPAIR',status:state.status,version:state.version,action_control_version:1,detail_availability:'AVAILABLE',allowed_actions:state.status==='DRAFT'?['SUBMIT']:[],target:{resource_type:'USAGE_HOLD',resource_id:id(1)},command:projection.normalized_command,reason:'Controlled ledger correction justification'});
 return response({items:[],has_more:false});
};
createRoot(document.getElementById('root')).render(<AdminFinanceWorkspace client={client} language={language} me={{user_id:id(8),platform_permissions:params.has('deny')?[]:['platform.billing.repairs.request','platform.billing.repairs.approve']}}/>);
