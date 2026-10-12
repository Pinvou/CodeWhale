import React from 'react';
import { formatUsagePoints } from './credit-units.js';
import { summarizeUsageRecords, usageRecordReference } from './usage-summary.js';
function Empty({ children }) { return <div className="ap-empty">{children}</div>; }

function formatUsageTime(value, language) {
  const locale = { zh: 'zh-CN', en: 'en-US', ja: 'ja-JP' }[language] || language;
  const date = new Date(value);
  return Number.isNaN(date.getTime())
    ? '—'
    : new Intl.DateTimeFormat(locale, {
        dateStyle: 'medium',
        timeStyle: 'short',
      }).format(date);
}

export function Usage({ copy, language, records, unavailable, page, hasPrevious, hasMore, busy, canCancel = true, onPrevious, onNext, onCancel, embedded = false }) {
  const Heading = embedded ? 'h3' : 'h2';
  const summaries = summarizeUsageRecords(records);
  const header = (
    <>
      {!embedded && <p className="ap-eyebrow">{copy.sections.usage}</p>}
      <Heading>{copy.usageTitle}</Heading>
      <p className="ap-lead">{copy.usageLead}</p>
    </>
  );
  if (unavailable)
    return (
      <section className={embedded ? 'ap-combined-section' : undefined}>
        {header}
        <div role="alert" className="ap-alert error">
          {copy.usageUnavailable}
        </div>
      </section>
    );
  return (
    <section className={embedded ? 'ap-combined-section' : undefined}>
      {header}
      {summaries.length ? (
        <div className="ap-usage-list">
          {summaries.map((record, index) => {
            const consumed = record.charged_microcredits == null ? '—' : formatUsagePoints(record.charged_microcredits, language);
            const statusClass = String(record.display_status || '')
              .toLowerCase()
              .replaceAll('_', '-');
            const tokenKnown = record.input_tokens != null && record.output_tokens != null;
            const canStop = Boolean(record.cancel_usage_request_id);
            return (
              <article className="ap-usage-row" key={record.logical_turn_id || record.usage_request_id || `${record.started_at}-${record.model}-${index}`}>
                <div>
                  <strong>{record.model || copy.unknownModel}</strong>
                  <small>{formatUsageTime(record.started_at, language)}</small>
                  {tokenKnown && (
                    <small className="ap-usage-tokens">
                      {copy.inputTokens}: {record.input_tokens.toLocaleString()} · {copy.outputTokens}: {record.output_tokens.toLocaleString()} · {copy.cachedTokens}: {(record.cached_input_tokens || 0).toLocaleString()}
                    </small>
                  )}
                  <small title={record.logical_turn_id || record.usage_request_id}>
                    {copy.usageRecord}: {usageRecordReference(record)}
                  </small>
                </div>
                <div>
                  <strong className="ap-usage-amount">
                    {consumed} {copy.points}
                  </strong>
                  <span className={`ap-status ap-usage-status-${statusClass}`}>{copy.usageStatuses[record.display_status] || record.display_status}</span>
                  {record.usage_request_count === 1 && (
                    <small>
                      {copy.executionState}: {copy.executionStatuses?.[record.execution_status] || record.execution_status} · {copy.settlementState}: {copy.settlementStatuses?.[record.settlement_status] || record.settlement_status}
                    </small>
                  )}
                  {canStop && (
                    <button disabled={busy || !canCancel} title={!canCancel ? copy.permissionDenied : undefined} onClick={() => onCancel(record.cancel_usage_request_id)}>
                      {copy.stopDelivery}
                    </button>
                  )}
                  {record.completed_at && (
                    <small>
                      {copy.completedAt}: {formatUsageTime(record.completed_at, language)}
                    </small>
                  )}
                </div>
              </article>
            );
          })}
          {(hasPrevious || hasMore) && (
            <nav className="ap-usage-pagination" aria-label={copy.usagePagination}>
              <button type="button" disabled={busy || !hasPrevious} onClick={onPrevious}>
                {copy.previousPage}
              </button>
              <span aria-current="page">{copy.pageNumber(page)}</span>
              <button type="button" disabled={busy || !hasMore} onClick={onNext}>
                {copy.nextPage}
              </button>
            </nav>
          )}
        </div>
      ) : (
        <Empty>{copy.noUsage}</Empty>
      )}
    </section>
  );
}
