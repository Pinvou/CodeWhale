import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';

const rust = readFileSync(new URL('../src-tauri/src/features/behavior_telemetry.rs', import.meta.url), 'utf8');
const usageRust = readFileSync(new URL('../src-tauri/src/platform/telemetry.rs', import.meta.url), 'utf8');
const command = readFileSync(new URL('../src-tauri/src/app/commands/behavior_telemetry.rs', import.meta.url), 'utf8');
const settingsBridge = readFileSync(new URL('../src/platform/tauri/bridge/settings.js', import.meta.url), 'utf8');
const acpProviderForm = readFileSync(new URL('../src/features/settings/ProviderFormModal.jsx', import.meta.url), 'utf8');
const schema = JSON.parse(readFileSync(new URL('../../docs/schemas/behavior-telemetry-v1.schema.json', import.meta.url), 'utf8'));

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

test('client privacy allowlist stays aligned with schema and excludes server identity', () => {
  const allowlistBody = rust.match(/fn client_event_field_allowed[\s\S]*?\n}\n\nfn validate_client_event_fields/)?.[0] || '';
  const actual = Array.from(allowlistBody.matchAll(/\|?\s*"([a-z0-9_]+)"/g), match => match[1])
    .toSorted((left, right) => left.localeCompare(right));
  const expected = schema['x-client-optional-fields'].toSorted((left, right) => left.localeCompare(right));
  assert.deepEqual(actual, expected);
  assert.ok(schema['x-server-injected-fields'].includes('account_subject_id'));
  for (const forbidden of ['message', 'content', 'file_path', 'tool_arguments', 'raw_error']) {
    assert.ok(!actual.includes(forbidden));
  }
});

test('retryable delivery and pending privacy controls keep bounded background retry loops', () => {
  assert.match(rust, /let retryable_remains = telemetry\.outbox[\s\S]*if retryable_remains \{[\s\S]*tokio::time::sleep/);
  assert.match(rust, /retry_delay = retry_delay\.saturating_mul\(2\)\.min\(RETRY_MAX_DELAY\)/);
  assert.match(rust, /async fn run_control_worker[\s\S]*while telemetry\.has_pending_control\(\)\.await/);
  assert.match(rust, /pending control retry deferred[\s\S]*tokio::select!/);
  assert.match(rust, /fn wake_control_worker[\s\S]*tx\.try_send/);
  assert.match(rust, /async fn retry_pending_control\(&self\)[\s\S]*self\.control_gate\.lock\(\)\.await/);
  assert.match(rust, /pending_request_matches\(state, &pending\)/);
  assert.match(rust, /behavior_events_url: state\.behavior_events_url\.clone\(\)/);
  assert.match(rust, /async fn migrate_endpoint_locked/);
  assert.match(rust, /StatusCode::GONE[\s\S]*device_credentials_expired[\s\S]*recover_expired_credentials/);
  assert.match(rust, /async fn recover_expired_credentials/);
  assert.match(rust, /rebind_period\(&started\.collection_period_id, generation\)/);
  assert.match(rust, /device_data_deleted[\s\S]*handle_authoritative_deletion/);
  assert.match(rust, /async fn handle_authoritative_deletion_locked[\s\S]*collecting\.store\(false[\s\S]*persist_state\(&fresh\)[\s\S]*outbox\.clear\(\)[\s\S]*tasks\.clear\(\)/);
  assert.match(rust, /fn state_after_authoritative_deletion[\s\S]*PersistedState::fresh/);
  assert.match(rust, /prepare_disabled_state\(state, generation\)[\s\S]*persist_state\(state\)[\s\S]*outbox\.clear\(\)/);
  assert.match(rust, /fresh\.retired_endpoints[\s\S]*fresh\.control\.pending_deletions/);
  assert.match(rust, /authorization_generation[\s\S]*fn lease_is_current/);
  assert.match(rust, /fn lease_matches_authorization[\s\S]*generation\.load\(Ordering::Acquire\) == lease\.generation/);
  assert.match(rust, /fn stale_producer_cannot_write_after_delete_and_reauthorization_aba/);
  assert.match(rust, /fn disabled_state_survives_restart_before_fallible_cleanup/);
  assert.match(settingsBridge, /function scheduleBehaviorTelemetryStatusPoll/);
  assert.match(settingsBridge, /status\.withdrawal_pending \|\| status\.deletion_pending/);
  assert.match(settingsBridge, /status\.enabled \|\| behaviorTelemetryControlPending\(status\)/);
  assert.match(settingsBridge, /get_behavior_telemetry_status/);
  assert.match(settingsBridge, /scheduleBehaviorTelemetryStatusPoll\(\);[\s\S]*return state\.behaviorTelemetry/);
  assert.match(usageRust, /StatusCode::GONE[\s\S]*credential_has_expired[\s\S]*clear_credentials/);
  assert.match(usageRust, /credential_has_expired[\s\S]*device_credentials_expired/);
  assert.match(usageRust, /credential_has_expired\("device_data_deleted"\)/);
});

test('credential and pending-control state is atomically replaced', () => {
  const persistence = rust.match(/fn persist_state_at[\s\S]*?\n}\n\nfn state_path/)?.[0] || '';
  assert.match(persistence, /filesystem::atomic_write\(path, &data\)/);
  assert.doesNotMatch(persistence, /create_secret_file|File::create|std::fs::write/);
  assert.match(rust, /telemetry state is unreadable; collection is disabled/);
  assert.match(rust, /refusing to replace the persisted installation identity/);
});
