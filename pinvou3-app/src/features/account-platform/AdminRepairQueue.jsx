import React, { useEffect, useId, useRef, useState } from 'react';
import { registrationRepairCopy } from '../../shared/i18n.js';
import { registrationQueueCopy } from '../../shared/admin-registration-queue-i18n.js';
import './admin-registration-queue.css';
import { profileRepairQueueCopy } from '../../shared/admin-profile-repair-i18n.js';
import './admin-profile-repair.css';
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

const profileUUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const validProfileID = value => typeof value === 'string' && profileUUID.test(value) && value !== '00000000-0000-0000-0000-000000000000';
const knownProfileWord = (table, key) => typeof key === 'string' && Object.prototype.hasOwnProperty.call(table, key) ? table[key] : '';
function profileRetryIsBlocked(item) {
  return item?.field === 'PASSWORD' && item.status === 'REPAIR_REQUIRED' &&
    (item.reason === 'LEGACY_SESSION_REVOCATION_UNCERTAIN' || item.reason === 'PASSWORD_SESSION_ORDER_UNPROVEN');
}
function profileRetryReceiptIsKnown(result) {
  return result?.resource_type === 'profile_change' && result.action === 'RETRY_SCHEDULED' ||
    result?.resource_type === 'password_reset' && result.action === 'ATOMIC_PASSWORD_RECOVERY_SCHEDULED';
}
function ProfileRepairRow({ item, copy, adminCopy, busy, act }) {
  const requestID = validProfileID(item?.profile_change_request_id) ? item.profile_change_request_id : '';
  const userID = validProfileID(item?.user_id) ? item.user_id : '';
  const kind = knownProfileWord(copy.kinds,item?.field) || (item?.field == null || item.field === '' ? copy.notReported : copy.unknownKind);
  const knownStatus = knownProfileWord(copy.statuses,item?.status);
  const status = knownStatus || (item?.status == null || item.status === '' ? copy.notReported : copy.unknownStatus);
  const count = Number.isSafeInteger(item?.attempts) && item.attempts >= 0 ? item.attempts : copy.notReported;
  const lastError = knownProfileWord(copy.errors,item?.reason) || (item?.reason == null || item.reason === '' ? copy.notReported : copy.unrecognizedError);
  return <tr data-operation-id={requestID || undefined} data-operation-status={knownStatus ? item.status : undefined}>
    <td>{requestID ? <code>{requestID}</code> : copy.notReported}</td>
    <td>{status}</td><td>{kind}</td>
    <td>{userID ? <code>{userID}</code> : copy.notReported}</td>
    <td>{count}</td><td>{lastError}</td>
    <td>{requestID && <>
      {profileRetryIsBlocked(item) ? <p className="adm-profile-repair-blocked">{copy.blocked}</p> : <button type="button" disabled={busy} onClick={() => void act(item)}>{adminCopy.userManagement.retry}</button>}
      <details className="adm-profile-repair-detail"><summary aria-label={copy.details + ' · ' + requestID}>{copy.details}</summary>
        <dl><div><dt>{copy.requestID}</dt><dd><code>{requestID}</code></dd></div>
          <div><dt>{copy.kind}</dt><dd>{kind}</dd></div><div><dt>{adminCopy.accounts.status}</dt><dd>{status}</dd></div>
          <div><dt>{copy.lastError}</dt><dd>{lastError}</dd></div>
        </dl>
      </details>
    </>}</td>
  </tr>;
}

export function AdminRepairQueue({ client, copy, language, profile = false }) {
  const labels = registrationRepairCopy[language], queryCopy = registrationQueueCopy[language], profileCopy = profileRepairQueueCopy[language];
  const [items, setItems] = useState(null);
  const [receivedAt, setReceivedAt] = useState(null);
  const [readState, setReadState] = useState("loading");
  const [readOwner, setReadOwner] = useState(null);
  const [profileReceipt, setProfileReceipt] = useState(null);
  const [profileSearch, setProfileSearch] = useState("");
  const [profileState, setProfileState] = useState("");
  const [filters, setFilters] = useState(initialFilters);
  const [unit, setUnit] = useState('minutes');
  const adopted = useRef({}), readController = useRef(null);
  const durationHelp = useId();
  const [error, setError] = useState('');
  const [busy, setBusy] = useState(false);
  const generation = useRef(0);
  const mutation = useRef(false);
  const load = async (requestFilters = adopted.current) => {
    const controller = new AbortController();
    readController.current?.abort(); readController.current = controller; setReadState("loading");
    const request = ++generation.current;
    setBusy(true); setError('');
    try {
      const result = await (profile ? client.profileChangeRepairs(controller.signal) : client.registrationRepairs(requestFilters, controller.signal));
      if (request !== generation.current) return;
      if (!Array.isArray(result?.items)) throw new Error('INVALID_REPAIR_COLLECTION');
      if (!profile) adopted.current = requestFilters;
      setReceivedAt(Date.now()); setReadOwner(client); setReadState("ready");
      setItems(result.items);
    } catch (reason) {
      if (request === generation.current) {
        if (reason?.status === 401 || reason?.status === 403) {
          setItems(null); setReceivedAt(null); setReadState('denied');
          if (profile) setProfileReceipt(null);
        } else {
          setReadState('error');
        }
        setError(localizedAccountPlatformError(copy, reason));
      }
    } finally { if (request === generation.current) setBusy(false); }
  };
  useEffect(() => { adopted.current = {}; setProfileReceipt(null); setProfileSearch(""); setProfileState(""); void load({}); return () => { generation.current++; readController.current?.abort(); }; }, [client, profile]); // eslint-disable-line react-hooks/exhaustive-deps
  const act = async (item, compensate = false) => {
    const allowed = profile ? { retry: true, compensate: false } : registrationRepairActions(item);
    if (mutation.current || busy || !(compensate ? allowed.compensate : allowed.retry)) return;
    if (profile && (readState !== "ready" || readOwner !== client || !validProfileID(item?.profile_change_request_id) || profileRetryIsBlocked(item))) return;
    const confirmation = profile ? profileCopy.confirmation + '\n' + item.profile_change_request_id
      : compensate ? copy.userManagement.confirmCompensation : copy.userManagement.confirmRetry;
    if (!globalThis.confirm(confirmation)) return;
    mutation.current = true;
    const request = ++generation.current;
    setBusy(true); setError('');
    const profileID = profile ? item.profile_change_request_id : '';
    if (profile) setProfileReceipt({ client, id:profileID, state:'scheduling' });
    try {
      if (profile) {
        const result = await client.retryProfileChange(profileID);
        if (request === generation.current) setProfileReceipt({ client, id:profileID, state:profileRetryReceiptIsKnown(result) ? 'scheduled' : 'uncertain' });
      }
      else if (compensate) await client.compensateRegistration(item.registration_operation_id);
      else await client.retryRegistration(item.registration_operation_id);
      if (request === generation.current) await load();
    } catch (reason) {
      if (request === generation.current) {
        if (profile) setProfileReceipt({ client, id:profileID, state:!reason?.status || reason.status >= 500 || reason.status === 202 ? 'uncertain' : 'rejected' });
        setError(localizedAccountPlatformError(copy, reason));
      }
    }
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
  const currentItems = readOwner === client ? items : null;
  const profileNeedle = profileSearch.trim().toLowerCase();
  const profileFiltered = Boolean(profileNeedle || profileState);
  const clearProfile = () => { setProfileSearch(''); setProfileState(''); };
  const profileItems = profile ? currentItems?.filter(item =>
    (!profileNeedle || [item?.profile_change_request_id,item?.user_id].some(id => validProfileID(id) && id.toLowerCase().includes(profileNeedle))) &&
    (!profileState || item?.status === profileState)) : null;
  const displayedItems = profile ? profileItems : currentItems;
  const filtered = Object.keys(applied).length > 0;
  const clearFilters = () => { if (busy) return; setFilters(initialFilters()); setUnit("minutes"); void load({}); };
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
    {profile && <div className="adm-profile-repair-filters" role="search" aria-label={profileCopy.filterTitle}>
      <label>{profileCopy.search}<input value={profileSearch} onChange={e=>setProfileSearch(e.target.value)} disabled={!currentItems || readState==='denied'} /></label>
      <label>{copy.accounts.status}<select value={profileState} onChange={e=>setProfileState(e.target.value)} disabled={!currentItems || readState==='denied'}>
        <option value="">{profileCopy.allStatuses}</option>
        {Object.entries(profileCopy.statuses).map(([status,word])=><option key={status} value={status}>{word}</option>)}
      </select></label>
      {profileFiltered && <button type="button" onClick={clearProfile}>{profileCopy.clear}</button>}
    </div>}
    {profile && <p className="adm-profile-repair-scope">{profileCopy.scope}</p>}
    <button type="button" disabled={busy} onClick={() => void load()}>{copy.refresh}</button>
    {profile && currentItems && readState !== 'denied' && <p className="adm-profile-repair-scope">{profileCopy.loaded} {currentItems.length} · {profileCopy.matching} {profileItems.length}</p>}
    {profile && currentItems && (readState === 'error' || readState === 'loading') && <p className="adm-profile-repair-scope">{profileCopy.previous}</p>}
    {!profile && currentItems && <div className="adm-registration-result-scope">
      <p className="adm-registration-filter-scope">{queryCopy.applied}: {appliedLabels.length ? appliedLabels.join(' · ') : queryCopy.unfiltered}</p>
      {receivedAt !== null && <p className="adm-registration-filter-scope">{queryCopy.receivedAt} <time dateTime={new Date(receivedAt).toISOString()}>{time(new Date(receivedAt).toISOString())}</time></p>}
      {(readState === 'error' || readState === 'loading') && <p className="adm-registration-previous">{currentItems.length ? queryCopy.previousRows : queryCopy.previousEmpty}</p>}
    </div>}
    {readState === 'denied' ? <p role="alert">{profile ? profileCopy.denied : queryCopy.denied}</p> : error && <div role="alert">{error.filter ? queryCopy[error.filter] : error}</div>}
    {profile && profileReceipt?.client === client && <p className="adm-profile-repair-receipt" role="status">{profileCopy[profileReceipt.state]} <code>{profileReceipt.id}</code></p>}
    {busy && <p role="status">{readState === 'loading' ? (profile ? profileCopy.reading : queryCopy.reading) : copy.loading}</p>}
    {/* biome-ignore lint/a11y/noNoninteractiveTabindex: The read-only wide table needs keyboard access to native horizontal scrolling. */}
    {displayedItems?.length > 0 && <div className={profile ? "adm-table-wrap adm-profile-repair-table" : "adm-table-wrap adm-registration-table"} tabIndex="0" role="region" aria-label={profile ? copy.userManagement.profileRepair : copy.userManagement.registrationRepair}><table><thead><tr><th>{copy.userManagement.operation}</th><th>{copy.accounts.status}</th>{profile && <><th>{profileCopy.kind}</th><th>{profileCopy.userID}</th><th>{profileCopy.attempts}</th><th>{profileCopy.lastError}</th></>}{!profile && <><th>{labels.responsibility}</th><th>{queryCopy.sinceUpdate}<small>{queryCopy.sinceUpdateHelp}</small></th><th>{labels.nextRetryAt}</th><th>{labels.sla}</th></>}<th>{copy.accounts.actions}</th></tr></thead><tbody>{displayedItems.map((item,index) => {
      if(profile)return <ProfileRepairRow key={validProfileID(item?.profile_change_request_id) ? item.profile_change_request_id : 'profile-missing-'+index} item={item} busy={busy || readState !== "ready"} act={act} copy={profileCopy} adminCopy={copy} />;
      const id = item.registration_operation_id;
      const allowed = registrationRepairActions(item);
      const age = !profile && receivedAt !== null && typeof item.updated_at === 'string' && !item.updated_at.startsWith('0001-')
        ? registrationOperationAge(item.updated_at, receivedAt) : null;
      return <tr key={id} data-operation-id={id} data-operation-status={item.status}><td><code>{id}</code><small>{labels.stages[item.stage] || item.stage}</small></td><td>{labels.statuses[item.status] || item.status}</td>
        {!profile && <><td>{labels.responsibilities[item.responsibility] || item.responsibility}</td><td>{age === null ? queryCopy.notReported : labels.minutes(age)}</td><td><RegistrationQueueTime value={item.next_retry_at} queryCopy={queryCopy} /></td><td><RegistrationQueueTime value={item.sla_expires_at} queryCopy={queryCopy} /></td></>}
        <td>{allowed.retry && <button disabled={busy} onClick={() => void act(item)}>{allowed.query ? labels.stages.IDENTITY_CREATING : copy.userManagement.retry}</button>}{allowed.compensate && <button disabled={busy} onClick={() => void act(item, true)}>{copy.userManagement.compensate}</button>}{!profile && (allowed.query || (!allowed.retry && !allowed.compensate)) && <small>{item.status === 'IDENTITY_CREATING' ? labels.identityUnknown : labels.readOnly}</small>}{!profile && <RegistrationEvidence {...{ item, labels, queryCopy, id }} />}</td></tr>;
    })}</tbody></table></div>}
    {profile && profileItems?.length === 0 && readState === 'ready' && <div className="adm-profile-repair-empty">
      <p role="status">{profileFiltered && currentItems.length ? profileCopy.filteredEmpty : profileCopy.empty}</p>
      {profileFiltered && currentItems.length > 0 && <button type="button" onClick={clearProfile}>{profileCopy.clear}</button>}
    </div>}
    {!profile && currentItems?.length === 0 && readState === 'ready' && <div className="adm-registration-empty">
      <p role="status">{filtered ? queryCopy.filteredEmpty : queryCopy.empty}</p>
      {filtered && <button type="button" disabled={busy} onClick={clearFilters}>{queryCopy.clear}</button>}
    </div>}
  </section>;
}
