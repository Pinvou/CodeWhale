import React from 'react';
import { previousAdminModelTokenCursor, rememberAdminModelTokenNextCursor, synchronizeAdminModelTokenCursorHistory } from './admin-navigation.js';

export function TokenResourceFeedback({ state, copy, onRetry }) {
  if (state.error) return <div className="adm-source-error" role="alert"><strong>{state.error.status === 410 ? copy.cursorExpired : copy.tokenResourceSourceErrors?.[state.error.code] || copy.sourceUnavailable}</strong>{state.error.requestID && <span>{copy.requestID}: {state.error.requestID}</span>}<button type="button" onClick={onRetry}>{copy.retry}</button></div>;
  if (state.loading && !state.items.length) return <div className="adm-source-state" role="status">{copy.loading}</div>;
  if (!state.loading && !state.items.length) return <div className="adm-source-state">{copy.noResults}</div>;
  return null;
}

export function TokenResourcePaging({ source, query, state, copy, histories, onChange }) {
  if (!state.page || state.error) return null;
  const history = synchronizeAdminModelTokenCursorHistory(histories.current[source], query);
  histories.current[source] = history;
  const previous = previousAdminModelTokenCursor(history);
  return <div className="adm-pagination adm-model-token-pagination"><span>{copy.pageCount(state.items.length, state.page.hasMore)}</span><div className="adm-row-actions">
    <button type="button" disabled={state.loading || !previous.available} onClick={() => { histories.current[source] = { ...history, currentCursor: previous.cursor }; onChange({ ...query, cursor: previous.cursor }); }}>{copy.previous}</button>
    <button type="button" disabled={state.loading || !state.page.nextCursor} onClick={() => { histories.current[source] = rememberAdminModelTokenNextCursor(history, state.page.nextCursor); onChange({ ...query, cursor: state.page.nextCursor }); }}>{copy.next}</button>
  </div></div>;
}
