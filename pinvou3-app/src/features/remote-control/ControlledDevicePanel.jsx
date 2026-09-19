// Windows B 被控端远程控制面板（DEV-21）。
//
// 与 Web 分享卡片完全独立：这里只展示本机被控状态、5 分钟短期连接码、
// 已授权工作区、模型来源和只读的审批请求。B 端没有 Allow/Approve 入口，
// 只能查看、停止任务或立即断开；所有文案走三语词条，倒计时通过
// aria-live 节流播报，键盘可达。

import { useEffect, useMemo, useRef, useState } from 'react';
import { RefreshCw, Wrench, X } from '../../components/icons.jsx';
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

/** 进行中的状态：只有这些状态才允许刷新/停止/立即断开。 */
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
  const [endpointProfiles, setEndpointProfiles] = useState([]);
  const [endpointId, setEndpointId] = useState(null);
  const liveRef = useRef(null);

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

  // 随包内置的「服务端点」列表：生产/测试 profile 都自带地址与信任根，
  // 用户不需要配置环境变量即可切换。
  useEffect(() => {
    const remote = bridge && bridge.available && bridge.desktopRemoteControl;
    // 必须按能力探测：bridge 是宿主注入对象，实现可能早于本面板（例如旧宿主、
    // jsdom 冒烟的桩）。直接调用不存在的方法会**同步抛错**并让整个设置页被
    // 错误边界接住（2026-09-20 实测：settings_ui_smoke 的 ① 断言失败）。
    if (!remote || typeof remote.endpointProfiles !== 'function') return undefined;
    let cancelled = false;
    let pending;
    try {
      pending = remote.endpointProfiles();
    } catch (failure) {
      return undefined;
    }
    Promise.resolve(pending)
      .then((profiles) => {
        if (cancelled || !Array.isArray(profiles)) return;
        setEndpointProfiles(profiles);
        const active = profiles.find((profile) => profile && profile.active);
        setEndpointId((current) => current || (active && active.id) || (profiles[0] && profiles[0].id) || null);
      })
      .catch(() => {});
    return () => { cancelled = true; };
  }, []);

  const endpointLabelKey = (id) => `endpoint${String(id || '').charAt(0).toUpperCase()}${String(id || '').slice(1)}`;
  const canSwitchEndpoint = !!(
    bridge
    && bridge.available
    && bridge.desktopRemoteControl
    && typeof bridge.desktopRemoteControl.setEndpointProfile === 'function'
  );

  const run = async (name, action) => {
    if (!bridge || !bridge.available || !bridge.desktopRemoteControl) return;
    setBusy(name);
    setError(null);
    try {
      await action(bridge.desktopRemoteControl);
    } catch (failure) {
      setError((failure && failure.code) || 'error');
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
      <p className="settings-hint">{copy.desc}</p>

      <div className="settings-row">
        <Toggle
          checked={active}
          disabled={busy === 'toggle'}
          aria-label={active ? copy.disable : copy.enable}
          onChange={() => run('toggle', (remote) => (active ? remote.controlledStop() : remote.controlledStart(null, [])))}
        />
        <span className="settings-value" role="status" aria-live="polite" ref={liveRef}>
          {copy[statusKey(status)] || copy.statusIdle}
        </span>
      </div>

      {CODE_STATUSES.includes(status) && device.pairingCode && (
        <div className="settings-row" role="group" aria-labelledby="controlled-device-code">
          <span id="controlled-device-code">{copy.codeLabel}</span>
          <output
            className="pairing-code"
            aria-live="polite"
            aria-label={`${copy.codeLabel} ${formatPairingCode(device.pairingCode)}`}
          >
            {formatPairingCode(device.pairingCode)}
          </output>
          <span className="settings-hint" aria-live="polite">
            {seconds > 0 ? copy.codeExpires(seconds) : copy.codeExpired}
          </span>
          {announce && <span className="sr-only" aria-live="polite">{copy.codeExpires(announce)}</span>}
        </div>
      )}
      <p className="settings-hint">{copy.codeHint}</p>

      <div className="settings-actions">
        <button
          type="button"
          onClick={() => run('refresh', (remote) => remote.controlledRefresh())}
          disabled={!active || busy === 'refresh'}
        >
          <RefreshCw aria-hidden="true" /> {busy === 'refresh' ? copy.refreshing : copy.refresh}
        </button>
        <button
          type="button"
          onClick={() => run('disconnect', (remote) => remote.controlledDisconnectNow())}
          disabled={!active || busy === 'disconnect'}
        >
          <X aria-hidden="true" /> {copy.disconnectNow}
        </button>
        <button
          type="button"
          onClick={() => run('stop-tasks', (remote) => remote.remoteTaskStatus && remote.permissionDenyTask ? Promise.all(tasks.map((task) => remote.cancelRemoteTask(task.taskId))) : Promise.resolve())}
          disabled={tasks.length === 0}
        >
          <Wrench aria-hidden="true" /> {copy.stopTasks}
        </button>
      </div>

      {tasks.length > 0 && (
        <p className="settings-hint" role="status" aria-live="polite">{copy.runningTasks(tasks.length)}</p>
      )}

      <div className="settings-row">
        <span>{copy.deviceLabel}</span>
        <span>{device.controllerDeviceName || copy.deviceUnknown}</span>
      </div>

      <div className="settings-row">
        <span>{copy.modelSource}</span>
        <span>{copy.modelSourceDevice}</span>
      </div>

      {canSwitchEndpoint && endpointProfiles.length > 0 && (
        <div className="settings-row">
          <label htmlFor="controlled-device-endpoint">{copy.endpointLabel}</label>
          <select
            id="controlled-device-endpoint"
            value={endpointId || ''}
            disabled={busy === 'endpoint'}
            onChange={(event) => {
              const next = event.target.value;
              const previous = endpointId;
              setEndpointId(next);
              run('endpoint', async (remote) => {
                try {
                  await remote.setEndpointProfile(next);
                } catch (failure) {
                  // 写入失败时把选择回滚到原值，避免 UI 与持久设置不一致。
                  setEndpointId(previous);
                  throw failure;
                }
              });
            }}
          >
            {endpointProfiles.map((profile) => (
              <option key={profile.id} value={profile.id}>
                {copy[endpointLabelKey(profile.id)] || profile.id}
              </option>
            ))}
          </select>
        </div>
      )}
      {canSwitchEndpoint && endpointProfiles.length > 0 && (
        <p className="settings-hint">{copy.endpointHint}</p>
      )}

      <h4>{copy.workspacesTitle}</h4>
      {workspaces.length === 0 ? (
        <p className="settings-hint">{copy.workspacesEmpty}</p>
      ) : (
        <ul className="settings-list">
          {workspaces.map((workspace) => (
            <li key={workspace.workspaceId}>
              <span>{workspace.displayName}</span>
              <span className="settings-hint">
                {workspace.codeAllowed ? copy.codeAllowed : copy.workspaceUnauthorized}
              </span>
            </li>
          ))}
        </ul>
      )}
      <p className="settings-hint">{copy.workspacesHint}</p>

      <h4>{copy.pendingPermissions}</h4>
      {permissions.length === 0 ? (
        <p className="settings-hint">{copy.pendingPermissionEmpty}</p>
      ) : (
        <ul className="settings-list">
          {permissions.map((permission) => (
            <li key={permission.permissionRequestId}>
              <span>{permission.operationType}</span>
              <span className="settings-hint">{permission.riskLevel} · {permission.targetScope}</span>
              <span className="settings-hint">{copy.noAllowControl}</span>
            </li>
          ))}
        </ul>
      )}
      <p className="settings-hint">{copy.readOnlyNotice}</p>

      {error && <p className="settings-error" role="alert">{copy.errorGeneric}</p>}
    </section>
  );
}

export { statusKey };
