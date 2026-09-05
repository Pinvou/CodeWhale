import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import test from 'node:test';
import { pathToFileURL } from 'node:url';
import { buildPriceDraftInput, createSynchronousMutationLock, matchingContinuationDrafts, newModelPriceAllowed, priceDraftMatchesInput, priceSpaceQuery, priceTransitionModes, pricingLifecycleMutationLock, publishedPricingFormulas, shanghaiBoundary, unknownMutationResolution } from '../../../pinvou3-app/src/features/account-platform/pricing-lifecycle.js';

const root = path.resolve(import.meta.dirname, '../..');
const read = relative => readFile(path.join(root, relative), 'utf8');
const formulaID = '00000000-0000-7000-8200-000000000001';
const inputUnitID = '00000000-0000-7000-8100-000000000001';
const outputUnitID = '00000000-0000-7000-8100-000000000003';

test('pricing lifecycle workspace executes one real server render', async () => {
  const appRoot = path.resolve(root, '../pinvou3-app');
  const [{ createServer }, React, reactDOMServer] = await Promise.all([
    import(pathToFileURL(path.join(appRoot, 'node_modules/vite/dist/node/index.js')).href),
    import(pathToFileURL(path.join(appRoot, 'node_modules/react/index.js')).href),
    import(pathToFileURL(path.join(appRoot, 'node_modules/react-dom/server.node.js')).href),
  ]);
  const server = await createServer({ root: appRoot, server: { middlewareMode: true }, appType: 'custom', logLevel: 'silent' });
  try {
    const [{ PricingLifecycleWorkspace }, { adminPlatformCopy }] = await Promise.all([
      server.ssrLoadModule('/src/features/account-platform/PricingLifecycleWorkspace.jsx'),
      server.ssrLoadModule('/src/shared/admin-platform-i18n.js'),
    ]);
    const client = new Proxy({}, { get: () => async () => ({ items: [], protocols: [], formulas: [] }) });
    const html = reactDOMServer.renderToString(React.createElement(PricingLifecycleWorkspace, { copy: adminPlatformCopy.zh, client, permissions: ['platform.pricing.read'] }));
    assert.match(html, /adm-pricing-workspace/);
  } finally {
    await server.close();
  }
});

test('pricing draft input preserves exact formula units, inheritance, Shanghai boundaries, and ETag', () => {
  const formula = {
    formula_version_id: formulaID,
    protocol_code: 'CHAT',
    version: 1,
    status: 'PUBLISHED',
    required_unit_versions: { INPUT_TOKEN: inputUnitID, OUTPUT_TOKEN: outputUnitID },
  };
  assert.deepEqual(publishedPricingFormulas({ formulas: [{ ...formula, status: 'DRAFT' }, formula] }), [formula]);
  assert.equal(shanghaiBoundary('2026-09-03'), '2026-09-03T00:00:00+08:00');
  const form = {
    scope_type: 'MODEL', scope_id: 'gpt-example', protocol_code: 'CHAT', formula_version_id: formulaID,
    effective_from: '2026-09-03', effective_to: '2026-09-05', units: {
      [inputUnitID]: { config_type: 'FIXED_PRICE', fixed_price_per_denominator: '125' },
      [outputUnitID]: { config_type: 'INHERIT_GLOBAL', fixed_price_per_denominator: '' },
    },
  };
  assert.deepEqual(priceSpaceQuery(form, formula), { scope_type: 'MODEL', scope_id: 'gpt-example', protocol_code: 'CHAT', formula_version_id: formulaID, credit_unit: 'MICROCREDIT' });
  const result = buildPriceDraftInput(form, formula, 9);
  assert.deepEqual(result.errors, {});
  assert.equal(result.input.expected_etag, 9);
  assert.equal(result.input.effective_from, '2026-09-03T00:00:00+08:00');
  assert.deepEqual(result.input.units, [
    { unit_definition_id: inputUnitID, config_type: 'FIXED_PRICE', fixed_price_per_denominator: 125 },
    { unit_definition_id: outputUnitID, config_type: 'INHERIT_GLOBAL' },
  ]);
});

test('unknown non-idempotent results stay locked until an authoritative read proves completion or safe retry', () => {
  const input = buildPriceDraftInput({
    scope_type: 'MODEL', scope_id: 'model-id', formula_version_id: formulaID, effective_from: '2026-09-03', effective_to: '',
    units: { [inputUnitID]: { config_type: 'FIXED_PRICE', fixed_price_per_denominator: '10' }, [outputUnitID]: { config_type: 'FIXED_PRICE', fixed_price_per_denominator: '20' } },
  }, { formula_version_id: formulaID, protocol_code: 'CHAT', status: 'PUBLISHED', required_unit_versions: { INPUT_TOKEN: inputUnitID, OUTPUT_TOKEN: outputUnitID } }, 37).input;
  const created = { ...input, price_version_id: 'new', lifecycle_status: 'DRAFT', units: input.units };
  assert.equal(priceDraftMatchesInput(created, input), true);
  assert.deepEqual(unknownMutationResolution({ kind: 'DRAFT_CREATE', input, beforeIDs: [] }, { items: [], has_more: true }), { confirmed: false, retrySafe: false });
  assert.deepEqual(unknownMutationResolution({ kind: 'DRAFT_CREATE', input, beforeIDs: [] }, { items: [created], has_more: false }), { confirmed: true, retrySafe: false });
  assert.deepEqual(unknownMutationResolution({ kind: 'DRAFT_CREATE', input, beforeIDs: [] }, { items: [], has_more: false }), { confirmed: false, retrySafe: true });
  assert.deepEqual(unknownMutationResolution({ kind: 'DRAFT_UPDATE', input }, { item: { ...created, units: [] } }), { confirmed: false, retrySafe: false });
  const unitInput = { unit_code: 'REQUEST', minimum_precision: 1, price_denominator: 1000, rounding_rule: 'CEILING' };
  assert.deepEqual(unknownMutationResolution({ kind: 'UNIT_CREATE', input: unitInput, beforeIDs: [] }, { items: [{ ...unitInput, unit_definition_id: 'new-unit' }] }), { confirmed: true, retrySafe: false });
});

test('synchronous mutation lock rejects a second submit while authoritative preflight is pending', async () => {
  const lock = createSynchronousMutationLock();
  let releasePreflight;
  const preflight = new Promise((resolve) => { releasePreflight = resolve; });
  let reads = 0;
  let writes = 0;
  const submit = async () => {
    if (!lock.tryAcquire()) return;
    try { reads += 1; await preflight; writes += 1; } finally { lock.release(); }
  };
  const first = submit();
  const second = submit();
  assert.equal(lock.isLocked(), true);
  releasePreflight();
  await Promise.all([first, second]);
  assert.equal(reads, 1);
  assert.equal(writes, 1);
  assert.equal(lock.isLocked(), false);
});

test('pricing mutation lock is shared across workspace instances', () => {
  assert.equal(pricingLifecycleMutationLock.tryAcquire(), true);
  assert.equal(pricingLifecycleMutationLock.tryAcquire(), false);
  pricingLifecycleMutationLock.release();
  assert.equal(pricingLifecycleMutationLock.isLocked(), false);
});

test('new model price eligibility fails closed after permission, removal, or protocol changes', () => {
  const form = { scope_type: 'MODEL', scope_id: 'model-1' };
  const formula = { protocol_code: 'CHAT' };
  const model = { model_id: 'model-1', lifecycle_status: 'PUBLISHED', protocol_code: 'CHAT' };
  assert.equal(newModelPriceAllowed(form, true, [model], formula), true);
  assert.equal(newModelPriceAllowed(form, false, [model], formula), false);
  assert.equal(newModelPriceAllowed(form, true, [{ ...model, lifecycle_status: 'REMOVED' }], formula), false);
  assert.equal(newModelPriceAllowed(form, true, [{ ...model, protocol_code: 'EMBEDDING' }], formula), false);
});

test('price transition choices preserve model/global state-machine boundaries', () => {
  const active = { price_version_id: 'active', derived_status: 'ACTIVE', lifecycle_status: 'PUBLISHED', scope_type: 'MODEL', scope_id: 'm', protocol_code: 'CHAT', formula_version_id: formulaID, credit_unit: 'MICROCREDIT' };
  const draft = { ...active, price_version_id: 'draft', lifecycle_status: 'DRAFT', derived_status: 'DRAFT' };
  assert.deepEqual(priceTransitionModes(active), ['FALLBACK_GLOBAL', 'CONTINUATION', 'BLOCK']);
  assert.deepEqual(priceTransitionModes({ ...active, scope_type: 'GLOBAL' }), ['CONTINUATION']);
  assert.deepEqual(matchingContinuationDrafts(active, [draft, { ...draft, scope_id: 'other' }]), [draft]);
});

test('admin pricing UI exposes the complete Billing lifecycle without legacy writes or browser prompts', async () => {
  const [client, view, workspace, billingLifecycle, copy, styles, built] = await Promise.all([
    read('../pinvou3-app/src/features/account-platform/admin-client.js'),
    read('../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx'),
    read('../pinvou3-app/src/features/account-platform/PricingLifecycleWorkspace.jsx'),
    read('internal/billing/pricing_admin_v2.go'),
    read('../pinvou3-app/src/shared/admin-platform-i18n.js'),
    read('../pinvou3-app/src/features/account-platform/admin-platform.css'),
    read('internal/portal/static/assets/admin.js'),
  ]);
  for (const endpoint of [
    '/admin/v1/pricing?limit=100', '/admin/v1/metering-units', '/admin/v1/protocols', '/admin/v1/pricing/drafts',
    '/draft`, \'PUT\'', '/preview`', '/publish`', '/transition`', '/deactivate`',
  ]) assert.match(client, new RegExp(endpoint.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')));
  assert.match(view, /<PricingLifecycleWorkspace/);
  assert.match(workspace, /platform\.pricing\.manage/);
  assert.match(workspace, /buildPriceDraftInput/);
  assert.match(workspace, /previewPrice/);
  assert.match(workspace, /publishPrice/);
  assert.match(workspace, /transitionPrice/);
  assert.match(workspace, /deactivateMeteringUnit/);
  assert.match(workspace, /INHERIT_GLOBAL/);
  assert.match(workspace, /unknownMutation/);
  assert.match(workspace, /generations\.current/);
  assert.match(workspace, /disabled=\{Boolean\(unknownMutation\)\}/);
  assert.match(workspace, /if \(!canManage\)/);
  assert.match(workspace, /managedPriceSpace/);
  assert.match(workspace, /disabled=\{contextual \|\| Boolean\(editor\.price_version_id\)\}/);
  assert.match(workspace, /client\.catalogModels/);
  assert.match(workspace, /generations\.current\.models \+= 1/);
  assert.match(workspace, /lifecycle_status !== 'REMOVED'/);
  assert.doesNotMatch(workspace, /lifecycle_status !== 'RETIRED'/);
  assert.match(workspace, /item\.protocol_code === editorFormula\?\.protocol_code/);
  assert.match(workspace, /baseline = await client\.meteringUnits\(baselineController\.signal\)/);
  assert.match(workspace, /if \(projection\.has_more\)/);
  assert.match(workspace, /STALE_RESOURCE_RESPONSE/);
  assert.match(workspace, /pricingLifecycleMutationLock\.tryAcquire\(\)/);
  assert.match(workspace, /newModelPriceAllowed/);
  assert.match(workspace, /phase: 'PREPARING'/);
  assert.match(workspace, /phase: 'DISPATCHED'/);
  assert.match(workspace, /latestFacts\.current/);
  assert.match(workspace, /controller\.abort/);
  assert.match(workspace, /client\.meteringUnits\(baselineController\.signal\)/);
  assert.match(workspace, /<fieldset className="adm-pricing-fieldset" disabled=/);
  assert.match(billingLifecycle, /CurrentSpaceETag\s+int64\s+`json:"current_space_etag"`/);
  assert.match(billingLifecycle, /ReadManagedPriceSpace/);
  assert.match(billingLifecycle, /scope_type=\$1 AND COALESCE\(scope_id,''\)=\$2/);
  assert.match(billingLifecycle, /limit\+1/);
  assert.match(billingLifecycle, /SELECT etag FROM pricing_space_versions/);
  assert.doesNotMatch(workspace, /window\.(prompt|confirm)/);
  assert.equal((copy.match(/pricingLifecycle:/g) || []).length, 3);
  assert.match(styles, /\.adm-pricing-workspace/);
  assert.match(built, /pricingLifecycle/);
  assert.doesNotMatch(client, /mutate\('billing', '\/admin\/v1\/pricing-versions'/);
});
