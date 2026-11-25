import React from 'react';
import { useModelGuidance } from './use-model-guidance.js';
import { guidanceConnectionRequest, guidanceSelectedModel, guidanceSelectionKey, selectedLocalModelConnection } from './model-guidance-connection.js';
import { guidanceReminderCanMute } from './model-guidance-mute.js';
const EMPTY_MODELS = Object.freeze([]);

export function ModelGuidanceView({ copy, snapshot, busy = false, error = '', onStart, onRefresh, onDismiss, onMute, onMuteInstall, onOpenModelStore, onOpenSettings }) {
  const phase = snapshot.phase;
  if (['checking', 'hidden', 'available'].includes(phase)) return null;
  const mute = phase === 'installCms' ? onMuteInstall : guidanceReminderCanMute(phase) ? onMute : null;
  const muteHint = phase === 'installCms' ? copy.installMuteHint : copy.muteHint;
  const knownError = ['guidance_mute_failed', 'guidance_unconfirmed', 'guidance_default_changed', 'guidance_ownership_required', 'guidance_result_unconfirmed', 'guidance_not_ready', 'guidance_files_missing']
    .find(code => error === code || error.startsWith(`Error: ${code}`)) || (error.startsWith('Error: [unknown_card]') ? 'unknown_card' : null);
  const primary = 'rounded-lg bg-blue-600 px-3 py-2 text-sm text-white disabled:opacity-50 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-blue-500';
  const secondary = 'rounded-lg px-3 py-2 text-sm hover:bg-black/5 dark:hover:bg-white/10 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-blue-500';
  return <section className="rounded-xl border border-blue-500/20 border-l-4 border-l-blue-500 bg-blue-50 px-4 py-3 text-[#1C1C1E] dark:bg-[#1c2638] dark:text-[#F2F2F7]" data-testid="model-guidance" data-mode="banner" aria-label={copy.titles[phase]} aria-busy={busy || phase === 'starting'}>
    <div className="flex flex-wrap items-center justify-between gap-x-4 gap-y-2">
      <div className="min-w-0 flex-1 basis-64">
        <h3 className="text-sm font-semibold">{copy.titles[phase]}</h3>
        <p className="mt-1 text-xs opacity-75">{snapshot.connectionMismatch ? copy.connectionMismatch : copy.bannerHints[phase]}</p>
        {['connectionUnavailable', 'connectionUnknown'].includes(phase) && snapshot.available?.length > 0 && <p className="mt-1 text-xs opacity-75">{copy.runningServiceHint}</p>}
      </div>
      <div className="flex min-w-0 max-w-full flex-wrap items-center gap-1">
        {phase === 'installCms' && onOpenSettings && <button type="button" className={primary} onClick={onOpenSettings} data-testid="guidance-install-cmsd">{copy.installCms}</button>}
        {onOpenModelStore && <button type="button" className={phase === 'installCms' ? secondary : primary} onClick={onOpenModelStore}>{copy.openStore}</button>}
        {!onOpenModelStore && onOpenSettings && <button type="button" className={primary} onClick={onOpenSettings}>{copy.openDevice}</button>}
        <button type="button" className={secondary} onClick={onDismiss} disabled={busy}>{copy.dismiss}</button>
        {mute && <button type="button" className={secondary} onClick={mute} disabled={busy} title={muteHint} data-testid="guidance-mute">{copy.mute}</button>}
      </div>
    </div>
    {error && <p role="alert" className="mt-2 break-words text-sm text-red-700 dark:text-red-300">{knownError ? copy.errors[knownError] : `${copy.failed} ${error}`}</p>}
    {phase === 'starting' && <p role="status" className="mt-2 text-sm">{copy.background}</p>}
    <details className="mt-2 text-sm">
      <summary className="w-fit cursor-pointer rounded py-1 text-xs opacity-75 focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-blue-500">{copy.moreOptions}</summary>
      <p className="mt-2 opacity-80">{copy.descriptions[phase]}</p>
      {mute && <p className="mt-2 text-xs opacity-70">{muteHint}</p>}
      {snapshot.selectedModelName && <p className="mt-2 break-words">{copy.selectedModel(snapshot.selectedModelName)}</p>}
      {snapshot.modelName && <p className="mt-2 break-words">{snapshot.historicalDefault ? copy.historicalDefaultModel(snapshot.modelName) : copy.defaultModel(snapshot.modelName)}</p>}
      {phase === 'recover' && <p className="mt-2 text-xs opacity-70">{copy.attemptHint}</p>}
      {snapshot.ownershipRequired && <p className="mt-2">{copy.ownershipHint}</p>}
      {snapshot.missingDefault && <p className="mt-2">{copy.missingHint}</p>}
      {phase === 'recover' && !snapshot.card && <p className="mt-2">{copy.noDefault}</p>}
      {snapshot.historyKnown === false && <p className="mt-2 text-xs opacity-70">{copy.historyUnavailable}</p>}
      <div className="mt-2 flex flex-wrap gap-1">
        {snapshot.canTry && <button type="button" className={secondary} onClick={onStart} disabled={busy}>{copy.tryDefault}</button>}
        {onOpenSettings && <button type="button" className={secondary} onClick={onOpenSettings}>{copy.openDevice}</button>}
        <button type="button" className={secondary} onClick={onRefresh} disabled={busy}>{copy.refresh}</button>
      </div>
    </details>
  </section>;
}

export function ModelGuidancePanel({ copy, device, modelSelection, savedModels = EMPTY_MODELS, existingUse = null, onOpenModelStore, onOpenSettings }) {
  const selectedModel = guidanceSelectedModel({ ...modelSelection, savedModels });
  // Cloud selections never mount the local-service polling hook. A selection
  // change also retires any in-flight result from the previous local model.
  if (!guidanceConnectionRequest(selectedModel)) return null;
  return <SelectedLocalModelGuidance key={guidanceSelectionKey(selectedModel)} copy={copy} device={device}
    selectedModel={selectedModel} savedModels={savedModels} existingUse={existingUse}
    onOpenModelStore={onOpenModelStore} onOpenSettings={onOpenSettings} />;
}

function SelectedLocalModelGuidance({ copy, device, selectedModel, savedModels, existingUse, onOpenModelStore, onOpenSettings }) {
  const guide = useModelGuidance(device.isCube, selectedModel, savedModels, existingUse);
  const snapshot = selectedLocalModelConnection(guide.snapshot, selectedModel);
  const error = guide.error;
  const silenced = guide.muted && !error;
  const visible = snapshot.phase === 'connectionUnavailable' && !guide.dismissed && !silenced;
  if (!visible) return null;
  const open = callback => callback && (() => { guide.dismiss(); callback(); });
  // A failed chat endpoint does not authorize starting a device-wide default.
  return <div className="px-4 pt-3"><ModelGuidanceView copy={copy} {...guide} error={error} snapshot={{ ...snapshot, canTry: false, modelName: null }}
    onRefresh={guide.refresh} onDismiss={() => guide.dismiss()} onMute={guide.muteUntilAvailable}
    onOpenModelStore={open(onOpenModelStore)} onOpenSettings={open(onOpenSettings)} /></div>;
}
