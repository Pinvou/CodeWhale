import React, { useEffect, useRef, useState } from 'react';
import { registrationRepairCopy } from '../../shared/i18n.js';
import { localizedAccountPlatformError } from './error-localization.js';
import { registrationInspectionFilters, registrationOperationAge, registrationRepairActions } from './registration-repair.js';

const initialFilters = () => ({ operation_id: '', status: '', stage: '', responsibility: '', sla: '', older_than_minutes: '' });
const time = value => value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString() : '—';

export function AdminRepairQueue({ client, copy, language, profile = false }) {
  const labels = registrationRepairCopy[language];
  const [items, setItems] = useState(null);
  const [filters, setFilters] = useState(initialFilters);
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const generation = useRef(0);
  const mutation = useRef(false);
  const load = async (values = filters) => {
    const requestFilters = profile ? {} : registrationInspectionFilters(values);
    if (!requestFilters) { setError(labels.invalidFilters); return; }
    const request = ++generation.current;
    setBusy(true); setError('');
    try {
      const result = await (profile ? client.profileChangeRepairs() : client.registrationRepairs(requestFilters));
      if (request !== generation.current) return;
      if (!Array.isArray(result?.items)) throw new Error('INVALID_REPAIR_COLLECTION');
      setItems(result.items);
    } catch (reason) { if (request === generation.current) setError(localizedAccountPlatformError(copy, reason)); }
    finally { if (request === generation.current) setBusy(false); }
  };
  useEffect(() => { void load(initialFilters()); return () => { generation.current++; }; }, [client, profile]); // eslint-disable-line react-hooks/exhaustive-deps
  const act = async (item, compensate = false) => {
    const allowed = profile ? { retry: true, compensate: false } : registrationRepairActions(item);
    if (mutation.current || busy || !(compensate ? allowed.compensate : allowed.retry)) return;
    if (!globalThis.confirm(compensate ? copy.userManagement.confirmCompensation : copy.userManagement.confirmRetry)) return;
    mutation.current = true;
    const request = ++generation.current;
    setBusy(true); setError('');
    try {
      if (profile) await client.retryProfileChange(item.profile_change_request_id);
      else if (compensate) await client.compensateRegistration(item.registration_operation_id);
      else await client.retryRegistration(item.registration_operation_id);
      if (request === generation.current) await load();
    } catch (reason) { if (request === generation.current) setError(localizedAccountPlatformError(copy, reason)); }
    finally { mutation.current = false; if (request === generation.current) setBusy(false); }
  };
  const change = (key, value) => setFilters(current => ({ ...current, [key]: value }));
  const select = (key, title, options) => <label>{title}<select name={key} value={filters[key]} disabled={busy} onChange={event => change(key, event.target.value)}><option value="">{labels.any}</option>{options.map(([value, text]) => <option key={value} value={value}>{text}</option>)}</select></label>;
  return <section className="adm-panel adm-repair-queue"><h2>{profile ? copy.userManagement.profileRepair : copy.userManagement.registrationRepair}</h2><p>{profile ? copy.userManagement.profileRepairLead : copy.userManagement.registrationRepairLead}</p>
    {!profile && <form className="adm-repair-filters" onSubmit={event => { event.preventDefault(); void load(); }}>
      <label>{labels.operationID}<input name="operation_id" value={filters.operation_id} disabled={busy} onChange={event => change('operation_id', event.target.value)} /></label>
      {select('status', labels.status, Object.entries(labels.statuses))}
      {select('stage', labels.stage, Object.entries(labels.stages))}
      {select('responsibility', labels.responsibility, Object.entries(labels.responsibilities))}
      {select('sla', labels.sla, [['WITHIN', labels.within], ['OVERDUE', labels.overdue]])}
      <label>{labels.olderThan}<input name="older_than_minutes" type="number" min="0" max="43200" step="1" value={filters.older_than_minutes} disabled={busy} onChange={event => change('older_than_minutes', event.target.value)} /></label>
      <button disabled={busy}>{copy.refresh}</button>
    </form>}
    {profile && <button disabled={busy} onClick={() => void load()}>{copy.refresh}</button>}
    {error && <div role="alert">{error}</div>}{busy && <p role="status">{copy.loading}</p>}
    {/* biome-ignore lint/a11y/noNoninteractiveTabindex: The read-only wide table needs keyboard access to native horizontal scrolling. */}
    {items && <div className="adm-table-wrap" tabIndex="0" role="region" aria-label={profile ? copy.userManagement.profileRepair : copy.userManagement.registrationRepair}><table><thead><tr><th>{copy.userManagement.operation}</th><th>{copy.accounts.status}</th>{!profile && <><th>{labels.responsibility}</th><th>{labels.age}</th><th>{labels.createdAt}</th><th>{labels.updatedAt}</th><th>{labels.attemptAt}</th><th>{labels.nextRetryAt}</th><th>{labels.sla}</th></>}<th>{copy.accounts.actions}</th></tr></thead><tbody>{items.map(item => {
      const id = profile ? item.profile_change_request_id : item.registration_operation_id;
      const allowed = profile ? { retry: true, compensate: false } : registrationRepairActions(item);
      const age = registrationOperationAge(item.created_at);
      return <tr key={id} data-operation-id={id} data-operation-status={item.status}><td><code>{id}</code><small>{profile ? item.stage : labels.stages[item.stage] || item.stage}</small></td><td>{profile ? item.status : labels.statuses[item.status] || item.status}{item.reason && <small>{item.reason}</small>}</td>
        {!profile && <><td>{labels.responsibilities[item.responsibility] || item.responsibility}</td><td>{age === null ? '—' : labels.minutes(age)}</td><td><time dateTime={item.created_at}>{time(item.created_at)}</time></td><td><time dateTime={item.updated_at}>{time(item.updated_at)}</time></td><td><time dateTime={item.identity_create_attempted_at}>{time(item.identity_create_attempted_at)}</time></td><td><time dateTime={item.next_retry_at}>{time(item.next_retry_at)}</time></td><td><time dateTime={item.sla_expires_at}>{time(item.sla_expires_at)}</time></td></>}
        <td>{allowed.retry && <button disabled={busy} onClick={() => void act(item)}>{copy.userManagement.retry}</button>}{allowed.compensate && <button disabled={busy} onClick={() => void act(item, true)}>{copy.userManagement.compensate}</button>}{!profile && !allowed.retry && !allowed.compensate && <small>{item.status === 'IDENTITY_CREATING' ? labels.identityUnknown : labels.readOnly}</small>}</td></tr>;
    })}</tbody></table></div>}
    {items?.length === 0 && <p>{copy.noData}</p>}
  </section>;
}
