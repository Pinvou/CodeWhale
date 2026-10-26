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
  const [quarantineConfirm, setQuarantineConfirm] = useState(null);
  const confirmPanel = useRef(null);
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

  useEffect(() => {
    if (quarantineConfirm) confirmPanel.current?.focus();
  }, [quarantineConfirm]);

  const archive = async (id, quarantine = false) => {
    const intent = ++request.current;
    setBusy(true);
    setStatus(null);
    try {
      const cleaned = quarantine
        ? await api.quarantineRemoteProxyHistory(id, true)
        : await api.archiveRemoteProxyHistory(id);
      if (request.current !== intent) return;
      const refreshed = await refresh();
      if (request.current !== intent + 1) return;
      setHistory(null);
      setStatus(quarantine
        ? (!refreshed ? 'quarantineRefreshPending' : cleaned ? 'quarantineCompleted' : 'quarantineCleanupPending')
        : (!refreshed ? 'completedRefreshPending' : cleaned ? 'completed' : 'cleanupPending'));
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
        <button type="button" disabled={busy || !!quarantineConfirm} onClick={() => refresh(page)}>{copy.refresh}</button>
      </div>
      <p className="text-[11px] leading-5">{copy.hint}</p>
      {status && <p role={['completed', 'cleanupPending', 'completedRefreshPending', 'quarantineCompleted', 'quarantineRefreshPending', 'quarantineCleanupPending'].includes(status) ? 'status' : 'alert'}>{copy[status]}</p>}
      {quarantineConfirm && <section role="alertdialog" aria-modal="false" aria-labelledby="remote-quarantine-confirm-title"
        tabIndex={-1} ref={confirmPanel} className="space-y-3 rounded-lg border border-amber-500/40 p-3"
        onKeyDown={event => { if (event.key === 'Escape') setQuarantineConfirm(null); }}>
        <h4 id="remote-quarantine-confirm-title">{copy.quarantineTitle}: {quarantineConfirm.title || copy.untitled}</h4>
        <p>{copy.quarantineWarning}</p>
        <div className="flex gap-3">
          <button type="button" onClick={() => setQuarantineConfirm(null)}>{t.cpCancel}</button>
          <button type="button" onClick={() => {
            const id = quarantineConfirm.localSessionId;
            setQuarantineConfirm(null);
            archive(id, true);
          }}>{copy.quarantineConfirm}</button>
        </div>
      </section>}
      <h4>{copy.active}</h4>
      {active.length === 0 && <p>{copy.empty}</p>}
      {active.map(item => (
        <div key={item.localSessionId} className="flex items-center justify-between gap-3">
          <span>{item.title || copy.untitled}</span>
          <div className="flex gap-3">
            <button type="button" disabled={busy || !!quarantineConfirm || item.pendingCount > 0} onClick={() => archive(item.localSessionId)}>{copy.archive}</button>
            {item.pendingCount > 0 && typeof api.quarantineRemoteProxyHistory === 'function'
              && <button type="button" disabled={busy || !!quarantineConfirm} onClick={() => setQuarantineConfirm(item)}>{copy.quarantine}</button>}
          </div>
        </div>
      ))}
      <h4>{copy.archived}</h4>
      {archives.length === 0 && <p>{copy.empty}</p>}
      {archives.map(item => <button type="button" key={item.localSessionId} disabled={busy || !!quarantineConfirm}
        className="block text-left" onClick={() => read(item.localSessionId)}>{item.title || copy.untitled}</button>)}
      <div className="flex gap-3">
        <button type="button" disabled={busy || !!quarantineConfirm || page.length === 1} onClick={() => refresh(page.slice(0, -1))}>{copy.previous}</button>
        <button type="button" disabled={busy || !!quarantineConfirm || archives.length < 256}
          onClick={() => refresh([...page, archives.at(-1).archiveCursor])}>{copy.next}</button>
      </div>
      {history && <section aria-label={copy.readOnly} className="max-h-80 space-y-2 overflow-auto">
        <h4>{copy.readOnly}: {history.title || copy.untitled}</h4>
        {history.quarantined && <div data-testid="remote-proxy-frozen-facts">
          <p>{copy.quarantinedHint}</p>
          <p>{copy.frozenPrepared}: {history.frozenPreparedCount}</p>
          <p>{copy.frozenUnknown}: {history.pendingUnknownCount}</p>
        </div>}
        {history.entries.map(entry => <div key={`${entry.journalEpoch}:${entry.sequence}`}>
          <p className="whitespace-pre-wrap break-words">{entry.body}</p>
          {entry.attachments.map(attachment => <p key={attachment.id}>{attachment.displayName}</p>)}
        </div>)}
      </section>}
    </section>
  );
}
