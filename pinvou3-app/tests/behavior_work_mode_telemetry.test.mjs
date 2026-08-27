import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const codeView = readFileSync(new URL('../src/features/codex/CodexAcpView.jsx', import.meta.url), 'utf8');
const teamView = readFileSync(new URL('../src/features/team-relay/TeamRelayView.jsx', import.meta.url), 'utf8');
const scheduledDesktop = readFileSync(new URL('../src/platform/tauri/bridge/scheduled.js', import.meta.url), 'utf8');
const scheduledWeb = readFileSync(new URL('../src/platform/web/bridge.js', import.meta.url), 'utf8');
const command = readFileSync(new URL('../src-tauri/src/app/commands/behavior_telemetry.rs', import.meta.url), 'utf8');
const telemetry = readFileSync(new URL('../src-tauri/src/features/behavior_telemetry.rs', import.meta.url), 'utf8');

test('code workspace reports one visible page instance with a controlled provider', () => {
  assert.match(codeView, /codeTelemetryReportedRef/);
  assert.match(codeView, /eventName: 'code_mode_opened'/);
  assert.match(codeView, /viewId: codeTelemetryViewIdRef\.current/);
  assert.match(codeView, /sessionId: activeId/);
  assert.match(codeView, /providerKey: activeAgentId/);
  assert.match(command, /"code_mode_opened" => "code_mode_opened"/);
  assert.match(command, /provider_registry_key\(&value\)/);
});

test('team workspace reports one page instance without team identity or member details', () => {
  assert.match(teamView, /teamTelemetryReportedRef/);
  assert.match(teamView, /eventName: 'team_mode_opened'/);
  assert.match(teamView, /sessionId: teamTelemetrySessionIdRef\.current/);
  assert.match(teamView, /memberCountBucket: memberCountBucket/);
  assert.match(teamView, /if \(count <= 1\) return '1'/);
  assert.doesNotMatch(teamView.match(/eventName: 'team_mode_opened'[^]*?\}\s*,\s*\}\)/)?.[0] || '', /teamId|memberId|email|displayName/);
});

test('scheduled creation and every terminal run are covered on desktop and web', () => {
  for (const source of [scheduledDesktop, scheduledWeb]) {
    assert.match(source, /eventName: "scheduled_task_created"/);
    assert.match(source, /taskType: templateId \|\| "custom"/);
    assert.match(source, /eventName: "scheduled_task_run_finished"/);
    assert.match(source, /scheduledTelemetryFinishedRuns\[key\]/);
    assert.match(source, /endedAt < scheduledTelemetryStartedAt - 5000/);
    assert.match(source, /ended - started/);
    assert.match(source, /\["completed", "failed", "canceled", "cancelled", "interrupted"\]/);
  }
});

test('backend replaces source ids with period-scoped pseudonyms and deterministic event ids', () => {
  assert.match(command, /track_scheduled_task_created/);
  assert.match(command, /track_scheduled_run_finished/);
  assert.match(telemetry, /fn scheduled_subject_id/);
  assert.match(telemetry, /collection_period_id/);
  assert.match(telemetry, /Sha256::new\(\)/);
  assert.match(telemetry, /event\.event_id = scheduled_subject_id\(&lease, "scheduled_created"/);
  assert.match(telemetry, /event\.event_id = scheduled_subject_id\(&lease, "scheduled_finished"/);
  assert.doesNotMatch(telemetry, /string\("scheduled_task_id",\s*source_task_id\)/);
  assert.doesNotMatch(telemetry, /string\("operation_id",\s*source_run_id\)/);
});
