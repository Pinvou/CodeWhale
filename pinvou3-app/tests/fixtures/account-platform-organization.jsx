import React from 'react';
import { createRoot } from 'react-dom/client';
import { OrganizationWorkspace } from '../../src/features/account-platform/OrganizationWorkspace.jsx';
import { OrganizationAdminWorkspace } from '../../src/features/account-platform/OrganizationAdminWorkspace.jsx';
import { AdminPlatformClient } from '../../src/features/account-platform/admin-client.js';
import { OrganizationDirectory } from '../../src/features/account-platform/OrganizationDirectory.jsx';
import { AccountPlatformBffClient } from '../../src/features/account-platform/bff-client.js';
import { organizationCopy } from '../../src/shared/i18n.js';
const root = createRoot(document.querySelector('#root'));
const org='11111111-1111-4111-8111-111111111111',user='22222222-2222-4222-8222-222222222222',other='33333333-3333-4333-8333-333333333333',unit='44444444-4444-4444-8444-444444444444';
const client = new AccountPlatformBffClient(); client.csrfToken='PUBLIC_SYNTHETIC_CSRF';
const adminClient = new AdminPlatformClient(); adminClient.csrfToken='PUBLIC_SYNTHETIC_CSRF';
const control={authorizationVersion:1,purchase:false,capabilityChannels:['ALIPAY'],productStatus:'AVAILABLE',testOnly:false,admin:false,org,user,other,unit,language:'en',readonly:false,unknown:false,delayed:false,context:'ORGANIZATION',contextVersion:1,calls:[],created:false};
const json=(value,status=200)=>new Response(JSON.stringify(value),{status,headers:{'Content-Type':'application/json'}});
globalThis.fetch=async(url,options={})=>{
 const path=new URL(String(url),'http://fixture.invalid').pathname;const method=options.method||'GET';control.calls.push({path,method,body:options.body,key:options.headers?.['Idempotency-Key']});
 if(path==='/api/admin/account/admin/v1/organizations'){
  if(method==='GET')return json({items:[{organization_id:org,name:'Controlled Org',status:'ACTIVE',member_count:1},{organization_id:other,name:'Other Org',status:'DISABLED',member_count:0}],next_cursor:null});
  if(method==='POST'){const body=JSON.parse(options.body);if(body.name!=='Controlled New Org'||body.owner_user_id!==user)return json({code:'INVALID_REQUEST'},400);return json({organization_id:unit,name:body.name,status:'ACTIVE',version:1},control.unknown?202:201);}
 }
 if(path===`/api/admin/account/admin/v1/organizations/${org}/units`||path===`/api/admin/account/admin/v1/organizations/${other}/units`){const id=path.includes(other)?other:org;return json({items:[{unit_id:unit,organization_id:id,parent_id:null,name:'Controlled Unit',status:'ACTIVE',version:1}],structure_version:1});}
 if(path===`/api/admin/account/admin/v1/organizations/${org}/members`||path===`/api/admin/account/admin/v1/organizations/${other}/members`)return json({items:[],next_cursor:null});
 if(path==='/api/account/v1/me/organizations')return json({items:[{organization_id:org,name:'Controlled Org',status:'ACTIVE',version:1}]});
 if(path==='/api/account/v1/me/invitations')return json({items:[]});
 if(path==='/api/account/v1/sessions/current/active-context'){
  const body=JSON.parse(options.body);if(body.expected_context_version!==control.contextVersion)return json({code:'CONFLICT'},409);
  control.context=body.context_type;control.contextVersion++;return json({context_type:control.context,organization_id:body.organization_id,context_version:control.contextVersion,permissions:[]});
 }
 if(path===`/api/account/v1/organizations/${org}/units`){
  if(method==='GET')return json({items:[{unit_id:unit,organization_id:org,parent_id:null,name:'Controlled Unit',status:'ACTIVE',version:1}],structure_version:1});
  if(method==='POST'){
   const body=JSON.parse(options.body);if(typeof body.name!=='string'||!body.name||body.parent_id!==null)return json({code:'INVALID_REQUEST'},400);
   const value={unit:{unit_id:unit,organization_id:org,parent_id:null,name:body.name,status:'ACTIVE',version:1},structure_version:2,replayed:false};
   if(control.delayed)return new Promise(resolve=>{control.release=()=>resolve(json(value,201));});
   return json(value,control.unknown?202:201);
  }
 }
 if(path===`/api/token/v1/organizations/${org}/recharge/products`)return json({items:[{product_id:'controlled-org-product',plan_name:'Controlled product',amount_fen:'100',channels:['ALIPAY'],availability_status:control.productStatus}]});
 if(path===`/api/token/v1/organizations/${org}/payment-capabilities`)return json({provider_id:'CONTROLLED',version:'v1',channels:control.capabilityChannels,test_only:control.testOnly,formal_collection_admitted:!control.testOnly});
 if(path===`/api/token/v1/organizations/${org}/balance`)return new Response(`{"wallet_id":"${unit}","owner_type":"ORGANIZATION","owner_id":"${org}","posted_microcredits":9007199254740993,"reserved_microcredits":0,"available_microcredits":9007199254740993}`,{status:200});
 if(path===`/api/token/v1/organizations/${org}/entitlement-packages`)return json({items:[]});
 return json({code:'UNEXPECTED_FIXTURE_ROUTE'},404);
};
const me=()=>({user_id:control.user,personal_permissions:['personal.profile.read','personal.context.switch'],platform_permissions:control.admin?['platform.organizations.read','platform.organizations.create']:[],active_context:{context_type:control.context,organization_id:control.context==='ORGANIZATION'?org:undefined,context_version:control.contextVersion,organization_authorization_version:control.authorizationVersion,permissions:['org.units.read','org.wallet.read',...(control.purchase?['org.entitlements.purchase']:[]),...(control.readonly?[]:['org.units.manage'])]}});
function render(){if(control.admin){root.render(<OrganizationAdminWorkspace client={adminClient} me={me()} language={control.language} />);return;}root.render(<><OrganizationDirectory client={client} me={me()} language={control.language} onChanged={async()=>render()} />{control.context==='ORGANIZATION'&&<OrganizationWorkspace client={client} me={me()} language={control.language} onChanged={async()=>render()} />}</>);}
window.f06=Object.assign(control,{render,copy:()=>organizationCopy[control.language],switchIdentity:()=>{control.user=other;client.financeSessionEpoch++;render();}});render();
