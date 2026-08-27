import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const rust = readFileSync(new URL('../src-tauri/src/features/behavior_telemetry.rs', import.meta.url), 'utf8');
const command = readFileSync(new URL('../src-tauri/src/app/commands/behavior_telemetry.rs', import.meta.url), 'utf8');
const settingsBridge = readFileSync(new URL('../src/platform/tauri/bridge/settings.js', import.meta.url), 'utf8');
const acpProviderForm = readFileSync(new URL('../src/features/settings/ProviderFormModal.jsx', import.meta.url), 'utf8');

test('activation cohort is persisted before its one-time event is enqueued', () => {
  assert.match(rust, /let emit_cohort = !state\.control\.cohort_started/);
  assert.match(rust, /state\.control\.cohort_started = true;[\s\S]*persist_state\(state\)\?/);
  assert.match(rust, /BehaviorEvent::new\("analytics_cohort_started"\)/);
  assert.match(rust, /BehaviorEvent::new\("app_opened"\)/);
});

test('provider setup and first task environment use the v1 lifecycle events', () => {
  for (const eventName of [
    'onboarding_started',
    'onboarding_completed',
    'provider_setup_started',
    'provider_setup_completed',
    'provider_setup_failed',
    'task_environment_ready',
  ]) {
    assert(command.includes(`"${eventName}"`), `Rust command must allow ${eventName}`);
    assert(settingsBridge.includes(`'${eventName}'`), `settings bridge must emit ${eventName}`);
  }
  assert(acpProviderForm.includes("eventName: 'provider_setup_started'"));
  assert(acpProviderForm.includes("eventName: 'provider_setup_completed'"));
  assert(acpProviderForm.includes("eventName: 'provider_setup_failed'"));
  assert(!settingsBridge.includes('apiKey: trimmedKey'));
});
