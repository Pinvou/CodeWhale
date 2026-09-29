import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const repositoryRoot = path.resolve(root, '..');
const read = (relative) => readFile(path.join(root, relative), 'utf8');
const readRepository = (relative) => readFile(path.join(repositoryRoot, relative), 'utf8');

function entitlementValidator(contract) {
  const document = JSON.parse(JSON.stringify({
    $schema: 'https://json-schema.org/draft/2020-12/schema',
    $ref: '#/$defs/EntitlementPackageList',
    $defs: contract.components.schemas,
  }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: true });
  addFormats(ajv);
  return ajv.compile(document);
}

const packageValue = {
  package_id: '11111111-1111-4111-8111-111111111111',
  provider_order_reference: '', source_type: 'SYSTEM', source_reference: 'seed',
  plan_id: 'PLAN', plan_name: 'purchase snapshot', sku_code: '', amount_fen: 0,
  points_total_microcredits: 100, points_reserved_microcredits: 0, points_used_microcredits: 0,
  points_remaining_microcredits: 100, points_expired_microcredits: 0, points_revoked_microcredits: 0,
  credited_at: '2026-09-28T00:00:00Z', valid_from: '2026-09-28T00:00:00Z', valid_until: '2026-10-28T00:00:00Z',
  status: 'ACTIVE', components: [],
};
const summary = { available_microcredits: 100, base_available_microcredits: 100, bonus_available_microcredits: 0, reserved_microcredits: 0, used_microcredits: 0, active_package_count: 1, next_expiry_microcredits: 100 };

test('personal and organization entitlement responses share a strict localized-name schema', async () => {
  const contract = YAML.parse(await read('contracts/openapi/token-gateway.yaml'));
  const personal = contract.paths['/me/entitlement-packages'].get.responses['200'].content['application/json'].schema;
  const organization = contract.paths['/organizations/{organization_id}/entitlement-packages'].get.responses['200'].content['application/json'].schema;
  assert.deepEqual(personal, { $ref: '#/components/schemas/EntitlementPackageList' });
  assert.deepEqual(organization, personal);
  const validate = entitlementValidator(contract);
  assert.equal(validate({ summary, items: [{ ...packageValue, current_plan_name_zh_cn: '中文', current_plan_name_en: 'English', current_plan_name_ja: '日本語' }] }), true, JSON.stringify(validate.errors));
  assert.equal(validate({ summary, items: [packageValue] }), true, JSON.stringify(validate.errors));
  assert.equal(validate({ summary, items: [{ ...packageValue, unexpected: true }] }), false);
});

test('Billing projects and Token/Portal preserve current names without mutating purchase snapshots', async () => {
  const [model, packages, tokenClient, tokenService, portal] = await Promise.all([
    read('internal/billing/model.go'), read('internal/billing/packages.go'), read('internal/token/billing.go'),
    read('internal/token/service.go'), read('internal/portal/service.go'),
  ]);
  for (const field of ['current_plan_name_zh_cn', 'current_plan_name_en', 'current_plan_name_ja']) assert.match(model, new RegExp(field));
  assert.match(packages, /ep\.plan_name,COALESCE\(system_product\.display_name_zh_cn,catalog\.display_name_zh_cn/);
  assert.match(packages, /LEFT JOIN recharge_product_catalog_items catalog ON catalog\.product_id=ep\.product_id/);
  assert.match(packages, /system_product\.product_id=ep\.product_id/);
  assert.match(tokenClient, /return c\.raw\(ctx, http\.MethodGet, "\/internal\/v1\/entitlement-packages"\+query/);
  assert.match(tokenService, /writeRawNoStore\(w, http\.StatusOK, payload\)/);
  assert.match(portal, /"\/v1\/me\/entitlement-packages"/);
});

test('Web list, detail, and organization allocation re-evaluate names from the active language', async () => {
  const view = await readRepository('pinvou3-app/src/features/account-platform/AccountPlatformView.jsx');
  assert.match(view, /EntitlementPackageRow key=.*language=\{language\}/s);
  assert.match(view, /EntitlementPackageDetail copy=\{copy\} language=\{language\}/);
  assert.match(view, /<option key=\{item\.package_id\}[\s\S]*entitlementPackageDisplayName\(item, language\)/);
  assert.match(view, /function EntitlementPackageRow\([\s\S]*entitlementPackageDisplayName\(item, language\)/);
  assert.match(view, /function EntitlementPackageDetail\([\s\S]*entitlementPackageDisplayName\(item, language\)/);
});

test('governed rename command is CAS, idempotent, audited, and invalidates on-sale admission', async () => {
  const [repository, handler, service, portal, policy] = await Promise.all([
    read('internal/billing/recharge_product_catalog.go'), read('internal/billing/recharge_product_admin.go'),
    read('internal/billing/service.go'), read('internal/portal/service.go'), read('internal/platform/approvalpolicy/registry.go'),
  ]);
  assert.match(repository, /version != command\.ExpectedVersion/);
  assert.match(repository, /lifecycle == "ON_SALE"[\s\S]*status='RETIRED'[\s\S]*product_admission_bindings SET effective_to/);
  assert.match(repository, /nextLifecycle = "OFF_SALE"/);
  assert.match(repository, /UPDATE recharge_products SET plan_name=/);
  assert.match(handler, /catalogResourceRequestHash[\s\S]*ExecuteRechargeProductDisplayNamesUpdate/);
  assert.match(repository, /recharge_product_command_records[\s\S]*recharge_product_audits[\s\S]*tx\.Commit/);
  assert.match(service, /display-names.*platform\.products\.publish.*requireRecentProductAuthentication/s);
  assert.match(portal, /parts\[4\] == "display-names"/);
  assert.match(policy, /BILLING_RECHARGE_PRODUCT_DISPLAY_NAMES_UPDATE/);
});
