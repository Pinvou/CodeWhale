import { statusLabel, tokenStatusLabel } from './status-label.js';
import React, { useEffect, useRef, useState } from 'react';
import { previousAdminModelTokenCursor, rememberAdminModelTokenNextCursor, synchronizeAdminModelTokenCursorHistory } from './admin-navigation.js';
import { executeAdminSourceRequest, initialAdminSourceState, reduceAdminSourceState, visibleAdminSourceState } from './admin-model-token-state.js';

export function useAdminSource({ enabled, load, query, refreshVersion, trackReadAccess = false }) {
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
      transition: (event) => setState(previous => {
        const next = reduceAdminSourceState(previous, event);
        if (!trackReadAccess || next === previous) return next;
        if (event.type === 'failure' && [401, 403].includes(event.error?.status)) return { ...next, readDenied: true };
        if (event.type === 'success') return { ...next, readDenied: false };
        return next;
      }),
    });
    return () => controller.abort();
  }, [enabled, load, queryKey, refreshVersion, trackReadAccess]);
  return visibleAdminSourceState(state, queryKey, enabled);
}

function sourceCursorHistory(cursorHistory, source, query) {
  const history = synchronizeAdminModelTokenCursorHistory(cursorHistory.current[source], query);
  cursorHistory.current[source] = history;
  return history;
}

export function Status({ value, copy }) {
  const label = tokenStatusLabel(value, copy.statusLanguage);
  const normalized = typeof value === 'string' ? value : '';
  const tone = ['ACTIVE', 'READY', 'PUBLISHED', 'HEALTHY', 'SETTLED', 'SUCCEEDED'].includes(normalized)
    ? 'ok'
    : ['PENDING', 'REPAIR_REQUIRED', 'DEGRADED', 'SETTLEMENT_PENDING'].includes(normalized) ? 'warn' : '';
  return <span className={`adm-status ${tone}`}>{label}</span>;
}

function adminRetrySecondsRemaining(retryAt) {
  return Math.max(0, Math.ceil((Number(retryAt || 0) - Date.now()) / 1000));
}

export function useAdminRetryCountdown(retryAt) {
  const [remaining, setRemaining] = useState(() => adminRetrySecondsRemaining(retryAt));
  useEffect(() => {
    let interval;
    const update = () => {
      const nextRemaining = adminRetrySecondsRemaining(retryAt);
      setRemaining(nextRemaining);
      if (nextRemaining === 0 && interval) {
        globalThis.clearInterval(interval);
        interval = undefined;
      }
    };
    update();
    if (!retryAt || Number(retryAt) <= Date.now()) return undefined;
    interval = globalThis.setInterval(update, 250);
    return () => {
      if (interval) globalThis.clearInterval(interval);
    };
  }, [retryAt]);
  return remaining;
}

export function SourceHeader({ title, titleID, state, copy, onRetry, actions = null, actionClassName = '' }) {
  const retryRemaining = useAdminRetryCountdown(state.retryAt);
  return (
    <div className="adm-source-head">
      <div>
        <h2 id={titleID}>{title}</h2>
        <p>{state.updatedAt ? `${copy.lastUpdated} ${new Date(state.updatedAt).toLocaleString()}` : copy.notUpdated}</p>
      </div>
      <div className={('adm-row-actions ' + actionClassName).trim()}>{actions}<button type="button" disabled={state.loading || retryRemaining > 0} onClick={onRetry}>{state.loading ? copy.loading : retryRemaining > 0 ? copy.retryCountdown(retryRemaining) : copy.refreshSource}</button></div>
    </div>
  );
}

export function SourceFeedback({ state, copy, onRetry, messages = null }) {
  const retryRemaining = useAdminRetryCountdown(state.retryAt);
  if (state.error) {
    const expired = state.error?.status === 410;
    const rateLimited = state.error?.status === 429;
    const denied = messages?.denied && [401, 403].includes(state.error?.status);
    const details = [
      retryRemaining > 0 ? copy.retryAfter(retryRemaining) : '',
      state.error.requestID ? `${copy.requestID}: ${state.error.requestID}` : '',
    ].filter(Boolean).join(' · ') || copy.retryHelp;
    return (
      <div className="adm-source-error" role="alert">
        <strong>{denied ? messages.denied : expired ? copy.cursorExpired : rateLimited ? copy.rateLimited : copy.sourceUnavailable}</strong>
        <span>{details}{!denied && messages?.stale && state.items.length > 0 ? ' · ' + messages.stale : ''}</span>
        <button type="button" disabled={retryRemaining > 0} onClick={onRetry}>{retryRemaining > 0 ? copy.retryCountdown(retryRemaining) : copy.retry}</button>
      </div>
    );
  }
  if (state.loading && state.items.length === 0) return <div className="adm-source-state" role="status">{copy.loading}</div>;
  if (!state.loading && state.items.length === 0) return <div className="adm-source-state" role={messages ? 'status' : undefined}>{messages?.empty || copy.noResults}</div>;
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

export function FilterForm({ source, query, fields, copy, onQueryChange, resetDraftOnClear = true }) {
  const [draft, setDraft] = useState(query);
  const id = React.useId();
  const update = (key, value) => setDraft(current => ({ ...current, [key]: value }));
  const clear = () => {
    const empty = Object.fromEntries(Object.keys(query).map(key => [key, '']));
    if (resetDraftOnClear) setDraft(empty);
    onQueryChange(source, empty);
  };
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
      {fields.map((field) => field.help ? <div className="adm-catalog-code-filter" key={field.key}>
        <label><span>{field.label}</span><input type="search" aria-describedby={id + '-' + field.key} value={fieldValue(field)} onChange={event => update(field.key, event.target.value)} /></label>
        <small id={id + '-' + field.key}>{field.help}</small>
        {field.suggestions?.length > 0 && <details><summary>{field.suggestionsCopy.knownProtocols}</summary>
          {field.suggestions.some(code => code.toLowerCase().includes(fieldValue(field).trim().toLowerCase())) ?
            <ul>{field.suggestions.filter(code => code.toLowerCase().includes(fieldValue(field).trim().toLowerCase())).map(code => <li key={code}><button type="button" onClick={() => update(field.key, code)}>{code}</button></li>)}</ul> :
            <p role="status">{field.suggestionsCopy.noKnownProtocols}</p>}
        </details>}
      </div> : (
        <label key={field.key}>
          <span>{field.label}</span>
          {field.options ? (
            <select value={draft[field.key] || ''} onChange={(event) => setDraft((current) => ({ ...current, [field.key]: event.target.value }))}>
              <option value="">{copy.all}</option>
              {field.options.map((option) => <option key={option} value={option}>{statusLabel(option, copy.statusLanguage, copy.enumLabels)}</option>)}
            </select>
          ) : (
            <input type={field.type || 'search'} value={fieldValue(field)} placeholder={field.placeholder || ''} onChange={(event) => setDraft((current) => ({ ...current, [field.key]: event.target.value }))} />
          )}
        </label>
      ))}
      <button type="submit">{copy.search}</button>
      <button type="button" className="secondary" onClick={clear}>{copy.clear}</button>
    </form>
  );
}
