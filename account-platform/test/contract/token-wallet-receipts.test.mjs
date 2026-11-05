import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { authorizationRoutes } from '../../../scripts/check-account-platform-authorization-coverage.mjs';

const execute = 'internal.billing.token-account-wallet-provision.execute';
const read = 'internal.billing.token-account-wallet-provision.read';
const uri = '/internal/v1/token-account-wallet-provisioning-receipts';

test('wallet receipts retain the actual Token consumer POST and GET with split exact permissions', async () => {
  const billing = await readFile(new URL('../../internal/billing/service.go', import.meta.url), 'utf8');
  const token = await readFile(new URL('../../internal/token/account_provisioning_saga.go', import.meta.url), 'utf8');
  const routes = authorizationRoutes('account-platform/internal/billing/service.go', billing).filter(r => r.path.startsWith(uri));
  assert.deepEqual(routes.map(({method,path}) => ({method,path})), [{method:'POST',path:uri},{method:'GET',path:`${uri}/:command_reference`}]);
  assert.ok(routes[0].registration.includes(`serviceauth.Require("${execute}")`));
  assert.ok(routes[1].registration.includes(`serviceauth.Require("${read}")`));
  assert.ok(token.includes(`http.MethodPost, "${uri}"`));
  assert.ok(token.includes(`http.MethodGet, "${uri}/"+reference.String()`));
  const schema = JSON.parse(await readFile(new URL('../../contracts/openapi/token-wallet-receipts.json', import.meta.url), 'utf8'));
  assert.equal(schema.paths[uri].post['x-required-permission'], execute);
  assert.equal(schema.paths[`${uri}/{command_reference}`].get['x-required-permission'], read);
});

test('production Billing workload gives receipt privileges only to its existing Token actor', async () => {
  const rules = JSON.parse(await readFile(new URL('../../deploy/production/workload-billing-rules.json.template', import.meta.url), 'utf8'));
  for (const permission of [execute,read]) {
    assert.deepEqual(rules.filter(rule => rule.permissions.includes(permission)).map(({service,subject}) => ({service,subject})), [{service:'token-gateway',subject:'__TOKEN_WORKLOAD_SUBJECT__'}]);
  }
});
