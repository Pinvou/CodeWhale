import { useSyncExternalStore } from 'react';
import { MsButton, TEXT_SECONDARY } from './model-store-ui.jsx';
import { operationWarningText } from './model-store-logic.js';
import {
  dismissModelStoreOperationResult,
  getModelStoreOperationResults,
  subscribeModelStoreOperationResults,
} from './operation-results.js';

function operationTitleText(copy, { kind, title, cardName }) {
  // Older in-memory tasks only carry their rendered title. Keep that fallback
  // while new results retain the card name and translate at display time.
  if (!cardName) return title;
  switch (kind) {
    case 'version_stage':
    case 'version_switch':
    case 'version_delete': return copy.versionTask(cardName);
    case 'uninstall': return copy.taskUninstall(cardName);
    default: return title;
  }
}

function operationDoneText(copy, { kind, title, result, resultTruncated }) {
  if (resultTruncated && result == null) return copy.operationResultUnavailable;
  switch (kind) {
    case 'force_remove': return copy.forceRemoveDone(title, result.remaining_paths.length);
    case 'uninstall': return copy.operationUninstallRemaining;
    case 'version_delete': return copy.versionDeleted((result.freed_bytes || 0) / 1024 ** 3);
    case 'version_stage': return copy.versionStaged;
    default: return copy.versionSwitched;
  }
}

/** Nonmodal results preserve the user's current card/dialog and survive navigation. */
export function ModelStoreOperationResults({ copy, hiddenResultId = null }) {
  const allResults = useSyncExternalStore(subscribeModelStoreOperationResults, getModelStoreOperationResults);
  const results = allResults.filter((result) => result.id !== hiddenResultId);
  if (!results.length) return null;
  return (
    <section className="mb-5 max-h-80 space-y-3 overflow-y-auto overscroll-contain rounded-xl" aria-label={copy.operationResultsTitle} data-testid="model-version-results">
      <h3 className="text-[13px] font-semibold">{copy.operationResultsTitle}</h3>
      {results.map(({ id, title, cardName, kind, result, resultTruncated }) => (
        <div key={id} className="rounded-xl border border-black/10 p-4 dark:border-white/10" data-testid="model-version-operation-result" data-operation-kind={kind}>
          <div className="flex items-start justify-between gap-3">
            <div className="min-w-0 text-[13px]">
              <p className="font-semibold">{operationTitleText(copy, { kind, title, cardName })}</p>
              <p className={`mt-1 ${TEXT_SECONDARY}`} role="status">{operationDoneText(copy, { kind, title, result, resultTruncated })}</p>
              {resultTruncated && result != null ? <p className="mt-1">{copy.operationResultSummarized}</p> : null}
              {result?.warning ? <p className="mt-1">{operationWarningText(copy, result.warning)}</p> : null}
            </div>
            <MsButton tone="ghost" onClick={() => dismissModelStoreOperationResult(id)}>{copy.operationResultDismiss}</MsButton>
          </div>
          {result?.remaining_paths.length ? (
            <div className="mt-3 text-[13px]" data-testid="model-version-cleanup-result">
              <p>{kind === 'force_remove' || kind === 'uninstall' ? copy.forceRemoveResidualTitle(result.remaining_paths.length) : copy.versionRemaining}</p>
              <ul className="mt-2 space-y-1 break-all text-[12px]">{result.remaining_paths.map((path) => <li key={path}>{path}</li>)}</ul>
            </div>
          ) : null}
        </div>
      ))}
    </section>
  );
}
