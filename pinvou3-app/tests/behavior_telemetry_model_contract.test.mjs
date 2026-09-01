import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const telemetry = [
  '../src-tauri/src/features/behavior_telemetry.rs',
  '../src-tauri/src/features/behavior_telemetry_tests.rs',
].map(path => readFileSync(new URL(path, import.meta.url), 'utf8')).join('\n');
const forwarder = readFileSync(new URL('../src-tauri/src/features/assistant/forwarder.rs', import.meta.url), 'utf8');
const acp = readFileSync(new URL('../src-tauri/src/features/codex_acp/mod.rs', import.meta.url), 'utf8');
const settings = readFileSync(new URL('../src-tauri/src/app/commands/settings.rs', import.meta.url), 'utf8');
const codexCommands = readFileSync(new URL('../src-tauri/src/app/commands/codex.rs', import.meta.url), 'utf8');
const remoteControl = readFileSync(new URL('../src-tauri/src/app/commands/remote_control.rs', import.meta.url), 'utf8');
const behaviorCommand = readFileSync(new URL('../src-tauri/src/app/commands/behavior_telemetry.rs', import.meta.url), 'utf8');
const chatView = readFileSync(new URL('../src/features/chat/ChatView.jsx', import.meta.url), 'utf8');
const toolStoreView = readFileSync(new URL('../src/features/tools/ToolStoreView.jsx', import.meta.url), 'utf8');
const registries = JSON.parse(readFileSync(new URL('../../docs/schemas/behavior-telemetry-registries.json', import.meta.url), 'utf8'));
const schema = JSON.parse(readFileSync(new URL('../../docs/schemas/behavior-telemetry-v1.schema.json', import.meta.url), 'utf8'));

test('model use is correlated to the admitted attempt in assistant and ACP modes', () => {
  assert(telemetry.includes('.string("attempt_id", attempt.attempt_id)'));
  assert(telemetry.includes('.string("selection_source", selection_source)'));
  assert(forwarder.includes('track_model_used('));
  assert(acp.includes('self.bridge.track_model_used('));
});

test('model telemetry emits registry keys instead of raw model metadata', () => {
  assert(telemetry.includes('provider_registry_key(provider)'));
  assert(telemetry.includes('model_registry_key(model)'));
  assert.equal(telemetry.includes('.string("model_id"'), false);
  assert.equal(telemetry.includes('.string("model_name"'), false);
  assert.equal(telemetry.includes('.string("provider_name"'), false);
  assert.ok(registries.providers.includes('xai'));
  assert.ok(registries.providers.includes('local'));
  assert.match(telemetry, /"xai" => "xai"/);
  assert.match(telemetry, /"local_vllm" => "local"/);
  assert.match(telemetry, /client_registries_match_the_shared_machine_readable_contract/);
});

test('explicit global and session selections produce model_selected', () => {
  assert(settings.includes('track_model_selected('));
  assert(settings.includes('"app_default"'));
  assert(settings.includes('"session"'));
});

test('client entry points stay inside the schema enum', () => {
  const allowed = new Set(schema.properties.entry_point.enum);
  for (const entryPoint of ['composer', 'marketplace', 'settings', 'sidebar']) {
    assert.ok(allowed.has(entryPoint), `${entryPoint} must be allowed by the shared schema`);
  }
  for (const source of [settings, codexCommands, remoteControl, chatView, toolStoreView]) {
    for (const invalid of [
      'code_model_picker',
      'session_model_picker',
      'web_code_model_picker',
      'tool_store',
      'chat_home',
    ]) {
      assert.ok(!source.includes(invalid), `${invalid} must not escape the schema enum`);
    }
  }
  assert.match(telemetry, /pub fn entry_point_registry_key[\s\S]*_ => "unknown"/);
  assert.match(behaviorCommand, /entry_point_registry_key\(&value\)/);
  assert.match(telemetry, /\.string\("entry_point", entry_point_registry_key\(entry_point\)\)/);
});
