import React from 'react';
import { createRoot } from 'react-dom/client';
import { Approvals } from '../../src/features/account-platform/AdminFinanceWorkspace.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { financeCopy } from '../../src/shared/i18n.js';
const id='11111111-1111-4111-8111-111111111111';
const client=new AdminPlatformClient('');client.csrfToken='SYNTHETIC_ONLY_CSRF';
const control={id,client,calls:[],version:1,committed:false,mode:new URLSearchParams(location.search).get('mode')||'ack',language:new URLSearchParams(location.search).get('language')||'en'};
control.action = new URLSearchParams(location.search).get('action') || 'APPROVE';
control.reason = ' \x85操作理由：请批准 😀\x85 ';
const digest = 'ea9704f0dc1500df04055192f7ed6bfe89e90bd48b79ba4d15b7277ecf6971ac';
const detail=()=>({approval_request_id:id,command_type:'WALLET_ADJUSTMENT',status:control.committed?(control.action==='APPROVE'?'APPROVED':'REJECTED'):'SUBMITTED',version:control.committed?5:4,action_control_version:control.version,allowed_actions:control.committed?[]:[control.action],command:{},requester:{user_id:id}});
const json=(value,status=200)=>new Response(JSON.stringify(value),{status,headers:{'Content-Type':'application/json'}});
globalThis.fetch=async(url,options={})=>{
 control.calls.push({url:String(url),method:options.method||'GET',body:options.body,headers:options.headers});
 if(url.endsWith('/approvals/capability'))return json({control:{version:control.version,epoch:1,authorization_relay_epoch:1},session_approval_wire_version:2});
 if(url.includes('/actions/decision-result?')){
  if(control.mode==='unknown')return json({code:'COMMAND_RESULT_UNKNOWN'},503);
  return json({approval_request_id:id,command_protocol:1,operation:'DECIDE',decision:control.action==='APPROVE'?'APPROVED':'REJECTED',reason_digest:digest,decision_id:'33333333-3333-4333-8333-333333333333',expected_version:4,result_version:5,origin_action_control_version:1,action_control_version:control.version,receipt_event_id:'22222222-2222-4222-8222-222222222222',recorded_at:'2026-10-25T00:00:00Z'});
 }
 if(url.endsWith('/actions/decide')){control.committed=true;if(control.mode!=='ack')throw new TypeError('controlled committed acknowledgement loss');return new Response(null,{status:204});}
 if(url.endsWith('/approvals/requests/'+id))return json(detail());
 if(url.includes('/approvals/requests?'))return json({items:[detail()],next_cursor:''});
 throw new Error('Uncontrolled decision fixture request');
};
const root=createRoot(document.querySelector('#root'));control.copy=()=>financeCopy[control.language];
window.decision=control;
root.render(<Approvals client={client} copy={control.copy()} permissions={['platform.billing.adjustments.approve']} owner={id} requestID={id} />);
