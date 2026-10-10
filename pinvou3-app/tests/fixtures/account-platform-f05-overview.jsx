import React from 'react';
import { createRoot } from 'react-dom/client';
import { AdminPlatformView } from '../../src/features/account-platform/AdminPlatformView.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { adminPlatformCopy, tokenManagementCopy } from '../../src/shared/i18n.js';
import { overviewDTO } from './account-platform-f05-overview-data.js';

const id='11111111-1111-4111-8111-111111111111';const root=createRoot(document.querySelector('#root'));
const control={id,language:'en',token:true,usage:true,models:false,page:true,aggregate:true,owner:id,version:1,calls:[],pending:[],delay:false,failure:0,invalid:false,client:new AdminPlatformClient()};
const json=(value,status=200)=>new Response(JSON.stringify(value),{status,headers:{'Content-Type':'application/json'}});
const collection=items=>({items,next_cursor:null,has_more:false,limit:50,as_of:'2026-10-09T00:00:00Z'});
globalThis.fetch=async(target,options={})=>{
 const url=new URL(target,location.href);const path=url.pathname;
 control.calls.push({path,query:url.search,method:options.method||'GET',owner:control.owner});
 if(path==='/api/session')return json({authenticated:true,csrf_token:'controlled-csrf',feature_flags:{admin_model_token_page_overviews:control.page,admin_model_operations_overview:control.aggregate}});
 if(path==='/api/account/v1/account/bootstrap')return json({user_id:control.owner,status:'ACTIVE',personal_permissions:[],platform_permissions:[...(control.token?['platform.tokens.read']:[]),...(control.usage?['platform.usage.read']:[]),...(control.models?['platform.models.read']:[])],authorization_version:control.version});
 if(path.endsWith('/overviews/model-operations')||path.endsWith('/operations-overview')){
  const view=control.token&&control.usage?'TOKEN_AND_USAGE':control.token?'TOKEN_ONLY':'USAGE_ONLY';
  const aggregate=path.endsWith('/operations-overview');
  if((view!=='TOKEN_ONLY'||aggregate)&&!url.searchParams.has('window'))return json({code:'INVALID_WINDOW'},400);
  if(control.failure)return json({code:'CONTROLLED_UNAVAILABLE'},control.failure);
  const value=overviewDTO(view,url.searchParams.get('window')||'24h',aggregate);
  if(control.invalid){(value.usage?.metrics||value.provisioning?.metrics||value.metrics)[0].value=9007199254740992;}
  if(control.delay){control.delay=false;return new Promise(resolve=>control.pending.push(()=>resolve(json(value))));}
  return json(value);
 }
 if(path.endsWith('/usage-requests'))return json(collection([]));
 if(path.endsWith('/reconciliation-cases'))return json(collection([{case_id:id,case_type:'USAGE_RECONCILIATION_REVIEW',severity:'P1',status:'OPEN',resource_id:id,resolution_due_at:'2026-10-09T00:00:00Z'}]));
 if(path.endsWith('/provisionings'))return json(collection([{provisioning_id:id,model_id:id,status:'PENDING',stage:'RESOLVING',desired_generation:1,actual_generation:0,updated_at:'2026-10-09T00:00:00Z'}]));
 if(path.endsWith('/models')||path.includes('/token-accounts'))return json(collection([]));
 return json({code:'CONTROLLED_UNINSTALLED'},404);
};
control.render=()=>root.render(<AdminPlatformView key={control.owner+':'+control.version} client={control.client} copy={adminPlatformCopy[control.language]} language={control.language} onLanguageChange={value=>{control.language=value;control.render();}}/>);
control.reset=()=>{control.calls=[];control.client=new AdminPlatformClient();control.version++;control.render();};
control.labels=()=>tokenManagementCopy[control.language].modelToken;
control.resolve=()=>control.pending.splice(0).forEach(resolve=>resolve());
globalThis.f05Overview=control;
history.replaceState({},'',`${location.pathname}?operations_tab=usage`);control.render();
