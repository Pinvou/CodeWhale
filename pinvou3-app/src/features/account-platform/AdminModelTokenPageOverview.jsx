import './admin-model-page-overview.css';
import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  modelTokenPageOverviewDrilldown,
  pageOverviewInteraction,
  validateModelCatalogPageOverview,
  validateProvisioningPageOverview,
  validateTokenResourcesPageOverview,
} from './admin-model-page-overview.js';
import {
  createModelTokenPageOverviewRequestTracker,
  modelTokenPageOverviewCacheKey,
  planAdminModelTokenPageOverview,
} from './admin-model-token-page-overview-state.js';

const tracker = createModelTokenPageOverviewRequestTracker();

function validate(section, raw, permissionView) {
  if (section === 'models') return validateModelCatalogPageOverview(raw);
  if (section === 'tokenResources') return validateTokenResourcesPageOverview(raw);
  return validateProvisioningPageOverview(raw, permissionView);
}

function overviewGroups(section, data) {
  if (section === 'models' || section === 'tokenResources') return [{ key: section, metrics: data.metrics }];
  return [{ key: 'provisioning', metrics: data.provisioning.metrics }];
}

function MetricCard({ section, metric, data, labels, onDrilldown }) {
  let target = modelTokenPageOverviewDrilldown(section, metric.key);
  const stableCount = section === 'tokenResources' ? data.metrics.get('stable_route_count').value : null;
  if (metric.key === 'healthy_route_count' && stableCount === 0) target = { interaction: pageOverviewInteraction.NON_INTERACTIVE };
  const detail = metric.key === 'healthy_route_count'
    ? stableCount === 0 ? labels.healthUnavailable : labels.healthRate(metric.value, stableCount)
    : metric.aggregation === 'WINDOW' ? labels.windowMetric : labels.pointMetric;
  const content = <>
    <span>{labels.metrics[metric.key]}</span>
    <strong>{metric.value.toLocaleString()}</strong>
    <small>{detail}</small>
    {target?.interaction === pageOverviewInteraction.EXACT && <em>{labels.exactDrilldown}</em>}
    {target?.interaction === pageOverviewInteraction.RELATED && <em>{labels.relatedDrilldown}</em>}
  </>;
  return target?.interaction === pageOverviewInteraction.EXACT || target?.interaction === pageOverviewInteraction.RELATED
    ? <button type="button" className="adm-model-overview-metric" onClick={() => onDrilldown(target, metric.key)}>{content}</button>
    : <div className="adm-model-overview-metric">{content}</div>;
}

export function AdminModelTokenPageOverview({ section, client, copy, permissions, userID, authorizationVersion, enabled, onDrilldown }) {
  const labels = copy.pageOverviews;
  const plan = useMemo(() => planAdminModelTokenPageOverview(section, permissions, enabled), [section, permissions, enabled]);
  const cacheKey = useMemo(() => plan ? modelTokenPageOverviewCacheKey({ userID, authorizationVersion, section, permissionView: plan.permissionView, window: plan.window }) : '', [authorizationVersion, plan, section, userID]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [clock, setClock] = useState(Date.now());
  const [state, setState] = useState({ key: '', loading: false, data: null, error: null, retryAt: 0, updatedAt: 0 });
  const controllerRef = useRef(null);

  const reload = useCallback(async () => {
    if (!plan || !cacheKey) return;
    const generation = tracker.begin(cacheKey);
    controllerRef.current?.abort();
    const controller = new globalThis.AbortController();
    controllerRef.current = controller;
    setState((current) => ({ key: cacheKey, loading: true, data: current.key === cacheKey ? current.data : null, error: null, retryAt: 0 }));
    try {
      const raw = await plan.load(client, controller.signal);
      const data = validate(section, raw, plan.permissionView);
      if (!data) throw Object.assign(new Error('invalid page overview response'), { code: 'INVALID_OVERVIEW_RESPONSE' });
      if (tracker.isCurrent(cacheKey, generation) && !controller.signal.aborted) setState({ key: cacheKey, loading: false, data, error: null, retryAt: 0, updatedAt: Date.now() });
    } catch (error) {
      if (!tracker.isCurrent(cacheKey, generation) || controller.signal.aborted || error?.name === 'AbortError') return;
      const retrySeconds = Math.max(0, Number(error?.retryAfterSeconds || 0));
      const failedAt = Date.now();
      setClock(failedAt);
      const status = Number(error?.status || 0);
      setState((current) => ({ ...current, key: cacheKey, loading: false, data: status === 403 ? null : current.data, error: { status, code: error?.code || 'OVERVIEW_UNAVAILABLE', requestID: error?.requestID || '' }, retryAt: retrySeconds ? failedAt + retrySeconds * 1000 : 0 }));
    }
  }, [cacheKey, client, plan, section]);

  useEffect(() => {
    if (!plan || !cacheKey) {
      controllerRef.current?.abort();
      setState({ key: '', loading: false, data: null, error: null, retryAt: 0, updatedAt: 0 });
      return undefined;
    }
    void reload();
    return () => {
      controllerRef.current?.abort();
      tracker.invalidate(cacheKey);
    };
  }, [cacheKey, plan, refreshVersion, reload]);

  useEffect(() => {
    if (!state.retryAt || state.retryAt <= clock) return undefined;
    const timer = globalThis.setTimeout?.(() => setClock(Date.now()), Math.min(1000, state.retryAt - clock));
    return () => globalThis.clearTimeout?.(timer);
  }, [clock, state.retryAt]);

  if (!plan || !cacheKey) return null;
  const data = state.key === cacheKey ? state.data : null;
  const retrySeconds = state.retryAt > clock ? Math.ceil((state.retryAt - clock) / 1000) : 0;
  const title = labels.titles[section];
  return <section className="adm-model-operations-overview adm-model-page-overview" aria-labelledby={`model-page-overview-title-${section}`}>
    <header>
      <div><span className="adm-eyebrow">{labels.eyebrow}</span><h2 id={`model-page-overview-title-${section}`}>{title}</h2><p>{labels.boundaries[section]}</p></div>
      <div className="adm-model-overview-actions">

        <button type="button" disabled={state.loading || retrySeconds > 0} onClick={() => setRefreshVersion((value) => value + 1)}>{retrySeconds > 0 ? labels.retryAfter(retrySeconds) : copy.refreshSource}</button>
      </div>
    </header>
    {state.loading && !data && <div className="adm-model-overview-state" role="status">{copy.loading}</div>}
    {state.error && <div className="adm-model-overview-state error" role="alert"><span>{state.error.status === 403 ? labels.permissionChanged : labels.unavailable[section]}{state.error.code ? ` · ${state.error.code}` : ''}{state.error.requestID ? ` · ${copy.requestID}: ${state.error.requestID}` : ''}{data ? ` · ${labels.stale}` : ''}</span><button type="button" disabled={retrySeconds > 0} onClick={() => setRefreshVersion((value) => value + 1)}>{retrySeconds > 0 ? labels.retryAfter(retrySeconds) : copy.retry}</button></div>}
    {data && <>
      <div className="adm-model-overview-groups">{overviewGroups(section, data).map((group) => <article key={group.key}><h3>{labels.groups[group.key]}</h3><div>{[...group.metrics.values()].map((metric) => <MetricCard key={metric.key} section={section} metric={metric} data={data} labels={labels} onDrilldown={onDrilldown} />)}</div></article>)}</div>
      <footer>{labels.asOf}: {new Date(data.as_of).toLocaleString()} · {copy.lastUpdated}: {new Date(state.updatedAt).toLocaleString()} · {labels.zeroKnown} · {labels.snapshotBoundary}</footer>
    </>}
  </section>;
}
