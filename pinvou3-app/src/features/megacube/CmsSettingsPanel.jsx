import React, { useCallback, useEffect, useId, useRef, useState } from 'react';
import './cms-panel.css';
import { can, onPlatformConnectionChange } from '../../shared/platform.js';
import { invokeTauri } from '../../platform/tauri/client.js';
import { CmsNotice } from './CmsNotice.jsx';
import { cmsNotice, cmsNoticeObservations, engineIssueNotice, hasCmsTaskIssue, isFailedCmsTask, modelIssueNotice, noticeReadKey, splitCmsTasks, taskIssueNotice } from './cms-notice-read.js';
import { useCmsNoticeRead } from './use-cms-notice-read.js';
import { CmsDaemonPanel } from './CmsDaemonPanel.jsx';
import { StartupControl } from './StartupControl.jsx';
import { CmsOverview } from './CmsOverview.jsx';
import { CmsEngineRow } from './CmsEngineRow.jsx';
import { ClipboardList, Layers, RefreshCw, Store, Wrench } from '../../components/icons.jsx';
import { StatusDot } from '../../components/StatusDot.jsx';
import { formatBytes } from '../../shared/format-number.js';
import {
  fetchCmsCapacity,
  fetchCmsEngines,
  fetchCmsModels,
  fetchCmsStatus,
  fetchCmsTasks,
  actOnCmsModel,
  cancelCmsTask,
} from './cms-api.js';
import {
  formatGiB,
  normalizeCapacity,
  normalizeEngines,
  normalizeModels,
  normalizeStatus,
} from './cms-view-model.js';

/** @param {{ title?: string, hint?: string, icon?: any, children?: any }} props */
const Section = ({ title, hint, icon: Icon, children }) => (
  <section className="cms-card">
    {title && <div className="cms-section-heading"><h3 className="cms-heading">{Icon && <span className="cms-icon-tile" aria-hidden="true"><Icon size={18} /></span>}{title}</h3>{hint && <p className="cms-muted mt-2 text-xs">{hint}</p>}</div>}
    {children}
  </section>
);

/** @param {{ label: any, children?: any }} props */
const Row = ({ label, children }) => (
  <div className="cms-row">
    <div className="min-w-0 text-sm text-black/60 dark:text-white/60">{label}</div>
    <div className="min-w-0 break-words text-sm">{children}</div>
  </div>
);

const detailText = (value) => (typeof value === 'string' ? value : JSON.stringify(value));

function TaskRow({ task, copy, busy, onCancel, read, onRead, modelName }) {
  const failed = isFailedCmsTask(task);
  const hasIssue = hasCmsTaskIssue(task);
  const error = detailText(task.error || task.last_event?.message || task.last_line || copy.taskFailedTitle);
  return (
    <article className="cms-task" data-task-id={task.id || task.run_id} data-task-source={task.source || 'cmsd'}>
      <div className="flex flex-wrap items-center gap-2">
        <span className="min-w-0 break-words font-medium">{modelName}</span>
        <span className="text-sm text-black/60 dark:text-white/60">
          {copy.taskKinds[task.kind] || task.kind}
        </span>
        <Tag
          tone={
            failed
              ? 'red'
              : ['running', 'queued', 'cancelling'].includes(task.status)
                ? 'blue'
                : 'gray'
          }
        >
          {copy.taskStatus[task.status] || task.status}
        </Tag>
        {failed && read && <Tag tone="gray">{copy.taskRead}</Tag>}
      </div>
      {hasIssue && !read && <CmsNotice copy={copy} onRead={onRead} className="cms-error mt-2"
        data-testid="cms-task-issue">{error}</CmsNotice>}
      {!hasIssue &&
        !['succeeded', 'cancelled'].includes(task.status) &&
        (task.last_event?.message || task.last_line) && (
          <p className="mt-2 break-words text-sm">
            {detailText(task.last_event?.message || task.last_line)}
          </p>
        )}
      {typeof task.progress_done_bytes === 'number' && (
        <p className="mt-2 text-sm">
          {formatBytes(task.progress_done_bytes, { missing: '—' })} /{' '}
          {formatBytes(task.progress_total_bytes, { missing: '—' })}
          {task.progress_total_bytes > 0
            ? ` (${Math.min(100, Math.round((task.progress_done_bytes * 100) / task.progress_total_bytes))}%)`
            : ''}
        </p>
      )}
      {task.status === 'running' &&
        ['install', 'download', 'install_local'].includes(task.kind) && (
          <button type="button" disabled={!!busy} className="cms-danger mt-3" onClick={onCancel}>
            {copy.cancelTask}
          </button>
        )}
      <details className="cms-details mt-2">
        <summary>{copy.taskDetails}</summary>
        <p className="break-all font-mono text-xs">{task.id || task.run_id}</p>
        {(task.card || task.card_id) && <p className="mt-2 break-all text-xs">{copy.modelCard} · {task.card || task.card_id}</p>}
        {hasIssue && <p className="cms-error mt-2 whitespace-pre-wrap">{error}</p>}
        {task.result && (
          <p className="mt-2 whitespace-pre-wrap break-words text-sm">{detailText(task.result)}</p>
        )}
      </details>
    </article>
  );
}

const TAG_TONES = {
  green: 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-400',
  gray: 'bg-black/[0.06] text-[#6C6C70] dark:bg-white/[0.08] dark:text-[#8E8E93]',
  amber: 'bg-amber-500/12 text-amber-800 dark:text-amber-400',
  red: 'bg-red-500/12 text-red-700 dark:text-red-400',
  blue: 'bg-blue-500/12 text-blue-700 dark:text-blue-400',
};

/** @param {{ tone?: keyof typeof TAG_TONES, children?: any }} props */
const Tag = ({ tone = 'gray', children }) => (
  <span
    className={`inline-flex max-w-full items-center text-xs px-2 py-1 rounded-md ${TAG_TONES[tone] || TAG_TONES.gray}`}
  >
    {children}
  </span>
);

/** @type {Record<string, { tone: keyof typeof TAG_TONES, dot: 'idle'|'run'|'fail'|'ok'|'okPulse'|'warn' }>} */
const STATUS_META = {
  running: { tone: 'green', dot: 'ok' },
  starting: { tone: 'amber', dot: 'run' },
  degraded: { tone: 'red', dot: 'fail' },
  stopped: { tone: 'gray', dot: 'idle' },
};

/**
 * MegaCube 设备管理面板：服务、模型、恢复操作与资源状态。
 * 由侧边栏运行状态/设备管理页面承载，查询失败保留恢复入口。
 * @param {{ copy: Record<string, any>, onOpenModelStore?: () => void }} props - copy = t.uiMegacube 三语文案。
 */
export function CmsSettingsPanel({ copy, onOpenModelStore }) {
  const [status, setStatus] = useState(
    /** @type {import('./cms-view-model.js').StatusView | null} */ (null),
  );
  const [models, setModels] = useState(
    /** @type {import('./cms-view-model.js').ModelsView | null} */ (null),
  );
  const [engines, setEngines] = useState(
    /** @type {import('./cms-view-model.js').EngineRow[] | null} */ (null),
  );
  const [capacity, setCapacity] = useState(
    /** @type {import('./cms-view-model.js').CapacityView | null} */ (null),
  );
  const [tasks, setTasks] = useState(/** @type {Array<Record<string, any>>|null} */ (null));
  const [operationError, setOperationError] = useState('');
  const [partialError, setPartialError] = useState('');
  const [busy, setBusy] = useState('');
  const [confirmStop, setConfirmStop] = useState('');
  const [showInactiveEngines, setShowInactiveEngines] = useState(false);
  const [showAllTasks, setShowAllTasks] = useState(false);
  const [loading, setLoading] = useState(true);
  const [unavailableReason, setUnavailableReason] = useState('');
  const [lastRefresh, setLastRefresh] = useState(/** @type {Date | null} */ (null));
  const tokenRef = useRef(0);
  const confirmId = useId();
  const stopConfirm = useRef(null),
    stopReturn = useRef(null);
  const { observe: observeNotices, dismiss: readNotice, reset: resetNotices, isRead, saveFailed } = useCmsNoticeRead();
  const noticeScope = useRef(null);
  useEffect(() => {
    if (!confirmStop) {
      stopReturn.current?.focus();
      return;
    }
    stopConfirm.current?.focus();
    const escape = (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        setConfirmStop('');
      }
    };
    document.addEventListener('keydown', escape);
    return () => document.removeEventListener('keydown', escape);
  }, [confirmStop]);
  const inFlight = useRef(false);

  const loadFinished = useRef(Promise.resolve());
  const loadAll = useCallback(async (options) => {
    if (inFlight.current) {
      await loadFinished.current;
      // A poll begun before a write cannot confirm the new preference.
      if (options?.fresh === true) return loadAll(options);
      return;
    }
    inFlight.current = true;
    let finish;
    loadFinished.current = new Promise(resolve => { finish = resolve; });
    // 序号闸：慢响应回来时若已有更新的请求，直接丢弃（模型商店同款防陈旧）
    const token = ++tokenRef.current;
    const stale = () => token !== tokenRef.current;
    setLoading(true);
    try {
      // 分区独立容错：某一区失败（如老 cmsd 缺端点）不拖垮整面板
      const [statusRes, modelsRes, enginesRes, capacityRes, tasksRes, serviceRes] = await Promise.allSettled([
        fetchCmsStatus(),
        fetchCmsModels(),
        fetchCmsEngines(),
        fetchCmsCapacity(),
        fetchCmsTasks(),
        invokeTauri('cms_service_status'),
      ]);
      if (stale()) return;
      /** @param {PromiseSettledResult<any>} r */
      const unwrap = (r) => (r.status === 'fulfilled' ? r.value : null);
      const statusValue = unwrap(statusRes);
      const scope = unwrap(serviceRes)?.recoveryScope;
      if (noticeReadKey(scope)) noticeScope.current = scope;
      if (!statusValue?.available) {
        const reason = statusValue?.reason || 'unavailable';
        setUnavailableReason(reason);
        setStatus(null);
        observeNotices(noticeScope.current, cmsNoticeObservations({ unavailable: reason }));
        return;
      }
      const nextStatus = normalizeStatus(statusValue.data);
      const modelsKnown = unwrap(modelsRes)?.available && Array.isArray(unwrap(modelsRes).data?.models);
      const enginesKnown = unwrap(enginesRes)?.available && Array.isArray(unwrap(enginesRes).data?.engines);
      const nextModels = modelsKnown ? normalizeModels(unwrap(modelsRes).data) : null;
      const nextEngines = enginesKnown ? normalizeEngines(unwrap(enginesRes).data) : null;
      const allTasks = unwrap(tasksRes)?.data?.tasks;
      const tasksKnown = unwrap(tasksRes)?.available && Array.isArray(allTasks);
      const nextTasks = tasksKnown ? allTasks.filter(task => task.source !== 'store'
        || !allTasks.some(parent => parent.source === 'cmsd' && parent.store_run_id === task.id)) : null;
      const partial = [modelsRes, enginesRes, capacityRes, tasksRes]
        .map((r, i) => !(i === 0 ? modelsKnown : i === 1 ? enginesKnown : i === 3 ? tasksKnown : unwrap(r)?.available)
          ? ['models', 'engines', 'capacity', 'tasks'][i] : null)
        .filter(Boolean).join(',');
      observeNotices(noticeScope.current, cmsNoticeObservations({
        status: nextStatus, models: nextModels, engines: nextEngines, tasks: nextTasks, partial,
      }));
      setUnavailableReason('');
      setStatus(nextStatus);
      setModels(nextModels);
      setEngines(nextEngines);
      setCapacity(
        normalizeCapacity(unwrap(capacityRes)?.available ? unwrap(capacityRes).data : undefined),
      );
      setTasks(nextTasks);
      setPartialError(partial);
      setLastRefresh(new Date());
    } catch (error) {
      if (!stale()) setUnavailableReason(String(error));
    } finally {
      inFlight.current = false;
      finish();
      if (!stale()) setLoading(false);
    }
  }, [observeNotices]);
  const refreshAfterChange = useCallback(() => loadAll({ fresh: true }), [loadAll]);

  useEffect(() => {
    loadAll();
    const timer = setInterval(loadAll, 5000);
    const disconnect = onPlatformConnectionChange(() => {
      ++tokenRef.current; noticeScope.current = null; resetNotices();
      setStatus(null); setModels(null); setEngines(null); setTasks(null); setOperationError('');
      void loadAll({ fresh: true });
    });
    return () => {
      clearInterval(timer);
      disconnect();
      ++tokenRef.current;
    };
  }, [loadAll, resetNotices]);

  const act = async (key, job) => {
    setBusy(key);
    setOperationError('');
    try {
      const result = await job();
      if (result?.error) throw new Error(result.error.message || result.error.code);
      setConfirmStop('');
      await loadAll({ fresh: true });
    } catch (error) {
      setOperationError(String(error));
    } finally {
      setBusy('');
    }
  };

  const inactiveEngines = (engines || []).filter(
    (engine) =>
      engine.instances.every((instance) => !instance.alive),
  );
  const visibleEngines = (engines || [])
    .filter((engine) => showInactiveEngines || !inactiveEngines.includes(engine))
    .sort(
      (a, b) => Number(b.instances.some((i) => i.alive)) - Number(a.instances.some((i) => i.alive)),
    );

  /** @param {import('./cms-view-model.js').StatusView['uptime']} uptime */
  const uptimeText = (uptime) => {
    if (!uptime) return '—';
    if (uptime.unit === 'h') return copy.uptimeHours(uptime.value);
    if (uptime.unit === 'm') return copy.uptimeMinutes(uptime.value);
    return copy.uptimeSeconds(uptime.value);
  };

  const activeModel = models?.models.find((model) => model.card === status?.activeModel);
  const { attention: attentionTasks, history: finishedTasks } = splitCmsTasks(tasks, isRead);
  const engineFailures = (engines || []).filter((engine) =>
    engine.instances.some((instance) => instance.alive && instance.healthy === false),
  );
  const renderNotice = (notice, content, className, props = {}) => !isRead(notice) && (
    <CmsNotice key={JSON.stringify([notice.group, notice.id])} copy={copy} onRead={() => readNotice(notice)}
      className={className} {...props}>{content}</CmsNotice>
  );
  const renderTask = (task) => (
    <TaskRow
      key={`${task.source}:${task.id || task.run_id}`}
      task={task}
      modelName={models?.models.find(model => model.card === (task.card || task.card_id))?.modelId || task.card || task.card_id}
      copy={copy}
      busy={busy}
      read={isRead(taskIssueNotice(task))}
      onRead={() => readNotice(taskIssueNotice(task))}
      onCancel={() => act(task.id, () => cancelCmsTask(task.id))}
    />
  );

  const modelRows = [...(models?.models || [])].sort(
    (a, b) =>
      Number(b.card === models?.activeModel) - Number(a.card === models?.activeModel) ||
      Number(b.status === 'running') - Number(a.status === 'running'),
  );
  const stoppedModels = modelRows.filter(
    (model) =>
      model.card !== models?.activeModel && model.status === 'stopped',
  );
  const renderModel = (m) => {
    const meta = STATUS_META[m.status] || STATUS_META.stopped;
    return (
      <article key={m.card} className="cms-model">
        <div className="cms-model-heading">
          <span className="cms-icon-tile" aria-hidden="true"><Layers size={18} /></span>
          <div className="min-w-0 flex-1">
          <h4 className="min-w-0 break-words font-semibold">{m.modelId}</h4>
          <div className="cms-model-meta">
          <StatusDot tone={meta.dot} />
          <Tag tone={meta.tone}>{copy.status[m.status] || m.status}</Tag>
          <Tag tone={m.managed ? 'blue' : 'amber'}>
            {m.managed ? copy.sourceManaged : copy.sourceExternal}
          </Tag>
          {m.bootEnabled && (
            <Tag tone="gray">
              {m.managed && m.recoveryOwner === 'cmsd' ? copy.recoveryManaged : copy.bootEnabled}
            </Tag>
          )}
          {models?.activeModel === m.card && (
            <Tag tone={m.status === 'running' && m.healthy === true ? 'green' : 'gray'}>
              {copy.activeTag}
            </Tag>
          )}
          </div>
          </div>
        </div>
        {m.degradedReason && renderNotice(modelIssueNotice(m.card, m.degradedReason), m.degradedReason,
          'cms-error mt-2', { 'data-testid': `cms-model-issue-${m.card}` })}
        {m.status === 'running' && m.healthy !== true && (
          m.healthy === false ? renderNotice(cmsNotice('model-health', m.card, 'unhealthy'), copy.unhealthy, 'cms-error mt-2')
            : <p className="cms-muted mt-2 text-xs">{copy.healthUnknown}</p>
        )}
        {!m.managed && (
          <p className="mt-2 text-sm text-amber-800 dark:text-amber-300">
            {m.canAdopt ? copy.adoptNote : copy.observedOnlyNote}
          </p>
        )}
        {m.managed && <StartupControl copy={copy} title={copy.modelStartup(m.modelId)} visualTitle={copy.modelBootTitle}
          hint={m.canSetAutostart ? copy.modelStartupHint : copy.modelStartupUnsupported}
          enabled={m.bootEnabled} disabled={!m.canSetAutostart || !!busy || loading || m.status === 'starting'}
          testId={`cms-model-startup-${m.card}`}
          onToggle={() => act(m.card, () => actOnCmsModel(m.card, m.bootEnabled ? 'disable-boot' : 'enable-boot'))} />}
        <div className="mt-3 flex flex-wrap gap-2">
          {(m.status !== 'running' || !m.managed) && <button
            type="button"
            disabled={(!m.managed && !m.canAdopt) || !!busy || m.status === 'starting'}
            onClick={() => act(m.card, () => actOnCmsModel(m.card, 'enable'))}
            className={m.status !== 'running' ? 'cms-primary' : ''}
          >
            {m.managed ? copy.startModel : copy.adoptModel}
          </button>}
          {(!m.managed || models?.activeModel !== m.card) && <button
            type="button"
            disabled={
              !m.managed ||
              !!busy ||
              m.status !== 'running' ||
              m.healthy !== true ||
              models?.activeModel === m.card
            }
            onClick={() => act(m.card, () => actOnCmsModel(m.card, 'set-default'))}
          >
            {copy.setDefault}
          </button>}
          {(m.status !== 'stopped' || !m.managed) && <button
            type="button"
            disabled={!m.managed || !!busy || m.status === 'stopped'}
            onClick={(event) => {
              stopReturn.current = event.currentTarget;
              setConfirmStop(m.card);
            }}
            className="cms-danger"
          >
            {copy.stopModel}
          </button>}
        </div>
        {confirmStop === m.card && (
          <div
            role="alertdialog"
            aria-label={`${copy.stopModel} · ${m.modelId}`}
            aria-describedby={confirmId}
            className="cms-notice cms-notice-warning mt-3"
          >
            <p className="font-semibold break-words">{m.modelId}</p>
            <p id={confirmId} className="mt-1 text-sm">
              {copy.stopWarning}
            </p>
            <div className="mt-3 flex flex-wrap gap-2">
              <button
                type="button"
                ref={stopConfirm}
                disabled={!!busy}
                onClick={() => act(m.card, () => actOnCmsModel(m.card, 'disable'))}
                className="cms-danger"
              >
                {copy.confirmStop}
              </button>
              <button type="button" onClick={() => setConfirmStop('')}>
                {copy.keepRunning}
              </button>
            </div>
          </div>
        )}
        <details className="cms-details mt-3">
          <summary>{copy.modelDetails}</summary>
          <Row label={copy.modelCard}>
            <code>{m.card}</code>
          </Row>
          {m.port && <Row label={copy.port}>{m.port}</Row>}
          {m.degradedReason && <Row label={copy.issueDetails}>{m.degradedReason}</Row>}
          <Row label={copy.residentMemory}>{formatGiB(m.footprintGiB)}</Row>
          {m.restartCount > 0 && (
            <p className="pt-2 text-xs">{copy.restarts(m.restartCount, m.lastRestart || '—')}</p>
          )}
        </details>
      </article>
    );
  };

  return (
    <div className="cms-management">
      <div className="cms-toolbar">
        <p className="text-xs text-black/60 dark:text-white/60">
          {lastRefresh ? copy.lastRefresh(lastRefresh.toLocaleTimeString()) : copy.notRefreshed}
        </p>
        <div className="flex flex-wrap gap-2">
          {onOpenModelStore && (
            <button type="button" className="cms-primary" onClick={onOpenModelStore}>
              <Store size={15} />
              {copy.modelStore}
            </button>
          )}
          <button
            type="button"
            disabled={loading}
            data-testid="cms-panel-refresh"
            onClick={loadAll}
          >
            <RefreshCw
              size={14}
              className={loading ? 'animate-spin motion-reduce:animate-none' : ''}
            />
            {copy.refresh}
          </button>
        </div>
      </div>
      {unavailableReason && renderNotice(cmsNotice('connection', 'cmsd', unavailableReason), <>
          <h3 className="font-semibold">{copy.unavailable}</h3>
          <p className="mt-1 text-sm">{copy.offlineHelp}</p>
          <details className="cms-details mt-2">
            <summary>{copy.technicalDetails}</summary>
            <p className="break-all text-xs">{unavailableReason}</p>
          </details>
        </>, 'cms-notice cms-notice-error')}
      {partialError && !unavailableReason && (
        renderNotice(cmsNotice('query', 'partial', partialError), copy.partialError, 'cms-notice cms-notice-warning')
      )}
      {operationError && (
        <CmsNotice copy={copy} onRead={() => setOperationError('')} className="cms-notice cms-notice-error break-words">
          {operationError}
        </CmsNotice>
      )}
      {!unavailableReason &&
        status?.degraded.map((d) => (
          renderNotice(modelIssueNotice(d.card, d.reason), copy.degradedBanner(d.card, d.reason), 'cms-notice cms-notice-error break-words')
        ))}

      {!unavailableReason && engineFailures.length > 0 && (
        renderNotice(engineIssueNotice(engines), <>
          {copy.sectionEngines} · {copy.unhealthy}:{' '}
          {engineFailures.map((engine) => engine.id).join(', ')}
        </>, 'cms-notice cms-notice-error')
      )}

      {!unavailableReason &&
        activeModel &&
        (activeModel.status !== 'running' || activeModel.healthy !== true) && (
          renderNotice(cmsNotice('default-ready', activeModel.card, 'not-ready'), copy.defaultNotReady,
            'cms-notice cms-notice-warning', { 'data-testid': 'cms-default-not-ready' })
        )}
      {saveFailed && <p role="status" className="cms-muted text-xs">{copy.noticeReadNotSaved}</p>}
      <CmsOverview copy={copy} status={status} models={models} capacity={capacity}
        loading={loading} unavailableReason={unavailableReason} uptime={uptimeText(status?.uptime)} />

      <div className={`cms-dashboard ${unavailableReason ? 'cms-dashboard-offline' : ''}`}>
        {!unavailableReason && <div className="cms-main-stack">
          <Section title={copy.sectionModels} hint={copy.localModelsHint} icon={Layers}>
            {!models && <Row label={loading ? copy.loading : copy.queryFailed} />}
            {models?.models.length === 0 && <Row label={copy.noModels} />}
            {modelRows.filter(model => !stoppedModels.includes(model)).map(renderModel)}
            {stoppedModels.length > 0 && <details className="cms-details cms-model-library" data-testid="cms-stopped-models">
              <summary>{copy.stoppedModels(stoppedModels.length)}{stoppedModels.some(model => model.degradedReason && !isRead(modelIssueNotice(model.card, model.degradedReason))) && <span className="cms-task-attention ml-2">{copy.taskAttention(stoppedModels.filter(model => model.degradedReason && !isRead(modelIssueNotice(model.card, model.degradedReason))).length)}</span>}</summary>
              {stoppedModels.map(renderModel)}
            </details>}
            {status?.storeAdoption && copy.service && <div className="cms-store-reconciliation" data-testid="cms-store-adoption">
              {status.storeAdoption.phase === 'unavailable' && <p role="status" className="cms-error mb-2 text-xs">{copy.service.adoptionUnavailable}</p>}
              {status.storeAdoption.skipped?.length > 0 && <p role="status" className="cms-error mb-2 text-xs">{copy.service.adoptionPending}</p>}
              <details className="cms-details">
                <summary><span>{copy.service.adoptionTitle}</span><span className="cms-badge">{status.storeAdoption.phase === 'unavailable' ? copy.syncUnavailable : status.storeAdoption.skipped?.length ? copy.syncPending : copy.syncedModels}</span></summary>
                <p className="cms-muted text-xs">{status.storeAdoption.phase === 'unavailable' ? copy.service.adoptionUnavailable : status.storeAdoption.skipped?.length ? copy.service.adoptionPending : copy.service.adoptionReady}</p>
                {status.storeAdoption.error && <p className="cms-error text-xs break-all">{status.storeAdoption.error}</p>}
                {(status.storeAdoption.skipped || []).map((item, index) => <p className="text-xs break-all" key={index}>{item.card} · {item.reason}</p>)}
              </details>
            </div>}
          </Section>
          <details className="cms-card cms-disclosure cms-task-panel" data-testid="cms-tasks-panel"
            open={attentionTasks.some(task => ['queued', 'running', 'cancelling'].includes(task.status)) || undefined}>
            <summary>
              <span className="cms-icon-tile" aria-hidden="true"><ClipboardList size={18} /></span>
              <span className="cms-disclosure-label"><span className="cms-heading">{copy.sectionTasks}</span></span>
              <span className={attentionTasks.length ? 'cms-disclosure-meta cms-task-attention' : 'cms-disclosure-meta cms-muted'}>{attentionTasks.length ? copy.taskAttention(attentionTasks.length) : tasks ? copy.completedTasks(finishedTasks.length) : '—'}</span>
            </summary>
            <p className="cms-muted px-5 pb-3 text-xs">{copy.backgroundHelp}</p>
            {!tasks && <Row label={loading ? copy.loading : copy.queryFailed} />}
            {tasks?.length === 0 && <Row label={copy.noTasks} />}
            {attentionTasks.length > 0 && <div aria-label={copy.attentionTasks} data-testid="cms-task-attention">{attentionTasks.map(renderTask)}</div>}
            {finishedTasks.length > 0 && <details className="cms-details mx-5 mb-4" data-testid="cms-task-history">
              <summary>{copy.completedTasks(finishedTasks.length)}</summary>
              {finishedTasks.slice(0, showAllTasks ? undefined : 20).map(renderTask)}
              {finishedTasks.length > 20 && <div className="mt-3 space-y-2">
                <p className="text-xs">{copy.historyLimit}</p>
                <button type="button" aria-expanded={showAllTasks} onClick={() => setShowAllTasks(value => !value)}>
                  {showAllTasks ? copy.showLessHistory : copy.showAllHistory(finishedTasks.length)}
                </button>
              </div>}
            </details>}
          </details>
        </div>}
        {copy.service && <div className="cms-side-stack"><CmsDaemonPanel copy={copy.service} onChanged={refreshAfterChange} /></div>}
      </div>

      <details className="cms-card cms-disclosure" data-testid="cms-advanced">
        <summary>
          <span className="cms-icon-tile" aria-hidden="true"><Wrench size={18} /></span>
          <span className="cms-disclosure-label"><span className="cms-heading">{copy.diagnosticsTitle}</span><span className="cms-muted text-xs">{copy.diagnosticsHint}</span></span>
        </summary>
        <div className="cms-diagnostics-content">
          {!unavailableReason && <>
            <Section title={copy.sectionStatus}>
              <Row label={copy.daemon}>{copy.service.displayName} {status?.version || '—'}</Row>
              <Row label={copy.gateway}>{status?.gateway || '—'}</Row>
              <Row label={copy.activeModel}>{activeModel?.modelId || status?.activeModel || copy.noActiveModel}</Row>
            </Section>
            <Section title={copy.sectionEngines}>
              {!engines && <Row label={loading ? copy.loading : copy.queryFailed} />}
              {engines?.length === 0 && <Row label={copy.noEngines} />}
              {visibleEngines.map(engine => <CmsEngineRow key={engine.id} engine={engine} copy={copy} />)}
              {inactiveEngines.length > 0 && <button type="button" className="m-4"
                aria-expanded={showInactiveEngines} onClick={() => setShowInactiveEngines(!showInactiveEngines)}>
                {showInactiveEngines ? copy.hideInactiveRuntimes : copy.inactiveRuntimes(inactiveEngines.length)}
              </button>}
            </Section>
            <Section title={copy.sectionCapacity}>
              <Row label={copy.memoryTotal}>{capacity ? formatGiB(capacity.totalGiB) : '—'}</Row>
              <Row label={copy.memoryAvailable}>{capacity ? formatGiB(capacity.availableGiB) : '—'}</Row>
              <Row label={copy.safetyMargin}>{capacity ? formatGiB(capacity.marginGiB) : '—'}</Row>
              {(capacity?.resident || []).map(resident => <Row key={resident.card} label={copy.residentRow(resident.card)}>{formatGiB(resident.measuredGiB)}</Row>)}
            </Section>
          </>}
        </div>
      </details>
    </div>
  );
}
