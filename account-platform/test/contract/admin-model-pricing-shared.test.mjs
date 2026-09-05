import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');
const id = digit => `${digit.repeat(8)}-${digit.repeat(4)}-4${digit.repeat(3)}-8${digit.repeat(3)}-${digit.repeat(12)}`;

function validator(contract, schemaName) {
  const document = JSON.parse(JSON.stringify({ $schema: 'https://json-schema.org/draft/2020-12/schema', $ref: `#/$defs/${schemaName}`, $defs: contract.components.schemas }).replaceAll('#/components/schemas/', '#/$defs/'));
  const ajv = new Ajv2020({ allErrors: true, strict: false });
  addFormats(ajv);
  return ajv.compile(document);
}

test('Billing managed pricing and metering responses are closed shared contracts', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  assert.equal(contract.paths['/billing/admin/v1/pricing'].get.responses['200'].$ref, '#/components/responses/ManagedPriceRead');
  assert.equal(contract.paths['/billing/admin/v1/metering-units'].get.responses['200'].$ref, '#/components/responses/MeteringUnitVersionCollection');
  const validatePrice = validator(contract, 'ManagedPriceVersion');
  const price = {
    price_version_id: id('1'), scope_type: 'MODEL', scope_id: id('2'), protocol_code: 'CHAT', formula_version_id: id('3'), credit_unit: 'MICROCREDIT',
    lifecycle_status: 'PUBLISHED', derived_status: 'ACTIVE', version: 2, space_etag: 7, current_space_etag: 7,
    effective_from: '2026-09-01T00:00:00+08:00', published_at: '2026-08-31T12:00:00Z',
    units: [{ unit_definition_id: id('4'), config_type: 'INHERIT_GLOBAL' }],
  };
  assert.equal(validatePrice(price), true, JSON.stringify(validatePrice.errors));
  assert.equal(validatePrice({ ...price, copied_model_price: true }), false);
  assert.equal(validatePrice({ ...price, scope_type: 'GLOBAL' }), false, 'global prices cannot carry a model scope identifier');
  assert.equal(validatePrice({ ...price, scope_type: 'MODEL', scope_id: undefined }), false, 'model prices require their stable model identifier');
  const validateUnit = validator(contract, 'MeteringUnitVersion');
  const unit = { unit_definition_id: id('4'), unit_code: 'INPUT_TOKEN', version: 3, minimum_precision: 1, price_denominator: 1000000, rounding_rule: 'CEILING', status: 'ACTIVE', created_at: '2026-08-01T00:00:00Z' };
  assert.equal(validateUnit(unit), true, JSON.stringify(validateUnit.errors));
  assert.equal(validateUnit({ ...unit, secret: 'never' }), false);
});

test('model details and finance mount one Billing-owned pricing lifecycle component', async () => {
  const [view, detail, workspace, navigation, portal, compose, helm] = await Promise.all([
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/AdminModelDetailWorkspace.jsx'),
    read('../pinvou3-app/src/features/account-platform/PricingLifecycleWorkspace.jsx'),
    read('../pinvou3-app/src/features/account-platform/admin-navigation.js'),
    read('internal/portal/service.go'),
    read('deploy/compose/compose.yaml'),
    read('deploy/helm/account-platform/templates/app-deployments.yaml'),
  ]);
  assert.match(view, /<PricingLifecycleWorkspace[^>]*onOpenModel=/);
  assert.match(detail, /<PricingLifecycleWorkspace[^>]*contextModel=\{detail\.model\}[^>]*contextFormula=\{formula\}/);
  assert.equal((view.match(/import \{ PricingLifecycleWorkspace \}/g) || []).length, 1);
  assert.equal((detail.match(/import \{ PricingLifecycleWorkspace \}/g) || []).length, 1);
  assert.match(workspace, /client\.managedPriceSpace\(\{ scope_type: 'MODEL', scope_id: contextModel\.model_id, protocol_code: contextFormula\.protocol_code, formula_version_id: contextFormula\.formula_version_id, credit_unit: 'MICROCREDIT' \}/);
  assert.match(workspace, /!contextual && <section className="adm-finance-subpanel">/);
  assert.doesNotMatch(detail, /createPriceDraft|updatePriceDraft|publishPrice|transitionPrice/);
  assert.match(navigation, /admin_model_pricing_edit/);
  assert.match(portal, /"admin_model_pricing_edit"/);
  assert.match(compose, /PORTAL_ADMIN_MODEL_PRICING_EDIT_ENABLED:-false/);
  assert.match(helm, /PORTAL_ADMIN_MODEL_PRICING_EDIT_ENABLED/);
});

test('model price projection remains an independent Billing snapshot instead of a copied definition', async () => {
  const contract = YAML.parse(await read('contracts/openapi/admin.yaml'));
  const validate = validator(contract, 'ModelPriceProjection');
  const priceID = id('1');
  const projection = {
    model_id: id('2'), protocol_code: 'CHAT', formula_version_id: id('3'), credit_unit: 'MICROCREDIT', as_of: '2026-09-05T01:00:00Z',
    current: { effective_at: '2026-09-05T01:00:00Z', coverage_status: 'AVAILABLE', price: { price_version_id: priceID, scope_type: 'MODEL', space_etag: 9, effective_from: '2026-09-01T00:00:00Z', effective_to: null, units: [{ unit_code: 'INPUT_TOKEN', unit_definition_id: id('4'), unit_version: 3, minimum_precision: 1, price_denominator: 1000000, rounding_rule: 'CEILING', resolved_price: 5, configuration_type: 'INHERIT_GLOBAL', price_source_scope: 'GLOBAL', source_price_version_id: id('5') }] } },
    next_change: null, source: 'BILLING', snapshot_semantics: 'BILLING_REPEATABLE_READ_ONLY; TOKEN_CATALOG_NOT_READ; NO_CROSS_DOMAIN_ATOMICITY',
  };
  assert.equal(validate(projection), true, JSON.stringify(validate.errors));
  assert.equal(validate({ ...projection, token_catalog_version: 7 }), false);
});
