import React, { useState } from 'react';
import { createRoot } from 'react-dom/client';
import { dictZh } from '../../src/shared/i18n/zh.js';
import { dictEn } from '../../src/shared/i18n/en.js';
import { dictJa } from '../../src/shared/i18n/ja.js';
import { recordRemoteDispatch, remoteDispatchCases } from '../../src/features/remote-control/remote-dispatch-ui.mjs';
import { remoteProxyLeaseIdentity } from '../../src/features/remote-control/remote-session-adapter.mjs';

// Mount the real ChatView once. Only the host bridge is controlled; React
// state, effect cleanup, retry handlers, transfer guard and case ledger are real.
const parameters = new URLSearchParams(location.search);
const copy = { zh: dictZh, en: dictEn, ja: dictJa }[parameters.get('lang') || 'zh'];
const fixture = window.dispatchFixture = {
  calls: [], commits: [], uploads: [], previews: [], controlledSnapshots: [], renewals: [], restores: [], current: null,
  scenario: parameters.get('scenario') || 'commit', copy,
};
const platformCapabilities = { loaded: true, os: 'windows', desktopRemoteControl: true };
window.PinvouPlatform = { kind: 'desktop', capabilities: new Proxy({}, { get: () => false }) };
let composerDraft = '';
const items = new Map();
function makeSession(id, leaseEpoch = 1, revision = 1, generation = 'generation-1') {
  const connection = {
    localSessionId: id, status: 'connected', sessionMode: 'chat', sessionRef: `ref-${id}`,
    connectionId: 'connection-1', controllerLeaseId: `lease-${leaseEpoch}`, leaseEpoch,
    relayEpoch: 'relay-1', generation, mappingConfirmed: true,
    capabilityRevision: revision, capabilityStatus: 'ready', capabilityError: null, temporaryModelAvailable: false,
  };
  const previous = [...items.values()].find(item => item.id === id);
  const handle = previous?.handle || { taskId: `task-${id}-${leaseEpoch}`, clientRequestId: `request-${id}-${leaseEpoch}`,
    requestFingerprint: '1'.repeat(64) };
  const history = { localSessionId: id, title: id, entries: [], pendingUnknownCount: 0,
    unattemptedPending: [{ ...handle, dispatchState: 'prepared_never_attempted' }] };
  const capabilities = {
    connectionId: connection.connectionId, controllerLeaseId: connection.controllerLeaseId,
    leaseEpoch, relayEpoch: connection.relayEpoch, remoteControlGeneration: connection.generation,
    remoteSessionRef: connection.sessionRef, capabilityRevision: revision,
    expiresAtMs: Date.now() + (fixture.scenario.startsWith('idle') ? 30_000 : 3600000), modes: ['chat'], workspaces: [],
    skills: fixture.scenario.startsWith('idle')
      ? [{ skillId: 'skill-document', displayName: '公文写作', scopes: ['chat'] }] : [], tools: [],
  };
  if (!previous && !fixture.scenario.startsWith('idle') && (fixture.scenario !== 'upload' || id !== 'remote-a')) {
    recordRemoteDispatch(id, connection.sessionRef, remoteProxyLeaseIdentity(id, connection),
      'Prepared retry text', { state: 'prepared_not_sent', retryHandle: handle }, history);
  } else if (!previous) {
    history.unattemptedPending = [];
  }
  const bs = {
    activeSessionId: id, remoteProxyHistory: history, remoteCapabilities: capabilities,
    chatItems: [], messages: [], artifacts: [], attachments: [], sessions: [], queuedMessages: [],
    tokens: { input: 0, max: 0 }, modeState: { mode: 'yolo' }, settings: {},
  };
  return { id, connection, handle, history, capabilities, bs };
}
function session(id, lease = 1, revision = 1, generation = 'generation-1') {
  const key = `${id}:${lease}:${revision}:${generation}`;
  if (!items.has(key)) items.set(key, makeSession(id, lease, revision, generation));
  return items.get(key);
}
window.TauriBridge = {
  available: true,
  state: {
    getMany: () => ({ platformCapabilities }),
    get: domain => domain === 'desktopRemoteControl'
      ? { controllerConnection: fixture.current.connection, remoteCapabilities: fixture.current.capabilities } : {},
    subscribe: () => () => {},
  },
  chat: {
    getComposerDraft: () => composerDraft, setComposerDraft: text => { composerDraft = text; },
    restoreTaskDraft: (text, options) => fixture.restores.push({ text, ...options }),
  },
  voice: {},
  models: { loadSessionModel: async () => {} },
  settings: { listBuiltinFeatures: async () => [] },
  desktopRemoteControl: {
    capabilitiesSnapshot: async () => fixture.current.capabilities,
    controllerCapabilitySend: () => fixture.scenario.startsWith('idle')
      ? new Promise((resolve, reject) => fixture.renewals.push({ resolve, reject })) : Promise.resolve(),
    listExportableModels: async () => [],
    artifactIndex: async id => [{ artifactId: `artifact-${id}`, taskId: `completed-${id}`,
      displayName: `${id}.txt`, mimeType: 'text/plain', totalBytes: 4, previewKind: 'text' }],
    permissionList: async () => [],
    artifactPreview(id) {
      return new Promise((resolve, reject) => fixture.previews.push({ id, resolve, reject }));
    },
    readRemoteProxyHistory: async id => session(id).history,
    async reserveRemoteTask(id, remoteRef, intent) {
      fixture.calls.push({ kind: 'reserve', id, remoteRef, intent,
        revision: fixture.current.connection.capabilityRevision });
      if (fixture.scenario === 'idle-removed-skill') {
        throw Object.assign(new Error('task_skill_invalid'), { code: 'task_skill_invalid' });
      }
      return { state: 'prepared_not_sent', retryHandle: session(id).handle };
    },
    attachmentBegin(id, ref, taskId) {
      fixture.calls.push({ kind: 'attachmentBegin', id, ref, taskId });
      if (fixture.calls.filter(call => call.kind === 'attachmentBegin').length === 1) {
        return Promise.reject(Object.assign(new Error('upload failed'), { code: 'remote_attachment_transport' }));
      }
      return new Promise((resolve, reject) => fixture.uploads.push({ id, resolve, reject }));
    },
    attachmentChunk: async (id, ref, taskId, transferId, index, data, length) => {
      fixture.calls.push({ kind: 'chunk', id, taskId, transferId });
      return { receivedBytes: length, state: 'completed' };
    },
    attachmentAbort: async () => {},
    controlledCapabilitiesSnapshot: () => new Promise((resolve, reject) =>
      fixture.controlledSnapshots.push({ resolve, reject })),
    controlledTaskSend: async (...args) => { fixture.calls.push({ kind: 'controlledSend', args }); },
    commitRemoteTask(id, remoteRef, handle, attachments) {
      fixture.calls.push({ kind: 'commit', id, remoteRef, handle, attachments });
      return new Promise((resolve, reject) => fixture.commits.push({ id, handle, resolve, reject }));
    },
  },
};
const { ChatView } = await import('../../src/features/chat/ChatView.jsx');
function App() {
  const [selected, setSelected] = useState(() => {
    if (fixture.scenario !== 'controlled') return session('remote-a');
    return { id: 'controlled-1', connection: null, bs: {
      ...session('remote-a').bs, activeSessionId: 'controlled-1', remoteProxyHistory: null,
      controlledDevice: { status: 'connected', generation: 'generation-b' },
      controlledSession: { sessionId: 'controlled-1', generation: 'generation-b', lane: 'chat' },
      controlledTranscript: [], controlledTasks: [],
    } };
  });
  fixture.current = selected;
  fixture.select = (...args) => setSelected(session(...args));
  fixture.selectExpired = (...args) => {
    const expired = session(...args);
    expired.capabilities.expiresAtMs = Date.now() - 1;
    setSelected(expired);
  };
  fixture.expire = () => {
    // Suspend the timer's scheduling while moving the wall clock past expiry.
    // The next user interaction rerenders the real ChatView with an old cache.
    const expiredNow = selected.capabilities.expiresAtMs + 1;
    Date.now = () => expiredNow;
  };
  fixture.completeRenewal = () => {
    const next = session(selected.id, selected.connection.leaseEpoch,
      selected.connection.capabilityRevision + 1, selected.connection.generation);
    if (fixture.scenario === 'idle-removed-skill') next.capabilities.skills = [];
    fixture.current = next;
    setSelected(next);
    // Keep host state authoritative even before React processes setSelected.
    fixture.renewals.at(-1).resolve();
  };
  fixture.cases = id => remoteDispatchCases(id, session(id).history);
  return <ChatView theme="dark" t={copy} bs={selected.bs}
    remoteConnection={selected.connection} remoteModelSource="controlled_device" />;
}
const appRoot = createRoot(document.getElementById('root'));
fixture.unmount = () => appRoot.unmount();
appRoot.render(<App />);
