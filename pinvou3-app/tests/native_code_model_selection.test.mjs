import assert from 'node:assert/strict';
import test from 'node:test';
import fs from 'node:fs';
import { resolveNativeModelRef, persistNativeSessionModelRef, claimNativeControlsRefreshId, canApplyNativeControlsRefresh, finalizePreparedSessionCreation } from '../src/features/codex/native-session-handoff.js';
const platform = { source: 'ACCOUNT_PLATFORM', model_id: 'public-a' };
const local = { source: 'LOCAL_CONFIG', saved_model_id: 'local-a' };
test('Code reference is bound to its current controls owner', () => {
  assert.equal(resolveNativeModelRef({ activeId: 'a', controlsSessionId: 'a', controlsModelRef: platform }), platform);
  assert.equal(resolveNativeModelRef({ activeId: 'b', controlsSessionId: 'a', controlsModelRef: platform, draftModelRef: local }), null);
});
test('first session handoff and draft retain their own reference', () => {
  assert.equal(resolveNativeModelRef({ activeId: 'a', controlsSessionId: null, handoffModelRef: platform }), platform);
  assert.equal(resolveNativeModelRef({ activeId: null, controlsModelRef: platform, draftModelRef: local }), local);
});
test('platform selection uses catalog authority command with explicit Code session', async () => {
  const calls=[]; await persistNativeSessionModelRef(async (...args)=>calls.push(args), 'code-a', platform);
  assert.deepEqual(calls, [['account_platform_select_session_model', { input: { sessionId: 'code-a', modelId: 'public-a' } }]]);
});
test('local selection preserves existing host command', async () => {
  const calls=[]; await persistNativeSessionModelRef(async (...args)=>calls.push(args), 'code-a', local);
  assert.deepEqual(calls, [['set_session_model', {sessionId:'code-a',modelId:'local-a'}]]);
});
test('unknown and incomplete references never dispatch', async () => {
  const calls=[]; const invoke=async (...args)=>calls.push(args);
  for (const ref of [null, {}, {source:'UNKNOWN', model_id:'public-a'}, {source:'ACCOUNT_PLATFORM',model_id:''}, {source:'LOCAL_CONFIG',saved_model_id:''}]) await assert.rejects(persistNativeSessionModelRef(invoke,'a',ref));
  await assert.rejects(persistNativeSessionModelRef(invoke,null,platform)); assert.deepEqual(calls,[]);
});
test('host rejection remains an error and cannot silently switch to local carrier', async () => {
  const calls=[];const error=new Error('controlled failure');
  await assert.rejects(persistNativeSessionModelRef(async (...args)=>{calls.push(args);throw error;},'a',platform),err=>err===error);assert.equal(calls.length,1);
});
test('stale response and stale request do not supersede current controls', () => {
  assert.deepEqual(claimNativeControlsRefreshId({sessionId:'a',activeId:'b',latestRequestId:2}),{requestId:0,latestRequestId:2});
  assert.equal(canApplyNativeControlsRefresh({requestId:2,latestRequestId:3,sessionId:'a',activeId:'a'}),false);
  assert.equal(canApplyNativeControlsRefresh({requestId:3,latestRequestId:3,sessionId:'a',activeId:'b'}),false);
  assert.equal(canApplyNativeControlsRefresh({requestId:3,latestRequestId:3,sessionId:'b',activeId:'b'}),true);
});
test('draft platform reference is persisted before activation and load', async () => {
  const calls=[];await finalizePreparedSessionCreation({sessionId:'new',prepareSession:async id=>persistNativeSessionModelRef(async command=>calls.push(command),id,platform),shouldActivate:()=>true,activateSession:()=>calls.push('activate'),loadSession:async()=>calls.push('load')});
  assert.deepEqual(calls,['account_platform_select_session_model','activate','load']);
});
test('failed preparation leaves created session available for retry and reports error', async () => {
  const error=new Error('controlled failure');const calls=[];const result=await finalizePreparedSessionCreation({sessionId:'new',prepareSession:async()=>{throw error;},shouldActivate:()=>true,activateSession:()=>calls.push('activate'),loadSession:async()=>calls.push('load')});
  assert.equal(result.preparationError,error);assert.deepEqual(calls,['activate','load']);
});
test('main and detached Code hosts pass real account client and Code selector reads own ref', () => {
  const source=relative=>fs.readFileSync(new URL(relative,import.meta.url),'utf8');
  const codex=source('../src/features/codex/CodexAcpView.jsx');
  assert.match(codex,/invoke\('get_session_model_ref', \{ sessionId \}\)/);
  assert.match(codex,/sessionModelRef=\{nativeSessionModelRef\}/);assert.match(codex,/onSwitchModelRef=\{switchNativeModelRef\}/);
  assert.match(codex,/accountPlatformClient=\{accountPlatformClient\}/);
  for(const file of ['../src/app/main.jsx','../src/app/DetachedShell.jsx'])assert.match(source(file),/accountPlatformClient=\{personalAccountClient\}/);
  assert.doesNotMatch(codex,/invoke\('set_session_model_ref'/);
});
