import React, {useCallback,useEffect,useId,useRef,useState} from 'react';
import { auditActionCopy, auditActionLabels, auditPrivacyCopy, auditExportPresentation } from '../../shared/i18n.js';
import './admin-audit-presentation.css';
import {secureRequestID} from './request-id.js';
import {acquireAuditExportTicket,newAuditExportAttempt,submitAuditExportAttempt} from './admin-audit-export.js';
import { AUDIT_EXPORT_STEPUP_QUERY, auditExportStepupNonce, newAuditExportRecovery, readAuditExportRecovery, clearAuditExportRecovery, requireAuditExportActor, reauthenticateAuditExport } from './audit-export-recovery.js';
function formatDate(value) {
  return value ? new Date(value).toLocaleString() : '—';
}

function shortID(value) {
  const text = String(value || '');
  return text.length > 16 ? `${text.slice(0, 8)}…${text.slice(-5)}` : text || '—';
}

function readAuditQueryFilters(locationLike = globalThis.location) {
  const search = new URLSearchParams(String(locationLike?.search || ''));
  return {
    actor: '',
    action: search.get('audit_action') || '',
    resource: search.get('audit_resource') || '',
    result: '',
    from: '',
    to: '',
  };
}

function AuditActionCode({ value, words }) {
  const [result, setResult] = useState('');
  if (typeof value !== 'string' || !value) return '—';
  const copyCode = async () => {
    setResult('');
    try { await navigator.clipboard.writeText(value); setResult('copied'); }
    catch { setResult('copyFailed'); }
  };
  return <div className="adm-audit-action-code"><code>{value}</code><button type="button" onClick={copyCode}>{words.copy}</button>
    {result && <span role={result === 'copied' ? 'status' : 'alert'}>{words[result]}</span>}
  </div>;
}

function auditActionLabel(language, value) {
  const labels = auditActionLabels[language] || auditActionLabels.zh;
  return Object.prototype.hasOwnProperty.call(labels, value) ? labels[value] : null;
}

function TechnicalID({ value, copy, compact = true }) {
  const [copied, setCopied] = useState(false);
  const text = String(value || '');
  if (!text) return '—';
  const copyValue = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1600);
    } catch {
      setCopied(false);
    }
  };
  return (
    <span className="adm-technical-id">
      <code title={text}>{compact ? shortID(text) : text}</code>
      <button type="button" onClick={copyValue} aria-label={`${copy.copyIdentifier}: ${text}`}>
        {copied ? copy.copiedIdentifier : copy.copyIdentifier}
      </button>
    </span>
  );
}

function Empty({ copy }) {
  return <div className="adm-empty" role="status">{copy.noData}</div>;
}

function Table({ columns, items, copy }) {
  if (!items?.length) return <Empty copy={copy} />;
  return (
    <div className="adm-table-wrap">
      <table className="adm-table">
        <thead>
          <tr>
            {columns.map((column) => (
              <th key={column.key}>{column.label}</th>
            ))}
          </tr>
        </thead>
        <tbody>
          {items.map((item, index) => (
            <tr key={item.audit_id || index}>
              {columns.map((column) => (
                <td key={column.key} className={column.className}>{column.render ? column.render(item) : (item[column.key] ?? '—')}</td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function SectionHeader({ title, count, copy, children }) {
  return (
    <header className="adm-section-head">
      <div>
        <h2>{title}</h2>
        {count !== undefined && <span>{copy.items(count)}</span>}
      </div>
      {children}
    </header>
  );
}

function DetailDrawer({ label, onClose, children, describedBy, role = 'dialog' }) {
  const drawerRef = useRef(null);
  useEffect(() => {
    const previousFocus = document.activeElement;
    drawerRef.current?.focus();
    const onKeyDown = (event) => {
      if (event.key === 'Escape') onClose();
    };
    globalThis.addEventListener?.('keydown', onKeyDown);
    return () => {
      globalThis.removeEventListener?.('keydown', onKeyDown);
      previousFocus?.focus?.();
    };
  }, [onClose]);
  const trapFocus = (event) => {
    if (event.key !== 'Tab') return;
    const focusable = [...(drawerRef.current?.querySelectorAll('button:not(:disabled), a[href], input:not(:disabled), select:not(:disabled), textarea:not(:disabled), [tabindex]:not([tabindex="-1"])') || [])];
    if (!focusable.length) {
      event.preventDefault();
      drawerRef.current?.focus();
      return;
    }
    const first = focusable[0];
    const last = focusable.at(-1);
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };
  return (
    <div className="adm-drawer-backdrop" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <aside ref={drawerRef} className="adm-detail-drawer" role={role} aria-modal="true" aria-label={label} aria-describedby={describedBy} tabIndex="-1" onKeyDown={trapFocus}>
        {children}
      </aside>
    </div>
  );
}

function ExportLimits({ words }) {
  return <dl className="adm-audit-export-limits">{words.limits.map(([label,value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>;
}

function AuditExportConfirmation({ snapshot, words, copy, onClose, onConfirm, disabled }) {
  const descriptionID = useId(), { attempt, timeZone } = snapshot;
  const labels = { actor: copy.audit.actor, action: copy.audit.action, resource: copy.audit.resource, result: copy.audit.result, from: copy.audit.from, to: copy.audit.to };
  return <DetailDrawer label={words.confirmTitle} role="alertdialog" describedBy={descriptionID} onClose={onClose}>
    <section className="adm-panel adm-audit-export-confirm">
      <h3>{words.confirmTitle}</h3><p id={descriptionID}>{words.confirmLead}</p>
      <dl className="adm-detail-list">
        {Object.entries(labels).map(([key,label]) => <div key={key}><dt>{label}</dt><dd>{!attempt.filter[key] ? words.all : key === 'from' || key === 'to' ? <time dateTime={attempt.filter[key]}>{attempt.filter[key]}</time> : <code>{attempt.filter[key]}</code>}</dd></div>)}
        <div><dt>{words.timeZone}</dt><dd>{timeZone || words.zoneUnavailable}</dd></div>
        <div><dt>{words.privacy}</dt><dd>{attempt.includePII ? words.unredacted : words.redacted}</dd></div>
        <div><dt>{words.reason}</dt><dd>{attempt.reason}</dd></div>
      </dl>
      <ExportLimits words={words} />
      <footer><button type="button" onClick={onClose}>{words.cancel}</button><button type="button" className="adm-audit-confirm-submit" disabled={disabled} onClick={onConfirm}>{words.confirmSubmit}</button></footer>
    </section>
  </DetailDrawer>;
}

function restoredFilters(filter) {
  const local = value => {
    if (!value) return '';
    const date = new Date(value);
    const pad = number => String(number).padStart(2, '0');
    return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
  };
  return { ...readAuditQueryFilters(), ...filter, from: local(filter.from), to: local(filter.to) };
}
function Audit({ copy, data, client, setData, permissions, currentUserID, language }) {
  const [recovery] = useState(() => {
    const nonce = auditExportStepupNonce();
    try {
      const record = readAuditExportRecovery(currentUserID, nonce);
      if (record && (!(permissions || []).includes('platform.audit.export') || !record.exportID && (record.attempt || record.draft)?.includePII && !(permissions || []).includes('platform.audit.pii.export'))) throw new Error('recovery permission changed');
      return { nonce, record, blocked: false };
    } catch { return { nonce, record: null, blocked: true }; }
  });
  const recoveredInput = recovery.record?.attempt || recovery.record?.draft;
  const [recoveryBlocked, setRecoveryBlocked] = useState(recovery.blocked);
  const [recoveryPending, setRecoveryPending] = useState(Boolean(recovery.record));
  const [needsAuthentication, setNeedsAuthentication] = useState(false);
  const recoveryNonce = useRef(recovery.nonce);
  const knownExportID = useRef(recovery.record?.exportID || '');
  const [filters, setFilters] = useState(() => recoveredInput ? restoredFilters(recoveredInput.filter) : readAuditQueryFilters());
  const [detail, setDetail] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [includePII, setIncludePII] = useState(recoveredInput?.includePII || false);
  const [exportJob, setExportJob] = useState(null);
  const [exportConfirmation, setExportConfirmation] = useState(null), exportConfirmationRef = useRef(null);
  const closeExportConfirmation = useCallback(() => { exportConfirmationRef.current = null; setExportConfirmation(null); }, []);
  const confirmationEpoch = client.financeSessionEpoch || 0;
  useEffect(() => { closeExportConfirmation(); }, [client, currentUserID, confirmationEpoch, closeExportConfirmation]);
  const [exportKey, setExportKey] = useState('');
  const [exportReason, setExportReason] = useState(recoveredInput?.reason || '');
  const [exportAttempted, setExportAttempted] = useState(Boolean(recovery.record?.attempt));
  const [ticketIssueUnknown, setTicketIssueUnknown] = useState(recovery.record?.ticketIssueUnknown || false);
  const liveRef = useRef(true);
  useEffect(() => { liveRef.current = true; return () => { liveRef.current = false; }; }, []);
  const exportAttemptRef = useRef(recovery.record?.attempt || null);
  const exportOutcomeUnknownRef = useRef(recovery.record?.outcomeUnknown || false);
  const exportTicketRef = useRef(null);
  const canRead = (permissions || []).includes('platform.audit.read');
  const canExport = (permissions || []).includes('platform.audit.export');
  const canExportPII = (permissions || []).includes('platform.audit.pii.export');
  const privacyID = useId(), privacyWords = auditPrivacyCopy[language];
  const exportWords = auditExportPresentation[language];
  const items = data.audits || [];
  const observeRecoveryError = cause => {
    if (cause?.status === 401 || cause?.code === 'RECENT_AUTHENTICATION_REQUIRED') setNeedsAuthentication(true);
    if (cause?.code === 'AUDIT_EXPORT_RECOVERY_UNAVAILABLE') setRecoveryBlocked(true);
  };
  const finishRecovery = () => {
    try {
      clearAuditExportRecovery(currentUserID, recoveryNonce.current);
      recoveryNonce.current = '';
      setRecoveryPending(false);
      return true;
    } catch { setError(copy.audit.recoveryCleanupFailed); return false; }
  };

  const queryFilters = () => ({
    ...filters,
    from: filters.from ? new Date(filters.from).toISOString() : '',
    to: filters.to ? new Date(filters.to).toISOString() : '',
  });
  const search = async (event) => {
    event?.preventDefault();
    if (!canRead) return;
    setBusy(true);
    setError('');
    try {
      const result = await client.audits(queryFilters());
      if (!liveRef.current) return;
      if (!Array.isArray(result?.items)) throw new Error('INVALID_AUDIT_LOGS_ACK');
      setData((current) => ({ ...current, audits: result.items }));
    } catch {
      if (liveRef.current) setError(copy.audit.actionFailed);
    } finally {
      if (liveRef.current) setBusy(false);
    }
  };
  const showDetail = async (item) => {
    if (!canRead) return;
    setBusy(true);
    setError('');
    try {
      const itemDetail = await client.audit(item.audit_id);
      if (!liveRef.current) return;
      if (itemDetail?.audit_id !== item.audit_id) throw new Error('INVALID_AUDIT_DETAIL_ACK');
      setDetail(itemDetail);
    } catch {
      if (liveRef.current) setError(copy.audit.actionFailed);
    } finally {
      if (liveRef.current) setBusy(false);
    }
  };
  const prepareExport = () => {
    if (!canExport || busy || recoveryBlocked || exportJob || exportConfirmationRef.current) return;
    if (exportAttemptRef.current) { void createExport(); return; }
    if (includePII && !canExportPII) { setError(copy.permissionDenied); return; }
    let filter;
    try { filter = queryFilters(); } catch { setError(copy.audit.exportRequestInvalid); return; }
    if (!filter.from || !filter.to) { setError(copy.audit.exportRangeRequired); return; }
    let attempt;
    try { attempt = newAuditExportAttempt(filter, exportReason, includePII, `audit-export-${secureRequestID()}`); }
    catch { setError(copy.audit.exportRequestInvalid); return; }
    let timeZone;
    try { timeZone = Intl.DateTimeFormat().resolvedOptions().timeZone; } catch { /* UTC wire values remain visible. */ }
    const snapshot = { client, actor: currentUserID, epoch: confirmationEpoch, attempt, timeZone };
    exportConfirmationRef.current = snapshot; setExportConfirmation(snapshot); setError('');
  };
  const confirmExport = snapshot => {
    if (exportConfirmationRef.current !== snapshot || snapshot.client !== client || snapshot.actor !== currentUserID || snapshot.epoch !== (client.financeSessionEpoch || 0) ||
        !canExport || snapshot.attempt.includePII && !canExportPII || busy || recoveryBlocked || exportJob || exportAttemptRef.current) return;
    closeExportConfirmation();
    exportAttemptRef.current = snapshot.attempt; setExportAttempted(true); void createExport();
  };
  const createExport = async () => {
    if (recoveryBlocked || !exportAttemptRef.current) return;
    setBusy(true);
    setError('');
    try {
      await requireAuditExportActor(client, currentUserID, exportAttemptRef.current.includePII);
      if (!liveRef.current) return;
      const job = await submitAuditExportAttempt(client, exportAttemptRef.current);
      if (knownExportID.current && job.export_id !== knownExportID.current) throw new Error('INVALID_AUDIT_EXPORT_ACK');
      if (!liveRef.current) return;
      exportOutcomeUnknownRef.current = false;
      knownExportID.current = job.export_id;
      setExportJob(job);
      setExportKey(job.decryption_key || '');
      setNeedsAuthentication(false);
      finishRecovery();
    } catch (cause) {
      if (!liveRef.current) return;
      observeRecoveryError(cause);
      if (cause?.code === 'AUDIT_EXPORT_RECOVERY_UNAVAILABLE') { setError(copy.audit.recoveryUnavailable); return; }
      // A first definite rejection permits correction. After any unknown
      // outcome, a later authorization/path rejection does not prove that the
      // first call created no job; retain the original facts and key until an
      // authoritative create receipt is recovered.
      const status = typeof cause?.status === 'number' ? cause.status : 0;
      if (status >= 400 && status < 500 && status !== 408 && status !== 409 && !exportOutcomeUnknownRef.current) {
        exportAttemptRef.current = null;
        setExportAttempted(false);
      } else {
        exportOutcomeUnknownRef.current = true;
      }
      if (liveRef.current) setError(copy.audit.exportFailed);
    } finally {
      if (liveRef.current) setBusy(false);
    }
  };
  const refreshExport = async () => {
    const id = exportJob?.export_id || knownExportID.current;
    if (!id || recoveryBlocked) return;
    setBusy(true);
    setError('');
    try {
      await requireAuditExportActor(client, currentUserID);
      if (!liveRef.current) return;
      const job = await client.auditExport(id);
      if (!liveRef.current) return;
      setExportJob(job);
      if (!job.download_ticket_issued_at) setTicketIssueUnknown(false);
    } catch (cause) {
      if (liveRef.current) { observeRecoveryError(cause); setError(cause?.code === 'AUDIT_EXPORT_RECOVERY_UNAVAILABLE' ? copy.audit.recoveryUnavailable : copy.audit.exportFailed); }
    } finally {
      if (liveRef.current) setBusy(false);
    }
  };
  useEffect(() => {
    if (recovery.record?.exportID && !recoveryBlocked) void refreshExport();
    // Returned records can trigger an authorized GET, never a mutation.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, currentUserID]);
  const reauthenticate = async () => {
    setBusy(true); setError('');
    try {
      if (recoveryBlocked && recoveryNonce.current && !recovery.record) {
        client.reauthenticate(`/admin.html?section=audit&${AUDIT_EXPORT_STEPUP_QUERY}=${encodeURIComponent(recoveryNonce.current)}`);
        return;
      }
      const draft = exportAttemptRef.current ? null : newAuditExportAttempt(queryFilters(), exportReason, includePII, 'audit-draft-validation');
      const record = newAuditExportRecovery({ actor: currentUserID, nonce: secureRequestID(),
        attempt: exportAttemptRef.current, draft: draft ? { filter: draft.filter, reason: draft.reason, includePII: draft.includePII } : null,
        outcomeUnknown: exportOutcomeUnknownRef.current, exportID: exportJob?.export_id || knownExportID.current,
        ticketIssueUnknown });
      await reauthenticateAuditExport(client, record);
    } catch (cause) {
      if (liveRef.current) { observeRecoveryError(cause); setError(cause?.code === 'AUDIT_EXPORT_RECOVERY_UNAVAILABLE' ? copy.audit.recoveryUnavailable : copy.audit.recoveryStorageFailed); }
    } finally { if (liveRef.current) setBusy(false); }
  };
  const newExport = () => {
    if (!finishRecovery()) return;
    exportAttemptRef.current = null; exportOutcomeUnknownRef.current = false; exportTicketRef.current = null; knownExportID.current = '';
    setTicketIssueUnknown(false); setExportAttempted(false); setExportJob(null); setExportKey(''); setExportReason(''); setIncludePII(false); setNeedsAuthentication(false);
  };
  const downloadExport = async () => {
    if (recoveryBlocked || !exportJob || (exportJob.download_ticket_issued_at && !exportTicketRef.current) || ticketIssueUnknown) return;
    setBusy(true);
    setError('');
    try {
      await requireAuditExportActor(client, currentUserID, Boolean(exportJob.include_pii));
      if (!liveRef.current) return;
      if (!exportTicketRef.current) {
        try {
          exportTicketRef.current = await acquireAuditExportTicket(client, exportJob.export_id, null);
        } catch (error) {
          // Explicit authorization rejection has not issued a ticket. Network,
          // timeout, conflict and server failures still require an authoritative GET.
          if (error?.status !== 401 && error?.status !== 403) setTicketIssueUnknown(true);
          throw error;
        }
      }
      if (!liveRef.current) return;
      // Permission may expire while the one-time ticket is being issued.
      await requireAuditExportActor(client, currentUserID, Boolean(exportJob.include_pii));
      if (!liveRef.current) return;
      const blob = await client.downloadAuditExport(exportJob.export_id, exportTicketRef.current.download_token);
      if (!liveRef.current) return;
      const href = URL.createObjectURL(blob);
      const link = document.createElement('a');
      link.href = href;
      link.download = exportJob.filename || `pinvou-audit-${exportJob.export_id}.pae`;
      document.body.append(link);
      try {
        link.click();
      } finally {
        link.remove();
        window.setTimeout(() => URL.revokeObjectURL(href), 60_000);
      }
      setExportJob((current) => ({ ...current, status: 'DOWNLOADED' }));
      finishRecovery();
    } catch (cause) {
      if (liveRef.current) { observeRecoveryError(cause); setError(cause?.code === 'AUDIT_EXPORT_RECOVERY_UNAVAILABLE' ? copy.audit.recoveryUnavailable : cause?.status === 403 ? copy.permissionDenied : copy.audit.exportFailed); }
    } finally {
      if (liveRef.current) setBusy(false);
    }
  };
  if (detail) return <AuditDetail copy={copy} item={detail} onBack={() => setDetail(null)} language={language} />;
  const ticketExpiry = exportJob?.download_expires_at || exportTicketRef.current?.expires_at;
  const columns = [
    {
      key: 'action',
      label: copy.audit.action,
      className: 'adm-audit-action-cell',
      render: (item) => (
        <button type="button" className="adm-name-link adm-audit-action" disabled={busy} onClick={() => showDetail(item)}>
          {auditActionLabel(language, item.action) || <code>{item.action || '—'}</code>}
        </button>
      ),
    },
    {
      key: 'resource_type',
      label: copy.audit.resource,
      render: (item) => (
        <span>
          {item.resource_type}
          <br />
          <TechnicalID value={item.resource_id} copy={copy} />
        </span>
      ),
    },
    {
      key: 'actor_user_id',
      label: copy.audit.actor,
      render: (item) => (
        <span>
          {item.actor_display_name || item.actor_email || '—'}
          <br />
          <TechnicalID value={item.actor_user_id} copy={copy} />
        </span>
      ),
    },
    {
      key: 'result',
      label: copy.audit.result,
      render: (item) => <Status value={item.result} />,
    },
    {
      key: 'request_id',
      label: copy.audit.request,
      render: (item) => <TechnicalID value={item.request_id} copy={copy} />,
    },
    {
      key: 'occurred_at',
      label: copy.audit.occurred,
      render: (item) => formatDate(item.occurred_at),
    },
  ];
  return (
    <>
    <section className="adm-panel">
      <SectionHeader title={copy.audit.title} count={items.length} copy={copy} />
      <form className="adm-ops-form" onSubmit={search}><fieldset disabled={busy || recoveryBlocked || exportAttempted || Boolean(exportJob)}>
        <input aria-label={copy.audit.actorFilter} placeholder={copy.audit.actorFilter} value={filters.actor} onChange={(event) => setFilters((current) => ({ ...current, actor: event.target.value }))} />
        <input
          aria-label={copy.audit.actionFilter}
          placeholder={copy.audit.actionFilter}
          value={filters.action}
          onChange={(event) =>
            setFilters((current) => ({
              ...current,
              action: event.target.value,
            }))
          }
        />
        <input
          aria-label={copy.audit.resourceFilter}
          placeholder={copy.audit.resourceFilter}
          value={filters.resource}
          onChange={(event) =>
            setFilters((current) => ({
              ...current,
              resource: event.target.value,
            }))
          }
        />
        <select
          aria-label={copy.audit.result}
          value={filters.result}
          onChange={(event) =>
            setFilters((current) => ({
              ...current,
              result: event.target.value,
            }))
          }
        >
          <option value="">{copy.audit.allResults}</option>
          <option>SUCCEEDED</option>
          <option>DENIED</option>
          <option>FAILED</option>
        </select>
        <label className="adm-filter-date">
          <span>{copy.audit.from}</span>
          <input
            type="datetime-local"
            value={filters.from}
            onChange={(event) =>
              setFilters((current) => ({
                ...current,
                from: event.target.value,
              }))
            }
          />
        </label>
        <label className="adm-filter-date">
          <span>{copy.audit.to}</span>
          <input type="datetime-local" value={filters.to} onChange={(event) => setFilters((current) => ({ ...current, to: event.target.value }))} />
        </label>
        <button disabled={busy || !canRead}>{copy.searchAction}</button></fieldset>
      </form>
      {canExport && <ExportLimits words={exportWords} />}
      {recoveryPending && <p role="status">{copy.audit.recoveryReady}</p>}
      {recoveryBlocked && <p role="alert">{copy.audit.recoveryUnavailable}</p>}
      {(needsAuthentication || recoveryBlocked) && <div><p>{copy.audit.reauthenticateHelp}</p><button type="button" disabled={busy} onClick={() => void reauthenticate()}>{copy.audit.reauthenticate}</button></div>}
      {canExport && <form className="adm-ops-form" onSubmit={(event) => { event.preventDefault(); prepareExport(); }}>
        <label>{copy.audit.exportReason}<input value={exportReason} minLength="10" disabled={recoveryBlocked || exportAttempted || busy || Boolean(exportJob)} onChange={(event) => setExportReason(event.target.value)} required /></label>
        <div className="adm-audit-export-privacy">
          <label className="adm-checkbox-field"><input type="checkbox" checked={includePII} aria-describedby={privacyID} disabled={recoveryBlocked || !canExportPII || exportAttempted || busy || Boolean(exportJob)} onChange={(event) => setIncludePII(event.target.checked)} />{privacyWords.includeUnredacted}</label>
          <small id={privacyID}>{privacyWords.defaultRedacted} {privacyWords.recentAuth} {!canExportPII && (includePII ? privacyWords.savedSelectionUnavailable : privacyWords.permissionUnavailable)}</small>
        </div>
        <button type="submit" disabled={recoveryBlocked || busy || Boolean(exportJob)}>{exportAttempted ? copy.audit.retryExport : exportWords.reviewButton}</button>
        {exportAttempted && !exportJob && <small role="status">{busy ? exportWords.submitting : exportOutcomeUnknownRef.current ? exportWords.unknown : copy.audit.retrySameExport}</small>}
      </form>}
      {exportConfirmation && exportConfirmation.client === client && exportConfirmation.actor === currentUserID && exportConfirmation.epoch === confirmationEpoch && canExport &&
        <AuditExportConfirmation snapshot={exportConfirmation} words={exportWords} copy={copy} onClose={closeExportConfirmation}
          onConfirm={() => confirmExport(exportConfirmation)} disabled={busy || recoveryBlocked || exportConfirmation.attempt.includePII && !canExportPII} />}
      {exportJob && (
        <div className="adm-inline-message">
          <strong>
            {copy.audit.exportStatus}: {exportWords.status[exportJob.status] || exportWords.statusUnknown}
          </strong>
          {typeof ticketExpiry === 'string' && Number.isFinite(Date.parse(ticketExpiry)) && <p>{exportWords.ticketExpiry}: <time dateTime={ticketExpiry}>{formatDate(ticketExpiry)}</time></p>}
          {exportKey && (
            <>
              <br />
              <span>{copy.audit.saveKey}</span>
              <br />
              <code>{exportKey}</code>
            </>
          )}
          <br />
          {recoveryPending && exportAttemptRef.current && ['PENDING', 'GENERATING', 'READY'].includes(exportJob.status) && <button disabled={busy || recoveryBlocked || (exportAttemptRef.current.includePII && !canExportPII)} onClick={() => void createExport()}>{copy.audit.recoverOriginal}</button>}
          <button disabled={busy || recoveryBlocked} onClick={refreshExport}>
            {copy.refresh}
          </button>
          {exportJob.status === 'READY' && (
            <button disabled={busy || recoveryBlocked || ticketIssueUnknown || (Boolean(exportJob.download_ticket_issued_at) && !exportTicketRef.current)} onClick={downloadExport}>
              {copy.audit.download}
            </button>
          )}
          {ticketIssueUnknown && <small role="status">{copy.audit.ticketIssueUnknown}</small>}
          {exportJob.download_ticket_issued_at && !exportTicketRef.current && <small role="status">{copy.audit.ticketUnavailable}</small>}
          <button type="button" disabled={busy} onClick={newExport}>{copy.audit.newExport}</button>
        </div>
      )}
      {error && <div className="adm-inline-message error" role="alert">{error}</div>}
      <Table columns={columns} items={items} copy={copy} />
    </section>

    </>
  );
}

function AuditDetail({ copy, item, onBack, language }) {
  const actionWords = auditActionCopy[language] || auditActionCopy.zh;
  const actionLabel = auditActionLabel(language, item.action);
  const actionCode = <AuditActionCode key={item.audit_id + ':' + item.action} value={item.action} words={actionWords} />;
  const rows = [
    [copy.audit.source, 'Account Service'],
    [copy.audit.actor, item.actor_display_name || item.actor_email || <TechnicalID key="audit.actor" value={item.actor_user_id} copy={copy} compact={false} />],
    [copy.audit.action, actionLabel || actionCode],
    ...(actionLabel ? [[actionWords.code, actionCode]] : []),
    [copy.audit.resource, <span key="audit.resource">{item.resource_type} · <TechnicalID value={item.resource_id} copy={copy} compact={false} /></span>],
    [copy.audit.result, <Status key="audit.result" value={item.result} />],
    [copy.audit.request, <TechnicalID key="audit.request" value={item.request_id} copy={copy} compact={false} />],
    [copy.audit.organization, <TechnicalID key="audit.organization" value={item.active_organization_id} copy={copy} compact={false} />],
    [copy.audit.occurred, formatDate(item.occurred_at)],
  ];
  return (
    <DetailDrawer label={copy.audit.detail} onClose={onBack}>
    <section className="adm-panel">
      <button className="adm-back" onClick={onBack}>
        ← {copy.audit.back}
      </button>
      <SectionHeader title={copy.audit.detail} copy={copy} />
      <dl className="adm-detail-list">
        {rows.map(([label, value]) => (
          <div key={label}>
            <dt>{label}</dt>
            <dd>{value}</dd>
          </div>
        ))}
      </dl>
      <h3>{copy.audit.changeSummary}</h3>
      <pre className="adm-json-detail">{JSON.stringify(item.change_summary || {}, null, 2)}</pre>
    </section>
    </DetailDrawer>
  );
}

function Status({value}) {return <span className="adm-status">{String(value || '—')}</span>;}
export function AuditWorkspace({client,copy,permissions,currentUserID,language = 'zh'}){
 const [data,setData]=useState({audits:[]});
 const [loading,setLoading]=useState(true);
 const [loadError,setLoadError]=useState(false);
 const [retry,setRetry]=useState(0);
 const canRead=(permissions || []).includes('platform.audit.read');
 useEffect(()=>{
  let live=true;setLoading(true);setLoadError(false);
  if(!canRead){setData({audits:[]});setLoading(false);return()=>{live=false};}
  client.audits().then(result=>{
   if(!Array.isArray(result?.items))throw new Error('INVALID_AUDIT_LOGS_ACK');
   if(live)setData({audits:result.items});
  }).catch(()=>{if(live)setLoadError(true)}).finally(()=>{if(live)setLoading(false)});
  return()=>{live=false};
 },[client,retry,canRead]);
 if(loading)return <p role="status">{copy.loading}</p>;
 if(loadError)return <div role="alert">{copy.audit.actionFailed}<button onClick={()=>setRetry(value=>value+1)}>{copy.refresh}</button></div>;
 return <Audit copy={copy} data={data} client={client} setData={setData} permissions={permissions} currentUserID={currentUserID} language={language}/>;
}
