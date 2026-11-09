import React, { useEffect, useId, useRef, useState } from 'react';
import { registrationRepairCopy } from '../../shared/i18n.js';
import { registrationQueueCopy } from '../../shared/admin-registration-queue-i18n.js';
import './admin-registration-queue.css';
import { localizedAccountPlatformError } from './error-localization.js';
import { registrationInspectionFilters, registrationOperationAge, registrationRepairActions } from './registration-repair.js';

const initialFilters = () => ({ operation_id: '', status: '', stage: '', responsibility: '', sla: '', older_than_minutes: '' });
const time = value => value && Number.isFinite(Date.parse(value)) ? new Date(value).toLocaleString() : '—';

// Convert only the new hours field; keep the existing minutes validator and GET contract.
function draftMinutes(raw, unit) {
  if (unit === 'minutes' || raw === '') return raw;
  if (raw.length > 32) return null;
  const parts = /^(\d+)(?:\.(\d+))?$/.exec(raw);
  if (!parts) return null;
  const fraction = parts[2] || '';
  const scale = 10n ** BigInt(fraction.length);
  const minutes = (BigInt(parts[1]) * scale + BigInt(fraction || '0')) * 60n;
  return minutes % scale === 0n ? String(minutes / scale) : null;
}

function RegistrationEvidence({ item, labels, queryCopy, id }) {
  const timestamp = value => typeof value === 'string' && Number.isFinite(Date.parse(value)) && !value.startsWith('0001-') ? <time dateTime={value}>{time(value)}</time> : queryCopy.notReported;
  // Only show a diagnostic token already supplied by this authorized collection.
  const reason = typeof item.reason === 'string' && /^[A-Z][A-Z0-9_]{1,95}$/.test(item.reason) ? <code>{item.reason}</code> : queryCopy.notReported;
  return <details className="adm-registration-row-evidence"><summary aria-label={queryCopy.details + ' · ' + id}>{queryCopy.details}</summary>
    <dl><div><dt>{labels.createdAt}</dt><dd>{timestamp(item.created_at)}</dd></div>
      <div><dt>{labels.updatedAt}</dt><dd>{timestamp(item.updated_at)}</dd></div>
      <div><dt>{labels.attemptAt}</dt><dd>{timestamp(item.identity_create_attempted_at)}</dd></div>
      <div><dt>{queryCopy.attempts}</dt><dd>{Number.isSafeInteger(item.attempts) && item.attempts >= 0 ? item.attempts : queryCopy.notReported}</dd></div>
      <div><dt>{queryCopy.lastError}</dt><dd>{reason}</dd></div>
    </dl>
  </details>;
}

function RegistrationQueueTime({ value, queryCopy }) {
  return typeof value === 'string' && Number.isFinite(Date.parse(value)) && !value.startsWith('0001-') ? <time dateTime={value}>{time(value)}</time> : queryCopy.notReported;
}

export function AdminRepairQueue({ client, copy, language, profile = false }) {
  const labels = registrationRepairCopy[language], queryCopy = registrationQueueCopy[language];
  const [items, setItems] = useState(null);
  const [receivedAt, setReceivedAt] = useState(null);
  const [filters, setFilters] = useState(initialFilters);
  const [unit, setUnit] = useState('minutes');
  const adopted = useRef({}), readController = useRef(null);
  const durationHelp = useId();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const generation = useRef(0);
  const mutation = useRef(false);
  const load = async (requestFilters = adopted.current) => {
    const controller = profile ? null : new AbortController();
    if (controller) { readController.current?.abort(); readController.current = controller; }
    const request = ++generation.current;
    setBusy(true); setError('');
    try {
      const result = await (profile ? client.profileChangeRepairs() : client.registrationRepairs(requestFilters, controller.signal));
      if (request !== generation.current) return;
      if (!Array.isArray(result?.items)) throw new Error('INVALID_REPAIR_COLLECTION');
      if (!profile) { adopted.current = requestFilters; setReceivedAt(Date.now()); }
      setItems(result.items);
    } catch (reason) {
      if (request === generation.current) {
        if (!profile && (reason?.status === 401 || reason?.status === 403)) setItems(null);
        setError(localizedAccountPlatformError(copy, reason));
      }
    } finally { if (request === generation.current) setBusy(false); }
  };
  useEffect(() => { adopted.current = {}; void load({}); return () => { generation.current++; readController.current?.abort(); }; }, [client, profile]); // eslint-disable-line react-hooks/exhaustive-deps
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
  const submitFilters = (event) => {
    event.preventDefault();
    if (busy) return;
    const minutes = draftMinutes(filters.older_than_minutes, unit);
    if (minutes === null || !registrationInspectionFilters({ older_than_minutes: minutes })) { setError({ filter: 'durationInvalid' }); return; }
    const criteria = registrationInspectionFilters({ ...filters, older_than_minutes: minutes });
    if (!criteria) { setError({ filter: 'operationInvalid' }); return; }
    void load(criteria);
  };
  const applied = adopted.current;
  const appliedLabels = [
    applied.operation_id, labels.statuses[applied.status], labels.stages[applied.stage], labels.responsibilities[applied.responsibility],
    applied.sla === 'WITHIN' ? labels.within : applied.sla === 'OVERDUE' ? labels.overdue : '',
    applied.older_than_minutes ? labels.minutes(applied.older_than_minutes) : '',
  ].filter(Boolean);
  const change = (key, value) => setFilters(current => ({ ...current, [key]: value }));
  const select = (key, title, options) => <label>{title}<select name={key} value={filters[key]} disabled={busy} onChange={event => change(key, event.target.value)}><option value="">{labels.any}</option>{options.map(([value, text]) => <option key={value} value={value}>{text}</option>)}</select></label>;
  return <section className="adm-panel adm-repair-queue"><h2>{profile ? copy.userManagement.profileRepair : copy.userManagement.registrationRepair}</h2><p>{profile ? copy.userManagement.profileRepairLead : copy.userManagement.registrationRepairLead}</p>
    {!profile && <form className="adm-repair-filters adm-registration-filters" onSubmit={submitFilters}>
      <label>{labels.operationID}<input name="operation_id" value={filters.operation_id} disabled={busy} onChange={event => change('operation_id', event.target.value)} /></label>
      {select('status', labels.status, Object.entries(labels.statuses))}
      {select('stage', labels.stage, Object.entries(labels.stages))}
      {select('responsibility', labels.responsibility, Object.entries(labels.responsibilities))}
      {select('sla', labels.sla, [['WITHIN', labels.within], ['OVERDUE', labels.overdue]])}
      <div className="adm-registration-duration">
        <label>{queryCopy.duration}<input name="older_than_minutes" type="number" min="0" max={unit === 'hours' ? '720' : '43200'} step={unit === 'hours' ? 'any' : '1'} value={filters.older_than_minutes} aria-describedby={durationHelp} aria-invalid={error?.filter === 'durationInvalid' || undefined} disabled={busy} onChange={event => { change('older_than_minutes', event.target.value); if (error?.filter === 'durationInvalid') setError(''); }} /></label>
        <label>{queryCopy.unit}<select value={unit} disabled={busy} onChange={event => { setUnit(event.target.value); change('older_than_minutes', ''); if (error?.filter === 'durationInvalid') setError(''); }}><option value="minutes">{queryCopy.minutes}</option><option value="hours">{queryCopy.hours}</option></select></label>
        <small id={durationHelp}>{queryCopy.durationHelp}</small>
      </div>
      <button type="submit" className="adm-primary-button" disabled={busy}>{queryCopy.search}</button>
    </form>}
    <button type="button" disabled={busy} onClick={() => void load()}>{copy.refresh}</button>
    {!profile && items && <p className="adm-registration-filter-scope">{queryCopy.applied}: {appliedLabels.length ? appliedLabels.join(' · ') : queryCopy.unfiltered}</p>}
    {error && <div role="alert">{error.filter ? queryCopy[error.filter] : error}</div>}{busy && <p role="status">{copy.loading}</p>}
    {/* biome-ignore lint/a11y/noNoninteractiveTabindex: The read-only wide table needs keyboard access to native horizontal scrolling. */}
    {items && <div className={profile ? "adm-table-wrap" : "adm-table-wrap adm-registration-table"} tabIndex="0" role="region" aria-label={profile ? copy.userManagement.profileRepair : copy.userManagement.registrationRepair}><table><thead><tr><th>{copy.userManagement.operation}</th><th>{copy.accounts.status}</th>{!profile && <><th>{labels.responsibility}</th><th>{queryCopy.sinceUpdate}<small>{queryCopy.sinceUpdateHelp}</small></th><th>{labels.nextRetryAt}</th><th>{labels.sla}</th></>}<th>{copy.accounts.actions}</th></tr></thead><tbody>{items.map(item => {
      const id = profile ? item.profile_change_request_id : item.registration_operation_id;
      const allowed = profile ? { retry: true, compensate: false } : registrationRepairActions(item);
      const age = !profile && receivedAt !== null && typeof item.updated_at === 'string' && !item.updated_at.startsWith('0001-')
        ? registrationOperationAge(item.updated_at, receivedAt) : null;
      return <tr key={id} data-operation-id={id} data-operation-status={item.status}><td><code>{id}</code><small>{profile ? item.stage : labels.stages[item.stage] || item.stage}</small></td><td>{profile ? item.status : labels.statuses[item.status] || item.status}{profile && item.reason && <small>{item.reason}</small>}</td>
        {!profile && <><td>{labels.responsibilities[item.responsibility] || item.responsibility}</td><td>{age === null ? queryCopy.notReported : labels.minutes(age)}</td><td><RegistrationQueueTime value={item.next_retry_at} queryCopy={queryCopy} /></td><td><RegistrationQueueTime value={item.sla_expires_at} queryCopy={queryCopy} /></td></>}
        <td>{allowed.retry && <button disabled={busy} onClick={() => void act(item)}>{allowed.query ? labels.stages.IDENTITY_CREATING : copy.userManagement.retry}</button>}{allowed.compensate && <button disabled={busy} onClick={() => void act(item, true)}>{copy.userManagement.compensate}</button>}{!profile && (allowed.query || (!allowed.retry && !allowed.compensate)) && <small>{item.status === 'IDENTITY_CREATING' ? labels.identityUnknown : labels.readOnly}</small>}{!profile && <RegistrationEvidence {...{ item, labels, queryCopy, id }} />}</td></tr>;
    })}</tbody></table></div>}
    {items?.length === 0 && <p>{copy.noData}</p>}
  </section>;
}
