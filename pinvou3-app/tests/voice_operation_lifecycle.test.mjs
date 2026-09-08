import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';

const root = path.resolve(import.meta.dirname, '..');
const read = file => fs.readFileSync(path.join(root, file), 'utf8');
const sources = {
  desktop: read('src/platform/tauri/bridge/voice.js'),
  web: read('src/platform/web/bridge.js'),
};

function harness(source) {
  const start = source.indexOf('  function voiceToken(');
  const end = source.indexOf('\n  function setVoiceInputStatus(', start);
  const events = [];
  const state = { activeSessionId: null, voiceInput: {} };
  const context = {
    state, activeVoiceInput: null, IS_WEB: true, Date, Math, Map,
    notify() {},
    setVoiceInputStatus(status, patch) { state.voiceInput = { ...state.voiceInput, ...patch, status }; },
    invoke: async (command, args) => { events.push({ command, ...args.request }); },
  };
  vm.createContext(context);
  vm.runInContext(`${source.slice(start, end)}\nthis.api = {
    rememberVoiceOperation, getVoiceOperationId, beginVoiceSubmission,
    completeVoiceSubmission, trackVoiceTerminal, dismissVoiceInput,
  };`, context);
  const operation = (id, ownerKind = 'chat', sessionId = null) => {
    const value = { operationId: id, ownerKind, sessionId, telemetrySessionId: sessionId || 'draft-session',
      startedAt: Date.now(), voiceResultReady: true, telemetryTerminal: false };
    context.api.rememberVoiceOperation(value);
    state.voiceInput = { status: 'completed', operationId: id };
    return value;
  };
  return { api: context.api, events, state, operation, context };
}

for (const [lane, source] of Object.entries(sources)) {
  test(`${lane}: actual finish waits for task writeback without losing its operation`, async () => {
    const h = harness(source);
    const operation = h.operation('voiceop-real-finish');
    let acknowledge;
    let delivered;
    const writeback = new Promise(resolve => { acknowledge = resolve; });
    Object.assign(operation, { mode: 'task', chunks: [], sampleRate: 16000,
      writeback: async (_text, _draft, context) => {
        delivered = context.operationId;
        h.api.beginVoiceSubmission(delivered);
        await writeback;
      } });
    Object.assign(h.context, {
      activeVoiceInput: operation, bt: key => key,
      cleanupVoiceInputSession() {}, syncVoiceShortcutRecording() {}, emitVoiceDiagnostic() {},
      mergeFloatChunks: () => new Float32Array(16000),
      downsamplePcm: samples => samples, encodeWav: () => new ArrayBuffer(44),
      encodeBase64Bytes: () => 'controlled-base64', encodeVoiceBase64Bytes: () => 'controlled-base64',
      voiceNow: () => 1, roundedMs: () => 1,
      normalizeVoiceMode: mode => mode,
      applyVoiceDeterministicCorrections: text => text,
      voiceContainsAny: () => [], VOICE_SUSPICIOUS_ASR_TERMS: [],
      classifyVoiceText: () => ({ strategy: 'use_asr', reason: 'short-clear' }),
      validateVoicePostprocessOutput: () => true, hasVoiceHighRiskResidual: () => [],
      logVoicePipeline() {}, normalizeVoiceError: error => error,
      invoke: async (command, args) => {
        if (command.includes('transcribe_voice_audio')) return { text: 'controlled result' };
        h.events.push({ command, ...args.request });
      },
    });
    const start = source.indexOf('  async function finishVoiceInput(');
    const tail = source.slice(start);
    const end = tail.indexOf('\n  async function ', 10);
    vm.runInContext(`${tail.slice(0, end)}\nthis.finishVoiceInput = finishVoiceInput;`, h.context);
    const finished = h.context.finishVoiceInput(false, false);
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(delivered, operation.operationId, String(h.state.voiceInput.error || h.state.voiceInput.message));
    await h.context.finishVoiceInput(true, false);
    h.api.completeVoiceSubmission(delivered, 'actual-session', true);
    acknowledge();
    await finished;
    assert.deepEqual(h.events.map(e => e.eventName), ['voice_recognition_completed', 'voice_message_submitted']);
    assert.equal(h.state.voiceInput.status, 'idle');
  });

  test(`${lane}: accepted writeback wins over deferred cancellation without reviving its operation`, async () => {
    const h = harness(source);
    const operation = h.operation('voiceop-task');
    let accept;
    const writeback = new Promise(resolve => { accept = resolve; });
    h.api.beginVoiceSubmission(operation.operationId);
    h.api.trackVoiceTerminal('voice_cancelled', operation, { stage: 'recording' });
    assert.equal(h.events.length, 0, 'cancellation cannot decide a pending admission outcome');
    h.api.completeVoiceSubmission(operation.operationId, 'real-created-session', true);
    accept();
    await writeback;
    h.api.trackVoiceTerminal('voice_cancelled', operation, { stage: 'recognition' });
    h.api.completeVoiceSubmission(operation.operationId, 'real-created-session', true);
    assert.equal(operation.telemetryTerminal, true);
    assert.deepEqual(h.events.map(e => e.eventName), ['voice_message_submitted']);
    assert.equal(h.events[0].sessionId, 'real-created-session');
    assert.equal(h.state.voiceInput.status, 'idle');
  });

  for (const mode of ['dictation', 'edit', 'task']) {
    test(`${lane}: ${mode} draft preserves provenance across UI dismissal and later manual admission`, () => {
      const h = harness(source);
      const operation = h.operation(`voiceop-${mode}`, 'chat', 'existing-session');
      h.api.dismissVoiceInput();
      assert.equal(h.api.getVoiceOperationId('existing-session', 'chat'), operation.operationId);
      h.api.beginVoiceSubmission(operation.operationId);
      h.api.completeVoiceSubmission(operation.operationId, 'existing-session', true);
      h.api.trackVoiceTerminal('voice_cancelled', operation, {});
      assert.deepEqual(h.events.map(e => e.eventName), ['voice_message_submitted']);
      assert.equal(h.api.getVoiceOperationId('existing-session', 'chat'), null);
    });
  }

  test(`${lane}: rejection retains retry provenance unless the user really cancelled`, () => {
    const h = harness(source);
    const operation = h.operation('voiceop-retry');
    h.api.beginVoiceSubmission(operation.operationId);
    h.api.completeVoiceSubmission(operation.operationId, null, false);
    assert.equal(h.api.getVoiceOperationId(null, 'chat'), operation.operationId);
    h.api.beginVoiceSubmission(operation.operationId);
    h.api.trackVoiceTerminal('voice_cancelled', operation, { stage: 'recognition' });
    h.api.completeVoiceSubmission(operation.operationId, null, false);
    h.api.completeVoiceSubmission(operation.operationId, null, false);
    assert.deepEqual(h.events.map(e => e.eventName), ['voice_cancelled']);
  });

  test(`${lane}: queued admission cannot associate the same voice with another manual message`, () => {
    const h = harness(source);
    const operation = h.operation('voiceop-queued', 'chat', 'existing-session');
    h.api.beginVoiceSubmission(operation.operationId);
    assert.equal(h.api.getVoiceOperationId('existing-session', 'chat'), null);
    h.api.completeVoiceSubmission(operation.operationId, 'existing-session', false);
    assert.equal(h.api.getVoiceOperationId('existing-session', 'chat'), operation.operationId);
    operation.voiceResultReady = false;
    assert.equal(h.api.getVoiceOperationId('existing-session', 'chat'), null);
  });

  test(`${lane}: late old admission cannot overwrite another composer or another operation`, () => {
    const h = harness(source);
    const old = h.operation('voiceop-old', 'native', 'native-session');
    h.api.beginVoiceSubmission(old.operationId);
    const current = h.operation('voiceop-new', 'chat', 'chat-session');
    assert.equal(h.api.getVoiceOperationId('chat-session', 'native'), null);
    assert.equal(h.api.getVoiceOperationId('native-session', 'chat'), null);
    h.api.completeVoiceSubmission(old.operationId, 'native-session', true);
    assert.equal(h.state.voiceInput.operationId, current.operationId);
    assert.equal(h.api.getVoiceOperationId('chat-session', 'chat'), current.operationId);
    assert.equal(h.events[0].sessionId, 'native-session');
  });

  test(`${lane}: ACP completion does not fabricate a backend task attempt`, () => {
    const h = harness(source);
    const operation = h.operation('voiceop-acp', 'acp', 'acp-session');
    h.api.beginVoiceSubmission(operation.operationId);
    h.api.completeVoiceSubmission(operation.operationId, 'acp-session', true);
    h.api.trackVoiceTerminal('voice_cancelled', operation, {});
    assert.equal(h.events.length, 0);
    assert.equal(operation.telemetryTerminal, true);
  });

  test(`${lane}: submitted payload cannot include transcript, draft, path, diagnostic or raw error`, () => {
    const h = harness(source);
    const operation = h.operation('voiceop-private', 'native', 'native-session');
    Object.assign(operation, { rawText: 'private conversation', draftBeforeStart: 'private draft',
      diagnostic: { error: 'secret raw error' }, modelId: 'private-model', path: 'private-path' });
    h.api.completeVoiceSubmission(operation.operationId, 'native-session', true);
    assert.deepEqual(Object.keys(h.events[0]).sort(), ['command', 'eventName', 'operationId', 'sessionId']);
  });

  test(`${lane}: implementation publishes context before awaiting writeback and never resets terminal`, () => {
    const finishStart = source.indexOf('  async function finishVoiceInput(');
    const finish = source.slice(finishStart, source.indexOf('\n  async function ', finishStart + 10));
    assert.match(finish, /session\.voiceResultReady = (?:true|!!finalText && !editUnchanged);[\s\S]*?await session\.writeback\([\s\S]*?operationId: session\.operationId/);
    if (lane === 'desktop') assert.match(finish, /session\.voiceResultReady = !!finalText && !editUnchanged;/);
    assert.ok(finish.indexOf('"voice_recognition_completed"') < finish.indexOf('await session.writeback'));
    assert.doesNotMatch(finish, /telemetryTerminal:\s*false/);
    assert.match(finish, /if \(session\.telemetryTerminal\) return;/);
  });
}

test('composer adapters preserve explicit metadata and authoritative native ownership', () => {
  const hook = read('src/features/voice-composer/useComposerVoiceInput.js');
  const chat = read('src/features/chat/ChatView.jsx');
  const native = read('src/features/codex/CodexAcpView.jsx');
  assert.match(hook, /deliverVoiceTask\(current, next, \{ \.\.\.preview\.context, mode: 'edit'/);
  assert.match(chat, /sendChatMessage\(constrained\.text, context\)/);
  assert.match(native, /sendTask: async \(outgoing, context\) => send\(outgoing, context\)/);
  assert.match(native, /nativeControlsSessionRef\.current === activeId \? nativeControls\.modelId : null/);
  assert.match(native, /completeVoiceSubmission\(voiceOperationId, targetId, true\)/);
  assert.match(native, /ownerKind: isNativeAgent \? 'native' : 'acp'/);
  const web = sources.web;
  assert.match(web, /completeVoiceSubmission\(submission\.voiceOperationId, metadata\.id, true\)/);
  assert.doesNotMatch(web, /error: "session_required"/);
});
