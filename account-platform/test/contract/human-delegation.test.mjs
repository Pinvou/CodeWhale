import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('Account human delegation is short lived signed and fully request bound', async () => {
  const [delegation, handler, service, client, compose, helm] = await Promise.all([
    read('internal/platform/humandelegation/delegation.go'),
    read('internal/account/human_delegation.go'),
    read('internal/account/service.go'),
    read('internal/platform/authorization/client.go'),
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
  assert.match(compose, /HUMAN_DELEGATION_SIGNING_KEY_BASE64URL/);
  assert.match(helm, /human-delegation-signing-key-base64url/);
});
