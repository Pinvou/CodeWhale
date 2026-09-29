import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const read = (path) => readFile(new URL(`../../${path}`, import.meta.url), 'utf8');
const [migration, configuration, resolver, handler, service, portal, openapi, client, view] = await Promise.all([
  read('migrations/account/000086_admin_sms_configuration.up.sql'),
  read('internal/account/sms_configuration.go'),
  read('internal/account/sms_provider_resolver.go'),
  read('internal/account/sms_configuration_http.go'),
  read('internal/account/service.go'),
  read('internal/portal/service.go'),
  read('contracts/openapi/admin.yaml'),
  read('../pinvou3-app/src/features/account-platform/admin-client.js'),
  read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
]);

test('SMS configuration is immutable, permission-gated, CAS-bound, and audited', () => {
  assert.match(migration, /CREATE TABLE sms_configuration_versions/);
  assert.match(migration, /CREATE TABLE sms_configuration_state/);
  assert.match(migration, /BEFORE UPDATE OR DELETE ON sms_configuration_versions/);
  assert.match(migration, /BEFORE TRUNCATE ON sms_configuration_versions/);
  assert.match(migration, /RAISE EXCEPTION 'sms_configuration_versions is immutable'/);
  assert.match(migration, /platform\.sms-config\.read/);
  assert.match(migration, /platform\.sms-config\.manage/);
  assert.match(migration, /idempotency_key TEXT NOT NULL CHECK \(length\(idempotency_key\) BETWEEN 16 AND 128\)/);
  assert.match(service, /PermissionPlatformSMSConfigRead/);
  assert.match(service, /PermissionPlatformSMSConfigManage/);
  assert.match(handler, /ExpectedRevision/);
  assert.match(handler, /Idempotency-Key/);
  assert.match(configuration, /account\.sms_configuration\.update/);
  assert.doesNotMatch(configuration, /json:"(?:REGISTER|LOGIN|PROFILE_MOBILE|PASSWORD_RESET),omitempty"/);
});

test('provider changes preserve historical challenge verification while disabling new sends', () => {
  assert.match(resolver, /ProviderForSend/);
  assert.match(resolver, /ProviderForVerification/);
  assert.match(resolver, /SMSConfigurationVersion/);
  assert.match(resolver, /if err != nil \|\| !active\.Enabled/);
  assert.match(resolver, /return r\.resolve\(ctx, descriptor\)/);
  assert.match(configuration, /"v1-c"/);
});

test('Admin API and UI expose only non-secret SMS settings', () => {
  assert.match(portal, /\/admin\/v1\/sms-configuration/);
  assert.match(openapi, /\/account\/admin\/v1\/sms-configuration:/);
  assert.match(openapi, /Credentials, endpoints, bearer tokens, verification keys, and provider responses are never returned/);
  assert.doesNotMatch(openapi.match(/SMSConfigurationUpdate:[\s\S]*?TokenUsageReconciliationCommandReadiness:/)?.[0] || '', /access_key|secret|bearer_token|endpoint|verification_key/i);
  assert.match(client, /updateSMSConfiguration/);
  assert.match(view, /platform\.sms-config\.read/);
  assert.match(view, /platform\.sms-config\.manage/);
  assert.doesNotMatch(view.match(/function SettingsWorkspace[\s\S]*?function UserMembershipDetail/)?.[0] || '', /type="password"|access_key|bearer_token|verification_key/i);
});
