import { useCallback, useEffect, useRef, useState } from 'react';
import { isCmsNoticeRead, markCmsNoticeRead, noticeReadKey, readCmsNotices, reconcileCmsNotices, saveCmsNotices } from './cms-notice-read.js';

function storage() { try { return globalThis.localStorage; } catch { return undefined; } }

export function useCmsNoticeRead() {
  const current = useRef({ scope: null, read: [] });
  const [, render] = useState(0);
  const [saveFailed, setSaveFailed] = useState(false);
  const changed = useCallback(() => render(n => n + 1), []);
  const observe = useCallback((scope, groups) => {
    const previous = current.current;
    const key = noticeReadKey(scope);
    const read = previous.scope === scope ? previous.read : readCmsNotices(storage(), key) || [];
    const next = reconcileCmsNotices(read, groups);
    current.current = { scope, read: next };
    if (JSON.stringify(next) !== JSON.stringify(read)) setSaveFailed(!saveCmsNotices(storage(), key, next));
    if (previous.scope !== scope || JSON.stringify(previous.read) !== JSON.stringify(next)) {
      if (previous.scope !== scope) setSaveFailed(false);
      changed();
    }
  }, [changed]);
  const dismiss = useCallback(notice => {
    const state = current.current;
    state.read = markCmsNoticeRead(state.read, notice);
    setSaveFailed(!saveCmsNotices(storage(), noticeReadKey(state.scope), state.read));
    changed();
  }, [changed]);
  const reset = useCallback(() => {
    current.current = { scope: null, read: [] }; setSaveFailed(false); changed();
  }, [changed]);
  useEffect(() => {
    const listener = event => {
      if (event.key !== null && event.key !== noticeReadKey(current.current.scope)) return;
      const read = readCmsNotices(storage(), noticeReadKey(current.current.scope));
      if (read) { current.current.read = read; changed(); }
    };
    globalThis.addEventListener?.('storage', listener);
    return () => globalThis.removeEventListener?.('storage', listener);
  }, [changed]);
  return { observe, dismiss, reset, saveFailed, isRead: notice => isCmsNoticeRead(current.current.read, notice) };
}
