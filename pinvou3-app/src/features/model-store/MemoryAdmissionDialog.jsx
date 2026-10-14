import { formatBytes } from './model-store-logic.js';
import { MsButton, MsDialog, MsDialogTitle, TEXT_PRIMARY, TEXT_SECONDARY } from './model-store-ui.jsx';

export function MemoryAdmissionDialog({ pending, onClose, onConfirm, copy, theme }) {
  const admission = pending?.admission;
  const memorySize = bytes => bytes == null ? copy.memoryUnreadable : formatBytes(bytes);
  return (
    <MsDialog open={!!pending} onClose={onClose} theme={theme}
      labelledBy="model-store-memory-confirm-title" testId="model-store-memory-confirm">
      {admission ? <>
        <MsDialogTitle id="model-store-memory-confirm-title">{copy.memoryTitle}</MsDialogTitle>
        <p className={`text-[13px] leading-6 ${TEXT_PRIMARY}`}>
          {copy.memoryMessages[admission.reason] || copy.memoryMessages.unverified}
        </p>
        {pending.action === 'deploy' ? <p className={`text-[12px] leading-6 ${TEXT_SECONDARY}`}>{copy.memoryDeployHint}</p> : null}
        <div className={`mt-4 rounded-[14px] bg-[#F0F4F9] p-3 text-[12px] leading-6 dark:bg-[#282A2C] ${TEXT_SECONDARY}`}>
          <div>{copy.memoryTotal(memorySize(admission.total_bytes))}</div>
          <div>{copy.memoryAvailable(memorySize(admission.available_bytes))}</div>
          <div>{copy.memoryReserve(memorySize(admission.safety_reserve_bytes))}</div>
          <div>{pending.action === 'deploy' ? copy.memoryDeployCandidate(pending.record.card_name) : copy.memoryCandidate(pending.record.card_name)}</div>
          <div>{admission.candidate_peak_bytes == null ? copy.memoryUnknownPeak
            : copy.memoryPeak(formatBytes(admission.candidate_peak_bytes))}</div>
          {admission.projected_available_bytes != null ?
            <div>{copy.memoryProjected(formatBytes(admission.projected_available_bytes))}</div> : null}
          {(admission.running_models || []).map((model) => (
            <div key={model.card_id}>
              {copy.memoryRunning(model.card_name, model.rss_bytes == null ? copy.memoryUnreadable : formatBytes(model.rss_bytes))}
            </div>
          ))}
          <p className="mt-2">{copy.memoryMeasurementHint}</p>
        </div>
        <div className="mt-5 flex justify-end gap-2">
          <MsButton className="min-w-0 !shrink !px-3 !text-[12px]" tone="ghost" onClick={onClose}>{pending.action === 'deploy' ? copy.memoryCancelDeploy : copy.memoryCancel}</MsButton>
          <MsButton className="min-w-0 !shrink !px-3 !text-[12px]" tone="danger" onClick={onConfirm}>{pending.action === 'deploy' ? copy.memoryContinueDeploy : copy.memoryContinue}</MsButton>
        </div>
      </> : null}
    </MsDialog>
  );
}
