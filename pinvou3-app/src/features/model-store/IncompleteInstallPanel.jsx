import { useState } from 'react';
import { previewIncomplete } from './model-store-api.js';
import { MsButton } from './model-store-ui.jsx';

const EMPTY = Object.freeze([]);

export function describeIncompleteError(copy, value) {
  const detail = String(value);
  const translated = Object.entries(copy.incompleteErrors).find(([key]) => detail.includes(key))?.[1];
  return translated ? `${translated} (${detail})` : detail;
}

export function IncompleteInstallPanel({ installations = EMPTY, error, copy, onRetry, onRefresh, isRetrying }) {
  const [preview, setPreview] = useState(null);
  const [loading, setLoading] = useState(false);
  const [requestError, setRequestError] = useState('');
  if (installations.length === 0 && !error) return null;
  const describe = value => describeIncompleteError(copy, value);
  const inspect = async item => {
    setLoading(true);
    setRequestError('');
    setPreview(null);
    try {
      setPreview(await previewIncomplete(item.runId));
    } catch (failure) {
      setRequestError(describe(failure));
    } finally {
      setLoading(false);
    }
  };
  return <section data-testid="incomplete-installations" className="mx-3 mb-5 rounded-2xl border border-amber-300/50 bg-amber-50/60 p-4 dark:bg-amber-950/20">
    <h3 className="text-sm font-semibold">{copy.incompleteTitle}</h3>
    <p className="mt-1 text-xs opacity-75">{copy.incompleteHint}</p>
    {(error || requestError) && <p role="alert" className="mt-2 break-words text-xs">{requestError || describe(error)}</p>}
    {installations.map(item => <div key={item.runId} className="mt-3 flex flex-wrap items-center justify-between gap-3">
      <div className="min-w-0 text-xs">
        <div className="font-semibold">{item.cardId}</div>
        <p className="mt-1 break-all opacity-75">{item.runId}</p>
        {item.error && <p className="mt-1 break-words">{describe(item.error)}</p>}
        {!item.terminal && <p className="mt-1 opacity-75">{copy.incompletePending}</p>}
        {item.terminal && !item.canPreview && <p className="mt-1 opacity-75">{copy.incompleteUnknownSource}</p>}
      </div>
      <MsButton disabled={loading || !item.canPreview || isRetrying?.(item.cardId)} onClick={() => inspect(item)}>{copy.incompletePreview}</MsButton>
    </div>)}
    {preview && <div role="alertdialog" aria-modal="false" aria-label={copy.incompletePreview} className="my-3 rounded-xl border border-amber-500 p-3 text-xs">
      <p className="font-semibold">{preview.cardName || preview.cardId}</p>
      <p className="mt-2">{preview.kind === 'download' ? copy.incompleteDownloadOnly : copy.incompleteLocalDeploy}</p>
      <p className="mt-2">{copy.incompleteConfirmHint}</p>
      {preview.legacyContext && <p className="mt-2">{copy.incompleteLegacy}</p>}
      <p className="mt-2 break-all">{copy.incompletePackage} {preview.packageSha256}</p>
      <p className="mt-2 break-all">{copy.incompleteStorage} {preview.modelsDir} · {preview.stagingDir}</p>
      <div className="mt-3 flex gap-3">
        <MsButton disabled={loading || isRetrying?.(preview.cardId)} onClick={() => {
          const acceptedPreview = preview;
          setPreview(null);
          onRetry(acceptedPreview);
        }}>{copy.incompleteRetry}</MsButton>
        <MsButton tone="ghost" onClick={() => setPreview(null)}>{copy.taskCancel}</MsButton>
      </div>
    </div>}
    <MsButton tone="ghost" className="mt-3" disabled={loading} onClick={onRefresh}>{copy.refresh}</MsButton>
  </section>;
}
