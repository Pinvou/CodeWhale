import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import vm from 'node:vm';
import { fileURLToPath } from 'node:url';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const bridgeRoot = path.join(root, 'src', 'platform', 'tauri', 'bridge');

function loadFactories() {
  const windowObject = {};
  const context = vm.createContext({ window: windowObject, console, setTimeout, clearTimeout });
  for (const file of ['settings.js', 'chat.js']) {
    vm.runInContext(fs.readFileSync(path.join(bridgeRoot, file), 'utf8'), context, { filename: file });
  }
  return { factories: windowObject.__PINVOU_TAURI_BRIDGE_FEATURES__, windowObject };
}

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}


// The retired LLM API account no longer owns model authority. Exercise the
// replacement settings transport, preserving failure and stale-response coverage.
function baseState() { return { activeSessionId: 'session-1', savedModels: [], currentSessionModelRef: {source:'LOCAL_CONFIG',saved_model_id:'local'}, effectiveModelConfig:{model:'local'} }; }
const { factories } = loadFactories();
function settings(state, invoke) { return factories.settings({state, invoke, notify(){}, listen(){}}); }
{
 const state=baseState(), calls=[];
 const domain=settings(state, async (command,args)=>{calls.push([command,args]); if(command==='get_session_model_ref')return {source:'ACCOUNT_PLATFORM',model_id:'public'}; if(command==='get_effective_model_config')return {model:'public',requires_user_api_key:false}; return null;});
 await domain.switchModelRef('session-1',{source:'ACCOUNT_PLATFORM',model_id:'public'});
 assert.equal(calls[0][0],'account_platform_select_session_model');
 assert.equal(calls[0][1].input.sessionId,'session-1');
 assert.equal(state.currentSessionModelRef.model_id,'public');
 assert.equal(state.effectiveModelConfig.model,'public');
 assert.equal(calls.some(([command])=>command.startsWith('get_llmapi')),false);
}
{
 const state=baseState(), previous=state.currentSessionModelRef;
 const domain=settings(state, async ()=>{throw new Error('PERMISSION_DENIED');});
 await assert.rejects(domain.switchModelRef('session-1',{source:'ACCOUNT_PLATFORM',model_id:'public'}),/PERMISSION_DENIED/);
 assert.equal(state.currentSessionModelRef,previous);
 assert.equal(state.effectiveModelConfig.model,'local');
}
{
 const state=baseState(), request=deferred();
 const domain=settings(state, command=>command==='get_effective_model_config'?request.promise:Promise.resolve(null));
 const pending=domain.loadSessionModel('session-1'); state.activeSessionId='session-2';
 request.resolve({model:'stale'}); assert.equal(await pending,false);
 assert.equal(state.effectiveModelConfig.model,'local');
}
{
 const state=baseState(), request=deferred(); let n=0;
 const domain=settings(state, command=>command==='get_effective_model_config'?(++n===1?request.promise:Promise.resolve({model:'new'})):Promise.resolve(null));
 const old=domain.loadSessionModel('session-1'); await domain.loadSessionModel('session-1');
 request.resolve({model:'old'}); await old; assert.equal(state.effectiveModelConfig.model,'new');
}
{
 const state=baseState();
 const domain=settings(state, async command=>{if(command==='get_effective_model_config')throw new Error('temporary'); return null;});
 assert.equal(await domain.loadSessionModel('session-1'),false);
 assert.equal(state.effectiveModelConfig.model,'local','transient refresh does not invent a missing model');
}
{
 const state=baseState(), calls=[];
 const domain=settings(state, async (command,args)=>{calls.push([command,args]);return null;});
 await domain.switchModelRef(null,{source:'ACCOUNT_PLATFORM',model_id:'public'});
 assert.equal(calls.length,0,'draft selection is deferred until the real session is materialized');
 assert.equal(state.pendingDraftModelRef.model_id,'public');
 await assert.rejects(domain.switchModelRef('session-1',{source:'UNKNOWN'}),/unsupported/);
 assert.equal(calls.length,0);
}
{
 const state=baseState(), requests=[deferred(),deferred()]; let n=0;
 const domain=settings(state, command=>command==='list_models'?requests[n++].promise:Promise.resolve(null));
 const old=domain.loadModels(), current=domain.loadModels();
 requests[1].resolve({models:[{id:'new'}]});await current;requests[0].resolve({models:[{id:'old'}]});await old;
 assert.equal(state.savedModels[0].id,'new','a late list cannot overwrite the current catalog');
}
console.log('account platform model bridge: 7 scenarios passed');
