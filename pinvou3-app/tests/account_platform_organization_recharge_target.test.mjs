import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import vm from 'node:vm';
import { walletRechargeTarget } from '../src/features/conversation/billing-notice.js';
import { organizationRechargeTargetAllowed, requirePersonalAccount } from '../src/features/account-platform/personal-contract.js';
const org = '0199a100-0001-7000-8000-000000000201';
const user = '0199a100-0001-7000-8000-000000000202';
const notice = {kind:'wallet_insufficient',errorCode:'WALLET_INSUFFICIENT',contextType:'ORGANIZATION',action:'OPEN_ORGANIZATION_RECHARGE',organizationId:org};
const profile = () => ({user_id:user,status:'ACTIVE',personal_permissions:[],platform_permissions:[],active_context:{context_type:'ORGANIZATION',organization_id:org,context_version:1,membership_version:1,organization_authorization_version:1,permissions:['org.entitlements.purchase']}});
function host() {
 const calls=[], seq={current:0}; const client={financeSessionEpoch:0,versionObservationEpoch:0,contextVersion:1,authorizationVersions:{},session:async()=>({authenticated:true}),bootstrap:async()=>profile()};
 const context={walletRechargeTarget,organizationRechargeTargetAllowed,requirePersonalAccount,personalAccountAvailable:true,personalAccountClient:client,accountRechargeSequence:seq,setAccountRechargeRequest:r=>calls.push(['request',r]),setPersonalAccountOpen:v=>calls.push(['open',v]),setSettingsToast:v=>calls.push(['error',v]),organizationCopy:{en:{permissions:'Denied'}},language:'en'};
 const source=readFileSync(new URL('../src/app/main.jsx',import.meta.url),'utf8');
 const start=source.indexOf('onOpenAccountRecharge: async userError => {');
 assert.ok(start>=0); const end=source.indexOf('        onGotoTools:',start);
 const fn=vm.runInNewContext('('+source.slice(start,end).replace(/^onOpenAccountRecharge: /,'').trim().replace(/,$/,'')+')',context);
 return {calls,client,seq,invoke:fn};
}
test('organization target is bounded; historical notice never grants current permission',()=>{
 assert.deepEqual(walletRechargeTarget(notice),{contextType:'ORGANIZATION',organizationId:org,view:'organizationRecharge'});
 for(const changed of [{action:'CONTACT_ORG_ADMIN'},{errorCode:'MEMBER_QUOTA_INSUFFICIENT'},{errorCode:'UNRELATED'},{organizationId:'https://example.invalid'},{organizationId:'00000000-0000-0000-0000-000000000000'},{contextType:'PERSONAL'}]) assert.equal(walletRechargeTarget({...notice,...changed}),null);
 const target=walletRechargeTarget(notice);assert.equal(organizationRechargeTargetAllowed(profile(),target),true);
 for(const context of [{context_type:'PERSONAL'},{organization_id:user},{permissions:[]}]){const p=profile();Object.assign(p.active_context,context);assert.equal(organizationRechargeTargetAllowed(p,target),false);}
});
test('actual shared host opens organization only after authoritative scope check',async()=>{
 const h=host();await h.invoke(notice);assert.equal(h.calls[0][0],'request');assert.equal(h.calls[0][1].actor,user);assert.equal(h.calls[0][1].client,h.client);assert.deepEqual(h.calls[1],['open',true]);
 for(const alter of [p=>{p.active_context.permissions=[];},p=>{p.active_context.organization_id=user;},p=>{p.active_context.context_type='PERSONAL';delete p.active_context.organization_id;}]){const h=host();h.client.bootstrap=async()=>{const p=profile();alter(p);return p;};await h.invoke(notice);assert.deepEqual(h.calls,[['error','Denied']]);}
 const guest=host();guest.client.session=async()=>({authenticated:false});await guest.invoke(notice);assert.deepEqual(guest.calls,[['error','Denied']]);
});
test('actual host rejects late actor/context reads and cancelled requests',async()=>{
 for(const change of [h=>h.client.financeSessionEpoch++,h=>h.client.versionObservationEpoch++,h=>h.client.contextVersion++,h=>{h.client.authorizationVersions.organization_authorization_version=2;},h=>h.seq.current++]){
  const h=host();let release,entered;const started=new Promise(r=>{entered=r;});h.client.bootstrap=()=>new Promise(r=>{release=r;entered();});const task=h.invoke(notice);await started;change(h);release(profile());await task;assert.equal(h.calls.some(c=>c[0]==='open'),false);
 }
});
test('personal target preserves original immediate host behavior',async()=>{const h=host();h.client.bootstrap=()=>{throw Error('must not read for personal');};await h.invoke({kind:'wallet_insufficient',contextType:'PERSONAL',action:'OPEN_RECHARGE'});assert.deepEqual(h.calls,[['request',null],['open',true]]);});

test('first authenticated session observation may initialize the existing observation epoch',async()=>{const h=host();h.client.session=async()=>{h.client.versionObservationEpoch++;return {authenticated:true};};await h.invoke(notice);assert.equal(h.calls.some(c=>c[0]==='open'),true);});
