import assert from 'node:assert/strict';
import test from 'node:test';
import {createHash} from 'node:crypto';
import {readFile} from 'node:fs/promises';
import {AdminPlatformClient} from '../src/features/account-platform/admin-client.js';
import {approvalDecisionReasonDigest,canonicalApprovalDecision} from '../src/features/account-platform/approval-decision-recovery.js';
const id='11111111-1111-4111-8111-111111111111';
const detail=Object.freeze({approval_request_id:id,version:4,action_control_version:1,critical_resource_versions:{version:3}});
const reason=' Controlled decision reason ';
const digest=createHash('sha256').update(reason,'utf8').digest('hex');
const capability=(version=1,wire=2)=>({control:{version,epoch:1,authorization_relay_epoch:1},session_approval_wire_version:wire});
const json=(body,status=200)=>new Response(JSON.stringify(body),{status});
const receipt=(decision='APPROVED',current=1)=>({approval_request_id:id,command_protocol:1,operation:'DECIDE',decision,expected_version:4,result_version:5,origin_action_control_version:1,action_control_version:current,reason_digest:digest,receipt_event_id:'22222222-2222-4222-8222-222222222222',decision_id:'33333333-3333-4333-8333-333333333333',recorded_at:'2026-10-25T00:00:00Z'});
async function fixture(fn){const original=globalThis.fetch;const client=new AdminPlatformClient('');client.csrfToken='controlled-csrf';const calls=[];try{await fn(client,calls,handler=>{globalThis.fetch=async(url,options={})=>{calls.push({url:String(url),...options});return handler(String(url),options);};});}finally{globalThis.fetch=original;}}

test('actual production client generates the closed wire accepted by the genuine Account HTTP contract',async t=>{
 const rows=JSON.parse(await readFile(new URL('../../account-platform/internal/account/testdata/approval-decision-wire.json',import.meta.url),'utf8'));
 for(const row of rows)await t.test(`v${row.wire}-${row.ui_action}`,()=>fixture(async(client,calls,wire)=>{
  wire(url=>url.endsWith('/capability')?json(capability(1,row.wire)):new Response(null,{status:204}));
  await client.decideGovernedApproval(id,detail,row.ui_action,row.body.reason,undefined,1);
  assert.equal(await approvalDecisionReasonDigest(row.body.reason),row.reason_digest);const posts=calls.filter(x=>x.method==='POST');assert.equal(posts.length,1);assert.deepEqual(JSON.parse(posts[0].body),row.body);assert.equal(posts[0].headers['X-Pinvou-Approval-Action-Control-Version'],'1');assert.equal(calls.filter(x=>x.url.includes('decision-result')).length,0);
 }));
});
test('wire reason digest preserves original UTF8 rather than copying server trimming',async()=>{
 for(const value of [reason,'\u0085Reason\u0085','\ufeffReason\ufeff','理由 "quoted" 😀'])assert.equal(await approvalDecisionReasonDigest(value),createHash('sha256').update(value,'utf8').digest('hex'));
 for(const value of ['unknown','toString','__proto__',null])assert.throws(()=>canonicalApprovalDecision(value),e=>e.financeRequestState==='NOT_DISPATCHED');
});
test('lost acknowledgement confirms exact original decision by a readonly receipt under current control',async()=>{
 for(const action of ['APPROVE','REJECT'])await fixture(async(client,calls,wire)=>{
  let current=1;const canonical=canonicalApprovalDecision(action);wire((url,options)=>{if(url.endsWith('/capability'))return json(capability(current));if(options.method==='POST'){current=2;return json({code:'APPROVAL_ACTION_CONTROL_CHANGED'},409);}return json(receipt(canonical,current));});
  await client.decideGovernedApproval(id,detail,action,reason,undefined,1);
  const posts=calls.filter(x=>x.method==='POST');assert.equal(posts.length,1);const body=JSON.parse(posts[0].body);assert.equal(body.expected_version,4);assert.equal(body.reason,reason);assert.equal(body.decision,canonical);assert.equal(body.action_control_version,1);
  const read=calls.find(x=>x.url.includes('decision-result'));assert.equal(read.method,undefined);const q=new URL(read.url,'http://controlled.invalid').searchParams;assert.equal(q.get('expected_version'),'4');assert.equal(q.get('origin_action_control_version'),'1');assert.equal(q.get('decision'),canonical);assert.equal(q.get('reason_digest'),digest);assert.equal(q.has('reason'),false);assert.equal(read.headers['X-Pinvou-Approval-Action-Control-Version'],'2');
 });
});
test('stale original write can only confirm; it never adopts new control or new CAS',()=>fixture(async(client,calls,wire)=>{
 wire(url=>url.endsWith('/capability')?json(capability(2)):json(receipt('APPROVED',2)));await client.decideGovernedApproval(id,detail,'APPROVE',reason,undefined,1);assert.equal(calls.filter(x=>x.method==='POST').length,0);assert.equal(calls.filter(x=>x.url.includes('decision-result')).length,1);
}));
test('missing, forbidden or mismatching evidence preserves unknown with no fallback write',async()=>{
 for(const [body,status]of [[{},404],[{},403],[{},503],[{decision:'APPROVED'},200],[{...receipt(),decision:'REJECTED'},200],[{...receipt(),expected_version:3},200],[{...receipt(),origin_action_control_version:2},200],[{...receipt(),reason_digest:'0'.repeat(64)},200],[{...receipt(),decision_id:'bad'},200],[{...receipt(),private_command:{}},200]])await fixture(async(client,calls,wire)=>{
  wire((url,options)=>{if(url.endsWith('/capability'))return json(capability());if(options.method==='POST')throw new TypeError('committed lost ACK');return json(body,status);});await assert.rejects(client.decideGovernedApproval(id,detail,'APPROVE',reason,undefined,1),e=>e.financeRequestState==='WRITE_OBSERVED');assert.equal(calls.filter(x=>x.method==='POST').length,1);
 });
});
test('legacy unresolved intent without protocol and pre-dispatch abort cannot be upgraded',()=>fixture(async(client,calls,wire)=>{
 wire(()=>{throw new Error('unexpected dispatch');});await assert.rejects(client.decideGovernedApproval(id,detail,'APPROVE',reason),e=>e.financeRequestState==='NOT_DISPATCHED');const controller=new AbortController();controller.abort();await assert.rejects(client.decideGovernedApproval(id,detail,'APPROVE',reason,controller.signal,1),e=>e.financeRequestState==='NOT_DISPATCHED');assert.equal(calls.length,0);
}));
test('retired session, csrf or authorization cannot confirm in a later identity',async()=>{
 for(const retire of [c=>c.retireSession(),c=>{c.csrfToken='new';},c=>{c.authorizationVersions={platform:2};}])await fixture(async(client,calls,wire)=>{
  wire((url,options)=>{if(url.endsWith('/capability'))return json(capability());if(options.method==='POST'){retire(client);throw new TypeError('late original response');}throw new Error('retired proof read');});await assert.rejects(client.decideGovernedApproval(id,detail,'APPROVE',reason,undefined,1));assert.equal(calls.filter(x=>x.url.includes('decision-result')).length,0);
 });
});
test('typed old-protocol rejection proves zero dispatch for this fresh attempt, without a write fallback',()=>fixture(async(client,calls,wire)=>{
 wire((url,options)=>url.endsWith('/capability')?json(capability()):options.method==='POST'?json({code:'INVALID_APPROVAL_DECISION'},400):json({},404));await assert.rejects(client.decideGovernedApproval(id,detail,'APPROVE',reason,undefined,1),e=>e.financeRequestState==='NOT_DISPATCHED');assert.equal(calls.filter(x=>x.method==='POST').length,1);
}));
test('actual fresh facts mark only implemented decision actions and preserve original pending facts',async()=>{
 const source=await readFile(new URL('../src/features/account-platform/AdminFinanceWorkspace.jsx',import.meta.url),'utf8');const line=source.split('\n').find(x=>x.includes('facts = { detail, action, reason, draft,'));assert.ok(line);const fresh=new Function('action','detail','reason','draft',`let facts;${line};return facts;`);for(const action of ['WITHDRAW','APPROVE','REJECT'])assert.equal(fresh(action,detail,reason,null).commandProtocol,1);for(const action of ['SUBMIT','query','claim','resolve'])assert.equal(fresh(action,detail,reason,null).commandProtocol,undefined);assert.ok(source.includes('let facts = operation.pendingFacts;'));assert.ok(source.includes('if (!facts) {'));assert.ok(source.includes('facts.reason, undefined, facts.commandProtocol'));
});


test('late proof response cannot clear an operation retired by identity or live-control replacement', async () => {
  for (const retire of [c => c.retireSession(), c => { c.csrfToken = 'replacement'; },
    c => { c.authorizationVersions = { platform: 9 }; }, c => { c.approvalControl = capability(3); }]) {
    await fixture(async (client, calls, wire) => {
      let complete;
      wire((url, options) => {
        if (url.endsWith('/capability')) return json(capability());
        if (options.method === 'POST') throw new TypeError('committed without acknowledgement');
        return new Promise(resolve => { complete = resolve; });
      });
      const pending = client.decideGovernedApproval(id, detail, 'APPROVE', reason, undefined, 1);
      while (!complete) await new Promise(resolve => setTimeout(resolve, 0));
      retire(client);
      const replacement = client.approvalControl;
      complete(json(receipt()));
      await assert.rejects(pending, error => error.financeRequestState === 'WRITE_OBSERVED');
      assert.equal(client.approvalControl, replacement);
      assert.equal(calls.filter(call => call.method === 'POST').length, 1);
    });
  }
});

test('concurrent newer control preserves original unknown; only explicit retry confirms with a readonly GET', () => fixture(async (client, calls, wire) => {
  let complete;
  let current = 1;
  wire((url, options) => {
    if (url.endsWith('/capability')) return json(capability(current));
    if (options.method === 'POST') return new Promise(resolve => { complete = resolve; });
    return json(receipt('APPROVED', current));
  });
  const original = client.decideGovernedApproval(id, detail, 'APPROVE', reason, undefined, 1);
  while (!complete) await new Promise(resolve => setTimeout(resolve, 0));
  current = 2;
  client.approvalControl = capability(current);
  const live = client.approvalControl;
  const reader = new AbortController();
  client.approvalControllers.add(reader);
  complete(json({ code: 'APPROVAL_ACTION_CONTROL_CHANGED' }, 409));
  await assert.rejects(original, error => error.financeRequestState === 'WRITE_OBSERVED');
  assert.equal(client.approvalControl, live);
  assert.equal(reader.signal.aborted, false);
  assert.equal(calls.filter(call => call.url.includes('decision-result')).length, 0);
  await client.decideGovernedApproval(id, detail, 'APPROVE', reason, undefined, 1);
  assert.equal(calls.filter(call => call.method === 'POST').length, 1);
  const proof = calls.find(call => call.url.includes('decision-result'));
  assert.equal(new URL(proof.url, 'http://controlled.invalid').searchParams.get('origin_action_control_version'), '1');
  assert.equal(proof.headers['X-Pinvou-Approval-Action-Control-Version'], '2');
}));
