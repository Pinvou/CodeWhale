import React from 'react';
import {createRoot} from 'react-dom/client';
import {AdminPlatformView} from '../src/features/account-platform/AdminPlatformView.jsx';
import {AdminPlatformClient} from '../src/features/account-platform/admin-client.js';
import {adminPlatformCopy,auditWorkspaceCopy} from '../src/shared/i18n.js';
const params=new URLSearchParams(location.search),language=params.get('language')||'zh';
const id='00000000-0000-4000-8000-000000000017';
const state=window.f17={language,id,copy:auditWorkspaceCopy[language],calls:[],lostCreate:params.has('lost'),createUnknownForbidden:params.has('unknownForbidden'),definiteForbidden:params.has('initialForbidden'),createCount:0,lostTicket:params.has('ticketLost'),downloaded:false};
const original=window.fetch.bind(window);
const response=(body,status=200)=>new Response(JSON.stringify(body),{status,headers:{'Content-Type':'application/json'}});
const job=()=>({export_id:id,status:'READY',filename:'controlled-audit.pae',redacted:true,...(state.ticketIssued?{download_ticket_issued_at:'2026-10-09T00:00:00Z'}:{})});
window.fetch=async(url,options={})=>{
 if(!String(url).startsWith('/api/'))return original(url,options);
 state.calls.push({url:String(url),method:options.method||'GET',body:options.body,headers:options.headers,credentials:options.credentials,cache:options.cache});
 if(url==='/api/session')return response({authenticated:true,csrf_token:'controlled-csrf'});
 if(url==='/api/account/v1/account/bootstrap')return response({user_id:id,status:'ACTIVE',personal_permissions:[],platform_permissions:params.has('deny')?[]:params.has('readOnly')?['platform.audit.read']:['platform.audit.read','platform.audit.export']});
 if(String(url).includes('/audit-logs?'))return response({items:[{audit_id:id,action:'CONTROLLED_AUDIT_ACTION',result:'SUCCEEDED',request_id:'controlled-request',resource_type:'USER',resource_id:id}]});
 if(String(url).endsWith('/audit-logs/'+id))return response({audit_id:id,action:'CONTROLLED_AUDIT_ACTION',result:'SUCCEEDED',change_summary:{status:'ACTIVE'}});
 if(String(url).endsWith('/audit-exports')&&options.method==='POST'){
  state.createCount++;
  if(state.definiteForbidden&&state.createCount===1)return response({code:'PERMISSION_DENIED'},403);
  if(state.createUnknownForbidden){if(state.createCount===1){state.committedJob=true;return response({code:'UPSTREAM_UNAVAILABLE'},503);}if(state.createCount===2)return response({code:'PERMISSION_DENIED'},403);}
  if(state.lostCreate){state.lostCreate=false;throw new TypeError('controlled lost create ACK');}
  return response({...job(),decryption_key:'controlled-page-only-key'},202);
 }
 if(String(url).endsWith('/download-tickets')){state.ticketIssued=true;if(state.lostTicket){state.lostTicket=false;throw new TypeError('controlled lost ticket ACK');}return response({export_id:id,download_token:'a'.repeat(43),expires_at:'2099-10-09T00:15:00Z'},201);}
 if(String(url).endsWith('/download')){state.downloaded=true;return new Response('controlled-encrypted-envelope',{headers:{'Content-Type':'application/vnd.pinvou.audit-envelope+json'}});}
 if(String(url).endsWith('/audit-exports/'+id))return response(job());
 return response({code:'CONTROLLED_NOT_FOUND'},404);
};
const client=new AdminPlatformClient();
createRoot(document.getElementById('root')).render(<AdminPlatformView client={client} copy={adminPlatformCopy[language]} language={language} onLanguageChange={()=>{}}/>);
