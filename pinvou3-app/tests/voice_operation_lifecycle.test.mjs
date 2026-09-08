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
    completeVoiceSubmission, trackVoiceTerminal, dismissVoiceInput, voiceOperationSessionId,
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

  test(`${lane}: rejected materialized first turn belongs to its real session, never another draft`, () => {
    const h = harness(source);
    const operation = h.operation('voiceop-materialized');
    h.api.beginVoiceSubmission(operation.operationId);
    h.api.completeVoiceSubmission(operation.operationId, 'new-real-session', false);
    assert.equal(h.api.getVoiceOperationId(null, 'chat'), null);
    assert.equal(h.api.getVoiceOperationId('new-real-session', 'chat'), operation.operationId);
    h.api.beginVoiceSubmission(operation.operationId);
    h.api.completeVoiceSubmission(operation.operationId, 'new-real-session', true);
    assert.equal(h.events[0].sessionId, 'new-real-session');
  });

  test(`${lane}: restored dispatch clears pending without pretending the voice task was accepted`, async () => {
    const h = harness(source);
    const operation = h.operation('voiceop-restored');
    const chat = read('src/features/chat/ChatView.jsx');
    const start = chat.indexOf('      const sendChatMessage = useCallback(');
    const end = chat.indexOf('\n      // ConversationTimeline', start);
    Object.assign(h.context, { useCallback: fn => fn, activeSessionId: null,
      bridge: { voice: h.api }, dispatchChatMessage: async () => 'restored' });
    vm.runInContext(`${chat.slice(start, end)}\nthis.sendChatMessage = sendChatMessage;`, h.context);
    assert.equal(await h.context.sendChatMessage('restored draft'), 'restored');
    assert.equal(operation.pendingSubmission, false);
    assert.equal(h.api.getVoiceOperationId(null, 'chat'), operation.operationId);
    assert.equal(h.events.length, 0);
    assert.match(chat, /return result === 'restored' \? false : result;/);
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

for (const code of ['permission_denied', 'outcome_unknown']) {
  test(`Web first-turn ${code} preserves the correct admission certainty`, async () => {
    const h = harness(sources.web);
    const operation = h.operation(`voiceop-${code}`);
    const start = sources.web.indexOf('  async function runFirstTurnSubmission(');
    const end = sources.web.indexOf('  function submitFirstWebTurn(', start);
    Object.assign(h.context, { findFirstTurnItem: () => ({}), firstTurnStillVisible: () => true,
      restoreFirstTurnUiState() {}, invokeWithRequestId: async () => { throw Object.assign(new Error(code), { code }); } });
    vm.runInContext(`${sources.web.slice(start, end)}\nthis.runFirstTurnSubmission = runFirstTurnSubmission;`, h.context);
    const submission = { voiceOperationId: operation.operationId, clientMessageId: 'first-turn' };
    await h.context.runFirstTurnSubmission(submission);
    assert.equal(operation.pendingSubmission, code === 'outcome_unknown');
    assert.equal(h.api.getVoiceOperationId(null, 'chat'), code === 'outcome_unknown' ? null : operation.operationId);
    assert.equal(h.events.length, 0);
  });
}

test('stale edit preview is abandoned, while applying a valid edit retains provenance', async () => {
  for (const stale of [true, false]) {
    const h = harness(sources.desktop);
    const operation = h.operation('voiceop-preview');
    const hook = read('src/features/voice-composer/useComposerVoiceInput.js');
    const preview = { original: 'original', next: 'edited', context: { operationId: operation.operationId } };
    let draft = stale ? 'manually changed' : 'original';
    Object.assign(h.context, {
      useCallback: fn => fn, trimDraft: value => String(value || '').trim(),
      editPreview: preview, editPreviewRef: { current: preview }, taskSendInFlightRef: { current: false },
      setEditPreview() {}, adapterRef: { current: {
        getDraft: () => draft, setDraft: value => { draft = value; }, bridge: { voice: {
          dismissVoiceInput: h.api.dismissVoiceInput,
          abandonVoiceResult: () => h.api.trackVoiceTerminal('voice_cancelled', operation, {}),
        } },
      } },
    });
    const discard = hook.slice(hook.indexOf('  const discardVoiceResult ='), hook.indexOf('  const cancelVoiceEditPreview ='));
    const apply = hook.slice(hook.indexOf('  const applyVoiceEditPreview ='), hook.indexOf('  const clearStaleVoiceState ='));
    vm.runInContext(`${discard}\n${apply}\nthis.applyPreview = applyVoiceEditPreview;`, h.context);
    assert.equal(await h.context.applyPreview(), !stale);
    assert.equal(draft, stale ? 'manually changed' : 'edited');
    assert.equal(h.api.getVoiceOperationId(null, 'chat'), stale ? null : operation.operationId);
    assert.deepEqual(h.events.map(e => e.eventName), stale ? ['voice_cancelled'] : []);
  }
});

test('desktop waits for Rust ownership and cannot open a microphone after a rejected claim', async () => {
  const h = harness(sources.desktop);
  let resolveClaim;
  const claim = new Promise(resolve => { resolveClaim = resolve; });
  let microphoneProbes = 0;
  h.state.voiceAsrSetup = {};
  Object.assign(h.context, {
    window: { AudioContext() {} }, navigator: { mediaDevices: { getUserMedia() {} } },
    normalizeVoiceMode: mode => mode, bt: key => key, emitVoiceDiagnostic() {},
    cleanupVoiceInputSession() {}, normalizeVoiceError: error => error,
    currentVoiceWindowLabel: () => 'detached-b', syncVoiceShortcutRecording: () => claim,
    probeVoiceAudioInput: () => { microphoneProbes += 1; return true; },
    invoke: async command => command === 'voice_asr_status' ? { ready: true } : undefined,
  });
  const start = sources.desktop.indexOf('  async function startVoiceInput(');
  const end = sources.desktop.indexOf('  function cancelVoiceInput(', start);
  vm.runInContext(`${sources.desktop.slice(start, end)}\nthis.startVoiceInput = startVoiceInput;`, h.context);
  const starting = h.context.startVoiceInput('', () => {}, { mode: 'dictation' });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(microphoneProbes, 0);
  resolveClaim(false);
  await starting;
  assert.equal(microphoneProbes, 0);
  assert.equal(h.state.voiceInput.status, 'failed');
});

function bridgeFunction(source, name) {
  const start = source.search(new RegExp(`^  (?:async )?function ${name}\\(`, 'm'));
  assert.ok(start >= 0, name);
  const end = source.indexOf('\n  }', start) + 4;
  assert.ok(end > start, name);
  return source.slice(start, end);
}

for (const lane of ['desktop', 'web']) {
  for (const outcome of ['restored', 'false', 'throw', 'switched']) {
    for (const typeDuringAwait of [false, true]) {
      test(`${lane}: real voice writeback/send/prefill ${outcome}, typing=${typeDuringAwait}, restores exactly once`, async () => {
        const h = harness(sources[lane]);
        const chat = read('src/features/chat/ChatView.jsx');
        const hook = read('src/features/voice-composer/useComposerVoiceInput.js');
        const sendSource = lane === 'desktop' ? read('src/platform/tauri/bridge/chat.js') : sources.web;
        const origin = outcome === 'restored' ? null : 'original-session';
        const operation = h.operation('voiceop-real-recovery', 'chat', origin);
        const buffers = { 'original-session': { composerDraft: '', remoteTurnActive: outcome === 'switched' },
          'other-session': { composerDraft: 'other draft' } };
        Object.assign(h.state, { activeSessionId: origin, composerDraft: '', draftEpoch: 4,
          composerPrefill: { id: 0, text: '' }, queued: [], chatItems: [],
          attachments: outcome === 'false' ? [{ status: 'parsing' }] : [] });
        let unblock;
        const gate = new Promise(resolve => { unblock = resolve; });
        let prefillPending = false;
        let draftSyncPending = false;
        let accepted = 0;
        const ctx = h.context;
        Object.assign(ctx, {
          sessionStates: buffers, voice: () => h.api, IS_WEB: lane === 'web', canInvoke: () => false,
          useCallback: fn => fn, useEffect: fn => fn(), activeSessionId: origin,
          activeSessionIdRef: { current: origin }, draftEpoch: 4, inputTextRef: { current: '' },
          setInputTextState() {}, setInputLimitReached() {}, constrainChatInput: text => ({ text: String(text), truncated: false }),
          bt: key => key, addSystemItem() {}, getBuffer: sid => buffers[sid], isBusyFor: () => false,
          formatAttachmentDisplayText: text => { if (outcome === 'throw') throw new Error('controlled send failure'); return text; },
          adoptManagedAttachments: () => outcome === 'switched' ? gate : Promise.resolve(), discardManagedAttachment() {},
          reconcileRemoteTurn: async () => { await gate; return false; },
          ensureSession: async () => { await gate; return null; },
          notify: () => { prefillPending ||= !!h.state.composerPrefill.text; draftSyncPending = true; },
          console: { warn() {} }, setTimeout() {}, composerRef: { current: null },
          onPrefillConsumed: () => { h.state.composerPrefill.text = ''; },
          normalizeMode: mode => mode, trimDraft: text => String(text || '').trim(),
          clearStaleVoiceState() {}, discardVoiceResult() {}, isActiveVoiceTarget: () => true,
          taskSendInFlightRef: { current: false },
        });
        const functions = ['getComposerDraft', 'setComposerDraft', 'prefillComposer', 'restoreTaskDraft',
          lane === 'desktop' ? 'restoreSteerText' : 'restoreComposerText', 'sendMessage'];
        vm.runInContext(functions.map(name => bridgeFunction(sendSource, name)).join('\n'), ctx);
        ctx.bridge = { available: true, voice: h.api, chat: {
          sendMessage: ctx.sendMessage, setComposerDraft: ctx.setComposerDraft, restoreTaskDraft: ctx.restoreTaskDraft,
        } };
        const setStart = chat.indexOf('      const setInputText = useCallback(');
        const setEnd = chat.indexOf('\n      const [artifactsOpen', setStart);
        const sendStart = chat.indexOf('      const sendChatMessage = useCallback(');
        const sendEnd = chat.indexOf('\n      // ConversationTimeline', sendStart);
        const taskStart = chat.indexOf('        sendTask: async (outgoing, context) => {');
        const taskEnd = chat.indexOf('\n        onTaskAccepted:', taskStart);
        const task = chat.slice(taskStart, taskEnd).trim().replace(/^sendTask:/, 'this.voiceTask =').replace(/,$/, ';');
        ctx.dispatchChatMessage = ctx.sendMessage;
        vm.runInContext(`${chat.slice(setStart, setEnd)}\n${chat.slice(sendStart, sendEnd)}\n${task}\nthis.setInput = setInputText;`, ctx);
        ctx.adapterRef = { current: { targetId: 'chat-composer', setDraft: ctx.setInput,
          getDraft: () => ctx.inputTextRef.current, sendTask: ctx.voiceTask,
          onTaskAccepted: () => { accepted += 1; ctx.setInput(''); } } };
        const deliverStart = hook.indexOf('async function deliverVoiceTask(');
        const deliverEnd = hook.indexOf('\nfunction useComposerVoiceInput(', deliverStart);
        const resultStart = hook.indexOf('  const handleVoiceResult = useCallback(');
        const resultEnd = hook.indexOf('\n  // Returns true only', resultStart);
        const errorStart = hook.indexOf('function createVoiceTaskSendError(');
        const errorEnd = hook.indexOf('\n// Task-send teardown:', errorStart);
        vm.runInContext(`${hook.slice(errorStart, errorEnd)}\n${hook.slice(deliverStart, deliverEnd)}\n${hook.slice(resultStart, resultEnd)}\nthis.writeback = handleVoiceResult;`, ctx);
        const sending = ctx.writeback('voice-session', 'voice task', '', { mode: 'task', operationId: operation.operationId })
          .then(() => 'accepted', error => error.category);
        assert.equal(ctx.inputTextRef.current, '', 'owned writeback draft must be consumed before awaiting bridge');
        if (outcome === 'switched') {
          h.state.activeSessionId = 'other-session';
          ctx.activeSessionIdRef.current = 'other-session';
          ctx.setInput('other draft');
        }
        if (typeDuringAwait) ctx.setInput(outcome === 'switched' ? 'other new input' : 'new input');
        unblock();
        assert.equal(await sending, 'send_failed');
        // Consume the actual ChatView prefill effect, not a replacement append stub.
        if (prefillPending) {
          ctx.prefill = h.state.composerPrefill.text;
          ctx.prefillAppend = h.state.composerPrefill.append;
          const effectStart = chat.indexOf('      useEffect(() => {\n        if (prefill)');
          const effectEnd = chat.indexOf('}, [prefill, prefillAppend]);', effectStart) + '}, [prefill, prefillAppend]);'.length;
          vm.runInContext(chat.slice(effectStart, effectEnd), ctx);
        } else if (draftSyncPending) {
          ctx.setInput(h.state.composerDraft);
        }
        const expected = outcome === 'switched' ? (typeDuringAwait ? 'other new input' : 'other draft')
          : typeDuringAwait ? 'new input\nvoice task' : 'voice task';
        assert.equal(ctx.inputTextRef.current, expected);
        if (outcome === 'switched') assert.equal(buffers['original-session'].composerDraft, 'voice task');
        assert.equal(accepted, 0, 'rejected/restored task must not clear the restored draft or claim success');
      });
    }
  }
}
