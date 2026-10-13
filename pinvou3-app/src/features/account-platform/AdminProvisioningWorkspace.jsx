import React, { useMemo, useRef, useState } from 'react';
import { adminModelTokenQueryUpdates, readAdminModelTokenQuery } from './admin-navigation.js';
import { FilterForm, Pagination, SourceFeedback, SourceHeader, Status, useAdminSource } from './AdminModelTokenPrimitives.jsx';
import { AdminModelTokenPageOverview } from './AdminModelTokenPageOverview.jsx';
function DateValue({ value }) { return <>{value ? new Date(value).toLocaleString() : '—'}</>; }
function sourceRetry({ state, source, query, histories, onQueryChange, setRefreshVersion }) {
  if (state.error?.status === 410) {
    delete histories.current[source];
    onQueryChange(source, { ...query, cursor: '' }, 'replace');
  } else setRefreshVersion((value) => value + 1);
}

function ProvisioningList({ load, copy, routeVersion, onQueryChange, onOpen, histories, canOpen }) {
  const query = useMemo(() => readAdminModelTokenQuery('provisionings'), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const state = useAdminSource({ enabled: true, load, query, refreshVersion });
  const retry = () => sourceRetry({ state, source: 'provisionings', query, histories, onQueryChange, setRefreshVersion });
  return <section className="adm-card adm-source-card" aria-labelledby="provisionings-title">
    <SourceHeader title={copy.provisionings} titleID="provisionings-title" state={state} copy={copy} onRetry={retry} />
    <FilterForm source="provisionings" query={query} copy={copy} onQueryChange={onQueryChange} fields={[
      { key: 'q', label: copy.provisioningSearch, placeholder: copy.identifierPlaceholder },
      { key: 'status', label: copy.status, options: ['PENDING', 'READY', 'FAILED', 'REPAIR_REQUIRED'] },
      { key: 'model_id', label: copy.modelID },
      { key: 'token_account_id', label: copy.tokenAccountID },
      { key: 'scope_type', label: copy.scope, options: ['PERSONAL'] },
      { key: 'scope_id', label: copy.scopeID },
      { key: 'sla_overdue', label: copy.slaOverdue, options: ['true', 'false'] },
    ]} />
    <SourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.items.length > 0 && <div className="adm-table-wrap"><table className="adm-table"><thead><tr><th>{copy.provisioning}</th><th>{copy.modelID}</th><th>{copy.status}</th><th>{copy.stage}</th><th>{copy.generation}</th><th>{copy.updatedAt}</th>{canOpen && <th>{copy.actions}</th>}</tr></thead><tbody>{state.items.map((item) => <tr key={item.provisioning_id}><td data-label={copy.provisioning}><span className="adm-mono">{item.provisioning_id}</span></td><td data-label={copy.modelID}><span className="adm-mono">{item.model_id}</span></td><td data-label={copy.status}><Status value={item.status} copy={copy} /></td><td data-label={copy.stage}>{copy.enumLabels?.[item.stage] || item.stage}</td><td data-label={copy.generation}>{item.actual_generation || 0} / {item.desired_generation}</td><td data-label={copy.updatedAt}><DateValue value={item.updated_at} /></td>{canOpen && <td data-label={copy.actions}><button type="button" onClick={() => onOpen(item.provisioning_id)}>{copy.viewDetail}</button></td>}</tr>)}</tbody></table></div>}
    <Pagination source="provisionings" query={query} state={state} copy={copy} cursorHistory={histories} onQueryChange={onQueryChange} />
  </section>;
}

export function AdminProvisioningWorkspace({ client, copy, permissions, userID, authorizationVersion, routeVersion, onRouteChange }) {
  const histories = useRef({});
  const load = useMemo(() => (query, signal) => client.provisionings(query, signal), [client]);
  const onQueryChange = (source, query, mode = 'push') => onRouteChange(adminModelTokenQueryUpdates(source, query), mode);
  if (!permissions.includes('platform.tokens.read')) return null;
  const drilldown = target => { delete histories.current.provisionings; onQueryChange('provisionings', { ...target.query, cursor: '' }); };
  return <><AdminModelTokenPageOverview section="modelOperations" {...{ client, copy, permissions, userID, authorizationVersion, routeVersion, onRouteChange }} enabled onDrilldown={drilldown} /><ProvisioningList {...{ load, copy, routeVersion, onQueryChange, histories }} canOpen={false} /></>;
}
