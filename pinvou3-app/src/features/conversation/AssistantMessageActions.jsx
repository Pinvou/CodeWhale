import React, { useCallback, useEffect, useId, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import {
  Check,
  ChevronDown,
  Code,
  Copy,
  Download,
  FileText,
  MessageCircle,
  Share2,
  RotateCcw,
  ThumbsDown,
  ThumbsUp,
  X,
} from '../../components/icons.jsx';
import { invokeTauri } from '../../platform/tauri/client.js';
import { isWeb } from '../../shared/platform.js';
import {
  openAssistantShareTarget,
  saveAssistantResponseFile,
  shareAssistantResponseWithSystem,
} from './assistant-response-save.js';
import {
  assistantExportFilename,
  buildAssistantResponseExport,
} from './assistant-response-export.js';
import { copyClipboardText, normalizeAssistantMessageText } from './message-clipboard.js';

const SHARE_TARGETS = Object.freeze(['wechat', 'wecom', 'feishu', 'dingtalk', 'qq']);
const responseRatings = new Map();

function telemetryCommand() {
  return isWeb ? 'web_access_track_behavior_event' : 'track_behavior_event';
}

function responseContentType(value) {
  if (/```[\s\S]*```/.test(value)) return 'code';
  if (/^\s*\|.+\|\s*\r?\n\s*\|(?:\s*:?-+:?\s*\|)+/m.test(value)) return 'table';
  return 'text';
}

function menuPosition(trigger, width, height) {
  const rect = trigger.getBoundingClientRect();
  const viewportWidth = window.innerWidth;
  const viewportHeight = window.innerHeight;
  const left = Math.max(6, Math.min(rect.left, viewportWidth - width - 6));
  const below = rect.bottom + 6;
  const top = below + height <= viewportHeight - 6
    ? below
    : Math.max(6, rect.top - height - 6);
  return { left, top, width };
}

export function AssistantMessageFooter({ children }) {
  return (
    <div data-testid="assistant-message-footer" className="!mt-0 flex min-h-8 flex-wrap items-center gap-x-2 gap-y-1 pt-2">
      {children}
    </div>
  );
}

export function AssistantMessageActions({ text, resolveText, copy, sessionId, turnId, telemetryTurnId, onRetry }) {
  const instanceId = useId().replace(/:/g, '');
  const [copyStatus, setCopyStatus] = useState('idle');
  const [menu, setMenu] = useState(null);
  const [actionFeedback, setActionFeedback] = useState(null);
  const feedbackKey = sessionId && turnId ? `${sessionId}\u001f${turnId}` : '';
  const canSubmitFeedback = Boolean(feedbackKey && telemetryTurnId);
  const [rating, setRating] = useState(() => responseRatings.get(feedbackKey) || null);
  const resetTimerRef = useRef(null);
  const exportTriggerRef = useRef(null);
  const shareTriggerRef = useRef(null);
  const menuItemsRef = useRef([]);
  const triggerIds = {
    export: `assistant-message-export-trigger-${instanceId}`,
    share: `assistant-message-share-trigger-${instanceId}`,
  };
  const menuIds = {
    export: `assistant-message-export-menu-${instanceId}`,
    share: `assistant-message-share-menu-${instanceId}`,
  };
  const copyLabel = copyStatus === 'copied'
    ? copy.copyReplySuccess
    : copyStatus === 'failed'
      ? copy.copyReplyFailed
      : copy.copyReply;

  const clearResetTimer = useCallback(() => {
    if (!resetTimerRef.current) return;
    clearTimeout(resetTimerRef.current);
    resetTimerRef.current = null;
  }, []);

  const resetStatusLater = useCallback(() => {
    clearResetTimer();
    resetTimerRef.current = setTimeout(() => {
      resetTimerRef.current = null;
      setCopyStatus('idle');
      setActionFeedback(null);
    }, 2400);
  }, [clearResetTimer]);

  useEffect(() => clearResetTimer, [clearResetTimer]);

  const restoreTriggerFocus = useCallback(kind => {
    window.requestAnimationFrame(() => {
      const trigger = kind === 'export' ? exportTriggerRef.current : shareTriggerRef.current;
      trigger?.focus({ preventScroll: true });
    });
  }, []);

  const closeMenu = useCallback((kind, restoreFocus = false) => {
    setMenu(current => current?.kind === kind ? null : current);
    if (restoreFocus) restoreTriggerFocus(kind);
  }, [restoreTriggerFocus]);

  useEffect(() => {
    if (!menu) return undefined;
    const frame = window.requestAnimationFrame(() => {
      menuItemsRef.current[0]?.focus({ preventScroll: true });
    });
    return () => window.cancelAnimationFrame(frame);
  }, [menu]);

  useEffect(() => {
    if (!menu) return undefined;
    const close = event => {
      if (event?.target?.closest?.('[data-assistant-message-action-menu]')) return;
      if (exportTriggerRef.current?.contains(event?.target)) return;
      if (shareTriggerRef.current?.contains(event?.target)) return;
      closeMenu(menu.kind);
    };
    const onKey = event => {
      if (event.key !== 'Escape') return;
      event.preventDefault();
      closeMenu(menu.kind, true);
    };
    document.addEventListener('mousedown', close, true);
    document.addEventListener('keydown', onKey);
    window.addEventListener('resize', close);
    window.addEventListener('scroll', close, true);
    return () => {
      document.removeEventListener('mousedown', close, true);
      document.removeEventListener('keydown', onKey);
      window.removeEventListener('resize', close);
      window.removeEventListener('scroll', close, true);
    };
  }, [closeMenu, menu]);

  // resolveText 可能返回 Promise(旧 HTML 会话的 turndown 懒加载转换);
  // 消费方都在 async 处理器里,统一 await。
  const responseText = async () => normalizeAssistantMessageText(
    typeof resolveText === 'function' ? await resolveText() : text,
  );

  const showFeedback = (message, failed = false) => {
    setCopyStatus('idle');
    setActionFeedback({ message, failed });
    resetStatusLater();
  };

  const copyResponseToClipboard = async value => {
    const copied = await copyClipboardText(value);
    if (copied && sessionId && telemetryTurnId) {
      invokeTauri(telemetryCommand(), { request: {
        eventName: 'response_copied',
        sessionId,
        turnId: telemetryTurnId,
        contentType: responseContentType(value),
      } }).catch(() => {});
    }
    return copied;
  };

  const handleCopy = async () => {
    let copied = false;
    let value = '';
    try {
      value = await responseText();
      copied = await copyResponseToClipboard(value);
    } catch {
      copied = false;
    }
    setActionFeedback(null);
    setCopyStatus(copied ? 'copied' : 'failed');
    resetStatusLater();
  };

  const changeRating = nextRating => {
    if (!canSubmitFeedback) return;
    const clearing = rating === nextRating;
    const next = clearing ? null : nextRating;
    if (next) responseRatings.set(feedbackKey, next);
    else responseRatings.delete(feedbackKey);
    setRating(next);
    if (sessionId && telemetryTurnId) {
      invokeTauri(telemetryCommand(), { request: {
        eventName: 'response_feedback_changed',
        sessionId,
        turnId: telemetryTurnId,
        feedbackAction: clearing ? 'clear' : 'set',
        ...(clearing ? {} : { rating: nextRating }),
      } }).catch(() => {});
    }
  };

  const openMenu = (kind, event) => {
    event.stopPropagation();
    const dimensions = kind === 'export' ? [236, 126] : [260, 320];
    const position = menuPosition(event.currentTarget, ...dimensions);
    setMenu(current => current?.kind === kind
      ? null
      : { kind, ...position });
  };

  const handleMenuKeyDown = event => {
    if (event.key === 'Tab') {
      closeMenu(menu.kind);
      return;
    }
    if (event.key === 'Escape') {
      event.preventDefault();
      event.stopPropagation();
      closeMenu(menu.kind, true);
      return;
    }
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
    const items = menuItemsRef.current.filter(item => item && !item.disabled);
    if (!items.length) return;
    event.preventDefault();
    const activeIndex = items.indexOf(document.activeElement);
    let nextIndex;
    if (event.key === 'Home') nextIndex = 0;
    else if (event.key === 'End') nextIndex = items.length - 1;
    else if (event.key === 'ArrowDown') nextIndex = (activeIndex + 1) % items.length;
    else nextIndex = (activeIndex <= 0 ? items.length : activeIndex) - 1;
    items[nextIndex].focus({ preventScroll: true });
  };

  const handleExport = async format => {
    closeMenu('export', true);
    try {
      const generated = buildAssistantResponseExport(await responseText(), format, {
        title: copy.exportReplyTitle,
        language: document.documentElement.lang || 'zh-CN',
      });
      const saved = await saveAssistantResponseFile({
        ...generated,
        format,
        filename: assistantExportFilename(format),
      });
      if (saved) showFeedback(copy.exportSuccess);
    } catch (error) {
      const tooLarge = String(error?.message || error).includes('assistant_export_too_large');
      showFeedback(tooLarge ? copy.exportTooLarge : copy.exportFailed, true);
    }
  };

  const copyForShare = async () => copyResponseToClipboard(await responseText());

  const handleSystemShare = async () => {
    closeMenu('share', true);
    let value = null;
    try {
      value = await responseText();
      const result = await shareAssistantResponseWithSystem({
        title: copy.shareReplyTitle,
        text: value,
      });
      if (result === 'cancelled') return;
      if (result === 'shared') {
        showFeedback(copy.shareSuccess);
        return;
      }
      const copied = await copyResponseToClipboard(value);
      showFeedback(copied ? copy.shareUnavailable : copy.shareFailed, !copied);
    } catch {
      // responseText 可能因 turndown chunk 拉取失败 reject——此时 catch 里再调
      // copyForShare() 会二次 reject 逃出 catch(unhandled rejection 且无反馈)。
      // 只复用 try 里已解析成功的 value 复制;连 value 都没有时如实报分享失败。
      try {
        const copied = value !== null ? await copyResponseToClipboard(value) : false;
        showFeedback(copied ? copy.shareUnavailable : copy.shareFailed, !copied);
      } catch {
        showFeedback(copy.shareFailed, true);
      }
    }
  };

  const handleAppShare = async target => {
    closeMenu('share', true);
    const appLabel = copy.shareTargets[target];
    try {
      const copied = await copyForShare();
      if (!copied) {
        showFeedback(copy.shareFailed, true);
        return;
      }
      let opened = false;
      try {
        opened = await openAssistantShareTarget(target);
      } catch {}
      showFeedback(opened ? copy.shareCopiedOpen(appLabel) : copy.shareCopiedWeb(appLabel));
    } catch {
      showFeedback(copy.shareFailed, true);
    }
  };

  const actionClass = 'inline-flex h-8 items-center gap-1.5 rounded-lg px-2 text-[12px] text-[#747775] transition-colors hover:bg-black/[0.06] hover:text-[#1F1F1F] focus:outline-none focus-visible:ring-2 focus-visible:ring-[#0A84FF]/50 dark:text-[#9AA0A6] dark:hover:bg-white/10 dark:hover:text-[#E3E3E3]';
  const menuClass = 'overflow-hidden rounded-xl border border-black/10 bg-white py-1 shadow-xl dark:border-white/10 dark:bg-[#2B2C2F]';
  const menuItemClass = 'flex min-h-11 w-full items-center gap-2.5 px-3 py-2 text-left text-[13px] text-[#1F1F1F] transition-colors hover:bg-black/[0.06] focus:bg-black/[0.06] focus:outline-none dark:text-[#E3E3E3] dark:hover:bg-white/10 dark:focus:bg-white/10';

  return (
    <>
      <div data-testid="assistant-message-actions" className="flex items-center gap-1">
        <button
          type="button"
          data-testid="assistant-message-copy"
          onClick={handleCopy}
          title={copyLabel}
          aria-label={copyLabel}
          className={`${actionClass} ${copyStatus === 'failed' ? '!bg-red-500/[0.08] !text-[#C5221F] dark:!bg-red-400/10 dark:!text-[#F28B82]' : ''}`}
        >
          {copyStatus === 'copied'
            ? <Check size={14} className="text-[#34C759]" />
            : copyStatus === 'failed'
              ? <X size={14} />
              : <Copy size={14} />}
          {copyStatus !== 'idle' && <span aria-live="polite">{copyLabel}</span>}
        </button>
        {canSubmitFeedback && <>
          <button type="button" data-testid="assistant-feedback-positive" onClick={() => changeRating('positive')} title={copy.feedbackPositive} aria-label={copy.feedbackPositive} aria-pressed={rating === 'positive'} className={`${actionClass} ${rating === 'positive' ? '!bg-emerald-500/[0.10] !text-emerald-600 dark:!text-emerald-300' : ''}`}>
            <ThumbsUp size={14} />
          </button>
          <button type="button" data-testid="assistant-feedback-negative" onClick={() => changeRating('negative')} title={copy.feedbackNegative} aria-label={copy.feedbackNegative} aria-pressed={rating === 'negative'} className={`${actionClass} ${rating === 'negative' ? '!bg-red-500/[0.08] !text-[#C5221F] dark:!text-[#F28B82]' : ''}`}>
            <ThumbsDown size={14} />
          </button>
        </>}
        {typeof onRetry === 'function' && (
          <button type="button" data-testid="assistant-message-retry" onClick={onRetry} title={copy.retryReply} aria-label={copy.retryReply} className={actionClass}>
            <RotateCcw size={14} />
          </button>
        )}
        <button
          ref={exportTriggerRef}
          id={triggerIds.export}
          type="button"
          data-assistant-message-action-trigger
          data-testid="assistant-message-export"
          onClick={event => openMenu('export', event)}
          title={copy.exportReplyTitle}
          aria-label={copy.exportReplyTitle}
          aria-haspopup="menu"
          aria-expanded={menu?.kind === 'export'}
          aria-controls={menuIds.export}
          className={actionClass}
        >
          <Download size={14} />
          <span>{copy.exportReply}</span>
          <ChevronDown size={11} className={menu?.kind === 'export' ? 'rotate-180' : ''} />
        </button>
        <button
          ref={shareTriggerRef}
          id={triggerIds.share}
          type="button"
          data-assistant-message-action-trigger
          data-testid="assistant-message-share"
          onClick={event => openMenu('share', event)}
          title={copy.shareReplyTitle}
          aria-label={copy.shareReplyTitle}
          aria-haspopup="menu"
          aria-expanded={menu?.kind === 'share'}
          aria-controls={menuIds.share}
          className={actionClass}
        >
          <Share2 size={14} />
          <span>{copy.shareReply}</span>
          <ChevronDown size={11} className={menu?.kind === 'share' ? 'rotate-180' : ''} />
        </button>
        {actionFeedback && (
          <span
            aria-live="polite"
            className={`max-w-[320px] truncate text-[12px] ${actionFeedback?.failed ? 'text-[#C5221F] dark:text-[#F28B82]' : 'text-[#747775] dark:text-[#9AA0A6]'}`}
            title={actionFeedback?.message || copyLabel}
          >
            {actionFeedback.message}
          </span>
        )}
      </div>
      {menu && createPortal((
        <div
          id={menuIds[menu.kind]}
          role="menu"
          aria-labelledby={triggerIds[menu.kind]}
          data-assistant-message-action-menu
          data-testid={`assistant-message-${menu.kind}-menu`}
          className={menuClass}
          style={{ position: 'fixed', zIndex: 9999, left: menu.left, top: menu.top, width: menu.width }}
          onMouseDown={event => event.stopPropagation()}
          onKeyDown={handleMenuKeyDown}
        >
          {menu.kind === 'export' ? (
            <>
              <button ref={element => { menuItemsRef.current[0] = element; }} type="button" role="menuitem" aria-label={copy.exportMarkdown} data-testid="assistant-export-md" className={menuItemClass} onClick={() => handleExport('md')}>
                <FileText size={16} className="shrink-0" />
                <span className="min-w-0 flex-1"><span className="block font-medium">{copy.exportMarkdown}</span><span className="block truncate text-[11px] text-[#747775] dark:text-[#9AA0A6]">{copy.exportMarkdownHint}</span></span>
                <Check size={14} className="shrink-0 text-[#0B57D0] dark:text-[#8AB4F8]" />
              </button>
              <button ref={element => { menuItemsRef.current[1] = element; }} type="button" role="menuitem" aria-label={copy.exportHtml} data-testid="assistant-export-html" className={menuItemClass} onClick={() => handleExport('html')}>
                <Code size={16} className="shrink-0" />
                <span className="min-w-0"><span className="block font-medium">{copy.exportHtml}</span><span className="block truncate text-[11px] text-[#747775] dark:text-[#9AA0A6]">{copy.exportHtmlHint}</span></span>
              </button>
            </>
          ) : (
            <>
              <button ref={element => { menuItemsRef.current[0] = element; }} type="button" role="menuitem" aria-label={copy.shareSystem} data-testid="assistant-share-system" className={menuItemClass} onClick={handleSystemShare}>
                <Share2 size={16} className="shrink-0" />
                <span className="min-w-0"><span className="block font-medium">{copy.shareSystem}</span><span className="block truncate text-[11px] text-[#747775] dark:text-[#9AA0A6]">{copy.shareSystemHint}</span></span>
              </button>
              <div role="separator" className="mx-3 my-1 border-t border-black/[0.08] dark:border-white/10" />
              <div id={`${menuIds.share}-apps-label`} role="presentation" className="px-3 pb-1 pt-1 text-[11px] font-medium text-[#747775] dark:text-[#9AA0A6]">{copy.shareApps}</div>
              <div role="group" aria-labelledby={`${menuIds.share}-apps-label`}>
                {SHARE_TARGETS.map((target, index) => (
                  <button ref={element => { menuItemsRef.current[index + 1] = element; }} key={target} type="button" role="menuitem" aria-label={copy.shareTargets[target]} data-testid={`assistant-share-${target}`} className={`${menuItemClass} !min-h-9 !py-1.5`} onClick={() => handleAppShare(target)}>
                    <MessageCircle size={15} className="shrink-0" />
                    <span>{copy.shareTargets[target]}</span>
                  </button>
                ))}
              </div>
            </>
          )}
        </div>
      ), document.body)}
    </>
  );
}
