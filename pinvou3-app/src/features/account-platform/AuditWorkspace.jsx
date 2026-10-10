import React, {useEffect,useRef,useState} from 'react';
import {secureRequestID} from './request-id.js';
import {acquireAuditExportTicket,newAuditExportAttempt,submitAuditExportAttempt} from './admin-audit-export.js';
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
                <td key={column.key}>{column.render ? column.render(item) : (item[column.key] ?? '—')}</td>
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

function DetailDrawer({ label, onClose, children }) {
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
      <aside ref={drawerRef} className="adm-detail-drawer" role="dialog" aria-modal="true" aria-label={label} tabIndex="-1" onKeyDown={trapFocus}>
        {children}
      </aside>
    </div>
  );
}

function Audit({ copy, data, client, setData, permissions }) {
  const [filters, setFilters] = useState(() => readAuditQueryFilters());
  const [detail, setDetail] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [includePII, setIncludePII] = useState(false);
  const [exportJob, setExportJob] = useState(null);
  const [exportKey, setExportKey] = useState('');
  const [exportReason, setExportReason] = useState('');
  const [exportAttempted, setExportAttempted] = useState(false);
  const [ticketIssueUnknown, setTicketIssueUnknown] = useState(false);
  const liveRef = useRef(true);
  useEffect(() => { liveRef.current = true; return () => { liveRef.current = false; }; }, []);
  const exportAttemptRef = useRef(null);
  const exportOutcomeUnknownRef = useRef(false);
  const exportTicketRef = useRef(null);
  const canRead = (permissions || []).includes('platform.audit.read');
  const canExport = (permissions || []).includes('platform.audit.export');
  const canExportPII = (permissions || []).includes('platform.audit.pii.export');
  const items = data.audits || [];
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
      setDetail(itemDetail);
    } catch {
      if (liveRef.current) setError(copy.audit.actionFailed);
    } finally {
      if (liveRef.current) setBusy(false);
    }
  };
  const createExport = async () => {
    if (!exportAttemptRef.current) {
      let filter;
      try { filter = queryFilters(); } catch { setError(copy.audit.exportRequestInvalid); return; }
      if (!filter.from || !filter.to) { setError(copy.audit.exportRangeRequired); return; }
      try {
        exportAttemptRef.current = newAuditExportAttempt(filter, exportReason, includePII,
          `audit-export-${secureRequestID()}`);
      } catch { setError(copy.audit.exportRequestInvalid); return; }
      setExportAttempted(true);
    }
    setBusy(true);
    setError('');
    try {
      const job = await submitAuditExportAttempt(client, exportAttemptRef.current);
      if (!liveRef.current) return;
      exportOutcomeUnknownRef.current = false;
      setExportJob(job);
      setExportKey(job.decryption_key || '');
    } catch (cause) {
      if (!liveRef.current) return;
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
    if (!exportJob) return;
    setBusy(true);
    setError('');
    try {
      const job = await client.auditExport(exportJob.export_id);
      if (!liveRef.current) return;
      setExportJob(job);
      if (!job.download_ticket_issued_at) setTicketIssueUnknown(false);
    } catch {
      if (liveRef.current) setError(copy.audit.exportFailed);
    } finally {
      if (liveRef.current) setBusy(false);
    }
  };
  const downloadExport = async () => {
    if (!exportJob || (exportJob.download_ticket_issued_at && !exportTicketRef.current) || ticketIssueUnknown) return;
    setBusy(true);
    setError('');
    try {
      if (!exportTicketRef.current) {
        try {
          exportTicketRef.current = await acquireAuditExportTicket(client, exportJob.export_id, null);
        } catch (error) {
          setTicketIssueUnknown(true);
          throw error;
        }
      }
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
    } catch {
      if (liveRef.current) setError(copy.audit.exportFailed);
    } finally {
      if (liveRef.current) setBusy(false);
    }
  };
  if (detail) return <AuditDetail copy={copy} item={detail} onBack={() => setDetail(null)} />;
  const columns = [
    {
      key: 'action',
      label: copy.audit.action,
      render: (item) => (
        <button className="adm-name-link" onClick={() => showDetail(item)}>
          {item.action}
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
      <form className="adm-ops-form" onSubmit={search}><fieldset disabled={busy || exportAttempted || Boolean(exportJob)}>
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
      <p>{copy.audit.exportLimits}</p>
      {canExport && <form className="adm-ops-form" onSubmit={(event) => { event.preventDefault(); void createExport(); }}>
        <label>{copy.audit.exportReason}<input value={exportReason} minLength="10" disabled={exportAttempted || busy || Boolean(exportJob)} onChange={(event) => setExportReason(event.target.value)} required /></label>
        <label><input type="checkbox" checked={includePII} disabled={!canExportPII || exportAttempted || busy || Boolean(exportJob)} title={!canExportPII ? copy.permissionDenied : undefined} onChange={(event) => setIncludePII(event.target.checked)} />{copy.audit.includePII}</label>
        <button type="submit" disabled={busy || Boolean(exportJob)}>{exportAttempted ? copy.audit.retryExport : copy.audit.export}</button>
        {exportAttempted && !exportJob && <small role="status">{copy.audit.retrySameExport}</small>}
      </form>}
      {exportJob && (
        <div className="adm-inline-message">
          <strong>
            {copy.audit.exportStatus}: {exportJob.status}
          </strong>
          {exportKey && (
            <>
              <br />
              <span>{copy.audit.saveKey}</span>
              <br />
              <code>{exportKey}</code>
            </>
          )}
          <br />
          <button disabled={busy} onClick={refreshExport}>
            {copy.refresh}
          </button>
          {exportJob.status === 'READY' && (
            <button disabled={busy || ticketIssueUnknown || (Boolean(exportJob.download_ticket_issued_at) && !exportTicketRef.current)} onClick={downloadExport}>
              {copy.audit.download}
            </button>
          )}
          {ticketIssueUnknown && <small role="status">{copy.audit.ticketIssueUnknown}</small>}
          {exportJob.download_ticket_issued_at && !exportTicketRef.current && <small role="status">{copy.audit.ticketUnavailable}</small>}
          <button type="button" disabled={busy} onClick={() => { exportAttemptRef.current = null; exportOutcomeUnknownRef.current = false; exportTicketRef.current = null; setTicketIssueUnknown(false); setExportAttempted(false); setExportJob(null); setExportKey(''); setExportReason(''); setIncludePII(false); }}>{copy.audit.newExport}</button>
        </div>
      )}
      {error && <div className="adm-inline-message error" role="alert">{error}</div>}
      <Table columns={columns} items={items} copy={copy} />
    </section>

    </>
  );
}

function AuditDetail({ copy, item, onBack }) {
  const rows = [
    [copy.audit.source, 'Account Service'],
    [copy.audit.actor, item.actor_display_name || item.actor_email || <TechnicalID key="audit.actor" value={item.actor_user_id} copy={copy} compact={false} />],
    [copy.audit.action, item.action],
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
export function AuditWorkspace({client,copy,permissions}){
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
 return <Audit copy={copy} data={data} client={client} setData={setData} permissions={permissions}/>;
}
