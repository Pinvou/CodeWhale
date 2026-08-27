import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const telemetry = readFileSync(new URL('../src-tauri/src/features/behavior_telemetry.rs', import.meta.url), 'utf8');
const forwarder = readFileSync(new URL('../src-tauri/src/features/assistant/forwarder.rs', import.meta.url), 'utf8');
const acp = readFileSync(new URL('../src-tauri/src/features/codex_acp/events.rs', import.meta.url), 'utf8');
const marketplace = readFileSync(new URL('../src-tauri/src/app/commands/marketplace.rs', import.meta.url), 'utf8');
const connectors = readFileSync(new URL('../src-tauri/src/app/commands/connectors.rs', import.meta.url), 'utf8');

test('assistant and ACP tool calls emit correlated start and terminal events', () => {
  assert(forwarder.includes('track_tool_call_started('));
  assert(forwarder.includes('track_tool_call_completed('));
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
  assert.match(telemetry, /lease_for_generation\(telemetry\.current_lease\(\), generation\)/);
  assert(telemetry.includes('tool_install_terminal_rebinds_across_renewal_but_not_reauthorization'));
});
