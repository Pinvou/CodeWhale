import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const source = relative => readFileSync(new URL(`../${relative}`, import.meta.url), 'utf8');
const actions = source('src/features/conversation/AssistantMessageActions.jsx');
const timeline = source('src/features/conversation/ConversationTimeline.jsx');
const chatView = source('src/features/chat/ChatView.jsx');
const desktopBridge = source('src/platform/tauri/bridge/interaction.js');
const webBridge = source('src/platform/web/bridge.js');
const command = source('src-tauri/src/app/commands/behavior_telemetry.rs');
const retryCommand = source('src-tauri/src/app/commands/memory.rs');
const telemetry = [
  'src-tauri/src/features/behavior_telemetry.rs',
  'src-tauri/src/features/behavior_telemetry_tests.rs',
].map(source).join('\n');

test('feedback and copy only send controlled response metadata', () => {
  assert.match(actions, /feedbackAction: clearing \? 'clear' : 'set'/);
  assert.match(actions, /\.\.\.\(clearing \? \{\} : \{ rating: nextRating \}\)/);
  assert.match(actions, /if \(copied && sessionId && turnId\)/);
  assert.match(actions, /contentType: responseContentType\(value\)/);
  assert.doesNotMatch(actions.match(/eventName: 'response_copied'[^]*?\}\s*\}/)?.[0] || '', /text:|value:|content:/);
  assert.match(command, /track_response_feedback_changed/);
  assert.match(command, /track_response_copied/);
  assert.match(telemetry, /scheduled_subject_id\(&lease, "feedback", &source\)/);
  assert.match(telemetry, /if \(action == "set"\) != rating\.is_some\(\)/);
});

test('regeneration creates a new task attempt correlated to the source turn', () => {
  assert.match(chatView, /retryReason: 'manual_regenerate'/);
  for (const bridge of [desktopBridge, webBridge]) {
    assert.match(bridge, /async function editLastTurn\(newText, options\)/);
    assert.match(bridge, /retryReason: options && options\.retryReason \|\| "input_changed"/);
  }
  assert.match(retryCommand, /begin_task_retry/);
  assert.match(retryCommand, /submission\.sent\(\)/);
  assert.match(telemetry, /let source_turn = app[\s\S]*?latest_turns/);
  assert.match(telemetry, /let guard = begin_task_submission/);
  assert.match(telemetry, /BehaviorEvent::new\("task_retry_requested"\)/);
  assert.match(telemetry, /\.string\("attempt_id", &attempt\.attempt_id\)/);
  assert.match(telemetry, /\.string\("source_turn_id", source_turn\.turn_id\)/);
});

test('response actions require an authoritative projected turn id', () => {
  assert.match(timeline, /turnId=\{turn\.lifecycleKnown \? turn\.id : null\}/);
  assert.match(timeline, /index === turns\.length - 1 \? props\.onRetry : undefined/);
});
