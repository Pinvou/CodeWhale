import React from 'react';
import {createRoot} from 'react-dom/client';
import {AdminPlatformView} from '../src/features/account-platform/AdminPlatformView.jsx';
import {AdminPlatformClient} from '../src/features/account-platform/admin-client.js';
import {adminPlatformCopy,auditWorkspaceCopy} from '../src/shared/i18n.js';
const params=new URLSearchParams(location.search),language=params.get('language')||'zh';
const id='00000000-0000-4000-8000-000000000017';
const state=window.f17={language,id,copy:auditWorkspaceCopy[language],calls:[],lostCreate:params.has('lost'),createUnknownForbidden:params.has('unknownForbidden'),definiteForbidden:params.has('initialForbidden'),createCount:0,lostTicket:params.has('ticketLost'),downloaded:false};
state.reauthenticated=params.has('reauthenticated');
state.actor=state.reauthenticated&&params.has('otherActor')?'00000000-0000-4000-8000-000000000018':id;
const original=window.fetch.bind(window);
const response=(body,status=200)=>new Response(JSON.stringify(body),{status,headers:{'Content-Type':'application/json'}});
state.piiAllowed=params.has('pii');
const job=()=>({export_id:id,status:'READY',filename:'controlled-audit.pae',include_pii:Boolean(state.includePII),redacted:!state.includePII,...(state.ticketIssued?{download_ticket_issued_at:'2026-10-09T00:00:00Z'}:{})});
window.fetch=async(url,options={})=>{
 if(!String(url).startsWith('/api/'))return original(url,options);
 state.calls.push({url:String(url),method:options.method||'GET',body:options.body,headers:options.headers,credentials:options.credentials,cache:options.cache});
 if(url==='/api/session')return response({authenticated:true,csrf_token:'controlled-csrf'});
 if(url==='/api/account/v1/account/bootstrap')return response({user_id:state.actor,status:'ACTIVE',active_context:{context_type:'PERSONAL',organization_id:null,membership_version:1,context_version:1,authorization_version:1,personal_authorization_version:1,platform_authorization_version:1,organization_authorization_version:1,permissions:[]},personal_permissions:[],platform_permissions:params.has('deny')||(state.reauthenticated&&params.has('lostPermission'))?[]:params.has('readOnly')?['platform.audit.read']:params.has('exportOnly')?['platform.audit.export']:params.has('piiOnly')?['platform.audit.pii.export']:state.piiAllowed?['platform.audit.read','platform.audit.export','platform.audit.pii.export']:['platform.audit.read','platform.audit.export']});
 if(String(url).includes('/audit-logs')&&params.has('exportOnly'))return response({code:'PERMISSION_DENIED'},403);
 if(String(url).includes('/audit-logs?')||String(url).endsWith('/audit-logs'))return response({items:[{audit_id:id,action:'CONTROLLED_AUDIT_ACTION',result:'SUCCEEDED',request_id:'controlled-request',resource_type:'USER',resource_id:id}]});
 if(String(url).endsWith('/audit-logs/'+id))return response({audit_id:id,action:'CONTROLLED_AUDIT_ACTION',result:'SUCCEEDED',change_summary:{status:'ACTIVE'}});
 if(String(url).endsWith('/audit-exports')&&options.method==='POST'){
  state.createCount++;
  state.includePII=JSON.parse(options.body).include_pii;
  if(!state.reauthenticated&&params.has('recentCreate'))return response({code:'RECENT_AUTHENTICATION_REQUIRED'},401);
  if(!state.reauthenticated&&params.has('unknownRecent')){if(state.createCount===1)return response({code:'UPSTREAM_UNAVAILABLE'},503);return response({code:'RECENT_AUTHENTICATION_REQUIRED'},401);}
  if(state.createCount===1&&['400','429','409'].includes(params.get('initialReject')))return response({code:'CONTROLLED_CREATE_REJECTION'},Number(params.get('initialReject')));
  if(state.definiteForbidden&&state.createCount===1)return response({code:'PERMISSION_DENIED'},403);
  if(state.createUnknownForbidden){if(state.createCount===1){state.committedJob=true;return response({code:'UPSTREAM_UNAVAILABLE'},503);}if(state.createCount===2)return response({code:'PERMISSION_DENIED'},403);}
  if(state.lostCreate){state.lostCreate=false;throw new TypeError('controlled lost create ACK');}
  return response({...job(),decryption_key:'controlled-page-only-key'},202);
 }
 if(String(url).endsWith('/download-tickets')){if(!state.reauthenticated&&params.has('ticketRecent'))return response({code:'RECENT_AUTHENTICATION_REQUIRED'},401);state.ticketIssued=true;if(state.revokeAfterTicket)state.piiAllowed=false;if(state.lostTicket){state.lostTicket=false;throw new TypeError('controlled lost ticket ACK');}return response({export_id:id,download_token:'a'.repeat(43),expires_at:'2099-10-09T00:15:00Z'},201);}
 if(String(url).endsWith('/download')){if(state.forbidDownload)return response({code:'PERMISSION_DENIED'},403);state.downloaded=true;return new Response('controlled-encrypted-envelope',{headers:{'Content-Type':'application/vnd.pinvou.audit-envelope+json'}});}
 if(String(url).endsWith('/audit-exports/'+id))return response(job());
 return response({code:'CONTROLLED_NOT_FOUND'},404);
};
const client=new AdminPlatformClient();
// Controlled authentication navigation only: the browser test explicitly returns
// through the same fixture with the nonce; no real issuer is contacted.
if(['recentCreate','unknownRecent','ticketRecent'].some(key=>params.has(key)))client.reauthenticate=returnTo=>{state.reauthURL=returnTo;};
createRoot(document.getElementById('root')).render(<AdminPlatformView client={client} copy={adminPlatformCopy[language]} language={language} onLanguageChange={()=>{}}/>);
