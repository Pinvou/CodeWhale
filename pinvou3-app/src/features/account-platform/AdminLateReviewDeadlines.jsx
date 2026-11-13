import React from 'react';
import { adminLayoutCopy } from '../../shared/i18n.js';
import './admin-late-review-deadlines.css';

const locale = { zh: 'zh-CN', en: 'en-GB', ja: 'ja-JP' };
const timeZone = 'Asia/Shanghai';
const dateOf = value => typeof value === 'string' && /^\d{4}-\d{2}-\d{2}T/.test(value) && !value.startsWith('0001-01-01') && Number.isFinite(Date.parse(value)) ? new Date(value) : null;
const isOpen = value => ['REVIEW_REQUIRED_LATE_PAYMENT', 'PROVIDER_BLIND'].includes(value.review_status) && !value.resolved_at;
const display = (value, language) => new Intl.DateTimeFormat(locale[language], { timeZone, year:'numeric', month:'2-digit', day:'2-digit', hour:'2-digit', minute:'2-digit', second:'2-digit', hourCycle:'h23' }).format(value);
const at = (value, language) => {
  const date = dateOf(value);
  return date ? <time dateTime={value} dir="ltr">{display(date, language)}</time> : adminLayoutCopy[language].lateReviewDeadlines.missing;
};
const relative = (value, reference, words) => {
  const date = dateOf(value);
  if (!date || !reference) return '';
  const delta = date.valueOf() - reference.valueOf();
  if (delta === 0) return words.now;
  const minutes = Math.ceil(Math.abs(delta) / 60000);
  return (delta > 0 ? words.until : words.past).replace('{minutes}', String(minutes));
};

export function AdminLateReviewDeadlines({ value, language, snapshot, detailed = false }) {
  const words = adminLayoutCopy[language].lateReviewDeadlines;
  const reference = !detailed && isOpen(value) ? dateOf(snapshot) : null;
  const primary = [
    ['claim', value.claim_due_at, value.claimed_at],
    ['review', value.next_review_at, null],
    ['resolve', value.resolution_due_at, value.resolved_at],
  ];
  const rows = detailed ? [primary[0], ['firstResponse', value.first_response_due_at, value.first_response_at], ['mitigate', value.mitigation_due_at, null], ...primary.slice(1)] : primary;
  return <dl className="adm-late-deadlines">
    {rows.map(([key, deadline, completed]) => {
      const confirmed = dateOf(completed);
      const hint = confirmed && !detailed ? words.completed : !completed && reference ? relative(deadline, reference, words) : '';
      return <div key={key}><dt>{words[key]}</dt><dd>{at(deadline, language)}{hint && <small>{hint}</small>}</dd></div>;
    })}
    {detailed && <div><dt>{words.zoneLabel}</dt><dd>{words.zone}</dd></div>}
  </dl>;
}

export function AdminLateReviewDeadlineTools({ language, query, snapshot, disabled, onChange }) {
  const words = adminLayoutCopy[language].lateReviewDeadlines;
  const reference = dateOf(snapshot);
  return <section className="adm-late-deadline-tools" aria-label={words.title}>
    <div className="adm-row-actions">
      <button type="button" disabled={disabled} aria-pressed={query.overdue === 'true'} onClick={() => onChange({ ...query, overdue: 'true' })}>{words.overdue}</button>
      <button type="button" disabled={disabled} onClick={() => onChange({ ...query, next_review_before: new Date(Date.now() + 86400000).toISOString() })}>{words.reviewUpperShortcut}</button>
    </div>
    <p>{words.boundary}{query.next_review_before && <> {words.currentUpper} {at(query.next_review_before, language)} {words.zone}</>}</p>
    <p>{words.zoneLabel}: {words.zone}.{reference && <> {words.reference} {at(snapshot, language)}. {words.referenceHint}</>}</p>
  </section>;
}
