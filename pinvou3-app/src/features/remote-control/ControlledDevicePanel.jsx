// Windows B 被控端远程控制面板（DEV-21）。
//
// 与 Web 分享卡片完全独立：这里只展示本机被控状态、5 分钟短期连接码、
// 已授权工作区、模型来源和只读的审批请求。B 端没有 Allow/Approve 入口，
// 只能查看、停止任务或用开关关闭远程控制；所有文案走三语词条，倒计时通过
// aria-live 节流播报，键盘可达。

import { useEffect, useMemo, useState } from 'react';
import { RefreshCw, Wrench } from '../../components/icons.jsx';
import { Toggle } from '../../components/Toggle.jsx';
import { bridge } from '../../hooks/useBridge.js';

const CODE_TICK_MS = 1000;
const COUNTDOWN_ANNOUNCE_EVERY = 30;

export function formatPairingCode(code) {
  const digits = String(code || '').replace(/\s+/g, '');
  if (digits.length !== 7) return digits;
  return digits.slice(0, 4) + " " + digits.slice(4);
}

export function countdownSeconds(expiresAtMs, nowMs) {
  const expires = Number(expiresAtMs);
  if (!isFinite(expires) || expires <= 0) return 0;
  return Math.max(0, Math.ceil((expires - nowMs) / 1000));
}

function statusKey(status) {
  switch (status) {
    // 后端 ControlledStatusKind（snake_case）与 DEV-21 早期拼写的兼容映射。
    case 'disabled':
    case 'idle': return 'statusIdle';
    case 'reserving':
    case 'registering': return 'statusRegistering';
    case 'waiting_connection':
    case 'waiting_for_pairing': return 'statusWaiting';
    case 'safety_handshake':
    case 'pairing_reserved': return 'statusReserved';
    case 'handshake_pending': return 'statusHandshake';
    case 'connected': return 'statusConnected';
    case 'reconnecting':
    case 'resume_provisional':
    case 'resume_activated_pending_liveness':
      return 'statusReconnecting';
    // E2E-07：刷新有自己的进行中文案（复用该面板既有的 `refreshing` 词条），
    // 不再借用「正在停止」的语义。
    case 'refreshing': return 'refreshing';
    case 'stopping': return 'statusStopping';
    case 'stopped': return 'statusStopped';
    case 'error': return 'statusError';
    default: return 'statusIdle';
  }
}

/** 进行中的状态：只有这些状态才允许刷新；开关负责开启或关闭被控。 */
const ACTIVE_STATUSES = [
  'reserving',
  'registering',
  'waiting_connection',
  'waiting_for_pairing',
  'safety_handshake',
  'pairing_reserved',
  'handshake_pending',
  'connected',
  'reconnecting',
  'refreshing',
  'stopping',
];

/** 正在展示连接码的状态。 */
const CODE_STATUSES = ['waiting_connection', 'waiting_for_pairing'];

export default function ControlledDevicePanel({ t, bs, supported }) {
  const copy = (t && t.controlledDevice) || {};
  const device = (bs && bs.controlledDevice) || {};
  const status = device.status || 'idle';
  const [now, setNow] = useState(() => Date.now());
  const [busy, setBusy] = useState(null);
  const [error, setError] = useState(null);

  const active = ACTIVE_STATUSES.includes(status);
  const seconds = countdownSeconds(device.expiresAtMs, now);
  const announce = useMemo(() => {
    if (!device.expiresAtMs || seconds <= 0) return null;
    if (seconds > COUNTDOWN_ANNOUNCE_EVERY && seconds % COUNTDOWN_ANNOUNCE_EVERY !== 0) return null;
    return String(seconds);
  }, [device.expiresAtMs, seconds]);

  useEffect(() => {
    if (!device.expiresAtMs) return undefined;
    const timer = setInterval(() => setNow(Date.now()), CODE_TICK_MS);
    return () => clearInterval(timer);
  }, [device.expiresAtMs]);

  // 进入面板时主动同步一次后端状态：事件丢失时（例如只错过了 status 事件）
  // 也能立刻显示真实状态与连接码。dev 模式改完刷新窗口即生效。
  useEffect(() => {
    const remote = bridge && bridge.available && bridge.desktopRemoteControl;
    if (!remote || typeof remote.controlledSync !== 'function') return undefined;
    let cancelled = false;
    Promise.resolve(remote.controlledSync()).catch(() => {
      if (!cancelled) { /* 同步失败不阻塞面板渲染 */ }
    });
    return () => { cancelled = true; };
  }, []);

  // 把前端持有的被控状态写入 Rust 日志文件，支持人员可直接读取定位
  // 「后端已出码、界面无码」这类投影问题（确认问题后随诊断行一起移除）。
  useEffect(() => {
    const remote = bridge && bridge.available && bridge.desktopRemoteControl;
    if (!remote || typeof remote.clientLog !== 'function') return undefined;
    Promise.resolve(remote.clientLog(
      `ui status=${String(status)} code=${device.pairingCode ? 'yes' : 'no'} expires=${String(device.expiresAtMs)} gen=${String(device.generation)}`,
    )).catch(() => {});
    return undefined;
  }, [status, device.pairingCode, device.expiresAtMs, device.generation]);

  const run = async (name, action) => {
    if (!bridge || !bridge.available || !bridge.desktopRemoteControl) return;
    setBusy(name);
    setError(null);
    try {
      await action(bridge.desktopRemoteControl);
    } catch (failure) {
      // 有界码优先；没有 code 时退回 message（至少能看出是哪条命令失败），
      // 最后才是占位符——避免界面只显示 "(error)" 而无从定位。
      setError((failure && (failure.code || failure.message)) || 'error');
    } finally {
      setBusy(null);
    }
  };

  if (!supported) {
    return (
      <section className="settings-section" aria-labelledby="controlled-device-title">
        <h3 id="controlled-device-title">{copy.section}</h3>
        <p className="settings-hint">{copy.unsupported}</p>
      </section>
    );
  }

  const workspaces = Array.isArray(device.workspaces) ? device.workspaces : [];
  const tasks = Array.isArray(bs && bs.tasks) ? bs.tasks.filter((task) => !['succeeded', 'failed', 'cancelled', 'interrupted'].includes(task.status)) : [];
  const permissions = Array.isArray(bs && bs.remotePermissions) ? bs.remotePermissions : [];

  return (
    <section className="settings-section" aria-labelledby="controlled-device-title">
      <h3 id="controlled-device-title">{copy.title}</h3>
      <p className="settings-hint mb-4">{copy.desc}</p>

      <div className="mb-4 rounded-xl border border-black/10 bg-black/[0.02] p-4 dark:border-white/10 dark:bg-white/[0.03]">
        <div className="mb-3 flex items-center justify-between gap-3">
          <div>
            <div className="text-sm font-medium">{copy.connectionTitle}</div>
            <div className="settings-hint">{copy[statusKey(status)] || copy.statusIdle}</div>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-[12px] leading-4 opacity-70">{active ? copy.disable : copy.enable}</span>
            <Toggle
              checked={active}
              disabled={busy === 'toggle'}
              aria-label={active ? copy.disable : copy.enable}
              onChange={() => run('toggle', (remote) => (active ? remote.controlledStop() : remote.controlledStart(null, [])))}
            />
          </div>
        </div>

        {CODE_STATUSES.includes(status) && device.pairingCode && (
          <div className="mb-3 rounded-xl border border-blue-500/20 bg-blue-500/[0.06] p-3" role="group" aria-labelledby="controlled-device-code">
            <div id="controlled-device-code" className="settings-hint">{copy.codeLabel}</div>
            <output className="pairing-code mt-1 block text-2xl font-semibold tracking-[0.18em]" aria-live="polite" aria-label={`${copy.codeLabel} ${formatPairingCode(device.pairingCode)}`}>
              {formatPairingCode(device.pairingCode)}
            </output>
            <div className="settings-hint mt-1" aria-live="polite">{seconds > 0 ? copy.codeExpires(seconds) : copy.codeExpired}</div>
            {announce && <span className="sr-only" aria-live="polite">{copy.codeExpires(announce)}</span>}
          </div>
        )}

        <div className="settings-actions">
          <button className="inline-flex items-center gap-1.5 whitespace-nowrap" type="button" onClick={() => run('refresh', (remote) => remote.controlledRefresh())} disabled={!active || busy === 'refresh'}>
            <RefreshCw aria-hidden="true" /> {busy === 'refresh' ? copy.refreshing : copy.refresh}
          </button>
        </div>
      </div>

      <div className="mb-4 grid gap-3 sm:grid-cols-2">
        <div className="rounded-xl border border-black/10 p-3 dark:border-white/10">
          <div className="settings-hint">{copy.deviceLabel}</div>
          <div className="mt-1 text-sm">{device.controllerDeviceName || copy.deviceUnknown}</div>
        </div>
        <div className="rounded-xl border border-black/10 p-3 dark:border-white/10">
          <div className="settings-hint">{copy.modelSource}</div>
          <div className="mt-1 text-sm">{copy.modelSourceDevice}</div>
        </div>
      </div>

      <div className="mb-4 rounded-xl border border-black/10 p-4 dark:border-white/10">
        <h4 className="mb-2">{copy.workspacesTitle}</h4>
        {workspaces.length === 0 ? (
          <p className="settings-hint">{copy.workspacesEmpty}</p>
        ) : (
          <ul className="settings-list">
            {workspaces.map((workspace) => (
              <li key={workspace.workspaceId}>
                <span>{workspace.displayName}</span>
                <span className="settings-hint">{workspace.codeAllowed ? copy.codeAllowed : copy.workspaceUnauthorized}</span>
              </li>
            ))}
          </ul>
        )}
      </div>

      <div className="mb-4 rounded-xl border border-black/10 p-4 dark:border-white/10">
        <div className="mb-2 flex items-center justify-between gap-3">
          <h4>{copy.pendingPermissions}</h4>
          <span className="settings-hint">{permissions.length}</span>
        </div>
        {permissions.length === 0 ? (
          <p className="settings-hint">{copy.pendingPermissionEmpty}</p>
        ) : (
          <ul className="settings-list">
            {permissions.map((permission) => (
              <li key={permission.permissionRequestId}>
                <span>{permission.operationType}</span>
                <span className="settings-hint">{permission.riskLevel} · {permission.targetScope}</span>
              </li>
            ))}
          </ul>
        )}
      </div>

      {tasks.length > 0 && (
        <div className="mb-4 rounded-xl border border-black/10 p-4 dark:border-white/10">
          <div className="mb-2 flex items-center justify-between gap-3">
            <h4>{copy.runningTasks(tasks.length)}</h4>
            <button type="button" onClick={() => run('stop-tasks', (remote) => Promise.all(tasks.map((task) => remote.cancelRemoteTask(task.taskId))))} disabled={busy === 'stop-tasks'}>
              <Wrench aria-hidden="true" /> {copy.stopTasks}
            </button>
          </div>
        </div>
      )}

      <p className="settings-hint">{copy.readOnlyNotice}</p>

      {(error || (status === 'error' && device.lastError)) && (
        <p className="settings-error" role="alert">
          {copy.errorGeneric}
          {/* 有界错误码直接透出：面板只显示通用文案时，用户与支持两端都无法定位 */}
          {(error || device.lastError) && (
            <span className="settings-error-code"> ({String(error || device.lastError)})</span>
          )}
        </p>
      )}
    </section>
  );
}

export { statusKey };
