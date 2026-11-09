import React, { useEffect, useId, useMemo, useRef, useState } from 'react';
import './admin-provisioning-table.css';
import { adminModelTokenQueryUpdates, readAdminModelTokenQuery } from './admin-navigation.js';
import { Pagination, SourceFeedback, SourceHeader, Status, useAdminSource } from './AdminModelTokenPrimitives.jsx';
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

function resourceChoices(items, field) {
  const values = new Map();
  for (const item of items) {
    const value = item[field];
    if (typeof value !== 'string' || !/^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$/i.test(value)) continue;
    const scope = field === 'scope_id' ? item.scope_type : '';
    if (field === 'scope_id' && !['PERSONAL','ORGANIZATION'].includes(scope)) continue;
    const key = scope ? scope + ':' + value : value;
    values.set(key, { key, value, scope });
  }
  return [...values.values()];
}

function ProvisioningFilters({ query, items, copy, onQueryChange }) {
  const [draft, setDraft] = useState(query);
  useEffect(() => setDraft(query), [JSON.stringify(query)]);
  const words = copy.provisioningPresentation;
  const fields = [
    { key: 'q', label: copy.provisioningSearch, placeholder: copy.identifierPlaceholder },
    { key: 'status', label: copy.status, options: ['PENDING', 'READY', 'FAILED', 'REPAIR_REQUIRED'] },
    { key: 'model_id', label: copy.modelID, choices: resourceChoices(items, 'model_id') },
    { key: 'token_account_id', label: copy.tokenAccountID, choices: resourceChoices(items, 'active_token_account_id') },
    { key: 'scope_type', label: copy.scope, options: ['PERSONAL', 'ORGANIZATION'] },
    { key: 'scope_id', label: copy.scopeID, choices: resourceChoices(items, 'scope_id') },
    { key: 'sla_overdue', label: copy.slaOverdue, options: ['true', 'false'] },
  ];
  const update = (key, value) => setDraft(current => ({ ...current, [key]: value }));
  return <form className="adm-model-token-filters adm-provisioning-filters" onSubmit={event => {
    event.preventDefault(); onQueryChange('provisionings', { ...draft, cursor: '' });
  }}>
    {fields.map(field => field.options ? <label key={field.key}><span>{field.label}</span>
      <select value={draft[field.key] || ''} onChange={event => update(field.key, event.target.value)}>
        <option value="">{copy.all}</option>
        {field.options.map(option => <option key={option} value={option}>{copy.enumLabels?.[option] || option}</option>)}
      </select>
    </label> : <div key={field.key} className="adm-provisioning-filter-field">
      <label><span>{field.label}</span><input type="search" placeholder={field.placeholder || ''} value={draft[field.key] || ''} onChange={event => update(field.key, event.target.value)} /></label>
      {field.choices?.length > 0 && <label><span>{words.fromPage + ' · ' + field.label}</span><select value="" onChange={event => {
        const selected = field.choices.find(candidate => candidate.key === event.target.value);
        if (selected) setDraft(current => ({ ...current, [field.key]: selected.value, ...(selected.scope && { scope_type: selected.scope }) }));
      }}><option value="">{words.choose}</option>
        {field.choices.map(choice => <option key={choice.key} value={choice.key}>{choice.scope ? (copy.enumLabels?.[choice.scope] || choice.scope) + ' · ' : ''}{choice.value}</option>)}
      </select></label>}
    </div>)}
    <button type="submit">{copy.search}</button>
    <button type="button" className="secondary" onClick={() => onQueryChange('provisionings', Object.fromEntries(Object.keys(query).map(key => [key, ''])))}>{copy.clear}</button>
  </form>;
}

function ConfigGeneration({ value, copy }) {
  const text = typeof value === 'string' ? value : Number.isSafeInteger(value) ? String(value) : '';
  const known = /^(0|[1-9][0-9]{0,18})$/.test(text) && BigInt(text) <= 9223372036854775807n;
  return known ? <span>{text}</span> : <span>{copy.provisioningTaskIdentity.unavailable}</span>;
}

function ProvisioningRows({ items, copy }) {
  const [open, setOpen] = useState('');
  const detailID = useId();
  const words = copy.provisioningTaskIdentity;
  return <div className="adm-table-wrap adm-provisioning-task-table"><table className="adm-table">
    <thead><tr><th>{words.object}</th><th>{copy.status}</th><th>{copy.stage}</th><th>{copy.provisioningPresentation.versions}</th><th>{copy.updatedAt}</th><th>{copy.actions}</th></tr></thead>
    <tbody>{items.map(item => {
      const expanded = open === item.provisioning_id;
      const details = detailID + '-' + item.provisioning_id;
      return <tr key={item.provisioning_id}>
        <td data-label={words.object}><ProvisioningObjects item={item} copy={copy} /></td>
        <td data-label={copy.status}><Status value={item.status} copy={copy} /></td>
        <td data-label={copy.stage}>{copy.enumLabels?.[item.stage] || item.stage}</td>
        <td data-label={copy.provisioningPresentation.versions}><dl className="adm-provisioning-versions"><div><dt>{copy.provisioningPresentation.effective}</dt><dd><ConfigGeneration value={item.actual_generation} copy={copy} /></dd></div><div><dt>{copy.provisioningPresentation.target}</dt><dd><ConfigGeneration value={item.desired_generation} copy={copy} /></dd></div></dl></td>
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
    <SourceHeader title={copy.provisioningPresentation.listTitle} titleID="provisionings-title" state={state} copy={copy} onRetry={retry} />
    <ProvisioningFilters query={query} items={!state.loading && !state.error ? state.items : []} copy={copy} onQueryChange={onQueryChange} />
    <SourceFeedback state={state} copy={copy} onRetry={retry} />
    {state.loading && !state.error && state.items.length > 0 && <div className="adm-source-state" role="status">{copy.loading}</div>}
    {!state.loading && !state.error && state.items.length > 0 && <ProvisioningRows key={routeVersion} items={state.items} copy={copy} />}
    <Pagination source="provisionings" query={query} state={state} copy={copy} cursorHistory={histories} onQueryChange={onQueryChange} />
  </section>;
}

export function AdminProvisioningWorkspace({ client, copy, permissions, userID, authorizationVersion, routeVersion, onRouteChange }) {
  const histories = useRef({});
  const load = useMemo(() => (query, signal) => client.provisioningsForPresentation(query, signal), [client]);
  const onQueryChange = (source, query, mode = 'push') => onRouteChange(adminModelTokenQueryUpdates(source, query), mode);
  if (!permissions.includes('platform.tokens.read')) return null;
  const drilldown = target => { delete histories.current.provisionings; onQueryChange('provisionings', { ...target.query, cursor: '' }); };
  return <><AdminModelTokenPageOverview section="modelOperations" {...{ client, copy, permissions, userID, authorizationVersion, routeVersion, onRouteChange }} enabled onDrilldown={drilldown} /><ProvisioningList key={userID + ':' + authorizationVersion} {...{ load, copy, routeVersion, onQueryChange, histories }} /></>;
}
