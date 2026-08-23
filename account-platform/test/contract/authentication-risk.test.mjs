import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('password and refresh grants pass the exact public token path through Redis risk admission', async () => {
  const [proxy, ingress, deployment, desktop, contract] = await Promise.all([
    read('internal/identityadapter/token_proxy.go'),
    read('deploy/helm/account-platform/templates/ingress.yaml'),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
    read('../pinvou3-app/src-tauri/src/features/account_platform/mod.rs'),
    read('contracts/openapi/identity.yaml').then(YAML.parse),
  ]);
  assert.match(proxy, /source-minute[^\n]*20, 60[\s\S]*source-day[^\n]*200, 86400[\s\S]*device-minute[^\n]*10, 60/);
  assert.match(proxy, /session-minute[^\n]*30, 60/);
  assert.match(proxy, /identifierCount >= 5[\s\S]*stableCount >= 10/);
  assert.match(proxy, /15\s*\*\s*time\.Minute/);
  assert.match(proxy, /min\(delay, 30\*time\.Second\)/);
  assert.doesNotMatch(proxy, /Complete[\s\S]*client\.Del/);
  assert.match(ingress, /path: \/realms\/pinvou\/protocol\/openid-connect\/token[\s\S]*pathType: Exact[\s\S]*identity-service/);
  assert.match(deployment, /AUTH_RISK_REDIS_URL[\s\S]*AUTH_RISK_HMAC_KEY_BASE64/);
  assert.match(desktop, /post\(config\.token_url\.clone\(\)\)[\s\S]*header\("X-Device-ID", &self\.device_id\)/);
  assert.ok(contract.paths['/realms/pinvou/protocol/openid-connect/token'].post.responses['429']);
});

test('SMS send and verification use one-way multidimensional counters and provider 80-percent envelopes', async () => {
  const [send, verify, migration, authenticator] = await Promise.all([
    read('internal/account/registration_saga.go'),
    read('internal/account/mobile_auth.go'),
    read('migrations/account/000029_authentication_risk_limits.up.sql'),
    read('deploy/keycloak/mobile-authenticator/src/main/java/com/pinvou/identity/MobileDirectGrantAuthenticator.java'),
  ]);
  for (const fragment of ['sourceHour >= 20', 'sourceDay >= 100', 'deviceHour >= 10', 'mobileDay >= 10', 'purposeSourceHour >= 10']) {
    assert.match(send, new RegExp(fragment));
  }
  assert.match(send, /125 \* 80 \/ 100[\s\S]*1250 \* 80 \/ 100/);
  assert.match(verify, /sourceWindow >= 30[\s\S]*deviceWindow >= 20[\s\S]*mobileWindow >= 10[\s\S]*purposeSourceWindow >= 20/);
  assert.match(verify, /375 \* 80 \/ 100/);
  assert.match(verify, /waitForResponseFloor\(ctx, startedAt, s\.loginResponseFloor\)/);
  assert.match(migration, /mobile_hash BYTEA[\s\S]*source_hash BYTEA[\s\S]*device_hash BYTEA/);
  assert.doesNotMatch(migration, /mobile_e164|sms_code|code_hash/);
  assert.match(authenticator, /X-Pinvou-Client-Source[\s\S]*X-Device-ID/);
});
