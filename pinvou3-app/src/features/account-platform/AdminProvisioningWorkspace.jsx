import React, { useId, useMemo, useRef, useState } from 'react';
import './admin-provisioning-table.css';
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

function Reference({ value, unavailable }) {
  return typeof value === 'string' && value ? <code className="adm-mono">{value}</code> : <span>{unavailable}</span>;
}

function ProvisioningObjects({ item, copy }) {
  const words = copy.provisioningTaskIdentity;
  return <dl className="adm-provisioning-object-facts">
    <div><dt>{words.model}</dt><dd><Reference value={item.model_id} unavailable={words.unavailable} /></dd></div>
    <div><dt>{words.account}</dt><dd><Reference value={item.active_token_account_id} unavailable={words.noAccount} /></dd></div>
    <div><dt>{copy.scope}</dt><dd>{copy.enumLabels?.[item.scope_type] || words.unavailable} <Reference value={item.scope_id} unavailable={words.unavailable} /></dd></div>
  </dl>;
}

function ProvisioningRows({ items, copy }) {
  const [open, setOpen] = useState('');
  const detailID = useId();
  const words = copy.provisioningTaskIdentity;
  return <div className="adm-table-wrap adm-provisioning-task-table"><table className="adm-table">
    <thead><tr><th>{words.object}</th><th>{copy.status}</th><th>{copy.stage}</th><th>{copy.generation}</th><th>{copy.updatedAt}</th><th>{copy.actions}</th></tr></thead>
    <tbody>{items.map(item => {
      const expanded = open === item.provisioning_id;
      const details = detailID + '-' + item.provisioning_id;
      return <tr key={item.provisioning_id}>
        <td data-label={words.object}><ProvisioningObjects item={item} copy={copy} /></td>
        <td data-label={copy.status}><Status value={item.status} copy={copy} /></td>
        <td data-label={copy.stage}>{copy.enumLabels?.[item.stage] || item.stage}</td>
        <td data-label={copy.generation}>{item.actual_generation || 0} / {item.desired_generation}</td>
        <td data-label={copy.updatedAt}><DateValue value={item.updated_at} /></td>
        <td data-label={copy.actions}><button type="button" aria-expanded={expanded} aria-controls={details}
          aria-label={words.view + ' · ' + item.provisioning_id} onClick={() => setOpen(expanded ? '' : item.provisioning_id)}>{words.view}</button>
          <div id={details} hidden={!expanded} className="adm-provisioning-row-detail">
            {expanded && <><p>{words.snapshot}</p><strong>{words.taskID}</strong> <Reference value={item.provisioning_id} unavailable={words.unavailable} /></>}
          </div>
        </td>
      </tr>;
    })}</tbody>
  </table></div>;
}

function ProvisioningList({ load, copy, routeVersion, onQueryChange, histories }) {
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
    {state.loading && !state.error && state.items.length > 0 && <div className="adm-source-state" role="status">{copy.loading}</div>}
    {!state.loading && !state.error && state.items.length > 0 && <ProvisioningRows key={routeVersion} items={state.items} copy={copy} />}
    <Pagination source="provisionings" query={query} state={state} copy={copy} cursorHistory={histories} onQueryChange={onQueryChange} />
  </section>;
}

export function AdminProvisioningWorkspace({ client, copy, permissions, userID, authorizationVersion, routeVersion, onRouteChange }) {
  const histories = useRef({});
  const load = useMemo(() => (query, signal) => client.provisionings(query, signal), [client]);
  const onQueryChange = (source, query, mode = 'push') => onRouteChange(adminModelTokenQueryUpdates(source, query), mode);
  if (!permissions.includes('platform.tokens.read')) return null;
  const drilldown = target => { delete histories.current.provisionings; onQueryChange('provisionings', { ...target.query, cursor: '' }); };
  return <><AdminModelTokenPageOverview section="modelOperations" {...{ client, copy, permissions, userID, authorizationVersion, routeVersion, onRouteChange }} enabled onDrilldown={drilldown} /><ProvisioningList key={userID + ':' + authorizationVersion} {...{ load, copy, routeVersion, onQueryChange, histories }} /></>;
}
