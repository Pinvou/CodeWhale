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
  // Exercise the real shared read ordering and catalog loader used by settings.
  vm.runInContext(fs.readFileSync(path.join(root, 'src', 'shared', 'bridge-shared-helpers.js'), 'utf8'), context, {
    filename: 'bridge-shared-helpers.js',
  });
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
{
 const state=baseState(), oldRequests=new Map(), calls=new Map(); let notifications=0;
 const commands=['get_session_model_id','get_session_model_ref','get_effective_model_config'];
 for(const command of commands) oldRequests.set(command,deferred());
 const domain=factories.settings({state, notify(){notifications++;}, listen(){}, invoke(command){
   const n=(calls.get(command)||0)+1; calls.set(command,n);
   if(n===1)return oldRequests.get(command).promise;
   if(command==='get_session_model_id')return Promise.resolve(null);
   if(command==='get_session_model_ref')return Promise.resolve({source:'ACCOUNT_PLATFORM',model_id:'selected'});
   return Promise.resolve({model:'selected'});
 }});
 const stale=domain.loadSessionModel('session-1');
 await domain.loadSessionModel('session-1');
 const published=notifications;
 oldRequests.get('get_session_model_id').resolve('stale-local');
 oldRequests.get('get_session_model_ref').resolve({source:'LOCAL_CONFIG',saved_model_id:'stale-local'});
 oldRequests.get('get_effective_model_config').resolve({model:'stale-local'});
 assert.equal(await stale,false);
 assert.equal(state.currentSessionModelId,null);
 assert.equal(state.currentSessionModelRef.source,'ACCOUNT_PLATFORM');
 assert.equal(state.currentSessionModelRef.model_id,'selected');
 assert.equal(state.effectiveModelConfig.model,'selected');
 assert.equal(notifications,published,'stale tuple must not publish a contradictory selection');
}
// The store's card ownership check and device endpoint snapshot check coexist.
// Exercise both through the actual settings factory: their post-delete refresh
// must take the backend's cleared default literally, without choosing the next
// remaining model or touching an independent session/draft selection.
for (const operation of [
  { method: 'deleteModelIfStoreLink', command: 'delete_model_if_store_link', args: ['saved-local', 'card-local'] },
  { method: 'deleteModelIfMatches', command: 'delete_model_if_matches', args: ['saved-local', 'http://127.0.0.1:8000/v1', 'qwen3'] },
]) {
  for (const deleted of [true, false]) {
    const state = baseState();
    state.pendingDraftModelRef = { source: 'ACCOUNT_PLATFORM', model_id: 'draft-choice' };
    state.currentSessionModelRef = { source: 'ACCOUNT_PLATFORM', model_id: 'session-choice' };
    state.effectiveModelConfig = { model: 'session-choice' };
    state.defaultModelRef = { source: 'LOCAL_CONFIG', saved_model_id: 'saved-local' };
    state.activeModelId = 'saved-local';
    const draft = state.pendingDraftModelRef;
    const session = state.currentSessionModelRef;
    const calls = [];
    const domain = settings(state, async (command, args) => {
      calls.push([command, args]);
      if (command === operation.command) return deleted;
      if (command === 'list_models') return {
        models: deleted ? [{ id: 'other-local' }] : [{ id: 'saved-local' }, { id: 'other-local' }],
        active_model_id: deleted ? null : 'saved-local',
        default_model_ref: deleted ? null : { source: 'LOCAL_CONFIG', saved_model_id: 'saved-local' },
      };
      if (command === 'get_settings') return { advanced: { active_model_id: deleted ? null : 'saved-local' } };
      if (command === 'get_effective_model_config') return { model: 'session-choice' };
      throw new Error(`unexpected command: ${command}`);
    });
    assert.equal(await domain[operation.method](...operation.args), deleted, operation.method);
    assert.equal(state.activeModelId, deleted ? null : 'saved-local', 'a cleared default must not auto-select a remaining model');
    assert.equal(state.defaultModelRef?.saved_model_id || null, deleted ? null : 'saved-local');
    assert.equal(state.pendingDraftModelRef, draft, 'refresh preserves the independently selected draft model');
    assert.equal(state.currentSessionModelRef, session, 'deleting a new-chat default does not retarget an existing session');
    assert.equal(state.effectiveModelConfig.model, 'session-choice');
    assert.equal(state.savedModels.some(row => row.id === 'other-local'), true);
    assert.equal(calls.filter(([command]) => command === operation.command).length, 1);
    assert.equal(calls.some(([command]) => command === 'set_active_model' || command === 'set_session_model'), false);
    assert.equal(calls.some(([command]) => command.startsWith('delete_model') && command !== operation.command), false,
      'one identity authority must not fall back to the other delete command');
  }
  {
    const state = baseState();
    state.savedModels = [{ id: 'preserved' }];
    state.activeModelId = 'preserved';
    state.defaultModelRef = { source: 'LOCAL_CONFIG', saved_model_id: 'preserved' };
    const previousModels = state.savedModels;
    const previousDefault = state.defaultModelRef;
    const previousEffective = state.effectiveModelConfig;
    const domain = settings(state, async command => {
      if (command === operation.command) return false;
      if (command === 'list_models' || command === 'get_effective_model_config') throw new Error('temporary transport failure');
      if (command === 'get_settings') return { language: 'en' };
      throw new Error(`unexpected command: ${command}`);
    });
    assert.equal(await domain[operation.method](...operation.args), false);
    assert.equal(state.savedModels, previousModels, 'failed read-back is not an authoritative empty catalog');
    assert.equal(state.defaultModelRef, previousDefault, 'failed read-back does not invent an empty typed default');
    assert.equal(state.activeModelId, 'preserved');
    assert.equal(state.effectiveModelConfig, previousEffective, 'failed effective read cannot publish a partial replacement');
  }
  {
    const state = baseState();
    const previous = JSON.stringify(state);
    const calls = [];
    const domain = settings(state, async command => { calls.push(command); throw new Error('identity check unavailable'); });
    await assert.rejects(domain[operation.method](...operation.args), /identity check unavailable/);
    assert.equal(JSON.stringify(state), previous, 'failed mutation cannot publish a success snapshot');
    assert.deepEqual(calls, [operation.command], 'failed mutation cannot fall back to unconditional deletion');
  }
}

console.log('settings model transport regressions passed');
