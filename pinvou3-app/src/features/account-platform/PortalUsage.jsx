import React, { useId, useState } from 'react';
import { accountCenterCopy } from '../../shared/i18n.js';
import { summarizeUsageRecords } from './usage-summary.js';
import { formatUsageTime } from './usage-time.js';
import { formatBalance } from './balance-view.js';
import { CopyIdentifier } from './CopyIdentifier.jsx';
import { useUsagePeriodSummary } from './useUsagePeriodSummary.js';

// The history endpoint is cursor-paginated. Never present this page's subtotal
// as an account-wide or date-range total.
export function PortalUsage({ client, owner, versions, copy, language, records, unavailable, page, hasPrevious, hasMore, busy, canCancel, onPrevious, onNext, onCancel }) {
  const words = accountCenterCopy[language];
  const [filters, setFilters] = useState({ from: '', to: '', model: '', status: '' });
  const modelOptionsID = useId();
  const period = useUsagePeriodSummary(client, owner, versions);
  // Preferred API: one logical-turn projection per row. Legacy API: individual
  // requests. Keep filtering at that transport row grain on every page.
  const summaries = records.map(record => summarizeUsageRecords([record])[0]);
  const invalidDates = Boolean(filters.from && filters.to && filters.from > filters.to);
  const options = field => [...new Set(summaries.map(record => record[field]).filter(Boolean))];
  const filtered = summaries.filter(record => {
    const date = new Date(record.started_at);
    const localDay = Number.isFinite(date.getTime()) ? `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}` : '';
    return !invalidDates && (!filters.from || localDay && localDay >= filters.from) && (!filters.to || localDay && localDay <= filters.to)
      && (!filters.model || record.model === filters.model) && (!filters.status || record.display_status === filters.status);
  });
  const total = filtered.reduce((sum, record) => record.charged_microcredits == null ? sum : sum + BigInt(record.charged_microcredits), 0n);
  const change = name => event => { period.invalidate(); setFilters(value => ({ ...value, [name]: event.target.value })); };
  const resultLabel = record => {
    if (record.usage_request_count === 1) {
      if (record.execution_status === 'FAILED' && record.settlement_status === 'RELEASED' && record.charged_microcredits != null && BigInt(record.charged_microcredits) === 0n) return words.callFailedNoCharge;
      if (record.execution_status === 'SUCCEEDED' && record.settlement_status === 'SETTLED' && record.charged_microcredits != null) return words.callCharged(formatBalance(record.charged_microcredits, language));
    }
    return copy.usageStatuses[record.display_status] || record.display_status;
  };
  return <section className="ap-usage-workspace">
    <h3>{copy.usageTitle}</h3>
    {unavailable ? <p role="alert">{copy.usageUnavailable}</p> : <>
      <div className="ap-usage-filters">
        <label>{words.usageFrom}<input type="date" value={filters.from} onChange={change('from')} /></label>
        <label>{words.usageTo}<input type="date" value={filters.to} onChange={change('to')} /></label>
        <label>{words.usageModel}<input list={modelOptionsID} placeholder={words.all} value={filters.model} onChange={change('model')} /><datalist id={modelOptionsID}>{options('model').map(value => <option key={value} value={value} />)}</datalist></label>
        <label>{words.usageStatus}<select value={filters.status} onChange={change('status')}><option value="">{words.all}</option>{[...new Set([...Object.keys(copy.usageStatuses), ...options('display_status')])].map(value => <option key={value} value={value}>{copy.usageStatuses[value] || value}</option>)}</select></label>
      </div>
      <p className="ap-muted">{words.usageScope}</p>
      {invalidDates && <p role="alert">{words.invalidDates}</p>}
      <section className="ap-period-summary" data-period-state={period.state?.status || 'idle'} aria-label={words.periodTitle}>
        <h4>{words.periodTitle}</h4><p className="ap-muted">{words.periodLead}</p>
        <button className="ap-primary" disabled={period.state?.status === 'loading' || !filters.from || !filters.to || invalidDates} onClick={() => void period.run(filters)}>{words.periodStart}</button>
        {!filters.from || !filters.to ? <p className="ap-muted">{words.periodDates}</p> : null}
        {period.state?.status === 'loading' && <><p role="status">{words.periodLoading}</p><button onClick={period.reset}>{words.periodCancel}</button></>}
        {period.state?.status === 'partial' && <><p role="status">{words.periodPartial(period.state.checkpoint.scanned)}</p><button onClick={() => void period.run(filters, true)}>{words.periodContinue}</button></>}
        {period.state?.status === 'error' && <><p role="alert">{period.state.denied ? copy.permissionDenied : words.periodError}</p><button onClick={() => void period.run(filters, true)}>{copy.retry}</button></>}
        {period.state?.status === 'invalid' && <p role="alert">{words.periodDates}</p>}
        {period.state?.status === 'complete' && <><p className="ap-usage-summary" data-period-total role="status">{words.periodTotal(formatBalance(period.state.checkpoint.charged, language))}</p>{period.state.checkpoint.unknown > 0 && <p>{words.usageUnknown}</p>}</>}
      </section>
      <p className="ap-usage-summary" role="status">{words.usageSummary(filtered.length, formatBalance(total, language))}</p>
      {filtered.some(record => record.charged_microcredits == null) && <p className="ap-muted">{words.usageUnknown}</p>}
      {filtered.length ? <div className="ap-usage-table-wrap"><table className="ap-usage-table"><thead><tr><th>{words.usageTime}</th><th>{words.usageModel}</th><th>{words.usageCost}</th><th>{words.usageResult}</th><th>{words.usageDetails}</th></tr></thead><tbody>{filtered.map((record, index) => <tr key={record.usage_request_id || record.logical_turn_id || index}>
        <td data-label={words.usageTime}>{formatUsageTime(record.started_at, language)}</td>
        <td data-label={words.usageModel}>{record.model || copy.unknownModel}</td>
        <td data-label={words.usageCost}>{record.charged_microcredits == null ? words.unspecified : formatBalance(record.charged_microcredits, language)}</td>
        <td data-label={words.usageResult}><span className="ap-status" data-status={record.display_status}>{resultLabel(record)}</span></td>
        <td data-label={words.usageDetails}><details><summary>{words.usageDetails}</summary><dl className="ap-fact-list">
          <dt>{copy.usageRecord}</dt><dd><CopyIdentifier value={record.logical_turn_id || record.usage_request_id || ''} language={language} /></dd>
          {record.usage_request_count === 1 && <><dt>{copy.executionState}</dt><dd>{copy.executionStatuses?.[record.execution_status] || record.execution_status || words.unspecified}</dd><dt>{copy.settlementState}</dt><dd>{copy.settlementStatuses?.[record.settlement_status] || record.settlement_status || words.unspecified}</dd></>}
          <dt>{copy.inputTokens}</dt><dd>{record.input_tokens == null ? words.unspecified : record.input_tokens.toLocaleString()}</dd>
          <dt>{copy.outputTokens}</dt><dd>{record.output_tokens == null ? words.unspecified : record.output_tokens.toLocaleString()}</dd>
          <dt>{copy.cachedTokens}</dt><dd>{record.cached_input_tokens == null ? words.unspecified : record.cached_input_tokens.toLocaleString()}</dd>
          {record.completed_at && <><dt>{copy.completedAt}</dt><dd>{formatUsageTime(record.completed_at, language)}</dd></>}
        </dl></details>{record.cancel_usage_request_id && <button disabled={busy || !canCancel} title={!canCancel ? copy.permissionDenied : undefined} onClick={() => onCancel(record.cancel_usage_request_id)}>{copy.stopDelivery}</button>}</td>
      </tr>)}</tbody></table></div> : <p className="ap-empty">{summaries.length ? words.noMatches : copy.noUsage}</p>}
    </>}
    {(hasPrevious || hasMore) && <nav className="ap-usage-pagination" aria-label={copy.usagePagination}><button disabled={busy || !hasPrevious} onClick={onPrevious}>{copy.previousPage}</button><span aria-current="page">{copy.pageNumber(page)}</span><button disabled={busy || !hasMore} onClick={onNext}>{copy.nextPage}</button></nav>}
  </section>;
}
