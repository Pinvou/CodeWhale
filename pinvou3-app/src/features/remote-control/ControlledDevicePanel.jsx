import { useEffect, useMemo, useRef, useState } from 'react';
import { AlertTriangle, CheckCircle2, Clock, Copy, FolderOpen, Lock, RefreshCw } from '../../components/icons.jsx';
import { Toggle } from '../../components/Toggle.jsx';
import { bridge } from '../../hooks/useBridge.js';
import { copyClipboardText } from '../../shared/clipboard.js';
import { CODE_ALPHABET, controlledAuthorityKey, controlledDeviceUiState, createAsyncOperationGuard } from './remote-control-logic.mjs';
import { CONTROLLED_BOUND_STATUSES, countdownAnnouncement, currentControlledTasks, normalizeSelectedWorkspaces, permissionOperationCopyKey, permissionRedactionCopyKey } from './controlled-device-panel-state.mjs';

const ACTIVE_STATUSES = [
  'registering', 'waiting_for_pairing', 'pairing_reserved', 'handshake_pending',
  'connected', 'reconnecting', 'resume_provisional',
  'resume_activated_pending_liveness', 'retiring_generation', 'stopping',
];
const TASK_STATUS_KEYS = {
  admitted: 'taskStatusAdmitted', running: 'taskStatusRunning',
  waiting_permission: 'taskStatusWaitingPermission',
};

const STATE_META = {
  off: { key: 'stateOff', detail: 'detailOff', tone: 'neutral' },
  stopped: { key: 'stateStopped', detail: 'detailStopped', tone: 'neutral' },
  registering: { key: 'stateRegistering', detail: 'detailRegistering', tone: 'progress' },
  waiting_for_pairing: { key: 'stateWaiting', detail: 'detailWaiting', tone: 'ready' },
  expiring: { key: 'stateExpiring', tone: 'warning' },
  expired: { key: 'stateExpired', tone: 'danger' },
  pairing_reserved: { key: 'statePairingReserved', detail: 'detailPairingReserved', tone: 'progress' },
  handshake_pending: { key: 'stateHandshakePending', detail: 'detailHandshakePending', tone: 'progress' },
  connected: { key: 'stateConnected', tone: 'success' },
  reconnecting: { key: 'stateReconnecting', tone: 'warning' },
  resume_provisional: { key: 'stateResumeProvisional', detail: 'detailResumeProvisional', tone: 'progress' },
  resume_activated_pending_liveness: { key: 'stateResumeActivatedPendingLiveness', detail: 'detailResumeActivatedPendingLiveness', tone: 'progress' },
  retiring_generation: { key: 'stateRetiringGeneration', detail: 'detailRetiringGeneration', tone: 'progress' },
  stopping: { key: 'stateStopping', tone: 'neutral' },
  error: { key: 'stateError', tone: 'danger' },
};

const TONE_CLASSES = {
  neutral: 'bg-black/[0.05] text-[#6E6E73] dark:bg-white/[0.08] dark:text-[#AEAEB2]',
  ready: 'bg-[#007AFF]/10 text-[#0066D6] dark:text-[#64A8FF]',
  progress: 'bg-[#5856D6]/10 text-[#4D4BC4] dark:text-[#9B99FF]',
  success: 'bg-[#34C759]/10 text-[#248A3D] dark:text-[#5DD879]',
  warning: 'bg-[#FF9F0A]/12 text-[#A65F00] dark:text-[#FFB340]',
  danger: 'bg-[#FF3B30]/10 text-[#C9342C] dark:text-[#FF6961]',
};

export function formatPairingCode(code) {
  const value = typeof code === 'string' ? code : '';
  return value.length === 7 && [...value].every(character => CODE_ALPHABET.includes(character))
    ? `${value.slice(0, 4)} ${value.slice(4)}` : '';
}

function StateChip({ copy, state }) {
  const meta = STATE_META[state] || STATE_META.off;
  const pulsing = ['registering', 'pairing_reserved', 'handshake_pending', 'reconnecting',
    'resume_provisional', 'resume_activated_pending_liveness', 'retiring_generation', 'stopping'].includes(state);
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-[11px] font-semibold ${TONE_CLASSES[meta.tone]}`}>
      <span className={`h-1.5 w-1.5 rounded-full bg-current ${pulsing ? 'animate-pulse motion-reduce:animate-none' : ''}`} aria-hidden="true" />
      {copy[meta.key]}
    </span>
  );
}

function StatusBanner({ copy, state, errorCode }) {
  const meta = STATE_META[state] || STATE_META.off;
  const Icon = state === 'connected' ? CheckCircle2 : state === 'error' || state === 'expired' ? AlertTriangle : ['off', 'stopped'].includes(state) ? Lock : Clock;
  const detailKey = meta.detail || `detail${state.charAt(0).toUpperCase()}${state.slice(1)}`;
  let detail = copy[detailKey];
  if (errorCode === 'desktop_controller_relay_unavailable') detail = copy.errorRelayUnavailable;
  if (errorCode === 'desktop_remote_control_unsupported') detail = copy.unsupported;
  if (errorCode && !['desktop_controller_relay_unavailable', 'desktop_remote_control_unsupported'].includes(errorCode)) {
    detail = copy.errorGeneric;
  }
  return (
    <div className={`flex gap-3 rounded-xl p-3.5 ${TONE_CLASSES[meta.tone]}`} role={state === 'error' || state === 'expired' ? 'alert' : 'status'} aria-live="polite">
      <Icon size={18} className="mt-0.5 shrink-0" aria-hidden="true" />
      <div className="min-w-0">
        <div className="text-[13px] font-semibold">{copy[meta.key]}</div>
        <div className="mt-0.5 text-[12px] leading-5 opacity-80">{detail}</div>
      </div>
    </div>
  );
}

export default function ControlledDevicePanel({
  t,
  bs,
  supported,
  effectiveState,
  secondsRemaining,
  nowMs,
}) {
  const copy = (t && t.controlledDevice) || {};
  const clockNow = Number.isFinite(nowMs) ? nowMs : Date.now();
  const device = (bs && bs.controlledDevice) || {};
  const status = device.status || 'disabled';
  const [busy, setBusy] = useState(null);
  const [localError, setLocalError] = useState(null);
  const previousAuthority = useRef(controlledAuthorityKey(device));
  const authority = useRef(previousAuthority.current);
  const operationGuard = useRef(createAsyncOperationGuard());
  const busyOperation = useRef(null);
  const [copied, setCopied] = useState(false);
  const headingRef = useRef(null);
  const restoreFocus = useRef(false);
  const [stagedWorkspaces, setStagedWorkspaces] = useState([]);
  const [authorizedWorkspaces, setAuthorizedWorkspaces] = useState([]);
  const [workspaceReadError, setWorkspaceReadError] = useState(false);
  const [permissions, setPermissions] = useState([]);
  const [permissionReadStatus, setPermissionReadStatus] = useState('idle');
  const durationObservation = useRef({ generation: null, duration: null, observedAt: clockNow });
  if (durationObservation.current.generation !== device.generation
    || durationObservation.current.duration !== device.connectedDurationMs) {
    durationObservation.current = {
      generation: device.generation, duration: device.connectedDurationMs, observedAt: clockNow,
    };
  }
  const connectedDurationMs = Number.isSafeInteger(device.connectedDurationMs) && device.connectedDurationMs >= 0
    ? device.connectedDurationMs + Math.max(0, clockNow - durationObservation.current.observedAt) : null;
  const seconds = Number.isSafeInteger(secondsRemaining) && secondsRemaining >= 0 ? secondsRemaining : 0;
  const uiState = effectiveState || controlledDeviceUiState(device, seconds);
  const active = ACTIVE_STATUSES.includes(status);
  const codeVisible = ['waiting_for_pairing', 'expiring'].includes(uiState);
  const codePending = ['registering', 'retiring_generation'].includes(uiState);
  const progress = Math.max(0, Math.min(100, (seconds / 300) * 100));
  const workspaces = active ? authorizedWorkspaces : stagedWorkspaces;
  const currentTasks = currentControlledTasks(device, bs?.controlledSession, bs?.controlledTasks);
  const visibleError = uiState === 'error' ? device.lastError : null;
  const announce = useMemo(() => Number.isSafeInteger(device.pairingRemainingMs) ? countdownAnnouncement(seconds) : null,
    [device.pairingRemainingMs, seconds]);

  const authorityKey = controlledAuthorityKey(device);
  authority.current = authorityKey;

  useEffect(() => {
    const guard = createAsyncOperationGuard();
    operationGuard.current = guard;
    return () => guard.dispose();
  }, []);

  useEffect(() => {
    if (previousAuthority.current === authorityKey) return;
    previousAuthority.current = authorityKey;
    operationGuard.current.invalidate();
    busyOperation.current = null;
    setBusy(null);
    setLocalError(null);
    setCopied(false);
    if (restoreFocus.current) {
      restoreFocus.current = false;
      headingRef.current?.focus();
    }
  }, [authorityKey]);

  useEffect(() => {
    const remote = bridge?.available && bridge.desktopRemoteControl;
    if (!remote?.controlledSync) return undefined;
    let mounted = true;
    remote.controlledSync().catch(failure => {
      if (mounted) setLocalError(failure?.code || failure?.message || 'desktop_remote_control_failed');
    });
    return () => { mounted = false; };
  }, []);

  useEffect(() => {
    const remote = bridge?.available && bridge.desktopRemoteControl;
    if (!active || !device.generation || !remote?.controlledListWorkspaces) {
      setAuthorizedWorkspaces([]);
      setWorkspaceReadError(active && !!device.generation && !remote?.controlledListWorkspaces);
      return undefined;
    }
    let cancelled = false;
    const startingAuthority = authority.current;
    setAuthorizedWorkspaces([]);
    setWorkspaceReadError(false);
    remote.controlledListWorkspaces().then(list => {
      if (!cancelled && authority.current === startingAuthority) {
        if (!Array.isArray(list)) {
          setWorkspaceReadError(true);
          return;
        }
        setAuthorizedWorkspaces(list.filter(item =>
          item && typeof item.workspaceId === 'string' && typeof item.displayName === 'string'
        ).slice(0, 16).map(item => ({
          workspaceId: item.workspaceId, displayName: item.displayName, codeAllowed: item.codeAllowed === true,
        })));
      }
    }).catch(() => { if (!cancelled && authority.current === startingAuthority) setWorkspaceReadError(true); });
    return () => { cancelled = true; };
  }, [active, device.generation, status]);

  useEffect(() => {
    const remote = bridge?.available && bridge.desktopRemoteControl;
    const waiting = currentTasks.filter(task => task.status === 'waiting_permission');
    if (!remote?.controlledPermissionList || waiting.length === 0) {
      setPermissions([]);
      setPermissionReadStatus(waiting.length > 0 ? 'error' : 'idle');
      return undefined;
    }
    let cancelled = false;
    const startingAuthority = authority.current;
    setPermissions([]);
    setPermissionReadStatus('loading');
    Promise.all(waiting.map(task => remote.controlledPermissionList(task.taskId))).then(results => {
      if (cancelled || authority.current !== startingAuthority) return;
      if (results.some(result => !Array.isArray(result))) {
        setPermissionReadStatus('error');
        return;
      }
      setPermissions(results.flatMap((result, index) => result
        .filter(item => item && typeof item.permissionRequestId === 'string'
          && item.permissionRequestId && item.taskId === waiting[index].taskId && item.state === 'pending')
        .slice(0, 8).map(item => ({
          permissionRequestId: String(item.permissionRequestId || ''),
          taskId: waiting[index].taskId,
          operationTypeCopyKey: permissionOperationCopyKey(item.operationType),
          targetScopeCopyKey: permissionRedactionCopyKey(item.targetScope),
          previewCopyKey: permissionRedactionCopyKey(item.preview),
        }))));
      setPermissionReadStatus('ready');
    }).catch(() => { if (!cancelled && authority.current === startingAuthority) setPermissionReadStatus('error'); });
    return () => { cancelled = true; };
  }, [device.generation, bs?.controlledSession?.sessionId, bs?.controlledTasks]);

  const run = async (name, action, focusOnCompletion = false) => {
    const remote = bridge?.available && bridge.desktopRemoteControl;
    if (!remote) {
      setLocalError('desktop_remote_control_unsupported');
      return;
    }
    if (busyOperation.current) return;
    busyOperation.current = name;
    if (focusOnCompletion) restoreFocus.current = true;
    const attempt = operationGuard.current.start();
    const startingAuthority = authority.current;
    setBusy(name);
    setLocalError(null);
    let failed = false;
    try {
      await action(remote, () => operationGuard.current.isCurrent(attempt)
        && authority.current === startingAuthority);
    } catch (failure) {
      if (operationGuard.current.isCurrent(attempt) && authority.current === startingAuthority) {
        failed = true;
        setLocalError(failure?.code || failure?.message || 'desktop_remote_control_failed');
      }
    } finally {
      if (operationGuard.current.isCurrent(attempt) && authority.current === startingAuthority) {
        busyOperation.current = null;
        setBusy(null);
        if (restoreFocus.current) {
          restoreFocus.current = false;
          headingRef.current?.focus();
        }
      }
    }
  };

  const copyCode = async () => {
    const code = device.pairingCode;
    const startingAuthority = authority.current;
    if (!formatPairingCode(code) || seconds <= 0) return;
    const ok = await copyClipboardText(code);
    if (authority.current !== startingAuthority) return;
    setCopied(ok);
    if (ok) setTimeout(() => setCopied(false), 1800);
  };

  const addWorkspace = async () => {
    await run('workspace', async (_remote, isCurrent) => {
      const picker = bridge?.files?.pickFolders;
      if (!picker) throw Object.assign(new Error('folder_picker_unavailable'), { code: 'folder_picker_unavailable' });
      const selected = await picker();
      if (!isCurrent()) return;
      const next = normalizeSelectedWorkspaces(selected);
      if (next.length) setStagedWorkspaces(current => normalizeSelectedWorkspaces(
        current.map(item => item.path).concat(next.map(item => item.path)),
      ));
    });
  };

  if (!supported) {
    return <StatusBanner copy={copy} state="error" errorCode="desktop_remote_control_unsupported" />;
  }

  return (
    <section data-testid="controlled-device-panel" data-ui-state={uiState} aria-labelledby="controlled-device-title" className="space-y-4 forced-colors:border forced-colors:border-[CanvasText]">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h3 ref={headingRef} tabIndex={-1} id="controlled-device-title" className="text-[16px] font-semibold text-[#1C1C1E] outline-none focus-visible:rounded focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] dark:text-[#F2F2F7]">{copy.title}</h3>
          <p className="mt-1 max-w-xl text-[12px] leading-5 text-[#6E6E73] dark:text-[#98989D]">{copy.desc}</p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <StateChip copy={copy} state={uiState} />
          <Toggle
            data-testid="controlled-device-toggle"
            checked={active}
            disabled={!!busy || ['registering', 'retiring_generation', 'stopping'].includes(uiState)}
            aria-label={active ? copy.disable : copy.enable}
            className="focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] forced-colors:border forced-colors:border-[ButtonText] motion-reduce:transition-none"
            onChange={() => run('toggle', async remote => {
              if (active) return remote.controlledStop();
              const result = await remote.controlledStart(null, stagedWorkspaces);
              setStagedWorkspaces([]);
              return result;
            })}
          />
        </div>
      </div>

      <div className="rounded-2xl border border-black/[0.08] bg-white p-4 shadow-[0_1px_2px_rgba(0,0,0,0.03)] dark:border-white/[0.1] dark:bg-white/[0.035]">
        <StatusBanner copy={copy} state={uiState} errorCode={visibleError} />
        {localError && <div className="mt-3"><StatusBanner copy={copy} state="error" errorCode={localError} /></div>}

        {codePending && (
          <div className="mt-4 animate-pulse motion-reduce:animate-none rounded-xl border border-[#007AFF]/10 bg-[#007AFF]/[0.025] p-4" role="status" aria-label={copy[(STATE_META[uiState] || STATE_META.error).key]}>
            <div className="flex items-center justify-between gap-3">
              <span className="h-3 w-24 rounded bg-black/[0.08] dark:bg-white/[0.1]" />
              <span className="h-3 w-14 rounded bg-black/[0.06] dark:bg-white/[0.08]" />
            </div>
            <div className="mt-3 h-9 w-48 max-w-full rounded-lg bg-black/[0.08] dark:bg-white/[0.1]" />
            <div className="mt-3 h-1 overflow-hidden rounded-full bg-black/[0.06] dark:bg-white/[0.08]">
              <div className="h-full w-2/5 rounded-full bg-[#007AFF]/60" />
            </div>
          </div>
        )}

        {codeVisible && (
          <div className="mt-4 rounded-xl border border-[#007AFF]/15 bg-[#007AFF]/[0.035] p-4" role="group" aria-labelledby="controlled-device-code-label">
            <div className="flex items-center justify-between gap-3">
              <span id="controlled-device-code-label" className="text-[11px] font-semibold uppercase tracking-[0.08em] text-[#6E6E73] dark:text-[#98989D]">{copy.codeLabel}</span>
              <span className={`text-[11px] font-medium ${uiState === 'expired' ? 'text-[#FF3B30]' : uiState === 'expiring' ? 'text-[#C76A00]' : 'text-[#6E6E73] dark:text-[#98989D]'}`}>
                {seconds > 0 ? copy.codeExpires(seconds) : copy.codeExpiredShort}
              </span>
            </div>
            <div className="mt-2 flex items-center justify-between gap-3">
              <output className={`font-mono text-[28px] font-semibold tracking-[0.16em] ${uiState === 'expired' ? 'opacity-35' : ''}`} aria-live="polite">
                {formatPairingCode(device.pairingCode) || '---- ---'}
              </output>
              <button type="button" onClick={copyCode} disabled={!formatPairingCode(device.pairingCode) || uiState === 'expired'} className="inline-flex h-9 items-center gap-1.5 rounded-lg border border-black/10 px-3 text-[12px] font-medium hover:bg-black/[0.03] disabled:opacity-40 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] forced-colors:border-[ButtonText] dark:border-white/10 dark:hover:bg-white/[0.06]">
                <Copy size={14} aria-hidden="true" /> {copied ? copy.copied : copy.copyCode}
              </button>
            </div>
            <div className="mt-3 h-1 overflow-hidden rounded-full bg-black/[0.06] dark:bg-white/[0.08]" aria-hidden="true">
              <div className={`h-full rounded-full transition-[width] duration-1000 motion-reduce:transition-none ${uiState === 'expiring' ? 'bg-[#FF9F0A]' : uiState === 'expired' ? 'bg-[#FF3B30]' : 'bg-[#007AFF]'}`} style={{ width: `${progress}%` }} />
            </div>
            {announce != null && <span className="sr-only" aria-live="polite">{copy.codeExpires(announce)}</span>}
          </div>
        )}

        <div className="mt-4 flex flex-wrap items-center justify-between gap-3 border-t border-black/[0.06] pt-4 dark:border-white/[0.08]">
          {CONTROLLED_BOUND_STATUSES.has(status) && <div className="text-[12px] text-[#6E6E73] dark:text-[#98989D]">
            <span>{copy.deviceLabel}: </span>
            <span className="font-medium text-[#1C1C1E] dark:text-[#F2F2F7]">{device.controllerDeviceName || copy.deviceUnknown}</span>
          </div>}
          <button type="button" data-testid="controlled-device-refresh" onClick={() => run('refresh', remote => remote.controlledRefresh(), true)} disabled={!active || ['retiring_generation', 'stopping'].includes(uiState) || !!busy} className="inline-flex items-center gap-1.5 rounded-lg px-3 py-2 text-[12px] font-medium text-[#007AFF] hover:bg-[#007AFF]/[0.06] disabled:opacity-40 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] forced-colors:border forced-colors:border-[ButtonText]">
            <RefreshCw size={14} className={busy === 'refresh' ? 'animate-spin motion-reduce:animate-none' : ''} aria-hidden="true" />
            {busy === 'refresh' ? copy.refreshing : copy.regenerate}
          </button>
        </div>
        {CONTROLLED_BOUND_STATUSES.has(status) && (
          <div className="mt-3 flex flex-wrap items-center justify-between gap-3 border-t border-black/[0.06] pt-3 text-[12px] dark:border-white/[0.08]">
            <span>{connectedDurationMs != null
              ? copy.connectedDuration(connectedDurationMs) : copy.connectedTimeUnknown}</span>
            <button type="button" onClick={() => run('disconnect', remote => remote.controlledDisconnectNow(), true)} disabled={!!busy}
              className="rounded-lg border border-red-500 px-3 py-2 font-semibold text-red-700 hover:bg-red-500/10 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-red-600 forced-colors:border-[ButtonText] forced-colors:text-[ButtonText] dark:text-red-300">
              {copy.disconnectNow}
            </button>
          </div>
        )}
        {uiState === 'expired' && <p role="status" className="mt-4 text-[12px]">{copy.codeExpired}</p>}
      </div>

      <div className="rounded-2xl border border-black/[0.08] bg-white p-4 dark:border-white/[0.1] dark:bg-white/[0.035]">
        <div className="flex items-center justify-between gap-3">
          <div>
            <h4 className="text-[13px] font-semibold">{copy.workspacesTitle}</h4>
            <p className="mt-1 text-[11px] leading-4 text-[#6E6E73] dark:text-[#98989D]">{active ? copy.workspacesHint : copy.workspaceStageHint}</p>
          </div>
          {!active && <button type="button" onClick={addWorkspace} disabled={!!busy}
            className="inline-flex shrink-0 items-center gap-1.5 rounded-lg bg-[#007AFF] px-3 py-2 text-[12px] font-semibold text-white hover:bg-[#006FE6] disabled:opacity-50 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-[#007AFF] forced-colors:border forced-colors:border-[ButtonText] forced-colors:text-[ButtonText]">
            <FolderOpen size={14} aria-hidden="true" /> {copy.workspaceAuthorize}
          </button>}
        </div>
        {active && workspaceReadError && <p role="alert" className="mt-3 text-[12px]">{copy.workspaceReadFailed}</p>}
        {!workspaceReadError && workspaces.length === 0 ? (
          <div className="mt-4 rounded-xl bg-black/[0.025] px-3 py-4 text-center text-[12px] text-[#6E6E73] dark:bg-white/[0.04] dark:text-[#AEAEB2]">{copy.workspacesEmpty}</div>
        ) : workspaces.length > 0 ? (
          <ul className="mt-3 divide-y divide-black/[0.06] dark:divide-white/[0.08]">
            {workspaces.map(workspace => (
              <li key={workspace.workspaceId || workspace.path} className="flex items-center justify-between gap-3 py-2.5 text-[12px]">
                <span className="min-w-0 truncate font-medium">{workspace.displayName}</span>
                <span className="shrink-0">{active
                  ? (workspace.codeAllowed ? copy.codeAllowed : copy.workspaceUnauthorized)
                  : copy.workspaceStaged}</span>
              </li>
            ))}
          </ul>
        ) : null}
      </div>

      {currentTasks.length > 0 && (
        <section aria-labelledby="controlled-device-tasks-title" className="rounded-2xl border border-black/[0.08] bg-white p-4 dark:border-white/[0.1] dark:bg-white/[0.035]">
          <h4 id="controlled-device-tasks-title" className="text-[13px] font-semibold">{copy.runningTasks(currentTasks.length)}</h4>
          <ul className="mt-3 space-y-3">
            {currentTasks.map(task => (
              <li key={task.taskId} className="rounded-xl border border-black/[0.1] p-3 text-[12px] forced-colors:border-[CanvasText] dark:border-white/[0.12]">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <span className="font-medium">{copy[TASK_STATUS_KEYS[task.status]]}</span>
                  <span>{copy.modelSource}: {task.modelSource === 'controlled_device'
                    ? copy.modelSourceDevice : task.modelSource === 'controller_temporary'
                      ? copy.modelSourceController : copy.modelSourceUnknown}</span>
                </div>
                <div className="mt-2 flex flex-wrap items-center justify-between gap-2">
                  <span className="font-mono text-[11px]">{task.taskId}</span>
                  <button type="button" aria-label={copy.stopTaskForTask(task.taskId)} onClick={() => run('task', remote => remote.controlledStopTask(task.taskId), true)} disabled={!!busy}
                    className="rounded-lg border border-red-500 px-3 py-1.5 font-semibold text-red-700 focus-visible:outline focus-visible:outline-2 focus-visible:outline-offset-2 focus-visible:outline-red-600 forced-colors:border-[ButtonText] forced-colors:text-[ButtonText] dark:text-red-300">
                    {copy.stopTask}
                  </button>
                </div>
              </li>
            ))}
          </ul>
          <div className="mt-3 rounded-xl bg-black/[0.03] p-3 text-[12px] dark:bg-white/[0.04]">
            <h5 className="font-semibold">{copy.permissionDetails}</h5>
            {permissionReadStatus === 'loading' ? <p className="mt-1">{copy.permissionLoading}</p>
              : permissionReadStatus === 'error' ? <p role="alert" className="mt-1">{copy.permissionReadFailed}</p>
                : permissions.length === 0 ? <p className="mt-1">{copy.pendingPermissionEmpty}</p> : (
              <ul className="mt-2 space-y-2">
                {permissions.map(permission => (
                  <li key={permission.permissionRequestId} className="border-l-2 border-current pl-2">
                    <span className="font-medium"><span className="sr-only">{copy.permissionOperationLabel}: </span>{copy[permission.operationTypeCopyKey]}</span>
                    {permission.targetScopeCopyKey && <span className="ml-2"><span className="sr-only">{copy.permissionTargetScopeLabel}: </span>{copy[permission.targetScopeCopyKey]}</span>}
                    {permission.previewCopyKey && <p className="mt-1"><span className="sr-only">{copy.permissionPreviewLabel}: </span>{copy[permission.previewCopyKey]}</p>}
                  </li>
                ))}
              </ul>
            )}
            <p className="mt-2">{copy.noAllowControl}</p>
          </div>
        </section>
      )}

      <div className="flex gap-2 rounded-xl bg-black/[0.025] px-3 py-2.5 text-[11px] leading-4 text-[#6E6E73] dark:bg-white/[0.035] dark:text-[#98989D]">
        <Lock size={14} className="mt-0.5 shrink-0" aria-hidden="true" />
        <span>{copy.securityNotice}</span>
      </div>
    </section>
  );
}
