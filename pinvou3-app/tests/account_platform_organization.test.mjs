import test from 'node:test';
import assert from 'node:assert/strict';
import { OrganizationClient, OrganizationAdminClient, requireQuotaGrant, organizationPaymentChannels } from '../src/features/account-platform/organization-client.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
import { AccountPlatformDesktopClient } from '../src/features/account-platform/desktop-client.js';
import { organizationCopy } from '../src/shared/i18n.js';
import { hasPermission } from '../src/features/account-platform/authorization-ui.js';
const org='11111111-1111-4111-8111-111111111111',user='22222222-2222-4222-8222-222222222222',other='33333333-3333-4333-8333-333333333333',grantID='44444444-4444-4444-8444-444444444444';
const grant={grant_id:grantID,organization_id:org,member_user_id:user,package_id:org,component_id:other,status:'ACTIVE',membership_version:2,allocation_version:1,original_microcredits:'9007199254740993',available_microcredits:'9007199254740993',reserved_microcredits:'0',used_microcredits:'0',expired_microcredits:'0',revoked_microcredits:'0'};
const facts={items:[{member_user_id:user,package_id:org,component_id:other,membership_version:2,component_type:'BASE',amount_microcredits:'9007199254740993'}]};
test('actual Admin list DTO has no fictional version requirement; public collection checks each organization', async()=>{
 const admin=new OrganizationAdminClient({request:async()=>({items:[{organization_id:org,name:'Owned',status:'ACTIVE',member_count:1}],next_cursor:null})});assert.equal((await admin.list()).items[0].organization_id,org);
 const client=new OrganizationClient({request:async()=>({items:[{organization_id:org,name:'Owned',status:'ACTIVE',version:1},{organization_id:other,name:'Other',status:'DISABLED',version:2}]})});assert.equal((await client.organizations()).length,2);
});
test('switch ACK binds target and actual unchanged-or-advanced context version',async()=>{
 const client=new OrganizationClient({request:async(_a,_p,options)=>({context_type:options.body.context_type,organization_id:options.body.organization_id,context_version:3,permissions:[]})});await client.switchContext(org,3);await client.switchContext(null,2);
 client.client.request=async()=>({context_type:'ORGANIZATION',organization_id:other,context_version:4,permissions:[]});await assert.rejects(client.switchContext(org,3),/INVALID_ORGANIZATION_ACK/);
});
test('organization quota ACK requires scope, exact amount, member version and complete indexed results',async()=>{
 const client=new OrganizationClient({request:async()=>({items:[{index:0,status:'SUCCEEDED',grant}]})});assert.equal((await client.allocate(org,facts,'owned-quota-operation-key')).items[0].grant.original_microcredits,'9007199254740993');
 for(const result of [{items:[]},{items:[{index:1,status:'SUCCEEDED',grant}]},{items:[{index:0,status:'SUCCEEDED',grant:{...grant,organization_id:other}}]},{items:[{index:0,status:'SUCCEEDED',grant:{...grant,original_microcredits:'9007199254740992'}}]},{items:[{index:0,status:'SUCCEEDED',grant:{...grant,membership_version:3}}]},{items:[{index:0,status:'FAILED',code:''}]}]){client.client.request=async()=>result;await assert.rejects(client.allocate(org,facts,'owned-quota-operation-key'));}
 client.client.request=async()=>({items:[{index:0,status:'FAILED',code:'INSUFFICIENT_COMPONENT_CAPACITY'}]});assert.equal((await client.allocate(org,facts,'owned-quota-operation-key')).items[0].status,'FAILED');
});
test('quota grant rejects rounded money and wrong expected target',()=>{assert.throws(()=>requireQuotaGrant({...grant,available_microcredits:9007199254740992},org));assert.throws(()=>requireQuotaGrant(grant,org,{grant_id:other}));});
test('BFF actual organization request preserves int64, key, CSRF and exact207; 202 is unknown',async()=>{
 const original=globalThis.fetch;let status=207;const requests=[];
 globalThis.fetch=async(url,options)=>{requests.push({url,options});return new Response(JSON.stringify({items:[{index:0,status:'SUCCEEDED',grant}]}),{status,headers:{'Content-Type':'application/json'}});};
 try{const transport=new AccountPlatformBffClient();transport.csrfToken='owned-session';const client=new OrganizationClient(transport);await client.allocate(org,facts,'owned-quota-operation-key');assert.match(requests[0].options.body,/"amount_microcredits":9007199254740993/);assert.equal(requests[0].options.headers['Idempotency-Key'],'owned-quota-operation-key');assert.equal(requests[0].options.headers['X-CSRF-Token'],'owned-session');status=202;await assert.rejects(client.allocate(org,facts,'owned-quota-operation-key'),error=>error.code==='COMMAND_RESULT_UNKNOWN'&&error.status===undefined);}finally{globalThis.fetch=original;}
});
test('Desktop actual transport passes exact digits as strings and decodes raw response money',async()=>{
 const requests=[];const client=new OrganizationClient(new AccountPlatformDesktopClient({request:async input=>{requests.push(input);return {status:207,bodyText:JSON.stringify({items:[{index:0,status:'SUCCEEDED',grant}]})};}}));await client.allocate(org,facts,'owned-quota-operation-key');assert.equal(requests[0].body.items[0].amount_microcredits,'9007199254740993');assert.equal(requests[0].idempotencyKey,'owned-quota-operation-key');assert.equal(requests[0].resource,`/v1/organizations/${org}/quota-grants:batch`);
});
test('BFF login binding increments operation epoch only when session changes',async()=>{
 const original=globalThis.fetch;let token='login-A';globalThis.fetch=async()=>new Response(JSON.stringify({authenticated:true,csrf_token:token}),{status:200});
 try{const client=new AccountPlatformBffClient();await client.session();const epoch=client.financeSessionEpoch;await client.session();assert.equal(client.financeSessionEpoch,epoch);token='login-B';await client.session();assert.equal(client.financeSessionEpoch,epoch+1);}finally{globalThis.fetch=original;}
});
test('Desktop relogin and logout retire organization operation epochs',async()=>{const client=new AccountPlatformDesktopClient({login:async()=>({}),logout:async()=>null});await client.login('owned','fixture');assert.equal(client.financeSessionEpoch,1);await client.logout();assert.equal(client.financeSessionEpoch,2);});
test('organization balance rejects foreign or personal wallet and preserves source digits',async()=>{
 const client=new OrganizationClient({request:async()=>({wallet_id:grantID,owner_type:'ORGANIZATION',owner_id:org,posted_microcredits:'9007199254740993',reserved_microcredits:'0',available_microcredits:'9007199254740993'})});assert.equal((await client.balance(org)).available_microcredits,'9007199254740993');client.client.request=async()=>({wallet_id:grantID,owner_type:'PERSONAL',owner_id:org});await assert.rejects(client.balance(org));
});
test('role receipt cannot settle a different requested role or unit',async()=>{
 const value={binding_id:grantID,organization_id:org,user_id:user,role_code:'AUDITOR',unit_id:null,status:'ACTIVE'};const client=new OrganizationClient({request:async()=>value});await client.grantRole(org,{user_id:user,role_code:'AUDITOR'},'owned-role-operation-key');await assert.rejects(client.grantRole(org,{user_id:user,role_code:'ORG_ADMIN'},'owned-role-operation-key'));
});
test('organization permissions come from active org snapshot; personal permissions stay separate',()=>{const me={personal_permissions:['personal.billing.read'],platform_permissions:[],active_context:{permissions:['org.wallet.read']}};assert.equal(hasPermission(me,'org.wallet.read'),true);assert.equal(hasPermission(me,'org.quota.allocate'),false);assert.equal(hasPermission(me,'personal.billing.read'),true);});
test('all three languages independently contain the same organization flow labels',()=>{const keys=Object.keys(organizationCopy.zh).sort();for(const locale of ['zh','en','ja']){assert.deepEqual(Object.keys(organizationCopy[locale]).sort(),keys);for(const key of keys)assert.equal(typeof organizationCopy[locale][key],'string');}assert.notEqual(organizationCopy.ja.contextUnknown,organizationCopy.en.contextUnknown);});

test('invitation receipts require an explicit canonical organization, for both token and ID acceptance',async()=>{
 const member={user_id:user,membership_id:grantID,version:1,status:'ACTIVE',roles:['MEMBER'],unit_ids:[]};
 for(const orgValue of [undefined,null,'invalid']){const client=new OrganizationClient({request:async()=>({organization_id:orgValue,member})});await assert.rejects(client.acceptLink('owned-public-fixture-invitation'));await assert.rejects(client.acceptInvitation(other));}
 const client=new OrganizationClient({request:async()=>({organization_id:org,member})});assert.equal((await client.acceptLink('owned-public-fixture-invitation')).organization_id,org);assert.equal((await client.acceptInvitation(other)).organization_id,org);
});

test('organization payment selection uses current capability and product availability without banning controlled test mode',()=>{
 const product={channels:['ALIPAY','WECHAT'],availability_status:'AVAILABLE'};
 assert.deepEqual(organizationPaymentChannels(product,{channels:['WECHAT'],test_only:false,formal_collection_admitted:true}),['WECHAT']);
 assert.deepEqual(organizationPaymentChannels({...product,availability_status:'PRODUCT_PAUSED'},{channels:product.channels}),[]);
 assert.deepEqual(organizationPaymentChannels(product,{channels:['ALIPAY'],test_only:true,formal_collection_admitted:false}),['ALIPAY']);
 assert.deepEqual(organizationPaymentChannels(product,null),[]);
});
test('organization capabilities reject missing or malformed source facts',async()=>{
 for(const value of [{},{channels:['ALIPAY'],test_only:true},{channels:['ALIPAY'],test_only:true,formal_collection_admitted:'false'}]){const client=new OrganizationClient({request:async()=>value});await assert.rejects(client.capabilities(org));}
 const value={channels:['ALIPAY'],test_only:true,formal_collection_admitted:false};assert.equal((await new OrganizationClient({request:async()=>value}).capabilities(org)).test_only,true);
});
