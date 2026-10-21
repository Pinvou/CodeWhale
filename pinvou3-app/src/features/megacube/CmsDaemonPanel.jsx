import React, { useCallback, useEffect, useId, useRef, useState } from 'react';
import { invokeTauri, openTauriDialog } from '../../platform/tauri/client.js';
import { StartupControl } from './StartupControl.jsx';
import { CmsDaemonIntroduction } from './CmsDaemonIntroduction.jsx';
import { useCmsInstallReminder } from './use-cms-install-reminder.js';
import { CmsBundledConfirmDialog } from './CmsBundledConfirmDialog.jsx';
import { RefreshCw, Server } from '../../components/icons.jsx';
export function CmsDaemonPanel({ copy, onChanged }) {
  const [service, setService] = useState(null),
    [error, setError] = useState(''),
    [queryError, setQueryError] = useState(''),
    [requesting, setRequesting] = useState(false);
  const [packagePath, setPackagePath] = useState(''),
    [candidate, setCandidate] = useState(null),
    [trusted, setTrusted] = useState(false),
    [confirm, setConfirm] = useState('');
  const [logs, setLogs] = useState(null);
  const [bundledReview, setBundledReview] = useState(null);
  const actionInFlight = useRef(false);
  const installReminder = useCmsInstallReminder(service);
  const impactId = useId();
  const maintenance = useRef(null);
  const confirmButton = useRef(null),
    returnFocus = useRef(null);
  useEffect(() => {
    if (!confirm) {
      returnFocus.current?.focus();
      return;
    }
    confirmButton.current?.focus();
    const escape = (event) => {
      if (event.key === 'Escape') {
        event.preventDefault();
        setConfirm('');
      }
    };
    document.addEventListener('keydown', escape);
    return () => document.removeEventListener('keydown', escape);
  }, [confirm]);
  const alive = useRef(true),
    inFlight = useRef(false),
    completed = useRef('');
  const loadFinished = useRef(Promise.resolve());
  const textError = useCallback(
    (value) => {
      const detail = String(value);
      const key = Object.keys(copy.errors).find((key) => detail.includes(key));
      return key ? `${copy.errors[key]} (${detail})` : detail;
    },
    [copy],
  );
  const load = useCallback(async (options) => {
    if (inFlight.current) {
      await loadFinished.current;
      if (options?.fresh === true) return load(options);
      return;
    }
    inFlight.current = true;
    let finish;
    loadFinished.current = new Promise(resolve => { finish = resolve; });
    try {
      const result = await invokeTauri('cms_service_status');
      if (!alive.current) return;
      setService(result);
      setQueryError('');
      const op = result?.operation;
      if (
        op &&
        ['succeeded', 'failed', 'interrupted'].includes(op.status) &&
        op.id !== completed.current
      ) {
        completed.current = op.id;
        onChanged?.();
      }
    } catch (e) {
      if (alive.current) setQueryError(textError(e));
    } finally {
      inFlight.current = false;
      finish();
    }
  }, [onChanged, textError]);
  useEffect(() => {
    alive.current = true;
    load();
    const timer = setInterval(load, 1500);
    return () => {
      alive.current = false;
      clearInterval(timer);
    };
  }, [load]);
  const running = requesting || ['queued', 'running'].includes(service?.operation?.status);
  const run = async (action, bundledPackage = null) => {
    if (actionInFlight.current || running) return;
    const bundled = bundledPackage?.source === 'bundled-with-pinvou' && bundledPackage?.available === true;
    const selected = bundled ? bundledPackage : candidate;
    const selectedPath = bundled ? bundledPackage.packagePath : packagePath;
    if (['install', 'update'].includes(action) && (!selected || !(bundled || trusted))) {
      setError(copy.packageRequired);
      return;
    }
    actionInFlight.current = true;
    setRequesting(true);
    setError('');
    try {
      const result = await invokeTauri('cms_service_action', {
        action,
        packagePath: ['install', 'update'].includes(action) ? selectedPath : null,
        expectedSha256: ['install', 'update'].includes(action)
          ? selected?.manifest.sha256
          : action === 'import_bootstrap'
            ? service?.bootstrapCandidate?.sha256
            : null,
      });
      setService((value) => ({
        ...value,
        operation: {
          id: result.operationId,
          action,
          status: 'queued',
          stage: 'queued',
        },
      }));
      setConfirm('');
      setBundledReview(null);
      await load({ fresh: true });
    } catch (e) {
      if (bundled) setBundledReview(value => value && ({ ...value, error: textError(e) }));
      else setError(textError(e));
    } finally {
      actionInFlight.current = false;
      setRequesting(false);
    }
  };
  const inspect = async () => {
    setRequesting(true);
    setError('');
    setCandidate(null);
    setTrusted(false);
    try {
      setCandidate(
        await invokeTauri('cms_service_inspect_package', {
          packagePath,
        }),
      );
    } catch (e) {
      setError(textError(e));
    } finally {
      setRequesting(false);
    }
  };
  const choose = async () => {
    try {
      const path = await openTauriDialog({
        directory: true,
        multiple: false,
        title: copy.choose,
      });
      if (typeof path === 'string') {
        setPackagePath(path);
        setCandidate(null);
        setTrusted(false);
      }
    } catch (e) {
      setError(textError(e));
    }
  };
  const useBundled = async () => {
    if (actionInFlight.current || running || queryError) return;
    const action = service?.canInstall ? 'install' : service?.canUpdate ? 'update' : null;
    if (!action) return;
    actionInFlight.current = true;
    setRequesting(true);
    setError('');
    setBundledReview({ action, phase: 'checking', package: null, error: '' });
    try {
      const result = await invokeTauri('cms_service_bundled_package');
      if (!result?.available || result.source !== 'bundled-with-pinvou' || !result.packagePath || !/^[a-f0-9]{64}$/.test(result.manifest?.sha256 || '')) throw new Error('bundled_package_missing');
      if (alive.current) setBundledReview({ action, phase: 'ready', package: result, error: '' });
    } catch (e) {
      if (alive.current) setBundledReview({ action, phase: 'failed', package: null, error: textError(e) });
    } finally {
      actionInFlight.current = false;
      if (alive.current) setRequesting(false);
    }
  };
  const actionLabel = (action) =>
    action === 'update' && service?.installed === false ? copy.repair : copy.actions[action];
  const impact =
    confirm === 'update' && service?.installed === false
      ? copy.repairImpact
      : confirm === 'import_bootstrap'
        ? copy.bootstrapImpact
        : confirm === 'stop'
          ? copy.stopImpact
          : confirm === 'uninstall'
            ? copy.uninstallImpact
            : confirm === 'install'
              ? copy.installImpact
              : confirm === 'rescue_stop'
                ? copy.rescueImpact
                : ['rollback', 'restore'].includes(confirm)
                  ? copy.rollbackImpact
                  : copy.updateImpact;
  const button = (action, enabled = true) => (
    <button
      key={action}
      type="button"
      data-testid={`cms-service-${action}`}
      disabled={running || !enabled}
      onClick={() =>
        ['start', 'resume', 'enable_boot', 'disable_boot'].includes(action)
          ? run(action)
          : ((returnFocus.current = document.activeElement), setConfirm(action))
      }
      className={
        ['stop', 'rescue_stop', 'uninstall'].includes(action)
          ? 'cms-danger'
          : action === 'start'
            ? 'cms-primary'
            : ''
      }
    >
      {actionLabel(action)}
    </button>
  );
  return (
    <section className="cms-card cms-service-panel" data-testid="cms-service-panel" aria-busy={running}>
      <div className="cms-toolbar">
        <h3 className="cms-heading"><span className="cms-icon-tile" aria-hidden="true"><Server size={18} /></span>{copy.title}</h3>
        <button type="button" className="cms-icon-button" aria-label={copy.refresh} title={copy.refresh} disabled={requesting} onClick={load}>
          <RefreshCw size={15} />
        </button>
      </div>
      {bundledReview && <CmsBundledConfirmDialog copy={copy} review={bundledReview} busy={requesting}
        blockedReason={queryError ? copy.queryFailed : !requesting && ['queued', 'running'].includes(service?.operation?.status) ? copy.errors.component_busy : !(bundledReview.action === 'install' ? service?.canInstall : service?.canUpdate) ? copy.errors.service_configuration_changed : ''}
        actionLabel={actionLabel(bundledReview.action)}
        impact={bundledReview.action === 'install' ? copy.installImpact : service?.installed === false ? copy.repairImpact : copy.updateImpact}
        onCancel={() => setBundledReview(null)} onConfirm={() => run(bundledReview.action, bundledReview.package)} />}
      <CmsDaemonIntroduction copy={copy} expanded={service?.supported === true && service?.installed === false && service?.canInstall === true} />
      {queryError && (
        <p role="alert" className="cms-notice cms-notice-error">
          {copy.queryFailed} {queryError}
        </p>
      )}
      {error && (
        <p role="alert" className="cms-notice cms-notice-error">
          {error}
        </p>
      )}
      {!service && (
        <p role="status" className="text-sm">
          {copy.loading}
        </p>
      )}
      {service?.supported === false && <p className="text-sm">{copy.unsupported}</p>}
      {service?.supported && (
        <>
          <div className="mt-3 flex flex-wrap items-center gap-2 text-sm">
            <span
              className={`cms-service-state ${service.activeState === 'active' ? 'cms-service-active' : ''}`}
            >
              {service.scope === 'unverified'
                ? copy.unverified
                : service.installed
                  ? copy.active[service.activeState] || service.activeState
                  : copy.notInstalled}
            </span>
            <span className="text-black/60 dark:text-white/60">
              {service.scope === 'user' ? copy.user : copy.unverified}
            </span>
          </div>
          {service.reason && (
            <p className="mt-3 text-sm text-amber-800 dark:text-amber-300">
              {service.reason === 'external_installation' ? copy.external : copy.unverified}
            </p>
          )}
          {service.canInstall && <div className="cms-notice mt-3" data-testid="cms-service-install-guide">
            <p className="text-sm">{copy.installGuide}</p>
            <button type="button" className="cms-primary mt-3" disabled={running || !!queryError} onClick={useBundled} data-testid="cms-service-prepare-install">{copy.useBundled}</button>
            {installReminder.muted && <div className="mt-3 flex flex-wrap items-center gap-2 text-xs" data-testid="cms-install-reminder-muted">
              <span>{copy.installReminderMuted}</span>
              <button type="button" onClick={() => installReminder.change(false)} data-testid="cms-install-reminder-restore">{copy.restoreInstallReminder}</button>
            </div>}
            {installReminder.failed && <p role="alert" className="cms-error mt-2 text-sm">{copy.installReminderFailed}</p>}
          </div>}
          {service.installed && <StartupControl copy={copy} title={copy.daemonStartup}
            hint={service.lingerEnabled === true ? copy.daemonBootHint : copy.daemonLoginHint}
            enabled={service.bootEnabled} disabled={running || !!queryError || service.scope !== 'user'}
            testId="cms-service-startup" onToggle={() => run(service.bootEnabled ? 'disable_boot' : 'enable_boot')} />}
          {service.bootstrapCandidate && (
            <p className="cms-notice cms-notice-warning mt-3">{copy.bootstrapAvailable}</p>
          )}
          {service.bootstrapImported && <p className="mt-3 text-sm">{copy.bootstrapImported}</p>}
          {service.bootstrapImportError && (
            <p role="status" className="cms-error mt-3">
              {textError(service.bootstrapImportError)}
            </p>
          )}
          <div className="mt-4 flex flex-wrap gap-2">
            {service.canStart && button('start')}
            {service.installed && service.canUpdate && <button type="button" disabled={running || !!queryError} onClick={useBundled} data-testid="cms-service-bundled-update">{copy.actions.update}</button>}
            {service.canImportBootstrap && button('import_bootstrap')}
            {service.canResumeMaintenance && button('resume')}
            {!service.installed && service.canRollback && button('rollback')}
            {service.canRestore && button('restore')}
          </div>
          {confirm && (
            <div
              role="alertdialog"
              aria-modal="false"
              aria-label={actionLabel(confirm)}
              aria-describedby={impactId}
              className="cms-notice cms-notice-warning mt-4"
            >
              <p className="mb-2 font-semibold">{actionLabel(confirm)}</p>
              <p id={impactId} className="text-sm">
                {impact}
              </p>
              <div className="mt-3 flex flex-wrap gap-2">
                <button
                  type="button"
                  className="cms-danger"
                  disabled={running}
                  onClick={() => run(confirm)}
                  data-testid="cms-service-confirm"
                  ref={confirmButton}
                >
                  {copy.confirm} · {actionLabel(confirm)}
                </button>
                <button type="button" onClick={() => setConfirm('')}>
                  {copy.cancel}
                </button>
              </div>
            </div>
          )}
          {service.operation && (
            <details className="cms-details mt-4" open={service.operation.status !== 'succeeded' || undefined}>
              <summary>{copy.operation}</summary>
              <div role="status" className="text-sm">
              <p className="font-medium">
                {copy.operation} ·{' '}
                {copy.actions[service.operation.action] || service.operation.action} ·{' '}
                {copy.stages[service.operation.stage] || service.operation.stage}
              </p>
              <p className="mt-1">
                {service.operation.status === 'succeeded'
                  ? copy.done
                  : ['failed', 'interrupted'].includes(service.operation.status)
                    ? copy[service.operation.status]
                    : copy.background}
              </p>
              {service.operation.error && (
                <p className="cms-error mt-2">{textError(service.operation.error)}</p>
              )}
              </div>
            </details>
          )}
          {service.operation?.recovery && (
            <p
              role="status"
              className={`mt-3 text-sm ${service.operation.recovery.status === 'succeeded' ? 'text-green-800 dark:text-green-300' : 'cms-error'}`}
            >
              {service.operation.recovery.status === 'succeeded'
                ? copy.recoveryRestored[service.operation.recovery.previousState] || copy.unverified
                : copy.recoveryFailed}
            </p>
          )}
          <details className="cms-details mt-4" data-testid="cms-service-advanced" ref={maintenance}>
            <summary>{copy.serviceMaintenance}</summary>
            {service.installed && <div className="my-3">
              <h4 className="text-sm font-medium">{copy.serviceControls}</h4>
              <div className="mt-2 flex flex-wrap gap-2">
                {service.canStop && button('stop')}
                {button('restart')}
                {service.canRollback && button('rollback')}
                {service.canStop && button('rescue_stop')}
              </div>
            </div>}
            <p className="mt-2 text-sm text-black/60 dark:text-white/60">{copy.description}</p>
            <dl className="my-3 grid grid-cols-1 gap-x-4 gap-y-2 text-sm sm:grid-cols-[auto_minmax(0,1fr)]">
              <dt>{copy.unit}</dt>
              <dd className="break-all">{service.unitName || copy.unverified}</dd>
              <dt>{copy.version}</dt>
              <dd>{service.version || '—'}</dd>
              <dt>{copy.socket}</dt>
              <dd className="break-all">{service.socketPath || '—'}</dd>
            </dl>
            {service.reason && <p className="my-2 break-all text-xs">{service.reason}</p>}
            {service.operation && (
              <p className="my-2 break-all font-mono text-xs">{service.operation.id}</p>
            )}
            {service.bootstrapCandidate && (
              <div className="my-3 break-all text-xs">
                <p>{service.bootstrapCandidate.root}</p>
                <p>SHA256 {service.bootstrapCandidate.sha256}</p>
              </div>
            )}
            {(service.canInstall || service.canUpdate) && (
              <div className="my-4 space-y-3">
                <label className="block text-sm">
                  {copy.packagePath}
                  <input
                    data-testid="cms-service-package"
                    className="mt-1 w-full rounded border bg-transparent px-3 py-2"
                    value={packagePath}
                    placeholder={copy.packagePlaceholder}
                    onChange={(e) => {
                      setPackagePath(e.target.value);
                      setCandidate(null);
                      setTrusted(false);
                    }}
                  />
                </label>
                <div className="flex flex-wrap gap-2">
                  <button type="button" disabled={running} onClick={useBundled} data-testid="cms-service-bundled">
                    {copy.useBundled}
                  </button>
                  <button type="button" disabled={running} onClick={choose}>
                    {copy.choose}
                  </button>
                  <button
                    type="button"
                    disabled={running || !packagePath}
                    onClick={inspect}
                    data-testid="cms-service-inspect"
                  >
                    {copy.inspect}
                  </button>
                </div>
                <p className="text-xs text-black/60 dark:text-white/60">{copy.packageHint}</p>
                <p className="text-xs text-black/60 dark:text-white/60">{copy.bundledHint}</p>
                {candidate && (
                  <>
                    <p className="break-all text-sm">
                      {copy.packageReady} · {candidate.manifest.version} ·{' '}
                      {candidate.manifest.architecture} · SHA256 {candidate.manifest.sha256}
                    </p>
                    <label className="flex items-start gap-2 text-sm">
                      <input
                        type="checkbox"
                        checked={trusted}
                        onChange={(e) => setTrusted(e.target.checked)}
                      />
                      {copy.packageTrust}
                    </label>
                  </>
                )}
              </div>
            )}
            <p className="my-3 text-sm">{copy.preservation}</p>
            <div className="my-3 flex flex-wrap gap-2">
              {service.canInstall && button('install', !!candidate && trusted)}
              {service.canUpdate && button('update', !!candidate && trusted)}
              {service.canUninstall && button('uninstall')}
            </div>
            {service.installed && (
              <button
                type="button"
                disabled={running}
                onClick={async () => {
                  if (logs !== null) {
                    setLogs(null);
                    return;
                  }
                  setError('');
                  try {
                    const result = await invokeTauri('cms_service_logs');
                    setLogs(result.lines || '');
                  } catch (e) {
                    setError(textError(e));
                  }
                }}
              >
                {logs === null ? copy.logs : copy.hideLogs}
              </button>
            )}
            {logs !== null && (
              <pre className="mt-3 max-h-64 overflow-auto whitespace-pre-wrap break-all rounded border p-3 text-xs">
                {logs}
              </pre>
            )}
          </details>
        </>
      )}
    </section>
  );
}
