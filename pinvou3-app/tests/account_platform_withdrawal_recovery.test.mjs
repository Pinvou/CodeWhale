import assert from 'node:assert/strict';
import test from 'node:test';
import { readFile } from 'node:fs/promises';
import { AdminPlatformClient } from '../src/features/account-platform/admin-client.js';

const id='11111111-1111-4111-8111-111111111111';
const detail=Object.freeze({approval_request_id:id,version:4,action_control_version:1});
const capability=version=>({control:{version,epoch:1,authorization_relay_epoch:1},session_approval_wire_version:2});
const receipt=(version=1)=>({approval_request_id:id,command_protocol:1,operation:'WITHDRAW',expected_version:4,result_version:5,
  origin_action_control_version:1,action_control_version:version,receipt_event_id:'22222222-2222-4222-8222-222222222222',status:'WITHDRAWN',recorded_at:'2026-10-25T00:00:00Z'});
async function fixture(action){
 const original=globalThis.fetch;const client=new AdminPlatformClient('');client.csrfToken='controlled-csrf';const calls=[];
 try{await action(client,calls,(handler)=>{globalThis.fetch=async(url,options={})=>{calls.push({url:String(url),...options});return handler(String(url),options);};});}finally{globalThis.fetch=original;}
}
const json=(body,status=200)=>new Response(JSON.stringify(body),{status});

test('normal withdrawal sends the explicit protocol and the original CAS/control without a confirmation write',()=>fixture(async(c,calls,wire)=>{
 wire((url,o)=>url.endsWith('/capability')?json(capability(1)):new Response(null,{status:204}));
 await c.withdrawGovernedApprovalRequest(id,detail,undefined,1);
 const post=calls.filter(x=>x.method==='POST');assert.equal(post.length,1);assert.deepEqual(JSON.parse(post[0].body),{expected_version:4,command_protocol:1});assert.equal(post[0].headers['X-Pinvou-Approval-Action-Control-Version'],'1');assert.equal(calls.filter(x=>x.url.includes('withdraw-result')).length,0);
}));
test('lost acknowledgement is confirmed only by the exact original immutable receipt',()=>fixture(async(c,calls,wire)=>{
 wire((url,o)=>{if(url.endsWith('/capability'))return json(capability(1));if(o.method==='POST')throw new TypeError('lost committed acknowledgement');return json(receipt());});
 await c.withdrawGovernedApprovalRequest(id,detail,undefined,1);
 assert.equal(calls.filter(x=>x.method==='POST').length,1);const read=calls.find(x=>x.url.includes('withdraw-result'));assert.equal(read.method,undefined);const q=new URL(read.url,'http://controlled.invalid').searchParams;assert.equal(q.get('expected_version'),'4');assert.equal(q.get('origin_action_control_version'),'1');
}));
test('a control change after commit confirms by current read authority while preserving original write facts',()=>fixture(async(c,calls,wire)=>{
 let version=1;wire((url,o)=>{if(url.endsWith('/capability'))return json(capability(version));if(o.method==='POST'){version=2;return json({code:'APPROVAL_ACTION_CONTROL_CHANGED'},409);}return json(receipt(2));});
 await c.withdrawGovernedApprovalRequest(id,detail,undefined,1);
 const post=calls.filter(x=>x.method==='POST');assert.equal(post.length,1);assert.equal(post[0].headers['X-Pinvou-Approval-Action-Control-Version'],'1');assert.equal(JSON.parse(post[0].body).expected_version,4);const get=calls.find(x=>x.url.includes('withdraw-result'));assert.equal(get.headers['X-Pinvou-Approval-Action-Control-Version'],'2');assert.match(get.url,/origin_action_control_version=1/);
}));
test('stale write is never resent with a newer control or expected version',()=>fixture(async(c,calls,wire)=>{
 wire(url=>url.endsWith('/capability')?json(capability(2)):json(receipt(2)));
 await c.withdrawGovernedApprovalRequest(id,detail,undefined,1);assert.equal(calls.filter(x=>x.method==='POST').length,0);assert.equal(calls.filter(x=>x.url.includes('withdraw-result')).length,1);
}));
test('missing, forbidden and nonmatching receipts retain uncertainty without fallback writes',async()=>{
 for(const [body,status]of[[{},404],[{},403],[{status:'WITHDRAWN'},200],[{...receipt(),expected_version:3},200],[{...receipt(),origin_action_control_version:2},200],[{...receipt(),receipt_event_id:'invalid'},200],[{...receipt(),private_command:{}},200]]){
  await fixture(async(c,calls,wire)=>{wire((url,o)=>{if(url.endsWith('/capability'))return json(capability(1));if(o.method==='POST')throw new TypeError('lost ACK');return json(body,status);});await assert.rejects(c.withdrawGovernedApprovalRequest(id,detail,undefined,1),e=>e.financeRequestState==='WRITE_OBSERVED');assert.equal(calls.filter(x=>x.method==='POST').length,1);});
 }
});
test('retired session, csrf or authorization generation cannot launch a confirmation in the next identity',async()=>{
 for(const retire of[c=>c.retireSession(),c=>{c.csrfToken='new-csrf';},c=>{c.authorizationVersions={platform_authorization_version:2};}])await fixture(async(c,calls,wire)=>{
  wire((url,o)=>{if(url.endsWith('/capability'))return json(capability(1));if(o.method==='POST'){retire(c);throw new TypeError('late old identity');}throw new Error('retired read');});await assert.rejects(c.withdrawGovernedApprovalRequest(id,detail,undefined,1));assert.equal(calls.filter(x=>x.url.includes('withdraw-result')).length,0);
 });
});
test('missing legacy protocol and pre-dispatch cancellation perform no requests',()=>fixture(async(c,calls,wire)=>{
 wire(()=>{throw new Error('unexpected network');});await assert.rejects(c.withdrawGovernedApprovalRequest(id,detail),e=>e.financeRequestState==='NOT_DISPATCHED');const abort=new AbortController();abort.abort();await assert.rejects(c.withdrawGovernedApprovalRequest(id,detail,abort.signal,1),e=>e.financeRequestState==='NOT_DISPATCHED');assert.equal(calls.length,0);
}));
test('actual approval fresh-facts expression marks WITHDRAW alone and retains no protocol for other actions',async()=>{
 const text=await readFile(new URL('../src/features/account-platform/AdminFinanceWorkspace.jsx',import.meta.url),'utf8');const line=text.split('\n').find(x=>x.includes('facts = { detail, action, reason, draft,'));assert.ok(line);const fresh=new Function('action','detail','reason','draft',`let facts;${line};return facts;`);
 assert.equal(fresh('WITHDRAW',detail,'',null).commandProtocol,1);for(const action of['SUBMIT','APPROVE','REJECT','EDIT','query','claim','resolve'])assert.equal(fresh(action,detail,'',null).commandProtocol,undefined);
 assert.ok(text.includes('let facts = operation.pendingFacts;'));assert.ok(text.includes('if (!facts) {'));assert.ok(text.includes('undefined, facts.commandProtocol'));
});
