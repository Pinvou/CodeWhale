import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('Account human delegation is short lived signed and fully request bound', async () => {
  const [delegation, handler, service, client, billing, billingQuota, tokenBilling, migration, compose, helm] = await Promise.all([
    read('internal/platform/humandelegation/delegation.go'),
    read('internal/account/human_delegation.go'),
    read('internal/account/service.go'),
    read('internal/platform/authorization/client.go'),
    read('internal/billing/human_delegation.go'),
    read('internal/billing/member_quota_http.go'),
    read('internal/token/billing.go'),
    read('migrations/billing/000041_human_delegation_audit.up.sql'),
    read('deploy/compose/compose.yaml'),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
  ]);
  assert.match(delegation, /MaximumLifetime = 5 \* time\.Minute/);
  assert.match(delegation, /ed25519\.Sign/);
  assert.match(delegation, /ed25519\.Verify/);
  for (const claim of ['HumanUserID', 'Audience', 'Permission', 'AuthorizationVersion', 'OrganizationID', 'UnitID', 'ResourceType', 'ResourceID', 'HTTPMethod', 'HTTPPath', 'RequestSHA256', 'ExpiresAt']) {
    assert.match(delegation, new RegExp(claim));
  }
  assert.match(handler, /repository\.Authorize/);
  assert.match(handler, /decision\.UserID/);
  assert.doesNotMatch(handler, /ActorUserID|CreatedBy/);
  assert.match(service, /internal\.POST\("\/authorization\/human-delegations"/);
  assert.match(client, /\/human-delegations/);
  assert.match(billing, /X-Pinvou-Human-Delegation/);
  assert.match(billing, /humanDelegations\.Verify/);
  assert.doesNotMatch(billingQuota, /json:"(?:actor_user_id|created_by)"/);
  assert.match(tokenBilling, /rawDelegated/);
  assert.match(tokenBilling, /RequestSHA256/);
  assert.match(migration, /workload_service/);
  assert.match(migration, /workload_subject/);
  assert.match(migration, /delegation_id/);
  assert.match(migration, /delegation_authorization_version/);
  assert.match(compose, /HUMAN_DELEGATION_SIGNING_KEY_BASE64URL/);
  assert.match(helm, /human-delegation-signing-key-base64url/);
});
