import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import * as sessionMention from '../src/features/chat/session-mention.js';
import { prepareSceneCapabilities, requiredCapabilitiesForMeta } from '../src/features/chat/scene-capabilities.js';

const root = path.resolve(import.meta.dirname, '..');
const read = file => fs.readFileSync(path.join(root, file), 'utf8');
const sources = {
  desktop: read('src/platform/tauri/bridge/voice.js'),
  web: read('src/platform/web/bridge.js'),
};

// #566 合并后锚点适配：共享簇收敛后，lane 内的原完整函数体可能只剩顶格薄委托
// （'function x()' / 'async function x()'），区块结束锚点需同时兼容两空格缩进与
// 顶格、以及 async 前缀的有无。
function blockEnd(source, from, name) {
  const hit = new RegExp(`\\n(?:  )?(?:async )?function ${name}\\(`).exec(source.slice(from));
  return hit === null ? -1 : from + hit.index;
}

function harness(source) {
  const start = source.indexOf('  function voiceToken(');
  // #566 合并后锚点适配：web lane 的 setVoiceInputStatus 收敛进共享簇后，lane 内只剩
  // 顶格薄委托（无两空格缩进）；边界锚点同时兼容两种缩进形态。
  const end = ['\n  function setVoiceInputStatus(', '\nfunction setVoiceInputStatus(']
    .map(marker => source.indexOf(marker, start))
    .filter(index => index !== -1)
    .reduce((a, b) => Math.min(a, b), Number.MAX_SAFE_INTEGER);
  const events = [];
  const state = { activeSessionId: null, voiceInput: {} };
  const context = {
    state, activeVoiceInput: null, IS_WEB: true, Date, Math, Map,
    notify() {},
    setVoiceInputStatus(status, patch) { state.voiceInput = { ...state.voiceInput, ...patch, status }; },
    invoke: async (command, args) => { events.push({ command, ...args.request }); },
  };
  vm.createContext(context);
  // #701 T0-c 壳化适配：切片内的操作生命周期函数已委托 helpers；沙箱按
  // index.html 顺序补载共享基座并接好 lane 簇访问器（上游 baseSandbox 同形）。
  vm.runInContext('this.window = this;', context);
  vm.runInContext(read('src/shared/bridge-shared-helpers.js'), context);
  vm.runInContext(`this.pinvouSharedtauriVoice = () => this.__voiceCluster || (this.__voiceCluster = window.PinvouBridgeShared.create("tauriVoice", {
    state, notify, invoke,
    activeVoiceInput: { get value() { return activeVoiceInput; }, set value(v) { activeVoiceInput = v; } },
    stopMediaTracks() {}, bt: (k) => k, VOICE_DEVICE_REQUEST_TIMEOUT_MS: 8000, finishVoiceInput() {},
    voiceOperations: { get value() { return voiceOperations; } },
  }));`, context);
  vm.runInContext(`this.pinvouSharedweb = () => this.__webCluster || (this.__webCluster = window.PinvouBridgeShared.create("web", {
    state, invoke, notify: () => {}, bt: { get value() { return (k) => k; } },
    voiceOperations: { get value() { return voiceOperations; } },
    activeVoiceInput: { get value() { return activeVoiceInput; }, set value(v) { activeVoiceInput = v; } },
    VOICE_DEVICE_REQUEST_TIMEOUT_MS: { get value() { return 8000; } },
    finishVoiceInput() {}, stopMediaTracks() {},
  }));`, context);
  vm.runInContext(`${source.slice(start, end)}\nthis.api = {
    rememberVoiceOperation, getVoiceOperationId, beginVoiceSubmission,
    completeVoiceSubmission, trackVoiceTerminal, dismissVoiceInput, voiceOperationSessionId,
    ...(typeof rebindVoiceDraftAfterRollback === 'function' ? { rebindVoiceDraftAfterRollback } : {}),
    ...(typeof rebindVoiceOperationToDraft === 'function' ? { rebindVoiceOperationToDraft } : {}),
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

// Official 适配：上游 #586 把发送裁决/引用台账收敛到 ChatView 模块级工厂
// （settleSessionRefSend 家族），发送切片随之依赖 session-mention.js 的解析
// 契约。沙箱按模块本体装载这两层；组件作用域的 ref 用可变对象模拟
// （sessionMentionEnabledRef 保持上游的 fail-open 缺省，草稿键按当前
// 会话/纪元推同形字符串）。
function injectChatViewMentionScope(ctx, chat) {
  // #566 合并后锚点适配：ChatView 的 legacy markdown 缓存已采纳上游 weak-cache 形态
  // （renderLegacyMarkdownCached = createWeakCache(...)），旧的 const legacyMarkdownCache
  // 切片锚点不复存在；模块级 helper 区段改以新锚点收尾。
  const helpers = chat.slice(chat.indexOf('const refsSurvivingAcceptance'), chat.indexOf('const renderLegacyMarkdownCached = createWeakCache('));
  if (!ctx.mentionDraftKeyRef || ctx.mentionDraftKeyRef.current === null) ctx.mentionDraftKeyRef = { current: (ctx.activeSessionId ? 'session:' + ctx.activeSessionId + ':' : 'draft:') + (ctx.draftEpoch || 0) };
  if (!ctx.sessionMentionEnabledRef) ctx.sessionMentionEnabledRef = { current: true };
  if (!ctx.setSessionRefs) ctx.setSessionRefs = () => {};
  if (!ctx.sessionRefs) ctx.sessionRefs = [];
  if (!ctx.sessionRefsRef) ctx.sessionRefsRef = { current: ctx.sessionRefs };
  if (!ctx.mentionPendingDraftSendsRef) ctx.mentionPendingDraftSendsRef = { current: new Set() };
  Object.assign(ctx, {
    ...sessionMention,
    useMemo: fn => fn(),
    sessionMentionEnabled: true,
    setSessionMentionEnabled() {},
  });
  vm.runInContext(helpers, ctx, { filename: 'ChatView:module-helpers' });
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
    Object.assign(h.context, { useCallback: fn => fn, useMemo: fn => fn, activeSessionId: null,
      bridge: { voice: h.api }, dispatchChatMessage: async () => 'restored' });
    injectChatViewMentionScope(h.context, chat);
    vm.runInContext(`${chat.slice(start, end)}\nthis.sendChatMessage = sendChatMessage;`, h.context);
    assert.equal(await h.context.sendChatMessage('restored draft'), 'restored');
    assert.equal(operation.pendingSubmission, false);
    assert.equal(h.api.getVoiceOperationId(null, 'chat'), operation.operationId);
    assert.equal(h.events.length, 0);
    assert.match(chat, /return result === 'restored' \|\| result === 'restored-materialized' \|\| result === 'held' \? false : result;/);
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
    assert.deepEqual(Object.keys(h.events[0]).sort(), ['command', 'eventName', 'inputType', 'operationId', 'sessionId']);
    assert.equal(h.events[0].inputType, 'voice');
  });

  test(`${lane}: implementation publishes context before awaiting writeback and never resets terminal`, () => {
    const finishStart = source.indexOf('  async function finishVoiceInput(');
    const finish = source.slice(finishStart, source.indexOf('\n  async function ', finishStart + 10));
    assert.match(finish, /session\.voiceResultReady = (?:true|!!finalText && !editUnchanged);[\s\S]*?await session\.writeback\([\s\S]*?operationId: session\.operationId/);
    if (lane === 'desktop') assert.match(finish, /session\.voiceResultReady = !!finalText && !editUnchanged;/);
    assert.ok(finish.indexOf('"voice_recognition_completed"') < finish.indexOf('await session.writeback'));
    assert.doesNotMatch(finish, /telemetryTerminal:\s*false/);
    // Backflow pinvou-agent#789: the latch gate now also resets the stuck
    // active status to idle (both before the write-back and before the
    // completed notice) instead of a bare return; the terminal flag itself is
    // still never reset (the doesNotMatch above).
    assert.match(finish, /if \(session\.telemetryTerminal\) \{[^}]*?setVoiceInputStatus\("idle"/);
  });
}

test('composer adapters preserve explicit metadata and authoritative native ownership', () => {
  const hook = read('src/features/voice-composer/useComposerVoiceInput.js');
  const chat = read('src/features/chat/ChatView.jsx');
  const native = read('src/features/codex/CodexAcpView.jsx');
  assert.match(hook, /deliverVoiceTask\(current, next, \{ \.\.\.preview\.context, mode: 'edit'/);
  assert.match(chat, /sendChatMessage\(outgoingText, \{ \.\.\.context, draftOwner: owner \}\)/);
  assert.match(native, /sendTask: async \(outgoing, context\) => send\(outgoing, \{ voiceOperationId: context\?\.operationId \}\)/);
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
      // applyVoiceEditPreview ends the unsent operation through the shared
      // dismiss helper; route it to the same dismiss bookkeeping the adapter
      // stub above exposes so the assertions observe one terminal path.
      dismissVoice: () => h.api.dismissVoiceInput(),
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

// PR#2138 review regression: the send gate (busy / workspace sync / session
// readiness / auth) can flip to false between recording start and transcription
// completion. A closed gate must fail the real submission chain with the
// send_failed error — the bridge's catch then lands on the failure notice and
// the completed "voiceTaskSent" writeback is unreachable — while an open gate
// keeps the normal accepted path untouched.
for (const gateOpen of [false, true]) {
  test(`voice task gate ${gateOpen ? 'open: the real send stays accepted' : 'closed: writeback chain fails instead of faking a sent success'}`, async () => {
    const hook = read('src/features/voice-composer/useComposerVoiceInput.js');
    const sends = [];
    const blocked = [];
    let acceptedCount = 0;
    let draft = '';
    const sandbox = {
      useCallback: fn => fn,
      trimDraft: value => String(value || '').trim(),
      normalizeMode: mode => mode,
      isActiveVoiceTarget: () => true,
      clearStaleVoiceState() {},
      discardVoiceResult() {},
      taskSendInFlightRef: { current: false },
      adapterRef: { current: {
        targetId: 'chat-composer',
        getDraft: () => draft,
        setDraft: value => { draft = String(value); },
        canSendTask: () => gateOpen,
        sendTask: async text => { sends.push(text); return true; },
        onTaskBlocked: (stage, text) => blocked.push([stage, text]),
        onTaskAccepted: () => { acceptedCount += 1; },
      } },
    };
    vm.createContext(sandbox);
    const errorStart = hook.indexOf('function createVoiceTaskSendError(');
    const deliverStart = hook.indexOf('async function deliverVoiceTask(');
    const deliverEnd = hook.indexOf('\nfunction useComposerVoiceInput(', deliverStart);
    const resultStart = hook.indexOf('  const handleVoiceResult = useCallback(');
    const resultEnd = hook.indexOf('\n  // Returns true only', resultStart);
    vm.runInContext(
      `${hook.slice(errorStart, hook.indexOf('\n// Task-send teardown:', errorStart))}\n`
      + `${hook.slice(deliverStart, deliverEnd)}\n`
      + `${hook.slice(resultStart, resultEnd)}\nthis.writeback = handleVoiceResult;`,
      sandbox,
    );
    const outcome = await sandbox.writeback('voice-session', 'voice task', '', { mode: 'task', operationId: 'voiceop-gate' })
      .then(() => 'accepted', error => error.category);
    if (gateOpen) {
      assert.deepEqual(sends, ['voice task'], 'an open gate must reach the real sendTask');
      assert.deepEqual(blocked, [], 'an open gate must not report a block');
      assert.equal(acceptedCount, 1, 'the accepted path keeps its success bookkeeping');
      assert.equal(outcome, 'accepted', 'a real send must not be flipped into a failure');
    } else {
      assert.deepEqual(sends, [], 'a closed gate must not pretend sendTask ran');
      assert.deepEqual(blocked, [['gate', 'voice task']], 'the gate rejection must still notify the adapter');
      assert.equal(acceptedCount, 0, 'a blocked task must not claim accepted bookkeeping');
      assert.equal(draft, 'voice task', 'the text stays in the draft for a manual retry');
      assert.equal(outcome, 'send_failed', 'the gate rejection must fail the writeback chain so no completed/voiceTaskSent writeback can follow');
    }
  });
}

test('desktop waits for Rust ownership and cannot open a microphone after a rejected claim', async () => {
  const h = harness(sources.desktop);
  let resolveClaim;
  const claim = new Promise(resolve => { resolveClaim = resolve; });
  let microphoneProbes = 0;
  h.state.voiceAsrSetup = {};
  Object.assign(h.context, {
    // T0-c 壳化适配：window 自引用承载共享基座（PinvouBridgeShared），覆盖时保留。
    window: Object.assign(h.context.window, { AudioContext() {} }), navigator: { mediaDevices: { getUserMedia() {} } },
    normalizeVoiceMode: mode => mode, bt: key => key, emitVoiceDiagnostic() {},
    cleanupVoiceInputSession() {}, normalizeVoiceError: error => error,
    currentVoiceWindowLabel: () => 'detached-b', syncVoiceShortcutRecording: () => claim,
    probeVoiceAudioInput: () => { microphoneProbes += 1; return true; },
    invoke: async command => command === 'voice_asr_status' ? { ready: true } : undefined,
  });
  const start = sources.desktop.indexOf('  async function startVoiceInput(');
  const end = blockEnd(sources.desktop, start, 'cancelVoiceInput');
  vm.runInContext(`${sources.desktop.slice(start, end)}\nthis.startVoiceInput = startVoiceInput;`, h.context);
  const starting = h.context.startVoiceInput('', () => {}, { mode: 'dictation' });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(microphoneProbes, 0);
  resolveClaim(false);
  await starting;
  assert.equal(microphoneProbes, 0);
  assert.equal(h.state.voiceInput.status, 'failed');
});

test('desktop: a cancel racing the ownership claim re-releases the claim token', async () => {
  const h = harness(sources.desktop);
  let resolveClaim;
  const claim = new Promise(resolve => { resolveClaim = resolve; });
  const ownershipCalls = [];
  let claiming = true;
  let microphoneProbes = 0;
  h.state.voiceAsrSetup = {};
  Object.assign(h.context, {
    window: Object.assign(h.context.window, { AudioContext() {} }), navigator: { mediaDevices: { getUserMedia() {} } },
    normalizeVoiceMode: mode => mode, bt: key => key, emitVoiceDiagnostic() {},
    cleanupVoiceInputSession() {}, normalizeVoiceError: error => error,
    currentVoiceWindowLabel: () => 'detached-b',
    syncVoiceShortcutRecording: (label, token) => {
      ownershipCalls.push([label, token]);
      if (claiming) return claim;
      return Promise.resolve(true);
    },
    probeVoiceAudioInput: () => { microphoneProbes += 1; return true; },
    invoke: async command => command === 'voice_asr_status' ? { ready: true } : undefined,
  });
  const start = sources.desktop.indexOf('  async function startVoiceInput(');
  const end = blockEnd(sources.desktop, start, 'cancelVoiceInput');
  vm.runInContext(`${sources.desktop.slice(start, end)}\nthis.startVoiceInput = startVoiceInput;`, h.context);
  const starting = h.context.startVoiceInput('', () => {}, { mode: 'dictation' });
  await new Promise(resolve => setImmediate(resolve));
  const token = h.state.voiceInput.ownershipToken;
  // A cancel lands while the claim IPC is still in flight: finishVoiceInput
  // already released, so the resolving claim must release again.
  h.context.activeVoiceInput = null;
  claiming = false;
  resolveClaim(true);
  await starting;
  assert.deepEqual(
    ownershipCalls,
    [['detached-b', token], [null, token]],
    'the raced claim must be re-released, or the leaked claim locks the microphone in every window',
  );
  assert.equal(microphoneProbes, 0, 'a raced start must not open the microphone');
});

test('desktop: a claim IPC failure fails closed without probing and reports the check, not another window', async () => {
  const h = harness(sources.desktop);
  let microphoneProbes = 0;
  h.state.voiceAsrSetup = {};
  Object.assign(h.context, {
    window: Object.assign(h.context.window, { AudioContext() {} }), navigator: { mediaDevices: { getUserMedia() {} } },
    normalizeVoiceMode: mode => mode, bt: key => key, emitVoiceDiagnostic() {},
    cleanupVoiceInputSession() {}, normalizeVoiceError: error => error,
    currentVoiceWindowLabel: () => 'detached-b', syncVoiceShortcutRecording: () => Promise.resolve('error'),
    probeVoiceAudioInput: () => { microphoneProbes += 1; return true; },
    invoke: async command => command === 'voice_asr_status' ? { ready: true } : undefined,
  });
  const start = sources.desktop.indexOf('  async function startVoiceInput(');
  const end = blockEnd(sources.desktop, start, 'cancelVoiceInput');
  vm.runInContext(`${sources.desktop.slice(start, end)}\nthis.startVoiceInput = startVoiceInput;`, h.context);
  await h.context.startVoiceInput('', () => {}, { mode: 'dictation' });
  assert.equal(microphoneProbes, 0, 'an unverifiable claim must still fail closed before the microphone');
  assert.equal(h.state.voiceInput.status, 'failed');
  assert.equal(
    h.state.voiceInput.message,
    'voiceMicOwnershipUnavailable',
    'a claim IPC failure must not be misattributed to another window recording',
  );
});

test('desktop: an unreadable window label reports the ownership check unavailable instead of another window', async () => {
  const h = harness(sources.desktop);
  const source = sources.desktop;
  // The slice must include the serialization chain variable: the sync helper
  // queues every attempt through it.
  const start = source.indexOf('  var voiceOwnershipSyncTail = Promise.resolve();');
  const end = source.indexOf('  function setVoiceShortcutEnabled(enabled) {', start);
  assert.ok(start >= 0 && end > start, 'desktop must keep the ownership sync helper');
  const invokeCalls = [];
  h.context.console = { warn() {} };
  h.context.emitVoiceDiagnostic = () => {};
  h.context.invoke = async (command, args) => {
    invokeCalls.push([command, { label: args.label, token: args.token }]);
    return true;
  };
  vm.runInContext(`${source.slice(start, end)}\nthis.sync = syncVoiceShortcutRecording;`, h.context);
  assert.equal(
    await h.context.sync('', 'voice_1'), 'error',
    'an unreadable window label must not go over the wire as a claim',
  );
  assert.deepEqual(invokeCalls, [], 'no ownership IPC may fire without a window label');
  assert.equal(await h.context.sync(null, 'voice_1'), true, 'a null label is a release and still dispatches');
  assert.deepEqual(
    invokeCalls,
    [['set_voice_shortcut_recording', { label: null, token: 'voice_1' }]],
    'the release path keeps dispatching with an explicit null label',
  );
});

test('desktop: a rejected ownership claim IPC maps to "error" with its diagnostics, never to false', async () => {
  // Direct test of the real helper's rejection mapping (the "claim IPC
  // failure" consumer test above stubs syncVoiceShortcutRecording, so the
  // real reject→"error" branch has no anchor there; a reject→false regression
  // would misreport an IPC failure as "another window is recording").
  const h = harness(sources.desktop);
  const source = sources.desktop;
  const start = source.indexOf('  var voiceOwnershipSyncTail = Promise.resolve();');
  const end = source.indexOf('  function setVoiceShortcutEnabled(enabled) {', start);
  assert.ok(start >= 0 && end > start, 'desktop must keep the ownership sync helper');
  const diagnostics = [];
  const warns = [];
  h.context.console = { warn: (...args) => warns.push(args) };
  h.context.emitVoiceDiagnostic = (scope, level, message, sessionId, reason) => {
    diagnostics.push({ scope, level, message, sessionId, reason });
  };
  h.context.invoke = async (command, args) => {
    if (args.label === null) return true; // the release dispatch lands normally
    throw new Error('ipc down'); // the claim IPC itself fails
  };
  vm.runInContext(`${source.slice(start, end)}\nthis.sync = syncVoiceShortcutRecording;`, h.context);
  assert.equal(
    await h.context.sync('detached-b', 'voice_1'), 'error',
    'a rejected claim IPC must resolve to "error" (the consumer maps it to voiceMicOwnershipUnavailable); returning false would claim another window is recording',
  );
  assert.equal(diagnostics.length, 1, 'the rejection must emit exactly one voice diagnostic');
  assert.equal(diagnostics[0].scope, 'recording');
  assert.equal(diagnostics[0].level, 'warn');
  assert.ok(String(diagnostics[0].message).includes('recording ownership sync failed'));
  assert.equal(diagnostics[0].reason, 'claim_error');
  assert.equal(warns.length, 1, 'the rejection must also land on the console.warn path');
  // The serialization chain must swallow the rejection: the next sync in the
  // queue still reaches the backend.
  assert.equal(await h.context.sync(null, 'voice_1'), true, 'the ownership chain must heal after a rejected claim');
});

test('desktop: ownership syncs reach the backend in call order', async () => {
  // The release-then-reclaim handoff relies on backend ordering: the chain
  // must not resolve a later sync before the earlier one finished its IPC.
  const h = harness(sources.desktop);
  const source = sources.desktop;
  const start = source.indexOf('  var voiceOwnershipSyncTail = Promise.resolve();');
  const end = source.indexOf('  function setVoiceShortcutEnabled(enabled) {', start);
  const order = [];
  let releaseIpc;
  const releaseGate = new Promise(resolve => { releaseIpc = resolve; });
  h.context.console = { warn() {} };
  h.context.emitVoiceDiagnostic = () => {};
  h.context.invoke = async (command, args) => {
    order.push(`start:${args.label}`);
    if (args.label === null) await releaseGate;
    order.push(`end:${args.label}`);
    return true;
  };
  vm.runInContext(`${source.slice(start, end)}\nthis.sync = syncVoiceShortcutRecording;`, h.context);
  const claim = h.context.sync('detached-b', 'voice_1');
  await claim;
  const release = h.context.sync(null, 'voice_1');
  const reclaim = h.context.sync('detached-b', 'voice_2');
  let reclaimSettled = false;
  reclaim.then(() => { reclaimSettled = true; });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(reclaimSettled, false, 'the reclaim must wait behind the in-flight release IPC');
  releaseIpc();
  await Promise.all([release, reclaim]);
  assert.deepEqual(order, [
    'start:detached-b', 'end:detached-b',
    'start:null', 'end:null',
    'start:detached-b', 'end:detached-b',
  ], 'syncs must run strictly one at a time, in call order');
});

function bridgeFunction(source, name) {
  const start = source.search(new RegExp(`^  (?:async )?function ${name}\\(`, 'm'));
  assert.ok(start >= 0, name);
  const end = source.indexOf('\n  }', start) + 4;
  assert.ok(end > start, name);
  return source.slice(start, end);
}

for (const navigation of ['none', 'session', 'new-draft', 'after-rollback']) {
  test(`desktop: actual multi-agent materialization rollback preserves only its own draft binding, navigation=${navigation}`, async () => {
    const h = harness(sources.desktop);
    const ctx = h.context;
    const sessionSource = read('src/platform/tauri/bridge/sessions.js');
    const chatSource = read('src/platform/tauri/bridge/chat.js');
    Object.assign(h.state, { draftEpoch: 4, pendingDraftMultiAgent: true,
      composerDraft: '', composerPrefill: { id: 0, text: '' }, messages: [], chatItems: [] });
    const operation = h.operation('voiceop-official-rollback');
    h.api.beginVoiceSubmission(operation.operationId);
    const buffers = {};
    let reachToggle;
    let finishToggle;
    const toggleStarted = new Promise(resolve => { reachToggle = resolve; });
    const toggleGate = new Promise(resolve => { finishToggle = resolve; });
    Object.assign(ctx, {
      voice: () => h.api, sessionStates: buffers, ensureSessionInFlight: null,
      ensureSessionDraftOutcome: null, sessionSwitchRequestToken: 0, pendingTaskDraftRecovery: { buffer: null },
      activeRemoteProxyId: null, isRemoteProxyView: () => false,
      freshBuffer: () => ({ composerDraft: '', messages: [], chatItems: [] }),
      getBuffer: sid => buffers[sid], saveWorkingSetTo() {},
      loadWorkingSetFrom: buffer => Object.assign(h.state, buffer),
      currentDraftModeState: () => ({ mode: 'yolo' }),
      switchActiveTo: sid => { buffers[sid] = ctx.freshBuffer(); h.state.activeSessionId = sid; },
      refreshHistoryList: async () => {
        if (navigation === 'after-rollback') {
          ctx.sessionSwitchRequestToken += 1;
          h.state.activeSessionId = 'other-session';
          h.state.composerDraft = 'unrelated input';
        }
      }, addSystemItem() {}, bt: key => key,
      invoke: async command => {
        if (command === 'create_session') return { id: 'rolled-back-session' };
        if (command === 'set_multi_agent_mode') { reachToggle(); await toggleGate; throw new Error('controlled toggle rejection'); }
        if (command === 'delete_session' && (navigation === 'session' || navigation === 'new-draft')) {
          if (navigation === 'new-draft') ctx.enterDraft();
          else { ctx.sessionSwitchRequestToken += 1; h.state.activeSessionId = 'other-session'; }
          h.state.composerDraft = 'unrelated input';
        }
        return null;
      },
    });
    // #484 backflow: the merged ensureSession captures its materialization
    // payload through captureDraftWorkspaceBinding; load the real helper
    // (matching name from the merged source) alongside the sliced functions.
    // #701 T0-c 壳化适配：prefillComposer/getComposerDraft 已委托 helpers，
    // 沙箱按 index.html 顺序补载共享基座并接好 lane 簇访问器。
    vm.runInContext('this.window = this;', ctx);
        vm.runInContext(read('src/shared/bridge-shared-helpers.js'), ctx);
        vm.runInContext(`this.pinvouSharedtauriChat = () => this.__chatCluster || (this.__chatCluster = window.PinvouBridgeShared.create("tauriChat", {
          state, sessionStates, notify: (...cbArgs) => this.notify(...cbArgs), bt: (k) => k, invoke: async () => null,
          addChatItem: (item) => { state.chatItems.push(item); },
          sendMessage: (...args) => this.sendMessage(...args),
          doSendFor: async () => true,
          flushQueued: () => {},
          getBuffer: (sid) => (sessionStates[sid] || null),
          touchSessionBuffer: () => {},
          ensureSessionBufferLoaded: async () => {},
          runSyncOnSession: (_sid, fn) => fn(),
          autoCreateScheduledTaskDraft: () => {},
          pendingAssistantBlocks: { get value() { return []; } },
          itemIdSeq: { get value() { return 0; }, set value(v) {} },
          formatAttachmentDisplayText: (t) => t,
        }));`, ctx);
        vm.runInContext('var notifyingLocalSessionNavigation = false; var localNavigationListeners = new Set();\n' + ['publishLocalSessionNavigation', 'beginLocalSessionNavigation', 'captureDraftWorkspaceBinding', 'ensureSession', 'enterDraft'].map(name => bridgeFunction(sessionSource, name)).join('\n')
      + '\n' + ['restoreTaskDraft', 'restoreSteerText', 'prefillComposer', 'getComposerDraft'].map(name => bridgeFunction(chatSource, name)).join('\n'), ctx);
    const owner = { sessionId: null, draftEpoch: 4, operationId: operation.operationId };
    const creating = ctx.ensureSession(owner);
    await toggleStarted;
    h.state.composerDraft = 'new input while waiting';
    finishToggle();
    assert.equal(await creating, null);
    ctx.restoreTaskDraft('voice task', owner);
    h.api.completeVoiceSubmission(operation.operationId, null, false);
    if (navigation === 'none') {
      assert.equal(h.state.draftEpoch, 5);
      assert.equal(owner.rollbackFromDraftEpoch, 4);
      assert.equal(operation.draftEpoch, 5);
      assert.equal(h.state.composerDraft, 'new input while waiting');
      assert.equal(h.state.composerPrefill.text, 'voice task');
      assert.equal(h.state.composerPrefill.append, true);
      assert.equal(h.api.getVoiceOperationId(null, 'chat'), operation.operationId);
      assert.equal(h.api.rebindVoiceDraftAfterRollback(operation.operationId, 4, 5), false, 'migration is compare-and-set');
      ctx.invoke = async (command, args) => { h.events.push({ command, ...args.request }); };
      h.api.beginVoiceSubmission(operation.operationId, 'retry-session');
      h.api.completeVoiceSubmission(operation.operationId, 'retry-session', true);
      assert.equal(h.events.filter(event => event.eventName === 'voice_message_submitted').length, 1);
    } else if (navigation === 'after-rollback') {
      assert.equal(operation.draftEpoch, 5, 'proven rollback stays bound when a later history refresh is navigated away from');
      assert.equal(h.state.composerDraft, 'unrelated input');
      assert.equal(h.state.composerPrefill.text, '');
      h.state.activeSessionId = null;
      h.state.composerDraft = '';
      assert.equal(ctx.getComposerDraft(), 'voice task');
      assert.equal(h.api.getVoiceOperationId(null, 'chat'), operation.operationId);
      ctx.enterDraft();
      assert.equal(h.api.getVoiceOperationId(null, 'chat'), null);
    } else {
      assert.equal(owner.rollbackFromDraftEpoch, undefined, 'real navigation cannot authorize migration');
      if (navigation === 'new-draft') {
        // enterDraft bumped the epoch: the restore lands in the live draft
        // (any-epoch branch) and the voice association follows the restored
        // text instead of being silently dropped.
        assert.equal(operation.draftEpoch, 5);
        assert.equal(h.state.composerDraft, 'unrelated input');
        assert.equal(h.state.composerPrefill.text, 'voice task');
        assert.equal(h.state.composerPrefill.append, true);
        assert.equal(h.api.getVoiceOperationId(null, 'chat'), operation.operationId,
          'the association follows the restored text into the live draft');
      } else {
        assert.equal(operation.draftEpoch, 4);
        assert.equal(h.state.composerDraft, 'unrelated input');
        assert.equal(h.state.composerPrefill.text, '');
        h.state.activeSessionId = null;
        h.state.draftEpoch = 5;
        assert.equal(h.api.getVoiceOperationId(null, 'chat'), null, 'new draft remains isolated');
      }
    }
  });
}

for (const lane of ['desktop', 'web']) {
  for (const outcome of ['restored', 'false', 'throw', 'switched', 'draft-switched', 'draft-failed-switched',
    'capability-switched', 'draft-capability-switched', 'draft-epoch-switched']) {
    for (const typeDuringAwait of [false, true]) {
      test(`${lane}: real voice writeback/send/prefill ${outcome}, typing=${typeDuringAwait}, restores exactly once`, async () => {
        const h = harness(sources[lane]);
        const chat = read('src/features/chat/ChatView.jsx');
        const hook = read('src/features/voice-composer/useComposerVoiceInput.js');
        const sendSource = lane === 'desktop' ? read('src/platform/tauri/bridge/chat.js') : sources.web;
        const origin = outcome === 'restored' || outcome.startsWith('draft-') ? null : 'original-session';
        const switching = outcome.includes('switched');
        const preparing = outcome.includes('capability');
        const createdInBackground = outcome === 'draft-switched' || outcome === 'draft-epoch-switched';
        const buffers = { 'original-session': { composerDraft: '', remoteTurnActive: outcome === 'switched' },
          'other-session': { composerDraft: 'other draft' } };
        Object.assign(h.state, { activeSessionId: origin, composerDraft: '', draftEpoch: 4,
          composerPrefill: { id: 0, text: '' }, queued: [], chatItems: [], messages: [], sessions: [], pinvouReviews: [],
          attachments: outcome === 'false' ? [{ status: 'parsing' }] : [] });
        const operation = h.operation('voiceop-real-recovery', 'chat', origin);
        let unblock;
        const gate = new Promise(resolve => { unblock = resolve; });
        let prefillPending = false;
        let draftSyncPending = false;
        let accepted = 0;
        const ctx = h.context;
        Object.assign(ctx, {
          sessionStates: buffers, voice: () => h.api, IS_WEB: lane === 'web', canInvoke: () => false,
          useCallback: fn => fn, useEffect: fn => fn(), activeSessionId: origin,
          activeSessionIdRef: { current: origin }, draftEpoch: 4, draftEpochRef: { current: 4 }, inputTextRef: { current: '' },
          pendingTaskDraftRecovery: { buffer: null }, sessionSwitchRequestToken: 0,
          findPersonalWorkbenchTemplateDraft: () => null, personalWorkbenchTemplateIdRef: { current: null },
          pendingModeScopeMigrationRef: { current: null }, pinvouModeStateRef: { current: null },
          attachments: h.state.attachments, hasReadyAttachment: false,
          visualPosterSceneActive: false, documentWritingSceneActive: preparing,
          personalWorkbenchSceneActive: false, dataVisualizationSceneActive: false, pptDesignSceneActive: false,
          controlledChatActive: false, controlledLinkReady: false,
          controlledCapabilityBound: false, controlledCapabilityBusy: false, controlledSkillIds: [],
          controlledSession: { sessionId: null },
          remoteChatActive: false, remoteConnection: null, remoteLinkReady: false,
          remoteSessionBusy: false,
          remoteSendIdentity: null, remoteTransferGuard: null, effectiveRemoteModelSource: null, remoteBinding: null,
          remoteFiles: [], remoteNewDispatchReady: false, remoteProxyHistory: null,
          remoteModelError: null, remoteModelSource: 'controlled_device',
          remoteMode: 'chat', remoteSkillIds: [], remoteWorkspaceId: '',
          selectedRemoteModelId: null,
          isRemoteProxyView: () => false,
          isKnownRemoteProxy: () => false,
          createDocumentWritingMessageMeta: () => ({ pinvouScene: 'work:document-writing' }),
          requiredCapabilitiesForMeta, prepareSceneCapabilities, canPrepareSceneCapabilities: () => true,
          isWeb: lane === 'web', can: () => true, setSceneCapabilityStatus() {},
          // #455 backflow: the merged send path reads the welcome-opt-in copy
          // and the opt-in outcome copy keys; the sandbox pins them alongside
          // the scene copy it already provided.
          t: { uiChatScenes: { documentWriting: { preparing: 'preparing', failure: 'failed' },
            switchedOffPacks: ids => `switched off: ${ids}`,
            notAppliedPacks: ids => `not applied: ${ids}`,
            missingCapabilities: ids => `missing: ${ids}` },
            uiChat: { welcomeOptInFailed: 'welcome opt-in failed' } },
          invokeTauri: async command => { await gate; return command === 'list_marketplace_tools'
            ? [{ id: 'gongwen', installed: true }] : [{ id: 'government-writing', installed: true }]; },
          setInputTextState() {}, setInputLimitReached() {}, constrainChatInput: text => ({ text: String(text), truncated: false }),
          bt: key => key, addSystemItem() {}, getBuffer: sid => buffers[sid], isBusyFor: () => false,
          formatAttachmentDisplayText: text => { if (outcome === 'throw') throw new Error('controlled send failure'); return text; },
          adoptManagedAttachments: () => outcome === 'switched' ? gate : Promise.resolve(), discardManagedAttachment() {},
          reconcileRemoteTurn: async () => { await gate; return false; },
          ensureSession: async () => { await gate; return null; },
          notify: () => { prefillPending ||= !!h.state.composerPrefill.text; draftSyncPending = true; },
          console: { warn() {} }, setTimeout() {}, composerRef: { current: null },
          // #455 backflow: the merged dispatchChatMessage wires the welcome
          // opt-in and session-keyed scene status; the sandbox pins those
          // bindings so the slice stays runnable without the module graph
          // (neutral stubs: opt-in succeeds, status resolution shows nothing).
          welcomeOptInAttemptRef: { current: null },
          welcomeToolIdRef: { current: null },
          setWelcomeToolId() {},
          sceneReadyToastEpochRef: { current: 0 },
          sceneStatusKey: (sid, epoch) => `${sid || ''}:${epoch || ''}`,
          runSharedWelcomeOptIn: async () => ({ failed: false, blocked: [] }),
          resolveSendCapabilityStatus: () => null,
          consumeWelcomeOptIn: async () => ({}),
          onPrefillConsumed: () => { h.state.composerPrefill.text = ''; },
          normalizeMode: mode => mode, trimDraft: text => String(text || '').trim(),
          clearStaleVoiceState() {}, discardVoiceResult() {}, isActiveVoiceTarget: () => true,
          taskSendInFlightRef: { current: false },
        });
        injectChatViewMentionScope(ctx, chat);
        ctx.__vdbg = [];
        // Official 适配：上游 #586 的恢复族依赖 stripMentionBlockForComposerRestore
        // 与块感知信封；#701 T0-c 壳化后信封/队列簇委托 helpers，沙箱按
        // index.html 顺序补载共享基座并接好 lane 簇访问器，lane 壳与
        // helpers 谓词体一起提取（queuedPayloadEnvelope 本体已不在
        // chat.js，改从 helpers 提取）。
        vm.runInContext('this.window = this;', ctx);
        vm.runInContext(read('src/shared/bridge-shared-helpers.js'), ctx);
        vm.runInContext(`this.pinvouSharedtauriChat = () => this.__chatCluster || (this.__chatCluster = window.PinvouBridgeShared.create("tauriChat", {
          state, sessionStates, notify: (...cbArgs) => this.notify(...cbArgs), bt: (k) => k, invoke: async () => null,
          addChatItem: (item) => { state.chatItems.push(item); },
          sendMessage: (...args) => this.sendMessage(...args),
          doSendFor: (...args) => this.doSendFor(...args),
          flushQueued: (...args) => this.flushQueued(...args),
          getBuffer: (sid) => (sessionStates[sid] || null),
          touchSessionBuffer: () => {},
          ensureSessionBufferLoaded: async () => {},
          runSyncOnSession: (_sid, fn) => fn(),
          autoCreateScheduledTaskDraft: () => {},
          pendingAssistantBlocks: { get value() { return []; } },
          itemIdSeq: { get value() { return 0; }, set value(v) {} },
          formatAttachmentDisplayText: (t) => t,
        }));`, ctx);
        // #701 T4a 壳化适配：web lane 的 setComposerDraft/prefillComposer/
        // stripMentionBlockForComposerRestore/makeQueuedMessage/rebuiltQueued*
        // 已委托共享基座；voice 切片簇（harness 内）依赖集不覆盖发送族，
        // 这里按 index.html 顺序另接一个发送簇（惰性创建，此时 ctx 的
        // sessionStates/itemIdSeq 等已就位），并把六个壳名直接绑到该簇。
        vm.runInContext(`this.pinvouSharedwebSend = () => this.__webClusterSend || (this.__webClusterSend = window.PinvouBridgeShared.create("web", {
          state, sessionStates, notify: (...cbArgs) => this.notify(...cbArgs), bt: { get value() { return (k) => k; } }, invoke: async () => null,
          addChatItem: (item) => { state.chatItems.push(item); },
          sendMessage: (...args) => this.sendMessage(...args),
          doSendFor: (...args) => this.doSendFor(...args),
          flushQueued: (...args) => this.flushQueued(...args),
          getBuffer: (sid) => (sessionStates[sid] || null),
          touchSessionBuffer: () => {},
          ensureSessionBufferLoaded: async () => {},
          runSyncOnSession: (_sid, fn) => fn(),
          autoCreateScheduledTaskDraft: () => {},
          pendingAssistantBlocks: { get value() { return []; } },
          itemIdSeq: { get value() { return 0; }, set value(v) {} },
          formatAttachmentDisplayText: (t) => t,
        }));`, ctx);
        const functions = ['getComposerDraft', 'restoreTaskDraft', 'sendMessage',
          lane === 'desktop' ? 'setComposerDraft' : 'restoreComposerText'];
        if (lane === 'desktop') functions.push('prefillComposer', 'stripMentionBlockForComposerRestore', 'makeQueuedMessage',
          'rebuiltQueuedPayload', 'rebuiltQueuedMetaPayload', 'restoreSteerText');
        if (lane === 'web') {
          vm.runInContext(['setComposerDraft', 'prefillComposer', 'stripMentionBlockForComposerRestore',
            'makeQueuedMessage', 'rebuiltQueuedPayload', 'rebuiltQueuedMetaPayload']
            .map(name => `this.${name} = (...args) => this.pinvouSharedwebSend().${name}(...args);`).join('\n'), ctx);
        }
        if (lane === 'web') functions.push('restoreAbortedMaterialization', 'maybeSubmitFirstWebTurn');
        vm.runInContext(functions.map(name => bridgeFunction(sendSource, name)).join('\n'), ctx);
        // 信封函数经簇访问（其内部的 mention 拆分依赖注入的 window 域），
        // 不再单独提取 helpers 谓词体。
        vm.runInContext('this.queuedPayloadEnvelope = (...args) => (lane === \'desktop\' ? this.pinvouSharedtauriChat() : this.pinvouSharedweb()).queuedPayloadEnvelope(...args);', ctx);
        const sentTargets = [];
        ctx.doSendFor = async sid => { sentTargets.push(sid); return true; };
        if (outcome.startsWith('draft-') && !preparing) {
          const sessionSource = lane === 'desktop' ? read('src/platform/tauri/bridge/sessions.js') : sources.web;
          Object.assign(ctx, { ensureSessionInFlight: null, ensureSessionDraftOutcome: null,
            freshBuffer: () => ({ composerDraft: '' }),
            invoke: async () => { await gate; if (outcome === 'draft-failed-switched') throw new Error('create failed');
              return { id: 'created-session' }; },
          });
          // #484 backflow: the desktop ensureSession captures its materialization
          // payload through captureDraftWorkspaceBinding and, on the
          // create-failure path, prunes workspace recents via
          // maybePruneFailedWorkspaceRecent; load the real helpers (matching
          // names from the merged source) beside the sliced function.
          vm.runInContext((lane === 'desktop'
            ? 'var notifyingLocalSessionNavigation = false; var localNavigationListeners = new Set();\n' + ['publishLocalSessionNavigation', 'beginLocalSessionNavigation', 'captureDraftWorkspaceBinding', 'maybePruneFailedWorkspaceRecent'].map(name => bridgeFunction(sessionSource, name)).join('\n') + '\n'
            : '') + bridgeFunction(sessionSource, 'ensureSession'), ctx);
        }
        const bridgeTargets = [];
        ctx.bridge = { available: true, voice: h.api, chat: {
          sendMessage: (...args) => { bridgeTargets.push(h.state.activeSessionId); return ctx.sendMessage(...args); },
          setComposerDraft: ctx.setComposerDraft, restoreTaskDraft: ctx.restoreTaskDraft,
        } };
        const setStart = chat.indexOf('      const setInputText = useCallback(');
        const setEnd = chat.indexOf('\n      const [artifactsOpen', setStart);
        const sendStart = chat.indexOf('      const sendChatMessage = useCallback(');
        const sendEnd = chat.indexOf('\n      // ConversationTimeline', sendStart);
        const dispatchStart = chat.indexOf('      const dispatchChatMessage = useCallback(');
        const dispatchEnd = chat.indexOf('      // SessionStore mapping/lease establishment', dispatchStart);
        const taskStart = chat.indexOf('        sendTask: async (outgoing, context) => {');
        const taskEnd = chat.indexOf('\n        onTaskAccepted:', taskStart);
        const task = chat.slice(taskStart, taskEnd).trim().replace(/^sendTask:/, 'this.voiceTask =').replace(/,$/, ';');
        vm.runInContext(`${chat.slice(setStart, setEnd)}\n${chat.slice(dispatchStart, dispatchEnd)}\n${chat.slice(sendStart, sendEnd)}\n${task}\nthis.setInput = setInputText;`, ctx);
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
        const __origRTD = ctx.restoreTaskDraft;
        const sending = ctx.writeback('voice-session', 'voice task', '', { mode: 'task', operationId: operation.operationId })
          .then(() => 'accepted', error => error.category);
        assert.equal(ctx.inputTextRef.current, '', 'owned writeback draft must be consumed before awaiting bridge');
        if (switching) {
          h.state.activeSessionId = 'other-session';
          ctx.activeSessionIdRef.current = 'other-session';
          ctx.sessionSwitchRequestToken += 1;
          if (outcome === 'draft-epoch-switched') { h.state.draftEpoch += 1; ctx.draftEpochRef.current += 1; }
          ctx.setInput('other draft');
        }
        if (typeDuringAwait) ctx.setInput(switching ? 'other new input' : 'new input');
        unblock();
        assert.equal(await sending, 'send_failed');
        console.error('VDBG', JSON.stringify(ctx.__vdbg || []), 'composerDraft=', JSON.stringify(h.state.composerDraft), 'buffer=', JSON.stringify(buffers));
        // Consume the actual ChatView prefill effect, not a replacement append stub.
        if (prefillPending) {
          ctx.prefill = h.state.composerPrefill.text;
          ctx.prefillAppend = h.state.composerPrefill.append;
          const prefillCondition = chat.indexOf('        if (prefill)');
          const effectStart = chat.lastIndexOf('      useEffect(() => {', prefillCondition);
          const effectEnd = chat.indexOf('}, [prefill, prefillAppend]);', effectStart) + '}, [prefill, prefillAppend]);'.length;
          vm.runInContext(chat.slice(effectStart, effectEnd), ctx);
        } else if (draftSyncPending) {
          ctx.setInput(h.state.composerDraft);
        }
        const expected = switching ? (typeDuringAwait ? 'other new input' : 'other draft')
          : typeDuringAwait ? 'new input\nvoice task' : 'voice task';
        assert.equal(ctx.inputTextRef.current, expected);
        if (switching && origin) assert.equal(buffers['original-session'].composerDraft, 'voice task');
        if (createdInBackground) {
          assert.equal(buffers['created-session'].composerDraft, 'voice task');
          assert.equal(operation.sessionId, 'created-session');
        }
        assert.deepEqual(sentTargets, [], 'ownership invalidation cannot admit a turn in B');
        if (preparing) assert.deepEqual(bridgeTargets, [], 'stale capability preparation must not enter sendMessage at all');
        if (outcome === 'draft-failed-switched' || outcome === 'draft-capability-switched') {
          assert.equal(ctx.getComposerDraft(), expected, 'reading B cannot consume the pending draft');
          h.state.activeSessionId = null;
          h.state.composerDraft = 'draft new input';
          assert.equal(ctx.getComposerDraft(), 'draft new input\nvoice task');
          assert.equal(ctx.getComposerDraft(), 'draft new input\nvoice task', 'same epoch recovery is once-only');
          assert.equal(h.api.getVoiceOperationId(null, 'chat'), operation.operationId);
          const owner = { sessionId: null, draftEpoch: 4 };
          h.state.activeSessionId = 'other-session';
          ctx.restoreTaskDraft('stale draft', owner);
          h.state.draftEpoch += 1;
          h.state.activeSessionId = null;
          h.state.composerDraft = 'fresh draft';
          // No epoch gate: re-entering the draft always allocates a new epoch
          // (enterDraft increments unconditionally), so the slot hands its
          // retained text back on the next draft return — the one chance
          // before it would otherwise be silently dropped.
          assert.equal(ctx.getComposerDraft(), 'fresh draft\nstale draft');
          assert.equal(ctx.getComposerDraft(), 'fresh draft\nstale draft', 'recovery is once-only');
          assert.equal(h.api.getVoiceOperationId(null, 'chat'), null, 'new epoch cannot inherit retained voice provenance');
        }
        assert.equal(accepted, 0, 'rejected/restored task must not clear the restored draft or claim success');
      });
    }
  }
}

// ── Scoped task-draft restore + departed-draft rebind (real bridge code):
// retention/consumption/settlement pins aligned with the upstream restore
// restructure (any-epoch draft branch, unconditional retain merge) ──

function restoreHarness(source, lane, stateOverrides, boundSessionId = null) {
  const state = { activeSessionId: null, draftEpoch: 4, composerDraft: '', ...stateOverrides };
  const sandbox = { console, Math, Date, Promise, Object, Array, JSON, String, Number, Boolean };
  vm.createContext(sandbox);
  sandbox.state = state;
  const calls = { complete: [], steer: [], restore: [], prefill: [], rebind: [] };
  sandbox.completeVoiceSubmission = (operationId, sessionId, accepted) => calls.complete.push({ operationId, sessionId, accepted });
  sandbox.rebindVoiceOperationToDraft = (operationId, toEpoch) => calls.rebind.push({ operationId, toEpoch });
  sandbox.voiceOperationSessionId = () => boundSessionId;
  sandbox.voice = () => ({
    rebindVoiceDraftAfterRollback: () => false,
    completeVoiceSubmission: (operationId, sessionId, accepted) => calls.complete.push({ operationId, sessionId, accepted }),
    voiceOperationSessionId: () => boundSessionId,
    rebindVoiceOperationToDraft: (operationId, toEpoch) => calls.rebind.push({ operationId, toEpoch }),
  });
  if (lane === 'desktop') sandbox.restoreSteerText = (sid, text) => {
    calls.steer.push({ sid, text });
    return true;
  };
  else sandbox.restoreComposerText = (sid, text) => calls.restore.push({ sid, text });
  sandbox.prefillComposer = (text, append) => calls.prefill.push({ text, append });
  const declStart = source.indexOf('  const pendingTaskDraftRecovery = { buffer: null };');
  assert.ok(declStart >= 0, `${lane} bridge must keep the retained recovery slot declaration`);
  // #701 T0-c 壳化适配：截取区段内的 setComposerDraft 等已委托 helpers，
  // 沙箱按 index.html 顺序补载共享基座并接好 lane 簇访问器。
  vm.runInContext('this.window = this;', sandbox);
  vm.runInContext(read('src/shared/bridge-shared-helpers.js'), sandbox);
  vm.runInContext(`this.pinvouSharedtauriChat = () => this.__chatCluster || (this.__chatCluster = window.PinvouBridgeShared.create("tauriChat", {
    state, notify: () => {}, bt: (k) => k, invoke: async () => null,
    addChatItem: () => {}, sendMessage: () => {}, doSendFor: async () => true,
    flushQueued: () => {}, getBuffer: () => null, touchSessionBuffer: () => {},
    ensureSessionBufferLoaded: async () => {}, runSyncOnSession: (_sid, fn) => fn(),
    autoCreateScheduledTaskDraft: () => {},
    pendingAssistantBlocks: { get value() { return []; } },
    itemIdSeq: { get value() { return 0; }, set value(v) {} },
    formatAttachmentDisplayText: (t) => t,
  }));`, sandbox);
  vm.runInContext(
    source.slice(declStart, source.indexOf('  function getComposerDraft(', declStart))
    + '\n' + bridgeFunction(source, 'getComposerDraft')
    + '\n' + bridgeFunction(source, 'restoreTaskDraft')
    + '\nthis.getComposerDraft = getComposerDraft;\nthis.restoreTaskDraft = restoreTaskDraft;',
    sandbox,
  );
  return { sandbox, state, calls };
}

for (const [lane, source] of Object.entries(sources)) {
  // The restore block lives in the desktop chat bridge and the web bridge;
  // sources[lane] (voice.js) only carries the operation lifecycle slice.
  const restoreSource = lane === 'desktop' ? read('src/platform/tauri/bridge/chat.js') : source;

  test(`${lane}: restore: a rejected send with an active session retains once, settles unparked, and consumes on draft return`, () => {
    const { sandbox, state, calls } = restoreHarness(restoreSource, lane, { activeSessionId: 'session-b', draftEpoch: 5 });
    const owner = { sessionId: null, draftEpoch: 4, operationId: 'voiceop-restore', restored: false };
    assert.equal(sandbox.restoreTaskDraft('dictated text', owner), true);
    assert.deepEqual(calls.prefill, [], 'the text must never land in the unrelated active composer');
    assert.deepEqual(calls.complete, [{ operationId: 'voiceop-restore', sessionId: null, accepted: false }], 'the restore settles the parked submission');
    // Returning to the draft allocates a NEW epoch (enterDraft increments
    // unconditionally) — the retained text must still be consumed exactly once.
    state.activeSessionId = null;
    state.draftEpoch = 7;
    assert.equal(sandbox.getComposerDraft(), 'dictated text');
    assert.equal(state.composerDraft, 'dictated text', 'the retained text lands in the composer draft');
    assert.deepEqual(calls.rebind, [{ operationId: 'voiceop-restore', toEpoch: 7 }], 'the voice association follows the text into the new epoch');
    state.composerDraft = '';
    assert.equal(sandbox.getComposerDraft(), '', 'the retained draft is consumed once');
    assert.equal(sandbox.restoreTaskDraft('again', owner), false, 'the restore is once-only');
  });

  test(`${lane}: restore: back in the draft it prefills directly regardless of the epoch`, () => {
    const { sandbox, calls } = restoreHarness(restoreSource, lane, { activeSessionId: null, draftEpoch: 9 });
    const owner = { sessionId: null, draftEpoch: 4, operationId: 'voiceop-prefill', restored: false };
    assert.equal(sandbox.restoreTaskDraft('dictated text', owner), true);
    assert.deepEqual(calls.prefill, [{ text: 'dictated text', append: true }]);
    assert.deepEqual(calls.complete, [{ operationId: 'voiceop-prefill', sessionId: null, accepted: false }]);
    // The text is visibly back in the live draft: the operation must follow it
    // to the new epoch, or a manual retry is orphaned in the dead epoch.
    assert.deepEqual(calls.rebind, [{ operationId: 'voiceop-prefill', toEpoch: 9 }]);
  });

  test(`${lane}: restore: stranded drafts merge unconditionally across epochs, newest operation wins`, () => {
    const { sandbox, state, calls } = restoreHarness(restoreSource, lane, { activeSessionId: 'session-b', draftEpoch: 5 });
    const first = { sessionId: null, draftEpoch: 4, operationId: 'voiceop-first', restored: false };
    assert.equal(sandbox.restoreTaskDraft('first strand', first), true);
    // The epoch moved on between the two strands: the merge must keep the
    // earlier text (an epoch-keyed merge would silently replace it).
    const second = { sessionId: null, draftEpoch: 7, operationId: 'voiceop-second', restored: false };
    assert.equal(sandbox.restoreTaskDraft('second strand', second), true);
    state.activeSessionId = null;
    state.draftEpoch = 9;
    assert.equal(sandbox.getComposerDraft(), 'first strand\nsecond strand');
    assert.deepEqual(calls.rebind, [{ operationId: 'voiceop-second', toEpoch: 9 }], 'the newest operation wins');
    state.composerDraft = '';
    assert.equal(sandbox.getComposerDraft(), '', 'consume-once after the merged hand-back');
  });

  test(`${lane}: a draft-return rebind re-adopts the restored operation, and only that shape`, () => {
    const h = harness(source);
    const operation = h.operation('voiceop-departed');
    h.state.draftEpoch = 9;
    assert.equal(h.api.getVoiceOperationId(null, 'chat'), null, 'the dead recording-time epoch cannot adopt');
    assert.equal(h.api.rebindVoiceOperationToDraft(operation.operationId, 9), true);
    assert.equal(operation.draftEpoch, 9);
    assert.equal(h.api.getVoiceOperationId(null, 'chat'), operation.operationId, 'the restored text manual retry keeps its provenance');
    // Refusals: a terminal operation, a session-bound operation, a non-chat operation.
    h.api.trackVoiceTerminal('voice_cancelled', operation);
    assert.equal(h.api.rebindVoiceOperationToDraft(operation.operationId, 9), false, 'terminal operations never rebind');
    const bound = h.operation('voiceop-bound', 'chat', 'session-a');
    assert.equal(h.api.rebindVoiceOperationToDraft(bound.operationId, 9), false, 'session-bound operations never rebind');
    const acp = h.operation('voiceop-acp', 'acp', null);
    assert.equal(h.api.rebindVoiceOperationToDraft(acp.operationId, 9), false, 'non-chat operations never rebind');
  });

  test(`${lane}: dismissing the finished notice keeps the operation until the next recording`, () => {
    const h = harness(source);
    const operation = h.operation('voiceop-dismissed');
    h.state.voiceInput = { status: 'idle', operationId: operation.operationId };
    h.api.dismissVoiceInput();
    assert.equal(operation.telemetryTerminal, false, 'dismissing must not end the unsent operation');
    assert.equal(operation.dismissed, true, 'the dismissal is marked so the next start can sweep it');
    assert.equal(h.api.getVoiceOperationId(null, 'chat'), operation.operationId, 'a dismissed operation stays adoptable by a manual send');
    const next = { operationId: 'voiceop-next', ownerKind: 'chat', sessionId: null, telemetrySessionId: 'draft-session',
      startedAt: Date.now(), voiceResultReady: true, telemetryTerminal: false };
    h.api.rememberVoiceOperation(next);
    assert.equal(h.api.getVoiceOperationId(null, 'chat'), 'voiceop-next', 'the next recording start sweeps the never-sent dismissal');
  });
}

test('desktop: actual session restore refuses missing buffers and failed remote persistence without claiming success', () => {
  const chatSource = read('src/platform/tauri/bridge/chat.js');
  for (const remote of [false, true]) {
    const { sandbox, state, calls } = restoreHarness(chatSource, 'desktop', {
      activeSessionId: remote ? 'remote-session' : 'other-session',
    });
    sandbox.sessionStates = {};
    sandbox.isRemoteProxyView = () => remote;
    sandbox.isKnownRemoteProxy = () => remote;
    let writable = false;
    sandbox.remoteProxyDrafts = { get: () => '', set: () => writable };
    sandbox.setComposerDraft = value => { state.composerDraft = value; };
    sandbox.notify = () => {};
    vm.runInContext(
      // Official 适配：合并后的 restoreSteerText 依赖 #586 的
      // stripMentionBlockForComposerRestore（chat.js 本体内联实现），
      // 随恢复族一并提取进沙箱（window 全局缺省时原样返回）。
      bridgeFunction(chatSource, 'stripMentionBlockForComposerRestore') + '\n'
        + bridgeFunction(chatSource, 'restoreSteerText'),
      sandbox,
    );
    const owner = { sessionId: remote ? 'remote-session' : 'missing-session',
      draftEpoch: 4, operationId: 'voiceop-refused', restored: false };
    assert.equal(sandbox.restoreTaskDraft('recoverable text', owner), false);
    assert.equal(owner.restored, false, 'an unsuccessful restore must remain retryable');
    assert.equal(state.composerDraft, '', 'no text may enter an unrelated or unpersisted composer');
    assert.deepEqual(calls.prefill, []);
    assert.deepEqual(calls.rebind, []);
    assert.deepEqual(calls.complete, [{ operationId: 'voiceop-refused', sessionId: owner.sessionId, accepted: false }]);
    if (remote) {
      assert.equal(state.remoteProxyDraftError, 'remote-session');
      writable = true;
      assert.equal(sandbox.restoreTaskDraft('recoverable text', owner), true);
      assert.equal(owner.restored, true);
      assert.equal(state.composerDraft, 'recoverable text');
      assert.equal(sandbox.restoreTaskDraft('duplicate', owner), false, 'successful retry consumes recovery once');
    }
  }
});

// ── Web enterDraft 收尾（移植上游真切片执行用例）：已完成的首轮提交在
// enterDraft 时就地关闭并丢弃记录，避免搁浅操作永远不可认领/清扫；
// 仍在途的提交留给自己的 RPC 结算（在此关闭会把即将送达的消息标成取消）──
test('web: enterDraft closes the operation of a finished first-turn submission and leaves an in-flight one to its RPC', () => {
  const webSource = sources.web;
  const start = webSource.indexOf('  function enterDraft() {');
  const end = blockEnd(webSource, start, 'createNewSession');
  assert.ok(start >= 0 && end > start, 'web bridge must keep the enterDraft function');
  const state = {
    chatItems: [{ clientMessageId: 'c-finished' }, { clientMessageId: 'c-inflight' }, { clientMessageId: 'c-novoice' }],
    activeSessionId: 'web-session',
    messages: [{ id: 'm1' }],
    draftEpoch: 4,
    composerDraft: '',
    scheduledRunContext: null,
    scheduledTaskPendingGuide: null,
    modeState: null,
  };
  const sandbox = { console, Math, Date, Promise, Object, Array, JSON, String, Number, Boolean };
  vm.createContext(sandbox);
  sandbox.state = state;
  sandbox.notify = () => {};
  const calls = { complete: [], abandoned: [] };
  sandbox.firstTurnSubmissions = Object.create(null);
  sandbox.firstTurnSubmissions['c-finished'] = { voiceOperationId: 'voiceop-finished', inFlight: false };
  sandbox.firstTurnSubmissions['c-inflight'] = { voiceOperationId: 'voiceop-inflight', inFlight: true };
  sandbox.sessionSwitchRequestToken = 0;
  sandbox.completeVoiceSubmission = (operationId, sessionId, accepted) => calls.complete.push({ operationId, sessionId, accepted });
  sandbox.abandonVoiceResult = (operationId) => calls.abandoned.push(operationId);
  sandbox.saveWorkingSetTo = () => {};
  sandbox.loadWorkingSetFrom = () => {};
  sandbox.freshBuffer = () => ({});
  sandbox.getBuffer = () => ({});
  sandbox.currentDraftModeState = () => 'chat';
  vm.runInContext(`${webSource.slice(start, end)}\nthis.enterDraft = enterDraft;`, sandbox);
  sandbox.enterDraft();
  assert.deepEqual(
    calls.complete,
    [{ operationId: 'voiceop-finished', sessionId: null, accepted: false }],
    "a finished submission's voice operation is un-parked and closed",
  );
  assert.deepEqual(calls.abandoned, ['voiceop-finished'], 'the closed operation is abandoned outright');
  assert.equal('c-finished' in sandbox.firstTurnSubmissions, false, 'the submission record is discarded');
  assert.equal('c-inflight' in sandbox.firstTurnSubmissions, false, 'the in-flight record is discarded too');
  assert.equal(
    calls.complete.some((call) => call.operationId === 'voiceop-inflight'),
    false,
    'an in-flight submission is left to its own RPC settlement (closing it here would mark a delivered message cancelled)',
  );
  assert.equal(state.draftEpoch, 5, 'enterDraft still advances the draft epoch');
});

// ── Abandon-on-clear (backflow pinvou-agent#789): cleared drafts must not receive late results ──

function abandonHarness(lane) {
  const source = sources[lane];
  const fnStart = source.indexOf("  function abandonCompletedVoiceResult(");
  assert.ok(fnStart >= 0, `${lane} bridge must keep abandonCompletedVoiceResult`);
  const bodyEnd = source.indexOf("\n  }\n", fnStart);
  assert.ok(bodyEnd > fnStart, `${lane} abandonCompletedVoiceResult must stay a self-contained slice`);
  const terminalCalls = [];
  const sandbox = {
    voiceOperations: new Map(), state: {},
    trackVoiceTerminal(reason, operation) { terminalCalls.push(reason); operation.telemetryTerminal = true; },
    // Official adaptation: this repo's slice also stamps durationMs through the
    // lane-local helper; the sandbox stubs it so the slice stays runnable.
    voiceDurationMs() { return 0; },
  };
  sandbox.window = sandbox;
  vm.createContext(sandbox);
  vm.runInContext(`${source.slice(fnStart, bodyEnd + 6)}\nthis.abandonCompletedVoiceResult = abandonCompletedVoiceResult;`, sandbox);
  return { sandbox, terminalCalls };
}

test("both lanes: a result recorded for another session survives clearing the viewed draft", () => {
  for (const [lane, harness] of [["desktop", abandonHarness("desktop")], ["web", abandonHarness("web")]]) {
    const { sandbox, terminalCalls } = harness;
    const operation = {
      operationId: "voiceop_cross", sessionId: "session-a", ownerKind: "chat",
      pendingSubmission: false, telemetryTerminal: false, voiceResultReady: true,
    };
    sandbox.voiceOperations.set("voiceop_cross", operation);
    sandbox.state = { activeSessionId: "session-b", voiceInput: { status: "completed", operationId: "voiceop_cross" } };
    sandbox.abandonCompletedVoiceResult();
    assert.equal(terminalCalls.length, 0, `${lane} a cross-session clear must not touch the foreign operation`);
    assert.equal(operation.telemetryTerminal, false, `${lane} a cross-session result must not be terminated by another session's clear`);
    assert.equal(sandbox.state.voiceInput.operationId, "voiceop_cross", `${lane} the foreign result keeps its notice binding`);
    // The fix must not over-block: clearing the owning session's draft still terminates.
    sandbox.state.activeSessionId = "session-a";
    sandbox.abandonCompletedVoiceResult();
    assert.equal(operation.telemetryTerminal, true, `${lane} a same-session clear still terminates the result`);
  }
});

test("both lanes: the terminal latch gates the write-back itself, not only the completed notice", () => {
  for (const [lane, source] of [["desktop", sources.desktop], ["web", sources.web]]) {
    const finishStart = source.indexOf("async function finishVoiceInput(");
    assert.ok(finishStart >= 0, `${lane} bridge must keep finishVoiceInput`);
    const writebackCall = source.indexOf("await session.writeback(", finishStart);
    assert.ok(writebackCall > finishStart, `${lane} finishVoiceInput must call the write-back`);
    const gate = source.lastIndexOf("if (session.telemetryTerminal) {", writebackCall);
    assert.ok(gate > finishStart, `${lane} must check the terminal latch before the write-back so an abandoned result never lands in the cleared draft`);
    const idleReset = source.indexOf('setVoiceInputStatus("idle"', gate);
    assert.ok(idleReset > gate && idleReset < writebackCall, `${lane} abandoning mid-pipeline must reset the active status instead of leaving the pill stuck`);
  }
});
