import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

import { behaviorTelemetryRust } from './behavior_telemetry_source.mjs';

const telemetry = behaviorTelemetryRust;
const forwarder = readFileSync(new URL('../src-tauri/src/features/assistant/forwarder.rs', import.meta.url), 'utf8');
const acp = readFileSync(new URL('../src-tauri/src/features/codex_acp/events.rs', import.meta.url), 'utf8');
const marketplace = readFileSync(new URL('../src-tauri/src/app/commands/marketplace.rs', import.meta.url), 'utf8');
const connectors = readFileSync(new URL('../src-tauri/src/app/commands/connectors.rs', import.meta.url), 'utf8');

test('assistant and ACP tool calls emit correlated start and terminal events', () => {
  assert(forwarder.includes('track_tool_call_started('));
  assert(forwarder.includes('track_tool_call_completed_with_metadata('));
  const completion = forwarder.slice(forwarder.indexOf('Event::ToolCallComplete {'), forwarder.indexOf('let background_task_id ='));
  assert.match(completion, /tool_completion_metadata\(\s*&shell_manager,\s*&session_id,\s*turn_id,\s*&id,\s*&name,\s*success,\s*metadata\.as_ref\(\),\s*\)/);
  assert.match(completion, /track_tool_call_completed_with_metadata\([\s\S]*?diagnostic_metadata\.as_ref\(\),\s*\);/);
  assert(acp.includes('track_tool_call_started('));
  assert(acp.includes('track_tool_call_completed('));
  assert(telemetry.includes('calls.remove(&tool_operation_key(session_id, call_id))'));
  assert(telemetry.includes('.integer("duration_ms"'));
});

test('tool names are reduced to the controlled registry', () => {
  assert(telemetry.includes('pub fn tool_registry_key'));
  assert(telemetry.includes('=> "mcp_custom"'));
  assert.match(telemetry, /_ => "custom"/);
});

test('marketplace and connector installs have a guarded terminal event', () => {
  assert(marketplace.match(/begin_tool_install/g).length >= 2);
  assert(connectors.match(/begin_tool_install/g).length >= 4);
  assert(telemetry.includes('impl Drop for ToolInstallGuard'));
  assert(telemetry.includes('BehaviorEvent::new("tool_install_started")'));
  assert(telemetry.includes('BehaviorEvent::new("tool_install_completed")'));
  assert.match(telemetry, /ToolInstallGuard[\s\S]*authorization_generation: Option<u64>/);
  assert(telemetry.includes('ToolInstallTerminalCoordinator'));
  assert(telemetry.includes('complete_tool_install_terminal'));
  assert(telemetry.includes('CredentialRenewalTransition'));
  assert.match(telemetry, /renewal_was_in_progress[\s\S]*renewal_in_progress\.load\(Ordering::Acquire\)/);
  const renewal = telemetry.slice(telemetry.indexOf('pub(super) async fn recover_expired_credentials_locked'), telemetry.indexOf('pub(super) fn finish_credential_renewal_checkpoint'));
  const finish = telemetry.slice(telemetry.indexOf('pub(super) fn finish_credential_renewal_checkpoint'), telemetry.indexOf('pub async fn status'));
  assert.match(finish, /persist_state\(&committed\)\?[\s\S]*collecting\.store\(true, Ordering::Release\);\s*Ok\(\(\)\)/);
  assert.match(renewal, /if let Err\(error\) = self\.finish_credential_renewal_checkpoint\(state\)\s*\{[^}]*return Err\(error\);\s*\}\s*renewal_transition\.complete\(\);\s*self\.flush_tool_install_terminals\(generation\);/);
  const background = telemetry.slice(telemetry.indexOf('pub(super) async fn retry_pending_control('), telemetry.indexOf('pub(super) async fn run_worker'));
  assert.match(background, /finish_credential_renewal_checkpoint\(state\)[\s\S]*return Err\(error\)[\s\S]*Some\(state\.control\.generation\)[\s\S]*if let Some\(generation\) = recovered_generation\s*\{\s*self\.flush_tool_install_terminals\(generation\);/);
  assert(telemetry.includes('tool_install_terminal_waits_for_renewal_and_drops_after_reauthorization'));
  assert(telemetry.includes('tool_install_persistence_failures_do_not_accumulate_and_recovery_converges'));
});

test('all CLI installation commands record the returned result before propagating errors', () => {
  for (const id of ['feishu', 'wecom', 'dingtalk', 'tmeet']) {
    const start = connectors.indexOf(`pub async fn ${id}_ensure_cli(`);
    const body = connectors.slice(start, connectors.indexOf('\n}', start) + 2);
    assert.match(body, new RegExp(`install\\.finish\\(${id}_domain::${id}_ensure_cli\\(\\)\\.await\\)`));
    assert.doesNotMatch(body, /\.await\?/);
  }
});

test('marketplace sync and join errors reach the installation guard before returning', () => {
  for (const name of ['tool', 'skill']) {
    const start = marketplace.indexOf(`pub async fn install_marketplace_${name}(`);
    const body = marketplace.slice(start, marketplace.indexOf('\n}', start) + 2);
    assert.match(body, /let installed\s*=[\s\S]*?\.map_err\([\s\S]*?\.and_then\(std::convert::identity\);/);
    assert.match(body, /if let Err\(error\) = installed\s*\{\s*return telemetry_install\.finish\(Err\(error\)\);/);
  }
  const start = marketplace.indexOf('pub async fn install_marketplace_tool(');
  const body = marketplace.slice(start, marketplace.indexOf('\n}', start) + 2);
  assert.match(body, /let post_install =[\s\S]*?mark_mcp_config_updated\(\);[\s\S]*?hot_refresh\(&pool, true\)\.await;\s*telemetry_install\.finish\(post_install\)/);
});
