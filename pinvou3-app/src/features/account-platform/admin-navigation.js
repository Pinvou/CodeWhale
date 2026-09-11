import { adminModelTokenOperationSources } from './admin-model-token-state.js';

export const legacyAdminNavigationGroups = Object.freeze([
  { key: 'workbench', sections: ['overview'] },
  { key: 'identity', sections: ['users', 'roles', 'organizations', 'myorg'] },
  { key: 'finance', sections: ['billing', 'payments', 'products'] },
  { key: 'models', sections: ['tokens'] },
  { key: 'governance', sections: ['approvals', 'audit', 'system', 'settings'] },
]);

export const adminModelTokenSections = Object.freeze(['models', 'tokenResources', 'modelOperations']);

export function adminNavigationGroupsForGate(modelTokenIAEnabled) {
  return legacyAdminNavigationGroups.map((group) => (
    group.key === 'models'
      ? { ...group, sections: modelTokenIAEnabled ? [...adminModelTokenSections] : ['tokens'] }
      : { ...group, sections: [...group.sections] }
  ));
}

// Legacy export remains stable for gate-off consumers and extensions.
export const adminNavigationGroups = legacyAdminNavigationGroups;

const adminModelTokenQueryParameters = Object.freeze({
  models: Object.freeze({ q: 'model_q', lifecycle_status: 'model_lifecycle_status', protocol_code: 'model_protocol_code', source_adapter: 'model_source_adapter', source_missing: 'model_source_missing', cursor: 'model_cursor' }),
  protocols: Object.freeze({ q: 'protocol_q', status: 'protocol_status', protocol_code: 'protocol_code', published: 'protocol_published', created_from: 'protocol_created_from', created_to: 'protocol_created_to', cursor: 'protocol_cursor' }),
  formulas: Object.freeze({ q: 'formula_q', status: 'formula_status', protocol_definition_id: 'formula_protocol_id', metering_unit_code: 'formula_metering_unit_code', published: 'formula_published', created_from: 'formula_created_from', created_to: 'formula_created_to', cursor: 'formula_cursor' }),
  tokenAccounts: Object.freeze({ q: 'token_q', owner_type: 'token_owner_type', owner_id: 'token_owner_id', status: 'token_status', health_status: 'token_health_status', credential_status: 'token_credential_status', cursor: 'token_cursor' }),
  provisionings: Object.freeze({ q: 'provisioning_q', status: 'provisioning_status', model_id: 'provisioning_model_id', token_account_id: 'provisioning_token_account_id', scope_type: 'provisioning_scope_type', scope_id: 'provisioning_scope_id', sla_overdue: 'provisioning_sla_overdue', cursor: 'provisioning_cursor' }),
  usage: Object.freeze({ q: 'usage_q', execution_status: 'usage_execution_status', settlement_status: 'usage_settlement_status', model: 'usage_model', token_account_id: 'usage_token_account_id', organization_id: 'usage_organization_id', started_from: 'usage_started_from', started_to: 'usage_started_to', cursor: 'usage_cursor' }),
  reconciliation: Object.freeze({ q: 'reconciliation_q', status: 'reconciliation_status', severity: 'reconciliation_severity', case_type: 'reconciliation_case_type', owner_id: 'reconciliation_owner_id', sla_overdue: 'reconciliation_sla_overdue', cursor: 'reconciliation_cursor' }),
  ownerCandidates: Object.freeze({ q: 'owner_candidate_q', owner_type: 'owner_candidate_type', status: 'owner_candidate_status', organization_id: 'owner_candidate_organization_id', cursor: 'owner_candidate_cursor' }),
  accountRecovery: Object.freeze({ q: 'account_recovery_q', cursor: 'account_recovery_cursor' }),
  modelAudit: Object.freeze({ action: 'model_audit_action', actor_id: 'model_audit_actor_id', occurred_from: 'model_audit_occurred_from', occurred_to: 'model_audit_occurred_to', cursor: 'model_audit_cursor' }),
  modelHistory: Object.freeze({ cursor: 'model_history_cursor' }),
	modelLifecyclePlans: Object.freeze({ status: 'model_lifecycle_plan_status', target_lifecycle: 'model_lifecycle_plan_target', cursor: 'model_lifecycle_plan_cursor' }),
	modelLifecycleCandidates: Object.freeze({ q: 'model_lifecycle_candidate_q', cursor: 'model_lifecycle_candidate_cursor' }),
  tokenAudit: Object.freeze({ action: 'token_audit_action', actor_id: 'token_audit_actor_id', occurred_from: 'token_audit_occurred_from', occurred_to: 'token_audit_occurred_to', cursor: 'token_audit_cursor' }),
  modelProvisionings: Object.freeze({ q: 'model_provisioning_q', status: 'model_provisioning_status', cursor: 'model_provisioning_cursor' }),
  tokenProvisionings: Object.freeze({ q: 'token_provisioning_q', status: 'token_provisioning_status', cursor: 'token_provisioning_cursor' }),
  tokenCredentials: Object.freeze({ status: 'token_credential_metadata_status', cursor: 'token_credential_cursor' }),
  tokenRoutes: Object.freeze({ health_status: 'token_route_health_status', cursor: 'token_route_cursor' }),
  provisioningAttempts: Object.freeze({ status: 'provisioning_attempt_status', cursor: 'provisioning_attempt_cursor' }),
  usageEvidence: Object.freeze({ evidence_type: 'usage_evidence_type', cursor: 'usage_evidence_cursor' }),
});

const adminModelTokenPageParameters = Object.freeze([
  'model_tab', 'model_id', 'model_detail_tab', 'model_price_manage', 'model_price_return', 'token_account_id', 'token_detail_tab',
  'operations_tab', 'provisioning_id', 'usage_request_id', 'model_operations_window',
  'model_overview_result', 'token_overview_result', 'operations_overview_result',
]);
const adminModelTokenDetailParameters = Object.freeze({
  model: Object.freeze(['model_id', 'model_detail_tab', 'model_price_manage', 'model_price_return', ...Object.values(adminModelTokenQueryParameters.modelAudit), ...Object.values(adminModelTokenQueryParameters.modelHistory), ...Object.values(adminModelTokenQueryParameters.modelProvisionings), ...Object.values(adminModelTokenQueryParameters.modelLifecyclePlans), ...Object.values(adminModelTokenQueryParameters.modelLifecycleCandidates)]),
  token: Object.freeze(['token_account_id', 'token_detail_tab', ...Object.values(adminModelTokenQueryParameters.tokenAudit), ...Object.values(adminModelTokenQueryParameters.tokenProvisionings), ...Object.values(adminModelTokenQueryParameters.tokenCredentials), ...Object.values(adminModelTokenQueryParameters.tokenRoutes)]),
  provisioning: Object.freeze(['provisioning_id', ...Object.values(adminModelTokenQueryParameters.provisioningAttempts)]),
  usage: Object.freeze(['usage_request_id', ...Object.values(adminModelTokenQueryParameters.usageEvidence)]),
});
const adminModelTokenTabParameters = Object.freeze({
  provisionings: Object.freeze([...Object.values(adminModelTokenQueryParameters.provisionings), ...adminModelTokenDetailParameters.provisioning]),
  usage: Object.freeze([...Object.values(adminModelTokenQueryParameters.usage), ...adminModelTokenDetailParameters.usage]),
  reconciliation: Object.freeze([...Object.values(adminModelTokenQueryParameters.reconciliation), ...adminModelTokenDetailParameters.usage]),
});
const adminModelTokenSectionParameters = Object.freeze({
  models: Object.freeze(['model_tab', 'model_id', 'model_detail_tab', 'model_overview_result', ...['models', 'protocols', 'formulas', 'modelAudit', 'modelHistory', 'modelProvisionings', 'modelLifecyclePlans', 'modelLifecycleCandidates'].flatMap((source) => Object.values(adminModelTokenQueryParameters[source]))]),
  tokenResources: Object.freeze(['token_account_id', 'token_detail_tab', 'token_overview_result', ...['tokenAccounts', 'ownerCandidates', 'tokenAudit', 'tokenProvisionings', 'tokenCredentials', 'tokenRoutes'].flatMap((source) => Object.values(adminModelTokenQueryParameters[source]))]),
  modelOperations: Object.freeze(['operations_tab', 'provisioning_id', 'usage_request_id', 'operations_overview_result', ...['provisionings', 'usage', 'reconciliation', 'provisioningAttempts', 'usageEvidence'].flatMap((source) => Object.values(adminModelTokenQueryParameters[source]))]),
});
const adminModelTokenOverviewResultParameters = Object.freeze({ models: 'model_overview_result', tokenResources: 'token_overview_result', modelOperations: 'operations_overview_result' });
const adminModelTokenOverviewResultKeys = Object.freeze({
  models: Object.freeze([]),
  tokenResources: Object.freeze(['healthy_route_count', 'route_attention_count']),
  modelOperations: Object.freeze(['provisioning_attention_count', 'reconciliation_pending_count', 'reconciliation_stopped_count', 'reconciliation_trend']),
});
const adminModelTokenOverviewResultTabs = Object.freeze({
  provisioning_attention_count: 'provisionings',
  reconciliation_pending_count: 'reconciliation',
  reconciliation_stopped_count: 'reconciliation',
  reconciliation_trend: 'reconciliation',
});
const legacyTokenParameters = Object.freeze(['tab', 'token_tab', 'tokens_tab', 'q', 'status', 'protocol', 'source', 'owner_type', 'health', 'model_id_filter', 'owner_id_filter', 'from', 'to', 'cursor']);
const uuidPattern = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;

function clearParameters(parameters, updates) {
  for (const parameter of parameters) updates[parameter] = null;
}

function hasAnyModelTokenState(search) {
  return [...adminModelTokenPageParameters, ...Object.values(adminModelTokenQueryParameters).flatMap((parameters) => Object.values(parameters))].some((parameter) => search.has(parameter));
}

export function adminModelTokenIAGateEnabled(session = {}) {
  return session?.feature_flags?.admin_model_token_ia === true;
}

export function adminTokenResourceFormsGateEnabled(session = {}) {
  return session?.feature_flags?.admin_token_resource_forms === true;
}

export function adminModelOperationsDetailsGateEnabled(session = {}) {
  return session?.feature_flags?.admin_model_operations_details === true;
}

export function adminModelDetailsGateEnabled(session = {}) {
  return session?.feature_flags?.admin_model_details === true;
}

export function adminModelPricingEditGateEnabled(session = {}) {
  return session?.feature_flags?.admin_model_pricing_edit === true;
}

export function adminTokenDetailsGateEnabled(session = {}) {
  return session?.feature_flags?.admin_token_details === true && typeof session?.credential_origin === 'string' && session.credential_origin.length > 0;
}

export function adminModelTokenActionsGateEnabled(session = {}) {
  return session?.feature_flags?.admin_model_token_actions === true;
}

export function adminModelOperationsOverviewGateEnabled(session = {}) {
  return session?.feature_flags?.admin_model_operations_overview === true;
}

export function adminModelTokenPageOverviewsGateEnabled(session = {}) {
  return session?.feature_flags?.admin_model_token_page_overviews === true;
}

export function readAdminModelTokenOverviewResult(section, locationLike = globalThis.location) {
  const parameter = adminModelTokenOverviewResultParameters[section];
  const value = parameter ? String(new URLSearchParams(String(locationLike?.search || '')).get(parameter) || '') : '';
  return adminModelTokenOverviewResultKeys[section]?.includes(value) ? value : '';
}

export function readAdminModelOperationsWindow(locationLike = globalThis.location) {
  const value = new URLSearchParams(String(locationLike?.search || '')).get('model_operations_window') || '24h';
  return ['24h', '7d', '30d'].includes(value) ? value : '24h';
}

export function readAdminModelTokenQuery(source, locationLike = globalThis.location) {
  const parameters = adminModelTokenQueryParameters[source];
  if (!parameters) return {};
  const search = new URLSearchParams(String(locationLike?.search || ''));
  const result = {};
  for (const [key, parameter] of Object.entries(parameters)) result[key] = String(search.get(parameter) || '').trim();
  for (const key of ['source_missing', 'published', 'sla_overdue']) {
    if (Object.prototype.hasOwnProperty.call(result, key) && result[key] !== 'true' && result[key] !== 'false') result[key] = '';
  }
  return result;
}

export function adminModelTokenQueryUpdates(source, input = {}) {
  const parameters = adminModelTokenQueryParameters[source];
  if (!parameters) return {};
  return Object.fromEntries(Object.entries(parameters).map(([key, parameter]) => [parameter, String(input[key] || '').trim() || null]));
}

export function adminModelTokenOverviewDrilldownUpdates(section, target, key) {
  const resultParameter = adminModelTokenOverviewResultParameters[section];
  if (!resultParameter || !target || target.section !== section || !target.source) return null;
  return {
    ...adminModelTokenQueryUpdates(target.source, target.query),
    ...(target.page || {}),
    [resultParameter]: target.interaction === 'RELATED' ? key : null,
  };
}

export function synchronizeAdminModelTokenCursorHistory(previous = {}, query = {}) {
  const signature = JSON.stringify(Object.entries(query).filter(([key]) => key !== 'cursor').sort(([left], [right]) => left.localeCompare(right)));
  const currentCursor = String(query.cursor || '');
  if (previous.signature !== signature) return { signature, currentCursor, previousByCursor: {} };
  return { ...previous, currentCursor, previousByCursor: { ...(previous.previousByCursor || {}) } };
}

export function rememberAdminModelTokenNextCursor(history, nextCursor) {
  const cursor = String(nextCursor || '');
  if (!cursor) return history;
  return { ...history, currentCursor: cursor, previousByCursor: { ...history.previousByCursor, [cursor]: history.currentCursor || '' } };
}

export function previousAdminModelTokenCursor(history) {
  const current = String(history?.currentCursor || '');
  if (!current || !Object.prototype.hasOwnProperty.call(history?.previousByCursor || {}, current)) return { available: false, cursor: '' };
  return { available: true, cursor: history.previousByCursor[current] || '' };
}

export function clearAdminModelTokenUpdates({ includeLegacy = false } = {}) {
  const keys = [
    ...adminModelTokenPageParameters,
    ...Object.values(adminModelTokenQueryParameters).flatMap((parameters) => Object.values(parameters)),
    ...(includeLegacy ? legacyTokenParameters : []),
  ];
  return Object.fromEntries([...new Set(keys)].map((key) => [key, null]));
}

export function readAdminModelTokenOperationsTab(permissions = [], locationLike = globalThis.location) {
  const visible = adminModelTokenOperationSources(permissions);
  const requested = new URLSearchParams(String(locationLike?.search || '')).get('operations_tab') || '';
  return { tab: visible.includes(requested) ? requested : visible[0] || '', replace: Boolean(visible.length && !visible.includes(requested)), visible };
}

export function readAdminModelTokenModelTab(permissions = [], locationLike = globalThis.location) {
  const granted = new Set(permissions);
  const visible = [
    granted.has('platform.models.read') ? 'directory' : '',
    granted.has('platform.pricing.read') ? 'protocols' : '',
  ].filter(Boolean);
  const requested = new URLSearchParams(String(locationLike?.search || '')).get('model_tab') || '';
  return { tab: visible.includes(requested) ? requested : visible[0] || '', replace: Boolean(visible.length && !visible.includes(requested)), visible };
}

export function normalizeAdminModelTokenEntry(requestedSection, visibleSections, modelTokenIAEnabled, locationLike = globalThis.location, permissions = null, options = {}) {
  const visible = new Set(visibleSections);
  const requested = String(requestedSection || '');
  const search = new URLSearchParams(String(locationLike?.search || ''));
  if (visible.size === 0 && (requested === 'tokens' || adminModelTokenSections.includes(requested) || hasAnyModelTokenState(search))) {
    return { section: 'forbidden', replace: true, updates: clearAdminModelTokenUpdates({ includeLegacy: true }) };
  }
  if (!modelTokenIAEnabled) {
    if (adminModelTokenSections.includes(requested)) {
      return { section: visible.has('tokens') ? 'tokens' : [...visible][0] || 'forbidden', replace: true, updates: clearAdminModelTokenUpdates() };
    }
    return { section: visible.has(requested) ? requested : '', replace: false, updates: {} };
  }
  if (requested === 'tokens') {
    return { section: adminModelTokenSections.find((item) => visible.has(item)) || 'forbidden', replace: true, updates: clearAdminModelTokenUpdates({ includeLegacy: true }) };
  }
  if (adminModelTokenSections.includes(requested) && !visible.has(requested)) {
    return { section: 'forbidden', replace: true, updates: clearAdminModelTokenUpdates({ includeLegacy: true }) };
  }
  if (!adminModelTokenSections.includes(requested)) return { section: visible.has(requested) ? requested : '', replace: false, updates: {} };
  const updates = {};
  let notice = '';
  for (const [section, parameters] of Object.entries(adminModelTokenSectionParameters)) {
    if (section === requested) continue;
    for (const parameter of parameters) if (search.has(parameter)) updates[parameter] = null;
  }
  if (permissions !== null && requested === 'tokenResources') {
    const granted = new Set(permissions);
    const ownerSelectionAllowed = options.tokenResourceFormsEnabled === true
      && granted.has('platform.tokens.manage')
      && granted.has('platform.tokens.owner-candidates.read');
    if (!ownerSelectionAllowed) {
      for (const parameter of Object.values(adminModelTokenQueryParameters.ownerCandidates)) if (search.has(parameter)) updates[parameter] = null;
    }
		if (!granted.has('platform.tokens.read')) {
			clearParameters(adminModelTokenDetailParameters.token, updates);
		} else if (options.tokenDetailsEnabled !== true && search.get('token_account_id')) {
			// The APT-129 route-health drill-down remains the PRE-compatible
			// read-only surface while the independent APT-133 account detail gate
			// is disabled. Strip every new-detail state group and normalize the
			// surviving stable account link to the legacy routes view.
			updates.token_detail_tab = 'routes';
			clearParameters(Object.values(adminModelTokenQueryParameters.tokenCredentials), updates);
			clearParameters(Object.values(adminModelTokenQueryParameters.tokenProvisionings), updates);
			clearParameters(Object.values(adminModelTokenQueryParameters.tokenAudit), updates);
		}
  }
  const clearOrphanOrInvalidDetail = (idKey, parameters) => {
    const id = search.get(idKey) || '';
    const hasNested = parameters.some((parameter) => parameter !== idKey && search.has(parameter));
    if ((id && !uuidPattern.test(id)) || (!id && hasNested)) {
      clearParameters(parameters, updates);
      if (id) notice = 'invalid';
    }
  };
  if (requested === 'modelOperations' && search.get('provisioning_id') && search.get('usage_request_id')) {
    clearParameters(adminModelTokenDetailParameters.provisioning, updates);
    clearParameters(adminModelTokenDetailParameters.usage, updates);
    notice = 'conflict';
  } else {
    if (requested === 'models') clearOrphanOrInvalidDetail('model_id', adminModelTokenDetailParameters.model);
    if (requested === 'tokenResources') clearOrphanOrInvalidDetail('token_account_id', adminModelTokenDetailParameters.token);
    if (requested === 'modelOperations') {
      clearOrphanOrInvalidDetail('provisioning_id', adminModelTokenDetailParameters.provisioning);
      clearOrphanOrInvalidDetail('usage_request_id', adminModelTokenDetailParameters.usage);
    }
  }
  if (permissions !== null && requested === 'modelOperations') {
    const operations = readAdminModelTokenOperationsTab(permissions, locationLike);
    if (operations.replace) updates.operations_tab = operations.tab || null;
    for (const tab of ['provisionings', 'usage', 'reconciliation']) {
      if (!operations.visible.includes(tab)) clearParameters(adminModelTokenTabParameters[tab], updates);
    }
    if (options.modelOperationsDetailsEnabled !== true) {
      clearParameters(adminModelTokenDetailParameters.provisioning, updates);
      clearParameters(adminModelTokenDetailParameters.usage, updates);
    } else {
      if (operations.tab !== 'provisionings' && search.get('provisioning_id')) clearParameters(adminModelTokenDetailParameters.provisioning, updates);
      if (!['usage', 'reconciliation'].includes(operations.tab) && search.get('usage_request_id')) clearParameters(adminModelTokenDetailParameters.usage, updates);
    }
  }
  if (permissions !== null && requested === 'models') {
    const modelTabs = readAdminModelTokenModelTab(permissions, locationLike);
    if (modelTabs.replace) updates.model_tab = modelTabs.tab || null;
    if (!modelTabs.visible.includes('directory')) {
      clearParameters(Object.values(adminModelTokenQueryParameters.models), updates);
      clearParameters(adminModelTokenDetailParameters.model, updates);
    }
    if (!modelTabs.visible.includes('protocols')) {
      clearParameters(Object.values(adminModelTokenQueryParameters.protocols), updates);
      clearParameters(Object.values(adminModelTokenQueryParameters.formulas), updates);
    }
    if (modelTabs.tab !== 'directory') clearParameters(adminModelTokenDetailParameters.model, updates);
    if (options.modelDetailsEnabled !== true) clearParameters(adminModelTokenDetailParameters.model, updates);
  }
	if (permissions !== null && adminModelTokenSections.includes(requested)) {
		const pageOverviews = options.modelTokenPageOverviewsEnabled === true;
		const resultParameter = adminModelTokenOverviewResultParameters[requested];
		const rawResult = resultParameter ? search.get(resultParameter) || '' : '';
		const allowedResults = new Set(pageOverviews ? adminModelTokenOverviewResultKeys[requested] || [] : []);
		if (requested === 'modelOperations') {
			if (!permissions.includes('platform.tokens.read')) allowedResults.delete('provisioning_attention_count');
			if (!permissions.includes('platform.usage.read')) {
				for (const key of ['reconciliation_pending_count', 'reconciliation_stopped_count', 'reconciliation_trend']) allowedResults.delete(key);
			}
		}
		const requiredTab = requested === 'modelOperations' ? adminModelTokenOverviewResultTabs[rawResult] : '';
		const effectiveTab = requested === 'modelOperations' ? readAdminModelTokenOperationsTab(permissions, locationLike).tab : '';
		if (rawResult && (!allowedResults.has(rawResult) || (requiredTab && requiredTab !== effectiveTab))) updates[resultParameter] = null;
		const canReadOverview = pageOverviews
			? requested === 'modelOperations' && permissions.includes('platform.usage.read')
			: options.modelOperationsOverviewEnabled === true
				&& ['platform.models.read', 'platform.tokens.read', 'platform.usage.read'].every((permission) => permissions.includes(permission));
		const overviewWindow = search.get('model_operations_window');
		if (!canReadOverview && overviewWindow !== null) updates.model_operations_window = null;
		else if (canReadOverview && overviewWindow !== null && !['24h', '7d', '30d'].includes(overviewWindow)) updates.model_operations_window = '24h';
	}
  if (permissions !== null && requested === 'models' && search.get('model_id') && updates.model_id !== null) {
    const detailTab = search.get('model_detail_tab') || 'overview';
    const allowedTabs = new Set(['overview', 'capabilities', 'lifecycle']);
    if (permissions.includes('platform.tokens.read')) allowedTabs.add('operations');
    if (permissions.includes('platform.pricing.read')) allowedTabs.add('pricing');
    if (permissions.includes('platform.audit.read')) allowedTabs.add('audit');
    if (!allowedTabs.has(detailTab)) {
      updates.model_detail_tab = 'overview';
      clearParameters(Object.values(adminModelTokenQueryParameters.modelProvisionings), updates);
      clearParameters(Object.values(adminModelTokenQueryParameters.modelAudit), updates);
      clearParameters(Object.values(adminModelTokenQueryParameters.modelHistory), updates);
			clearParameters(Object.values(adminModelTokenQueryParameters.modelLifecyclePlans), updates);
			clearParameters(Object.values(adminModelTokenQueryParameters.modelLifecycleCandidates), updates);
    }
    const effectiveDetailTab = allowedTabs.has(detailTab) ? detailTab : 'overview';
    const canManageModelPrice = options.modelPricingEditEnabled === true && permissions.includes('platform.pricing.read') && permissions.includes('platform.pricing.manage');
    if (effectiveDetailTab !== 'pricing' || !canManageModelPrice) updates.model_price_manage = null;
    if (search.get('model_price_manage') && search.get('model_price_manage') !== '1') updates.model_price_manage = null;
    if (search.get('model_price_return') && search.get('model_price_return') !== 'products') updates.model_price_return = null;
    if (effectiveDetailTab !== 'operations') clearParameters(Object.values(adminModelTokenQueryParameters.modelProvisionings), updates);
    if (effectiveDetailTab !== 'audit') clearParameters(Object.values(adminModelTokenQueryParameters.modelAudit), updates);
    if (effectiveDetailTab !== 'capabilities') clearParameters(Object.values(adminModelTokenQueryParameters.modelHistory), updates);
		if (effectiveDetailTab !== 'lifecycle') {
			clearParameters(Object.values(adminModelTokenQueryParameters.modelLifecyclePlans), updates);
			clearParameters(Object.values(adminModelTokenQueryParameters.modelLifecycleCandidates), updates);
		}
  }
  if (permissions !== null && requested === 'tokenResources' && search.get('token_account_id') && updates.token_account_id !== null) {
    const detailTab = search.get('token_detail_tab') || 'overview';
    const allowedTabs = new Set(['overview', 'credentials', 'routes', 'provisionings']);
    if (permissions.includes('platform.audit.read')) allowedTabs.add('audit');
    if (!allowedTabs.has(detailTab)) {
      updates.token_detail_tab = 'overview';
      clearParameters(Object.values(adminModelTokenQueryParameters.tokenAudit), updates);
    }
		if (detailTab !== 'credentials') clearParameters(Object.values(adminModelTokenQueryParameters.tokenCredentials), updates);
		if (detailTab !== 'routes') clearParameters(Object.values(adminModelTokenQueryParameters.tokenRoutes), updates);
		if (detailTab !== 'provisionings') clearParameters(Object.values(adminModelTokenQueryParameters.tokenProvisionings), updates);
		if (detailTab !== 'audit') clearParameters(Object.values(adminModelTokenQueryParameters.tokenAudit), updates);
  }
  return { section: requested, replace: Object.keys(updates).length > 0, updates, notice };
}

export function normalizeAdminModelTokenNavigation(requestedSection, visibleSections, modelTokenIAEnabled, permissions, queryUpdates = {}, locationLike = globalThis.location, options = {}) {
  const candidateURL = new URL(adminSectionURL(requestedSection, locationLike, queryUpdates), 'https://admin.invalid');
  const candidateLocation = { pathname: candidateURL.pathname, search: candidateURL.search, hash: candidateURL.hash };
  const normalized = normalizeAdminModelTokenEntry(requestedSection, visibleSections, modelTokenIAEnabled, candidateLocation, permissions, options);
  return {
    ...normalized,
    updates: { ...queryUpdates, ...normalized.updates },
  };
}

export function adminModelTokenOverviewRoute(permissions = [], modelTokenIAEnabled = false) {
  const granted = new Set(permissions);
  if (!modelTokenIAEnabled) return granted.has('platform.tokens.read') || granted.has('platform.usage.read') ? { section: 'tokens', query: {} } : null;
  if (granted.has('platform.tokens.read')) return { section: 'tokenResources', query: {} };
  if (granted.has('platform.usage.read')) return { section: 'modelOperations', query: { operations_tab: 'usage' } };
  return null;
}

export function readAdminSection(locationLike = globalThis.location) {
  if (!locationLike) return '';
  return new URLSearchParams(String(locationLike.search || '')).get('section') || '';
}

const organizationTabs = new Set(['overview', 'members', 'units', 'lifecycle']);
const financeTabs = Object.freeze({
  billing: new Set(['wallets', 'receivables', 'adjustments', 'holds']),
  payments: new Set(['orders', 'refunds', 'lateReviews', 'anomalies', 'reconciliations', 'controls']),
  products: new Set(['catalog', 'prices']),
});
const financeDefaultTabs = Object.freeze({ billing: 'wallets', payments: 'orders', products: 'catalog' });
const financeQueryParameters = Object.freeze({
  wallets: Object.freeze({ owner_type: 'wallet_owner_type', status: 'wallet_status', q: 'wallet_q', limit: 'wallet_limit', cursor: 'wallet_cursor' }),
  receivables: Object.freeze({ wallet_id: 'receivable_wallet_id', status: 'receivable_status', source_type: 'receivable_source_type', created_from: 'receivable_created_from', created_to: 'receivable_created_to', limit: 'receivable_limit', cursor: 'receivable_cursor' }),
  adjustments: Object.freeze({ wallet_id: 'adjustment_wallet_id', status: 'adjustment_status', requested_by: 'adjustment_requested_by', created_from: 'adjustment_created_from', created_to: 'adjustment_created_to', limit: 'adjustment_limit', cursor: 'adjustment_cursor' }),
  holds: Object.freeze({ wallet_id: 'hold_wallet_id', status: 'hold_status', model_id: 'hold_model_id', created_from: 'hold_created_from', created_to: 'hold_created_to', limit: 'hold_limit', cursor: 'hold_cursor' }),
  orders: Object.freeze({ q: 'payment_q', status: 'payment_status', provider_id: 'payment_provider_id', channel: 'payment_channel', created_from: 'payment_created_from', created_to: 'payment_created_to', limit: 'payment_limit', cursor: 'payment_cursor' }),
  refunds: Object.freeze({ q: 'refund_q', status: 'refund_status', provider_id: 'refund_provider_id', created_from: 'refund_created_from', created_to: 'refund_created_to', limit: 'refund_limit', cursor: 'refund_cursor' }),
  lateReviews: Object.freeze({
    q: 'late_q',
    status: 'late_status',
    severity: 'late_severity',
    provider_id: 'late_provider_id',
    owner_id: 'late_owner_id',
    created_from: 'late_created_from',
    created_to: 'late_created_to',
    entered_from: 'late_entered_from',
    entered_to: 'late_entered_to',
    next_review_before: 'late_next_review_before',
    overdue: 'late_overdue',
    limit: 'late_limit',
    cursor: 'late_cursor',
  }),
  anomalies: Object.freeze({ q: 'anomaly_q', anomaly_type: 'anomaly_type', status: 'anomaly_status', severity: 'anomaly_severity', owner_id: 'anomaly_owner_id', provider_id: 'anomaly_provider_id', created_from: 'anomaly_created_from', created_to: 'anomaly_created_to', limit: 'anomaly_limit', cursor: 'anomaly_cursor' }),
  reconciliations: Object.freeze({ provider_id: 'reconciliation_provider_id', channel: 'reconciliation_channel', status: 'reconciliation_status', business_date_from: 'reconciliation_business_date_from', business_date_to: 'reconciliation_business_date_to', limit: 'reconciliation_limit', cursor: 'reconciliation_cursor' }),
});
const financeQueryDefaults = Object.freeze({
  wallets: Object.freeze({ owner_type: '', status: '', q: '', limit: '50', cursor: '' }),
  receivables: Object.freeze({ wallet_id: '', status: '', source_type: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  adjustments: Object.freeze({ wallet_id: '', status: '', requested_by: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  holds: Object.freeze({ wallet_id: '', status: '', model_id: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  orders: Object.freeze({ q: '', status: '', provider_id: '', channel: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  refunds: Object.freeze({ q: '', status: '', provider_id: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  lateReviews: Object.freeze({ q: '', status: '', severity: '', provider_id: '', owner_id: '', created_from: '', created_to: '', entered_from: '', entered_to: '', next_review_before: '', overdue: false, limit: '50', cursor: '' }),
  anomalies: Object.freeze({ q: '', anomaly_type: '', status: 'OPEN', severity: '', owner_id: '', provider_id: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  reconciliations: Object.freeze({ provider_id: '', channel: '', status: '', business_date_from: '', business_date_to: '', limit: '50', cursor: '' }),
});
const financeDetailQueryParameters = Object.freeze({
  ledger: Object.freeze({ entry_type: 'ledger_entry_type', business_reference: 'ledger_business_reference', created_from: 'ledger_created_from', created_to: 'ledger_created_to', limit: 'ledger_limit', cursor: 'ledger_cursor' }),
  reconciliationCases: Object.freeze({ status: 'case_status', severity: 'case_severity', owner_id: 'case_owner_id', case_type: 'case_type', created_from: 'case_created_from', created_to: 'case_created_to', limit: 'case_limit', cursor: 'case_cursor' }),
});
const financeDetailQueryDefaults = Object.freeze({
  ledger: Object.freeze({ entry_type: '', business_reference: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
  reconciliationCases: Object.freeze({ status: '', severity: '', owner_id: '', case_type: '', created_from: '', created_to: '', limit: '50', cursor: '' }),
});
const financeListLimits = new Set(['25', '50', '100', '200']);
const financeFilterValues = Object.freeze({
  wallets: Object.freeze({ owner_type: new Set(['', 'PERSONAL', 'ORGANIZATION', 'ORG_UNIT']), status: new Set(['', 'ACTIVE', 'FROZEN', 'CLOSED']) }),
  receivables: Object.freeze({ status: new Set(['', 'OPEN', 'PARTIALLY_SETTLED', 'SETTLED', 'WRITTEN_OFF']) }),
  adjustments: Object.freeze({ status: new Set(['', 'PENDING_APPROVAL', 'APPROVED', 'REJECTED', 'POSTED']) }),
  holds: Object.freeze({ status: new Set(['', 'ACTIVE', 'COMMITTED', 'RELEASED', 'EXPIRED', 'REVIEW']) }),
  orders: Object.freeze({ status: new Set(['', 'CREATED', 'PENDING', 'PAID', 'PAID_LATE', 'FAILED', 'CLOSED', 'EXPIRED', 'MANUAL_REVIEW']) }),
  refunds: Object.freeze({ status: new Set(['', 'REQUESTED', 'SUCCEEDED', 'FAILED', 'MANUAL_REVIEW', 'REVIEW_REQUIRED', 'RESOLVED']) }),
  lateReviews: Object.freeze({ status: new Set(['', 'REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND', 'ARCHIVED_CONFIRMED_UNPAID', 'RESOLVED_PAID']), severity: new Set(['', 'P0', 'P1', 'P2']) }),
  anomalies: Object.freeze({
    anomaly_type: new Set(['', 'CHARGEBACK', 'FORCED_REFUND', 'DUPLICATE_PAYMENT', 'LATE_PAYMENT_CONFLICT', 'PAYMENT_FACT_CONFLICT', 'REFUND_FAILURE', 'ILLEGAL_TRANSITION']),
    status: new Set(['', 'OPEN', 'RESOLVED']),
    severity: new Set(['', 'P0', 'P1', 'P2']),
  }),
  reconciliations: Object.freeze({ status: new Set(['', 'RUNNING', 'COMPLETED', 'FAILED']) }),
});
const financeDetailFilterValues = Object.freeze({
  ledger: Object.freeze({
    entry_type: new Set(['', 'RECHARGE', 'USAGE', 'REFUND', 'REVERSAL', 'ADJUSTMENT', 'EXPIRY', 'RECEIVABLE', 'CHARGEBACK']),
  }),
  reconciliationCases: Object.freeze({
    status: new Set(['', 'OPEN', 'IN_REVIEW', 'RESOLVED', 'IGNORED']),
    severity: new Set(['', 'P0', 'P1', 'P2']),
    case_type: new Set(['', 'PROVIDER_PAID_INTERNAL_MISSING', 'INTERNAL_PAID_PROVIDER_MISSING', 'AMOUNT_MISMATCH', 'DUPLICATE_TRANSACTION', 'PROVIDER_REFUND_INTERNAL_MISSING', 'INTERNAL_REFUND_PROVIDER_MISSING', 'REFUND_AMOUNT_MISMATCH', 'PROVIDER_CHARGEBACK_INTERNAL_MISSING', 'INTERNAL_CHARGEBACK_PROVIDER_MISSING', 'PAYMENT_LEDGER_MISSING', 'REFUND_LEDGER_MISSING', 'CHARGEBACK_LEDGER_MISSING', 'UNSUPPORTED_PROVIDER_ENTRY']),
  }),
});

export const financePaginatedTabs = Object.freeze(Object.keys(financeQueryParameters));

export function isAdminFinancePaginatedTab(tab) {
  return Object.prototype.hasOwnProperty.call(financeQueryParameters, tab);
}

export function readAdminFinanceTab(section, locationLike = globalThis.location) {
  const requestedTab = new URLSearchParams(String(locationLike?.search || '')).get('finance_tab') || '';
  return financeTabs[section]?.has(requestedTab) ? requestedTab : financeDefaultTabs[section] || '';
}

export function normalizeAdminFinanceQuery(tab, input = {}) {
  const defaults = financeQueryDefaults[tab] || {};
  const normalized = { ...defaults };
  for (const key of Object.keys(defaults)) {
    if (key === 'overdue') normalized[key] = input[key] === true || input[key] === 'true';
    else if (key === 'limit') normalized[key] = financeListLimits.has(String(input[key] || '')) ? String(input[key]) : defaults[key];
    else normalized[key] = String(input[key] || '').trim();
  }
  for (const [key, allowed] of Object.entries(financeFilterValues[tab] || {})) {
    if (!allowed.has(normalized[key])) normalized[key] = defaults[key];
  }
  for (const key of ['created_from', 'created_to', 'entered_from', 'entered_to', 'next_review_before']) {
    if (Object.prototype.hasOwnProperty.call(normalized, key)) {
      if (normalized[key] && (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(normalized[key]) || Number.isNaN(new Date(normalized[key]).getTime()))) normalized[key] = '';
    }
  }
  for (const key of ['business_date_from', 'business_date_to']) {
    if (Object.prototype.hasOwnProperty.call(normalized, key) && normalized[key] && !/^\d{4}-\d{2}-\d{2}$/.test(normalized[key])) normalized[key] = '';
  }
  return normalized;
}

export function normalizeAdminFinanceDetailQuery(type, input = {}) {
  const defaults = financeDetailQueryDefaults[type] || {};
  const normalized = { ...defaults };
  for (const key of Object.keys(defaults)) {
    if (key === 'limit') normalized[key] = financeListLimits.has(String(input[key] || '')) ? String(input[key]) : defaults[key];
    else normalized[key] = String(input[key] || '').trim();
  }
  for (const [key, allowed] of Object.entries(financeDetailFilterValues[type] || {})) {
    if (!allowed.has(normalized[key])) normalized[key] = defaults[key];
  }
  for (const key of ['created_from', 'created_to']) {
    if (normalized[key] && (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}$/.test(normalized[key]) || Number.isNaN(new Date(normalized[key]).getTime()))) normalized[key] = '';
  }
  return normalized;
}

export function readAdminFinanceQuery(section, tab, locationLike = globalThis.location) {
  if (!financeTabs[section]?.has(tab) || !financeQueryParameters[tab]) return {};
  const search = new URLSearchParams(String(locationLike?.search || ''));
  const raw = {};
  for (const [key, parameter] of Object.entries(financeQueryParameters[tab])) raw[key] = search.get(parameter) || '';
  return normalizeAdminFinanceQuery(tab, raw);
}

export function adminFinanceQueryUpdates(tab, input = {}) {
  if (!financeQueryParameters[tab]) return {};
  const query = normalizeAdminFinanceQuery(tab, input);
  return Object.fromEntries(Object.entries(financeQueryParameters[tab]).map(([key, parameter]) => [
    parameter,
    key === 'overdue' ? (query[key] ? 'true' : null) : query[key] || null,
  ]));
}

export function readAdminFinanceDetail(section, tab, locationLike = globalThis.location) {
  const search = new URLSearchParams(String(locationLike?.search || ''));
  const type = search.get('finance_detail') || '';
  const id = search.get('finance_detail_id') || '';
  const valid = (section === 'billing' && tab === 'wallets' && type === 'ledger')
    || (section === 'payments' && tab === 'reconciliations' && type === 'reconciliationCases');
  if (!valid || !id || !financeDetailQueryParameters[type]) return { type: '', id: '', query: {} };
  const raw = {};
  for (const [key, parameter] of Object.entries(financeDetailQueryParameters[type])) raw[key] = search.get(parameter) || '';
  return { type, id, query: normalizeAdminFinanceDetailQuery(type, raw) };
}

export function adminFinanceDetailUpdates(type, id, input = {}) {
  const updates = clearAdminFinanceDetailUpdates();
  if (!financeDetailQueryParameters[type] || !id) return updates;
  const query = normalizeAdminFinanceDetailQuery(type, input);
  updates.finance_detail = type;
  updates.finance_detail_id = id;
  for (const [key, parameter] of Object.entries(financeDetailQueryParameters[type])) updates[parameter] = query[key] || null;
  return updates;
}

export function clearAdminFinanceDetailUpdates() {
  return {
    finance_detail: null,
    finance_detail_id: null,
    ...Object.fromEntries(Object.values(financeDetailQueryParameters).flatMap((parameters) => Object.values(parameters)).map((parameter) => [parameter, null])),
  };
}

export function clearAdminFinanceQueryUpdates() {
  return Object.fromEntries(Object.values(financeQueryParameters).flatMap((parameters) => Object.values(parameters)).map((parameter) => [parameter, null]));
}

export function resolveAdminFinanceTab(requestedTab, currentTab, visibleTabs = []) {
  if (visibleTabs.includes(requestedTab)) return { tab: requestedTab, replace: false };
  const fallback = visibleTabs.includes(currentTab) ? currentTab : visibleTabs[0] || '';
  return { tab: fallback, replace: Boolean(fallback && fallback !== requestedTab) };
}

export function readAdminOrganizationRoute(locationLike = globalThis.location) {
  const search = new URLSearchParams(String(locationLike?.search || ''));
  const organizationID = search.get('organization_id') || '';
  const requestedTab = search.get('organization_tab') || 'overview';
  return {
    organizationID,
    tab: organizationTabs.has(requestedTab) ? requestedTab : 'overview',
  };
}

export function readAdminOrganizationDirectory(locationLike = globalThis.location) {
  const search = new URLSearchParams(String(locationLike?.search || ''));
  const requestedStatus = search.get('organization_status') || '';
  return {
    q: search.get('organization_q') || '',
    status: ['ACTIVE', 'DISABLED', 'DELETED'].includes(requestedStatus) ? requestedStatus : '',
    cursor: search.get('organization_cursor') || '',
  };
}

export function adminSectionURL(section, locationLike = globalThis.location, queryUpdates = {}) {
  const pathname = String(locationLike?.pathname || '/admin.html');
  const search = new URLSearchParams(String(locationLike?.search || ''));
  search.set('section', section);
  for (const [key, value] of Object.entries(queryUpdates || {})) {
    if (value === null || value === undefined || value === '') search.delete(key);
    else search.set(key, String(value));
  }
  const query = search.toString();
  return `${pathname}${query ? `?${query}` : ''}${String(locationLike?.hash || '')}`;
}

export function writeAdminSection(section, mode = 'push', historyLike = globalThis.history, locationLike = globalThis.location, queryUpdates = {}) {
  if (!historyLike || !section) return;
  const url = adminSectionURL(section, locationLike, queryUpdates);
  if (mode === 'replace') historyLike.replaceState({ section }, '', url);
  else historyLike.pushState({ section }, '', url);
}
