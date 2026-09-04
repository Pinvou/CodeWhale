import React, { useEffect, useRef, useState } from 'react';
import { previousAdminModelTokenCursor, rememberAdminModelTokenNextCursor, synchronizeAdminModelTokenCursorHistory } from './admin-navigation.js';
import { executeAdminSourceRequest, initialAdminSourceState, reduceAdminSourceState, visibleAdminSourceState } from './admin-model-token-state.js';

export function useAdminSource({ enabled, load, query, refreshVersion }) {
  const [state, setState] = useState(initialAdminSourceState);
  const generation = useRef(0);
  const queryKey = JSON.stringify(query);
  useEffect(() => {
    if (!enabled) return undefined;
    const controller = new globalThis.AbortController();
    const requestID = ++generation.current;
    void executeAdminSourceRequest({
      requestID,
      queryKey,
      query,
      load,
      signal: controller.signal,
      transition: (event) => setState((previous) => reduceAdminSourceState(previous, event)),
    });
    return () => controller.abort();
  }, [enabled, load, queryKey, refreshVersion]);
  return visibleAdminSourceState(state, queryKey, enabled);
}

function sourceCursorHistory(cursorHistory, source, query) {
  const history = synchronizeAdminModelTokenCursorHistory(cursorHistory.current[source], query);
  cursorHistory.current[source] = history;
  return history;
}

export function Status({ value, copy }) {
  const label = copy.enumLabels?.[value] || value || '—';
  const normalized = String(value || '').toUpperCase();
  const tone = ['ACTIVE', 'READY', 'PUBLISHED', 'HEALTHY', 'SETTLED', 'SUCCEEDED'].includes(normalized)
    ? 'ok'
    : ['PENDING', 'REPAIR_REQUIRED', 'DEGRADED', 'SETTLEMENT_PENDING'].includes(normalized) ? 'warn' : '';
  return <span className={`adm-status ${tone}`}>{label}</span>;
}

export function SourceHeader({ title, titleID, state, copy, onRetry, actions = null }) {
  return (
    <div className="adm-source-head">
      <div>
        <h2 id={titleID}>{title}</h2>
        <p>{state.updatedAt ? `${copy.lastUpdated} ${new Date(state.updatedAt).toLocaleString()}` : copy.notUpdated}</p>
      </div>
      <div className="adm-row-actions">{actions}<button type="button" disabled={state.loading} onClick={onRetry}>{state.loading ? copy.loading : copy.refreshSource}</button></div>
    </div>
  );
}

export function SourceFeedback({ state, copy, onRetry }) {
  if (state.error) {
    const expired = state.error?.status === 410;
    return (
      <div className="adm-source-error" role="alert">
        <strong>{expired ? copy.cursorExpired : copy.sourceUnavailable}</strong>
        <span>{state.error.requestID ? `${copy.requestID}: ${state.error.requestID}` : copy.retryHelp}</span>
        <button type="button" onClick={onRetry}>{copy.retry}</button>
      </div>
    );
  }
  if (state.loading && state.items.length === 0) return <div className="adm-source-state" role="status">{copy.loading}</div>;
  if (!state.loading && state.items.length === 0) return <div className="adm-source-state">{copy.noResults}</div>;
  return null;
}

export function Pagination({ source, query, state, copy, cursorHistory, onQueryChange }) {
  if (!state.page || state.error) return null;
  const history = sourceCursorHistory(cursorHistory, source, query);
  const previous = previousAdminModelTokenCursor(history);
  const goNext = () => {
    cursorHistory.current[source] = rememberAdminModelTokenNextCursor(history, state.page.nextCursor);
    onQueryChange(source, { ...query, cursor: state.page.nextCursor });
  };
  const goPrevious = () => {
    cursorHistory.current[source] = { ...history, currentCursor: previous.cursor };
    onQueryChange(source, { ...query, cursor: previous.cursor });
  };
  return (
    <div className="adm-pagination adm-model-token-pagination">
      <span>{copy.pageCount(state.items.length, state.page.hasMore)}</span>
      {state.page.asOf && <small>{copy.snapshotAt} {new Date(state.page.asOf).toLocaleString()}</small>}
      <div className="adm-row-actions">
        <button type="button" disabled={state.loading || !previous.available} onClick={goPrevious}>{copy.previous}</button>
        <button type="button" disabled={state.loading || !state.page.nextCursor} onClick={goNext}>{copy.next}</button>
      </div>
    </div>
  );
}

export function FilterForm({ source, query, fields, copy, onQueryChange }) {
  const [draft, setDraft] = useState(query);
  useEffect(() => setDraft(query), [JSON.stringify(query)]);
  const fieldValue = (field) => {
    const value = draft[field.key] || '';
    if (!value || field.type !== 'datetime-local') return value;
    const parsed = new Date(value);
    if (Number.isNaN(parsed.getTime())) return '';
    return new Date(parsed.getTime() - parsed.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
  };
  const submitted = () => Object.fromEntries(Object.entries(draft).map(([key, value]) => {
    const field = fields.find((candidate) => candidate.key === key);
    if (!value || field?.type !== 'datetime-local') return [key, value];
    const parsed = new Date(value);
    return [key, Number.isNaN(parsed.getTime()) ? value : parsed.toISOString()];
  }));
  return (
    <form className="adm-model-token-filters" onSubmit={(event) => {
      event.preventDefault();
      onQueryChange(source, { ...submitted(), cursor: '' });
    }}>
      {fields.map((field) => (
        <label key={field.key}>
          <span>{field.label}</span>
          {field.options ? (
            <select value={draft[field.key] || ''} onChange={(event) => setDraft((current) => ({ ...current, [field.key]: event.target.value }))}>
              <option value="">{copy.all}</option>
              {field.options.map((option) => <option key={option} value={option}>{copy.enumLabels?.[option] || option}</option>)}
            </select>
          ) : (
            <input type={field.type || 'search'} value={fieldValue(field)} placeholder={field.placeholder || ''} onChange={(event) => setDraft((current) => ({ ...current, [field.key]: event.target.value }))} />
          )}
        </label>
      ))}
      <button type="submit">{copy.search}</button>
      <button type="button" className="secondary" onClick={() => onQueryChange(source, Object.fromEntries(Object.keys(query).map((key) => [key, ''])))}>{copy.clear}</button>
    </form>
  );
}
