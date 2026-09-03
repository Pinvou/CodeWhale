import React, { useEffect, useMemo, useRef, useState } from 'react';
import { adminModelTokenQueryUpdates, previousAdminModelTokenCursor, readAdminModelTokenModelTab, readAdminModelTokenOperationsTab, readAdminModelTokenQuery, rememberAdminModelTokenNextCursor, synchronizeAdminModelTokenCursorHistory } from './admin-navigation.js';
import { executeAdminSourceRequest, initialAdminSourceState, planAdminModelTokenOperationRequests, reduceAdminSourceState, visibleAdminSourceState } from './admin-model-token-state.js';
import { AdminProtocolFormulaWorkspace } from './AdminProtocolFormulaWorkspace.jsx';

function useAdminSource({ enabled, load, query, refreshVersion }) {
  const [state, setState] = useState(initialAdminSourceState);
  const generation = useRef(0);
  const queryKey = JSON.stringify(query);
  useEffect(() => {
    if (!enabled) return undefined;
    const controller = new globalThis.AbortController();
    const requestID = ++generation.current;
    void executeAdminSourceRequest({
      requestID,
      queryKey,
      query,
      load,
      signal: controller.signal,
      transition: (event) => setState((previous) => reduceAdminSourceState(previous, event)),
    });
    return () => controller.abort();
  }, [enabled, load, queryKey, refreshVersion]);
  return visibleAdminSourceState(state, queryKey, enabled);
}

function sourceCursorHistory(cursorHistory, source, query) {
  const history = synchronizeAdminModelTokenCursorHistory(cursorHistory.current[source], query);
  cursorHistory.current[source] = history;
  return history;
}

function Status({ value, copy }) {
  const label = copy.enumLabels?.[value] || value || '—';
  const normalized = String(value || '').toUpperCase();
  const tone = ['ACTIVE', 'READY', 'PUBLISHED', 'HEALTHY', 'SETTLED', 'SUCCEEDED'].includes(normalized)
    ? 'ok'
    : ['PENDING', 'REPAIR_REQUIRED', 'DEGRADED', 'SETTLEMENT_PENDING'].includes(normalized) ? 'warn' : '';
  return <span className={`adm-status ${tone}`}>{label}</span>;
}

function SourceHeader({ title, titleID, state, copy, onRetry }) {
  return (
    <div className="adm-source-head">
      <div>
        <h2 id={titleID}>{title}</h2>
        <p>{state.updatedAt ? `${copy.lastUpdated} ${new Date(state.updatedAt).toLocaleString()}` : copy.notUpdated}</p>
      </div>
      <button type="button" disabled={state.loading} onClick={onRetry}>{state.loading ? copy.loading : copy.refreshSource}</button>
    </div>
  );
}

function SourceFeedback({ state, copy, onRetry }) {
  if (state.error) {
    const expired = state.error?.status === 410;
    return (
      <div className="adm-source-error" role="alert">
        <strong>{expired ? copy.cursorExpired : copy.sourceUnavailable}</strong>
        <span>{state.error.requestID ? `${copy.requestID}: ${state.error.requestID}` : copy.retryHelp}</span>
        <button type="button" onClick={onRetry}>{copy.retry}</button>
      </div>
    );
  }
  if (state.loading && state.items.length === 0) return <div className="adm-source-state" role="status">{copy.loading}</div>;
  if (!state.loading && state.items.length === 0) return <div className="adm-source-state">{copy.noResults}</div>;
  return null;
}

function Pagination({ source, query, state, copy, cursorHistory, onQueryChange }) {
  if (!state.page || state.error) return null;
  const history = sourceCursorHistory(cursorHistory, source, query);
  const previous = previousAdminModelTokenCursor(history);
  const goNext = () => {
    cursorHistory.current[source] = rememberAdminModelTokenNextCursor(history, state.page.nextCursor);
    onQueryChange(source, { ...query, cursor: state.page.nextCursor });
  };
  const goPrevious = () => {
    cursorHistory.current[source] = { ...history, currentCursor: previous.cursor };
    onQueryChange(source, { ...query, cursor: previous.cursor });
  };
  return (
    <div className="adm-pagination adm-model-token-pagination">
      <span>{copy.pageCount(state.items.length, state.page.hasMore)}</span>
      {state.page.asOf && <small>{copy.snapshotAt} {new Date(state.page.asOf).toLocaleString()}</small>}
      <div className="adm-row-actions">
        <button type="button" disabled={state.loading || !previous.available} onClick={goPrevious}>{copy.previous}</button>
        <button type="button" disabled={state.loading || !state.page.nextCursor} onClick={goNext}>{copy.next}</button>
      </div>
    </div>
  );
}

function FilterForm({ source, query, fields, copy, onQueryChange }) {
  const [draft, setDraft] = useState(query);
  useEffect(() => setDraft(query), [JSON.stringify(query)]);
  return (
    <form className="adm-model-token-filters" onSubmit={(event) => {
      event.preventDefault();
      onQueryChange(source, { ...draft, cursor: '' });
    }}>
      {fields.map((field) => (
        <label key={field.key}>
          <span>{field.label}</span>
          {field.options ? (
            <select value={draft[field.key] || ''} onChange={(event) => setDraft((current) => ({ ...current, [field.key]: event.target.value }))}>
              <option value="">{copy.all}</option>
              {field.options.map((option) => <option key={option} value={option}>{copy.enumLabels?.[option] || option}</option>)}
            </select>
          ) : (
            <input type={field.type || 'search'} value={draft[field.key] || ''} placeholder={field.placeholder || ''} onChange={(event) => setDraft((current) => ({ ...current, [field.key]: event.target.value }))} />
          )}
        </label>
      ))}
      <button type="submit">{copy.search}</button>
      <button type="button" className="secondary" onClick={() => onQueryChange(source, Object.fromEntries(Object.keys(query).map((key) => [key, ''])))}>{copy.clear}</button>
    </form>
  );
}

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

function TokenAccounts({ client, copy, routeVersion, onQueryChange, cursorHistory }) {
  const query = useMemo(() => readAdminModelTokenQuery('tokenAccounts'), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const load = useMemo(() => (filters, signal) => client.tokenAccounts(filters, signal), [client]);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion });
  const retry = () => {
    if (state.error?.status === 410) {
      delete cursorHistory.current.tokenAccounts;
      onQueryChange('tokenAccounts', { ...query, cursor: '' }, 'replace');
    } else setRefreshVersion((value) => value + 1);
  };
  return (
    <section className="adm-card adm-source-card" aria-labelledby="token-accounts-title">
      <SourceHeader title={copy.tokenAccounts} titleID="token-accounts-title" state={state} copy={copy} onRetry={retry} />
      <FilterForm source="tokenAccounts" query={query} copy={copy} onQueryChange={onQueryChange} fields={[
        { key: 'q', label: copy.tokenSearch, placeholder: copy.tokenSearchPlaceholder },
        { key: 'owner_type', label: copy.ownerType, options: ['USER', 'ORG', 'ORG_UNIT'] },
        { key: 'status', label: copy.status, options: ['ACTIVE', 'DISABLED', 'FROZEN'] },
        { key: 'health_status', label: copy.health, options: ['UNKNOWN', 'PROBING', 'HEALTHY', 'DEGRADED', 'UNHEALTHY', 'STALE'] },
      ]} />
      <SourceFeedback state={state} copy={copy} onRetry={retry} />
      {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.tokenAccount}</th><th>{copy.owner}</th><th>{copy.status}</th><th>{copy.credentials}</th><th>{copy.routes}</th><th>{copy.updatedAt}</th></tr></thead><tbody>
        {state.items.map((item) => <tr key={item.token_account_id}><td data-label={copy.tokenAccount}><strong>{item.name}</strong><small className="adm-secondary-id">{item.token_account_id}</small></td><td data-label={copy.owner}>{copy.enumLabels?.[item.owner_type] || item.owner_type}<small className="adm-secondary-id">{item.owner_id}</small></td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.credentials}>{Number(item.active_credential_count || 0).toLocaleString()}</td><td data-label={copy.routes}>{Number(item.route_count || 0).toLocaleString()}</td><td data-label={copy.updatedAt}>{new Date(item.updated_at).toLocaleString()}</td></tr>)}
      </tbody></table></div>}
      <Pagination source="tokenAccounts" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} />
    </section>
  );
}

function Provisionings({ load, copy, routeVersion, onQueryChange, cursorHistory }) {
  const query = useMemo(() => readAdminModelTokenQuery('provisionings'), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion });
  const retry = () => {
    if (state.error?.status === 410) {
      delete cursorHistory.current.provisionings;
      onQueryChange('provisionings', { ...query, cursor: '' }, 'replace');
    } else setRefreshVersion((value) => value + 1);
  };
  return <section className="adm-card adm-source-card" aria-labelledby="provisionings-title"><SourceHeader title={copy.provisionings} titleID="provisionings-title" state={state} copy={copy} onRetry={retry} />
    <FilterForm source="provisionings" query={query} copy={copy} onQueryChange={onQueryChange} fields={[
      { key: 'q', label: copy.provisioningSearch, placeholder: copy.identifierPlaceholder },
      { key: 'status', label: copy.status, options: ['PENDING', 'READY', 'FAILED', 'REPAIR_REQUIRED'] },
      { key: 'model_id', label: copy.modelID },
      { key: 'token_account_id', label: copy.tokenAccountID },
    ]} />
    <SourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.provisioning}</th><th>{copy.modelID}</th><th>{copy.status}</th><th>{copy.stage}</th><th>{copy.generation}</th><th>{copy.updatedAt}</th></tr></thead><tbody>{state.items.map((item) => <tr key={item.provisioning_id}><td data-label={copy.provisioning}><span className="adm-mono">{item.provisioning_id}</span></td><td data-label={copy.modelID}><span className="adm-mono">{item.model_id}</span></td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.stage}>{copy.enumLabels?.[item.stage] || item.stage}</td><td data-label={copy.generation}>{item.actual_generation || 0} / {item.desired_generation}</td><td data-label={copy.updatedAt}>{new Date(item.updated_at).toLocaleString()}</td></tr>)}</tbody></table></div>}
    <Pagination source="provisionings" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} />
  </section>;
}

function UsageRequests({ load, copy, routeVersion, onQueryChange, cursorHistory }) {
  const query = useMemo(() => readAdminModelTokenQuery('usage'), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion });
  const retry = () => {
    if (state.error?.status === 410) {
      delete cursorHistory.current.usage;
      onQueryChange('usage', { ...query, cursor: '' }, 'replace');
    } else setRefreshVersion((value) => value + 1);
  };
  return <section className="adm-card adm-source-card" aria-labelledby="usage-requests-title"><SourceHeader title={copy.usageRequests} titleID="usage-requests-title" state={state} copy={copy} onRetry={retry} />
    <FilterForm source="usage" query={query} copy={copy} onQueryChange={onQueryChange} fields={[
      { key: 'q', label: copy.usageSearch, placeholder: copy.identifierPlaceholder },
      { key: 'execution_status', label: copy.executionStatus, options: ['RECEIVED', 'HOLD_RESERVING', 'HOLD_RESERVED', 'DISPATCH_INTENT_PERSISTED', 'UPSTREAM_DISPATCHING', 'STREAMING', 'SETTLEMENT_PENDING', 'SUCCEEDED', 'FAILED', 'CANCELLED_BEFORE_DISPATCH', 'REVIEW'] },
      { key: 'settlement_status', label: copy.settlementStatus, options: ['HOLD_PENDING', 'HOLD_ACTIVE', 'USAGE_PENDING', 'SETTLED', 'RELEASED', 'REVIEW'] },
      { key: 'model', label: copy.model },
    ]} />
    <SourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.request}</th><th>{copy.model}</th><th>{copy.executionStatus}</th><th>{copy.settlementStatus}</th><th>{copy.startedAt}</th></tr></thead><tbody>{state.items.map((item) => <tr key={item.usage_request_id}><td data-label={copy.request}><span className="adm-mono">{item.usage_request_id}</span></td><td data-label={copy.model}>{item.model}</td><td data-label={copy.executionStatus}><Status value={item.execution_status} copy={copy} /></td><td data-label={copy.settlementStatus}><Status value={item.settlement_status} copy={copy} /></td><td data-label={copy.startedAt}>{new Date(item.started_at).toLocaleString()}</td></tr>)}</tbody></table></div>}
    <Pagination source="usage" query={query} state={state} copy={copy} cursorHistory={cursorHistory} onQueryChange={onQueryChange} />
  </section>;
}

export function AdminModelTokenWorkspace({ section, copy, client, permissions, routeVersion, onRouteChange, routeNotice }) {
  const modelTokenCopy = copy.modelToken;
  const granted = new Set(permissions || []);
  const cursorHistory = useRef({});
  const operations = useMemo(() => readAdminModelTokenOperationsTab(permissions), [permissions, routeVersion]);
  const modelTabs = useMemo(() => readAdminModelTokenModelTab(permissions), [permissions, routeVersion]);
  const operationRequests = useMemo(() => planAdminModelTokenOperationRequests(permissions, client), [permissions, client]);
  const provisioningRequest = operationRequests.find((request) => request.source === 'provisionings');
  const usageRequest = operationRequests.find((request) => request.source === 'usage');
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
    {section === 'tokenResources' && <TokenAccounts client={client} copy={modelTokenCopy} routeVersion={routeVersion} onQueryChange={onQueryChange} cursorHistory={cursorHistory} />}
    {section === 'modelOperations' && <>
      <div className="adm-tabs" role="tablist" aria-label={modelTokenCopy.operationsViews} onKeyDown={moveOperationsTab}>
        {operations.visible.map((tab) => <button type="button" role="tab" id={`model-operations-tab-${tab}`} aria-controls={`model-operations-panel-${tab}`} aria-selected={operations.tab === tab} tabIndex={operations.tab === tab ? 0 : -1} className={operations.tab === tab ? 'active' : ''} key={tab} onClick={() => onRouteChange({ operations_tab: tab }, 'push')}>{tab === 'provisionings' ? modelTokenCopy.provisionings : modelTokenCopy.usageRequests}</button>)}
      </div>
      {provisioningRequest && <div role="tabpanel" hidden={operations.tab !== 'provisionings'} id="model-operations-panel-provisionings" aria-labelledby="model-operations-tab-provisionings"><Provisionings load={provisioningRequest.load} copy={modelTokenCopy} routeVersion={routeVersion} onQueryChange={onQueryChange} cursorHistory={cursorHistory} /></div>}
      {usageRequest && <div role="tabpanel" hidden={operations.tab !== 'usage'} id="model-operations-panel-usage" aria-labelledby="model-operations-tab-usage"><UsageRequests load={usageRequest.load} copy={modelTokenCopy} routeVersion={routeVersion} onQueryChange={onQueryChange} cursorHistory={cursorHistory} /></div>}
    </>}
  </>;
}
