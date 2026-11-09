import { useEffect, useState } from 'react';
import { fetchVersions } from './model-store-api.js';
import { formatBytes } from './model-store-logic.js';
import { MsButton, MsDialog, MsDialogTitle, TEXT_SECONDARY } from './model-store-ui.jsx';

const sizeLabel = (bytes) => formatBytes(Math.max(0, bytes || 0));

/** All destructive actions are explicit and revalidated by the backend. */
export function ModelVersionsDialog({ record, copy, theme, busy, onClose, onAction }) {
  const [versions, setVersions] = useState(null);
  const [error, setError] = useState(null);
  const [attempt, setAttempt] = useState(0);
  const [measuring, setMeasuring] = useState(false);
  const [confirmDelete, setConfirmDelete] = useState(null);
  useEffect(() => {
    let cancelled = false;
    fetchVersions(record.card_id).then((rows) => {
      if (!cancelled) { setVersions(rows); setError(null); }
    }).catch((failure) => { if (!cancelled) setError(String(failure?.message || failure)); });
    return () => { cancelled = true; };
  }, [record.card_id, attempt, busy]);

  // Registered sizes can go stale when files change outside the app; the
  // explicit re-measure is the invalidation entry (review round 7).
  const remeasure = () => {
    if (measuring) return;
    setMeasuring(true);
    fetchVersions(record.card_id, { measure: true }).then((rows) => {
      setVersions(rows);
      setError(null);
    }).catch((failure) => { setError(String(failure?.message || failure)); })
      .finally(() => { setMeasuring(false); });
  };

  const act = (action, version) => {
    onAction(record, action, version.version_id);
    onClose();
  };
  return (
    <MsDialog open onClose={onClose} theme={theme} labelledBy="model-versions-title" testId="model-store-versions-dialog">
      <MsDialogTitle id="model-versions-title" onClose={onClose} closeLabel={copy.detailClose}>
        {copy.versionTitle(record.card_name)}
      </MsDialogTitle>
      <p className={`mb-4 text-[13px] leading-6 ${TEXT_SECONDARY}`}>{copy.versionHint}</p>
      <p className={`mb-4 text-[13px] leading-6 ${TEXT_SECONDARY}`} data-testid="model-version-cost-hint">{copy.versionCostHint}</p>
      {record.card_id === 'qwen38-27b-nvfp4-mtp' ? (
        <p className={`mb-4 text-[13px] leading-6 ${TEXT_SECONDARY}`} data-testid="model-version-mtp-network-hint">{copy.mtpNetworkHint}</p>
      ) : null}
      {record.running ? <p role="status" className="mb-3 text-[13px] text-amber-700 dark:text-amber-300">{copy.versionStopFirst}</p> : null}
      {error ? <div role="alert"><p>{error}</p><MsButton tone="ghost" onClick={() => setAttempt((n) => n + 1)}>{copy.retry}</MsButton></div> : null}
      {!versions && !error ? <p role="status">{copy.cardUpdateChecking}</p> : null}
      {versions ? (
        <div className="mb-2 flex justify-end" data-testid="model-version-measure-row">
          <MsButton tone="ghost" disabled={busy || measuring} onClick={remeasure}>
            {measuring ? copy.versionMeasuring : copy.versionMeasure}
          </MsButton>
        </div>
      ) : null}
      {confirmDelete ? (
        <div className="space-y-3" data-testid="model-version-delete-confirm">
          <p>{copy.versionDeleteConfirm(confirmDelete.filename || confirmDelete.version_id.slice(0, 12), sizeLabel(confirmDelete.size_bytes))}</p>
          <div className="flex justify-end gap-2">
            <MsButton tone="ghost" onClick={() => setConfirmDelete(null)}>{copy.detailClose}</MsButton>
            <MsButton tone="danger" disabled={busy} onClick={() => act('delete', confirmDelete)}>{copy.versionDelete}</MsButton>
          </div>
        </div>
      ) : (
        <div className="max-h-[55vh] space-y-3 overflow-y-auto">
          {versions?.map((version) => (
            <div key={version.version_id} className="rounded-xl border border-black/10 p-3 dark:border-white/10" data-testid="model-version-row">
              <div className="flex items-start justify-between gap-3 text-[13px]">
                <div className="min-w-0 space-y-1">
                  <p className="break-all font-medium">{version.filename || version.version_id.slice(0, 12)}</p>
                  <p className={TEXT_SECONDARY}>{copy.versionStates[version.state] || copy.versionUnspecified} · {sizeLabel(version.size_bytes)}</p>
                  {version.engine_version ? <p className={TEXT_SECONDARY}>{version.engine_version}</p> : null}
                  {version.error ? <p role="alert" className="text-rose-600 dark:text-rose-400">{version.error === "version_download_interrupted" ? copy.versionInterrupted : version.error}</p> : null}
                </div>
                <div className="flex shrink-0 gap-1">
                  {version.can_retry ? <MsButton tone="ghost" disabled={busy} onClick={() => act('stage', version)}>{copy.retry}</MsButton> : null}
                  {version.can_switch ? <MsButton tone="ghost" disabled={busy || record.running} onClick={() => act('switch', version)}>{copy.versionSwitch}</MsButton> : null}
                  {version.can_delete ? <MsButton tone="ghost" disabled={busy} onClick={() => setConfirmDelete(version)}>{copy.versionDelete}</MsButton> : null}
                </div>
              </div>
            </div>
          ))}
        </div>
      )}
    </MsDialog>
  );
}
