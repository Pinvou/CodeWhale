import { useEffect, useMemo, useRef, useState } from 'react';
import { AlertTriangle, CheckCircle2, Clock, Copy, FolderOpen, Lock, RefreshCw } from '../../components/icons.jsx';
import { Toggle } from '../../components/Toggle.jsx';
import { bridge } from '../../hooks/useBridge.js';
import { copyClipboardText } from '../../shared/clipboard.js';
import { controlledAuthorityKey, controlledDeviceUiState, createAsyncOperationGuard } from './remote-control-logic.mjs';

const COUNTDOWN_ANNOUNCE_EVERY = 30;
const ACTIVE_STATUSES = [
  'reserving', 'waiting_connection', 'safety_handshake', 'connected',
  'reconnecting',
  'refreshing',
];

const STATE_META = {
  off: { key: 'stateOff', tone: 'neutral' },
  reserving: { key: 'stateReserving', tone: 'progress' },
  waiting: { key: 'stateWaiting', tone: 'ready' },
  expiring: { key: 'stateExpiring', tone: 'warning' },
  expired: { key: 'stateExpired', tone: 'danger' },
  reserved: { key: 'stateReserved', tone: 'progress' },
  handshake: { key: 'stateHandshake', tone: 'progress' },
  connected: { key: 'stateConnected', tone: 'success' },
  reconnecting: { key: 'stateReconnecting', tone: 'warning' },
  refreshing: { key: 'stateRefreshing', tone: 'progress' },
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
  const value = String(code || '').replace(/\s+/g, '');
  return value.length === 7 ? `${value.slice(0, 4)} ${value.slice(4)}` : value;
}

export function countdownSeconds(expiresAtMs, nowMs) {
  const expires = Number(expiresAtMs);
  if (!Number.isFinite(expires) || expires <= 0) return 0;
  return Math.max(0, Math.ceil((expires - nowMs) / 1000));
}

function StateChip({ copy, state }) {
  const meta = STATE_META[state] || STATE_META.off;
  const pulsing = ['reserving', 'reserved', 'handshake', 'reconnecting', 'refreshing', 'stopping'].includes(state);
  return (
    <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-[11px] font-semibold ${TONE_CLASSES[meta.tone]}`}>
      <span className={`h-1.5 w-1.5 rounded-full bg-current ${pulsing ? 'animate-pulse' : ''}`} aria-hidden="true" />
      {copy[meta.key]}
    </span>
  );
}

function StatusBanner({ copy, state, errorCode }) {
  const meta = STATE_META[state] || STATE_META.off;
  const Icon = state === 'connected' ? CheckCircle2 : state === 'error' || state === 'expired' ? AlertTriangle : state === 'off' ? Lock : Clock;
  const detailKey = `detail${state.charAt(0).toUpperCase()}${state.slice(1)}`;
  let detail = copy[detailKey];
  if (errorCode === 'desktop_controller_relay_unavailable') detail = copy.errorRelayUnavailable;
  if (errorCode === 'desktop_remote_control_unsupported') detail = copy.unsupported;
  return (
    <div className={`flex gap-3 rounded-xl p-3.5 ${TONE_CLASSES[meta.tone]}`} role={state === 'error' || state === 'expired' ? 'alert' : 'status'} aria-live="polite">
      <Icon size={18} className="mt-0.5 shrink-0" aria-hidden="true" />
      <div className="min-w-0">
        <div className="text-[13px] font-semibold">{copy[meta.key]}</div>
        <div className="mt-0.5 text-[12px] leading-5 opacity-80">{detail}</div>
        {errorCode && errorCode !== 'desktop_remote_control_unsupported' && <div className="mt-1 break-all font-mono text-[10px] opacity-70">{String(errorCode)}</div>}
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
  onLocalStateChange,
}) {
  const copy = (t && t.controlledDevice) || {};
  const device = (bs && bs.controlledDevice) || {};
  const status = device.status || 'idle';
  const [busy, setBusy] = useState(null);
  const [localError, setLocalError] = useState(null);
  const previousStatus = useRef(status);
  const previousAuthority = useRef(controlledAuthorityKey(device));
  const authority = useRef(previousAuthority.current);
  const operationGuard = useRef(createAsyncOperationGuard());
  const busyOperation = useRef(null);
  const [copied, setCopied] = useState(false);
  const seconds = Number.isFinite(secondsRemaining)
    ? secondsRemaining
    : countdownSeconds(device.expiresAtMs, Date.now());
  const uiState = effectiveState || controlledDeviceUiState(device, seconds, localError ? 'error' : null);
  const active = ACTIVE_STATUSES.includes(status) || uiState === 'reserving';
  const codeVisible = ['waiting', 'expiring', 'expired'].includes(uiState);
  const codePending = ['reserving', 'refreshing'].includes(uiState);
  const progress = Math.max(0, Math.min(100, (seconds / 300) * 100));
  const workspaces = Array.isArray(device.workspaces) ? device.workspaces : [];
  const visibleError = localError || (uiState === 'error' ? device.lastError : null);
  const announce = useMemo(() => {
    if (!device.expiresAtMs || seconds <= 0) return null;
    return seconds <= COUNTDOWN_ANNOUNCE_EVERY || seconds % COUNTDOWN_ANNOUNCE_EVERY === 0 ? seconds : null;
  }, [device.expiresAtMs, seconds]);

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
    onLocalStateChange?.(null);
  }, [authorityKey, onLocalStateChange]);

  useEffect(() => {
    const wasWaiting = previousStatus.current === 'waiting_connection';
    let timer;
    if (status === 'safety_handshake' && wasWaiting) {
      onLocalStateChange?.('reserved');
      timer = setTimeout(() => {
        previousStatus.current = status;
        onLocalStateChange?.(null);
      }, 800);
    } else if (status !== 'safety_handshake') {
      previousStatus.current = status;
    }
    return () => clearTimeout(timer);
  }, [onLocalStateChange, status]);

  useEffect(() => {
    const remote = bridge?.available && bridge.desktopRemoteControl;
    if (!remote?.controlledSync) return undefined;
    remote.controlledSync().catch(() => {});
    return undefined;
  }, []);

  const run = async (name, action, pendingState = null) => {
    const remote = bridge?.available && bridge.desktopRemoteControl;
    if (!remote) {
      setLocalError('desktop_remote_control_unsupported');
      onLocalStateChange?.('error');
      return;
    }
    if (busyOperation.current) return;
    busyOperation.current = name;
    const attempt = operationGuard.current.start();
    const startingAuthority = authority.current;
    setBusy(name);
    onLocalStateChange?.(pendingState);
    setLocalError(null);
    let failed = false;
    try {
      await action(remote);
    } catch (failure) {
      if (operationGuard.current.isCurrent(attempt) && authority.current === startingAuthority) {
        failed = true;
        setLocalError(failure?.code || failure?.message || 'desktop_remote_control_failed');
        onLocalStateChange?.('error');
      }
    } finally {
      if (operationGuard.current.isCurrent(attempt) && authority.current === startingAuthority) {
        busyOperation.current = null;
        setBusy(null);
        if (!failed) onLocalStateChange?.(null);
      }
    }
  };

  const copyCode = async () => {
    const ok = await copyClipboardText(device.pairingCode);
    setCopied(ok);
    if (ok) setTimeout(() => setCopied(false), 1800);
  };

  const addWorkspace = async () => {
    await run('workspace', async (remote) => {
      const picker = bridge?.files?.pickFolders;
      if (!picker) throw Object.assign(new Error('folder_picker_unavailable'), { code: 'folder_picker_unavailable' });
      const selected = await picker();
      const paths = (Array.isArray(selected) ? selected : [selected]).filter(Boolean);
      if (paths.length === 0) return;
      await Promise.all(paths.map(path => {
        const displayName = String(path).split(/[\\/]/).filter(Boolean).pop() || String(path);
        return remote.controlledAuthorizeWorkspace({ path, displayName, codeAllowed: true });
      }));
      if (remote.controlledSync) await remote.controlledSync();
    });
  };

  if (!supported) {
    return <StatusBanner copy={copy} state="error" errorCode="desktop_remote_control_unsupported" />;
  }

  return (
    <section data-testid="controlled-device-panel" data-ui-state={uiState} aria-labelledby="controlled-device-title" className="space-y-4">
      <div className="flex items-start justify-between gap-4">
        <div>
          <h3 id="controlled-device-title" className="text-[16px] font-semibold text-[#1C1C1E] dark:text-[#F2F2F7]">{copy.title}</h3>
          <p className="mt-1 max-w-xl text-[12px] leading-5 text-[#6E6E73] dark:text-[#98989D]">{copy.desc}</p>
        </div>
        <div className="flex shrink-0 items-center gap-2">
          <StateChip copy={copy} state={uiState} />
          <Toggle
            checked={active}
            disabled={busy === 'toggle' || ['reserving', 'stopping'].includes(uiState)}
            aria-label={active ? copy.disable : copy.enable}
            onChange={() => run(
              'toggle',
              remote => active ? remote.controlledStop() : remote.controlledStart(null, []),
              active ? 'stopping' : 'reserving',
            )}
          />
        </div>
      </div>

      <div className="rounded-2xl border border-black/[0.08] bg-white p-4 shadow-[0_1px_2px_rgba(0,0,0,0.03)] dark:border-white/[0.1] dark:bg-white/[0.035]">
        <StatusBanner copy={copy} state={uiState} errorCode={visibleError} />

        {codePending && (
          <div className="mt-4 animate-pulse rounded-xl border border-[#007AFF]/10 bg-[#007AFF]/[0.025] p-4" role="status" aria-label={copy[uiState === 'refreshing' ? 'stateRefreshing' : 'stateReserving']}>
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
              <button type="button" onClick={copyCode} disabled={!device.pairingCode || uiState === 'expired'} className="inline-flex h-9 items-center gap-1.5 rounded-lg border border-black/10 px-3 text-[12px] font-medium hover:bg-black/[0.03] disabled:opacity-40 dark:border-white/10 dark:hover:bg-white/[0.06]">
                <Copy size={14} aria-hidden="true" /> {copied ? copy.copied : copy.copyCode}
              </button>
            </div>
            <div className="mt-3 h-1 overflow-hidden rounded-full bg-black/[0.06] dark:bg-white/[0.08]" aria-hidden="true">
              <div className={`h-full rounded-full transition-[width] duration-1000 ${uiState === 'expiring' ? 'bg-[#FF9F0A]' : uiState === 'expired' ? 'bg-[#FF3B30]' : 'bg-[#007AFF]'}`} style={{ width: `${progress}%` }} />
            </div>
            {announce != null && <span className="sr-only" aria-live="polite">{copy.codeExpires(announce)}</span>}
          </div>
        )}

        <div className="mt-4 flex flex-wrap items-center justify-between gap-3 border-t border-black/[0.06] pt-4 dark:border-white/[0.08]">
          <div className="text-[12px] text-[#6E6E73] dark:text-[#98989D]">
            <span>{copy.deviceLabel}: </span>
            <span className="font-medium text-[#1C1C1E] dark:text-[#F2F2F7]">{device.controllerDeviceName || copy.deviceUnknown}</span>
          </div>
          <button type="button" onClick={() => run('refresh', remote => remote.controlledRefresh(), 'refreshing')} disabled={!active || status === 'connected' || ['reserving', 'reserved', 'handshake', 'reconnecting', 'refreshing', 'stopping'].includes(uiState) || busy === 'refresh'} className="inline-flex items-center gap-1.5 rounded-lg px-3 py-2 text-[12px] font-medium text-[#007AFF] hover:bg-[#007AFF]/[0.06] disabled:opacity-40">
            <RefreshCw size={14} className={busy === 'refresh' ? 'animate-spin' : ''} aria-hidden="true" />
            {busy === 'refresh' ? copy.refreshing : copy.regenerate}
          </button>
        </div>
      </div>

      <div className="rounded-2xl border border-black/[0.08] bg-white p-4 dark:border-white/[0.1] dark:bg-white/[0.035]">
        <div className="flex items-center justify-between gap-3">
          <div>
            <h4 className="text-[13px] font-semibold">{copy.workspacesTitle}</h4>
            <p className="mt-1 text-[11px] leading-4 text-[#6E6E73] dark:text-[#98989D]">{copy.workspacesHint}</p>
          </div>
          <button type="button" onClick={addWorkspace} disabled={busy === 'workspace'} className="inline-flex shrink-0 items-center gap-1.5 rounded-lg bg-[#007AFF] px-3 py-2 text-[12px] font-semibold text-white hover:bg-[#006FE6] disabled:opacity-50">
            <FolderOpen size={14} aria-hidden="true" /> {copy.workspaceAuthorize}
          </button>
        </div>
        {workspaces.length === 0 ? (
          <div className="mt-4 rounded-xl bg-black/[0.025] px-3 py-4 text-center text-[12px] text-[#8E8E93] dark:bg-white/[0.04]">{copy.workspacesEmpty}</div>
        ) : (
          <ul className="mt-3 divide-y divide-black/[0.06] dark:divide-white/[0.08]">
            {workspaces.map(workspace => (
              <li key={workspace.workspaceId} className="flex items-center justify-between gap-3 py-2.5 text-[12px]">
                <span className="min-w-0 truncate font-medium">{workspace.displayName}</span>
                <span className="shrink-0 text-[#34C759]">{workspace.codeAllowed ? copy.codeAllowed : copy.workspaceUnauthorized}</span>
              </li>
            ))}
          </ul>
        )}
      </div>

      <div className="flex gap-2 rounded-xl bg-black/[0.025] px-3 py-2.5 text-[11px] leading-4 text-[#6E6E73] dark:bg-white/[0.035] dark:text-[#98989D]">
        <Lock size={14} className="mt-0.5 shrink-0" aria-hidden="true" />
        <span>{copy.securityNotice}</span>
      </div>
    </section>
  );
}
