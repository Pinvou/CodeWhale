import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

import { behaviorTelemetryRust } from './behavior_telemetry_source.mjs';

const telemetry = behaviorTelemetryRust;
const chat = readFileSync(new URL('../src-tauri/src/app/commands/chat.rs', import.meta.url), 'utf8');
const forwarder = readFileSync(new URL('../src-tauri/src/features/assistant/forwarder.rs', import.meta.url), 'utf8');
const runtime = readFileSync(new URL('../src-tauri/src/app/commands/runtime.rs', import.meta.url), 'utf8');

test('assistant submissions are guarded through admission or typed rejection', () => {
  const begin = chat.indexOf('begin_task_submission(');
  const send = chat.indexOf('.send_reserved_user_message(');
  const sent = chat.indexOf('submission.sent()');
  assert(begin >= 0 && begin < send);
  assert(sent > send);
  assert.match(telemetry, /impl Drop for TaskSubmissionGuard[\s\S]*reject_task_submission/);
  assert.match(telemetry, /BehaviorEvent::new\("task_submitted"\)/);
  assert.match(telemetry, /BehaviorEvent::new\("task_rejected"\)/);
});

test('authoritative engine events close the admitted lifecycle exactly once', () => {
  assert(forwarder.includes('behavior_telemetry::admit_task('));
  assert(forwarder.includes('behavior_telemetry::task_first_output('));
  assert.equal(forwarder.includes('BehaviorEvent::new("task_started")'), false);
  assert.equal((forwarder.match(/behavior_telemetry::finish_task\(/g) || []).length, 2);
  assert(runtime.includes('behavior_telemetry::request_task_cancel('));
  assert.match(telemetry, /first_output_emitted/);
});

test('in-flight attempts are private, durable and compensated after restart', () => {
  assert(telemetry.includes('behavior-telemetry-tasks.json'));
  assert(telemetry.includes('persist_task_attempts(&tasks)'));
  assert.match(telemetry, /for \(session_id, attempt\) in recovered_tasks/);
  assert.match(telemetry, /status\("interrupted"\)/);
  assert.match(telemetry, /stage\("shutdown"\)/);
});
