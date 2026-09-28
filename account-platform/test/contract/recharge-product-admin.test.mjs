import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import YAML from 'yaml';

const read = relative => readFile(new URL(`../../${relative}`, import.meta.url), 'utf8');

test('recharge product commands use atomic permissions, recent OIDC authentication, idempotency, and audit policy', async () => {
  const [service, handlers, policies, permissions, migration] = await Promise.all([
    read('internal/billing/service.go'), read('internal/billing/recharge_product_admin.go'),
    read('internal/platform/approvalpolicy/registry.go'), read('migrations/account/000037_recharge_product_permissions.up.sql'),
    read('migrations/billing/000044_recharge_product_commands.up.sql'),
  ]);
  for (const permission of ['read', 'create', 'publish', 'unpublish', 'delete']) {
    assert.match(permissions, new RegExp(`platform\\.products\\.${permission}`));
    assert.match(service, new RegExp(`platform\\.products\\.${permission}`));
  }
  for (const command of ['CREATE', 'ADMISSION_VERIFY', 'ADMISSION_REVOKE', 'PUBLISH', 'UNPUBLISH', 'DELETE', 'DISPLAY_NAMES_UPDATE']) {
    assert.match(policies, new RegExp(`BILLING_RECHARGE_PRODUCT_${command}`));
  }
  assert.match(handlers, /principal\.AuthenticatedAt/);
  assert.match(handlers, /rechargeProductRecentAuthentication = 15 \* time\.Minute/);
  assert.doesNotMatch(handlers, /X-Auth-Time|auth_time.*Header|auth_time.*json/);
  assert.match(handlers, /Idempotency-Key/);
  assert.match(migration, /recharge_product_command_records/);
  assert.match(migration, /recharge_product_audits/);
  assert.match(service, /display-names.*platform\.products\.publish/);
  assert.match(handlers, /adminUpdateRechargeProductDisplayNames/);
  assert.match(handlers, /strings\.TrimSpace\(body\.DisplayNameZhCN\)/);
});

test('portal proxy and OpenAPI expose only the exact product lifecycle', async () => {
  const [portal, openapiSource] = await Promise.all([read('internal/portal/service.go'), read('contracts/openapi/admin.yaml')]);
  const openapi = YAML.parse(openapiSource);
  assert.match(portal, /parts\[2\] == "recharge-products"/);
  assert.match(portal, /parts\[6\] == "retry" \|\| parts\[6\] == "revoke"/);
  assert.doesNotMatch(portal, /recharge-products.*HasPrefix/);
  for (const path of [
    '/billing/admin/v1/recharge-products',
    '/billing/admin/v1/recharge-products/{catalog_item_id}/publish',
    '/billing/admin/v1/recharge-products/{catalog_item_id}/unpublish',
    '/billing/admin/v1/recharge-products/{catalog_item_id}/delete',
    '/billing/admin/v1/recharge-products/{catalog_item_id}/display-names',
    '/billing/admin/v1/recharge-products/{catalog_item_id}/admission-verifications',
    '/billing/admin/v1/recharge-products/{catalog_item_id}/admission-verifications/{verification_id}/retry',
    '/billing/admin/v1/recharge-products/{catalog_item_id}/admission-verifications/{verification_id}/revoke',
  ]) assert.ok(openapi.paths[path], path);
  const createSchema = openapi.components.schemas.RechargeProductCreate;
  assert.equal(createSchema.properties.credit_microcredits.deprecated, true);
  assert.equal(createSchema.properties.credit_microcredits.minimum, 1);
  assert.ok(!createSchema.required.includes('credit_microcredits'));
});

test('admin product UI is permission driven, tri-lingual, and never auto-replays after step-up', async () => {
  const [html, ui, portal] = await Promise.all([
    read('internal/portal/static/admin.html'), read('internal/portal/static/assets/product-admin.js'), read('internal/portal/service.go'),
  ]);
  assert.match(html, /product-admin-root/);
  assert.match(html, /product-admin\.js/);
  for (const language of ['zh:', 'en:', 'ja:']) assert.match(ui, new RegExp(language));
  for (const permission of ['read', 'create', 'publish', 'unpublish', 'delete']) assert.match(ui, new RegExp(`platform\\.products\\.${permission}`));
  assert.match(ui, /sessionStorage/);
  assert.match(ui, /reauthenticate=1/);
  assert.match(ui, /refresh and confirm again|刷新状态后再次确认/);
  assert.doesNotMatch(ui, /auto.?replay|automaticSubmit|retryAfterLogin/i);
  assert.doesNotMatch(ui, /name="credit_microcredits"|credit_microcredits:Number\(f\.credit_microcredits\)/);
  assert.match(portal, /prompt", "login"/);
  assert.match(portal, /max_age", "900"/);
});
