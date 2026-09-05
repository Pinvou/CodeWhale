import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { modelOperationsOverviewDrilldown, modelOperationsWindow, validateModelOperationsOverview } from './admin-model-operations-overview.js';
import { readAdminModelOperationsWindow } from './admin-navigation.js';

const groups = Object.freeze({
  models: ['published_model_count', 'deprecated_model_count', 'model_draft_count'],
  routing: ['stable_route_count', 'healthy_route_count', 'route_attention_count', 'provisioning_pending_count', 'provisioning_attention_count'],
  reconciliation: ['usage_request_count', 'reconciliation_pending_count', 'reconciliation_stopped_count'],
});

export function AdminModelOperationsOverview({ client, copy, routeVersion, onRouteChange, onNavigate }) {
  const windowValue = useMemo(() => readAdminModelOperationsWindow(), [routeVersion]);
  const [refreshVersion, setRefreshVersion] = useState(0);
  const [state, setState] = useState({ loading: true, data: null, error: null });
  const generation = useRef(0);
  const controllerRef = useRef(null);
  const reload = useCallback(async () => {
    const requestGeneration = ++generation.current;
    controllerRef.current?.abort();
    const controller = new globalThis.AbortController();
    controllerRef.current = controller;
    setState({ loading: true, data: null, error: null });
    try {
      const raw = await client.tokenOperationsOverview(modelOperationsWindow(windowValue), controller.signal);
      const data = validateModelOperationsOverview(raw);
      if (!data) throw Object.assign(new Error('invalid operations overview'), { code: 'INVALID_OVERVIEW_RESPONSE' });
      if (requestGeneration === generation.current && !controller.signal.aborted) setState({ loading: false, data, error: null });
    } catch (error) {
      if (requestGeneration === generation.current && error?.name !== 'AbortError') setState({ loading: false, data: null, error: { code: error?.code || 'OVERVIEW_UNAVAILABLE', requestID: error?.requestID || '' } });
    }
  }, [client, windowValue]);
  useEffect(() => {
    void reload();
    return () => { generation.current += 1; controllerRef.current?.abort(); };
  }, [reload, refreshVersion]);

  const labels = copy.operationsOverview;
  const data = state.data;
  const maxTrend = data ? Math.max(1, ...data.trend.points.flatMap((point) => [point.usage_request_count, point.reconciliation_attention_count])) : 1;
  const openMetric = (key) => {
    const target = modelOperationsOverviewDrilldown(key, data.window);
    if (target) onNavigate(target.section, { ...target.query, model_operations_window: windowValue });
  };
  return <section className="adm-model-operations-overview" aria-labelledby="model-operations-overview-title">
    <header>
      <div><span className="adm-eyebrow">{labels.eyebrow}</span><h2 id="model-operations-overview-title">{labels.title}</h2><p>{labels.boundary}</p></div>
      <div className="adm-model-overview-actions"><label>{labels.window}<select value={windowValue} onChange={(event) => onRouteChange({ model_operations_window: event.target.value }, 'push')}><option value="24h">{labels.windows['24h']}</option><option value="7d">{labels.windows['7d']}</option><option value="30d">{labels.windows['30d']}</option></select></label><button type="button" disabled={state.loading} onClick={() => setRefreshVersion((value) => value + 1)}>{copy.refreshSource}</button></div>
    </header>
    {state.loading && !data && <div className="adm-model-overview-state" role="status">{copy.loading}</div>}
    {state.error && <div className="adm-model-overview-state error" role="alert"><span>{labels.unavailable}{state.error.code ? ` · ${state.error.code}` : ''}{state.error.requestID ? ` · ${copy.requestID}: ${state.error.requestID}` : ''}</span><button type="button" onClick={() => setRefreshVersion((value) => value + 1)}>{copy.retry}</button></div>}
    {data && !state.error && <>
      <div className="adm-model-overview-groups">{Object.entries(groups).map(([group, keys]) => <article key={group}><h3>{labels.groups[group]}</h3><div>{keys.map((key) => { const metric = data.metrics.get(key); const target = modelOperationsOverviewDrilldown(key, data.window); const content = <><span>{labels.metrics[key]}</span><strong>{metric.value.toLocaleString()}</strong><small>{metric.denominator_key ? labels.denominator(metric.denominator_key, data.metrics.get(metric.denominator_key)?.value || 0) : metric.aggregation === 'WINDOW' ? labels.windowMetric : labels.pointMetric}</small></>; return target ? <button type="button" key={key} onClick={() => openMetric(key)}>{content}<em>{target.exact ? labels.exactDrilldown : labels.relatedDrilldown}</em></button> : <div key={key}>{content}</div>; })}</div></article>)}</div>
      <section className="adm-model-overview-trend" aria-labelledby="model-overview-trend-title"><div><h3 id="model-overview-trend-title">{labels.trendTitle}</h3><p>{labels.trendLead}</p></div><div className="adm-model-trend-legend"><span className="usage">{labels.usageTrend}</span><span className="reconciliation">{labels.reconciliationTrend}</span></div>{data.trendHasData ? <div className="adm-model-trend-chart" role="img" aria-label={labels.trendAria(data.window.from, data.window.to)}>{data.trend.points.map((point) => <div key={point.from} title={`${new Date(point.from).toLocaleString()} · ${labels.usageTrend} ${point.usage_request_count} · ${labels.reconciliationTrend} ${point.reconciliation_attention_count}`}><i className="usage" style={{ height: `${point.usage_request_count === 0 ? 0 : Math.max(2, point.usage_request_count / maxTrend * 100)}%` }} /><i className="reconciliation" style={{ height: `${point.reconciliation_attention_count === 0 ? 0 : Math.max(2, point.reconciliation_attention_count / maxTrend * 100)}%` }} /></div>)}</div> : <p className="adm-model-trend-empty" role="status">{labels.trendEmpty}</p>}<small>{labels.bucket(data.trend.bucket_seconds)} · {labels.halfOpenWindow(new Date(data.window.from).toLocaleString(), new Date(data.window.to).toLocaleString(), data.window.timezone)}</small></section>
      <footer>{labels.asOf}: {new Date(data.as_of).toLocaleString()} · {labels.zeroKnown} · {labels.crossDomain}</footer>
    </>}
  </section>;
}
