import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

import { behaviorTelemetryRust } from './behavior_telemetry_source.mjs';

const source = relative => readFileSync(new URL(`../${relative}`, import.meta.url), 'utf8');
const actions = source('src/features/conversation/AssistantMessageActions.jsx');
const timeline = source('src/features/conversation/ConversationTimeline.jsx');
const chatView = source('src/features/chat/ChatView.jsx');
const desktopBridge = source('src/platform/tauri/bridge/interaction.js');
const webBridge = source('src/platform/web/bridge.js');
const command = source('src-tauri/src/app/commands/behavior_telemetry.rs');
const retryCommand = source('src-tauri/src/app/commands/memory.rs');
const timing = source('src-tauri/src/features/assistant/timing.rs');
const forwarder = source('src-tauri/src/features/assistant/forwarder.rs');
const projection = source('src/features/conversation/deepseek-conversation.js');
const telemetry = behaviorTelemetryRust;

test('feedback and copy only send controlled response metadata', () => {
  assert.match(actions, /feedbackAction: clearing \? 'clear' : 'set'/);
  assert.match(actions, /\.\.\.\(clearing \? \{\} : \{ rating: nextRating \}\)/);
  assert.match(actions, /if \(copied && sessionId && telemetryTurnId\)/);
  assert.match(actions, /turnId: telemetryTurnId/);
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
  assert.match(timeline, /telemetryTurnId=\{turn\.lifecycleKnown \? turn\.behaviorTurnId : null\}/);
  assert.match(actions, /if \(sessionId && telemetryTurnId\)/);
  assert.match(timing, /pub fn bind_behavior_turn_id/);
  assert.match(timing, /entry\["behavior_turn_id"\]/);
  assert.match(forwarder, /bind_behavior_turn_id\([\s\S]*?&turn_id/);
  assert.match(projection, /behaviorTurnId = String\(event\.behavior_turn_id\)/);
  assert.match(timeline, /index === turns\.length - 1 \? props\.onRetry : undefined/);
});
