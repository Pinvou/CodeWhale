import { useEffect, useRef, useState } from 'react';
import { bridge } from '../../hooks/useBridge.js';

export default function RemoteProxyArchivePanel({ t }) {
  const api = bridge.desktopRemoteControl;
  const copy = t.remoteArchive || {};
  const [active, setActive] = useState([]);
  const [archives, setArchives] = useState([]);
  const [page, setPage] = useState([null]);
  const [history, setHistory] = useState(null);
  const [status, setStatus] = useState(null);
  const [busy, setBusy] = useState(false);
  const request = useRef(0);
  const supported = typeof api?.listRemoteProxyArchives === 'function';

  const refresh = async (cursors = [null]) => {
    const intent = ++request.current;
    setBusy(true);
    setStatus(null);
    try {
      const [current, archived] = await Promise.all([
        api.listRemoteProxyArchiveCandidates(), api.listRemoteProxyArchives(cursors.at(-1)),
      ]);
      if (request.current !== intent) return false;
      setActive(current);
      setArchives(archived);
      setPage(cursors);
      return true;
    } catch {
      if (request.current === intent) setStatus('unavailable');
      return false;
    } finally {
      if (request.current === intent) setBusy(false);
    }
  };
  useEffect(() => {
    if (supported) refresh();
    return () => { ++request.current; };
    // The platform adapter is stable for this mounted host.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [supported]);

  const archive = async id => {
    const intent = ++request.current;
    setBusy(true);
    setStatus(null);
    try {
      const cleaned = await api.archiveRemoteProxyHistory(id);
      if (request.current !== intent) return;
      const refreshed = await refresh();
      if (request.current !== intent + 1) return;
      setHistory(null);
      setStatus(!refreshed ? 'completedRefreshPending' : cleaned ? 'completed' : 'cleanupPending');
    } catch (error) {
      if (request.current === intent) {
        setStatus(error.code === 'remote_proxy_archive_online' ? 'online'
          : error.code === 'remote_proxy_archive_pending' ? 'pending' : 'unavailable');
      }
    } finally {
      if (request.current === intent) setBusy(false);
    }
  };
  const read = async id => {
    const intent = ++request.current;
    setBusy(true);
    setStatus(null);
    try {
      const record = await api.readRemoteProxyHistory(id, { navigation: true });
      if (request.current !== intent) return;
      if (!record || !record.archived) throw new Error('remote_proxy_archive_unavailable');
      setHistory(record);
    } catch {
      if (request.current === intent) setStatus('unavailable');
    } finally {
      if (request.current === intent) setBusy(false);
    }
  };

  if (!supported) return null;
  return (
    <section className="space-y-3 rounded-xl border border-black/10 p-4 dark:border-white/10" data-testid="remote-proxy-archive">
      <div className="flex items-center justify-between gap-3">
        <h3 className="text-[13px] font-semibold">{copy.title}</h3>
        <button type="button" disabled={busy} onClick={() => refresh(page)}>{copy.refresh}</button>
      </div>
      <p className="text-[11px] leading-5">{copy.hint}</p>
      {status && <p role={['completed', 'cleanupPending', 'completedRefreshPending'].includes(status) ? 'status' : 'alert'}>{copy[status]}</p>}
      <h4>{copy.active}</h4>
      {active.length === 0 && <p>{copy.empty}</p>}
      {active.map(item => (
        <div key={item.localSessionId} className="flex items-center justify-between gap-3">
          <span>{item.title || copy.untitled}</span>
          <button type="button" disabled={busy} onClick={() => archive(item.localSessionId)}>{copy.archive}</button>
        </div>
      ))}
      <h4>{copy.archived}</h4>
      {archives.length === 0 && <p>{copy.empty}</p>}
      {archives.map(item => <button type="button" key={item.localSessionId} disabled={busy}
        className="block text-left" onClick={() => read(item.localSessionId)}>{item.title || copy.untitled}</button>)}
      <div className="flex gap-3">
        <button type="button" disabled={busy || page.length === 1} onClick={() => refresh(page.slice(0, -1))}>{copy.previous}</button>
        <button type="button" disabled={busy || archives.length < 256}
          onClick={() => refresh([...page, archives.at(-1).archiveCursor])}>{copy.next}</button>
      </div>
      {history && <section aria-label={copy.readOnly} className="max-h-80 space-y-2 overflow-auto">
        <h4>{copy.readOnly}: {history.title || copy.untitled}</h4>
        {history.entries.map(entry => <div key={`${entry.journalEpoch}:${entry.sequence}`}>
          <p className="whitespace-pre-wrap break-words">{entry.body}</p>
          {entry.attachments.map(attachment => <p key={attachment.id}>{attachment.displayName}</p>)}
        </div>)}
      </section>}
    </section>
  );
}
