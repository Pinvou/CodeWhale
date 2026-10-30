import { useCallback, useEffect, useRef, useState } from 'react';
import { checkCardUpdate } from './model-store-api.js';
import { MsButton, MsDialog, MsDialogTitle, MsRow, TEXT_SECONDARY } from './model-store-ui.jsx';

/** Review remote changes before replacing an installed card. */
export function CardUpdateDialog({ record, copy, theme, busy, onClose, onUpdate, onStage, onVersions }) {
  const [preview, setPreview] = useState(null);
  const [error, setError] = useState(null);
  const [checking, setChecking] = useState(true);
  const generation = useRef(0);
  const check = useCallback(async () => {
    const token = ++generation.current;
    setChecking(true);
    setPreview(null);
    setError(null);
    try {
      const next = await checkCardUpdate(record.card_id);
      if (token === generation.current) setPreview(next);
    } catch (failure) {
      if (token === generation.current) setError(String(failure?.message || failure));
    } finally {
      if (token === generation.current) setChecking(false);
    }
  }, [record.card_id]);

  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- Explicit dialog opening starts a remote check; late responses are invalidated on close.
    check();
    return () => { generation.current += 1; };
  }, [check]);

  const stopFirst = !!record.running || !!preview?.running;
  const compatibility = preview?.compatibility;
  const canUpdate = compatibility?.can_update ?? true;
  const canStage = !!compatibility?.can_stage;
  const blocked = !!preview?.changed && !canUpdate && !canStage;
  const valueText = (value) => value === null || value === undefined || value === '' ? copy.versionUnspecified : String(value);
  const actions = (
    <div className="flex flex-wrap justify-end gap-2" data-testid="card-update-actions">
      {onVersions ? <MsButton tone="ghost" onClick={() => { onClose(); onVersions(record); }}>{copy.versionHistory}</MsButton> : null}
      {blocked ? <MsButton tone="neutral" disabled={busy || checking} onClick={check}>{copy.cardUpdateCheck}</MsButton> : null}
      <MsButton tone="ghost" onClick={onClose}>{copy.detailClose}</MsButton>
      {preview?.changed && canStage ? <MsButton tone="primary" disabled={busy || checking} data-testid="model-store-stage-version" onClick={() => { onStage(record, preview.revision); onClose(); }}>{copy.versionStage}</MsButton> : null}
      {preview?.changed && canUpdate ? (
        <MsButton tone="primary" disabled={busy || stopFirst || checking} data-testid="model-store-apply-card-update" onClick={() => {
          onUpdate(record, preview.revision);
          onClose();
        }}>{copy.cardUpdateApply}</MsButton>
      ) : null}
    </div>
  );
  return (
    <MsDialog open wide onClose={onClose} theme={theme} labelledBy="card-update-title" testId="model-store-card-update-dialog" footer={actions}>
      <MsDialogTitle id="card-update-title" onClose={onClose} closeLabel={copy.detailClose}>
        {copy.cardUpdateTitle(record.card_name)}
      </MsDialogTitle>
      {checking ? <p className={TEXT_SECONDARY} role="status">{copy.cardUpdateChecking}</p> : null}
      {error ? (
        <div role="alert">
          <p className="text-[13px] text-rose-600 dark:text-rose-400">{error}</p>
          <MsButton tone="ghost" onClick={check}>{copy.retry}</MsButton>
        </div>
      ) : null}
      {preview ? (
        <div className="space-y-3 text-[13px]">
          <p role="status">{preview.changed ? copy.cardUpdateAvailable : (record.card_update_pending ? copy.cardUpdatePending : copy.cardUpdateCurrent)}</p>
          {preview.changed ? (
            <>
              <MsRow label={copy.cardUpdateInstalled}>{[preview.current_engine, preview.current_version].filter(Boolean).join(' ')}</MsRow>
              <MsRow label={copy.cardUpdateLatest}>{[preview.next_engine, preview.next_version].filter(Boolean).join(' ')}</MsRow>
              {preview.description ? <p className={`whitespace-pre-wrap leading-6 ${TEXT_SECONDARY}`}>{preview.description}</p> : null}
              {compatibility ? (
                <div className="space-y-3" data-testid="card-update-compatibility">
                  <p role="status">{compatibility.reason === 'unchanged_legacy_identity'
                    ? copy.compatibilityLegacyUnverified
                    : (copy.compatibilityKinds[compatibility.kind] || copy.compatibilityKinds.unverified)}</p>
                  {!canUpdate ? <p className={TEXT_SECONDARY}>{copy.compatibilityReasons[compatibility.reason] || copy.compatibilityKinds[compatibility.reason] || copy.compatibilityReasonUnknown}</p> : null}
                  {canStage ? <p className={TEXT_SECONDARY}>{copy.versionUpgradeNextStep}</p> : null}
                  {blocked ? <p className={TEXT_SECONDARY}>{copy.versionUpgradeBlocked}</p> : null}
                  {compatibility.changes?.map((change) => (
                    <MsRow key={change.field} label={copy.compatibilityFields[change.field] || change.field}>
                      <span className="break-all">{valueText(change.current)} → {valueText(change.next)}</span>
                    </MsRow>
                  ))}
                  {['current_weights', 'next_weights'].map((side) => compatibility[side] ? (
                    <div key={side} className={`text-[11px] ${TEXT_SECONDARY}`}>
                      <p>{side === 'current_weights' ? copy.versionOldWeights : copy.versionNewWeights}</p>
                      {Object.entries(compatibility[side]).map(([path, hash]) => <p key={path} className="break-all"><span>{path}</span><br /><code>{hash}</code></p>)}
                    </div>
                  ) : null)}
                </div>
              ) : null}
              {canUpdate ? <p className={`leading-6 ${TEXT_SECONDARY}`}>{copy.cardUpdateHint}</p> : null}
              {canUpdate && stopFirst ? <p role="status" className="text-amber-700 dark:text-amber-300">{copy.cardUpdateStopFirst}</p> : null}
            </>
          ) : null}
        </div>
      ) : null}
    </MsDialog>
  );
}
