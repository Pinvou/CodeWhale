import React, { useEffect, useState } from 'react';
import { adminLayoutCopy, financeCopy } from '../../shared/i18n.js';
import { integerMoney, formatFen } from './finance-contract.js';
import { formatBalance } from './balance-view.js';
import { financeOverviewDrilldown, financeOverviewMetricTimeScope, formatFinanceOverviewValue } from './admin-finance-overview.js';
import { financeListRouteUpdates, financeReadDefinition } from './admin-finance-read-route.js';

export function AdminFinanceOverview({ client, language, surface, route, permissions }) {
  const words = adminLayoutCopy[language].financeOverview, copy = financeCopy[language];
  const [state, setState] = useState({ client: null, surface: '', data: null, error: null }), [revision, setRevision] = useState(0);
  useEffect(() => {
    const controller = new AbortController(); setState({ client, surface, data: null, error: null });
    void (async () => {
      const data = await client.financeOverview(surface, controller.signal);
      if (!Number.isFinite(Date.parse(data?.as_of)) || !Array.isArray(data.metrics) || !data.metrics.length) throw new Error('INVALID_FINANCE_OVERVIEW');
      if (data.window && (!Number.isFinite(Date.parse(data.window.from)) || !Number.isFinite(Date.parse(data.window.to)) || data.window.timezone !== 'UTC')) throw new Error('INVALID_FINANCE_OVERVIEW');
      for (const metric of data.metrics) {
        if (typeof metric.key !== 'string' || !metric.key || !['COUNT', 'MICROCREDITS', 'CNY_FEN'].includes(metric.unit) || !Array.isArray(metric.included_statuses) || metric.included_statuses.some(value => typeof value !== 'string')) throw new Error('INVALID_FINANCE_OVERVIEW');
        integerMoney(metric.value);
      }
      if (!controller.signal.aborted) setState({ client, surface, data, error: null });
    })().catch(error => { if (!controller.signal.aborted) setState({ client, surface, data: null, error }); });
    return () => controller.abort();
  }, [client, surface, revision]);
  const visible = state.client === client && state.surface === surface ? state : {};
  // The accounts endpoint publishes all metrics in one point-in-time response.
  const sharedAccountSnapshot = surface === 'accounts' && visible.data && visible.data.window == null;
  const number = value => integerMoney(value).toLocaleString({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language]);
  const date = value => new Date(value).toLocaleString({ zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language], { timeZone: 'UTC' });
  const drilldown = target => {
    if (['products', 'pricing'].includes(surface)) return route.navigate('products', { ...(surface === 'products' ? financeListRouteUpdates('products', {}) : {}), finance_tab: target.tab, product_cursor: null, ...target.query });
    const section = surface === 'accounts' ? 'billing' : 'payments';
    const updates = financeReadDefinition(target.tab) ? financeListRouteUpdates(target.tab, target.query) : {};
    route.navigate(section, { finance_tab: target.tab, ...updates });
  };
  return <section className="adm-finance-overview" data-finance-overview={surface}><header>{sharedAccountSnapshot ? <div><h3>{words.title}</h3><small>{words.point}: <time dateTime={visible.data.as_of}>{date(visible.data.as_of)} UTC</time></small></div> : <h3>{words.title}</h3>}<button onClick={() => setRevision(value => value + 1)}>{copy.refresh}</button></header>
    {visible.error && <p role="alert">{visible.error.status === 403 ? copy.permissions : copy.unavailable}<button onClick={() => setRevision(value => value + 1)}>{copy.retry}</button></p>}{!visible.data && !visible.error && <p role="status">{copy.loading}</p>}
    {visible.data && <><div className="adm-finance-overview-grid">{visible.data.metrics.map(metric => {
      const candidate = financeOverviewDrilldown(surface, metric.key, visible.data.window);
      const target = candidate?.tab === 'reconciliations' && !permissions.includes('platform.billing.reconciliation.read') ? null : candidate;
      const usesWindow = financeOverviewMetricTimeScope(surface, metric.key) === 'window' && visible.data.window;
      const content = <><span>{words.metrics[metric.key] || metric.key}</span><strong>{formatFinanceOverviewValue(metric, { formatCount: number, formatPoints: value => formatBalance(value, language), formatMoney: value => formatFen(value, language) })}</strong>{sharedAccountSnapshot ? <small>{words.units[metric.unit]}</small> : <><small>{usesWindow ? `${words.window}: ${date(visible.data.window.from)} – ${date(visible.data.window.to)} UTC` : `${words.point}: ${date(visible.data.as_of)} UTC`}</small><small>{words.units[metric.unit]} · {words.statuses}: {metric.included_statuses.join(', ') || '—'}</small></>}{target && <em>{target.exact ? words.filtered : words.all}</em>}</>;
      return target ? <button className="adm-finance-overview-card" key={metric.key} onClick={() => drilldown(target)}>{content}</button> : <article className="adm-finance-overview-card" key={metric.key}>{content}</article>;
    })}</div>{sharedAccountSnapshot ? <footer><details className="adm-finance-overview-scope"><summary>{words.metricScope}</summary><dl>{visible.data.metrics.map(metric => <React.Fragment key={metric.key}><dt>{words.metrics[metric.key] || metric.key}</dt><dd>{words.statuses}: {metric.included_statuses.join(', ') || '—'}</dd></React.Fragment>)}</dl></details></footer> : <small>{words.point}: {date(visible.data.as_of)} UTC</small>}</>}
  </section>;
}
