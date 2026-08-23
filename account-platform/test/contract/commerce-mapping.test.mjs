import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';

import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = (relative) => readFile(path.join(root, relative), 'utf8');

test('Commerce Customer mapping is supplier-neutral and outside authorization', async () => {
  const [migration, accountModel, accountStore, tokenModel, tokenService, accountContract, billingContract] = await Promise.all([
    read('migrations/account/000015_commerce_customer_mapping.up.sql'),
    read('internal/account/model.go'),
    read('internal/account/commerce_mapping.go'),
    read('internal/token/model.go'),
    read('internal/token/service.go'),
    read('contracts/openapi/account.yaml'),
    read('contracts/openapi/billing.yaml'),
  ]);

  for (const table of ['commerce_customer_mappings', 'commerce_customer_sync_jobs', 'commerce_customer_sync_evidence']) {
    assert.match(migration, new RegExp(`CREATE TABLE ${table}`));
  }
  for (const field of ['provider_id', 'adapter_version', 'external_reference_redacted', 'mapping_version']) {
    assert.match(migration, new RegExp(field));
  }
  assert.doesNotMatch(accountModel, /MallSync|mall_sync/);
  assert.doesNotMatch(tokenModel, /MallSync|mall_sync/);
  assert.doesNotMatch(tokenService, /MOBILE_ACCOUNT_NOT_READY|synchronized with the Mall/);
  assert.match(accountStore, /COMMERCE_PROVIDER_UNAVAILABLE/);

  const account = YAML.parse(accountContract);
  const authorization = account.paths['/me/authorization-context'].get.responses['200'].content['application/json'].schema;
  assert.equal(authorization.properties.commerce_customer_status, undefined);
  assert.ok(account.paths['/me/commerce-customer-mapping']);
  assert.equal(account.components.schemas.CommerceCustomerMapping.properties.external_reference_redacted, undefined);

  const billing = YAML.parse(billingContract);
  assert.ok(billing.components.schemas.CreateRechargeOrderRequest.required.includes('commerce_customer_status'));
});
