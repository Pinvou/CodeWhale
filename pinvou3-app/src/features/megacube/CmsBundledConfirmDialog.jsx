import React, { useId, useRef } from 'react';
import { ModalShell } from '../../components/ModalShell.jsx';
import { useDialogFocusTrap } from '../../hooks/useDialogFocusTrap.js';

export function CmsBundledConfirmDialog({ copy, review, busy, blockedReason = '', actionLabel, impact, onCancel, onConfirm }) {
  const titleId = useId();
  const content = useRef(null);
  useDialogFocusTrap(content);
  return <ModalShell testid="cms-bundled-confirm" labelledBy={titleId}
    backdropLabel={copy.cancel} backdropClass="absolute inset-0 bg-black/40 backdrop-blur-sm"
    panelClass="w-full max-w-md max-h-[calc(100dvh-2rem)] overflow-y-auto rounded-2xl border border-black/10 bg-white p-5 text-[#1C1C1E] shadow-xl dark:border-white/10 dark:bg-[#1c1c1e] dark:text-[#F2F2F7]"
    busy={busy} onCancel={onCancel}>
    <div ref={content}>
      <h3 id={titleId} className="text-base font-semibold">{copy.bundledConfirmTitle}</h3>
      <p className="mt-3 text-sm text-black/70 dark:text-white/70">{copy.bundledConfirmHint}</p>
      <p className="mt-3 text-sm">{impact}</p>
      {review.phase === 'checking' && <p role="status" className="mt-3 text-sm">{copy.bundledChecking}</p>}
      {review.error && <p role="alert" className="mt-3 break-words text-sm text-red-700 dark:text-red-300">{review.error}</p>}
      {!review.error && blockedReason && <p role="alert" className="mt-3 text-sm text-red-700 dark:text-red-300">{blockedReason}</p>}
      <div className="mt-5 flex flex-wrap justify-end gap-2">
        <button type="button" disabled={busy} onClick={onCancel}
          className="rounded-lg px-4 py-2 text-sm hover:bg-black/5 focus-visible:outline-2 focus-visible:outline-blue-500 disabled:opacity-50 dark:hover:bg-white/10"
          data-testid="cms-bundled-cancel">{copy.cancel}</button>
        <button type="button" disabled={busy || !!blockedReason || review.phase !== 'ready'} onClick={onConfirm}
          className="rounded-lg bg-blue-600 px-4 py-2 text-sm text-white focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-blue-500 disabled:opacity-50"
          data-testid="cms-bundled-install">{copy.confirm} · {actionLabel}</button>
      </div>
    </div>
  </ModalShell>;
}
