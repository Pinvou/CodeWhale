import test from 'node:test';
import assert from 'node:assert/strict';
import { OrganizationClient, OrganizationAdminClient, organizationUnitIDs } from '../src/features/account-platform/organization-client.js';
import { AccountPlatformBffClient } from '../src/features/account-platform/bff-client.js';
const org='11111111-1111-4111-8111-111111111111',user='22222222-2222-4222-8222-222222222222',unit='33333333-3333-4333-8333-333333333333',other='44444444-4444-4444-8444-444444444444';
const ack=()=>({organization_id:org,member:{user_id:user,membership_id:other,status:'ACTIVE',version:8,roles:['MEMBER'],unit_ids:[unit]},replayed:false});
test('ordinary unit IDs are explicit, canonical and separate from role scope',()=>{
 assert.deepEqual(organizationUnitIDs([other,unit,unit]),[unit,other]);assert.deepEqual(organizationUnitIDs([]),[]);
 for(const value of [null,undefined,'unit',[null],['not-a-uuid']])assert.throws(()=>organizationUnitIDs(value));
});
test('actual organization client writes exact membership CAS and durable key to units PUT',async()=>{
 const calls=[];const c=new OrganizationClient({request:async(...args)=>{calls.push(args);return ack();}});
 await c.replaceUnits(org,user,[unit,unit],7,'member-units-operation-key');
 assert.equal(calls[0][0],'account');assert.equal(calls[0][1],`/v1/organizations/${org}/members/${user}/units`);
 assert.deepEqual(calls[0][2].body,{expected_version:7,unit_ids:[unit]});assert.equal(calls[0][2].method,'PUT');assert.equal(calls[0][2].headers['Idempotency-Key'],'member-units-operation-key');
});
for(const [name,transform] of [
 ['foreign-org',a=>({...a,organization_id:other})],['foreign-member',a=>({...a,member:{...a.member,user_id:other}})],
 ['missing-scope',a=>({...a,member:{...a.member,unit_ids:null}})],['different-scope',a=>({...a,member:{...a.member,unit_ids:[other]}})],
 ['stale-version',a=>({...a,member:{...a.member,version:7}})],['unexpected-new-version',a=>({...a,member:{...a.member,version:9}})],
 ['malformed-unit',a=>({...a,member:{...a.member,unit_ids:['invalid']}})],['revoked-member',a=>({...a,member:{...a.member,status:'REVOKED'}})],
])test(`member units ACK rejects ${name}`,async()=>{const c=new OrganizationClient({request:async()=>transform(ack())});await assert.rejects(c.replaceUnits(org,user,[unit],7,'member-units-operation-key'));});
test('authority-confirmed replay returns a later current member without inferring own commit by version',async()=>{
 const c=new OrganizationClient({request:async()=>({...ack(),replayed:true,member:{...ack().member,version:9,unit_ids:[]}})});
 assert.equal((await c.replaceUnits(org,user,[unit],7,'member-units-operation-key')).replayed,true);
});
for (const status of ['DISABLED', 'REVOKED']) test(`verified receipt replay preserves current ${status} projection`, async () => {
 const c = new OrganizationClient({request:async()=>({...ack(),replayed:true,member:{...ack().member,status,version:10,unit_ids:[]}})});
 const result = await c.replaceUnits(org,user,[unit],7,'member-units-operation-key');assert.equal(result.member.status,status);assert.equal(result.member.version,10);
});

test('explicit empty clears membership via same real client contract',async()=>{
 const c=new OrganizationClient({request:async()=>({...ack(),member:{...ack().member,unit_ids:[]}})});await c.replaceUnits(org,user,[],7,'member-units-operation-key');
});
test('actual BFF unknown202 successor retry preserves request, CSRF and key',async()=>{
 const old=globalThis.fetch,calls=[];let unknown=true;
 globalThis.fetch=async(url,options)=>{calls.push({url,options});return new Response(JSON.stringify(ack()),{status:unknown?202:200,headers:{'Content-Type':'application/json'}});};
 try {const transport=new AccountPlatformBffClient();transport.csrfToken='owned-session';const c=new OrganizationClient(transport);await assert.rejects(c.replaceUnits(org,user,[unit],7,'member-units-operation-key'),error=>error.code==='COMMAND_RESULT_UNKNOWN');unknown=false;await c.replaceUnits(org,user,[unit],7,'member-units-operation-key');assert.equal(calls.length,2);assert.equal(calls[0].options.body,calls[1].options.body);assert.equal(calls[0].options.headers['Idempotency-Key'],calls[1].options.headers['Idempotency-Key']);assert.equal(calls[0].options.headers['X-CSRF-Token'],'owned-session');}
 finally{globalThis.fetch=old;}
});
test('admin ordinary scope is a distinct actual PUT operation with exact receipt',async()=>{
 const calls=[];const c=new OrganizationAdminClient({mutate:async(...args)=>{calls.push(args);return ack();}});
 await c.write(org,'units',{unit_ids:[unit],expected_version:7},'member-units-operation-key',user);
 assert.equal(calls[0][1],`/admin/v1/organizations/${org}/members/${user}/units`);assert.equal(calls[0][2],'PUT');assert.equal(calls[0][4],'member-units-operation-key');assert.deepEqual(calls[0][3],{unit_ids:[unit],expected_version:7});
});
test('admin create sends independent role and ordinary unit arrays',async()=>{
 let sent;const body={user_id:user,roles:['MEMBER'],unit_admin_unit_ids:[other],unit_ids:[unit]};
 const c=new OrganizationAdminClient({mutate:async(...args)=>{sent=args[3];return ack();}});await c.write(org,'add',body,'member-units-operation-key');assert.deepEqual(sent,body);assert.notDeepEqual(sent.unit_ids,sent.unit_admin_unit_ids);
});
test('invitation ACK must bind explicitly frozen ordinary units',async()=>{
 const body={target_type:'LINK',roles:['MEMBER'],expires_in_hours:24,unit_ids:[unit]};const value={invitation_id:other,organization_id:org,status:'PENDING',unit_ids:[unit]};
 const c=new OrganizationClient({request:async()=>value});await c.invite(org,body,'member-units-operation-key');c.client.request=async()=>({...value,unit_ids:[other]});await assert.rejects(c.invite(org,body,'member-units-operation-key'));
 c.client.request=async()=>({...value,unit_ids:undefined});await c.invite(org,{...body,unit_ids:[]},'member-units-operation-key');await c.invite(org,{roles:['MEMBER']},'member-units-operation-key');
});
