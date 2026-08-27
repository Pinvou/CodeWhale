import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const telemetry = readFileSync(new URL('../src-tauri/src/features/behavior_telemetry.rs', import.meta.url), 'utf8');
const forwarder = readFileSync(new URL('../src-tauri/src/features/assistant/forwarder.rs', import.meta.url), 'utf8');
const acp = readFileSync(new URL('../src-tauri/src/features/codex_acp/mod.rs', import.meta.url), 'utf8');
const settings = readFileSync(new URL('../src-tauri/src/app/commands/settings.rs', import.meta.url), 'utf8');

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
});

test('explicit global and session selections produce model_selected', () => {
  assert(settings.includes('track_model_selected('));
  assert(settings.includes('"app_default"'));
  assert(settings.includes('"session"'));
});
