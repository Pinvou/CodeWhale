import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');

test('legacy pricing versions are read-only across API proxy client and UI', async () => {
  const [contractText, billingService, billingAdmin, policyRegistry, portalService, portalTest, client, view, copy, styles, built] = await Promise.all([
    read('contracts/openapi/admin.yaml'),
    read('internal/billing/service.go'),
    read('internal/billing/admin.go'),
    read('internal/platform/approvalpolicy/registry.go'),
    read('internal/portal/service.go'),
    read('internal/portal/service_test.go'),
    read('../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../pinvou3-app/src/shared/admin-platform-i18n.js'),
    read('../pinvou3-app/src/features/account-platform/admin-platform.css'),
    read('internal/portal/static/assets/admin.js'),
  ]);
  const contract = YAML.parse(contractText);
  const legacyPath = contract.paths['/billing/admin/v1/pricing-versions'];

  assert.deepEqual(Object.keys(legacyPath), ['get']);
  assert.doesNotMatch(contractText, /createAdminPricingVersion|PricingVersionCreate/);
  assert.match(billingService, /admin\.GET\("\/pricing-versions"/);
  assert.doesNotMatch(billingService, /admin\.POST\("\/pricing-versions"/);
  assert.doesNotMatch(billingAdmin, /adminCreatePriceVersion|AdminCreatePriceVersion/);
  assert.match(billingAdmin, /FROM pricing_versions WHERE published_at IS NOT NULL/);
  assert.match(billingAdmin, /projectPublishedAdminPrices\(items, r\.now\(\)\)/);
  assert.doesNotMatch(policyRegistry, /BILLING_PRICE_VERSION_CREATE/);
  assert.match(portalService, /adminReadPathAllowed\(method, resource,[^\n]*"\/admin\/v1\/pricing-versions"/);
  assert.doesNotMatch(portalService, /resource == "\/admin\/v1\/pricing-versions"[^\n]*method == http\.MethodPost/);
  assert.match(portalTest, /billingAdminPathAllowed\(http\.MethodPost, "\/admin\/v1\/pricing-versions"\)/);
  assert.match(client, /priceVersions\(signal\)[\s\S]*?request\('billing', '\/admin\/v1\/pricing-versions\?limit=100'/);
  assert.doesNotMatch(client, /createPriceVersion|mutate\('billing', '\/admin\/v1\/pricing-versions'/);

  const billing = view.slice(view.indexOf('function Billing('), view.indexOf('function BillingDetail('));
  assert.match(billing, /pricingReadOnlyTitle/);
  assert.match(billing, /pricingReadOnlyDescription/);
  assert.match(billing, /role="status"/);
  assert.match(billing, /key: 'status'[\s\S]*?<Status value=\{item\.status\}/);
  assert.doesNotMatch(billing, /effective_to \? formatDate\(item\.effective_to\) : <Status value="ACTIVE"/);
  assert.doesNotMatch(billing, /createPrice|setPrice|canManagePricing|activatePrice/);
  assert.match(styles, /\.adm-pricing-read-only/);
  assert.equal((copy.match(/pricingReadOnlyTitle:/g) || []).length, 3);
  assert.equal((copy.match(/pricingReadOnlyDescription:/g) || []).length, 3);
  assert.match(built, /pricingReadOnlyTitle/);
  assert.doesNotMatch(built, /Activate new price|立即启用新价格|新価格を有効化/);
});
