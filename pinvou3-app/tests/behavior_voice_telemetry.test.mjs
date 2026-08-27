import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import test from 'node:test';
import vm from 'node:vm';

const root = path.resolve(import.meta.dirname, '..');
const read = (...parts) => fs.readFileSync(path.join(root, ...parts), 'utf8');
const voice = read('src', 'platform', 'tauri', 'bridge', 'voice.js');
const webVoice = read('src', 'platform', 'web', 'bridge.js');
const chat = read('src', 'platform', 'tauri', 'bridge', 'chat.js');
const behaviorCommand = read('src-tauri', 'src', 'app', 'commands', 'behavior_telemetry.rs');
const behaviorFeature = read('src-tauri', 'src', 'features', 'behavior_telemetry.rs');
const webPolicy = JSON.parse(read('src', 'platform', 'web', 'access-policy.json'));

test('voice telemetry terminal helper is idempotent and uses controlled categories', async () => {
  const start = voice.indexOf('  function voiceToken(');
  const end = voice.indexOf('\n  function setVoiceInputStatus(', start);
  assert.ok(start >= 0 && end > start);

  const calls = [];
  const context = {
    state: { voiceInput: {} },
    invoke: async (command, args) => { calls.push({ command, args }); },
    Date,
    Math,
  };
  vm.createContext(context);
  vm.runInContext(`${voice.slice(start, end)}\nthis.api = { trackVoiceTerminal, abandonCompletedVoiceResult, voiceTelemetryErrorCategory };`, context);

  const session = { operationId: 'voiceop_123456789012', telemetrySessionId: 'session_123456789012' };
  context.api.trackVoiceTerminal('voice_cancelled', session, { stage: 'recording', durationMs: 10 });
  context.api.trackVoiceTerminal('voice_cancelled', session, { stage: 'recording', durationMs: 20 });
  await Promise.resolve();
  assert.equal(calls.length, 1, 'one operation may emit only one terminal event');
  assert.equal(calls[0].args.request.operationId, session.operationId);

  context.state.voiceInput = {
    status: 'completed',
    operationId: 'voiceop_abcdefghijkl',
    telemetrySessionId: 'session_abcdefghijkl',
    startedAt: Date.now(),
    telemetryTerminal: false,
  };
  context.api.abandonCompletedVoiceResult('recognition');
  context.api.abandonCompletedVoiceResult('recognition');
  await Promise.resolve();
  assert.equal(calls.filter(({ args }) => args.request.eventName === 'voice_cancelled').length, 2);
  assert.equal(context.api.voiceTelemetryErrorCategory('permission_denied'), 'permission_denied');
  assert.equal(context.api.voiceTelemetryErrorCategory('empty_result'), 'tool_error');
  assert.equal(context.api.voiceTelemetryErrorCategory('context_mismatch'), 'validation_failed');
});

test('desktop and web voice paths cover the complete operation funnel', () => {
  for (const [name, source] of [['desktop', voice], ['web', webVoice]]) {
    for (const eventName of [
      'voice_entry_clicked',
      'voice_permission_result',
      'voice_started',
      'voice_recognition_completed',
      'voice_recognition_failed',
      'voice_cancelled',
    ]) {
      assert.match(source, new RegExp(`['"]${eventName}['"]`), `${name} must emit ${eventName}`);
    }
    assert.match(source, /operationId:\s*voiceToken\("voiceop_"\)/);
    assert.match(source, /recognizerType:\s*"local"/);
  }
});

test('voice submission is correlated with the authoritative task attempt', () => {
  assert.match(chat, /eventName:\s*"voice_message_submitted"[\s\S]*?sessionId:\s*sid/);
  assert.match(behaviorCommand, /track_voice_message_submitted/);
  assert.match(
    behaviorFeature,
    /BehaviorEvent::new\("voice_message_submitted"\)[\s\S]*?\.string\("attempt_id", attempt\.attempt_id\)/,
  );
  assert.ok(webPolicy.allowed_commands.includes('web_access_track_behavior_event'));
});
