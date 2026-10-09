import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import {
  modelTokenPageOverviewDrilldown,
  pageOverviewInteraction,
  validateModelOperationsPageOverview,
  readAdminModelOperationsWindow,
} from './admin-usage-overview.js';
import {
  createModelTokenPageOverviewRequestTracker,
  modelTokenPageOverviewCacheKey,
  planAdminModelTokenPageOverview,
} from './admin-usage-overview-state.js';

const tracker = createModelTokenPageOverviewRequestTracker();

function validate(section, raw, permissionView) {
  return validateModelOperationsPageOverview(raw, permissionView);
}

function overviewGroups(section, data) {
  if (section === 'models' || section === 'tokenResources') return [{ key: section, metrics: data.metrics }];
  return [
    ...(data.provisioning ? [{ key: 'provisioning', metrics: data.provisioning.metrics }] : []),
    ...(data.usage ? [{ key: 'usage', metrics: data.usage.metrics }] : []),
  ];
}

function MetricCard({ section, metric, data, labels, onDrilldown }) {
  let target = modelTokenPageOverviewDrilldown(section, metric.key, data.usage?.window);
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

function UsageTrend({ section, data, labels, onDrilldown }) {
  if (!data.usage) return null;
  const trend = data.usage.trend;
  const max = Math.max(1, ...trend.points.flatMap((point) => [point.usage_request_count, point.reconciliation_attention_count]));
  return <section className="adm-model-overview-trend" aria-labelledby="model-page-overview-trend-title">
    <div><h3 id="model-page-overview-trend-title">{labels.trendTitle}</h3><p>{labels.trendLead}</p></div>
    <div className="adm-model-trend-legend"><span className="usage">{labels.usageTrend}</span><span className="reconciliation">{labels.reconciliationTrend}</span></div>
    {trend.hasData ? <div className="adm-model-trend-chart" role="group" aria-label={labels.trendAria(data.usage.window.from, data.usage.window.to)}>
      {trend.points.map((point) => <div className="adm-model-trend-bucket" key={point.from}>
        <button type="button" className="usage" aria-label={labels.trendPoint(labels.usageTrend, point.from, point.to, point.usage_request_count)} onClick={() => onDrilldown(modelTokenPageOverviewDrilldown(section, 'usage_trend', data.usage.window, point), 'usage_trend')}><i style={{ height: `${point.usage_request_count === 0 ? 0 : Math.max(2, point.usage_request_count / max * 100)}%` }} /></button>
        <button type="button" className="reconciliation" aria-label={labels.trendPoint(labels.reconciliationTrend, point.from, point.to, point.reconciliation_attention_count)} onClick={() => onDrilldown(modelTokenPageOverviewDrilldown(section, 'reconciliation_trend', data.usage.window, point), 'reconciliation_trend')}><i style={{ height: `${point.reconciliation_attention_count === 0 ? 0 : Math.max(2, point.reconciliation_attention_count / max * 100)}%` }} /></button>
      </div>)}
    </div> : <p className="adm-model-trend-empty" role="status">{labels.trendEmpty}</p>}
    <small>{labels.bucket(trend.bucket_seconds)} · {labels.halfOpenWindow(new Date(data.usage.window.from).toLocaleString(), new Date(data.usage.window.to).toLocaleString(), data.usage.window.timezone)}</small>
  </section>;
}

export function AdminUsagePageOverview({ section, client, copy, permissions, userID, authorizationVersion, routeVersion, enabled, onRouteChange, onDrilldown }) {
  const labels = copy.pageOverviews;
  const windowValue = useMemo(() => readAdminModelOperationsWindow(), [routeVersion]);
  const plan = useMemo(() => planAdminModelTokenPageOverview(section, permissions, enabled, windowValue), [section, permissions, enabled, windowValue]);
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
        {section === 'modelOperations' && plan.permissionView !== 'TOKEN_ONLY' && <label>{labels.window}<select value={windowValue} onChange={(event) => onRouteChange({ model_operations_window: event.target.value }, 'push')}><option value="24h">{labels.windows['24h']}</option><option value="7d">{labels.windows['7d']}</option><option value="30d">{labels.windows['30d']}</option></select></label>}
        <button type="button" disabled={state.loading || retrySeconds > 0} onClick={() => setRefreshVersion((value) => value + 1)}>{retrySeconds > 0 ? labels.retryAfter(retrySeconds) : copy.refreshSource}</button>
      </div>
    </header>
    {state.loading && !data && <div className="adm-model-overview-state" role="status">{copy.loading}</div>}
    {state.error && <div className="adm-model-overview-state error" role="alert"><span>{state.error.status === 403 ? labels.permissionChanged : labels.unavailable[section]}{state.error.code ? ` · ${state.error.code}` : ''}{state.error.requestID ? ` · ${copy.requestID}: ${state.error.requestID}` : ''}{data ? ` · ${labels.stale}` : ''}</span><button type="button" disabled={retrySeconds > 0} onClick={() => setRefreshVersion((value) => value + 1)}>{retrySeconds > 0 ? labels.retryAfter(retrySeconds) : copy.retry}</button></div>}
    {data && <>
      <div className="adm-model-overview-groups">{overviewGroups(section, data).map((group) => <article key={group.key}><h3>{labels.groups[group.key]}</h3><div>{[...group.metrics.values()].map((metric) => <MetricCard key={metric.key} section={section} metric={metric} data={data} labels={labels} onDrilldown={onDrilldown} />)}</div></article>)}</div>
      <UsageTrend section={section} data={data} labels={labels} onDrilldown={onDrilldown} />
      <footer>{labels.asOf}: {new Date(data.as_of).toLocaleString()} · {copy.lastUpdated}: {new Date(state.updatedAt).toLocaleString()} · {labels.zeroKnown} · {labels.snapshotBoundary}</footer>
    </>}
  </section>;
}
