import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';
import { validateBehaviorEventShape } from '../../remote-control-relay/behavior-telemetry-schema.js';

const root = path.resolve(import.meta.dirname, '..');
const read = file => fs.readFileSync(path.join(root, file), 'utf8');
const sources = {
  desktop: read('src/platform/tauri/bridge/voice.js'),
  web: read('src/platform/web/bridge.js'),
};

// Execute the real operation/telemetry helpers and startVoiceInput branch.
// ASR status and UI/media side effects are controlled; no desktop/WebView IPC,
// microphone, installer or online recognizer is exercised by this harness.
function harness(source) {
  const requests = [];
  let installs = 0;
  let readiness;
  const state = { activeSessionId: 'session_voice_preflight', voiceInput: {}, voiceAsrSetup: {} };
  const context = vm.createContext({
    state, Date, Math, Map, IS_WEB: true, activeVoiceInput: null,
    notify() {}, bt: key => key, normalizeVoiceMode: mode => mode,
    emitVoiceDiagnostic() {}, cleanupVoiceInputSession() {},
    installVoiceAsr() { installs += 1; },
    setVoiceInputStatus(status, patch) { state.voiceInput = { ...state.voiceInput, ...patch, status }; },
    invoke: async (command, args) => {
      if (command === 'voice_asr_status') return readiness;
      if (command.endsWith('track_behavior_event')) requests.push(JSON.parse(JSON.stringify(args.request)));
    },
  });
  vm.runInContext('this.window = this;', context);
  vm.runInContext(read('src/shared/bridge-shared-helpers.js'), context);
  vm.runInContext(`
    const sharedContext = {
      state, notify, invoke,
      activeVoiceInput: { get value() { return activeVoiceInput; }, set value(v) { activeVoiceInput = v; } },
      voiceOperations: { get value() { return voiceOperations; } },
      stopMediaTracks() {}, bt: key => key, VOICE_DEVICE_REQUEST_TIMEOUT_MS: 8000, finishVoiceInput() {},
    };
    this.pinvouSharedtauriVoice = () => this.desktopHelpers || (this.desktopHelpers = window.PinvouBridgeShared.create('tauriVoice', sharedContext));
    this.pinvouSharedweb = () => this.webHelpers || (this.webHelpers = window.PinvouBridgeShared.create('web', sharedContext));
  `, context);
  const helpersStart = source.indexOf('  function voiceToken(');
  const helpersEnd = /\n(?:  )?function setVoiceInputStatus\(/.exec(source.slice(helpersStart));
  assert.ok(helpersStart >= 0 && helpersEnd);
  vm.runInContext(source.slice(helpersStart, helpersStart + helpersEnd.index), context);
  const start = source.indexOf('  async function startVoiceInput(');
  const end = /\n(?:  )?function cancelVoiceInput\(/.exec(source.slice(start));
  assert.ok(start >= 0 && end);
  vm.runInContext(`${source.slice(start, start + end.index)}\nthis.start = startVoiceInput; this.terminal = trackVoiceTerminal;`, context);
  return {
    requests, state, context, installs: () => installs,
    async start(status) {
      readiness = status;
      await context.start('private draft never uploaded', () => {}, { mode: 'dictation' });
      await Promise.resolve();
    },
  };
}

function assertReceiverAccepts(request) {
  // Transport shape is camelCase at the bridge and snake_case in BehaviorEvent.
  // Only the standard envelope is supplied here; every event field/value comes
  // from the actual producer request above, not an independently invented fixture.
  const event = {
    schema_version: 1, event_id: 'beh_voice_preflight_fixture', event_source: 'client',
    occurred_at: Date.now(), collection_period_id: 'period_voice_preflight',
    producer: 'pinvou_agent', producer_version: '0.10.16', app_version: '0.10.16',
    platform: 'windows', arch: 'x86_64', channel: 'dev', locale: 'en',
    ...Object.fromEntries(Object.entries(request).map(([key, value]) => [key.replace(/[A-Z]/g, c => `_${c.toLowerCase()}`), value])),
  };
  assert.deepEqual(validateBehaviorEventShape(event), { ok: true });
}

for (const [lane, source] of Object.entries(sources)) {
  test(`${lane}: real ASR preflight produces bounded component evidence accepted by the existing receiver`, async () => {
    const components = ['engine', 'ffmpeg', 'model'];
    for (let mask = 1; mask < 8; mask += 1) {
      const h = harness(source);
      const missing = components.filter((_component, bit) => mask & (1 << bit));
      const status = Object.fromEntries(components.map(component => [component, !missing.includes(component)]));
      await h.start({ ...status, ready: false, installable: true,
        missing: [...missing, ...missing, '/private/model.bin', 'raw_error_secret'], error: 'private raw error' });
      assert.deepEqual(h.requests.map(r => r.eventName), ['voice_entry_clicked', 'voice_recognition_failed']);
      const [entry, failure] = h.requests;
      assert.equal(failure.operationId, entry.operationId);
      assert.equal(failure.sessionId, entry.sessionId);
      assert.equal(failure.stage, 'submission');
      assert.equal(failure.inputType, 'voice');
      assert.equal(failure.step, `asr_missing_${missing.join('_')}`);
      assert.equal(failure.errorCategory, 'configuration_missing');
      assert.equal(h.installs(), lane === 'desktop' ? 1 : 0);
      assert.ok(!JSON.stringify(h.requests).includes('private'));
      assert.ok(!JSON.stringify(h.requests).includes('secret'));
      for (const request of h.requests) assertReceiverAccepts(request);
    }
  });

  test(`${lane}: absent component facts stay unknown and another click owns a fresh operation`, async () => {
    const h = harness(source);
    for (let attempt = 0; attempt < 2; attempt += 1) await h.start({ ready: false, installable: true });
    const failures = h.requests.filter(r => r.eventName === 'voice_recognition_failed');
    assert.equal(failures.length, 2);
    assert.notEqual(failures[0].operationId, failures[1].operationId);
    assert.equal(failures[0].sessionId, failures[1].sessionId);
    for (const failure of failures) {
      assert.equal(failure.step, 'asr_dependencies');
      assert.equal(failure.stage, 'submission');
      assertReceiverAccepts(failure);
    }
  });

  test(`${lane}: boolean-only status supports legacy responses without copying arbitrary fields`, async () => {
    const h = harness(source);
    await h.start({ ready: false, installable: true, engine: false, model: false, ffmpeg: true,
      missing: 'private path instead of an array' });
    const failure = h.requests.find(r => r.eventName === 'voice_recognition_failed');
    assert.equal(failure.step, 'asr_missing_engine_model');
    assertReceiverAccepts(failure);
  });
}
