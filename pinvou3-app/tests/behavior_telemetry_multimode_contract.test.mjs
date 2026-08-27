import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const codex = readFileSync(new URL('../src-tauri/src/app/commands/codex.rs', import.meta.url), 'utf8');
const events = readFileSync(new URL('../src-tauri/src/features/codex_acp/events.rs', import.meta.url), 'utf8');
const chat = readFileSync(new URL('../src-tauri/src/app/commands/chat.rs', import.meta.url), 'utf8');
const team = readFileSync(new URL('../src-tauri/src/app/commands/team_relay.rs', import.meta.url), 'utf8');

test('ACP user prompts share the durable task lifecycle', () => {
  assert(codex.indexOf('begin_task_submission(') < codex.indexOf('.send_message('));
  assert(codex.includes('submission.sent()'));
  assert(codex.includes('request_task_cancel(&app, &session_id)'));
  assert(events.includes('behavior_telemetry::admit_task('));
  assert(events.includes('behavior_telemetry::task_first_output('));
  assert(events.includes('behavior_telemetry::finish_task('));
});

test('team-linked sessions use team mode while scheduled runs stay excluded', () => {
  assert(team.includes('register_team_session(&app'));
  assert(chat.includes('if team_initiated { "team" } else { "assistant" }'));
  assert(codex.includes('is_team_session(app, &session_id)'));
  assert.match(chat, /\(!is_scheduled\)\.then/);
  assert.equal(codex.includes('"schedule"'), false);
});
