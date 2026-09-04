import React, { useEffect, useMemo, useRef, useState } from 'react';
import { adminModelTokenQueryUpdates, readAdminModelTokenModelTab, readAdminModelTokenOperationsTab, readAdminModelTokenQuery } from './admin-navigation.js';
import { planAdminModelTokenOperationRequests } from './admin-model-token-state.js';
import { AdminProtocolFormulaWorkspace } from './AdminProtocolFormulaWorkspace.jsx';
import { AdminTokenResourcesWorkspace } from './AdminTokenResourcesWorkspace.jsx';
import { AdminModelOperationsWorkspace } from './AdminModelOperationsWorkspace.jsx';
import { FilterForm, Pagination, SourceFeedback, SourceHeader, Status, useAdminSource } from './AdminModelTokenPrimitives.jsx';
import { AdminHighRiskActionDialog } from './AdminHighRiskActionDialog.jsx';
import { governedTokenEnvelope, requireGovernedTokenCommandResult, requireGovernedTokenPrecheck, requireTokenApprovalImpact, validApprovalRequest } from './admin-high-risk-action-state.js';

function tokenApprovalDraft(commandType, targetResourceType, targetResourceID, reason, impact, idempotencyKey, normalizedCommand) {
  return {
    command_type: commandType, idempotency_key: idempotencyKey, normalized_command: normalizedCommand,
    target_resource_type: targetResourceType, target_resource_id: targetResourceID, reason,
    redacted_evidence: { precheck_fingerprint: impact.precheck_fingerprint, token_observed_at: impact.token_observed_at, billing_observed_at: impact.billing_observed_at || null },
    expected_impact: { blockers: impact.blockers, dependencies: impact.evidence.dependencies },
    critical_resource_versions: impact.critical_resource_versions,
    financial_snapshot: impact.financial_snapshot,
  };
}

async function createAndSubmitTokenApproval(client, draft, signal) {
  const created = await client.createApprovalDraft(draft, signal);
  if (!validApprovalRequest(created, draft, ['DRAFT'])) throw Object.assign(new Error('invalid approval draft response'), { code: 'INVALID_APPROVAL_RESPONSE' });
  try {
    const submitted = await client.submitApprovalRequest(created.approval_request_id, created.version, signal);
    if (!validApprovalRequest(submitted, draft, ['SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTED'])) throw Object.assign(new Error('invalid approval submit response'), { code: 'INVALID_APPROVAL_RESPONSE' });
    return submitted;
  } catch (error) {
    throw Object.assign(error instanceof Error ? error : new Error('approval submit failed'), { commandReference: created.approval_request_id });
  }
}

async function recoverTokenApproval(client, reference, draft, signal) {
  const value = await client.approvalRequest(reference, signal);
  const statuses = ['DRAFT', 'SUBMITTED', 'APPROVED', 'REJECTED', 'WITHDRAWN', 'EXPIRED', 'INVALIDATED', 'EXECUTING', 'EXECUTED', 'EXECUTION_FAILED'];
  if (!validApprovalRequest(value, draft, statuses)) throw Object.assign(new Error('invalid approval recovery response'), { code: 'INVALID_APPROVAL_RESPONSE', commandReference: reference });
  if (['SUBMITTED', 'APPROVED', 'EXECUTING', 'EXECUTED'].includes(value.status)) return value;
  if (value.status === 'DRAFT') throw Object.assign(new Error('approval submission outcome remains unknown'), { status: 202, code: 'COMMAND_RESULT_UNKNOWN', commandReference: reference });
  throw Object.assign(new Error('approval reached a terminal non-success state'), { code: `APPROVAL_${value.status}`, commandReference: reference, finalResult: true });
}

function ModelDirectory({ client, copy, permissions, actionsEnabled, routeVersion, onQueryChange, cursorHistory }) {
  const query = useMemo(() => readAdminModelTokenQuery('models'), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [action, setAction] = useState(null);
  const canPublish = actionsEnabled && permissions.includes('platform.models.manage');
	const canSync = actionsEnabled && permissions.includes('platform.models.sync');
	const canLifecycle = actionsEnabled && permissions.includes('platform.models.lifecycle.request');
	const canRollback = actionsEnabled && permissions.includes('platform.models.catalog.rollback.request');
	const activeActionAllowed = !action || (action.type === 'PUBLISH' && canPublish) || (action.type === 'SYNC' && canSync)
		|| (action.type === 'LIFECYCLE' && canLifecycle) || (action.type === 'ROLLBACK' && canRollback);
  useEffect(() => {
		if (action && !activeActionAllowed) setAction(null);
	}, [action, activeActionAllowed]);
  const load = useMemo(() => (filters, signal) => client.catalogModels(filters, signal), [client]);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion });
  const retry = () => {
    if (state.error?.status === 410) {
      delete cursorHistory.current.models;
      onQueryChange('models', { ...query, cursor: '' }, 'replace');
    } else setRefreshVersion((value) => value + 1);
  };
  return (
    <section className="adm-card adm-source-card" aria-labelledby="model-directory-title">
			<SourceHeader title={copy.modelDirectory} titleID="model-directory-title" state={state} copy={copy} onRetry={retry} actions={actionsEnabled && <>{canSync && <button type="button" onClick={() => setAction({ type: 'SYNC' })}>{copy.syncCatalog}</button>}{canRollback && <button type="button" onClick={() => setAction({ type: 'ROLLBACK', sourceVersion: '' })}>{copy.rollbackCatalog}</button>}</>} />
      <FilterForm source="models" query={query} copy={copy} onQueryChange={onQueryChange} fields={[
        { key: 'q', label: copy.modelSearch, placeholder: copy.modelSearchPlaceholder },
        { key: 'lifecycle_status', label: copy.lifecycle, options: ['PUBLISHED', 'DEPRECATED', 'REMOVED'] },
        { key: 'protocol_code', label: copy.protocol },
        { key: 'source_adapter', label: copy.sourceAdapter },
      ]} />
      <SourceFeedback state={state} copy={copy} onRetry={retry} />
      {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.model}</th><th>{copy.lifecycle}</th><th>{copy.protocol}</th><th>{copy.contextLength}</th><th>{copy.source}</th>{actionsEnabled && <th>{copy.actions}</th>}</tr></thead><tbody>
				{state.items.map((item) => <tr key={item.model_id}><td data-label={copy.model}><strong>{item.display_name}</strong><small className="adm-secondary-id">{item.model_id}</small></td><td data-label={copy.lifecycle}><Status value={item.lifecycle_status} copy={copy} /></td><td data-label={copy.protocol}>{item.protocol_code} · v{item.protocol_version}</td><td data-label={copy.contextLength}>{Number(item.context_length || 0).toLocaleString()}</td><td data-label={copy.source}><Status value={item.source_available ? 'ACTIVE' : 'UNAVAILABLE'} copy={copy} /></td>{actionsEnabled && <td data-label={copy.actions}><div className="adm-row-actions">{canPublish && item.draft && <button type="button" onClick={() => setAction({ type: 'PUBLISH', item })}>{copy.publish}</button>}{canLifecycle && item.lifecycle_status === 'PUBLISHED' && <button type="button" onClick={() => setAction({ type: 'LIFECYCLE', targetStatus: 'DEPRECATED', item })}>{copy.deprecateModel}</button>}{canLifecycle && item.lifecycle_status !== 'REMOVED' && <button type="button" onClick={() => setAction({ type: 'LIFECYCLE', targetStatus: 'REMOVED', item })}>{copy.removeModel}</button>}</div></td>}</tr>)}
      </tbody></table></div>}
      <Pagination source="models" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} />
			{action?.type === 'PUBLISH' && action.item && canPublish && <AdminHighRiskActionDialog action={{ key: `model-publish-${action.item.model_id}`, impactAction: 'TOKEN_MODEL_PUBLISH', title: copy.publishModel, lead: copy.modelActionLead }} copy={copy} target={{ label: action.item.display_name, version: `${copy.catalogVersion}: ${action.item.catalog_version}`, effectiveAt: copy.actionImmediate }} policy="AUDIT_ONLY" recentAuth payloadDetails={{ model_id: action.item.model_id, expected_catalog_version: action.item.catalog_version, expected_draft_version: action.item.draft?.draft_version || 0 }} impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_MODEL_PUBLISH', target_resource_type: 'MODEL', target_resource_id: action.item.model_id, command: { model_id: action.item.model_id } }, signal), { commandType: 'TOKEN_MODEL_PUBLISH', targetResourceType: 'MODEL', targetResourceID: action.item.model_id })} onClose={() => setAction(null)} execute={async (payload, key, signal) => {
				const result = await client.createGovernedTokenCommand(await governedTokenEnvelope({ commandType: 'TOKEN_MODEL_PUBLISH', targetResourceType: 'MODEL', targetResourceID: action.item.model_id, reason: payload.reason, impact: payload.precheck, command: { model_id: action.item.model_id } }), key, signal);
        return requireGovernedTokenCommandResult(result, { commandType: 'TOKEN_MODEL_PUBLISH', targetResourceType: 'MODEL', targetResourceID: action.item.model_id });
      }} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_MODEL_PUBLISH', targetResourceType: 'MODEL', targetResourceID: action.item.model_id })} onCompleted={() => setRefreshVersion((value) => value + 1)} />}
			{action?.type === 'SYNC' && canSync && <AdminHighRiskActionDialog action={{ key: 'model-catalog-sync', impactAction: 'TOKEN_MODEL_CATALOG_SYNC', title: copy.syncCatalog, lead: copy.modelActionLead }} copy={copy} target={{ label: copy.modelDirectory, version: copy.serverResolvedBeforeSubmit }} policy="AUDIT_ONLY" impactLoader={async (signal) => requireGovernedTokenPrecheck(await client.governedTokenCommandPrecheck({ command_type: 'TOKEN_MODEL_CATALOG_SYNC', target_resource_type: 'MODEL_CATALOG', target_resource_id: 'CURRENT', command: {} }, signal), { commandType: 'TOKEN_MODEL_CATALOG_SYNC', targetResourceType: 'MODEL_CATALOG', targetResourceID: 'CURRENT' })} onClose={() => setAction(null)} execute={async (payload, key, signal) => requireGovernedTokenCommandResult(await client.createGovernedTokenCommand(await governedTokenEnvelope({ commandType: 'TOKEN_MODEL_CATALOG_SYNC', targetResourceType: 'MODEL_CATALOG', targetResourceID: 'CURRENT', reason: payload.reason, impact: payload.precheck, command: {} }), key, signal), { commandType: 'TOKEN_MODEL_CATALOG_SYNC', targetResourceType: 'MODEL_CATALOG', targetResourceID: 'CURRENT' })} recover={async (reference, signal) => requireGovernedTokenCommandResult(await client.governedTokenCommandResult(reference, signal), { commandType: 'TOKEN_MODEL_CATALOG_SYNC', targetResourceType: 'MODEL_CATALOG', targetResourceID: 'CURRENT' })} onCompleted={() => setRefreshVersion((value) => value + 1)} />}
			{action?.type === 'LIFECYCLE' && action.item && canLifecycle && <AdminHighRiskActionDialog action={{ key: `model-lifecycle-${action.item.model_id}-${action.targetStatus}`, impactAction: `MODEL_LIFECYCLE_${action.targetStatus}`, title: action.targetStatus === 'DEPRECATED' ? copy.deprecateModel : copy.removeModel, lead: copy.modelActionLead }} copy={copy} target={{ label: action.item.display_name, version: `${copy.catalogVersion}: ${action.item.catalog_version}`, effectiveAt: copy.actionImmediate }} policy="APPROVAL_REQUIRED" recentAuth dangerous={action.targetStatus === 'REMOVED'} impactLoader={async (signal) => requireTokenApprovalImpact(await client.modelLifecycleImpact(action.item.model_id, action.targetStatus, '', signal), { action: `MODEL_LIFECYCLE_${action.targetStatus}`, commandType: 'TOKEN_MODEL_LIFECYCLE_CHANGE', targetResourceID: action.item.model_id, targetStatus: action.targetStatus })} onClose={() => setAction(null)} execute={async (payload, key, signal) => {
				const snapshot = payload.precheck.critical_resource_versions;
				const normalized = { model_id: action.item.model_id, target_lifecycle: action.targetStatus, effective_at: snapshot.effective_at, existing_session_cutoff_at: snapshot.existing_session_cutoff_at, replacement_model_id: snapshot.replacement_model_id };
				return createAndSubmitTokenApproval(client, tokenApprovalDraft('TOKEN_MODEL_LIFECYCLE_CHANGE', 'MODEL', action.item.model_id, payload.reason, payload.precheck, key, normalized), signal);
			}} recover={(reference, signal, payload) => {
				const snapshot = payload.precheck.critical_resource_versions;
				return recoverTokenApproval(client, reference, tokenApprovalDraft('TOKEN_MODEL_LIFECYCLE_CHANGE', 'MODEL', action.item.model_id, payload.reason, payload.precheck, '', { model_id: action.item.model_id, target_lifecycle: action.targetStatus, effective_at: snapshot.effective_at, existing_session_cutoff_at: snapshot.existing_session_cutoff_at, replacement_model_id: snapshot.replacement_model_id }), signal);
			}} onCompleted={() => setRefreshVersion((value) => value + 1)} />}
			{action?.type === 'ROLLBACK' && canRollback && <AdminHighRiskActionDialog action={{ key: `model-catalog-rollback-${action.sourceVersion}`, impactAction: 'MODEL_CATALOG_ROLLBACK', title: copy.rollbackCatalog, lead: copy.rollbackCatalogLead }} copy={copy} target={{ label: copy.modelDirectory, version: action.sourceVersion || copy.serverResolvedBeforeSubmit }} policy="APPROVAL_REQUIRED" recentAuth dangerous impactLoader={async (signal) => {
				const sourceVersion = Number(action.sourceVersion);
				if (!Number.isInteger(sourceVersion) || sourceVersion < 1) throw Object.assign(new Error('rollback source required'), { code: 'INVALID_REQUEST' });
				return requireTokenApprovalImpact(await client.catalogRollbackImpact(sourceVersion, signal), { action: 'MODEL_CATALOG_ROLLBACK', commandType: 'TOKEN_MODEL_CATALOG_ROLLBACK', sourceCatalogVersion: sourceVersion });
			}} onClose={() => setAction(null)} execute={(payload, key, signal) => createAndSubmitTokenApproval(client, tokenApprovalDraft('TOKEN_MODEL_CATALOG_ROLLBACK', 'MODEL_CATALOG', String(action.sourceVersion), payload.reason, payload.precheck, key, { source_catalog_version: Number(action.sourceVersion) }), signal)} recover={(reference, signal, payload) => recoverTokenApproval(client, reference, tokenApprovalDraft('TOKEN_MODEL_CATALOG_ROLLBACK', 'MODEL_CATALOG', String(action.sourceVersion), payload.reason, payload.precheck, '', { source_catalog_version: Number(action.sourceVersion) }), signal)} onCompleted={() => setRefreshVersion((value) => value + 1)}>
				<label>{copy.rollbackSource}<input type="number" min="1" required value={action.sourceVersion} onChange={(event) => setAction((current) => ({ ...current, sourceVersion: event.target.value }))} /></label>
			</AdminHighRiskActionDialog>}
    </section>
  );
}

export function AdminModelTokenWorkspace({ section, copy, client, permissions, routeVersion, onRouteChange, onNavigate, routeNotice, tokenResourceFormsEnabled = false, modelOperationsDetailsEnabled = false, modelTokenActionsEnabled = false }) {
  const modelTokenCopy = copy.modelToken;
  const granted = new Set(permissions || []);
  const cursorHistory = useRef({});
  const operations = useMemo(() => readAdminModelTokenOperationsTab(permissions), [permissions, routeVersion]);
  const modelTabs = useMemo(() => readAdminModelTokenModelTab(permissions), [permissions, routeVersion]);
  const operationRequests = useMemo(() => planAdminModelTokenOperationRequests(permissions, client), [permissions, client]);
  const onQueryChange = (source, query, mode = 'push') => onRouteChange(adminModelTokenQueryUpdates(source, query), mode);
  const notice = routeNotice === 'invalid' ? modelTokenCopy.invalidLink : routeNotice === 'conflict' ? modelTokenCopy.conflictingDetail : '';
  const moveOperationsTab = (event) => {
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return;
    const tabs = [...event.currentTarget.querySelectorAll('[role="tab"]')];
    const current = tabs.indexOf(document.activeElement);
    if (current < 0 || tabs.length < 2) return;
    event.preventDefault();
    const target = event.key === 'Home' ? 0 : event.key === 'End' ? tabs.length - 1 : (current + (event.key === 'ArrowRight' ? 1 : -1) + tabs.length) % tabs.length;
    tabs[target].focus();
    tabs[target].click();
  };
  return <>
    {notice && <div className="adm-inline-message info" role="status">{notice}</div>}
    {!modelTokenActionsEnabled && <div className="adm-inline-message info" role="status">{modelTokenCopy.actionsReadOnly}</div>}
    {section === 'models' && <>
      <div className="adm-tabs adm-model-tabs" role="tablist" aria-label={modelTokenCopy.modelViews} onKeyDown={moveOperationsTab}>
        {modelTabs.visible.map((tab) => <button type="button" role="tab" id={`model-tab-${tab}`} aria-controls={`model-panel-${tab}`} aria-selected={modelTabs.tab === tab} tabIndex={modelTabs.tab === tab ? 0 : -1} className={modelTabs.tab === tab ? 'active' : ''} key={tab} onClick={() => onRouteChange({ model_tab: tab }, 'push')}>{tab === 'directory' ? modelTokenCopy.modelDirectory : modelTokenCopy.protocolFormulaView}</button>)}
      </div>
      {modelTabs.tab === 'directory' && granted.has('platform.models.read') && <div role="tabpanel" id="model-panel-directory" aria-labelledby="model-tab-directory"><ModelDirectory client={client} copy={modelTokenCopy} permissions={permissions} actionsEnabled={modelTokenActionsEnabled} routeVersion={routeVersion} onQueryChange={onQueryChange} cursorHistory={cursorHistory} /></div>}
			{modelTabs.tab === 'protocols' && granted.has('platform.pricing.read') && <div role="tabpanel" id="model-panel-protocols" aria-labelledby="model-tab-protocols"><AdminProtocolFormulaWorkspace client={client} copy={modelTokenCopy} canManage={modelTokenActionsEnabled && granted.has('platform.pricing.manage')} routeVersion={routeVersion} onRouteChange={onRouteChange} cursorHistory={cursorHistory} /></div>}
    </>}
    {section === 'tokenResources' && <AdminTokenResourcesWorkspace client={client} copy={modelTokenCopy} permissions={permissions} formsEnabled={tokenResourceFormsEnabled} actionsEnabled={modelTokenActionsEnabled} routeVersion={routeVersion} onQueryChange={onQueryChange} onRouteChange={onRouteChange} histories={cursorHistory} />}
    {section === 'modelOperations' && <>
      <div className="adm-tabs" role="tablist" aria-label={modelTokenCopy.operationsViews} onKeyDown={moveOperationsTab}>
        {operations.visible.map((tab) => <button type="button" role="tab" id={`model-operations-tab-${tab}`} aria-controls={`model-operations-panel-${tab}`} aria-selected={operations.tab === tab} tabIndex={operations.tab === tab ? 0 : -1} className={operations.tab === tab ? 'active' : ''} key={tab} onClick={() => onRouteChange({ operations_tab: tab }, 'push')}>{tab === 'provisionings' ? modelTokenCopy.provisionings : tab === 'usage' ? modelTokenCopy.usageRequests : modelTokenCopy.reconciliationQueue}</button>)}
      </div>
      <AdminModelOperationsWorkspace client={client} copy={modelTokenCopy} permissions={permissions} detailsEnabled={modelOperationsDetailsEnabled} actionsEnabled={modelTokenActionsEnabled} routeVersion={routeVersion} operations={operations} requests={operationRequests} onRouteChange={onRouteChange} onQueryChange={onQueryChange} onNavigate={onNavigate} histories={cursorHistory} />
    </>}
  </>;
}
