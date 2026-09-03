import assert from 'node:assert/strict';
import test from 'node:test';
import {
  adminModelTokenOverviewRoute,
  adminModelTokenQueryUpdates,
  adminNavigationGroupsForGate,
  adminSectionURL,
  clearAdminModelTokenUpdates,
  normalizeAdminModelTokenEntry,
  normalizeAdminModelTokenNavigation,
  previousAdminModelTokenCursor,
  readAdminModelTokenOperationsTab,
  readAdminModelTokenQuery,
  rememberAdminModelTokenNextCursor,
  synchronizeAdminModelTokenCursorHistory,
  writeAdminSection,
} from '../../../pinvou3-app/src/features/account-platform/admin-navigation.js';
import {
  executeAdminSourceRequest,
  initialAdminSourceState,
  planAdminModelTokenOperationRequests,
  reduceAdminSourceState,
  visibleAdminSourceState,
} from '../../../pinvou3-app/src/features/account-platform/admin-model-token-state.js';
import { AdminPlatformClient } from '../../../pinvou3-app/src/features/account-platform/admin-client.js';
import { adminVisibleSections } from '../../../pinvou3-app/src/features/account-platform/authorization-ui.js';

const locationLike = (search) => ({ pathname: '/admin.html', search, hash: '' });
const authorization = (...permissions) => ({ platform_permissions: permissions });

test('model and Token IA gate preserves the legacy entry off and exposes three canonical sections on', () => {
  assert.deepEqual(adminNavigationGroupsForGate(false).find((group) => group.key === 'models').sections, ['tokens']);
  assert.deepEqual(adminNavigationGroupsForGate(true).find((group) => group.key === 'models').sections, ['models', 'tokenResources', 'modelOperations']);
  assert.deepEqual(adminVisibleSections(authorization('platform.models.read'), { modelTokenIAEnabled: false }).filter((item) => item !== 'settings'), ['tokens']);
  assert.deepEqual(adminVisibleSections(authorization('platform.models.read'), { modelTokenIAEnabled: true }).filter((item) => item !== 'settings'), ['models']);
  assert.deepEqual(adminVisibleSections(authorization('platform.pricing.read'), { modelTokenIAEnabled: true }).filter((item) => item !== 'settings'), ['products', 'models']);
  assert.deepEqual(adminVisibleSections(authorization('platform.tokens.read'), { modelTokenIAEnabled: true }).filter((item) => item !== 'settings'), ['overview', 'tokenResources', 'modelOperations']);
  assert.deepEqual(adminVisibleSections(authorization('platform.usage.read'), { modelTokenIAEnabled: true }).filter((item) => item !== 'settings'), ['overview', 'billing', 'payments', 'modelOperations']);
  const gateOffDeepLink = normalizeAdminModelTokenEntry('modelOperations', ['overview', 'tokens', 'settings'], false, locationLike('?section=modelOperations&usage_q=private&usage_cursor=signed'));
  assert.equal(gateOffDeepLink.section, 'tokens');
  assert.equal(gateOffDeepLink.replace, true);
  assert.equal(gateOffDeepLink.updates.usage_q, null);
  assert.equal(gateOffDeepLink.updates.usage_cursor, null);
  assert.equal(normalizeAdminModelTokenEntry('models', ['products', 'settings'], false, locationLike('?section=models&model_cursor=signed')).section, 'products');
});

test('legacy and unauthorized model-token routes clear all scoped state before a single replacement', () => {
  const visible = ['overview', 'models', 'settings'];
  const legacy = normalizeAdminModelTokenEntry('tokens', visible, true, locationLike('?section=tokens&tab=models&q=secret&model_cursor=signed&token_cursor=other'));
  assert.equal(legacy.section, 'models');
  assert.equal(legacy.replace, true);
  for (const key of ['tab', 'q', 'model_cursor', 'token_cursor', 'operations_tab', 'model_id', 'token_account_id']) assert.equal(legacy.updates[key], null);

  const denied = normalizeAdminModelTokenEntry('tokenResources', visible, true, locationLike('?section=tokenResources&token_account_id=00000000-0000-4000-8000-000000000201&token_q=private'));
  assert.equal(denied.section, 'forbidden');
  assert.equal(denied.updates.token_account_id, null);
  assert.equal(denied.updates.token_q, null);

  for (const route of ['?section=tokens&tab=models&q=private&cursor=signed&model_id=not-valid', '?section=models&model_q=private&model_cursor=signed&model_audit_cursor=audit']) {
    const empty = normalizeAdminModelTokenEntry(readSection(route), [], true, locationLike(route), []);
    assert.equal(empty.section, 'forbidden');
    const finalURL = adminSectionURL(empty.section, locationLike(route), empty.updates);
    assert.equal(finalURL, '/admin.html?section=forbidden');
  }
  const gateOffEmpty = normalizeAdminModelTokenEntry('tokens', [], false, locationLike('?section=tokens&tab=usage&q=private&cursor=signed'), []);
  assert.equal(adminSectionURL(gateOffEmpty.section, locationLike('?section=tokens&tab=usage&q=private&cursor=signed'), gateOffEmpty.updates), '/admin.html?section=forbidden');
});

function readSection(search) {
  return new URLSearchParams(search).get('section') || '';
}

test('invalid and mutually exclusive detail targets are removed without selecting a target', () => {
  const visible = ['models', 'tokenResources', 'modelOperations'];
  const invalid = normalizeAdminModelTokenEntry('models', visible, true, locationLike('?section=models&model_id=not-an-id&model_detail_tab=audit'));
  assert.deepEqual({ section: invalid.section, notice: invalid.notice, model: invalid.updates.model_id, tab: invalid.updates.model_detail_tab }, { section: 'models', notice: 'invalid', model: null, tab: null });
  const conflict = normalizeAdminModelTokenEntry('modelOperations', visible, true, locationLike('?section=modelOperations&provisioning_id=00000000-0000-4000-8000-000000000301&usage_request_id=00000000-0000-4000-8000-000000000401'));
  assert.equal(conflict.notice, 'conflict');
  assert.equal(conflict.updates.provisioning_id, null);
  assert.equal(conflict.updates.usage_request_id, null);

  const crossSection = normalizeAdminModelTokenEntry('models', visible, true, locationLike('?section=models&token_q=hidden&token_cursor=signed&usage_request_id=00000000-0000-4000-8000-000000000401'));
  assert.equal(crossSection.updates.token_q, null);
  assert.equal(crossSection.updates.token_cursor, null);
  assert.equal(crossSection.updates.usage_request_id, null);

  const nestedInvalid = normalizeAdminModelTokenEntry('models', visible, true, locationLike('?section=models&model_id=bad&model_detail_tab=audit&model_audit_action=PUBLISH&model_audit_cursor=a&model_provisioning_status=FAILED&model_provisioning_cursor=p'), ['platform.models.read']);
  for (const key of ['model_id', 'model_detail_tab', 'model_audit_action', 'model_audit_cursor', 'model_provisioning_status', 'model_provisioning_cursor']) assert.equal(nestedInvalid.updates[key], null);

  const nestedConflict = normalizeAdminModelTokenEntry('modelOperations', visible, true, locationLike('?section=modelOperations&operations_tab=usage&provisioning_id=00000000-0000-4000-8000-000000000301&provisioning_attempt_status=FAILED&provisioning_attempt_cursor=p&usage_request_id=00000000-0000-4000-8000-000000000401&usage_evidence_type=CANCELLATION&usage_evidence_cursor=u'), ['platform.tokens.read', 'platform.usage.read']);
  for (const key of ['provisioning_id', 'provisioning_attempt_status', 'provisioning_attempt_cursor', 'usage_request_id', 'usage_evidence_type', 'usage_evidence_cursor']) assert.equal(nestedConflict.updates[key], null);

  const auditDenied = normalizeAdminModelTokenEntry('tokenResources', visible, true, locationLike('?section=tokenResources&token_account_id=00000000-0000-4000-8000-000000000201&token_detail_tab=audit&token_audit_action=READ&token_audit_cursor=a'), ['platform.tokens.read']);
  assert.equal(auditDenied.updates.token_detail_tab, 'overview');
  assert.equal(auditDenied.updates.token_audit_action, null);
  assert.equal(auditDenied.updates.token_audit_cursor, null);

  const invalidToken = normalizeAdminModelTokenEntry('tokenResources', visible, true, locationLike('?section=tokenResources&token_account_id=bad&token_detail_tab=routes&token_credential_metadata_status=ACTIVE&token_credential_cursor=c&token_route_health_status=HEALTHY&token_route_cursor=r&token_provisioning_status=READY&token_provisioning_cursor=p&token_audit_action=READ&token_audit_cursor=a'), ['platform.tokens.read', 'platform.audit.read']);
  for (const key of ['token_account_id', 'token_detail_tab', 'token_credential_metadata_status', 'token_credential_cursor', 'token_route_health_status', 'token_route_cursor', 'token_provisioning_status', 'token_provisioning_cursor', 'token_audit_action', 'token_audit_cursor']) assert.equal(invalidToken.updates[key], null);
});

test('usage-only operation links normalize before render and never plan a Provisioning request', async () => {
  const route = locationLike('?section=modelOperations&operations_tab=provisionings&provisioning_q=private&provisioning_cursor=p&provisioning_id=00000000-0000-4000-8000-000000000301&provisioning_attempt_cursor=a&usage_q=review&usage_cursor=u');
  const normalized = normalizeAdminModelTokenEntry('modelOperations', ['modelOperations', 'settings'], true, route, ['platform.usage.read']);
  assert.equal(normalized.updates.operations_tab, 'usage');
  for (const key of ['provisioning_q', 'provisioning_cursor', 'provisioning_id', 'provisioning_attempt_cursor']) assert.equal(normalized.updates[key], null);
  assert.equal(normalized.updates.usage_q, undefined);
  assert.equal(normalized.updates.usage_cursor, undefined);
  const finalURL = adminSectionURL(normalized.section, route, normalized.updates);
  assert.equal(finalURL, '/admin.html?section=modelOperations&operations_tab=usage&usage_q=review&usage_cursor=u');
  const calls = { provisionings: 0, usage: 0 };
  const operations = readAdminModelTokenOperationsTab(['platform.usage.read'], locationLike(finalURL.slice(finalURL.indexOf('?'))));
  const client = {
    provisionings: async () => { calls.provisionings += 1; return { items: [] }; },
    usageRequests: async () => { calls.usage += 1; return { items: [] }; },
  };
  await Promise.all(planAdminModelTokenOperationRequests(['platform.usage.read'], client).map((request) => request.load({}, new AbortController().signal)));
  assert.deepEqual(calls, { provisionings: 0, usage: 1 });
  assert.deepEqual(operations.visible, ['usage']);

  const tokenOnly = normalizeAdminModelTokenEntry('modelOperations', ['modelOperations', 'settings'], true, locationLike('?section=modelOperations&operations_tab=usage&usage_q=review&usage_cursor=u&usage_request_id=00000000-0000-4000-8000-000000000401&usage_evidence_cursor=e'), ['platform.tokens.read']);
  assert.equal(tokenOnly.updates.operations_tab, 'provisionings');
  for (const key of ['usage_q', 'usage_cursor', 'usage_request_id', 'usage_evidence_cursor']) assert.equal(tokenOnly.updates[key], null);
});

test('operation tab changes normalize the candidate URL and clear the inactive detail in one history write', () => {
  const permissions = ['platform.tokens.read', 'platform.usage.read'];
  const visible = ['modelOperations'];
  const historyCalls = [];
  const mutableLocation = { pathname: '/admin.html', search: '?section=modelOperations&operations_tab=provisionings&provisioning_q=failed&provisioning_id=00000000-0000-4000-8000-000000000301&provisioning_attempt_cursor=attempt&usage_q=review', hash: '' };
  const history = {
    pushState(_state, _unused, url) {
      historyCalls.push({ mode: 'push', url });
      const parsed = new URL(url, 'https://admin.invalid');
      mutableLocation.pathname = parsed.pathname;
      mutableLocation.search = parsed.search;
    },
    replaceState(_state, _unused, url) { historyCalls.push({ mode: 'replace', url }); },
  };
  const toUsage = normalizeAdminModelTokenNavigation('modelOperations', visible, true, permissions, { operations_tab: 'usage' }, mutableLocation);
  writeAdminSection(toUsage.section, 'push', history, mutableLocation, toUsage.updates);
  assert.equal(historyCalls.length, 1);
  assert.equal(historyCalls[0].url, '/admin.html?section=modelOperations&operations_tab=usage&provisioning_q=failed&usage_q=review');
  const refreshedUsage = normalizeAdminModelTokenEntry('modelOperations', visible, true, mutableLocation, permissions);
  assert.equal(refreshedUsage.replace, false);

  mutableLocation.search += '&usage_request_id=00000000-0000-4000-8000-000000000401&usage_evidence_cursor=evidence';
  const toProvisionings = normalizeAdminModelTokenNavigation('modelOperations', visible, true, permissions, { operations_tab: 'provisionings' }, mutableLocation);
  writeAdminSection(toProvisionings.section, 'push', history, mutableLocation, toProvisionings.updates);
  assert.equal(historyCalls.length, 2);
  assert.equal(historyCalls[1].url, '/admin.html?section=modelOperations&operations_tab=provisionings&provisioning_q=failed&usage_q=review');
  const refreshedProvisionings = normalizeAdminModelTokenEntry('modelOperations', visible, true, mutableLocation, permissions);
  assert.equal(refreshedProvisionings.replace, false);
});

test('overview drilldowns obey the IA gate and atomic Token or usage permissions', () => {
  assert.deepEqual(adminModelTokenOverviewRoute(['platform.tokens.read', 'platform.usage.read'], true), { section: 'tokenResources', query: {} });
  assert.deepEqual(adminModelTokenOverviewRoute(['platform.usage.read'], true), { section: 'modelOperations', query: { operations_tab: 'usage' } });
  assert.deepEqual(adminModelTokenOverviewRoute(['platform.tokens.read'], false), { section: 'tokens', query: {} });
  assert.equal(adminModelTokenOverviewRoute(['platform.models.read'], true), null);
});

test('every resource owns distinct URL filters and operation tabs follow atomic read permissions', () => {
  const model = readAdminModelTokenQuery('models', locationLike('?model_q=flash&model_lifecycle_status=PUBLISHED&model_cursor=one&usage_cursor=two'));
  assert.deepEqual(model, { q: 'flash', lifecycle_status: 'PUBLISHED', protocol_code: '', source_adapter: '', source_missing: '', cursor: 'one' });
  assert.deepEqual(adminModelTokenQueryUpdates('usage', { q: 'request', cursor: 'next' }), {
    usage_q: 'request', usage_execution_status: null, usage_settlement_status: null, usage_model: null,
    usage_token_account_id: null, usage_organization_id: null, usage_started_from: null, usage_started_to: null, usage_cursor: 'next',
  });
  const clear = clearAdminModelTokenUpdates({ includeLegacy: true });
  for (const key of ['model_cursor', 'protocol_cursor', 'formula_cursor', 'token_cursor', 'provisioning_cursor', 'usage_cursor', 'reconciliation_cursor', 'owner_candidate_cursor', 'model_audit_cursor', 'token_audit_cursor']) assert.equal(clear[key], null);
  assert.deepEqual(readAdminModelTokenOperationsTab(['platform.usage.read'], locationLike('?operations_tab=provisionings')), { tab: 'usage', replace: true, visible: ['usage'] });
});

test('cursor history is filter-bound and browser history cannot reuse a cursor under another query', () => {
  let history = synchronizeAdminModelTokenCursorHistory({}, { q: 'flash', status: '', cursor: '' });
  history = rememberAdminModelTokenNextCursor(history, 'cursor-1');
  history = synchronizeAdminModelTokenCursorHistory(history, { q: 'flash', status: '', cursor: 'cursor-1' });
  assert.deepEqual(previousAdminModelTokenCursor(history), { available: true, cursor: '' });
  history = rememberAdminModelTokenNextCursor(history, 'cursor-2');
  assert.deepEqual(previousAdminModelTokenCursor(history), { available: true, cursor: 'cursor-1' });
  const back = synchronizeAdminModelTokenCursorHistory(history, { q: 'flash', status: '', cursor: 'cursor-1' });
  assert.deepEqual(previousAdminModelTokenCursor(back), { available: true, cursor: '' });
  const changedFilter = synchronizeAdminModelTokenCursorHistory(back, { q: 'other', status: '', cursor: 'cursor-1' });
  assert.deepEqual(previousAdminModelTokenCursor(changedFilter), { available: false, cursor: '' });
  assert.deepEqual(changedFilter.previousByCursor, {});
});

test('production source transitions clear stale queries, reject late results, and isolate source failure', async () => {
  const deferred = () => {
    let resolve;
    let reject;
    const promise = new Promise((resolvePromise, rejectPromise) => { resolve = resolvePromise; reject = rejectPromise; });
    return { promise, resolve, reject };
  };
  let state = initialAdminSourceState();
  const transition = (event) => { state = reduceAdminSourceState(state, event); };
  const oldRequest = deferred();
  const newRequest = deferred();
  const oldRun = executeAdminSourceRequest({ requestID: 1, queryKey: '{"q":"old"}', query: { q: 'old' }, load: () => oldRequest.promise, transition });
  oldRequest.resolve({ items: [{ id: 'old' }], as_of: '2026-09-04T00:00:00Z' });
  await oldRun;
  assert.deepEqual(state.items, [{ id: 'old' }]);
  assert.deepEqual(visibleAdminSourceState(state, '{"q":"new"}', true).items, [], 'the first frame for a new query cannot expose old rows');

  const lateOld = deferred();
  const lateOldRun = executeAdminSourceRequest({ requestID: 2, queryKey: '{"q":"old-late"}', query: { q: 'old-late' }, load: () => lateOld.promise, transition });
  const newRun = executeAdminSourceRequest({ requestID: 3, queryKey: '{"q":"new"}', query: { q: 'new' }, load: () => newRequest.promise, transition });
  lateOld.resolve({ items: [{ id: 'late-old' }] });
  await lateOldRun;
  assert.deepEqual(state.items, [], 'a late request cannot populate the active query');
  newRequest.resolve({ items: [{ id: 'new' }], as_of: '2026-09-04T00:01:00Z' });
  await newRun;
  assert.deepEqual(state.items, [{ id: 'new' }]);

  let provisionState = initialAdminSourceState();
  let usageState = initialAdminSourceState();
  await Promise.all([
    executeAdminSourceRequest({ requestID: 1, queryKey: 'provision', query: {}, load: async () => { throw Object.assign(new Error('down'), { status: 503 }); }, transition: (event) => { provisionState = reduceAdminSourceState(provisionState, event); } }),
    executeAdminSourceRequest({ requestID: 1, queryKey: 'usage', query: {}, load: async () => ({ items: [{ id: 'usage-kept' }] }), transition: (event) => { usageState = reduceAdminSourceState(usageState, event); } }),
  ]);
  assert.equal(provisionState.error.status, 503);
  assert.deepEqual(usageState.items, [{ id: 'usage-kept' }]);
  assert.equal(usageState.error, null);
});

test('history replacement, popstate normalization, and cross-section restoration preserve canonical state', () => {
  const replacementCalls = [];
  const deniedLocation = locationLike('?section=tokens&tab=usage&q=private&cursor=signed');
  const denied = normalizeAdminModelTokenEntry('tokens', [], true, deniedLocation, []);
  writeAdminSection(denied.section, 'replace', { replaceState: (_state, _unused, url) => replacementCalls.push(url) }, deniedLocation, denied.updates);
  assert.deepEqual(replacementCalls, ['/admin.html?section=forbidden']);

  const modelHistoryURL = '?section=models&model_q=flash&model_cursor=page-two';
  const popModel = normalizeAdminModelTokenEntry('models', ['models', 'tokenResources'], true, locationLike(modelHistoryURL), ['platform.models.read', 'platform.tokens.read']);
  assert.equal(popModel.replace, true);
  assert.equal(popModel.updates.model_tab, 'directory');
  assert.deepEqual(readAdminModelTokenQuery('models', locationLike(modelHistoryURL)), { q: 'flash', lifecycle_status: '', protocol_code: '', source_adapter: '', source_missing: '', cursor: 'page-two' });
  const crossSection = normalizeAdminModelTokenNavigation('tokenResources', ['models', 'tokenResources'], true, ['platform.models.read', 'platform.tokens.read'], {}, locationLike(modelHistoryURL));
  const tokenURL = adminSectionURL(crossSection.section, locationLike(modelHistoryURL), crossSection.updates);
  assert.equal(tokenURL, '/admin.html?section=tokenResources');
  const restoredByPopState = normalizeAdminModelTokenEntry('models', ['models', 'tokenResources'], true, locationLike(modelHistoryURL), ['platform.models.read', 'platform.tokens.read']);
  assert.equal(restoredByPopState.replace, true);
  assert.equal(restoredByPopState.updates.model_tab, 'directory');
  assert.equal(readAdminModelTokenQuery('models', locationLike(modelHistoryURL)).cursor, 'page-two');
});

test('new page source keeps independent loading, error, refresh and last-updated state contracts', async () => {
  const source = await (await import('node:fs/promises')).readFile(new URL('../../../pinvou3-app/src/features/account-platform/AdminModelTokenWorkspace.jsx', import.meta.url), 'utf8');
  assert.match(source, /function useAdminSource/);
  assert.match(source, /executeAdminSourceRequest/);
  assert.match(source, /visibleAdminSourceState/);
  assert.match(source, /planAdminModelTokenOperationRequests/);
  assert.match(source, /return \(\) => controller\.abort\(\)/);
  assert.match(source, /state\.error\?\.status === 410/);
  assert.match(source, /onQueryChange\('usage', \{ \.\.\.query, cursor: '' \}, 'replace'\)/);
  assert.match(source, /ArrowLeft.*ArrowRight.*Home.*End/);
  assert.match(source, /role="tabpanel"/);
  assert.doesNotMatch(source, /usageRequest\(|provisioning\(|tokenAccount\(/);
  assert.doesNotMatch(source, /window\.prompt|window\.confirm/);
  const view = await (await import('node:fs/promises')).readFile(new URL('../../../pinvou3-app/src/features/account-platform/AdminPlatformView.jsx', import.meta.url), 'utf8');
  const initialNormalize = view.indexOf('const normalized = normalizeAdminModelTokenEntry(requested, visible, iaEnabled');
  const initialDenied = view.indexOf('if (visible.length === 0)', initialNormalize);
  assert.ok(initialNormalize >= 0 && initialNormalize < initialDenied, 'initial authorization normalizes before rendering zero-permission denial');
  assert.match(view, /normalizeAdminModelTokenEntry\(section, visible, modelTokenIAEnabled, undefined, next\.platform_permissions \|\| \[\], \{ tokenResourceFormsEnabled \}\)/);
  assert.match(view, /const normalized = normalizeAdminModelTokenNavigation\(section, visibleSections, modelTokenIAEnabled, platformPermissions, updates, undefined, \{ tokenResourceFormsEnabled \}\)/);
  assert.match(source, /provisioningRequest &&/);
  assert.match(source, /usageRequest &&/);
});

test('APT-127 presentation keeps three-language labels, visible focus, and a 320px layout boundary', async () => {
  const fs = await import('node:fs/promises');
  const i18n = await fs.readFile(new URL('../../../pinvou3-app/src/shared/admin-platform-i18n.js', import.meta.url), 'utf8');
  const css = await fs.readFile(new URL('../../../pinvou3-app/src/features/account-platform/admin-platform.css', import.meta.url), 'utf8');
  for (const label of ['模型管理', 'Models', 'モデル管理', 'Token 资源', 'Token resources', 'Token リソース', '运行与用量', 'Operations & usage', '運用と使用量']) assert.ok(i18n.includes(label));
  assert.match(css, /\.adm-shell button:focus-visible/);
  assert.match(css, /@media\(max-width:360px\)/);
  assert.match(css, /\.adm-source-card \.adm-table td\{grid-template-columns:1fr/);
  assert.doesNotMatch(css, /adm-source-card[^}]*linear-gradient/);
});

test('new collection calls use the paginated BFF contract while gate-off calls keep legacy URLs', async () => {
  const requested = [];
  const previousFetch = globalThis.fetch;
  globalThis.fetch = async (url) => {
    requested.push(String(url));
    return new Response(JSON.stringify({ items: [] }), { status: 200, headers: { 'Content-Type': 'application/json' } });
  };
  try {
    const client = new AdminPlatformClient('https://portal.example');
    const legacySignal = new AbortController().signal;
    await client.catalogModels(legacySignal);
    await client.tokenAccounts(legacySignal);
    await client.provisionings(legacySignal);
    await client.usageRequests(legacySignal);
    await client.catalogModels({ q: 'flash', lifecycle_status: 'PUBLISHED', cursor: 'signed' });
    await client.tokenAccounts({ owner_type: 'ORGANIZATION', status: 'ACTIVE' });
    await client.provisionings({ status: 'FAILED', sla_overdue: 'true' });
    await client.usageRequests({ settlement_status: 'REVIEW', started_from: '2026-09-01T00:00:00Z' });
    await client.tokenReconciliationCases({ status: 'OPEN', severity: 'P1' });
    await client.tokenOwnerCandidates({ q: 'team', owner_type: 'ORG' });
    await client.tokenResourceAudit({ resource_type: 'MODEL', resource_id: '00000000-0000-4000-8000-000000000101', action: 'PUBLISHED' });
  } finally {
    globalThis.fetch = previousFetch;
  }
  assert.deepEqual(requested.slice(0, 4), [
    'https://portal.example/api/admin/token/admin/v1/models',
    'https://portal.example/api/admin/token/admin/v1/token-accounts?limit=100',
    'https://portal.example/api/admin/token/admin/v1/provisionings?limit=100',
    'https://portal.example/api/admin/token/admin/v1/usage-requests?limit=100',
  ]);
  assert.equal(requested[4], 'https://portal.example/api/admin/token/admin/v1/models?limit=50&q=flash&lifecycle_status=PUBLISHED&cursor=signed');
  assert.equal(requested[5], 'https://portal.example/api/admin/token/admin/v1/token-accounts?limit=50&owner_type=ORGANIZATION&status=ACTIVE');
  assert.equal(requested[6], 'https://portal.example/api/admin/token/admin/v1/provisionings?limit=50&status=FAILED&sla_overdue=true');
  assert.equal(requested[7], 'https://portal.example/api/admin/token/admin/v1/usage-requests?limit=50&settlement_status=REVIEW&started_from=2026-09-01T00%3A00%3A00Z');
  assert.equal(requested[8], 'https://portal.example/api/admin/token/admin/v1/reconciliation-cases?limit=50&status=OPEN&severity=P1');
  assert.equal(requested[9], 'https://portal.example/api/admin/token/admin/v1/owner-candidates?limit=50&q=team&owner_type=ORG');
  assert.equal(requested[10], 'https://portal.example/api/admin/token/admin/v1/resource-audit?limit=50&resource_type=MODEL&resource_id=00000000-0000-4000-8000-000000000101&action=PUBLISHED');
});

test('Portal BFF explicitly allowlists every APT-127 list source and exposes the gate through session only', async () => {
  const fs = await import('node:fs/promises');
  const service = await fs.readFile(new URL('../../internal/portal/service.go', import.meta.url), 'utf8');
  const deployment = await fs.readFile(new URL('../../deploy/helm/account-platform/templates/app-deployments.yaml', import.meta.url), 'utf8');
  for (const path of ['/admin/v1/models', '/admin/v1/protocols', '/admin/v1/formulas', '/admin/v1/token-accounts', '/admin/v1/provisionings', '/admin/v1/usage-requests', '/admin/v1/reconciliation-cases', '/admin/v1/owner-candidates', '/admin/v1/resource-audit']) assert.match(service, new RegExp(path.replaceAll('/', '\\/')));
  assert.match(service, /feature_flags/);
  assert.match(service, /admin_model_token_ia/);
  assert.match(deployment, /PORTAL_ADMIN_MODEL_TOKEN_IA_ENABLED/);
});
