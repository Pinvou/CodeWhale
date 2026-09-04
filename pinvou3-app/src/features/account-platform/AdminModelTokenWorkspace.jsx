import React, { useMemo, useRef, useState } from 'react';
import { adminModelTokenQueryUpdates, readAdminModelTokenModelTab, readAdminModelTokenOperationsTab, readAdminModelTokenQuery } from './admin-navigation.js';
import { planAdminModelTokenOperationRequests } from './admin-model-token-state.js';
import { AdminProtocolFormulaWorkspace } from './AdminProtocolFormulaWorkspace.jsx';
import { AdminTokenResourcesWorkspace } from './AdminTokenResourcesWorkspace.jsx';
import { AdminModelOperationsWorkspace } from './AdminModelOperationsWorkspace.jsx';
import { FilterForm, Pagination, SourceFeedback, SourceHeader, Status, useAdminSource } from './AdminModelTokenPrimitives.jsx';

function ModelDirectory({ client, copy, routeVersion, onQueryChange, cursorHistory }) {
  const query = useMemo(() => readAdminModelTokenQuery('models'), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
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
      <SourceHeader title={copy.modelDirectory} titleID="model-directory-title" state={state} copy={copy} onRetry={retry} />
      <FilterForm source="models" query={query} copy={copy} onQueryChange={onQueryChange} fields={[
        { key: 'q', label: copy.modelSearch, placeholder: copy.modelSearchPlaceholder },
        { key: 'lifecycle_status', label: copy.lifecycle, options: ['PUBLISHED', 'DEPRECATED', 'REMOVED'] },
        { key: 'protocol_code', label: copy.protocol },
        { key: 'source_adapter', label: copy.sourceAdapter },
      ]} />
      <SourceFeedback state={state} copy={copy} onRetry={retry} />
      {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.model}</th><th>{copy.lifecycle}</th><th>{copy.protocol}</th><th>{copy.contextLength}</th><th>{copy.source}</th></tr></thead><tbody>
        {state.items.map((item) => <tr key={item.model_id}><td data-label={copy.model}><strong>{item.display_name}</strong><small className="adm-secondary-id">{item.model_id}</small></td><td data-label={copy.lifecycle}><Status value={item.lifecycle_status} copy={copy} /></td><td data-label={copy.protocol}>{item.protocol_code} · v{item.protocol_version}</td><td data-label={copy.contextLength}>{Number(item.context_length || 0).toLocaleString()}</td><td data-label={copy.source}><Status value={item.source_available ? 'ACTIVE' : 'UNAVAILABLE'} copy={copy} /></td></tr>)}
      </tbody></table></div>}
      <Pagination source="models" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} />
    </section>
  );
}

export function AdminModelTokenWorkspace({ section, copy, client, permissions, routeVersion, onRouteChange, onNavigate, routeNotice, tokenResourceFormsEnabled = false, modelOperationsDetailsEnabled = false }) {
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
    {section === 'models' && <>
      <div className="adm-tabs adm-model-tabs" role="tablist" aria-label={modelTokenCopy.modelViews} onKeyDown={moveOperationsTab}>
        {modelTabs.visible.map((tab) => <button type="button" role="tab" id={`model-tab-${tab}`} aria-controls={`model-panel-${tab}`} aria-selected={modelTabs.tab === tab} tabIndex={modelTabs.tab === tab ? 0 : -1} className={modelTabs.tab === tab ? 'active' : ''} key={tab} onClick={() => onRouteChange({ model_tab: tab }, 'push')}>{tab === 'directory' ? modelTokenCopy.modelDirectory : modelTokenCopy.protocolFormulaView}</button>)}
      </div>
      {modelTabs.tab === 'directory' && granted.has('platform.models.read') && <div role="tabpanel" id="model-panel-directory" aria-labelledby="model-tab-directory"><ModelDirectory client={client} copy={modelTokenCopy} routeVersion={routeVersion} onQueryChange={onQueryChange} cursorHistory={cursorHistory} /></div>}
      {modelTabs.tab === 'protocols' && granted.has('platform.pricing.read') && <div role="tabpanel" id="model-panel-protocols" aria-labelledby="model-tab-protocols"><AdminProtocolFormulaWorkspace client={client} copy={modelTokenCopy} canManage={granted.has('platform.pricing.manage')} routeVersion={routeVersion} onRouteChange={onRouteChange} cursorHistory={cursorHistory} /></div>}
    </>}
    {section === 'tokenResources' && <AdminTokenResourcesWorkspace client={client} copy={modelTokenCopy} permissions={permissions} formsEnabled={tokenResourceFormsEnabled} routeVersion={routeVersion} onQueryChange={onQueryChange} onRouteChange={onRouteChange} histories={cursorHistory} />}
    {section === 'modelOperations' && <>
      <div className="adm-tabs" role="tablist" aria-label={modelTokenCopy.operationsViews} onKeyDown={moveOperationsTab}>
        {operations.visible.map((tab) => <button type="button" role="tab" id={`model-operations-tab-${tab}`} aria-controls={`model-operations-panel-${tab}`} aria-selected={operations.tab === tab} tabIndex={operations.tab === tab ? 0 : -1} className={operations.tab === tab ? 'active' : ''} key={tab} onClick={() => onRouteChange({ operations_tab: tab }, 'push')}>{tab === 'provisionings' ? modelTokenCopy.provisionings : tab === 'usage' ? modelTokenCopy.usageRequests : modelTokenCopy.reconciliationQueue}</button>)}
      </div>
      <AdminModelOperationsWorkspace client={client} copy={modelTokenCopy} permissions={permissions} detailsEnabled={modelOperationsDetailsEnabled} routeVersion={routeVersion} operations={operations} requests={operationRequests} onRouteChange={onRouteChange} onQueryChange={onQueryChange} onNavigate={onNavigate} histories={cursorHistory} />
    </>}
  </>;
}
