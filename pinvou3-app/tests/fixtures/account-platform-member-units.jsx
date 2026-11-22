import React from 'react';
import { createRoot } from 'react-dom/client';
import { OrganizationWorkspace } from '../../src/features/account-platform/OrganizationWorkspace.jsx';
import { OrganizationAdminWorkspace } from '../../src/features/account-platform/OrganizationAdminWorkspace.jsx';
import { AccountPlatformBffClient } from '../../src/features/account-platform/bff-client.js';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { organizationCopy } from '../../src/shared/i18n.js';
const root = createRoot(document.querySelector('#root'));
const org='11111111-1111-4111-8111-111111111111', user='22222222-2222-4222-8222-222222222222', target='33333333-3333-4333-8333-333333333333', unit='44444444-4444-4444-8444-444444444444', otherUnit='55555555-5555-4555-8555-555555555555';
const client = new AccountPlatformBffClient(), adminClient = new AdminPlatformClient();
client.csrfToken=adminClient.csrfToken='PUBLIC_SYNTHETIC_CSRF';
const control={org,user,target,unit,otherUnit,language:'en',admin:false,readonly:false,unknown:false,calls:[],receipts:new Map(),member:{organization_id:org,user_id:target,membership_id:'66666666-6666-4666-8666-666666666666',display_name:'Controlled Member',status:'ACTIVE',roles:['MEMBER'],unit_ids:[unit],unit_admin_unit_ids:[],version:7}};
const json=(body,status=200)=>new Response(JSON.stringify(body),{status,headers:{'Content-Type':'application/json'}});
globalThis.confirm=()=>true;
globalThis.fetch=async(url,options={})=>{
 const path=new URL(String(url),'http://fixture.invalid').pathname, method=options.method||'GET';
 control.calls.push({path,method,body:options.body,key:options.headers?.['Idempotency-Key']});
 if(method==='PUT'&&path.endsWith(`/members/${target}/units`)) {
  const body=JSON.parse(options.body), key=options.headers['Idempotency-Key'], previous=control.receipts.get(key);
  if(previous) return previous.body===options.body?json({...previous.result,replayed:true,member:control.member}):json({code:'CONFLICT'},409);
  if(body.expected_version!==control.member.version)return json({code:'CONFLICT'},409);
  control.member={...control.member,unit_ids:body.unit_ids,version:control.member.version+1};
  const result={organization_id:org,target_user_id:target,member:control.member,replayed:false};
  control.receipts.set(key,{body:options.body,result}); return json(result,control.unknown?202:200);
 }
 if(method==='POST'&&path.endsWith('/invitations')) {
  const body=JSON.parse(options.body);return json({invitation_id:'77777777-7777-4777-8777-777777777777',organization_id:org,target_type:body.target_type,target:body.target,roles:body.roles,unit_ids:body.unit_ids,status:'PENDING',expires_at:'2099-01-01T00:00:00Z'},201);
 }
 if(path==='/api/admin/account/admin/v1/organizations')return json({items:[{organization_id:org,name:'Controlled Org',status:'ACTIVE',version:1}],next_cursor:null});
 if(path.endsWith('/units'))return json({items:[{unit_id:unit,organization_id:org,name:'Controlled Unit',status:'ACTIVE',parent_id:null,version:1},{unit_id:otherUnit,organization_id:org,name:'Other Unit',status:'ACTIVE',parent_id:null,version:1}],structure_version:1});
 if(path.endsWith('/members'))return json({items:[control.member],next_cursor:null});
 return json({code:'UNEXPECTED_FIXTURE_ROUTE'},404);
};
const me=()=>({user_id:user,platform_permissions:control.admin?['platform.organizations.read',...(control.readonly?[]:['platform.organizations.members.manage'])]:[],active_context:{context_type:'ORGANIZATION',organization_id:org,context_version:1,organization_authorization_version:1,permissions:['org.units.read','org.members.read',...(control.readonly?[]:['org.members.manage'])]}});
function render(){root.render(control.admin?<OrganizationAdminWorkspace client={adminClient} me={me()} language={control.language}/>:<OrganizationWorkspace client={client} me={me()} language={control.language} onChanged={async()=>{}}/>);}
window.memberUnits=Object.assign(control,{render,copy:()=>organizationCopy[control.language]});render();
