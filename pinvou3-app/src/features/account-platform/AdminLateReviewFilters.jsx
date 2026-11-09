import React from 'react';
import { adminLayoutCopy, accountLayoutCopy } from '../../shared/i18n.js';
import { financeReadDefinition } from './admin-finance-read-route.js';
import { localFinanceDate } from './AdminFinanceFilters.jsx';
import './admin-late-review-filters.css';

const fields = financeReadDefinition('late').fields;
const dateKeys = ['created_from', 'created_to', 'entered_from', 'entered_to', 'next_review_before'];
const localDraft = query => Object.fromEntries(Object.entries(query).map(([key, value]) => [key, dateKeys.includes(key) ? localFinanceDate(value) : value]));
const effectiveDate = (query, edge) => query['created_' + edge] || query['entered_' + edge] || '';
const timeDisplay = value => value ? localFinanceDate(value).replace('T', ' ') || value : '';

export function AdminLateReviewFilters({ language, draft, applied, setDraft, onApply, disabled }) {
  const words = adminLayoutCopy[language].financeLists;
  const ui = adminLayoutCopy[language].lateReviewFilters;
  const options = (key, items, labels = null) => <select disabled={disabled} value={draft[key] || ''} onChange={event => setDraft({ ...draft, [key]: event.target.value })}>
    <option value="">{words.allValues}</option>{items.map(item => <option key={item} value={item}>{labels?.[item] || words[item] || accountLayoutCopy[language].statuses[item] || item}</option>)}
  </select>;
  const text = key => <input disabled={disabled} maxLength={key === 'q' ? 160 : 120} value={draft[key] || ''} onChange={event => setDraft({ ...draft, [key]: event.target.value })} />;
  const changeBoundary = (edge, value) => setDraft({ ...draft, ['created_' + edge]: value, ['entered_' + edge]: '' });
  const date = (value, update) => <input disabled={disabled} type="datetime-local" step="0.001" value={value || ''} onChange={event => update(event.target.value)} />;
  const setBefore = value => setDraft({ ...draft, next_review_before: value });
  const previous = localDraft(applied);
  const submit = event => {
    event.preventDefault();
    const next = { ...draft };
    for (const key of dateKeys) if (next[key] === previous[key]) next[key] = applied[key];
    onApply(next);
  };
  const clear = () => onApply({ ...Object.fromEntries(fields.map(key => [key, ''])), limit: applied.limit });
  const toLabel = value => words[value] || accountLayoutCopy[language].statuses[value] || value;
  const timezone = Intl.DateTimeFormat().resolvedOptions().timeZone || ui.deviceTime;
  const dateLabel = value => value ? timeDisplay(value) + ' ' + timezone : '';
  const advanced = [
    [words.q, applied.q], [words.severity, toLabel(applied.severity)], [words.provider_id, applied.provider_id],
    [ui.from, dateLabel(effectiveDate(applied, 'from'))], [ui.to, dateLabel(effectiveDate(applied, 'to'))],
    [ui.review + ' · ' + ui.before, dateLabel(applied.next_review_before)],
  ].filter(([, value]) => value);
  const hasApplied = Boolean(applied.status || applied.owner_id || ['true', 'false'].includes(applied.overdue) || advanced.length);
  const changed = fields.some(key => (draft[key] || '') !== (previous[key] || '')) || draft.limit !== applied.limit;
  return <form className="adm-late-filter" onSubmit={submit}>
    <div className="adm-late-filter-primary">
      <label>{words.status}{options('status', financeReadDefinition('late').statuses)}</label>
      <label>{words.owner_id}{text('owner_id')}<small>{ui.ownerHint}</small></label>
      <label className="adm-late-filter-deadline">{words.overdue}{options('overdue', ['true', 'false'], ui.overdueOptions)}</label>
    </div>
    {advanced.length > 0 && <p className="adm-late-filter-active"><strong>{ui.applied}:</strong>{' '}{advanced.map(([key, value]) => key + ' ' + value).join(' · ')}</p>}
    <details className="adm-late-filter-advanced">
      <summary>{ui.advanced}</summary>
      <div className="adm-late-filter-secondary">
        <label>{words.q}{text('q')}<small>{ui.queryHint}</small></label>
        <label>{words.severity}{options('severity', ['P0', 'P1', 'P2'])}</label>
        <label>{words.provider_id}{text('provider_id')}</label>
      </div>
      <fieldset>
        <legend>{ui.created}</legend>
        <p>{ui.dateHint} {timezone}</p>
        <div className="adm-late-filter-secondary">
          <label>{ui.from}{date(effectiveDate(draft, 'from'), value => changeBoundary('from', value))}</label>
          <label>{ui.to}{date(effectiveDate(draft, 'to'), value => changeBoundary('to', value))}</label>
        </div>
      </fieldset>
      <fieldset>
        <legend>{ui.review}</legend>
        <div className="adm-late-filter-secondary"><label>{ui.before}{date(draft.next_review_before, setBefore)}</label></div>
      </fieldset>
      <div className="adm-late-filter-secondary"><label>{words.limit}<select disabled={disabled} value={draft.limit} onChange={event => setDraft({ ...draft, limit: event.target.value })}>{['25', '50', '100', '200'].map(item => <option key={item}>{item}</option>)}</select></label></div>
    </details>
    <div className="adm-row-actions">
      <button type="submit" disabled={disabled}>{words.search}</button>
      {hasApplied && <button type="button" disabled={disabled} onClick={clear}>{ui.clear}</button>}
      {changed && <button type="button" disabled={disabled} onClick={() => setDraft(previous)}>{ui.cancelDraft}</button>}
    </div>
  </form>;
}
