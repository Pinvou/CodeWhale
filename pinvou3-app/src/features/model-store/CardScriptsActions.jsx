import { useState } from 'react';
import { MsButton, TEXT_SECONDARY } from './model-store-ui.jsx';

/** Explicit opt-in; opening model details never fetches or changes scripts. */
export function CardScriptsActions({ record, copy, busy, onAction }) {
  const [pending, setPending] = useState(null);
  const disabled = !!busy || !!record?.running;
  return (
    <section className="mt-5" data-testid="model-store-card-scripts">
      <h3 className="mb-2 text-[13px] font-medium">{copy.cardScriptsTitle}</h3>
      <p className={`text-[12px] ${TEXT_SECONDARY}`}>{record?.running ? copy.cardScriptsStopFirst : copy.cardScriptsHint}</p>
      <div className="mt-2 flex flex-wrap gap-2">
        <MsButton disabled={disabled} onClick={() => setPending('update')} data-testid="model-store-update-scripts">{copy.cardScriptsUpdate}</MsButton>
        <MsButton disabled={disabled} onClick={() => setPending('rollback')} data-testid="model-store-rollback-scripts">{copy.cardScriptsRollback}</MsButton>
      </div>
      {pending ? (
        <div className="mt-3 rounded-xl border border-slate-200 p-3 dark:border-white/10" role="group" aria-label={copy.cardScriptsConfirm}>
          <p className="text-[13px]">{pending === 'rollback' ? copy.cardScriptsRollbackWarning : copy.cardScriptsUpdateWarning}</p>
          <div className="mt-2 flex flex-wrap justify-end gap-2">
            <MsButton onClick={() => setPending(null)}>{copy.cardScriptsCancel}</MsButton>
            <MsButton tone="primary" disabled={disabled} onClick={() => {
              const rollback = pending === 'rollback';
              setPending(null);
              onAction(record, rollback);
            }}>{copy.cardScriptsConfirm}</MsButton>
          </div>
        </div>
      ) : null}
    </section>
  );
}
