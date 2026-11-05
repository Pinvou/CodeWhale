import React, { useEffect, useState } from 'react';
import { createRoot } from 'react-dom/client';
import '../../src/styles/base.css';
import '../../src/styles/tailwind.css';
import { dictZh } from '../../src/shared/i18n/zh.js';

// Test host: mount full ChatView and real desktop JS bridge; mock native IPC only.
const scenario = new URLSearchParams(location.search).get('scenario') || 'offline';
const id = 'remote-offline-smoke-' + scenario;
const handle = { taskId: 'task-never-' + scenario, clientRequestId: 'client-never-' + scenario,
  requestFingerprint: 'a'.repeat(64), dispatchState: 'prepared_never_attempted' };
const originalHistory = { localSessionId:id, archived:false, title:'Offline history probe',
  pinned:false,lastOpenedAt:null,entries:[],authenticatedCursor:null,rejections:[],
  pendingUnknownCount:scenario === 'unknown' ? 1 : 0,
  unattemptedPending:scenario === 'unknown' ? [] : [handle] };
const probe = window.offlineAbandonFixture = {
  scenario,id,handle,calls:[],unexpected:[],ready:false,pendingNative:null,
  durableHistory:structuredClone(originalHistory),copy:dictZh.remoteConversation,
  resolveAbandon() {
    if (!this.pendingNative) throw new Error('no_pending_native');
    const { resolve } = this.pendingNative; this.pendingNative = null;
    this.durableHistory = { ...this.durableHistory,unattemptedPending:[] };
    resolve(null);
  },
  rejectAbandon() {
    if (!this.pendingNative) throw new Error('no_pending_native');
    const { reject } = this.pendingNative; this.pendingNative = null;
    reject('remote_pending_abandon_failed');
  },
};
let renderSnapshot = () => {};
const hostState = { activeSessionId:id, chatItems:[], sessions:[], savedModels:[],
  attachments:[],queued:[],artifacts:[],busy:false,modeState:{mode:'yolo'},
  composerDraft:'',settings:{language:'zh-Hans'},voiceInput:{status:'idle'},
  voiceAsrSetup:{open:false},draftEpoch:0,tokens:{input:0,max:0},
  desktopRemoteControl:window.__PINVOU_REMOTE_CONTROL_STATE__.createState() };
const listeners = new Set();
const publish = () => { probe.publishedHistory=structuredClone(hostState.remoteProxyHistory);renderSnapshot({ ...hostState }); for (const listener of listeners) listener({ ...hostState }); };
function unexpected(name,args) { probe.unexpected.push({name,args}); throw new Error('unexpected_mock_call:' + name); }
function nativeInvoke(name,args) {
  probe.calls.push({boundary:'native-invoke-mock',name,args:structuredClone(args || {})});
  if (name === 'desktop_remote_controller_status') return Promise.resolve({status:'idle'});
  if (name === 'desktop_remote_proxy_history_read') {
    if (args.localSessionId !== id) return unexpected(name,args);
    return Promise.resolve(structuredClone(probe.durableHistory));
  }
  if (name === 'desktop_remote_task_abandon_prepared_a') {
    if (probe.pendingNative) return unexpected('duplicate_native_abandon',args);
    return new Promise((resolve,reject) => { probe.pendingNative={resolve,reject}; });
  }
  return unexpected(name,args);
}
const remote = window.__PINVOU_TAURI_BRIDGE_FEATURES__['desktop-remote-control']({
  state:hostState,notify:publish,invoke:nativeInvoke,
  can:name => name === 'desktopRemoteControl',listen:() => Promise.resolve(() => {}),
});
window.PinvouPlatform = {kind:'desktop',isWeb:false,
  capabilities:{computerUse:false,localModelSetup:false,dependencyInstall:false} };
window.TauriBridge = {
  available:true,desktopRemoteControl:remote,
  state:{
    getMany:() => ({...hostState,platformCapabilities:{loaded:true,os:'windows',desktopRemoteControl:true}}),
    get:() => ({...hostState}),
    subscribeMany:(domains,listener) => {listeners.add(listener);return () => listeners.delete(listener);},
    subscribe:(domain,listener) => {listeners.add(listener);return () => listeners.delete(listener);},
  },
  chat:{
    getComposerDraft:() => '',
    setComposerDraft:value => { probe.calls.push({boundary:'unrelated-startup',name:'setComposerDraft',args:[value]}); },
    restoreTaskDraft:(...args) => unexpected('restoreTaskDraft_for_history_without_text',args),
    sendMessage:(...args) => unexpected('sendMessage',args),
  },
  models:{loadSessionModel:sessionId => {probe.calls.push({boundary:'unrelated-startup',name:'loadSessionModel',args:[sessionId]});return Promise.resolve(null);}},
  sessions:{getSessionWorkspaceBinding:sessionId => {probe.calls.push({boundary:'unrelated-startup',name:'getSessionWorkspaceBinding',args:[sessionId]});return Promise.resolve(null);}},
  settings:{listBuiltinFeatures:() => Promise.resolve([{id:'session-mention',enabled:false}])},
  voice:{appendVoiceText:(...args) => unexpected('appendVoiceText',args)},
  interaction:{setModeLane:() => {}},
};
// Capture bridge-level arguments while delegating to the real JS bridge method.
const actualAbandon = remote.abandonRemoteTask;
remote.abandonRemoteTask = (...args) => {
  probe.calls.push({boundary:'actual-bridge-method',name:'abandonRemoteTask',args:structuredClone(args)});
  return actualAbandon(...args);
};
const { ChatView } = await import('../../src/features/chat/ChatView.jsx');
await remote.readRemoteProxyHistory(id);
const connection = scenario === 'recovering' ? {status:'reconnecting',capabilityStatus:'error'} : {status:'disconnected'};
function Host() {
  const [bs,setBs] = useState({ ...hostState });
  useEffect(() => { renderSnapshot=setBs;probe.ready=true;return () => {renderSnapshot=()=>{};probe.ready=false;};},[]);
  return <div style={{width:'1280px',height:'900px',display:'flex'}}>
    <ChatView theme="light" t={dictZh} bs={bs} remoteConnection={connection}
      remoteModelSource="controlled_device" />
  </div>;
}
createRoot(document.getElementById('root')).render(<Host />);
