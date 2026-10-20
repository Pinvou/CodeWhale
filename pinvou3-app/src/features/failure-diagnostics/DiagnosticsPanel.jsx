import { useCallback, useRef, useState } from 'react';
import { bridge, usePlatformCapability } from '../../hooks/useBridge.js';

export default function DiagnosticsPanel({ copy, sessionId, turnId, compact = false }) {
  const supported = usePlatformCapability('failureDiagnostics');
  const [open, setOpen] = useState(false);
  const [data, setData] = useState(null);
  const [selected, setSelected] = useState('');
  const [notes, setNotes] = useState('');
  const [files, setFiles] = useState([]);
  const [preview, setPreview] = useState(null);
  const [consent, setConsent] = useState(false);
  const [busy, setBusy] = useState(false);
  const [message, setMessage] = useState('');
  const request = useRef(0);
  const locked = useRef(false);
  const invalidate = () => { request.current += 1; setPreview(null); setConsent(false); setMessage(''); };
  const refresh = useCallback(async () => {
    const result = await bridge.feedback.listFailureDiagnostics();
    setData(result);
    if (!result.enabled) { setPreview(null); setConsent(false); setFiles([]); setNotes(''); }
    return result;
  }, []);
  const run = async action => {
    if (locked.current) return;
    locked.current = true; setBusy(true); setMessage('');
    try { await action(); } catch { setMessage(copy.error); }
    finally { locked.current = false; setBusy(false); }
  };
  const incidents = (data?.incidents || []).filter(item => (!sessionId || item.session_id === sessionId) && (!turnId || item.turn_id === turnId));
  const incident = incidents.find(item => item.diagnostic_id === selected);
  if (!supported) return null;
  return <section className="my-3 rounded-2xl border border-black/10 bg-white/60 p-3 text-sm dark:border-white/10 dark:bg-white/5" data-testid="failure-diagnostics">
    <button type="button" className="font-medium text-blue-600" aria-expanded={open} onClick={() => {
      setOpen(value => !value);
      if (!open) void run(refresh);
    }}>{compact ? copy.report : copy.title}</button>
    {open && <div className="mt-3 space-y-3">
      <p className="text-xs leading-5 text-gray-500">{copy.automatic}</p>
      {data && !data.enabled && <p role="status">{copy.disabled}</p>}
      <button type="button" disabled={busy} onClick={() => void run(refresh)}>{copy.refresh}</button>
      {data?.enabled && !incidents.length && <p role="status">{copy.empty}</p>}
      {!!incidents.length && <>
        <label className="block">{copy.choose}
          <select className="mt-1 w-full rounded-lg border p-2 text-black" value={selected} disabled={busy} onChange={event => { invalidate(); setSelected(event.target.value); }}>
            <option value="">{copy.choose}</option>
            {incidents.map(item => <option key={item.diagnostic_id} value={item.diagnostic_id}>{item.diagnostic_code} · {item.diagnostic_id}</option>)}
          </select>
        </label>
        {incident && <details><summary>{copy.timeline}</summary><pre className="max-h-40 overflow-auto whitespace-pre-wrap break-words text-xs">{JSON.stringify(incident.diagnostic_steps, null, 2)}</pre></details>}
        <label className="block">{copy.notes}<textarea className="mt-1 w-full rounded-lg border p-2 text-black" maxLength={6000} rows={3} disabled={busy} value={notes} onChange={event => { invalidate(); setNotes(event.target.value); }} /></label>
        <p className="text-xs text-gray-500">{copy.attachmentHint}</p>
        <button type="button" disabled={busy} onClick={() => void run(async () => {
          const chosen = await bridge.feedback.pickDiagnosticFiles();
          if (chosen.length > 3) throw new Error('diagnostic_file_limit');
          invalidate(); setFiles(chosen);
        })}>{copy.attachments}</button>
        {files.map((file, index) => <div key={file} className="flex items-center gap-2"><span>{file.split(/[\\/]/).at(-1)}</span><button type="button" aria-label={copy.remove} disabled={busy} onClick={() => { invalidate(); setFiles(values => values.filter((_, at) => at !== index)); }}>×</button></div>)}
        <button type="button" className="block rounded-lg bg-blue-600 px-3 py-2 text-white disabled:opacity-40" disabled={busy || !incident || !data.enabled} onClick={() => void run(async () => {
          const revision = ++request.current;
          const result = await bridge.feedback.previewFailureDiagnostic({ incident_id: selected, notes, attachments: files });
          if (revision === request.current) { setPreview(result); setConsent(false); }
        })}>{copy.preview}</button>
        {preview && <div className="space-y-2 rounded-xl border p-3">
          <strong>{copy.previewTitle}</strong>
          <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-words text-xs">{JSON.stringify({ ...preview.report, attachments: preview.report.attachments.map(item => ({ kind: item.kind, ...(item.kind === 'text' ? { content: item.content } : {}) })) }, null, 2)}</pre>
          {preview.report.attachments.filter(item => item.kind !== 'text').map((image, index) => <img key={index} alt={copy.previewTitle} className="max-h-64 max-w-full rounded-lg" src={`data:${image.kind};base64,${image.content}`} />)}
          <label className="flex gap-2"><input type="checkbox" checked={consent} disabled={busy} onChange={event => setConsent(event.target.checked)} /><span>{copy.consent}</span></label>
          <button type="button" className="rounded-lg bg-blue-600 px-3 py-2 text-white disabled:opacity-40" disabled={busy || !consent || !data.enabled} onClick={() => void run(async () => {
            await bridge.feedback.submitFailureDiagnostic(preview.preview_token);
            invalidate(); setMessage(copy.queued); await refresh();
          })}>{copy.submit}</button>
        </div>}
      </>}
      {(data?.reports || []).map(report => <p key={report.report_id} className="text-xs">{report.incident_id} · {report.status === 'received' ? copy.received : report.status === 'rejected' ? copy.rejected : report.reason === 'server_upgrade_required' ? copy.serverUpgrade : report.reason === 'incident_pending' ? copy.incidentPending : copy.queued}</p>)}
      {busy && <p role="status">{copy.busy}</p>}
      {message && <p role="status">{message}</p>}
    </div>}
  </section>;
}
