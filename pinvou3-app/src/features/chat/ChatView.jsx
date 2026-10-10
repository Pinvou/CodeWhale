import React, { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, useSyncExternalStore } from 'react';
import { createPortal } from 'react-dom';
import {
  invokeObservedPanelSelection,
  isSubagentPanelPublicationCurrent,
} from './subagent-panel-publication.mjs';
import { sceneStatusKey } from './scene_status_key.js';
import { AlertTriangle, ArrowLeft, BarChart2, Brain, Briefcase, Check, ChevronDown, ChevronRight, ClipboardList, Copy, Edit2, FileText, FolderOpen, Globe, ImageIcon, MessageSquare, Monitor, Package, Paperclip, PinIcon, Presentation, Send, Sparkles, StopCircle, Terminal, Trash2, Upload, X, Zap } from '../../components/icons.jsx';
import { bridge } from '../../hooks/useBridge.js';
import { useCopyFlash } from '../../hooks/useCopyFlash.js';
import { can, isWeb } from '../../shared/platform.js';
import { isImeComposing } from '../../shared/ime-guard.mjs';
import { formatCompactCount } from '../../shared/format-number.js';
import { getSyntaxHighlightVersion, subscribeSyntaxHighlight } from '../../shared/syntax-highlighter.js';
import { renderMarkdown } from '../../shared/markdown-renderer.js';
import { AppIcon, DEPT_ORDER, deptColor, deptLabelFor, personaText } from '../personas/persona-shared.jsx';
import { ComposerModelSelector, ComposerToolMenu } from '../settings/composer-shared.jsx';
import { ComposerPopover, POPOVER_SURFACE, useOutsidePointerClose } from '../../components/ComposerPopover.jsx';
import { PinvouLogo } from '../../components/PinvouLogo.jsx';
import { ViewErrorBoundary } from '../../shared/ViewErrorBoundary.jsx';
import { ArtifactCard } from '../tools/tool-common.jsx';
import { RightDockPanel, useRightDockOcclusion } from '../../components/layout/RightDock.jsx';
import { CarefulBlockedCard, PlanCard, PlanStuckCard, ToolCard, UserInputCard, cardBtnCls } from '../tools/tool-renderers.jsx';
import { annotateAgentSpawnGroups } from '../multiagent/spawn-aggregation.mjs';
import { RunningAgentsOverlay } from '../multiagent/RunningAgentsOverlay.jsx';
import { ComputerUseBanner, ComputerUseDialogs } from '../computer-use/ComputerUseConsent.jsx';
import {
  ConversationActivityIndicator,
  ConversationTimeline,
  useConversationSecondClock,
} from '../conversation/ConversationTimeline.jsx';
import { AuxQuoteSelection } from '../aux-chat/AuxQuoteSelection.jsx';
import { HomeModeSwitcher } from '../conversation/HomeModeSwitcher.jsx';
import {
  conversationItemsForMode,
  projectDeepSeekConversation,
} from '../conversation/deepseek-conversation.js';
import {
  measureConversationScrollGeometry,
  shouldForceScrollFollow,
  startConversationBottomFollower,
  transitionConversationScrollState,
} from '../conversation/conversation-scroll.js';
import {
  captureConversationScrollPosition,
  isFetchTool,
  isSearchTool,
  restoreConversationScrollPosition,
} from '../conversation/conversation-model.js';
import { AttachmentChips } from '../attachments/AttachmentChips.jsx';
import {
  collectClipboardImages,
  pasteEventNeedsClipboardFallback,
  pasteImageClipboardFallbackAvailable,
  readPasteImageAsBytes,
} from '../attachments/paste-image.js';
import { formatAttachmentLimitError } from '../attachments/attachment-limit-errors.js';
import { ComposerAttachmentDropOverlay } from '../attachments/ComposerAttachmentDropOverlay.jsx';
import { ConversationAttachmentBubble } from '../attachments/ConversationAttachmentBubble.jsx';
import { splitAttachmentLine } from '../attachments/attachment-message.js';
import { CHAT_INPUT_MAX_LENGTH, constrainChatInput } from './chat-input-limit.js';
import {
  controlledTaskChatItems as projectControlledTaskChatItems,
  mergeControlledTaskChatItems,
  remoteChatItems as projectRemoteChatItems,
} from '../remote-control/chat-transcript-projection.mjs';
import {
  liveRemoteProxyBinding,
  activeLiveRemoteTaskId,
  remoteProxyHandshakeBinding,
  remoteProxyLeaseIdentity,
  remoteProxyComposerIdentity,
  remoteProxyModelSelectionIdentity,
  remoteProjectionPollRevision,
  remoteProxyPresentationState,
  remoteProjectionLabelKey,
  newRemoteTaskIntent,
  remoteProxyChatItems,
  remoteProxyForSession,
  selectedExportableRemoteModelId,
  sameRemoteComposerOwner,
} from '../remote-control/remote-session-adapter.mjs';
import {
  downloadRemoteArtifact,
  createRemoteTransferGuard,
  saveRemoteArtifactBlob,
  uploadRemoteFiles,
  validateRemoteFiles,
} from '../remote-control/remote-file-transfer.mjs';
import {
  forgetRemoteDispatch,
  recordRemoteDispatch,
  markRemoteDispatchPayloadLost,
  remoteDispatchCases,
} from '../remote-control/remote-dispatch-ui.mjs';
import {
  isControlledDeviceChatActive,
  isTaskTerminal,
  taskStatusKey,
} from '../remote-control/remote-control-logic.mjs';
import { createCapabilityRequestLoop } from '../remote-control/capability-request-loop.mjs';
import { permissionOperationCopyKey, permissionRedactionCopyKey } from '../remote-control/controlled-device-panel-state.mjs';
import {
  canUseControllerTemporaryModel,
  clearResolvedRemoteModelRequiredError,
  normalizeRemoteModelSource,
  remoteModelSelectionError,
} from '../remote-control/remote-model-source.mjs';
import { deriveRunningShellTasks, formatElapsedMs, tailOutputLines } from './background-tasks.js';
import { useShellTaskCancel } from './shell-task-cancel.js';
import {
  VOICE_SHORTCUT_ENABLED_KEY,
  VOICE_SHORTCUT_SETTINGS_EVENT,
  setVoiceShortcutEnabled,
  setVoiceShortcutIntroSeen,
  voiceShortcutEnabled,
  voiceShortcutIntroSeen,
} from './voice-shortcut-settings.mjs';
import { AssistantMessageActions, AssistantMessageFooter } from '../conversation/AssistantMessageActions.jsx';
// 重面板惰性化:ArtifactsPanel(design/产物场景才出现)与 SubagentTranscriptPanel
// (专家卡点开才出现)各带一串专属依赖(design-runtime/EditableMarkdownPreview/
// subagent-conversation 等,合计 ~130KB 源码),条件渲染本就存在。预取挂在打开
// 动作同 tick,首次打开仍可能挂起一个微任务级窗口——因此各挂载点必须配局部
// Suspense(面板形状 fallback)与局部 ErrorBoundary,把挂起/失败限制在面板槽位,
// 不冒泡到应用级边界闪断整视图(口径同 LazyCodexAcpView/WebAccessModal)。
// 动态 import 收口在本表(口径同 app/view-loaders.js 的 VIEW_LOADERS)。
const LazyToolWelcomeCard = React.lazy(() => import('../tools/ToolWelcomeCard.jsx'));

const CHAT_PANEL_LOADERS = Object.freeze({
  artifacts: () => import('../artifacts/ArtifactsPanel.jsx'),
  subagent: () => import('../multiagent/SubagentTranscriptPanel.jsx'),
  auxChat: () => import('../aux-chat/AuxChatPanel.jsx'),
});
const LazyArtifactsPanel = React.lazy(() => CHAT_PANEL_LOADERS.artifacts().then((m) => ({ default: m.ArtifactsPanel })));
const LazySubagentTranscriptPanel = React.lazy(() => CHAT_PANEL_LOADERS.subagent().then((m) => ({ default: m.SubagentTranscriptPanel })));
const LazyAuxChatPanel = React.lazy(() => CHAT_PANEL_LOADERS.auxChat().then((m) => ({ default: m.AuxChatPanel })));
const prefetchChatPanel = (key) => {
  const loader = CHAT_PANEL_LOADERS[key];
  if (loader) loader().catch(() => {});
};

// RemoteProxy entries expose one Rust-sanitized display body. Keep this
// allowlist aligned with system projection kinds and render only that string;
// never serialize the source entry, whose non-display fields are not UI data.
const REMOTE_PROXY_SYSTEM_BODY_KINDS = new Set([
  'approval',
  'error',
  'attachment',
  'artifact',
]);

function remoteProxySystemBody(item) {
  if (!REMOTE_PROXY_SYSTEM_BODY_KINDS.has(item?.remoteKind) || typeof item?.text !== 'string') return '';
  return item.text.trim();
}

function remoteActionFailureCode(failure) {
  const candidate = failure?.code || failure?.message || failure;
  return String(candidate || '') === 'result_unknown' ? 'result_unknown' : 'errorGeneric';
}

function constrainDialogFocus(event, dialog) {
  if (event.key !== 'Tab' || !dialog) return;
  const focusable = [...dialog.querySelectorAll(
    'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])',
  )].filter(element => !element.hidden && element.getAttribute('aria-hidden') !== 'true');
  if (!focusable.length) {
    event.preventDefault();
    dialog.focus();
    return;
  }
  const first = focusable[0];
  const last = focusable[focusable.length - 1];
  if (event.shiftKey && document.activeElement === first) {
    event.preventDefault();
    last.focus();
  } else if (!event.shiftKey && document.activeElement === last) {
    event.preventDefault();
    first.focus();
  }
}

const reportRightDockSelectionFailure = (error) => {
  console.error('[chat] Right Dock selection failed', error);
};
// 面板槽位级挂起 fallback:与 LazyCodexAcpView 同款容器,懒 chunk 解析的
// 微任务窗口内占住面板位置,避免挂起冒泡到应用级边界把整视图闪断成 fallback。
function PanelSuspense({ children }) {
  return (
    <React.Suspense fallback={(
      <div className="flex-1 flex items-center justify-center min-h-0 text-sm text-gray-500 dark:text-gray-300">
        …
      </div>
    )}>
      {children}
    </React.Suspense>
  );
}

import { copyClipboardText } from '../../shared/clipboard.js';
import {
  assistantItemCopyText,
  readClipboardText,
} from '../conversation/message-clipboard.js';
import { parseLooseJson } from '../conversation/structured-assistant-content.js';
import {
  createPinvouModeScopeKey,
  loadPinvouModeState,
  reducePinvouModeState,
  savePinvouModeState,
} from './pinvou-mode-state.js';
import { SceneCardGrid, TemplateCardGrid } from './scene-cards.jsx';
import { createDesignChange, createDesignChangeScopeKey, reduceScopedDesignChanges, uniqueDesignChanges } from './design-changes.js';
import { createVisualPosterMessageMeta, shouldUseVisualPosterScene } from './visual-poster-scene.js';
import {
  createDataVisualizationMessageMeta,
  createDocumentWritingMessageMeta,
  createPersonalWorkbenchMessageMeta,
  createPptDesignMessageMeta,
  PERSONAL_WORKBENCH_SCENE_KEY,
  shouldUseDataVisualizationScene,
  shouldUseDocumentWritingScene,
  shouldUsePersonalWorkbenchScene,
  shouldUsePptDesignScene,
} from './work-scene-routes.js';
import {
  PERSONAL_WORKBENCH_TEMPLATES,
  findPersonalWorkbenchTemplateDraft,
  getPersonalWorkbenchTemplate,
  getPersonalWorkbenchTemplateById,
  isPersonalWorkbenchTemplateDraftForTemplate,
} from './personal-workbench-scene.js';
import { canPrepareSceneCapabilities, prepareSceneCapabilities, requiredCapabilitiesForMeta } from './scene-capabilities.js';
import { consumeWelcomeOptIn, resolveSendCapabilityStatus, runSharedWelcomeOptIn } from './welcome-optin.js';
import { invokeTauri } from '../../platform/tauri/client.js';
import {
  COMPOSER_ICON_BUTTON_CLASS,
  COMPOSER_MENU_ENTRY_CLASS,
  ComposerKbSelector,
  ComposerModeChip,
} from './composer-controls.jsx';
import { ComposerWorkspaceSelector } from './ComposerWorkspaceSelector.jsx';
import { YoloConfirmCard } from '../../shared/yolo-confirm-card.jsx';
import { needsYoloConfirmation } from '../codex/code-permission-state.js';
import { CHAT_YOLO_GATE_UNKNOWN_BINDING, chatYoloGateApplies, shouldShowWorkspaceBindingChip } from './chat-workspace-binding.js';
import { workspaceName } from '../../shared/workspace-recents.js';
import {
  VoiceComposerButton,
  VoiceEditPreview,
  VoiceComposerPillLayer,
  VoiceComposerStatus,
} from '../voice-composer/VoiceComposerControls.jsx';
import { VoiceShortcutIntroModal } from '../voice-composer/VoiceShortcutIntroModal.jsx';
import {
  isVoiceActive,
  isVoiceBusy,
  normalizeVoiceMode,
} from '../voice-composer/voice-ui-policy.mjs';
import { useComposerVoiceInput } from '../voice-composer/useComposerVoiceInput.js';

const MULTI_AGENT_ENABLED = can('multiAgent');
const SCENE_REGISTRY = Object.freeze({
  work: new Set([PERSONAL_WORKBENCH_SCENE_KEY, 'document-writing']),
  design: new Set(['poster', 'data-visualization', 'ppt']),
});

function sceneRegistryFields(l1, l2) {
  const level1 = SCENE_REGISTRY[l1] ? l1 : 'unknown';
  const level2 = level1 !== 'unknown' && SCENE_REGISTRY[level1].has(l2) ? l2 : 'unknown';
  return { sceneL1: level1, sceneL2: level2 };
}

function createSceneViewId() {
  const bytes = new Uint8Array(12);
  window.crypto.getRandomValues(bytes);
  return `scene-view-${Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('')}`;
}

// Computer use (screenshot + keyboard/mouse control) is a desktop-only capability: on Web
// can() is always false, so the consent banner/dialogs render not at all and tool cards
// fall back to the default card.
const COMPUTER_USE_ENABLED = can('computerUse');

// Enter-to-submit guard (shared by the main input, queued-message edit, and in-bubble edit):
// Shift+Enter still inserts a newline; Enter during IME composition confirms the candidate text
// and must not also trigger submit — otherwise one Enter both commits and sends. Matches PetWindow.
const isPlainEnter = (e) => e.key === 'Enter' && !e.shiftKey && !isImeComposing(e);

// Second-clock wrapper for the composer activity indicator: the tick used to live on ChatView
// top-level state, so while busy the whole ChatView (including all transcript coordination)
// re-rendered once per second; the indicator is the only place showing elapsed time, and now the
// tick re-renders just this small subtree.
/**
 * @param {object} props - component props
 * @param {{ status: string } | null} props.turn - active conversation turn, if any
 * @param {() => void} props.onRequestAttention - scroll-to-bottom request handler
 * @param {string} props.className - extra class for the indicator
 * @param {object} props.copy - conversation copy table
 * @returns {React.ReactElement | null} the ticking activity indicator
 */
function LiveConversationActivityIndicator({ turn, onRequestAttention, className, copy }) {
  const running = !!turn && turn.status === 'running';
  const now = useConversationSecondClock(running);
  return (
    <ConversationActivityIndicator
      turn={turn}
      now={now}
      onRequestAttention={onRequestAttention}
      className={className}
      copy={copy}
    />
  );
}

// Unified scene table after the design lane was merged into work: a scene
// only expresses "the professional context of this message" and is
// lane-independent; scene cards render below the empty-state greeting
// (scene-cards.jsx).
const SCENE_TABS = [
  { key: PERSONAL_WORKBENCH_SCENE_KEY, labelKey: 'personalWorkbench', Icon: Briefcase },
  { key: 'document-writing', labelKey: 'documentWriting', Icon: FileText },
  { key: 'poster', labelKey: 'poster', Icon: ImageIcon },
  { key: 'data-visualization', labelKey: 'dataVisualization', Icon: BarChart2 },
  { key: 'ppt', labelKey: 'pptDesign', Icon: Presentation },
];

// legacy assistant 气泡由 item.text 现算 markdown(懒语言注册后恢复高亮所必需),
// 但 ChatBubble 未 memo 化:输入框每个按键、流式每个 delta、秒级 tick 都会全量
// 重渲染,长会话下每次全量重跑 marked+DOMPurify。content-visibility(#275)只省
// 浏览器合成,不省 React 渲染。item 引用稳定(bridge 会话数据),按 item 键控、
// text+syntaxVersion 未变直接复用上次结果;版本号 bump(懒语言注册)自然失效重算。
const legacyMarkdownCache = new WeakMap();
function renderLegacyMarkdownCached(item, syntaxVersion) {
  const cached = legacyMarkdownCache.get(item);
  if (cached && cached.text === item.text && cached.version === syntaxVersion) {
    return cached.html;
  }
  const html = renderMarkdown(item.text);
  legacyMarkdownCache.set(item, { text: item.text, version: syntaxVersion, html });
  return html;
}

// Same idea as legacyMarkdownCache: every composer keystroke, streaming
// delta, and clock tick re-renders the full view, and each render of an
// assistant bubble ran three rounds of pre/code regex parsing (persona
// draft, scheduled-task draft, card-question follow-up) plus the streaming
// fold. The whole chain is a pure function of
// (html, streaming, allowScheduledTaskDraft, streamingDraftLabel), cached
// per item and reused while the tuple is unchanged; while streaming, the
// item gets a new reference per delta so the cache invalidates naturally —
// behavior unchanged.
const assistantParseCache = new WeakMap();
function parseAssistantBubblesCached(item, html, streaming, allowScheduledTaskDraft, streamingDraftLabel) {
  const cached = assistantParseCache.get(item);
  if (cached
    && cached.html === html
    && cached.streaming === streaming
    && cached.allowScheduledTaskDraft === allowScheduledTaskDraft
    && cached.streamingDraftLabel === streamingDraftLabel) {
    return cached.parsed;
  }
  const pd = streaming ? { draft: null, html: hideStreamingDraft(html, streamingDraftLabel) } : parsePersonaDraft(html);
  const sd = (streaming || !allowScheduledTaskDraft) ? { draft: null, html: pd.html } : parseScheduledTaskDraft(pd.html);
  const cq = streaming ? { q: null, html: sd.html } : parseCardQuestion(sd.html);
  const parsed = { pd, sd, cq };
  assistantParseCache.set(item, { html, streaming, allowScheduledTaskDraft, streamingDraftLabel, parsed });
  return parsed;
}

// The memory status label map depends only on the current language
// dictionary t (a module singleton); caching per t avoids rebuilding the
// same map on every render of every bubble.
const memoryStatusLabelsCache = new WeakMap();
function getMemoryStatusLabels(t) {
  let labels = memoryStatusLabelsCache.get(t);
  if (!labels) {
    const chatCopy = t.uiChat;
    const chatViewCopy = t.uiChatView;
    labels = {
      '已忽略': chatCopy.ignoreOnce,
      '不再提示': chatCopy.neverAsk,
      '已记住': chatViewCopy.memStatusRemembered,
      '已归档': chatViewCopy.memStatusArchived,
      '已删除': chatViewCopy.memStatusDeleted,
      '记忆已更新': chatCopy.memoryUpdated,
      '记忆已归档': chatViewCopy.memStatusArchivedNotice,
      '记忆已删除': chatViewCopy.memStatusDeletedNotice,
    };
    memoryStatusLabelsCache.set(t, labels);
  }
  return labels;
}

function localizeSceneTabs(items, copy) {
  return items.map(item => ({
    ...item,
    label: copy[item.labelKey],
  }));
}

function pinvouSceneDisplay(scene, copy) {
  switch (scene) {
    case 'work:document-writing':
      return { label: copy.documentWriting, Icon: FileText };
    case 'work:personal-workbench':
      return { label: copy.personalWorkbench, Icon: Briefcase };
    case 'design:poster':
      return { label: copy.poster, Icon: ImageIcon };
    case 'design:data-visualization':
      return { label: copy.dataVisualization, Icon: BarChart2 };
    case 'design:ppt':
      return { label: copy.pptDesign, Icon: Presentation };
    default:
      return null;
  }
}

const openChatExternalUrl = (url) => {
  if (isWeb) {
    const opened = window.open(url, '_blank', 'noopener,noreferrer');
    if (opened) opened.opener = null;
    return;
  }
  invokeTauri('open_user_external_url', { url }).catch(() => {});
};

    // 输入框底栏:知识库挂载选择器(与 ComposerModelSelector/ComposerToolMenu 同款 pill,
    // class 暗色策略)。给当前对话挂一个知识集(会话级粘连),挂上后每条消息发送前后端自动
    // 检索注入相关片段(commands::chat)。草稿态选集会经 bridge.knowledge.mountCollection 先物化 session。
    // 附件入口。桌面端与旧桌面实例保持原有单入口(直开各自的文件选择器);
    // 仅当桌面实例通过能力协商声明支持浏览器上传(deviceFileUpload)时,才展开
    // "从此设备上传 / 从桌面实例选择"双入口菜单。能力在点击时评估,不依赖
    // 能力快照到达时机的渲染竞态。
    const ComposerAttachButton = ({ t, compact }) => {
      const [open, setOpen] = useState(false);
      const triggerRef = useRef(null);
      const fileInputRef = useRef(null);
      function onTriggerClick() {
        if (!bridge.available) return;
        if (can('deviceFileUpload')) setOpen(v => !v);
        else bridge.attachments.pickAndAttach();
      }
      function pickFromDevice() {
        setOpen(false);
        if (fileInputRef.current) fileInputRef.current.click();
      }
      function pickFromHost() {
        setOpen(false);
        bridge.attachments.pickAndAttach();
      }
      function onFilesChosen(event) {
        const files = event.target.files;
        if (files && files.length && bridge.available) bridge.attachments.uploadDeviceFiles(files);
        event.target.value = '';
      }
      return (
        <div className="relative">
          <button ref={triggerRef} onClick={onTriggerClick} title={t.attachAdd} className={COMPOSER_ICON_BUTTON_CLASS}>
            <Paperclip size={18} />
          </button>
          <input ref={fileInputRef} type="file" multiple className="hidden" data-testid="device-file-input" onChange={onFilesChosen} />
          <ComposerPopover open={open} onClose={() => setOpen(false)} triggerRef={triggerRef} compact={compact}
            desktopClassName={`absolute bottom-full left-0 mb-2 z-50 w-56 ${POPOVER_SURFACE}`}>
            <button type="button" onClick={pickFromDevice} className={COMPOSER_MENU_ENTRY_CLASS}>
              <Upload size={15} className="shrink-0 text-gray-400 group-hover:text-white/90" />
              {t.attachFromDevice}
            </button>
            <button type="button" onClick={pickFromHost} className={COMPOSER_MENU_ENTRY_CLASS}>
              <Monitor size={15} className="shrink-0 text-gray-400 group-hover:text-white/90" />
              {t.attachFromHost}
            </button>
          </ComposerPopover>
        </div>
      );
    };
    const SceneModeTag = ({ scene, onClear, clearLabel }) => {
      if (!scene) return null;
      const SceneIcon = scene.Icon || Sparkles;
      return (
        <div className="mb-2 flex flex-wrap justify-start gap-2 px-1" data-testid="pinvou-scene-tag">
          <div className={`inline-flex h-8 items-center gap-2 rounded-[14px] px-3 text-[13px] font-semibold shadow-sm ${
            'bg-[#F5F5F7] text-[#1D1D1F] ring-1 ring-black/[0.06] dark:bg-[#2A2B2D] dark:text-[#F5F5F7] dark:ring-white/10'
          }`}>
            <SceneIcon size={15} className="shrink-0" />
            <span>{scene.label}</span>
            {onClear && (
              <button
                type="button"
                data-testid="pinvou-scene-tag-clear"
                aria-label={clearLabel}
                title={clearLabel}
                onClick={(event) => {
                  event.preventDefault();
                  event.stopPropagation();
                  onClear();
                }}
                className={`-mr-1 flex h-5 w-5 items-center justify-center rounded-full transition-colors ${
                  'text-[#5F6368] hover:bg-black/10 hover:text-[#1D1D1F] dark:text-[#C7C7CC] dark:hover:bg-white/10 dark:hover:text-white'
                }`}
              >
                <X size={13} />
              </button>
            )}
          </div>
        </div>
      );
    };

    const ChatRightDockSwitcher = ({
      theme,
      artifactsLabel,
      browserLabel,
      artifactCount,
      browserAvailable,
      browserOpen,
      activePanelId,
      onOpenArtifacts,
      onOpenBrowser,
    }) => {
      const [menuOpen, setMenuOpen] = useState(false);
      const rootRef = useRef(null);
      // Outside-pointer close (contains guard on a document-captured pointerdown) plus Escape
      // close, via ComposerPopover's shared listener group; rootRef covers trigger button and panel.
      useOutsidePointerClose(menuOpen, () => setMenuOpen(false), [rootRef], { escape: true });
      const browserSelected = browserAvailable && (
        activePanelId === 'browser'
        || (activePanelId !== 'artifact-preview' && browserOpen)
      );
      const selectedId = browserSelected ? 'browser' : 'artifact-preview';
      const selectedLabel = browserSelected ? browserLabel : artifactsLabel;
      const SelectedIcon = browserSelected ? Globe : Package;

      const openPanel = (panelId) => {
        setMenuOpen(false);
        const select = panelId === 'browser' ? onOpenBrowser : onOpenArtifacts;
        void invokeObservedPanelSelection(select, [], reportRightDockSelectionFailure);
      };
      const triggerClass = `pointer-events-auto flex h-10 shrink-0 items-center gap-2 rounded-full border px-3 text-[14px] font-medium shadow-sm transition-colors ${
        theme === 'dark'
          ? 'border-white/10 bg-[#1E1F20] text-[#E3E3E3] hover:bg-[#333537]'
          : 'border-black/10 bg-white text-[#1F1F1F] hover:bg-[#F0F4F9]'
      }`;

      if (!browserAvailable) {
        return (
          <button
            type="button"
            data-testid="chat-artifacts-entry"
            onMouseEnter={() => prefetchChatPanel('artifacts')}
            onFocus={() => prefetchChatPanel('artifacts')}
            onClick={onOpenArtifacts}
            className={triggerClass}
          >
            <Package size={16} />
            <span className="max-sm:hidden">{artifactsLabel}</span>
            {artifactCount > 0 && (
              <span className="rounded-full bg-[#0B57D0] px-1.5 text-[11px] text-white dark:bg-[#A8C7FA] dark:text-[#062E6F]">
                {artifactCount}
              </span>
            )}
          </button>
        );
      }

      return (
        <div ref={rootRef} className="pointer-events-auto relative" data-testid="chat-right-dock-switcher">
          <button
            type="button"
            data-testid="chat-right-dock-switcher-trigger"
            aria-haspopup="menu"
            aria-expanded={menuOpen}
            title={`${artifactsLabel} / ${browserLabel}`}
            onMouseEnter={() => prefetchChatPanel('artifacts')}
            onFocus={() => prefetchChatPanel('artifacts')}
            onClick={() => setMenuOpen((open) => !open)}
            className={triggerClass}
          >
            <SelectedIcon size={16} />
            <span className="max-sm:hidden">{selectedLabel}</span>
            {!browserSelected && artifactCount > 0 && (
              <span className="rounded-full bg-[#0B57D0] px-1.5 text-[11px] text-white dark:bg-[#A8C7FA] dark:text-[#062E6F]">
                {artifactCount}
              </span>
            )}
            <ChevronDown
              size={14}
              className={`transition-transform ${menuOpen ? 'rotate-180' : ''}`}
            />
          </button>
          {menuOpen && (
            <div
              role="menu"
              className={`absolute right-0 top-full mt-2 w-44 overflow-hidden rounded-xl border p-1 shadow-xl ${
                theme === 'dark'
                  ? 'border-white/10 bg-[#252628] text-[#E8EAED]'
                  : 'border-black/10 bg-white text-[#202124]'
              }`}
            >
              {[
                { id: 'artifact-preview', label: artifactsLabel, Icon: Package, count: artifactCount },
                { id: 'browser', label: browserLabel, Icon: Globe, count: 0 },
              ].map(({ id, label, Icon, count }) => (
                <button
                  key={id}
                  type="button"
                  role="menuitemradio"
                  aria-checked={selectedId === id}
                  data-testid={`chat-right-dock-option-${id}`}
                  onClick={() => openPanel(id)}
                  className={`flex w-full items-center gap-2 rounded-lg px-3 py-2 text-left text-[13px] transition-colors ${
                    theme === 'dark' ? 'hover:bg-white/10' : 'hover:bg-black/5'
                  }`}
                >
                  <Icon size={16} className="shrink-0" />
                  <span className="min-w-0 flex-1 truncate">{label}</span>
                  {count > 0 && (
                    <span className="rounded-full bg-[#0B57D0] px-1.5 text-[10px] text-white dark:bg-[#A8C7FA] dark:text-[#062E6F]">
                      {count}
                    </span>
                  )}
                  {selectedId === id && <Check size={14} className="shrink-0" />}
                </button>
              ))}
            </div>
          )}
        </div>
      );
    };

    // 后台任务指示器（输入框上方"处理中"提示行右侧的蓝色胶囊）：仅当前会话有运行中后台 shell 任务时渲染。
    // 数据来自 bridge 轮询 reconcile 进 chatItems 的快照（terminal.js
    // applyShellSnapshots / markBackgroundToolItem），点击弹出列表可查看输出、取消任务。
    const BackgroundTaskRow = ({ task, t, chatCopy }) => {
      const { cancelling, cancelError, cancel } = useShellTaskCancel(t);
      // Timing baseline: the polling reconcile only touches the card when new output arrives, so
      // a quiet task's elapsedMs never changes; per-second movement is derived by the indicator
      // from "baseline + locally elapsed time" (one 1s clock per row, mounted only while the
      // popover is expanded). The second tick reuses ConversationTimeline's second clock (timer
      // created only while active, cleaned up on unmount). Do not fold the second tick into the
      // global chatItems reconcile — that would re-render all of ChatView every second while a
      // background task lives (why the second clock left ChatView top level; see the comment
      // above LiveConversationActivityIndicator).
      const now = useConversationSecondClock(true);
      // 服务端 elapsedMs 变化（新输出触发 reconcile）时，渲染期同步换基线
      // （React "adjust state when a prop changes" 模式）；基线时间戳直接用
      // now 状态，保证渲染纯度。
      const [elapsedBaseline, setElapsedBaseline] = useState(() => ({ elapsedMs: task.elapsedMs, at: now }));
      if (elapsedBaseline.elapsedMs !== task.elapsedMs) {
        setElapsedBaseline({ elapsedMs: task.elapsedMs, at: now });
      }
      const elapsedMs = elapsedBaseline.elapsedMs + Math.max(0, now - elapsedBaseline.at);
      const tail = tailOutputLines(task.output, 3);
      const lastLine = tailOutputLines(task.output, 1);
      return (
        <div className="px-3 py-2 hover:bg-black/[0.03] dark:hover:bg-white/[0.04]"
          data-testid="bg-task-row" data-bg-shell-task-id={task.taskId}>
          <div className="flex items-center gap-2 min-w-0">
            <span className="w-1.5 h-1.5 rounded-full bg-emerald-500 animate-pulse shrink-0" />
            <span className={`flex-1 min-w-0 truncate font-mono text-[12px] ${'text-[#1F1F1F] dark:text-[#E3E3E3]'}`} title={task.command}>{task.command}</span>
            <span className={`shrink-0 text-[11px] tabular-nums ${'text-[#85888D] dark:text-[#9AA0A6]'}`}>{formatElapsedMs(elapsedMs)}</span>
            <button type="button" data-testid="bg-cancel-shell-task" disabled={cancelling} onClick={() => cancel(task.sessionId, task.taskId)}
              aria-label={chatCopy.cancel} title={cancelling ? chatCopy.cancelling : chatCopy.cancel}
              className={`shrink-0 p-1 rounded-full disabled:opacity-50 transition-colors ${'text-[#85888D] hover:text-[#C5221F] hover:bg-black/5 dark:text-[#9AA0A6] dark:hover:text-[#F28B82] dark:hover:bg-white/10'}`}>
              <StopCircle size={14} />
            </button>
          </div>
          {lastLine && (
            <div className={`mt-0.5 pl-3.5 truncate font-mono text-[11px] ${'text-[#9AA0A6] dark:text-[#6E7276]'}`} title={tail}>{lastLine}</div>
          )}
          {cancelError && (
            <div className={`mt-1 ml-3.5 px-2 py-1 rounded-lg text-[11px] truncate ${'bg-red-50 text-[#C5221F] dark:bg-red-500/10 dark:text-[#F28B82]'}`} title={cancelError}>{cancelError}</div>
          )}
        </div>
      );
    };

    const BackgroundTasksIndicator = ({ tasks, t, chatCopy, compact }) => {
      const [open, setOpen] = useState(false);
      const [prevTaskCount, setPrevTaskCount] = useState(tasks.length);
      const triggerRef = useRef(null);
      // 任务全部结束时自动收起，避免浮层停留在空列表（渲染期间同步调整 state）。
      if (prevTaskCount !== tasks.length) {
        setPrevTaskCount(tasks.length);
        if (tasks.length === 0) setOpen(false);
      }
      if (tasks.length === 0) return null;
      return (
        <div className="relative pointer-events-auto">
          <button type="button" ref={triggerRef} data-testid="chat-bg-tasks-entry"
            onClick={() => setOpen(value => !value)}
            aria-label={chatCopy.bgTasks} title={chatCopy.bgTasks}
            className={`h-6 px-2.5 rounded-full text-[11px] font-medium flex items-center gap-1.5 whitespace-nowrap shrink-0 ${'bg-[#E8F0FE] text-[#1967D2] hover:bg-[#D2E3FC] dark:bg-[#A8C7FA] dark:text-[#062E6F] dark:hover:bg-[#8FB8F8]'}`}>
            <Terminal size={12} /> <span>{chatCopy.bgTasks}</span>
            <span className={`px-1 rounded-full text-[10px] ${'bg-white/70 dark:bg-black/15'}`}>{tasks.length}</span>
          </button>
          {/* compact 跟随其他输入框弹层的视口契约：窄屏/移动 WebUI 走 portal 锚定路径 */}
          <ComposerPopover open={open} onClose={() => setOpen(false)} triggerRef={triggerRef} compact={compact}
            desktopClassName={`absolute bottom-full left-0 mb-2 z-50 w-[360px] max-w-[calc(100vw-24px)] max-h-[420px] overflow-y-auto ${POPOVER_SURFACE}`}>
            <div className={`px-3 py-2 text-[12px] font-medium ${'text-[#85888D] dark:text-[#9AA0A6]'}`}>{chatCopy.bgTasksRunning(tasks.length)}</div>
            <div className={`divide-y ${'divide-black/5 dark:divide-white/5'}`}>
              {tasks.map(task => <BackgroundTaskRow key={task.taskId} task={task} t={t} chatCopy={chatCopy} />)}
            </div>
          </ComposerPopover>
        </div>
      );
    };

    // eslint-disable-next-line sonarjs/cognitive-complexity -- legacy main view: session/mode/artifact/browser state is highly cohesive; split refactor tracked separately
    const ChatView = ({ theme, t, bs, prefill, prefillAppend = false, focusComposerTick = 0, onPrefillConsumed, onOpenEditor, justInstalledTool, setJustInstalledTool, onGotoSettings, onGotoModelSettings, onGotoTools, onBackScheduledRun, codeModeAvailable = false, onSwitchHomeMode, onRemoteModelSourceChange, browserDockAvailable = false, browserDockOpen = false, rightDockActivePanelId = null, onRightDockPanelSelectionChange, onOpenBrowserDock, remoteConnection = null, remoteModelSource = 'controller_temporary' }) => {
      const chatCopy = t.uiChat;
      const chatViewCopy = t.uiChatView;
      const sceneCopy = chatCopy.sceneModes;
      const sceneTabs = localizeSceneTabs(SCENE_TABS, sceneCopy);
      const canInstallLocalAsr = can('localModelSetup') && can('dependencyInstall');
      const initialInput = constrainChatInput(
        bridge.available && bridge.chat && bridge.chat.getComposerDraft
          ? bridge.chat.getComposerDraft()
          : ((bs && bs.composerDraft) || '')
      );
      const [inputText, setInputTextState] = useState(initialInput.text);
      const [inputLimitReached, setInputLimitReached] = useState(initialInput.limitReached);
      const inputTextRef = useRef(initialInput.text);
      const setInputText = useCallback((valueOrUpdater) => {
        const rawValue = typeof valueOrUpdater === 'function'
          ? valueOrUpdater(inputTextRef.current)
          : valueOrUpdater;
        const constrained = constrainChatInput(rawValue);
        inputTextRef.current = constrained.text;
        setInputTextState(constrained.text);
        setInputLimitReached(constrained.limitReached);
        if (bridge.available && bridge.chat && bridge.chat.setComposerDraft) {
          bridge.chat.setComposerDraft(constrained.text);
        }
      }, []);
      const [artifactsOpen, setArtifactsOpen] = useState(false);
      const [artifactsFullscreen, setArtifactsFullscreen] = useState(false);
      const [activeArtifactPath, setActiveArtifactPath] = useState(null);
      const [artifactDockActivation, setArtifactDockActivation] = useState(0);
      const initialPinvouModeScope = createPinvouModeScopeKey(bs && bs.activeSessionId);
      const [pinvouModeState, setPinvouModeState] = useState(() => loadPinvouModeState(undefined, initialPinvouModeScope));
      const sceneViewIdRef = useRef(createSceneViewId());
      const sceneImpressionsRef = useRef(new Set());
      const pinvouModeScopeRef = useRef(initialPinvouModeScope);
      const pinvouModeStateRef = useRef(pinvouModeState);
      const pendingModeScopeMigrationRef = useRef(null);
      const [personalWorkbenchTemplateId, setPersonalWorkbenchTemplateId] = useState(null);
      const personalWorkbenchTemplateIdRef = useRef(null);
      const [selectedDesignElement, setSelectedDesignElement] = useState(null);
      const [designChangesByScope, setDesignChangesByScope] = useState({});
      const [designCommand, setDesignCommand] = useState(null);
      const [designAiState, setDesignAiState] = useState({ text: '', status: 'idle', lastPrompt: '', pendingPath: '', startedAt: 0 });
      const [sceneCapabilityStatus, setSceneCapabilityStatus] = useState(null);
      // Round-32 minor 10 (review #455): per-toast epoch — the session key
      // above discriminates sessions/drafts, not two sends in ONE session, so
      // a stale auto-clear timeout could shorten a newer send's ready toast.
      // Each ready toast stamps the next epoch; its closer only clears the
      // exact epoch it scheduled.
      const sceneReadyToastEpochRef = useRef(0);
      const designAiSessionRef = useRef(null);
      const updateDesignAiState = useCallback((valueOrUpdater) => {
        setDesignAiState((prev) => {
          const next = typeof valueOrUpdater === 'function' ? valueOrUpdater(prev) : valueOrUpdater;
          const merged = { text: '', status: 'idle', lastPrompt: '', pendingPath: '', startedAt: 0, ...(next || {}) };
          if (typeof window !== 'undefined') {
            if (merged.status !== 'idle' || merged.text || merged.lastPrompt) {
              window.__PINVOU_DESIGN_AI_STATE__ = merged;
            } else {
              window.__PINVOU_DESIGN_AI_STATE__ = null;
            }
          }
          return merged;
        });
      }, []);
      // Artifacts live in the app-level Right Dock; its host owns narrow-layout fallback.
      const artColRef = useRef(null);
      const scrollRef = useRef(null);
      const conversationContentRef = useRef(null);
      const autoScrollRef = useRef(true);
      const lastUserSnapLengthRef = useRef(0);
      const lastScrollTopRef = useRef(0);
      const lastScrollHeightRef = useRef(0);
      const subagentPanelScrollRef = useRef(null);
      const [showScrollBottom, setShowScrollBottom] = useState(false);
      const chatRootRef = useRef(null);
      const composerRef = useRef(null);
      const pendingVoiceAfterIntroRef = useRef(null);
      const voiceIntroResolveRef = useRef(null);
      const voiceAsrPopoverRef = useRef(null);
      const voiceAsrInstallWasActiveRef = useRef(false);
      const voiceAsrReadyNoticeTimerRef = useRef(null);
      const [voiceIntroOpen, setVoiceIntroOpen] = useState(false);
      const [voiceAsrPopoverOpen, setVoiceAsrPopoverOpen] = useState(false);
      const [voiceAsrReadyNotice, setVoiceAsrReadyNotice] = useState(false);
      const [voiceIntroSeenState, setVoiceIntroSeenState] = useState(() => voiceShortcutIntroSeen());
      const [voiceShortcutEnabledState, setVoiceShortcutEnabledState] = useState(() => voiceShortcutEnabled());
      const voiceShortcutEnabledRef = useRef(voiceShortcutEnabledState);
      useEffect(() => {
        voiceShortcutEnabledRef.current = voiceShortcutEnabledState;
      }, [voiceShortcutEnabledState]);
      useEffect(() => {
        function handleVoiceShortcutSettings() {
          setVoiceShortcutEnabledState(voiceShortcutEnabled());
        }
        // Detached windows (standalone window) write shortcut setting changes only to
        // localStorage, so this window never sees the CustomEvent and must sync via the
        // storage event. Same policy as the Router: only accept this feature's key.
        // event.key === null means localStorage.clear(), which says nothing about the
        // authoritative switch (Rust-side settings.json, replayed at startup); ignore it so a
        // cleared store's default-false mirror cannot overwrite the local display state.
        function handleVoiceShortcutStorage(event) {
          if (!event || event.key !== VOICE_SHORTCUT_ENABLED_KEY) return;
          handleVoiceShortcutSettings();
        }
        window.addEventListener(VOICE_SHORTCUT_SETTINGS_EVENT, handleVoiceShortcutSettings);
        window.addEventListener('storage', handleVoiceShortcutStorage);
        return () => {
          window.removeEventListener(VOICE_SHORTCUT_SETTINGS_EVENT, handleVoiceShortcutSettings);
          window.removeEventListener('storage', handleVoiceShortcutStorage);
        };
      }, []);
      useEffect(() => {
        if (!focusComposerTick) return undefined;
        const timer = window.setTimeout(() => {
          if (composerRef.current) composerRef.current.focus();
        }, 80);
        return () => window.clearTimeout(timer);
      }, [focusComposerTick]);
      // 输入框自动增高:随内容从最小(~2行)长到上限 160px,再内部滚动(iOS 手感)。
      // 清空(发送后)inputText 变 '' → 自动缩回最小高。
      useEffect(() => {
        const el = composerRef.current;
        if (!el) return;
        el.style.height = 'auto';
        el.style.height = Math.min(Math.max(el.scrollHeight, 48), 160) + 'px';
      }, [inputText]);
      // 输入框是浮动绝对定位,会随 auto-grow / 附件 / 排队 chips 变高 → 量它实际高度,
      // 动态给滚动区底部留白(= 输入框高 + 间距),保证最后几条消息永不被遮挡、也不浪费空间。
      const composerWrapRef = useRef(null);
      const [composerH, setComposerH] = useState(0);
      // 底栏响应式:输入框实际可用宽 < 阈值 → 控件收成纯图标;够宽 → 图标+文字(像 WorkBuddy)
      const [composerCompact, setComposerCompact] = useState(false);
      const COMPOSER_COMPACT_W = 660;
      useEffect(() => {
        const el = composerWrapRef.current;
        if (!el) return;
        const measure = () => { setComposerH(el.offsetHeight); setComposerCompact(el.clientWidth < COMPOSER_COMPACT_W); };
        measure();
        if (!window.ResizeObserver) return;
        const ro = new ResizeObserver(measure);
        ro.observe(el);
        return () => ro.disconnect();
      }, []);
      // Memoization keeps the reference stable: any domain change in the bs snapshot swaps in a new
      // object, but unchanged chatItems are shared by reference; without this layer the downstream
      // projection useMemo would invalidate every render in the empty state ([] literal).
      const chatItems = useMemo(() => (bs ? bs.chatItems : []), [bs]);
      // 远程模式下 ChatView 只承担输入/正文输出；权威数据仍来自 remote journal，
      // 本地会话的普通工具/产物流程不参与远程回合。
      const activeSessionId = bs ? bs.activeSessionId : null;
      const remoteProxyHistory = remoteProxyForSession(activeSessionId, bs?.remoteProxyHistory);
      const remoteChatActive = !!remoteProxyHistory;
      const controlledDevice = (bs && bs.controlledDevice) || {};
      const controlledChatActive = isControlledDeviceChatActive(controlledDevice);
      const controlledSession = (bs && bs.controlledSession) || {};
      const [remoteCapabilityBusy, setRemoteCapabilityBusy] = useState(false);
      const [remoteCapabilityError, setRemoteCapabilityError] = useState(null);
      const remoteCapabilityRequesterRef = useRef(null);
      const [remoteSendError, setRemoteSendError] = useState(null);
      const [remoteDispatchRevision, setRemoteDispatchRevision] = useState(0);
      const [remoteDispatchAction, setRemoteDispatchAction] = useState(null);
      const [remoteDispatchActionError, setRemoteDispatchActionError] = useState(null);
      const [remoteFiles, setRemoteFiles] = useState([]);
      const [remoteMode, setRemoteMode] = useState('chat');
      const [remoteWorkspaceId, setRemoteWorkspaceId] = useState('');
      const [remoteSkillIds, setRemoteSkillIds] = useState([]);
      const [controlledCapabilities, setControlledCapabilities] = useState(null);
      const [controlledSkillIds, setControlledSkillIds] = useState([]);
      const [controlledCapabilityBusy, setControlledCapabilityBusy] = useState(false);
      const [controlledCapabilityError, setControlledCapabilityError] = useState(null);
      const controlledCapabilityRequestRef = useRef(0);
      const [remotePreparedFiles, setRemotePreparedFiles] = useState(null);
      const [remoteTransfer, setRemoteTransfer] = useState(null);
      const [remoteTransferGuard] = useState(createRemoteTransferGuard);
      const [remotePermissions, setRemotePermissions] = useState([]);
      const [remoteArtifacts, setRemoteArtifacts] = useState([]);
      const [remoteArtifactPreview, setRemoteArtifactPreview] = useState(null);
      const [remoteActionError, setRemoteActionError] = useState(null);
      const remoteFileInputRef = useRef(null);
      const remoteArtifactPreviewDialogRef = useRef(null);
      const remoteArtifactPreviewCloseRef = useRef(null);
      const remoteArtifactPreviewTriggerRef = useRef(null);
      const remoteTranscriptActive = remoteChatActive || controlledChatActive;
      const remoteSendIdentity = remoteChatActive
        ? remoteProxyLeaseIdentity(activeSessionId, remoteConnection)
        : (controlledChatActive
          ? `controlled:${controlledDevice.generation || ''}:${controlledSession.sessionId || ''}`
          : '');
      const remoteSendIdentityRef = useRef(remoteSendIdentity);
      // eslint-disable-next-line react-hooks/refs -- async completion checks the latest committed binding
      remoteSendIdentityRef.current = remoteSendIdentity;
      const remoteComposerIdentity = remoteChatActive
        ? remoteProxyComposerIdentity(activeSessionId, remoteConnection)
        : remoteSendIdentity;
      const remoteComposerIdentityRef = useRef(remoteComposerIdentity);
      // eslint-disable-next-line react-hooks/refs -- failed sends restore only the current draft owner
      remoteComposerIdentityRef.current = remoteComposerIdentity;
      useEffect(() => {
        setRemoteSendError(current => (
          current && current.identity !== remoteSendIdentity ? null : current
        ));
      }, [remoteSendIdentity]);
      const projectedRemoteChatItems = useMemo(
        () => remoteChatActive
          ? remoteProxyChatItems(activeSessionId, remoteProxyHistory)
          : projectRemoteChatItems(
            controlledChatActive ? { status: 'connected', sessionId: controlledSession.sessionId } : null,
            bs ? bs.controlledTranscript : [],
            bs ? bs.controlledTasks : [],
          ),
        [
          remoteChatActive,
          activeSessionId,
          remoteProxyHistory,
          controlledChatActive,
          controlledSession.sessionId,
          bs && bs.controlledTranscript,
          bs && bs.controlledTasks,
        ],
      );
      const projectedControlledTaskItems = useMemo(
        () => projectControlledTaskChatItems(
          bs ? bs.controlledTasks : [],
          t.remoteConversation,
          controlledSession.sessionId || '',
        ),
        [bs && bs.controlledTasks, controlledSession.sessionId, t.remoteConversation],
      );
      const controlledSessionChatItems = useMemo(
        () => mergeControlledTaskChatItems(
          activeSessionId && activeSessionId === controlledSession.sessionId
            ? chatItems
            : projectedRemoteChatItems,
          projectedControlledTaskItems,
        ),
        [
          activeSessionId,
          controlledSession.sessionId,
          chatItems,
          projectedRemoteChatItems,
          projectedControlledTaskItems,
        ],
      );
      const displayChatItems = remoteChatActive
        ? projectedRemoteChatItems
        : (controlledChatActive ? controlledSessionChatItems : chatItems);
      const controlledActiveTasks = useMemo(
        () => ((bs && bs.controlledTasks) || []).filter(task => (
          task
          && task.sessionId === controlledSession.sessionId
          && (!activeSessionId || activeSessionId === controlledSession.sessionId)
          && !isTaskTerminal(task.status)
        )),
        [bs && bs.controlledTasks, controlledSession.sessionId, activeSessionId],
      );
      const showControlledTask = controlledDevice.status === 'connected' && controlledActiveTasks.length > 0;
      const activeSessionIdRef = useRef(activeSessionId);
      activeSessionIdRef.current = activeSessionId;
      const localBusy = bs ? bs.busy : false;
      const remoteBinding = useMemo(
        () => liveRemoteProxyBinding(activeSessionId, remoteProxyHistory, remoteConnection),
        [activeSessionId, remoteProxyHistory, remoteConnection],
      );
      const remoteHandshakeBinding = useMemo(
        () => remoteProxyHandshakeBinding(activeSessionId, remoteProxyHistory, remoteConnection),
        [activeSessionId, remoteProxyHistory, remoteConnection],
      );
      const remoteLinkReady = !!remoteBinding && remoteConnection.capabilityStatus !== 'error';
      const scopedRemoteConnection = remoteChatActive && remoteConnection?.localSessionId === activeSessionId
        ? remoteConnection : null;
      const remotePresentation = remoteProxyPresentationState(activeSessionId, remoteProxyHistory, scopedRemoteConnection);
      const remoteStatusLabel = remotePresentation === 'ready' ? t.controllerConnection.remoteStatus
        : remotePresentation === 'reconnecting' ? t.controllerConnection.stateReconnecting
          : remotePresentation === 'capability_error' ? t.controllerConnection.remoteCapabilityFailed
            : remotePresentation === 'preparing' ? t.controllerConnection.remoteSessionPreparing
              : t.remoteConversation.offlineCopy;
      const remoteTemporaryModelAvailable = canUseControllerTemporaryModel(scopedRemoteConnection);
      const effectiveRemoteModelSource = remoteMode === 'code'
        ? 'controlled_device'
        : normalizeRemoteModelSource(remoteModelSource, scopedRemoteConnection);
      const remoteModelBindingKey = remoteBinding
        ? remoteProxyModelSelectionIdentity(activeSessionId, scopedRemoteConnection)
        : '';
      const remoteArtifactPollRevision = remoteProjectionPollRevision(remoteProxyHistory,
        ['artifact', 'status', 'snapshot_marker']);
      const remotePermissionPollRevision = remoteProjectionPollRevision(remoteProxyHistory,
        ['approval', 'status', 'snapshot_marker']);
      const remoteCapabilities = bs?.remoteCapabilities
        && bs.remoteCapabilities.connectionId === scopedRemoteConnection?.connectionId
        && bs.remoteCapabilities.controllerLeaseId === scopedRemoteConnection?.controllerLeaseId
        && String(bs.remoteCapabilities.leaseEpoch || '') === String(scopedRemoteConnection?.leaseEpoch || '')
        && bs.remoteCapabilities.relayEpoch === scopedRemoteConnection?.relayEpoch
        && bs.remoteCapabilities.remoteControlGeneration === scopedRemoteConnection?.generation
        && bs.remoteCapabilities.remoteSessionRef === scopedRemoteConnection?.sessionRef
        && Number(bs.remoteCapabilities.capabilityRevision) === Number(scopedRemoteConnection?.capabilityRevision)
        ? bs.remoteCapabilities : null;
      const remoteModes = Array.isArray(remoteCapabilities?.modes) ? remoteCapabilities.modes : [];
      const remoteCapabilityFresh = Number(remoteCapabilities?.expiresAtMs) > Date.now();
      const remoteWorkspaces = Array.isArray(remoteCapabilities?.workspaces)
        ? remoteCapabilities.workspaces.filter(workspace => workspace?.codeAllowed === true) : [];
      const remoteSkills = Array.isArray(remoteCapabilities?.skills)
        ? remoteCapabilities.skills.filter(skill => Array.isArray(skill?.scopes)
          && skill.scopes.includes(remoteMode)) : [];
      const remoteTools = Array.isArray(remoteCapabilities?.tools) ? remoteCapabilities.tools : [];
      const pendingRemoteDispatches = useMemo(
        () => remoteChatActive ? remoteDispatchCases(activeSessionId, remoteProxyHistory) : [],
        [activeSessionId, remoteChatActive, remoteDispatchRevision, remoteProxyHistory],
      );
      const pendingPermissionCoordinationIds = remotePermissions
        .filter(permission => permission?.state === 'pending')
        .map(permission => permission.permissionRequestId);
      const attachmentCoordinationIds = pendingRemoteDispatches
        .filter(item => item?.state === 'prepared_not_sent')
        .map(item => item.taskId);
      const remoteUiCoordinationId = pendingPermissionCoordinationIds.length === 1
        ? pendingPermissionCoordinationIds[0]
        : (attachmentCoordinationIds.length === 1 ? attachmentCoordinationIds[0] : '');
      const currentRemoteTaskId = useMemo(
        () => remoteChatActive
          ? activeLiveRemoteTaskId(activeSessionId, remoteProxyHistory, scopedRemoteConnection)
          : null,
        [activeSessionId, remoteChatActive, remoteProxyHistory, scopedRemoteConnection],
      );
      const remoteModelListingRequestIdentity = remoteBinding ? remoteSendIdentity : '';
      const [remoteModelsForBinding, setRemoteModelsForBinding] = useState({ key: '', models: [] });
      const [remoteModelSelection, setRemoteModelSelection] = useState({ key: '', modelId: '' });
      useEffect(() => {
        let cancelled = false;
        if (!remoteModelBindingKey || !remoteModelListingRequestIdentity
          || !remoteTemporaryModelAvailable || !bridge.available) return undefined;
        const listingKey = remoteModelBindingKey;
        bridge.desktopRemoteControl.listExportableModels()
          .then(models => {
            if (!cancelled) setRemoteModelsForBinding({
              key: listingKey,
              models: (Array.isArray(models) ? models : []).filter(model => model?.exportable === true),
            });
          })
          .catch(() => {
            if (!cancelled) setRemoteModelsForBinding({ key: listingKey, models: [] });
          });
        return () => { cancelled = true; };
      }, [remoteModelBindingKey, remoteModelListingRequestIdentity, remoteTemporaryModelAvailable]);
      useEffect(() => {
        if (!remoteBinding || !scopedRemoteConnection?.sessionRef || !bridge.available) return undefined;
        let cancelled = false;
        bridge.desktopRemoteControl.capabilitiesSnapshot(scopedRemoteConnection.sessionRef)
          .catch(failure => {
            if (!cancelled) setRemoteCapabilityError(String(failure?.code || 'capability_snapshot_unavailable'));
          });
        return () => { cancelled = true; };
      }, [remoteModelBindingKey, scopedRemoteConnection?.capabilityRevision, scopedRemoteConnection?.sessionRef]);
      useEffect(() => {
        if (!remoteCapabilities || !scopedRemoteConnection?.sessionRef || !bridge.available) return undefined;
        const expiresAtMs = Number(remoteCapabilities.expiresAtMs);
        if (!Number.isSafeInteger(expiresAtMs) || expiresAtMs <= 0) return undefined;
        let cancelled = false;
        const refreshDelayMs = Math.max(0, expiresAtMs - Date.now() - 5_000);
        const timer = setTimeout(() => {
          if (cancelled) return;
          setRemoteCapabilityBusy(true);
          setRemoteCapabilityError(null);
          bridge.desktopRemoteControl.controllerCapabilitySend(scopedRemoteConnection.sessionRef)
            .then(() => { if (!cancelled) setRemoteCapabilityError(null); })
            .catch(failure => {
              if (!cancelled) setRemoteCapabilityError(String(failure?.code || 'capability_snapshot_unavailable'));
            })
            .finally(() => { if (!cancelled) setRemoteCapabilityBusy(false); });
        }, refreshDelayMs);
        return () => {
          cancelled = true;
          clearTimeout(timer);
        };
      }, [
        remoteCapabilities?.connectionId,
        remoteCapabilities?.controllerLeaseId,
        remoteCapabilities?.leaseEpoch,
        remoteCapabilities?.relayEpoch,
        remoteCapabilities?.remoteControlGeneration,
        remoteCapabilities?.remoteSessionRef,
        remoteCapabilities?.capabilityRevision,
        remoteCapabilities?.expiresAtMs,
        scopedRemoteConnection?.sessionRef,
      ]);
      useEffect(() => {
        if (!remoteCapabilities) return;
        setRemoteMode(current => remoteModes.includes(current) ? current : (remoteModes[0] || 'chat'));
        setRemoteWorkspaceId(current => remoteWorkspaces.some(item => item.workspaceId === current)
          ? current : (remoteWorkspaces[0]?.workspaceId || ''));
        setRemoteSkillIds(current => current.filter(id => remoteSkills.some(item => item.skillId === id)));
      }, [remoteCapabilities, remoteMode]);
      const exportableRemoteModels = remoteModelsForBinding.key === remoteModelBindingKey
        ? remoteModelsForBinding.models : [];
      const selectedRemoteModelId = selectedExportableRemoteModelId(
        remoteModelBindingKey, remoteModelsForBinding, remoteModelSelection, bs?.activeModelId,
      );
      const remoteModeReady = remoteModes.includes(remoteMode)
        && (remoteMode !== 'code' || remoteWorkspaces.some(item => item.workspaceId === remoteWorkspaceId));
      const remoteTaskContextReady = remoteLinkReady && remoteCapabilityFresh && remoteModeReady;
      const remoteModelError = remoteModelSelectionError(
        effectiveRemoteModelSource, selectedRemoteModelId,
      );
      useEffect(() => {
        setRemoteSendError(current => clearResolvedRemoteModelRequiredError(
          current, remoteSendIdentity, remoteModelError,
        ));
      }, [remoteModelError, remoteSendIdentity]);
      const remoteDispatchReady = remoteTaskContextReady && !remoteModelError;
      const remoteNewDispatchReady = remoteTaskContextReady
        && !pendingRemoteDispatches.some(item => item.state === 'prepared_not_sent');
      useEffect(() => {
        setRemoteFiles([]);
        setRemoteMode('chat');
        setRemoteWorkspaceId('');
        setRemoteSkillIds([]);
        setRemotePreparedFiles(null);
        setRemoteTransfer(null);
        remoteTransferGuard.invalidate();
        setRemotePermissions([]);
        setRemoteArtifacts([]);
        setRemoteArtifactPreview(null);
        setRemoteActionError(null);
      }, [remoteComposerIdentity, remoteTransferGuard]);
      useEffect(() => {
        if (!remoteArtifactPreview) return undefined;
        remoteArtifactPreviewCloseRef.current?.focus();
        return () => {
          const trigger = remoteArtifactPreviewTriggerRef.current;
          remoteArtifactPreviewTriggerRef.current = null;
          if (trigger?.isConnected && typeof trigger.focus === 'function') trigger.focus();
        };
      }, [remoteArtifactPreview]);
      useEffect(() => {
        if (!remoteBinding || !bridge.available) return undefined;
        let cancelled = false;
        bridge.desktopRemoteControl.artifactIndex(remoteBinding.localSessionId, remoteBinding.remoteRef)
          .then(items => { if (!cancelled) setRemoteArtifacts(Array.isArray(items) ? items : []); })
          .catch(failure => { if (!cancelled) setRemoteActionError(remoteActionFailureCode(failure)); });
        return () => { cancelled = true; };
      }, [remoteModelBindingKey, remoteArtifactPollRevision]);
      useEffect(() => {
        if (!remoteBinding || !currentRemoteTaskId || !bridge.available) {
          setRemotePermissions([]);
          return undefined;
        }
        let cancelled = false;
        bridge.desktopRemoteControl.permissionList(
          remoteBinding.localSessionId, remoteBinding.remoteRef, currentRemoteTaskId,
        ).then(items => {
          if (!cancelled) setRemotePermissions(Array.isArray(items) ? items : []);
        }).catch(failure => {
          if (!cancelled) setRemoteActionError(remoteActionFailureCode(failure));
        });
        return () => { cancelled = true; };
      }, [remoteModelBindingKey, currentRemoteTaskId, remotePermissionPollRevision]);
      const controlledLinkReady = !!(
        controlledChatActive
        && controlledSession.sessionId
        && controlledSession.generation
        && String(controlledSession.generation) === String(controlledDevice.generation || '')
      );
      const controlledCapabilityBound = !!(
        controlledCapabilities
        && controlledCapabilities.sessionId === controlledSession.sessionId
        && controlledCapabilities.remoteControlGeneration === controlledSession.generation
        && controlledCapabilities.remoteControlGeneration === controlledDevice.generation
        && (controlledCapabilities.sessionLane === 'chat' || controlledCapabilities.sessionLane === 'code')
        && controlledCapabilities.sessionLane === controlledSession.lane
        && Number.isSafeInteger(Number(controlledCapabilities.capabilityRevision))
        && Number(controlledCapabilities.expiresAtMs) > Date.now()
      );
      const controlledSkills = controlledCapabilityBound && Array.isArray(controlledCapabilities?.skills)
        ? controlledCapabilities.skills.filter(skill => Array.isArray(skill?.scopes)
          && skill.scopes.includes(controlledCapabilities.sessionLane)) : [];
      const loadControlledCapabilities = useCallback(() => {
        if (!controlledLinkReady || !controlledSession.sessionId || !bridge.available) {
          return Promise.resolve(null);
        }
        const request = controlledCapabilityRequestRef.current + 1;
        controlledCapabilityRequestRef.current = request;
        const identity = remoteSendIdentity;
        setControlledCapabilityBusy(true);
        setControlledCapabilityError(null);
        return bridge.desktopRemoteControl.controlledCapabilitiesSnapshot(controlledSession.sessionId)
          .then(snapshot => {
            if (controlledCapabilityRequestRef.current !== request
              || remoteSendIdentityRef.current !== identity) return null;
            setControlledCapabilities(snapshot || null);
            setControlledCapabilityError(null);
            return snapshot;
          })
          .catch(failure => {
            if (controlledCapabilityRequestRef.current === request
              && remoteSendIdentityRef.current === identity) {
              setControlledCapabilities(null);
              setControlledCapabilityError(String(failure?.code || 'capability_snapshot_unavailable'));
            }
            return null;
          })
          .finally(() => {
            if (controlledCapabilityRequestRef.current === request
              && remoteSendIdentityRef.current === identity) setControlledCapabilityBusy(false);
          });
      }, [controlledLinkReady, controlledSession.sessionId, remoteSendIdentity]);
      useEffect(() => {
        if (!controlledLinkReady || !bridge.available) {
          controlledCapabilityRequestRef.current += 1;
          setControlledCapabilities(null);
          setControlledSkillIds([]);
          setControlledCapabilityBusy(false);
          setControlledCapabilityError(null);
          return undefined;
        }
        void loadControlledCapabilities();
        return undefined;
      }, [controlledLinkReady, controlledSession.generation, controlledSession.lane, controlledSession.sessionId, loadControlledCapabilities]);
      useEffect(() => {
        if (!controlledCapabilityBound) return undefined;
        const exactSnapshot = controlledCapabilities;
        // A owns revision renewal. B performs a bounded 1 Hz observation during
        // the five-second lead window, so it adopts A's new exact snapshot
        // without minting a competing revision or creating a render storm.
        const delay = Math.max(1_000, Number(exactSnapshot.expiresAtMs) - Date.now() - 5_000);
        const timer = setTimeout(() => {
          if (remoteSendIdentityRef.current !== remoteSendIdentity) return;
          void loadControlledCapabilities();
        }, delay);
        return () => clearTimeout(timer);
      }, [
        controlledCapabilityBound,
        controlledCapabilities?.connectionId,
        controlledCapabilities?.controllerLeaseId,
        controlledCapabilities?.leaseEpoch,
        controlledCapabilities?.relayEpoch,
        controlledCapabilities?.remoteControlGeneration,
        controlledCapabilities?.sessionId,
        controlledCapabilities?.sessionLane,
        controlledCapabilities?.capabilityRevision,
        controlledCapabilities?.expiresAtMs,
        loadControlledCapabilities,
        remoteSendIdentity,
      ]);
      useEffect(() => {
        setControlledSkillIds(current => current.filter(id => (
          controlledSkills.some(skill => skill.skillId === id)
        )));
      }, [controlledCapabilities]);
      const busy = remoteChatActive
        ? (localBusy || remoteCapabilityBusy || !remoteLinkReady)
        : (controlledChatActive
          ? (localBusy || controlledCapabilityBusy || !controlledCapabilityBound)
          : localBusy);
      // Capability refresh/readiness may block sending without representing an
      // active turn. A follows its authenticated remote task; B follows only
      // the actual local generation state so completed turns stay done.
      const conversationBusy = remoteChatActive
        ? !!currentRemoteTaskId
        : (controlledChatActive ? localBusy : busy);
      const computerUseCopy = t.uiComputerUse;
      const computerUseSlice = (bs && bs.computerUse) || null;
      // Fetch the authoritative computer-use state on session mount/switch: the banner and
      // consent dialogs apply only to the active session; pending requests of background
      // sessions are parked per session on the bridge side and resurface with this refresh
      // when switching back. Before issuing the IPC, refreshStatus synchronously calls
      // clearSessionRequests to drop pending dialogs left by the previous session (a pure
      // state operation), so during the async refresh window the old session's consent
      // dialogs are no longer clickable.
      useEffect(() => {
        if (!COMPUTER_USE_ENABLED || !bridge.available || !bridge.computerUse || !activeSessionId) return;
        bridge.computerUse.refreshStatus(activeSessionId).catch(() => {});
        // Missed-event fallback: if grant/stop events were missed by the frontend (e.g. a
        // refresh or a backgrounded window), the banner would drift from the real
        // authorization state — this periodic refreshStatus reconciles with the backend,
        // recovering missed grant/stop events so the banner realigns (a pure recovery
        // mechanism; it does not change the authorization's own lifecycle).
        // Skip polling while there is no last-known state yet (nothing has
        // published a slice) or the last known state is disabled: a null
        // slice used to still fire an empty round-trip every 30s, and a
        // disabled feature has nothing to reconcile (the mount refresh above
        // covers cold start; re-enabling goes through setEnabled, which
        // re-reads the authoritative state directly and cannot miss the
        // state flip).
        const reconciler = setInterval(() => {
          try {
            const snapshot = bridge.state.get("computerUse");
            const slice = snapshot && snapshot.computerUse;
            if (!slice || slice.enabled === false) return;
          } catch {
            /* state.get unavailable: keep polling */
          }
          bridge.computerUse.refreshStatus(activeSessionId).catch(() => {});
        }, 30_000);
        return () => clearInterval(reconciler);
      }, [activeSessionId]);
      // 停止按钮 single-flight:busy 在首次 cancel_generation 返回前就复位,
      // 双击会发第二个并发取消请求。cancellingSessionIds 在 invoke 完成前禁用
      // **对应 session** 的按钮（按 session 集合记录而非全局布尔或单个 sid：
      // ChatView 在切换 active session 时不 remount，全局 single-flight 会阻断
      // 新会话的停止，直到旧会话的 invoke 返回；单个 sid 无法表示多个会话
      // 并发取消——A 取消中切到 B 发起取消再切回 A，A 的标记会被 B 覆盖导致
      // 按钮误启用。Set 让各会话独立记录，配合后端 turn generation 守护，
      // 消除跨轮误取消窗口）。
      const [cancellingSessionIds, setCancellingSessionIds] = useState(() => new Set());
      const hasMessages = displayChatItems.length > 0;
      const attachments = (bs && bs.attachments) || [];
      const formatAttachmentError = (error) => {
        const limitError = formatAttachmentLimitError(error, t.uiAttachments);
        if (limitError) return limitError;
        const raw = String(error || '');
        if (/under sensitive system dir|crosses sensitive (dir|component)/i.test(raw)) {
          return t.attachProtectedLocation;
        }
        return '';
      };
      // 排队待发消息（当前 session 生成中时积压）。Same stability contract as
      // chatItems above: the effect reconciling an interrupted queue edit
      // depends on this array by reference.
      const queued = useMemo(() => (bs && bs.queued) || [], [bs]);
      const [queuedEdits, setQueuedEdits] = useState({});
      const [queuedActions, setQueuedActions] = useState({});
      const [queuedActionErrors, setQueuedActionErrors] = useState({});
      const queuedEditInputRef = useRef(null);
      // ChatView survives session switches. Key transient queue UI state by
      // session instead of clearing it from an effect: a slow withdrawal in
      // session A must not disable or surface an error in newly active B.
      const queuedEditCandidate = activeSessionId ? queuedEdits[activeSessionId] || null : null;
      const queuedEdit = queuedEditCandidate && queued.some(item => item && item.id === queuedEditCandidate.id)
        ? queuedEditCandidate
        : null;
      const queuedAction = activeSessionId ? queuedActions[activeSessionId] || null : null;
      const queuedActionBusy = !!(queuedAction && queuedAction.id != null);
      const queuedActionErrorState = activeSessionId ? queuedActionErrors[activeSessionId] || null : null;
      const queuedActionError = queuedActionErrorState ? queuedActionErrorState.text : '';
      const queuedEditFocusKey = queuedEdit ? `${activeSessionId}:${queuedEdit.id}` : '';
      useLayoutEffect(() => {
        if (!queuedEditFocusKey || !queuedEditInputRef.current) return;
        queuedEditInputRef.current.focus();
        queuedEditInputRef.current.select();
      }, [queuedEditFocusKey]);
      // Shared flash for queued-action notices: write the keyed notice, then auto-delete after
      // the ttl. Deletion must verify entry identity (object reference): when a newer notice was
      // written to the same session in the meantime, a stale timer must not clear it. Timers are
      // deliberately not cancelled on unmount/session switch (existing contract) — the identity
      // guard stops stale deletes.
      const flashQueuedNotice = useCallback((sessionId, notice, ttl = 5000) => {
        setQueuedActionErrors(current => ({ ...current, [sessionId]: notice }));
        window.setTimeout(() => {
          setQueuedActionErrors(current => {
            if (current[sessionId] !== notice) return current;
            const next = { ...current };
            delete next[sessionId];
            return next;
          });
        }, ttl);
      }, []);
      // The queue drains itself on turn end while an editor may be open: the
      // item is sent with its pre-edit text, so a modified draft would be
      // discarded silently. Disclose it once through the shared action-error
      // notice. A successful save or an explicit cancel removes the entry
      // while the item is still queued, so neither lands here.
      useEffect(() => {
        if (!queuedEditCandidate || !activeSessionId) return;
        if (queued.some(item => item && item.id === queuedEditCandidate.id)) return;
        const lostEntry = queuedEditCandidate;
        const lostSessionId = activeSessionId;
        // eslint-disable-next-line react-hooks/set-state-in-effect -- the queue lives in the external bridge store; when a flushed item takes an open editor with it, the stale entry must be dropped and the lost draft disclosed in the same commit
        setQueuedEdits(current => {
          if (!current[lostSessionId] || current[lostSessionId].id !== lostEntry.id) return current;
          const next = { ...current };
          delete next[lostSessionId];
          return next;
        });
        if (String(lostEntry.text || '') !== String(lostEntry.initial || '')) {
          flashQueuedNotice(lostSessionId, { queuedId: lostEntry.id, text: t.queuedEditInterrupted });
        }
      }, [queuedEditCandidate, queued, activeSessionId, t, flashQueuedNotice]);
      const ctxTokens = (bs && bs.tokens) || null; // {input, max}，chat:usage 每轮更新
      const ctxPct = ctxTokens && ctxTokens.max > 0 ? ctxTokens.input / ctxTokens.max : 0;
      const artifactItems = (bs && bs.artifacts) || [];
      const artifactCount = artifactItems.length;
      const latestArtifact = artifactItems[artifactItems.length - 1] || null;
      // 当前会话仍在运行的后台 shell 任务（编译、下载等），驱动输入框上方的胶囊指示器。
      // chatItems 只含当前会话条目，且由 bridge 轮询保持新鲜，直接派生即可。
      const runningShellTasks = deriveRunningShellTasks(displayChatItems);
      const conversationStarted = displayChatItems.some(item => item && item.type === 'user') || (!remoteTranscriptActive && artifactCount > 0);
      const pinvouMode = pinvouModeState.mode;
      const sceneSubtab = pinvouModeState.subtab;
      const designScopeKey = createDesignChangeScopeKey(activeSessionId, activeArtifactPath);
      const designChanges = designChangesByScope[designScopeKey] || [];
      const visibleDesignChanges = uniqueDesignChanges(designChanges);
      const reduceCurrentDesignChanges = useCallback((action) => {
        setDesignChangesByScope((prev) => reduceScopedDesignChanges(prev, designScopeKey, action));
      }, [designScopeKey]);
      const updatePinvouModeState = useCallback((action) => {
        setPinvouModeState((prev) => {
          const next = savePinvouModeState(
            reducePinvouModeState(prev, action),
            undefined,
            pinvouModeScopeRef.current,
          );
          pinvouModeStateRef.current = next;
          return next;
        });
      }, []);
      const trackSceneEntry = useCallback((eventName, l1, l2) => {
        const fields = sceneRegistryFields(l1, l2);
        invokeTauri(isWeb ? 'web_access_track_behavior_event' : 'track_behavior_event', {
          request: {
            eventName,
            viewId: sceneViewIdRef.current,
            ...fields,
            entryPoint: 'composer',
          },
        }).catch(() => {});
      }, []);
      const trackSceneImpression = useCallback((l1, item) => {
        const key = `${l1}:${item.key}`;
        if (sceneImpressionsRef.current.has(key)) return;
        sceneImpressionsRef.current.add(key);
        trackSceneEntry('scene_impression', l1, item.key);
      }, [trackSceneEntry]);
      const trackWorkSceneImpression = useCallback(
        item => trackSceneImpression('work', item),
        [trackSceneImpression],
      );
      useEffect(() => {
        const nextScope = createPinvouModeScopeKey(activeSessionId);
        if (nextScope === pinvouModeScopeRef.current) return;

        const pending = pendingModeScopeMigrationRef.current;
        if (pending && activeSessionId) {
          const lastUser = [...chatItems].reverse().find((item) => item && item.type === 'user');
          if (!lastUser) return;
          pendingModeScopeMigrationRef.current = null;
          const lastUserText = String(lastUser.text || '').trim();
          if (
            !pending.text ||
            lastUserText === pending.text ||
            lastUserText.startsWith(`${pending.text}\n\n📎 `)
          ) {
            const migrated = savePinvouModeState(pending.state, undefined, nextScope);
            pinvouModeScopeRef.current = nextScope;
            pinvouModeStateRef.current = migrated;
            setPinvouModeState(migrated);
            return;
          }
        }

        const restored = loadPinvouModeState(undefined, nextScope);
        pinvouModeScopeRef.current = nextScope;
        pinvouModeStateRef.current = restored;
        setPinvouModeState(restored);
      }, [activeSessionId, chatItems.length]);
      // 把当前工作区 lane（work/code）同步给 bridge：草稿态 mode 的全局默认
      // 按 lane 两分（工作/代码各记各的，遗留 design 默认折叠进 work），
      // lane 是纯前端概念，bridge 自身不读 localStorage。
      useEffect(() => {
        if (bridge.available && bridge.interaction && bridge.interaction.setModeLane) {
          bridge.interaction.setModeLane(pinvouMode);
        }
      }, [pinvouMode]);
      useEffect(() => {
        // eslint-disable-next-line react-hooks/set-state-in-effect -- synchronously reset the locally selected element when the design scope switches or fullscreen editing ends (the fullscreen panel unmounts without running its own exit hook)
        setSelectedDesignElement(null);
      }, [designScopeKey, artifactsFullscreen]);
      const clearPersonalWorkbenchTemplateDraft = useCallback(() => {
        if (personalWorkbenchTemplateIdRef.current || findPersonalWorkbenchTemplateDraft(inputTextRef.current)) setInputText('');
        personalWorkbenchTemplateIdRef.current = null;
        setPersonalWorkbenchTemplateId(null);
      }, [setInputText]);
      const handlePinvouModeChange = useCallback((mode) => {
        updatePinvouModeState({ type: 'set-mode', mode });
      }, [updatePinvouModeState]);
      const handleHomeModeChange = useCallback((mode) => {
        if (mode === 'code') {
          if (onSwitchHomeMode) onSwitchHomeMode(mode);
          return;
        }
        handlePinvouModeChange(mode);
      }, [handlePinvouModeChange, onSwitchHomeMode]);
      const handleSubtabChange = useCallback((subtab) => {
        const nextSubtab = subtab === pinvouModeStateRef.current.subtab ? 'general' : subtab;
        if (nextSubtab !== 'general') trackSceneEntry('scene_selected', 'work', nextSubtab);
        if (nextSubtab !== PERSONAL_WORKBENCH_SCENE_KEY) clearPersonalWorkbenchTemplateDraft();
        updatePinvouModeState({
          type: 'set-subtab',
          subtab: nextSubtab,
        });
      }, [clearPersonalWorkbenchTemplateDraft, trackSceneEntry, updatePinvouModeState]);
      const handleClearActiveScene = useCallback(() => {
        clearPersonalWorkbenchTemplateDraft();
        updatePinvouModeState({ type: 'set-subtab', subtab: 'general' });
      }, [clearPersonalWorkbenchTemplateDraft, updatePinvouModeState]);
      const handlePersonalWorkbenchTemplateSelect = useCallback((index) => {
        const template = getPersonalWorkbenchTemplate(index);
        const normalized = template ? template.id : null;
        personalWorkbenchTemplateIdRef.current = normalized;
        setPersonalWorkbenchTemplateId(normalized);
        if (template) {
          setInputText(template.prompt);
          window.requestAnimationFrame(() => {
            if (composerRef.current) {
              composerRef.current.focus();
              composerRef.current.selectionStart = composerRef.current.value.length;
              composerRef.current.selectionEnd = composerRef.current.value.length;
            }
          });
        }
      }, [setInputText]);
      const handleComposerInputChange = useCallback((value) => {
        setInputText(value);
        if (!String(value || '').trim() && bridge.voice && typeof bridge.voice.abandonVoiceResult === 'function') {
          bridge.voice.abandonVoiceResult();
        }
        const currentTemplate = getPersonalWorkbenchTemplateById(personalWorkbenchTemplateIdRef.current);
        if (!currentTemplate) return;
        if (!isPersonalWorkbenchTemplateDraftForTemplate(value, currentTemplate)) {
          personalWorkbenchTemplateIdRef.current = null;
          setPersonalWorkbenchTemplateId(null);
        }
      }, [setInputText]);
      const handleDesignElementSelected = useCallback((element) => {
        setSelectedDesignElement(element || null);
      }, []);
      const handleApplyDesignChange = useCallback(({ type, property, oldValue, newValue }) => {
        if (!selectedDesignElement || !selectedDesignElement.selector) return;
        if (String(oldValue == null ? '' : oldValue) === String(newValue == null ? '' : newValue)) return;
        if (designChanges.some((change) => (
          change.selector === selectedDesignElement.selector &&
          change.type === type &&
          change.property === property &&
          change.oldValue === String(oldValue == null ? '' : oldValue) &&
          change.newValue === String(newValue == null ? '' : newValue)
        ))) return;
        const change = createDesignChange({
          element: selectedDesignElement,
          type,
          property,
          oldValue,
          newValue,
        });
        reduceCurrentDesignChanges({ type: 'add', change });
        setDesignCommand({
          seq: Date.now(),
          kind: 'apply',
          payload: {
            selector: selectedDesignElement.selector,
            changeId: change.id,
            changeType: type,
            property,
            oldValue,
            value: newValue,
          },
        });
        setSelectedDesignElement((prev) => {
          if (!prev) return prev;
          if (type === 'text') return { ...prev, text: String(newValue || '') };
          return {
            ...prev,
            computedStyle: {
              ...(prev.computedStyle || {}),
              [property]: String(newValue || ''),
            },
          };
        });
      }, [designChanges, reduceCurrentDesignChanges, selectedDesignElement]);
      const handleDesignChangeApplied = useCallback((result) => {
        if (!result || !result.changeId || result.changeId === 'clear') return;
        reduceCurrentDesignChanges({
          type: 'mark-applied',
          changeId: result.changeId,
          ok: result.ok,
          error: result.error,
        });
      }, [reduceCurrentDesignChanges]);
      const handleDesignMutation = useCallback((payload) => {
        const element = payload && payload.element;
        const changes = Array.isArray(payload && payload.changes) ? payload.changes : [];
        if (!element || !changes.length) return;
        const groupId = `design-group-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
        const groupLabel = payload.groupLabel || chatViewCopy.designEditGroup;
        changes.forEach((item) => {
          if (!item || String(item.oldValue == null ? '' : item.oldValue) === String(item.newValue == null ? '' : item.newValue)) return;
          const change = createDesignChange({
            element,
            type: item.type || 'style',
            property: item.property,
            oldValue: item.oldValue,
            newValue: item.newValue,
            groupId,
            groupLabel,
          });
          reduceCurrentDesignChanges({ type: 'add', change });
          reduceCurrentDesignChanges({ type: 'mark-applied', changeId: change.id, ok: true });
        });
        setSelectedDesignElement(element);
      }, [reduceCurrentDesignChanges, chatViewCopy]);
      const handleClearDesignChanges = useCallback(() => {
        setDesignCommand({ seq: Date.now(), kind: 'clear' });
        reduceCurrentDesignChanges({ type: 'clear' });
        setSelectedDesignElement(null);
      }, [reduceCurrentDesignChanges]);
      const visualPosterSceneActive = shouldUseVisualPosterScene(sceneSubtab);
      const documentWritingSceneActive = shouldUseDocumentWritingScene(sceneSubtab);
      const personalWorkbenchSceneActive = shouldUsePersonalWorkbenchScene(sceneSubtab);
      const dataVisualizationSceneActive = shouldUseDataVisualizationScene(sceneSubtab);
      const pptDesignSceneActive = shouldUsePptDesignScene(sceneSubtab);
      const activeScene = sceneSubtab === 'general'
        ? null
        : sceneTabs.find(item => item.key === sceneSubtab) || null;
      const composerPlaceholder = selectedDesignElement
        ? chatViewCopy.placeholderSceneAdjust
        : visualPosterSceneActive
          ? chatViewCopy.placeholderScenePoster
          : dataVisualizationSceneActive
            ? chatViewCopy.placeholderSceneDataViz
            : pptDesignSceneActive
              ? chatViewCopy.placeholderScenePpt
              : personalWorkbenchSceneActive
                ? chatViewCopy.placeholderPersonalWorkbench
                : documentWritingSceneActive
                  ? chatViewCopy.placeholderWorkDocument
                  : t.placeholder;
      const isScheduledTaskCreationChat = !!(bs && bs.scheduledTaskCreationSessionId && bs.activeSessionId === bs.scheduledTaskCreationSessionId);
      const scheduledRunContext = bs && bs.scheduledRunContext && bs.scheduledRunContext.sessionId === bs.activeSessionId
        ? bs.scheduledRunContext
        : null;
      // Conversation projection and derived collections. ChatView re-renders on every keystroke
      // (composer state), every streaming chunk, and every clock tick; this O(messages) group of
      // projection/filter/scan steps used to rerun in full inside the render body each time. Bridge
      // subscription snapshots share unchanged values by reference
      // (platform/tauri/bridge.js subscriptionStateValue), so memoizing on the input references
      // recomputes only when the underlying conversation data actually changes; the projection itself
      // is a read-only pure function (no in-place mutation).
      const chatThinking = bs ? bs.thinking : undefined;
      const turnTimeline = bs ? bs.turnTimeline : undefined;
      // Timeline error cards build friendly copy in the UI language and
      // derive the provider label from bridge state (providerLabelFromState
      // reads currentSessionModelId/activeModelId/savedModels/
      // effectiveModelConfig/activeProvider). bs is a whole-snapshot object
      // whose reference changes on every domain update; putting bs itself in
      // the projection memo deps would re-project the full transcript on
      // non-chat updates (updater/monitor). Same narrowing as CodexAcpView's
      // nativeModelServiceState.
      const modelServiceLanguage = bs && bs.settings && bs.settings.language;
      const chatModelServiceState = useMemo(
        () => (bs ? {
          currentSessionModelId: bs.currentSessionModelId,
          activeModelId: bs.activeModelId,
          savedModels: bs.savedModels,
          effectiveModelConfig: bs.effectiveModelConfig,
          activeProvider: bs.activeProvider,
        } : null),
        // eslint-disable-next-line react-hooks/exhaustive-deps -- track only the field references providerLabelFromState consumes, not the whole bs snapshot
        [bs && bs.currentSessionModelId, bs && bs.activeModelId, bs && bs.savedModels, bs && bs.effectiveModelConfig, bs && bs.activeProvider],
      );
      const derivedConversation = useMemo(() => {
        const visibleChatItems = displayChatItems.filter((item) => !(item.type === 'memory_candidate' && !item.resolved));
        const latestArtIdByPath = {};
        displayChatItems.forEach((item) => {
          if (item.type === 'artifact_card' && item.path) latestArtIdByPath[item.path] = item.id;
        });
        const latestArtifactIds = new Set(Object.values(latestArtIdByPath));
        // The Set is a fresh reference on every recompute; this additionally derives a value-compared
        // key (a string) for the renderItem stabilization below as a dep: the callback identity stays
        // stable while the set contents are unchanged.
        const latestArtifactIdsKey = Object.values(latestArtIdByPath).sort((left, right) => (
          left < right ? -1 : left > right ? 1 : 0
        )).join('\u0000');
        let lastUserId = null;
        for (let i = displayChatItems.length - 1; i >= 0; i--) { if (displayChatItems[i].type === 'user') { lastUserId = displayChatItems[i].id; break; } }
        // Swarm rework: consecutive spawn-type agent calls aggregate into one
        // count row (annotated with spawnGroup / spawnGroupHidden). Annotation
        // runs once on the projection input; the unified timeline lane reads
        // the same result through the projected items' legacyItem.
        const spawnAnnotatedItems = annotateAgentSpawnGroups(visibleChatItems);
        const conversationProjection = projectDeepSeekConversation({
          chatItems: conversationItemsForMode(spawnAnnotatedItems),
          busy: conversationBusy,
          thinking: chatThinking,
          tokens: ctxTokens,
          sessionId: activeSessionId,
          timelineEvents: turnTimeline,
          allowScheduledTaskDraft: isScheduledTaskCreationChat,
          language: modelServiceLanguage,
          modelServiceState: chatModelServiceState,
        });
        // Equivalent to [...turns].reverse().find(turn => turn.status === 'running'):
        // scan backwards for the last running turn, skipping the full reversed copy.
        let activeConversationTurn = null;
        const turns = conversationProjection.turns;
        for (let i = turns.length - 1; i >= 0; i--) {
          if (turns[i].status === 'running') { activeConversationTurn = turns[i]; break; }
        }
        return { latestArtifactIds, latestArtifactIdsKey, lastUserId, conversationProjection, activeConversationTurn };
      }, [displayChatItems, conversationBusy, ctxTokens, isScheduledTaskCreationChat, chatThinking, turnTimeline, activeSessionId, modelServiceLanguage, chatModelServiceState]);
      const { latestArtifactIds, latestArtifactIdsKey, lastUserId, conversationProjection, activeConversationTurn } = derivedConversation;

      // External entries can prefill the composer and focus its end.
      // Template/navigation entries (KnowledgeView "continue in chat",
      // scheduled-task guide, markdown preview, ...) use whole-draft
      // replacement semantics; failure recovery uses append semantics
      // (prefillAppend=true, joined with a newline) — the user may have
      // started the next message during the await and replacement would
      // clobber it (re-review #4: the two consumer classes stay separate;
      // recovery must not leak back into template entries).
      useEffect(() => {
        if (prefill) {
          const merged = (prefillAppend && inputTextRef.current)
            ? inputTextRef.current + '\n' + prefill
            : prefill;
          setInputText(merged);
          setTimeout(() => {
            if (composerRef.current) {
              composerRef.current.focus();
              composerRef.current.setSelectionRange(merged.length, merged.length);
            }
          }, 80);
          if (onPrefillConsumed) onPrefillConsumed();
        }
      // eslint-disable-next-line react-hooks/exhaustive-deps -- deps reviewed manually: prefill/prefillAppend only when they change; setInputText is a stable callback, adding onPrefillConsumed would retrigger consumption
      }, [prefill, prefillAppend]);

      // 用户向上翻历史时暂停流式自动贴底；回到底部或发送新消息后恢复。
      useEffect(() => {
        const el = scrollRef.current;
        if (!el) return;
        const onScroll = () => {
          const transition = transitionConversationScrollState({
            scrollElement: el,
            following: autoScrollRef.current,
            previousScrollTop: lastScrollTopRef.current,
            previousScrollHeight: lastScrollHeightRef.current,
          });
          lastScrollTopRef.current = transition.scrollTop;
          lastScrollHeightRef.current = transition.scrollHeight;
          autoScrollRef.current = transition.following;
          const shouldShow = !autoScrollRef.current && el.scrollHeight > el.clientHeight + 4;
          setShowScrollBottom(v => v === shouldShow ? v : shouldShow);
        };
        onScroll();
        el.addEventListener('scroll', onScroll, { passive: true });
        return () => el.removeEventListener('scroll', onScroll);
      }, []);

      function scrollChatToBottom() {
        const el = scrollRef.current;
        if (!el) return;
        autoScrollRef.current = true;
        setShowScrollBottom(false);
        el.scrollTo({ top: el.scrollHeight, behavior: 'smooth' });
      }

      // Auto-scroll：只在原本贴底时滚内部容器到底（绝不动外层窗口，避免浏览历史时被拉回底部）
      // Mid-turn steer: once the injected user bubble becomes the LAST item, the
      // streaming assistant item keeps growing ABOVE it — keying the follow only
      // on the last item's traits freezes live output visually until the turn
      // ends (length/last-html never change while only the streaming item
      // updates). Follow the last streaming/running item's traits as well.
      let streamingFollowHtml;
      let runningFollowOutputLength;
      for (let followIdx = displayChatItems.length - 1; followIdx >= 0; followIdx--) {
        const followItem = displayChatItems[followIdx];
        if (followItem?.streaming && streamingFollowHtml === undefined) {
          streamingFollowHtml = followItem.html;
        }
        if (followItem?.state === 'running' && runningFollowOutputLength === undefined) {
          runningFollowOutputLength = followItem.output?.length;
        }
        if (streamingFollowHtml !== undefined && runningFollowOutputLength !== undefined) break;
      }
      useEffect(() => {
        const el = scrollRef.current;
        if (!el) return;
        const lastItem = displayChatItems[displayChatItems.length - 1];
        // eslint-disable-next-line react-hooks/set-state-in-effect -- synchronously reset the back-to-bottom button when there are no messages
        if (!lastItem) { autoScrollRef.current = true; lastUserSnapLengthRef.current = 0; setShowScrollBottom(false); return; }
        // A mid-turn steered bubble parks a user item LAST while the turn's
        // streaming output above it keeps changing the follow traits this
        // effect depends on — the snap for it must fire once per appended
        // item, or every delta would re-force the bottom and overwrite the
        // scroll listener's "user scrolled up" state for the rest of the
        // turn (r13 review M1).
        if (shouldForceScrollFollow({
          following: autoScrollRef.current,
          lastItemType: lastItem.type,
          itemCount: displayChatItems.length,
          lastSnapItemCount: lastUserSnapLengthRef.current,
        })) {
          lastUserSnapLengthRef.current = displayChatItems.length;
          el.scrollTop = el.scrollHeight;
          autoScrollRef.current = true;
          setShowScrollBottom(false);
        } else {
          const shouldShow = el.scrollHeight > el.clientHeight + 4;
          setShowScrollBottom(v => v === shouldShow ? v : shouldShow);
        }
      }, [
        displayChatItems.length,
        displayChatItems[displayChatItems.length - 1]?.html,
        streamingFollowHtml,
        // eslint-disable-next-line react-hooks/exhaustive-deps -- streaming output length is an intentionally narrow conditional-expression dep
        displayChatItems[displayChatItems.length - 1]?.state === 'running'
          ? displayChatItems[displayChatItems.length - 1]?.output?.length
          : 0,
        runningFollowOutputLength,
        composerH,
      ]);

      // 切换/加载会话:无条件把新会话滚到最底部(最新消息)并复位 autoScrollRef。
      // 上面的流式 auto-scroll 复用了跨会话持久的 autoScrollRef + 不 remount 的滚动容器,
      // 若切走前在旧会话翻过历史(autoScrollRef=false),切来的新会话会命中 else 分支、停在
      // 残留 scrollTop 半空处。按 activeSessionId 单独滚底,且在流式 effect 之后声明→后跑覆盖它。
      useEffect(() => {
        const el = scrollRef.current;
        autoScrollRef.current = true;
        lastScrollTopRef.current = 0;
        setShowScrollBottom(false);
        if (el) {
          el.scrollTop = el.scrollHeight;
          lastScrollTopRef.current = el.scrollTop;
          lastScrollHeightRef.current = el.scrollHeight;
        }
      }, [bs && bs.activeSessionId]);

      // Session content can finish measuring after the active-session effect runs, especially
      // when an inactive WebView resumes or content-visibility replaces intrinsic estimates.
      // Keep following the bottom across those layout changes, but never override a user who
      // deliberately scrolled up.
      useEffect(() => {
        const scrollElement = scrollRef.current;
        const contentElement = conversationContentRef.current;
        if (!scrollElement || !contentElement) return undefined;
        return startConversationBottomFollower({
          scrollElement,
          contentElement,
          isFollowing: () => autoScrollRef.current,
          onMeasured: () => {
            const measurement = measureConversationScrollGeometry({
              scrollElement,
              following: autoScrollRef.current,
              previousScrollTop: lastScrollTopRef.current,
              previousScrollHeight: lastScrollHeightRef.current,
            });
            lastScrollTopRef.current = measurement.scrollTop;
            lastScrollHeightRef.current = measurement.scrollHeight;
          },
          onRestored: (scrollTop) => {
            lastScrollTopRef.current = scrollTop;
            lastScrollHeightRef.current = scrollElement.scrollHeight;
            setShowScrollBottom(false);
          },
        });
      }, [activeSessionId, hasMessages]);

      // 安装工具后新建会话 → 本地显示欢迎卡片（不发 LLM query，不浪费 token）。
      // welcomeToolId 是一次性引导态,必须跟随会话身份:只有"装完工具"(justInstalledTool 非
      // null)才显示;其余任何新建对话/切换会话(activeSessionId 变)都清掉,否则残留的工具卡会
      // 顶掉「你好」欢迎语(该 tool 无 welcomeQueries 时 ToolWelcomeCard 渲染 null → 整块空白)。
      // 设置与清空收进同一 effect,按 justInstalledTool 优先,避免多 effect 同帧竞态。
      const [welcomeToolId, setWelcomeToolId] = useState(null);
      // sendChatMessage's useCallback must not depend on welcomeToolId (avoids
      // identity-churn rebuilds); the free-input path consumes the current
      // welcome pack through this ref (review #455 R8-2).
      const welcomeToolIdRef = useRef(null);
      // Round-16 minor 13: in-flight welcome opt-in attempt ({ toolId,
      // promise } | null) shared across concurrent sends — a send arriving
      // during the first enable's await window joins it instead of no-op'ing
      // and later clearing the first send's failure banner.
      const welcomeOptInAttemptRef = useRef(null);
      const welcomeSessionKeyRef = useRef(null);
      // Web 只读判定：多智能体是桌面专属能力（ADR-0006），Web 端只读呈现。
      // modeState.multiAgent 经 get_mode_state 双端同步（开关已持久化）。
      const isMultiAgentReadOnly = !MULTI_AGENT_ENABLED
        && !!(bs && bs.modeState && bs.modeState.multiAgent);
      // Read-only mirror of the swarm mode switch: mood border color of the
      // top-right running overlay (on = purple / off = blue).
      const swarmModeOn = !!(bs && bs.modeState && bs.modeState.multiAgent);
      const artifactsVisible = Boolean(activeSessionId && artifactsOpen && !remoteTranscriptActive);
      const artifactFullscreenPublicationReady = useRightDockOcclusion(
        'artifact-fullscreen',
        artifactsVisible && artifactsFullscreen,
      );
      useEffect(() => {
        if (designAiSessionRef.current && designAiSessionRef.current !== activeSessionId) {
          updateDesignAiState({ text: '', status: 'idle', lastPrompt: '', pendingPath: '', startedAt: 0 });
          if (typeof window !== 'undefined') window.__PINVOU_DESIGN_AI_STATE__ = null;
        }
        designAiSessionRef.current = activeSessionId || null;
      }, [activeSessionId, updateDesignAiState]);
      useEffect(() => {
        if (!artifactsFullscreen || typeof window === 'undefined') return;
        const saved = window.__PINVOU_DESIGN_AI_STATE__;
        if (!saved || (!saved.text && !saved.lastPrompt && saved.status === 'idle')) return;
        if (!designAiState.text && !designAiState.lastPrompt && designAiState.status === 'idle') {
          setDesignAiState(saved);
        }
      }, [artifactsFullscreen, designAiState.text, designAiState.lastPrompt, designAiState.status]);
      useEffect(() => {
        if (!activeSessionId) setArtifactsOpen(false);
        if (!activeSessionId) setArtifactsFullscreen(false);
      }, [activeSessionId]);
      useEffect(() => {
        if (!artifactsVisible) setArtifactsFullscreen(false);
      }, [artifactsVisible]);
      const closeArtifactsPanel = useCallback(() => {
        setArtifactsFullscreen(false);
        setArtifactsOpen(false);
        if (browserDockOpen) {
          void invokeObservedPanelSelection(
            onRightDockPanelSelectionChange,
            ['browser', activeSessionId],
            reportRightDockSelectionFailure,
          );
        }
      }, [activeSessionId, browserDockOpen, onRightDockPanelSelectionChange]);
      // Read-only subagent transcript panel (Codex-style right column, ADR-0006).
      // It is available in every work session, including bare agents in normal chats.
      // null means closed; an empty agentId opens the list. selectionRequestId makes
      // detail -> list -> same parent card a new selection even when agentId is unchanged.
      const [subagentPanel, setSubagentPanel] = useState(null);
      const subagentPanelRequestRef = useRef(0);
      // The aux chat panel's mount state is declared early: the scroll-restore
      // useLayoutEffect below depends on it (opening/closing the panel changes
      // the right dock layout, so the main conversation scroll position must
      // be restored, same as in CodexAcpView).
      const [auxChatPanel, setAuxChatPanel] = useState(null);
      // The panel's real visibility in the dock (same mechanism as
      // CodexAcpView's auxChatDockActive): when another dock panel covers the
      // aux panel the entry pill does not highlight, and clicking it again
      // brings the panel to the front via the openTick bump.
      const [auxChatDockActive, setAuxChatDockActive] = useState(false);
      const auxChatPanelRequestRef = useRef(0);
      // eslint-disable-next-line react-hooks/set-state-in-effect -- synchronously close the sub-agent panel on session switch
      useEffect(() => { setSubagentPanel(null); }, [activeSessionId]);
      // When the mount condition drops (sched- session / no active session /
      // bridge or auxChat domain unavailable), the panel unmounts outright and
      // RightDockPanel's onActiveChange has no unmount cleanup, so the
      // highlight would linger; reset it here and let the re-mounted panel
      // report its real visibility again. The conjuncts mirror the entry
      // button and the mount gate exactly (round-26 minor M4).
      useEffect(() => {
        if (auxChatPanel && activeSessionId && !activeSessionId.startsWith('sched-')
          && bridge.available && bridge.auxChat) return;
        // eslint-disable-next-line react-hooks/set-state-in-effect -- synchronously reset dock highlight when the panel unmounts; one-shot mirror, same pattern as CodexAcpView
        setAuxChatDockActive(false);
      }, [auxChatPanel, activeSessionId]);
      // The panel stays mounted across main-session switches (it rebinds), but
      // the recorded dock-restore target must not: it was captured against the
      // PREVIOUS task's dock context, so applying it to the new task pops a
      // stale panel (the native browser webview included) over whatever the
      // user is on (round-30 D3). The subagent panel gets the same reset by
      // closing outright on the switch.
      useEffect(() => {
        // eslint-disable-next-line react-hooks/set-state-in-effect -- one-shot reset of the restore target on session switch; the subagent panel's close-on-switch is the same mirror
        setAuxChatPanel((current) => (current ? { ...current, restorePanelId: null } : current));
      }, [activeSessionId]);
      const rememberScrollBeforeSubagentPanelChange = useCallback(() => {
        subagentPanelScrollRef.current = captureConversationScrollPosition(
          scrollRef.current,
          autoScrollRef.current,
        );
      }, []);
      const closeSubagentPanel = useCallback(() => {
        const requestId = subagentPanelRequestRef.current + 1;
        subagentPanelRequestRef.current = requestId;
        const requestedSessionId = activeSessionId;
        const restorePanelId = subagentPanel?.restorePanelId || null;
        const publishClose = ({ isCurrent = () => true } = {}) => {
          if (!isSubagentPanelPublicationCurrent({
            transitionCurrent: isCurrent(),
            requestId,
            currentRequestId: subagentPanelRequestRef.current,
            sessionId: requestedSessionId,
            currentSessionId: activeSessionIdRef.current,
          })) return false;
          rememberScrollBeforeSubagentPanelChange();
          setSubagentPanel(null);
          return true;
        };
        if (browserDockOpen && restorePanelId && onRightDockPanelSelectionChange) {
          return invokeObservedPanelSelection(
            onRightDockPanelSelectionChange,
            [restorePanelId, requestedSessionId, publishClose],
            reportRightDockSelectionFailure,
          );
        }
        return publishClose();
      }, [
        activeSessionId,
        browserDockOpen,
        onRightDockPanelSelectionChange,
        rememberScrollBeforeSubagentPanelChange,
        subagentPanel,
      ]);
      useLayoutEffect(() => {
        const snapshot = subagentPanelScrollRef.current;
        if (!snapshot) return;
        subagentPanelScrollRef.current = null;
        const el = scrollRef.current;
        if (!el) return;
        restoreConversationScrollPosition(el, snapshot);
        lastScrollTopRef.current = el.scrollTop;
        lastScrollHeightRef.current = el.scrollHeight;
        if (snapshot.stickToBottom) {
          autoScrollRef.current = true;
          setShowScrollBottom(false);
        }
      }, [subagentPanel, auxChatPanel]);
      useEffect(() => {
        if (typeof window === 'undefined') return undefined;
        const onOpen = (event) => {
          const detail = event && event.detail;
          if (detail?.sessionId && detail.sessionId !== activeSessionId) return;
          const requestedSessionId = activeSessionId;
          if (!requestedSessionId) return;
          const requestId = subagentPanelRequestRef.current + 1;
          subagentPanelRequestRef.current = requestId;
          prefetchChatPanel('subagent');
          const publishOpen = ({ isCurrent = () => true } = {}) => {
            if (!isSubagentPanelPublicationCurrent({
              transitionCurrent: isCurrent(),
              requestId,
              currentRequestId: subagentPanelRequestRef.current,
              sessionId: requestedSessionId,
              currentSessionId: activeSessionIdRef.current,
            })) return false;
            rememberScrollBeforeSubagentPanelChange();
            setSubagentPanel((current) => ({
              agentId: (detail && detail.agentId) || null,
              selectionRequestId: (current?.selectionRequestId || 0) + 1,
              // Re-selecting another agent while this panel is already active
              // must retain the dock that preceded the first open. App has
              // already moved browserPaneSelected away from `browser` by then.
              restorePanelId: current
                ? current.restorePanelId
                : browserDockOpen ? rightDockActivePanelId : null,
            }));
            return true;
          };
          if (onRightDockPanelSelectionChange) {
            void invokeObservedPanelSelection(
              onRightDockPanelSelectionChange,
              ['subagent-transcript', requestedSessionId, publishOpen],
              reportRightDockSelectionFailure,
            );
          } else {
            publishOpen();
          }
        };
        window.addEventListener('pinvou:open-subagent', onOpen);
        return () => {
          subagentPanelRequestRef.current += 1;
          window.removeEventListener('pinvou:open-subagent', onOpen);
        };
      }, [
        activeSessionId,
        browserDockOpen,
        onRightDockPanelSelectionChange,
        rememberScrollBeforeSubagentPanelChange,
        rightDockActivePanelId,
      ]);
      // Aux chat panel (an independent Q&A-only conversation in the right
      // dock, outside the sub-agent system). Unlike subagentPanel: it does
      // **not** close on main-session switch; the new sessionId is passed in
      // to rebind. Before opening/closing, record the main conversation
      // scroll position (subagentPanelScrollRef); the restore useLayoutEffect
      // above replays it uniformly, matching the aux entry behavior in
      // CodexAcpView.
      const openAuxChatPanel = useCallback(() => {
        const requestedSessionId = activeSessionId;
        if (!requestedSessionId) return;
        const requestId = auxChatPanelRequestRef.current + 1;
        auxChatPanelRequestRef.current = requestId;
        prefetchChatPanel('auxChat');
        const publishOpen = ({ isCurrent = () => true } = {}) => {
          if (!isSubagentPanelPublicationCurrent({
            transitionCurrent: isCurrent(),
            requestId,
            currentRequestId: auxChatPanelRequestRef.current,
            sessionId: requestedSessionId,
            currentSessionId: activeSessionIdRef.current,
          })) return false;
          rememberScrollBeforeSubagentPanelChange();
          setAuxChatPanel((current) => ({
            openTick: (current?.openTick || 0) + 1,
            // Remember the dock panel active at first open and jump back to
            // it on close (same pattern as the subagent panel; repeat opens
            // must keep the first record, or the restore target would be
            // overwritten by the panel itself).
            restorePanelId: current
              ? current.restorePanelId
              : browserDockOpen ? rightDockActivePanelId : null,
          }));
          return true;
        };
        if (onRightDockPanelSelectionChange) {
          void invokeObservedPanelSelection(
            onRightDockPanelSelectionChange,
            ['aux-chat', requestedSessionId, publishOpen],
            reportRightDockSelectionFailure,
          );
        } else {
          publishOpen();
        }
      }, [activeSessionId, browserDockOpen, onRightDockPanelSelectionChange, rememberScrollBeforeSubagentPanelChange, rightDockActivePanelId]);
      const closeAuxChatPanel = useCallback(() => {
        const requestId = auxChatPanelRequestRef.current + 1;
        auxChatPanelRequestRef.current = requestId;
        const requestedSessionId = activeSessionId;
        const restorePanelId = auxChatPanel?.restorePanelId || null;
        // Publish with the same currency guard as closeSubagentPanel
        // (round-26 minor M5): the dock restore below is async, so a rapid
        // close→open could settle out of order — the stale close's restore
        // would land after the open's publication and leave the dock on the
        // restore panel with the aux panel mounted but occluded. The
        // requestId/session currency check makes the stale publication inert.
        const publishClose = ({ isCurrent = () => true } = {}) => {
          if (!isSubagentPanelPublicationCurrent({
            transitionCurrent: isCurrent(),
            requestId,
            currentRequestId: auxChatPanelRequestRef.current,
            sessionId: requestedSessionId,
            currentSessionId: activeSessionIdRef.current,
          })) return false;
          rememberScrollBeforeSubagentPanelChange();
          setAuxChatPanel(null);
          return true;
        };
        if (browserDockOpen && restorePanelId && onRightDockPanelSelectionChange) {
          return invokeObservedPanelSelection(
            onRightDockPanelSelectionChange,
            [restorePanelId, requestedSessionId, publishClose],
            reportRightDockSelectionFailure,
          );
        }
        return publishClose();
      }, [activeSessionId, auxChatPanel, browserDockOpen, onRightDockPanelSelectionChange, rememberScrollBeforeSubagentPanelChange]);
      const handlePreviewArtifact = useCallback((artifact) => {
        setActiveArtifactPath(artifact && artifact.path ? artifact.path : null);
        setArtifactDockActivation((value) => value + 1);
      }, []);
      const showArtifactsPreview = useCallback((path) => {
        prefetchChatPanel('artifacts');
        if (path) setActiveArtifactPath(path);
        setArtifactsOpen(true);
        setArtifactDockActivation((value) => value + 1);
        void invokeObservedPanelSelection(
          onRightDockPanelSelectionChange,
          ['artifact-preview', activeSessionId],
          reportRightDockSelectionFailure,
        );
      }, [activeSessionId, onRightDockPanelSelectionChange]);
      const openArtifactsPreview = useCallback(() => {
        showArtifactsPreview(latestArtifact && latestArtifact.path);
      }, [latestArtifact, showArtifactsPreview]);
      useEffect(() => {
        if (typeof window === 'undefined') return undefined;
        const onPresentArtifact = (event) => {
          const detail = event && event.detail;
          if (!detail || detail.sessionId !== activeSessionId || !detail.path) return;
          showArtifactsPreview(detail.path);
        };
        window.addEventListener('pinvou:present-artifact', onPresentArtifact);
        return () => window.removeEventListener('pinvou:present-artifact', onPresentArtifact);
      }, [activeSessionId, showArtifactsPreview]);
      const draftEpoch = bs ? bs.draftEpoch : 0;
      // Round-31 m5 (review #455): the async send-flow guards compare the
      // CURRENT draft epoch at write time; the callback closure would go
      // stale, so mirror the epoch in a ref (same pattern as
      // activeSessionIdRef above).
      // Capability preparation and first-turn materialization can outlive the
      // originating voice composer; the dispatch guard compares the live epoch
      // at await boundaries (a ref, not the render value).
      const draftEpochRef = useRef(draftEpoch);
      draftEpochRef.current = draftEpoch;
      // 切换 session / 新建草稿会话时读取各自 working set 里的未发送内容。
      // 从设置、工具商店等页面返回时 ChatView 会重新挂载，初始 state 也从
      // 同一份内存草稿恢复。
      useEffect(() => {
        const restored = bridge.available && bridge.chat && bridge.chat.getComposerDraft
          ? bridge.chat.getComposerDraft()
          : ((bs && bs.composerDraft) || '');
        setInputText(restored);
      }, [activeSessionId, draftEpoch, setInputText]);
      const voiceInput = (bs && bs.voiceInput) || { status: 'idle' };
      const voiceMode = normalizeVoiceMode(voiceInput.mode);
      const voiceActive = isVoiceActive(voiceInput);
      const voiceBusy = isVoiceBusy(voiceInput);
      const hasDraftText = inputText.trim().length > 0;
      const hasReadyAttachment = attachments.some(a => a.status === 'ready');
      const hasRemoteFile = remoteChatActive && remoteFiles.length > 0;
      const firstTurnPending = !activeSessionId && chatItems.some(item => (
        item && item.type === 'user' && !!item.deliveryState
      ));
      const canSend = !isMultiAgentReadOnly
        && !firstTurnPending
        && (remoteChatActive ? (hasDraftText || hasRemoteFile)
          : (controlledChatActive ? (hasDraftText || hasReadyAttachment)
            : (hasDraftText || hasReadyAttachment)));
      const sceneCapabilityPreparing = sceneCapabilityStatus && sceneCapabilityStatus.kind === 'preparing';
      const canClearInput = hasDraftText && !voiceActive;
      function handleClearInput() {
        if (!canClearInput) return;
        handleComposerInputChange('');
        composerRef.current?.focus();
      }
      // eslint-disable-next-line sonarjs/cognitive-complexity -- scene-capability preflight and send orchestration are cohesive in a single callback; split refactor tracked separately
      const dispatchChatMessage = useCallback(async (text, voiceMeta, voiceOwner) => {
        // Round-29 m8 (review #455): this flow awaits IPC between status
        // writes — a slow welcome enable settling after a session switch
        // must not render its banner (or ready toast) on the wrong
        // session's composer. Capture the session at send start and let
        // only same-session writes through; the switch effect clears the
        // slot outright. Round-31 m5: the key is the FULL session identity
        // (`session:draftEpoch`, the exact key the welcome-card reset
        // builds) — drafts are both `null` for activeSessionId, so a send
        // in flight from draft D1 could otherwise still paint into fresh
        // draft D2 (a new-chat click bumps the epoch while the session id
        // stays null→null).
        const sceneStatusSession = sceneStatusKey(activeSessionIdRef.current, draftEpochRef.current);
        const sceneStatusKeyNow = () =>
          sceneStatusKey(activeSessionIdRef.current, draftEpochRef.current);
        const setSceneStatusForSession = (status) => {
          if (sceneStatusKeyNow() === sceneStatusSession) {
            setSceneCapabilityStatus(status);
          }
        };
        // Both welcome-card send paths (sample-question click / free input)
        // complete the opt-in here (review #455 R8-2; logic extracted into
        // welcome-optin.js for direct testing): failure must not block the
        // send but must stay fail-visible — banner notice + console trace
        // (review #455 R9-M4); the tool's absence is likewise visible in the reply.
        // Official adaptation: the await only happens when a welcome card is
        // actually pending (toolId queued or an attempt in flight) — a plain
        // send stays synchronous so the draft-ownership timing the voice lane
        // pins (voice_operation_lifecycle.test.mjs) is unchanged.
        const welcomeOptIn = (welcomeToolIdRef.current || welcomeOptInAttemptRef.current)
          ? await runSharedWelcomeOptIn(welcomeOptInAttemptRef, {
            toolId: welcomeToolIdRef.current,
            run: () => consumeWelcomeOptIn({
              getToolId: () => welcomeToolIdRef.current,
              consume: () => {
                welcomeToolIdRef.current = null;
                setWelcomeToolId(null);
              },
              invoke: invokeTauri,
            }),
          })
          : { failed: false, blocked: [] };
        if (welcomeOptIn.failed) {
          console.warn("[pinvou3][chat-ui] welcome-card opt-in failed:", welcomeOptIn.error);
        }
        if (welcomeOptIn.blocked && welcomeOptIn.blocked.length) {
          // The welcome pack was explicitly switched off by the user: abort the
          // send with guidance (same contract as the scene blocked path,
          // round-10 Major 2) instead of sending a reply without the tool.
          setSceneStatusForSession({
            kind: 'error',
            text: t.uiChatScenes.switchedOffPacks(welcomeOptIn.blocked.join(', ')),
          });
          return false;
        }
        // The scene block resolves its status into a local; the single
        // setSceneCapabilityStatus below combines it with the welcome opt-in
        // result — an earlier welcome-error set would be batched away by any
        // later synchronous set in the same run (round-10 Major 1).
        let sceneStatus = null;
        let readyAutoClear = false;
        console.warn('DBG post-welcome');

        if (!bridge.available) return false;
        const outgoing = String(text || '').trim();
        if (controlledChatActive && !remoteChatActive) {
          if (!controlledLinkReady || !controlledCapabilityBound || controlledCapabilityBusy) return false;
          const sendIdentity = remoteSendIdentity;
          const readyAttachments = attachments.filter(attachment => (
            attachment?.status === 'ready' && attachment.result
          ));
          const taskText = outgoing || t.remoteConversation.attachmentOnlyPrompt;
          try {
            await bridge.desktopRemoteControl.controlledTaskSend(
              controlledSession.sessionId,
              taskText,
              readyAttachments,
              controlledSkillIds,
            );
            setRemoteSendError(current => (
              current && current.identity === sendIdentity ? null : current
            ));
          } catch (failure) {
            setRemoteSendError({
              identity: sendIdentity,
              code: String((failure && failure.code) || 'errorGeneric'),
            });
            throw failure;
          }
          return true;
        }
        if (remoteChatActive) {
          // 会话引用只表示 A/B 通道已建立；Rust 仅在能力快照已经应用后才允许
          // 构造 task intent。发送按钮、回车、语音与欢迎卡片最终都经过这里，
          // 因此必须在统一入口拦截，不能只依赖按钮 disabled。
          if (!remoteLinkReady) return false;
          const sendIdentity = remoteSendIdentity;
          if (!remoteBinding || !remoteNewDispatchReady) {
            setRemoteSendError({ identity: sendIdentity, code: 'remote_task_intent_invalid' });
            return false;
          }
          if (remoteModelError) {
            setRemoteSendError({ identity: sendIdentity, code: remoteModelError });
            return false;
          }
          const transferAttempt = remoteTransferGuard.start(sendIdentity);
          const currentTransfer = () => remoteTransferGuard.isCurrent(transferAttempt, remoteSendIdentityRef.current);
          try {
            const taskText = outgoing || t.remoteConversation.attachmentOnlyPrompt;
            const intent = newRemoteTaskIntent(
              taskText,
              effectiveRemoteModelSource,
              selectedRemoteModelId,
              remoteMode,
              remoteMode === 'code' ? remoteWorkspaceId : null,
              remoteSkillIds,
            );
            if (!intent) return false;
            const prepared = await bridge.desktopRemoteControl.reserveRemoteTask(
              remoteBinding.localSessionId, remoteBinding.remoteRef, intent,
            );
            const preparedOutcome = recordRemoteDispatch(
              remoteBinding.localSessionId, remoteBinding.remoteRef, sendIdentity, taskText, prepared,
              remoteProxyHistory,
            );
            setRemoteDispatchRevision(value => value + 1);
            if (preparedOutcome.state !== 'prepared_not_sent' || !preparedOutcome.handle) return 'held';
            if (!currentTransfer()) return 'held';
            const selectedFiles = remoteFiles.slice();
            setRemotePreparedFiles({ taskId: preparedOutcome.handle.taskId, files: selectedFiles });
            let attachmentIds = [];
            if (selectedFiles.length) {
              setRemoteTransfer({ owner: transferAttempt, state: 'uploading', fileName: selectedFiles[0].name, receivedBytes: 0, totalBytes: selectedFiles[0].size });
              attachmentIds = await uploadRemoteFiles(
                bridge.desktopRemoteControl, remoteBinding, preparedOutcome.handle.taskId, selectedFiles,
                progress => setRemoteTransfer(current => currentTransfer()
                  ? { owner: transferAttempt, state: 'uploading', ...progress } : current),
              );
              if (!currentTransfer()) return 'held';
              setRemotePreparedFiles({ taskId: preparedOutcome.handle.taskId, files: selectedFiles, attachmentIds });
            }
            const report = await bridge.desktopRemoteControl.commitRemoteTask(
              remoteBinding.localSessionId, remoteBinding.remoteRef, preparedOutcome.handle, attachmentIds,
            );
            await bridge.desktopRemoteControl.readRemoteProxyHistory(remoteBinding.localSessionId).catch(() => null);
            const outcome = recordRemoteDispatch(
              remoteBinding.localSessionId, remoteBinding.remoteRef, sendIdentity, taskText, report,
              remoteProxyHistory,
            );
            setRemoteDispatchRevision(value => value + 1);
            setRemoteSendError(current => current?.identity === sendIdentity ? null : current);
            if (currentTransfer()) {
              setRemotePreparedFiles(null);
              setRemoteFiles(current => current === remoteFiles ? [] : current);
            }
            if (outcome.state !== 'queued') return 'held';
          } catch (failure) {
            if (remoteSendIdentityRef.current === sendIdentity) {
              setRemoteSendError({ identity: sendIdentity, code: String(failure?.code || 'errorGeneric') });
            }
            throw failure;
          } finally {
            if (remoteTransferGuard.finish(transferAttempt)) {
              setRemoteTransfer(current => current?.owner === transferAttempt ? null : current);
            }
          }
          return true;
        }
        const matchedPersonalWorkbenchDraft = findPersonalWorkbenchTemplateDraft(outgoing);
        const templateId = personalWorkbenchTemplateIdRef.current
          || (matchedPersonalWorkbenchDraft && matchedPersonalWorkbenchDraft.template
            ? matchedPersonalWorkbenchDraft.template.id
            : null);
        const visibleOutgoing = outgoing;
        let meta;
        if (visibleOutgoing || hasReadyAttachment) {
          const scenePrompt = outgoing || t.uiChatScenes.attachmentPrompt;
          if (visualPosterSceneActive) meta = createVisualPosterMessageMeta(scenePrompt);
          else if (documentWritingSceneActive) meta = createDocumentWritingMessageMeta(scenePrompt);
          else if (personalWorkbenchSceneActive) meta = createPersonalWorkbenchMessageMeta(scenePrompt, templateId);
          else if (dataVisualizationSceneActive) meta = createDataVisualizationMessageMeta(scenePrompt);
          else if (pptDesignSceneActive) meta = createPptDesignMessageMeta(scenePrompt);
        }
        // The voice task lane hands its operation id in alongside the text; it
        // must ride on meta or the bridge's submission gate never sees it and
        // a dispatched send can never settle its operation as accepted.
        if (voiceMeta && voiceMeta.voiceOperationId != null) meta = Object.assign({}, meta, voiceMeta);
        const requirements = requiredCapabilitiesForMeta(meta);
        if (requirements) {
          const sceneCopy = t.uiChatScenes[requirements.key];
          if (canPrepareSceneCapabilities({ isWebHost: isWeb, dependencyInstallAvailable: can('dependencyInstall') })) {
            setSceneStatusForSession({ kind: 'preparing', text: sceneCopy.preparing });
            try {
              const prepared = await prepareSceneCapabilities(meta, invokeTauri);
              if (!prepared.ok) {
                // Round-11 m8: the banner carries translated copy only — raw
                // backend/IPC error strings are diagnostics, traced to the
                // console instead of rendered untranslated to the user.
                if (prepared.error) {
                  console.warn('[pinvou3][chat-ui] scene capability prepare failed:', prepared.error);
                }
                // Round-16 minor 13: notApplied (installed but matched no
                // expansion entry) gets its own retry-inviting copy — the
                // missingCapabilities branch would invite a reinstall that
                // cannot help.
                const detail = prepared.blocked && prepared.blocked.length
                  ? t.uiChatScenes.switchedOffPacks(prepared.blocked.join(', '))
                  : (prepared.notApplied && prepared.notApplied.length
                    ? t.uiChatScenes.notAppliedPacks(prepared.notApplied.join(', '))
                    : (prepared.missing && prepared.missing.length
                      ? t.uiChatScenes.missingCapabilities(prepared.missing.join(', '))
                      : ''));
                // Round-13 m2: the welcome card is one-shot — if its opt-in
                // failed, a later resend never re-attempts it, so the welcome
                // failure must win over the scene failure copy here (the
                // scene preflight re-runs and resurfaces on the next send;
                // the welcome failure otherwise never surfaces at all).
                setSceneStatusForSession({
                  kind: 'error',
                  text: welcomeOptIn.failed
                    ? t.uiChat.welcomeOptInFailed
                    : (detail || sceneCopy.failure),
                });
                return false;
              }
              // Post-DenyAll, ready means installed or explicitly opted back
              // in: a pack gated off by default completes its opt-in here and
              // gets the same enabled toast (#455 R5-B3).
              if (prepared.installed || prepared.optedIn) {
                sceneReadyToastEpochRef.current += 1;
                sceneStatus = {
                  kind: 'ready',
                  text: sceneCopy.ready,
                  toastEpoch: sceneReadyToastEpochRef.current,
                };
                readyAutoClear = true;
              }
              // else: leave the local null — nothing to show.
            } catch (error) {
              console.warn('[pinvou3][chat-ui] scene capability prepare raised:', error);
              setSceneStatusForSession({
                kind: 'error',
                  text: welcomeOptIn.failed
                    ? t.uiChat.welcomeOptInFailed
                    : sceneCopy.failure,
              });
              return false;
            }
          }
        }
        console.warn('DBG pre-closer', JSON.stringify({ active: activeSessionIdRef.current, epoch: draftEpochRef.current }));
        setSceneStatusForSession(resolveSendCapabilityStatus({
          welcomeFailed: welcomeOptIn.failed,
          welcomeText: t.uiChat.welcomeOptInFailed,
          sceneStatus,
        }));
        if (readyAutoClear) {
          // The session guard inside the timeout keeps this stale closer
          // from clearing a NEWER session's fresh ready toast.
          window.setTimeout(() => setSceneCapabilityStatus((current) => (
            current && current.kind === 'ready'
              && current.toastEpoch === sceneStatus.toastEpoch
              && sceneStatusKeyNow() === sceneStatusSession
              ? null
              : current
          )), 1800);
        }
        // Capability preparation can outlive the originating voice composer.
        // Do not let the bridge resolve its current active session as our owner.
        if (voiceOwner && ((activeSessionIdRef.current || null) !== voiceOwner.sessionId
          || (!voiceOwner.sessionId && Number(draftEpochRef.current || 0) !== voiceOwner.draftEpoch))) return false;
        if (!activeSessionId) {
          pendingModeScopeMigrationRef.current = {
            text: visibleOutgoing,
            state: pinvouModeStateRef.current,
          };
        }
        // Capability preparation can outlive the originating voice composer.
        // Do not let the bridge resolve its current active session as our owner.
        if (voiceOwner && ((activeSessionIdRef.current || null) !== voiceOwner.sessionId
          || (!voiceOwner.sessionId && Number(draftEpochRef.current || 0) !== voiceOwner.draftEpoch))) {
          // Mirror the catch path: a refused dispatch must not leave the
          // stale mode-scope migration payload installed for a later session.
          pendingModeScopeMigrationRef.current = null;
          return false;
        }
        let dispatchResult;
        try {
          dispatchResult = await bridge.chat.sendMessage(visibleOutgoing, meta, voiceOwner);
        } catch (error) {
          pendingModeScopeMigrationRef.current = null;
          throw error;
        }
        // sendMessage resolves true only when something was actually
        // dispatched (sent / steered / queued). false marks notice-only early
        // returns (attachments still parsing, remote-turn sync block): nothing
        // was sent and the composer text was not put back, so the caller must
        // restore it — returning false routes the resolve through handleSend's
        // empty-vs-typed restore instead of silently dropping the draft
        // (#406). "restored" marks paths that already returned the text to the
        // composer (first-turn materialization abort, session switch);
        // restoring again would duplicate it. The three-state value is kept
        // as-is: the voice task lane needs to tell "restored" apart from
        // accepted, while ordinary sends treat both as not-false.
        return dispatchResult;
      }, [activeSessionId, attachments, controlledCapabilityBound, controlledCapabilityBusy, controlledChatActive, controlledLinkReady, controlledSession.sessionId, controlledSkillIds, dataVisualizationSceneActive, documentWritingSceneActive, effectiveRemoteModelSource, hasReadyAttachment, personalWorkbenchSceneActive, pptDesignSceneActive, remoteBinding, remoteChatActive, remoteFiles, remoteMode, remoteModelError, remoteNewDispatchReady, remoteProxyHistory, remoteSendIdentity, remoteSkillIds, remoteTransferGuard, remoteWorkspaceId, selectedRemoteModelId, t, visualPosterSceneActive]);

      // SessionStore mapping/lease establishment is owned by the A-side
      // authenticated ensure/bind command, never by an implicit ChatView open.
      useEffect(() => {
        const requester = createCapabilityRequestLoop({
          send: (sessionRef) => bridge.desktopRemoteControl.controllerCapabilitySend(sessionRef),
          onBusy: (busy) => {
            setRemoteCapabilityBusy(busy);
            if (busy) setRemoteSendError(null);
          },
          onError: setRemoteCapabilityError,
        });
        remoteCapabilityRequesterRef.current = requester;
        return () => {
          requester.dispose();
          remoteCapabilityRequesterRef.current = null;
        };
      }, []);

      useEffect(() => {
        const requester = remoteCapabilityRequesterRef.current;
        if (!requester) return;
        const sessionRef = remoteConnection?.sessionRef || null;
        const requestKey = [
          remoteConnection?.connectionId,
          remoteConnection?.leaseEpoch,
          remoteConnection?.remoteControlGeneration,
          sessionRef,
        ].map((part) => String(part || '')).join(':');
        requester.update({
          enabled: remoteChatActive
            && !!remoteHandshakeBinding
            && !!sessionRef
            && (remoteConnection?.capabilityRevision == null || remoteConnection?.mappingConfirmed !== true)
            && remoteConnection?.capabilityStatus !== 'error',
          key: requestKey,
          sessionRef,
        });
      }, [
        remoteChatActive,
        remoteHandshakeBinding,
        remoteConnection?.capabilityRevision,
        remoteConnection?.capabilityStatus,
        remoteConnection?.connectionId,
        remoteConnection?.leaseEpoch,
        remoteConnection?.remoteControlGeneration,
        remoteConnection?.sessionRef,
        remoteConnection?.status,
      ]);
      const requestRemoteCapabilities = useCallback(() => {
        const sessionRef = remoteConnection?.sessionRef;
        if (!remoteHandshakeBinding || !sessionRef) return;
        if (remoteCapabilityBusy) return;
        setRemoteCapabilityBusy(true);
        setRemoteCapabilityError(null);
        setRemoteSendError(null);
        bridge.desktopRemoteControl.controllerCapabilitySend(sessionRef)
          .then(() => setRemoteCapabilityError(null))
          .catch((failure) => setRemoteCapabilityError(String((failure && failure.code) || 'errorGeneric')))
          .finally(() => setRemoteCapabilityBusy(false));
      }, [remoteHandshakeBinding, remoteCapabilityBusy, remoteConnection]);
      useEffect(() => {
        if (remoteConnection?.capabilityError) {
          setRemoteCapabilityError(String(remoteConnection.capabilityError));
        } else if (remoteConnection?.capabilityStatus === 'ready') {
          setRemoteCapabilityError(null);
        }
      }, [remoteConnection?.capabilityError, remoteConnection?.capabilityStatus]);
      const remoteCapabilityMessage = !scopedRemoteConnection ? null
        : remoteCapabilityError === 'capability_snapshot_timeout'
          ? t.controllerConnection.remoteCapabilityTimeout
          : (remoteCapabilityError ? t.controllerConnection.remoteCapabilityFailed : null);
      const currentRemoteSendError = remoteSendError?.identity === remoteSendIdentity
        ? remoteSendError.code
        : null;
      const activeRemoteDispatchError = remoteDispatchActionError?.localSessionId === activeSessionId
        ? remoteDispatchActionError.code : null;
      async function commitPreparedRemoteTask(item) {
        if (item.origin !== 'process' || item.state !== 'prepared_not_sent'
          || !item.handle || !remoteDispatchReady || !remoteBinding
          || item.localSessionId !== remoteBinding.localSessionId
          || item.remoteRef !== remoteBinding.remoteRef
          || item.identity !== remoteSendIdentity) return;
        setRemoteDispatchAction(item.taskId);
        setRemoteDispatchActionError(null);
        const transferAttempt = remoteTransferGuard.start(item.identity);
        const currentTransfer = () => remoteTransferGuard.isCurrent(transferAttempt, remoteSendIdentityRef.current);
        try {
          const preparedFiles = remotePreparedFiles?.taskId === item.taskId ? remotePreparedFiles : null;
          let attachmentIds = preparedFiles?.attachmentIds || [];
          if (!attachmentIds.length && preparedFiles?.files?.length) {
            setRemoteTransfer({ owner: transferAttempt, state: 'uploading', receivedBytes: 0, totalBytes: preparedFiles.files[0].size, fileName: preparedFiles.files[0].name });
            attachmentIds = await uploadRemoteFiles(
              bridge.desktopRemoteControl, remoteBinding, item.taskId, preparedFiles.files,
              progress => setRemoteTransfer(current => currentTransfer()
                ? { owner: transferAttempt, state: 'uploading', ...progress } : current),
            );
            if (!currentTransfer()) return;
            setRemotePreparedFiles({ ...preparedFiles, attachmentIds });
          }
          const report = await bridge.desktopRemoteControl.commitRemoteTask(
            item.localSessionId, item.remoteRef, item.handle, attachmentIds,
          );
          await bridge.desktopRemoteControl.readRemoteProxyHistory(item.localSessionId).catch(() => null);
          const echoed = report?.retryHandle;
          const sameHandle = echoed?.taskId === item.handle.taskId
            && echoed?.clientRequestId === item.handle.clientRequestId
            && echoed?.requestFingerprint === item.handle.requestFingerprint;
          recordRemoteDispatch(item.localSessionId, item.remoteRef, item.identity, item.text,
            sameHandle ? report : { state: 'outcome_unknown', retryHandle: item.handle },
            remoteProxyHistory);
          if (currentTransfer()) {
            setRemotePreparedFiles(null);
            setRemoteFiles(current => current === remoteFiles ? [] : current);
          }
          setRemoteDispatchRevision(value => value + 1);
        } catch (failure) {
          if (currentTransfer()) setRemoteDispatchActionError({ localSessionId: item.localSessionId, code: String(failure?.code || 'errorGeneric') });
        } finally {
          if (remoteTransferGuard.finish(transferAttempt)) {
            setRemoteTransfer(current => current?.owner === transferAttempt ? null : current);
            setRemoteDispatchAction(current => current === item.taskId ? null : current);
          }
        }
      }
      async function abandonRemoteTask(item) {
        if (item.state !== 'prepared_not_sent' || !item.handle) return;
        setRemoteDispatchAction(item.taskId);
        setRemoteDispatchActionError(null);
        try {
          await bridge.desktopRemoteControl.abandonRemoteTask(
            item.localSessionId, item.handle.clientRequestId, item.handle.taskId,
          );
          forgetRemoteDispatch(item.localSessionId, item.taskId);
          if (item.text) {
            bridge.chat.restoreTaskDraft(item.text, { sessionId: item.localSessionId, restored: false });
          }
          setRemoteDispatchRevision(value => value + 1);
          setRemotePreparedFiles(current => current?.taskId === item.taskId ? null : current);
          void bridge.desktopRemoteControl.readRemoteProxyHistory(item.localSessionId).catch(() => {});
        } catch (failure) {
          markRemoteDispatchPayloadLost(item.localSessionId, item.taskId);
          await bridge.desktopRemoteControl.readRemoteProxyHistory(item.localSessionId).catch(() => null);
          setRemoteDispatchRevision(value => value + 1);
          setRemoteDispatchActionError({ localSessionId: item.localSessionId, code: String(failure?.code || 'errorGeneric') });
        } finally {
          setRemoteDispatchAction(null);
        }
      }
      async function cancelCurrentRemoteTask() {
        if (!remoteBinding || !currentRemoteTaskId || remoteDispatchAction) return;
        setRemoteDispatchAction(currentRemoteTaskId);
        setRemoteDispatchActionError(null);
        try {
          await bridge.desktopRemoteControl.cancelRemoteTask(
            remoteBinding.localSessionId, remoteBinding.remoteRef, currentRemoteTaskId,
          );
          await bridge.desktopRemoteControl.readRemoteProxyHistory(remoteBinding.localSessionId).catch(() => null);
        } catch (failure) {
          setRemoteDispatchActionError({ localSessionId: remoteBinding.localSessionId, code: String(failure?.code || 'errorGeneric') });
        } finally {
          setRemoteDispatchAction(null);
        }
      }
      function selectRemoteFiles(event) {
        try {
          setRemoteFiles(validateRemoteFiles(event.target.files));
          setRemoteActionError(null);
        } catch (failure) {
          setRemoteActionError(String(failure?.message || 'remote_attachment_invalid'));
        } finally {
          event.target.value = '';
        }
      }
      async function decideRemotePermission(permission, decision) {
        if (!remoteBinding || permission.state !== 'pending') return;
        setRemoteDispatchAction(permission.permissionRequestId);
        setRemoteActionError(null);
        try {
          await bridge.desktopRemoteControl.permissionDecide(
            remoteBinding.localSessionId, remoteBinding.remoteRef, permission.taskId,
            permission.permissionRequestId, decision,
          );
          setRemotePermissions(current => current.filter(item => item.permissionRequestId !== permission.permissionRequestId));
        } catch (failure) {
          setRemoteActionError(remoteActionFailureCode(failure));
        } finally {
          setRemoteDispatchAction(null);
        }
      }
      async function previewRemoteArtifact(artifact) {
        if (!remoteBinding) return;
        remoteArtifactPreviewTriggerRef.current = document.activeElement;
        setRemoteDispatchAction(artifact.artifactId);
        setRemoteActionError(null);
        try {
          const preview = await bridge.desktopRemoteControl.artifactPreview(
            remoteBinding.localSessionId, remoteBinding.remoteRef, artifact.artifactId,
          );
          setRemoteArtifactPreview({ ...preview, displayName: artifact.displayName });
        } catch (failure) {
          setRemoteActionError(remoteActionFailureCode(failure));
        } finally {
          setRemoteDispatchAction(null);
        }
      }
      function closeRemoteArtifactPreview() {
        setRemoteArtifactPreview(null);
      }
      async function saveRemoteArtifact(artifact) {
        if (!remoteBinding) return;
        setRemoteDispatchAction(artifact.artifactId);
        setRemoteActionError(null);
        try {
          const blob = await downloadRemoteArtifact(bridge.desktopRemoteControl, remoteBinding, artifact);
          saveRemoteArtifactBlob(blob, artifact.displayName);
        } catch (failure) {
          setRemoteActionError(remoteActionFailureCode(failure));
        } finally {
          setRemoteDispatchAction(null);
        }
      }
      const remoteSendMessage = currentRemoteSendError === 'task_session_busy'
        ? t.remoteConversation.sessionBusy
        : (currentRemoteSendError === 'temporary_model_capability_required'
          ? t.remoteConversation.temporaryModelCapabilityRequired
          : (currentRemoteSendError === 'remote_model_required'
            ? t.remoteConversation.modelRequired
            : (currentRemoteSendError === 'model_credential_unavailable'
              ? t.remoteConversation.controlledModelCredentialUnavailable
              : (currentRemoteSendError ? t.remoteConversation.errorGeneric : null))));
      const sendChatMessage = useCallback(async (text, voiceContext) => {
        const operationId = voiceContext?.operationId
          || (bridge.voice?.getVoiceOperationId
            ? bridge.voice.getVoiceOperationId(activeSessionId || null, 'chat')
            : null);
        if (operationId && bridge.voice?.beginVoiceSubmission) bridge.voice.beginVoiceSubmission(operationId);
        try {
          const accepted = await dispatchChatMessage(text, { voiceOperationId: operationId }, voiceContext?.draftOwner);
          if ((accepted === false || accepted === 'restored' || accepted === 'held') && operationId
            && bridge.voice?.completeVoiceSubmission) {
            bridge.voice.completeVoiceSubmission(operationId, null, false);
          }
          // Preserve "restored": ordinary send must not restore twice, while
          // voice task delivery must treat it as not accepted.
          return accepted;
        } catch (error) {
          if (operationId && bridge.voice?.completeVoiceSubmission) {
            bridge.voice.completeVoiceSubmission(operationId, null, false);
          }
          throw error;
        }
      }, [activeSessionId, dispatchChatMessage]);
      // ConversationTimeline render-callback stabilization: ConversationTurn is React.memoized, so a
      // per-render callback identity would make every turn fully re-render each time. Callbacks only
      // rebuild identity when their inputs change; the latestArtifactIds Set is a fresh reference on
      // every projection recompute, so the dep is the value-stable latestArtifactIdsKey and the latest
      // set is read through a ref at render time (the callback only runs during actual rendering, by
      // which point the ref already points at the committed projection result).
      const latestArtifactIdsRef = useRef(latestArtifactIds);
      latestArtifactIdsRef.current = latestArtifactIds;
      const handleTimelineRenderUser = useCallback((item) => (
        <ChatBubble
          item={item}
          sessionId={activeSessionId}
          theme={theme}
          t={t}
          editable={!busy && !isMultiAgentReadOnly && !item.remote && item.id === lastUserId}
          conversationVariant="unified"
        />
      ), [activeSessionId, busy, isMultiAgentReadOnly, lastUserId, t, theme]);
      const handleTimelineRenderItem = useCallback((item) => {
        // reasoning items are handled by ConversationTimeline's ReasoningItem and must not be handed to
        // the legacy ChatBubble; the latter does not know the type and would return null, silently
        // swallowing real-time thinking the backend already delivered.
        if (item.type === 'reasoning') return;
        if (!item.legacyItem) return;
        return (
          <ChatBubble
            item={item.legacyItem}
            sessionId={activeSessionId}
            theme={theme}
            t={t}
            onPrefill={setInputText}
            onSend={sendChatMessage}
            onOpenEditor={onOpenEditor}
            onPlanStuckGo={handlePlanStuckGo}
            isLatestArtifact={latestArtifactIdsRef.current.has(item.legacyItem.id)}
            allowScheduledTaskDraft={isScheduledTaskCreationChat} showAssistantActions={false}
          />
        );
      // eslint-disable-next-line react-hooks/exhaustive-deps -- latestArtifactIdsKey is an intentional extra dep: a content-keyed proxy for the artifact-id Set (read fresh via latestArtifactIdsRef) so the callback identity only changes when the set contents change
      }, [activeSessionId, isScheduledTaskCreationChat, latestArtifactIdsKey, onOpenEditor, sendChatMessage, setInputText, t, theme, handlePlanStuckGo]);
      const handleTimelineRenderToolItem = useCallback((item) => (item.legacyItem
        && !isSearchTool(item.tool)
        && !isFetchTool(item.tool)
        ? <ToolCard
            item={item.legacyItem}
            sessionId={activeSessionId}
            t={t}
            variant="timeline"
          />
        : undefined), [activeSessionId, t]);
      const timelineAssistantAvatar = useMemo(() => (
        <div className="mt-1 flex h-7 w-7 shrink-0 items-center justify-center">
          <PinvouLogo className="h-5 w-5" title={chatViewCopy.agentName} />
        </div>
      ), [chatViewCopy.agentName]);
      const handleDesignAiSubmit = useCallback((text) => {
        const raw = String(text || '').trim();
        if (!raw) return;
        const elementLabel = selectedDesignElement
          ? `${selectedDesignElement.tagName || chatViewCopy.designElementFallback}${selectedDesignElement.className ? `.${String(selectedDesignElement.className).trim().split(/\s+/)[0]}` : ''}`
          : '';
        const scopedText = selectedDesignElement
          ? chatViewCopy.designAdjustSelected(elementLabel || chatViewCopy.designElementFallback, raw)
          : raw;
        sendChatMessage(scopedText);
      }, [selectedDesignElement, sendChatMessage]);
      const primaryVoiceDisabled = !bridge.available || voiceBusy || (remoteChatActive && !remoteLinkReady);
      const voiceAsrSetup = (bs && bs.voiceAsrSetup) || { open: false };
      const voiceAsrSetupPublicationReady = useRightDockOcclusion(
        'voice-asr-setup',
        !!voiceAsrSetup.open && canInstallLocalAsr
      );
      const voiceAsrBusy = !!(voiceAsrSetup.installing || voiceAsrSetup.cancelling);
      const voiceAsrProgress = voiceAsrSetup.progress || {};
      const voiceInputRef = useRef(voiceInput);
      voiceInputRef.current = voiceInput;
      // Mounted-state ref: after unmount/view switch, reject in-flight ASR/LLM callbacks from
      // writing back (the hook's isStillActive defense line).
      const composerMountedRef = useRef(true);
      useEffect(() => {
        composerMountedRef.current = true;
        return () => { composerMountedRef.current = false; };
      }, []);
      // Return focus to the composer input after recording ends / writeback completes
      // (leaving any of the four active states).
      const voiceWasActiveRef = useRef(false);
      useEffect(() => {
        const wasActive = voiceWasActiveRef.current;
        voiceWasActiveRef.current = voiceActive;
        if (wasActive && !voiceActive && composerRef.current) {
          composerRef.current.focus();
        }
      }, [voiceActive]);
      useEffect(() => () => {
        if (voiceAsrReadyNoticeTimerRef.current) window.clearTimeout(voiceAsrReadyNoticeTimerRef.current);
        if (voiceIntroResolveRef.current) {
          voiceIntroResolveRef.current(false);
          voiceIntroResolveRef.current = null;
        }
        const voice = voiceInputRef.current;
        if (voice && isVoiceActive(voice) && bridge.available) {
          bridge.voice.cancelVoiceInput();
        }
      }, []);
      useEffect(() => {
        // eslint-disable-next-line react-hooks/set-state-in-effect -- close the install popover whenever the busy state clears; mirrors main's dialog dismiss pattern
        if (!voiceAsrBusy) setVoiceAsrPopoverOpen(false);
      }, [voiceAsrBusy]);
      // Outside-click close for the ASR progress popover is handled inside VoiceComposerButton
      // via useOutsidePointerClose (passed in via onCloseAsrPopover); the view no longer
      // attaches duplicate listeners.
      useEffect(() => {
        const sessionKey = sceneStatusKey(activeSessionId, draftEpoch);
        if (justInstalledTool) {
          setWelcomeToolId(justInstalledTool);
          welcomeToolIdRef.current = justInstalledTool;
          welcomeSessionKeyRef.current = sessionKey;
          if (setJustInstalledTool) setJustInstalledTool(null);
        } else if (welcomeSessionKeyRef.current && welcomeSessionKeyRef.current !== sessionKey) {
          setWelcomeToolId(null);
          welcomeToolIdRef.current = null;
          welcomeSessionKeyRef.current = null;
          // Round-2 review: drop any in-flight opt-in attempt slot from the
          // previous session — a send in a new card-less session must never
          // join it (it would inherit the previous pack's failure banner).
          welcomeOptInAttemptRef.current = null;
        }
        // justInstalledTool stays in the deps (a one-shot directive; parent
        // rerenders do not retrigger: the effect clears it immediately via
        // setJustInstalledTool(null), and re-entry takes the else branch, which
        // only clears the card on session-key change). Deps: activeSessionId
        // (session switch) + draftEpoch (incremented per "New chat" click) — the
        // latter forces re-evaluation even when "New chat" is clicked again
        // while already in draft state (activeSessionId stays null→null);
        // otherwise a leftover tool card would displace the "Hello" greeting.
      // eslint-disable-next-line react-hooks/exhaustive-deps -- deps reviewed manually: setJustInstalledTool is a parent one-shot directive callback; adding it would retrigger clearing on parent rerenders
      }, [justInstalledTool, activeSessionId, draftEpoch]);

      // Round-29 m8 + round-30 m8 (review #455): clear the per-send
      // scene-capability banner/toast on session switch AND on "New chat"
      // while already in draft (draftEpoch increments on the click while
      // activeSessionId stays null→null — the exact case the welcome-card
      // reset above documents), so a settled banner never bleeds into the
      // next conversation or the fresh draft. Async writers are additionally
      // suppressed by their send-start session guard inside sendChatMessage
      // (a slow welcome enable settling after the switch would otherwise
      // re-render here).
      useEffect(() => {
        // eslint-disable-next-line react-hooks/set-state-in-effect -- synchronously clear the per-send banner/toast when the session or draft epoch changes (same reset family as the design-element and back-to-bottom resets)
        setSceneCapabilityStatus(null);
      }, [activeSessionId, draftEpoch]);


      // chip 显示当前会话绑定的模型:切会话/草稿时刷新 currentSessionModelId
      useEffect(() => {
        if (bridge.available) bridge.models.loadSessionModel(activeSessionId);
      }, [activeSessionId]);

      // Workspace binding indicator for the active session: bound sessions match
      // the code mode safety posture and show a read-only chip beside the
      // composer. Queried through the bridge's sessions domain
      // (method-existence guard; the Web
      // stub returns null), cached per session; query failure/unbound → hidden.
      const [sessionWorkspaceBinding, setSessionWorkspaceBinding] = useState(null);
      const workspaceBindingCacheRef = useRef({});
      // Generation counter for the binding queries. Bumped on every cache wipe
      // (rebind invalidation) so an in-flight query that was issued before the
      // wipe cannot write its pre-rebind value back into the cache (review
      // #464 round-5 item 6, extended to resolveBindingForGate below).
      const workspaceBindingEpochRef = useRef(0);
      // Which session the live `sessionWorkspaceBinding` state belongs to. The
      // chip keeps its value across a same-session revalidation instead of
      // flipping through null (review #464 round-6 finding 8c).
      const bindingSidRef = useRef(null);
      // A folder rebind moves session bindings behind the cache's back; the
      // sessions-list refresh that follows (session:list_changed) is the
      // signal. Drop cached bindings then, so the chip and the YOLO gate
      // re-resolve instead of showing the pre-rebind directory (#464 r3 m9).
      const bindingCacheSessionsRef = useRef(null);
      const sessionsForBindingCache = bs && bs.sessions;
      useEffect(() => {
        if (bindingCacheSessionsRef.current !== sessionsForBindingCache) {
          bindingCacheSessionsRef.current = sessionsForBindingCache;
          workspaceBindingCacheRef.current = {};
          workspaceBindingEpochRef.current += 1;
        }
        if (!activeSessionId || !bridge.available || !bridge.sessions
          || typeof bridge.sessions.getSessionWorkspaceBinding !== 'function') {
          bindingSidRef.current = null;
          // eslint-disable-next-line react-hooks/set-state-in-effect -- synchronously clear the binding chip when leaving a bound session
          setSessionWorkspaceBinding(null);
          return;
        }
        const sid = activeSessionId;
        if (Object.prototype.hasOwnProperty.call(workspaceBindingCacheRef.current, sid)) {
          bindingSidRef.current = sid;
          setSessionWorkspaceBinding(workspaceBindingCacheRef.current[sid]);
          return;
        }
        // Clear synchronously only when the session actually changed: keeping
        // the old value across a session switch would briefly show the previous
        // session's directory and make the YOLO gate misjudge with a stale
        // binding. A same-session revalidation (every sessions-slice change,
        // including every chat:done) keeps the previous value until the fresh
        // query lands instead of flickering through null (review #464 round-6
        // finding 8c).
        if (bindingSidRef.current !== sid) {
          bindingSidRef.current = sid;
          setSessionWorkspaceBinding(null);
        }
        let cancelled = false;
        const epoch = workspaceBindingEpochRef.current;
        // The generation is captured separately from `cancelled`: the cleanup
        // runs per effect pass (a session switch), while a cache wipe can land
        // between a query's start and its resolution within the same pass.
        const cacheStillValid = () => workspaceBindingEpochRef.current === epoch;
        bridge.sessions.getSessionWorkspaceBinding(sid)
          .then(binding => {
            const normalized = binding || null;
            // Guard the cache write too, not just the setState: a query issued
            // just before a rebind may resolve after the invalidation wipe and
            // would otherwise re-cache the pre-rebind value (review #464
            // round-5 item 6).
            if (!cancelled && cacheStillValid()) {
              workspaceBindingCacheRef.current[sid] = normalized;
              setSessionWorkspaceBinding(normalized);
            }
          })
          // Treat query failure (e.g. old backend without the command) as unbound; never misreport.
          .catch(() => { if (!cancelled) setSessionWorkspaceBinding(null); });
        return () => { cancelled = true; };
      }, [activeSessionId, sessionsForBindingCache]);

      // One-time YOLO confirmation gate for the first switch of a session/draft
      // with a bound workspace (matching code mode): confirming writes the global
      // flag and continues the switch (for a draft, exitPlanToYolo stages the mode
      // choice); cancelling stays in Plan. Unbound targets keep the original path
      // with no card.
      const [pendingChatYoloSwitch, setPendingChatYoloSwitch] = useState(false);
      const [chatYoloConfirmBusy, setChatYoloConfirmBusy] = useState(false);
      const [chatYoloConfirmError, setChatYoloConfirmError] = useState('');
      // Target session captured when the confirm card opens: if the user switches
      // away during the confirm round-trip, exitPlanToYolo must not act on the new
      // active session.
      const pendingChatYoloSwitchSidRef = useRef(null);
      // Action to continue after the confirmation: the mode-chip switch
      // (default) or a plan-stuck card's Go. Both must pass the same
      // one-time YOLO gate — the Go path used to call the bridge's no-arg
      // exitPlanToYolo and flipped a bound session without any card
      // (#445 R9 MAJOR 1).
      const pendingChatYoloActionRef = useRef(null);
      // Binding resolution at switch time cannot rely on async state: while a query
      // is in flight a click would see null and skip the gate. Resolve synchronously
      // before the verdict (cache → bridge query) so clicks made while waiting still
      // get the authoritative binding. The sid argument is captured at click time;
      // after every await it must be compared against activeSessionIdRef.current
      // (latest rendered value) — the closure's activeSessionId is a render-time
      // constant whose self-comparison is always true, leaving a fail-open bypass
      // from unbound A to bound B.
      async function resolveBindingForGate(sid) {
        if (!sid) return null;
        // Web has no binding backend (the bridge stubs the query with null =
        // unbound), so a web session that IS bound would silently skip the
        // one-time YOLO gate. Fail closed for the GATE decision with the
        // unknown-binding sentinel (treated as bound); the chip state stays
        // untouched — web has no directory to display (round-8 should-fix 11).
        if (isWeb) return CHAT_YOLO_GATE_UNKNOWN_BINDING;
        if (sid === activeSessionIdRef.current && sessionWorkspaceBinding !== null) return sessionWorkspaceBinding;
        if (Object.prototype.hasOwnProperty.call(workspaceBindingCacheRef.current, sid)) {
          return workspaceBindingCacheRef.current[sid];
        }
        if (bridge.available && bridge.sessions && typeof bridge.sessions.getSessionWorkspaceBinding === 'function') {
          const epoch = workspaceBindingEpochRef.current;
          try {
            const binding = await bridge.sessions.getSessionWorkspaceBinding(sid);
            const normalized = binding || null;
            // The resolved value is what the gate adjudicates on, so it is
            // returned as-is; the gate itself fails closed, so a query issued
            // before a rebind can only over-protect. Both derived writes —
            // cache AND chip state — are epoch-guarded: sid-keying alone is
            // not enough, because a query issued just before a rebind can
            // resolve after the invalidation wipe and would then re-poison the
            // cache (every later reader) and overwrite the fresh chip (until
            // the next sessions-slice change) with the pre-rebind directory
            // (review #464 round-6 finding 8a; round-7 should-fix extends the
            // guard to the state write, correcting this comment's earlier
            // "always safe" claim about it).
            if (workspaceBindingEpochRef.current === epoch) {
              workspaceBindingCacheRef.current[sid] = normalized;
              if (sid === activeSessionIdRef.current) {
                bindingSidRef.current = sid;
                setSessionWorkspaceBinding(normalized);
              }
            }
            return normalized;
          } catch {
            // Transient query failure (the old backend's unknown-command error is already
            // mapped to null = unbound at the bridge layer, so it never reaches here):
            // default bound sessions to Plan and fail closed by asking once too often
            // rather than silently skipping a bound session. Return the non-null
            // sentinel meaning "treat unknown as bound"; skip the cache so the next
            // click retries.
            return CHAT_YOLO_GATE_UNKNOWN_BINDING;
          }
        }
        return null;
      }
      // Adjudicate the one-time YOLO gate for a click on `gateSid`.
      // Returns true when the caller may proceed with its final action now
      // (unbound, already confirmed, or bridge without prefs support);
      // false when the confirmation card was opened (or the click went
      // stale mid-await and the action must be abandoned).
      async function chatYoloGateAllows(gateSid) {
        const sessionBinding = await resolveBindingForGate(gateSid);
        // If the user switched away during the query round-trip, the
        // adjudication belongs to the session captured at click time — the
        // action must be abandoned.
        if (gateSid !== activeSessionIdRef.current) return false;
        const boundTarget = chatYoloGateApplies({
          activeSessionId: gateSid,
          sessionBinding,
          draftWorkspacePath: bs && bs.draftWorkspacePath,
        });
        if (boundTarget && typeof bridge.interaction.getCodePermissionPrefs === 'function') {
          const prefs = await bridge.interaction.getCodePermissionPrefs();
          // Re-check after the second await as well.
          if (gateSid !== activeSessionIdRef.current) return false;
          if (needsYoloConfirmation(prefs)) {
            pendingChatYoloSwitchSidRef.current = gateSid;
            if (!pendingChatYoloActionRef.current) pendingChatYoloActionRef.current = { kind: 'modeChip' };
            setPendingChatYoloSwitch(true);
            return false;
          }
        }
        return true;
      }
      async function handleModeChipSwitch(target, { isPlan }) {
        if (!bridge.available || !bridge.interaction) return;
        if (target === 'plan' && !isPlan) {
          await bridge.interaction.setPlanModeNext();
          return;
        }
        if (target !== 'yolo' || !isPlan) return;
        const gateSid = activeSessionId;
        // Post-await re-checks live inside chatYoloGateAllows: the
        // adjudication is for the session captured at click time, and the
        // final action must not follow a switch-away.
        if (gateSid !== activeSessionIdRef.current) return;
        if (!(await chatYoloGateAllows(gateSid))) return;
        // The final action targets the adjudicated sid explicitly: the ref
        // re-checks only cover this component's awaits, while the bridge's
        // no-arg exitPlanToYolo reads live-active at call time — a
        // render-phase mirror can lag the bridge store by one flush, so the
        // adjudicated session must reach the command as a parameter.
        await bridge.interaction.exitPlanToYolo(gateSid);
      }
      // The plan-stuck card's Go reaches Yolo too, so it passes the same
      // one-time gate and threads the card's session id into the bridge
      // (the bridge's no-arg form reads live-active, which is both a
      // gate bypass for a bound session and the stale-active defect).
      async function handlePlanStuckGo(itemId) {
        if (!bridge.available || !bridge.interaction) return;
        const gateSid = activeSessionId;
        if (!gateSid) { await bridge.interaction.planStuckGo(itemId); return; }
        if (gateSid !== activeSessionIdRef.current) return;
        pendingChatYoloActionRef.current = { kind: 'planStuckGo', itemId };
        if (!(await chatYoloGateAllows(gateSid))) return;
        await bridge.interaction.planStuckGo(itemId, gateSid);
      }
      async function confirmChatYoloSwitch() {
        if (chatYoloConfirmBusy) return;
        setChatYoloConfirmBusy(true);
        setChatYoloConfirmError('');
        try {
          await bridge.interaction.confirmCodeYolo();
          if (pendingChatYoloSwitchSidRef.current !== activeSessionIdRef.current) {
            // Switched away: just dismiss the card (the global flag is already written;
            // the target session will not be prompted again after switching back).
            setPendingChatYoloSwitch(false);
            return;
          }
          setPendingChatYoloSwitch(false);
          // Dispatch on the action captured when the card opened, targeting
          // the captured sid — never live-active at call time.
          const action = pendingChatYoloActionRef.current;
          pendingChatYoloActionRef.current = null;
          if (action && action.kind === 'planStuckGo') {
            await bridge.interaction.planStuckGo(action.itemId, pendingChatYoloSwitchSidRef.current);
          } else {
            await bridge.interaction.exitPlanToYolo(pendingChatYoloSwitchSidRef.current);
          }
        } catch (e) {
          // No longer fail silently: keep the card open and show the reason inline
          // (matching the code lane; on old backends without confirmCodeYolo the user
          // must not be left without feedback).
          console.warn('confirm chat yolo switch failed', e);
          setChatYoloConfirmError(String(e && e.message || e || 'error'));
        } finally {
          setChatYoloConfirmBusy(false);
        }
      }
      // Switching sessions invalidates the old session's confirm card and error
      // message.
      useEffect(() => {
        pendingChatYoloSwitchSidRef.current = null;
        pendingChatYoloActionRef.current = null;
        // eslint-disable-next-line react-hooks/set-state-in-effect -- synchronously close the previous session's confirm card on session switch
        setPendingChatYoloSwitch(false);
        setChatYoloConfirmError('');
      }, [activeSessionId]);

      // 普通会话选图即时警告(阶段 G):当前模型图片路由为 unsupported 时在附件区提示,
      // 仅提示不拦截,发送时后端仍按同一路径复核(chat 命令 image_input_unsupported)。
      // scheduled 会话发送时不做图片路由(固定工具兜底),这里同样不提示。
      const hasImageAttachment = attachments.some(a => !!(a && a.result && a.result.kind === 'image'));
      const isScheduledSession = !!(scheduledRunContext || isScheduledTaskCreationChat);
      const sessionModelKey = (bs && bs.currentSessionModelId) || (bs && bs.activeModelId) || '';
      const [imageInputInfo, setImageInputInfo] = useState(null);
      useEffect(() => {
        if (!hasImageAttachment || isScheduledSession || !bridge.available
          || !bridge.models || typeof bridge.models.getImageInputCapability !== 'function') {
          setImageInputInfo(null);
          return undefined;
        }
        let cancelled = false;
        bridge.models.getImageInputCapability(activeSessionId)
          .then(info => { if (!cancelled) setImageInputInfo(info || null); })
          // 查询失败(如凭据未备/旧后端无此命令)按无警告处理,绝不误报。
          .catch(() => { if (!cancelled) setImageInputInfo(null); });
        return () => { cancelled = true; };
      }, [hasImageAttachment, isScheduledSession, activeSessionId, sessionModelKey, bs && bs.savedModels]);
      const imageInputWarning = imageInputInfo && imageInputInfo.image_mode === 'unsupported'
        ? (imageInputInfo.capability === 'unknown' ? t.uiAttachments.imageUnknown : t.uiAttachments.imageUnsupported)
        : '';
      // 云上传隐私提示(§11.8/§11.9):图片字节离开本机时告知去向——native 直发看主模型
      // 端点,fallback 看兜底视觉模型端点(图片实际发给视觉模型);本机 loopback 不显示
      // 任何云上传字样;查询失败或旧后端无对应字段时 fail-open 不显示。
      const imagePrivacyHint = imageInputInfo && (
        (imageInputInfo.image_mode === 'native' && imageInputInfo.is_local_endpoint === false)
        || (imageInputInfo.image_mode === 'vision_tool_fallback' && imageInputInfo.vision_is_local_endpoint === false)
      )
        ? (imageInputInfo.image_mode === 'vision_tool_fallback'
          ? t.uiAttachments.imageCloudUploadVision
          : t.uiAttachments.imageCloudUpload)
        : '';

      async function handleSend() {
        // 不再因 busy 拦截:bridge.chat.sendMessage 在生成中会把这句排队(本轮跑完自动发)。
        // 远程能力准备不是生成中的「可排队」状态：快照到达前 Rust 无法构造
        // task intent，必须保留草稿并等待 remoteLinkReady。
        if (
          isMultiAgentReadOnly
          || !canSend
          || (remoteChatActive && !remoteNewDispatchReady)
          || (controlledChatActive && !remoteChatActive && !controlledLinkReady)
        ) return;
        if (chatVoice && chatVoice.editPreview) {
          await chatVoice.applyVoiceEditPreview({ send: true });
          return;
        }
        const constrained = constrainChatInput(inputText);
        if (constrained.truncated) {
          setInputText(constrained.text);
          return;
        }
        const text = constrained.text;
        const sentSessionId = activeSessionId;
        const sentRemoteIdentity = remoteComposerIdentity;
        const mayRestore = () => !remoteTranscriptActive || sameRemoteComposerOwner(
          sentSessionId, sentRemoteIdentity, activeSessionIdRef.current, remoteComposerIdentityRef.current,
        );
        // Clear the composer the moment the button is clicked (before the
        // await returns); on failure (reserve conflict etc.) or a notice-only
        // not-dispatched resolution (attachments still parsing, remote-turn
        // sync block — sendMessage reports false) restore the text — a message
        // is never silently lost. While busy the bridge steers into
        // the current turn (with attachments it queues locally), see
        // sendMessage. The restore replaces wholesale only when the composer is
        // empty: sendChatMessage awaits capability installs etc. and the user
        // may have started the next message meanwhile — an unconditional
        // overwrite would clobber it, so a non-empty composer degrades to an
        // append-style prefill (newline separator, re-review #4) that does not
        // interrupt typing.
        setInputText('');
        try {
          const accepted = await sendChatMessage(text);
          if (!accepted && mayRestore()) {
            if (inputTextRef.current === '') setInputText(text);
            else if (text) bridge.chat.prefillComposer(text, true);
          } else if (!accepted && remoteTranscriptActive && sentSessionId) {
            bridge.chat.restoreTaskDraft(text, { sessionId: sentSessionId, restored: false });
          }
        } catch (error) {
          if (mayRestore()) {
            if (inputTextRef.current === '') setInputText(text);
            else if (text) bridge.chat.prefillComposer(text, true);
          } else if (remoteTranscriptActive && sentSessionId) {
            bridge.chat.restoreTaskDraft(text, { sessionId: sentSessionId, restored: false });
          }
          // Swallow here: the bridge already surfaced the failure (notice +
          // restore), and the primary send paths (Enter / send button) have
          // no catch of their own — a rethrow would only produce an
          // unhandledrejection next to the visible recovery.
          console.warn("[pinvou3][chat-ui] send failed", error);
        }
        personalWorkbenchTemplateIdRef.current = null;
        setPersonalWorkbenchTemplateId(null);
      }

      // Zap-send a queued chip: interrupt the current generation and send
      // that queued message immediately (cancel + chat path, abandoning the
      // current AI progress). On failure the bridge restores the message to
      // the queue with a notice — nothing to do here. No local single-flight
      // needed: the bridge removes the chip first, so a double click simply
      // finds no target.
      async function handleInterruptQueued(queuedId) {
        if (isMultiAgentReadOnly) return;
        if (!bridge.chat || typeof bridge.chat.interruptAndSendQueued !== "function") {
          // Never silent (this guard's silent return is exactly the
          // historical "composer cleared but nothing happened" bug shape):
          // dead on current targets (desktop exports it; web hides the
          // button), but a missing export should be visible in diagnostics.
          console.warn("[pinvou3][chat-ui] interruptAndSendQueued unavailable on this host");
          return;
        }
        await bridge.chat.interruptAndSendQueued(activeSessionId, queuedId);
      }

      /**
       * @param {number|string} queuedId Queue item identifier.
       * @param {() => boolean|Promise<boolean>} action Session-scoped bridge operation.
       */
      async function runQueuedAction(queuedId, action) {
        if (isMultiAgentReadOnly || queuedActionBusy) return false;
        const actionSessionId = activeSessionId;
        if (!actionSessionId) return false;
        setQueuedActionErrors(current => {
          const next = { ...current };
          delete next[actionSessionId];
          return next;
        });
        setQueuedActions(current => ({ ...current, [actionSessionId]: { id: queuedId } }));
        /** @param {string} text Failure message to display. */
        const showFailure = (text) => {
          flashQueuedNotice(actionSessionId, { queuedId, text });
        };
        try {
          const completed = await action();
          if (!completed) showFailure(t.queuedActionFailed);
          return !!completed;
        } catch (error) {
          console.warn('[pinvou3][chat-ui] queued message action failed', error);
          showFailure(t.queuedActionFailed);
          return false;
        } finally {
          setQueuedActions(current => {
            if (!current[actionSessionId] || current[actionSessionId].id !== queuedId) return current;
            const next = { ...current };
            delete next[actionSessionId];
            return next;
          });
        }
      }

      /** @param {number|string} queuedId Queue item identifier. */
      function handlePrioritizeQueued(queuedId) {
        if (!bridge.chat || typeof bridge.chat.prioritizeQueued !== 'function') return;
        void runQueuedAction(queuedId, () => bridge.chat.prioritizeQueued(activeSessionId, queuedId));
      }

      /** @param {{id: number|string, attachments?: unknown[]}} item Queue item being edited. */
      async function handleSaveQueuedEdit(item) {
        if (!queuedEdit || queuedEdit.id !== item.id) return;
        const editSessionId = activeSessionId;
        if (!editSessionId) return;
        const nextText = String(queuedEdit.text || '').trim();
        if (!nextText && !(item.attachments || []).length) {
          flashQueuedNotice(editSessionId, { queuedId: item.id, text: t.queuedEmpty });
          return;
        }
        if (!bridge.chat || typeof bridge.chat.editQueued !== 'function') return;
        const completed = await runQueuedAction(item.id, () => (
          bridge.chat.editQueued(editSessionId, item.id, nextText)
        ));
        if (completed) {
          setQueuedEdits(current => {
            if (!current[editSessionId] || current[editSessionId].id !== item.id) return current;
            const next = { ...current };
            delete next[editSessionId];
            return next;
          });
        }
      }

      function handleKeyDown(e) {
        if (chatVoice && chatVoice.editPreview) {
          if (e.key === 'Escape') {
            e.preventDefault();
            chatVoice.cancelVoiceEditPreview();
            return;
          }
          if (isPlainEnter(e)) {
            e.preventDefault();
            chatVoice.applyVoiceEditPreview({ send: e.ctrlKey || e.metaKey });
            return;
          }
        }
        // 输入法合成期间(例如中文输入法敲回车确认候选词上屏)不要触发发送,
        // 否则一次回车会既上屏又发送消息。与 PetWindow 处理保持一致。
        if (isPlainEnter(e)) {
          e.preventDefault();
          handleSend();
        }
      }

      async function handleCancel() {
        // single-flight:同一 session 已在取消中则忽略后续点击，避免并发
        // cancel_generation。按 session 集合记录（而非全局布尔或单个 sid）——
        // 多个会话可以同时处于取消中（取消 A 期间切到 B 发起取消，再切回 A，
        // A 的标记不能被 B 覆盖）。各自 Promise 完成时只删除对应 sid。
        if (!bridge.available || cancellingSessionIds.has(activeSessionId)) return;
        const cancellingSid = activeSessionId;
        setCancellingSessionIds(prev => new Set(prev).add(cancellingSid));
        try {
          await bridge.chat.cancelGeneration();
        } finally {
          // 只清当前 session 自己的取消标记；若期间已切到别的会话并开始了
          // 新的取消（cancellingSessionIds 里已有其他 sid），不要误清对方。
          setCancellingSessionIds(prev => {
            if (!prev.has(cancellingSid)) return prev;
            const next = new Set(prev);
            next.delete(cancellingSid);
            return next;
          });
        }
      }

      const chatVoice = useComposerVoiceInput({
        targetId: 'chat-composer',
        ownerKind: 'chat',
        bridge,
        voiceInput,
        voiceBusy,
        workspaceId: activeSessionId || 'draft',
        sessionId: activeSessionId || null,
        getDraft: () => inputText,
        setDraft: setInputText,
        appendDraft: bridge.voice.appendVoiceText,
        isStillActive: () => composerMountedRef.current,
        canStart: () => !isMultiAgentReadOnly,
        canSendTask: () => !isMultiAgentReadOnly,
        resolveMode: (mode, context) => {
          if (mode === 'dictation' && context && context.source !== 'button'
            && String(context.draft || '').trim()) {
            return 'edit';
          }
          return mode;
        },
        beforePermission: context => {
          pendingVoiceAfterIntroRef.current = null;
          return requestVoiceShortcutIntroAfterAsr(context && context.mode);
        },
        sendTask: async (outgoing, context) => {
          // Direct voice task send passes the same length gate: on overflow, truncate and write
          // back into the input box without sending (same policy as handleSend).
          const constrained = constrainChatInput(outgoing);
          if (constrained.truncated) {
            setInputText(constrained.text);
            return false;
          }
          if (context?.diagnostic?.task_send_blocked) return false;
          // Consume only the exact draft this task owns. The bridge's #406
          // restored path may then append it once without duplicating writeback.
          if (inputTextRef.current !== constrained.text) return false;
          const owner = {
            sessionId: (activeSessionIdRef && activeSessionIdRef.current) || null,
            draftEpoch: Number(draftEpoch || 0),
            operationId: context?.operationId,
            restored: false,
          };
          setInputText('');
          try {
            const result = await sendChatMessage(constrained.text, { ...context, draftOwner: owner });
            if (result === true) {
              // The composer was cleared before the await, so deliverVoiceTask's
              // draftUntouched check can never fire onTaskAccepted; its one
              // non-redundant duty — dropping the pinned personal-workbench
              // template so the next send re-matches — moves here. Any text the
              // user typed during the await is newer input and stays.
              personalWorkbenchTemplateIdRef.current = null;
              setPersonalWorkbenchTemplateId(null);
            }
            if (result === false && bridge.chat.restoreTaskDraft) {
              bridge.chat.restoreTaskDraft(constrained.text, owner);
            }
            // Voice task delivery treats "restored" as not accepted (the text
            // is back, not sent); ordinary callers map it through to false.
            return result === 'restored' || result === 'held' ? false : result;
          } catch (error) {
            if (bridge.chat.restoreTaskDraft) bridge.chat.restoreTaskDraft(constrained.text, owner);
            console.warn('[voice-input] task send failed after writeback', error);
            return false;
          }
        },
        onTaskAccepted: (sentText) => {
          // The user may have typed new content during the await send window; clear only when
          // the draft was not modified.
          setInputText(prev => (prev === sentText ? '' : prev));
          personalWorkbenchTemplateIdRef.current = null;
          setPersonalWorkbenchTemplateId(null);
        },
      });
      const handleVoiceTrigger = chatVoice.triggerVoice;

      function rememberVoiceIntroSeen() {
        setVoiceIntroSeenState(true);
        setVoiceShortcutIntroSeen(true);
      }

      function shouldShowVoiceShortcutIntro(mode) {
        return normalizeVoiceMode(mode) === 'dictation'
          && !voiceIntroSeenState
          && !voiceShortcutEnabledRef.current;
      }

      function requestVoiceShortcutIntroAfterAsr(mode) {
        if (!shouldShowVoiceShortcutIntro(mode)) return Promise.resolve(true);
        if (voiceIntroResolveRef.current) return Promise.resolve(false);
        setVoiceIntroOpen(true);
        return new Promise((resolve) => {
          voiceIntroResolveRef.current = resolve;
        });
      }

      function resolveVoiceShortcutIntro(value) {
        const resolve = voiceIntroResolveRef.current;
        voiceIntroResolveRef.current = null;
        if (resolve) resolve(value);
      }

      function handleVoiceIntroClose() {
        rememberVoiceIntroSeen();
        setVoiceIntroOpen(false);
        resolveVoiceShortcutIntro(true);
      }

      function handleVoiceIntroToggleShortcut(enabled) {
        rememberVoiceIntroSeen();
        setVoiceShortcutEnabled(enabled);
        setVoiceShortcutEnabledState(enabled);
        setVoiceIntroOpen(false);
        resolveVoiceShortcutIntro(true);
      }

      useEffect(() => {
        const wasActive = voiceAsrInstallWasActiveRef.current;
        const ready = !!(voiceAsrSetup.status && voiceAsrSetup.status.ready);
        const done = voiceAsrProgress.stage === 'done';
        if (wasActive && !voiceAsrBusy && ready && done) {
          setVoiceAsrReadyNotice(true);
          if (voiceAsrReadyNoticeTimerRef.current) window.clearTimeout(voiceAsrReadyNoticeTimerRef.current);
          voiceAsrReadyNoticeTimerRef.current = window.setTimeout(() => {
            setVoiceAsrReadyNotice(false);
            voiceAsrReadyNoticeTimerRef.current = null;
          }, 3200);
          const pendingVoice = pendingVoiceAfterIntroRef.current;
          if (pendingVoice) {
            pendingVoiceAfterIntroRef.current = null;
            requestVoiceShortcutIntroAfterAsr(pendingVoice.mode).then((shouldContinue) => {
              if (!shouldContinue) return;
              handleVoiceTrigger(pendingVoice.mode, { source: pendingVoice.source || 'button' });
            });
          }
        }
        voiceAsrInstallWasActiveRef.current = voiceAsrBusy;
      // eslint-disable-next-line react-hooks/exhaustive-deps -- one-shot transition on install completion; handleVoiceTrigger/requestVoiceShortcutIntroAfterAsr are stable callbacks read via closure at fire time
      }, [voiceAsrBusy, voiceAsrProgress.stage, voiceAsrSetup.status]);

      function handleVoiceClick() {
        const shouldIntro = shouldShowVoiceShortcutIntro('dictation');
        if (shouldIntro) {
          pendingVoiceAfterIntroRef.current = { mode: 'dictation', source: 'button' };
        } else {
          pendingVoiceAfterIntroRef.current = null;
        }
        // triggerVoice returns false when it did not start a fresh session
        // (busy, read-only, cancelled permission request, recording stop…);
        // in those cases the stashed intent can never be consumed by the
        // intro flow and would instead fire on a much later, unrelated ASR
        // install completion. Drop it right away.
        if (!handleVoiceTrigger('dictation', { source: 'button' }) && shouldIntro) {
          pendingVoiceAfterIntroRef.current = null;
        }
      }

      function handleVoiceCancel() {
        chatVoice.cancelVoice();
      }

      function handleVoiceClose() {
        // Hiding the notice dismisses without ending the unsent operation
        // (an in-flight recording still cancels); closeVoice would abandon it.
        chatVoice.dismissVoice();
      }

      async function handlePaste(e) {
        if (isWeb || remoteTranscriptActive) return;
        // WebKit-compatible image filtering + FileReader byte reads (incl. jpeg→jpg normalization) live in the shared module.
        const images = collectClipboardImages(e);
        if (images.length) {
          e.preventDefault();
          for (const file of images) {
            try {
              const { bytes, ext } = await readPasteImageAsBytes(file);
              if (bridge.available) {
                bridge.attachments.addPasteImage(
                  `paste-${Date.now()}.${ext}`,
                  bytes,
                  formatAttachmentError,
                );
              }
            } catch { /* A single failed read is silently dropped, as before; the rest of the pasted images proceed */ }
          }
          return;
        }
        // Linux WebKitGTK never exposes image clipboard data through the paste event
        // (an image clipboard surfaces as an empty clipboardData, swallowing text too).
        // Swallow that shape and let the native layer read the clipboard instead;
        // text pastes keep their clipboardData and the default path stays untouched.
        if (!pasteImageClipboardFallbackAvailable(bs && bs.platformCapabilities)) return;
        if (!pasteEventNeedsClipboardFallback(e)) return;
        e.preventDefault();
        if (bridge.available) await bridge.attachments.addPasteImageFromClipboard(formatAttachmentError);
      }

      const responsiveGutterStyle = {
        paddingInline: 'clamp(16px, calc((100% - 800px) / 2), 160px)',
      };

      // The two ArtifactsPanel mounts (fullscreen portal / right Dock) share the same 18-prop
      // list through this single source; isFullscreen and onToggleFullscreen are passed per
      // mount point: collapsed (false) in fullscreen, expanded (true) in the Dock.
      const artifactsPanelProps = {
        bs,
        t,
        onClose: closeArtifactsPanel,
        isWide: true,
        preferredArtifactPath: activeArtifactPath,
        onPreviewArtifact: handlePreviewArtifact,
        onGotoSettings,
        designCommand,
        selectedDesignElement,
        designChanges: visibleDesignChanges,
        onDesignElementSelected: handleDesignElementSelected,
        onDesignChangeApplied: handleDesignChangeApplied,
        onDesignMutation: handleDesignMutation,
        onDesignApplyChange: handleApplyDesignChange,
        onDesignClearChanges: handleClearDesignChanges,
        onDesignAiSubmit: handleDesignAiSubmit,
        designAiState,
        onDesignAiStateChange: updateDesignAiState,
      };

      return (
        <div className="flex-1 flex flex-row w-full h-full min-h-0 relative z-10 animate-in fade-in duration-300">
          <div ref={chatRootRef} className="flex-1 flex flex-col min-w-0 relative h-full">
            <ComposerAttachmentDropOverlay
              enabled={!remoteChatActive && bridge.available && (!isWeb || can('deviceFileUpload'))}
              onFiles={files => bridge.attachments.uploadDeviceFiles(files)}
              dark={theme === 'dark'}
              variant={isWeb ? 'web' : 'desktop'}
              copy={t.uiAttachments}
            />

          {/* Top Header (浮动) */}
          <div className="absolute top-0 left-0 right-0 p-4 flex justify-between items-center z-20 pointer-events-none">
            <div className="flex items-center gap-2 min-w-0">
              {controlledChatActive && (
                <div
                  data-testid="chat-controlled-status"
                  role="status"
                  title={controlledDevice.controllerDeviceName
                    ? `${t.controlledDevice.controlledStatus}: ${controlledDevice.controllerDeviceName}`
                    : t.controlledDevice.controlledStatus}
                  className={`pointer-events-auto flex h-10 min-w-0 items-center gap-1.5 rounded-full border px-3 text-[13px] font-medium ${'border-emerald-500/20 bg-emerald-500/10 text-emerald-700 dark:border-emerald-400/20 dark:bg-emerald-400/10 dark:text-emerald-300'}`}
                >
                  <Monitor size={15} className="shrink-0" />
                  <span className="shrink-0">{t.controlledDevice.controlledStatus}</span>
                  {controlledDevice.controllerDeviceName && (
                    <span className="max-w-[180px] truncate text-[11px] opacity-80">
                      {controlledDevice.controllerDeviceName}
                    </span>
                  )}
                  {controlledDevice.status === 'reconnecting' && (
                    <span className="max-w-[180px] truncate text-[11px] opacity-80">
                      {t.controlledDevice.statusReconnecting}
                    </span>
                  )}
                </div>
              )}
              {remoteChatActive && (
                <div
                  data-testid="chat-remote-status"
                  title={scopedRemoteConnection?.deviceName
                    ? `${remoteStatusLabel}: ${scopedRemoteConnection.deviceName}` : remoteStatusLabel}
                  className={`pointer-events-auto flex h-10 items-center gap-1.5 rounded-full border px-3 text-[13px] font-medium ${'border-blue-500/20 bg-blue-500/10 text-[#1A73E8] dark:border-blue-400/20 dark:bg-blue-400/10 dark:text-[#8AB4F8]'}`}
                >
                  <Monitor size={15} className="shrink-0" />
                  <span className="max-w-[220px] truncate">{remoteStatusLabel}</span>
                  <span
                    className={`text-[11px] ${scopedRemoteConnection?.sessionRef ? 'opacity-80' : 'animate-pulse motion-reduce:animate-none opacity-90'}`}
                  >
                    {scopedRemoteConnection?.sessionRef
                      ? (effectiveRemoteModelSource === 'controller_temporary'
                        ? t.remoteConversation.modelSourceMine
                        : t.controllerConnection.modelSourceDevice)
                      : t.controllerConnection.remoteSessionPreparing}
                  </span>
                </div>
              )}
              {scheduledRunContext && (
                <button type="button" onClick={onBackScheduledRun}
                  data-testid="scheduled-run-back"
                  aria-label={chatCopy.backRuns}
                  title={chatCopy.backRuns}
                  className={`pointer-events-auto h-10 max-w-[520px] max-sm:max-w-[55vw] px-3 rounded-full flex items-center gap-2 border text-[14px] font-medium transition-colors ${'bg-white border-[#E3E5E8] text-[#1F1F1F] hover:bg-[#F5F5F6] shadow-sm dark:bg-[#1E1F20] dark:border-[#333537] dark:text-[#E3E3E3] dark:hover:bg-[#2B2C2F]'}`}>
                  <ArrowLeft size={16} className="shrink-0" />
                  <span className="truncate">{scheduledRunContext.taskName || chatCopy.scheduledRun}</span>
                  <span className={`shrink-0 text-[12px] max-sm:hidden ${'text-[#85888D] dark:text-[#9AA0A6]'}`}>{chatCopy.runRecords}</span>
                </button>
              )}
            </div>
            <div className="flex items-center gap-2">
              {activeSessionId && !activeSessionId.startsWith('sched-') && bridge.available && bridge.auxChat && (
                <button
                  type="button"
                  data-testid="aux-chat-open"
                  aria-label={t.uiAuxChat.openLabel}
                  title={t.uiAuxChat.openLabel}
                  onMouseEnter={() => prefetchChatPanel('auxChat')}
                  onFocus={() => prefetchChatPanel('auxChat')}
                  onClick={openAuxChatPanel}
                  className={`pointer-events-auto flex h-10 shrink-0 items-center gap-2 rounded-full border px-3 text-[14px] font-medium shadow-sm transition-colors ${
                    auxChatPanel && auxChatDockActive
                      ? theme === 'dark'
                        ? 'border-blue-400/30 bg-blue-500/15 text-blue-300'
                        : 'border-blue-500/20 bg-blue-500/10 text-blue-600'
                      : theme === 'dark'
                        ? 'border-white/10 bg-[#1E1F20] text-[#E3E3E3] hover:bg-[#333537]'
                        : 'border-black/10 bg-white text-[#1F1F1F] hover:bg-[#F0F4F9]'
                  }`}
                >
                  <MessageSquare size={16} />
                  <span className="max-sm:hidden">{t.uiAuxChat.openLabel}</span>
                </button>
              )}
              {activeSessionId && !remoteTranscriptActive && (
                <RunningAgentsOverlay
                  sessionId={activeSessionId}
                  theme={theme}
                  t={t}
                  swarmOn={swarmModeOn}
                />
              )}
              {activeSessionId && !remoteTranscriptActive && (
                <ChatRightDockSwitcher
                  theme={theme}
                  artifactsLabel={t.artifacts}
                  browserLabel={t.browser}
                  artifactCount={artifactCount}
                  browserAvailable={browserDockAvailable}
                  browserOpen={browserDockOpen}
                  activePanelId={rightDockActivePanelId}
                  onOpenArtifacts={openArtifactsPreview}
                  onOpenBrowser={onOpenBrowserDock}
                />
              )}
            </div>
          </div>


          {/* Main Chat Area */}
          {/* 有消息时底部留白由列表内的实体 spacer 承担，避免 WebKitGTK/Safari
              不把 overflow flex 容器的尾部 padding 完整计入 scrollHeight。
              空态不滚动，仍需 paddingBottom 让欢迎语在悬浮输入框上方居中。 */}
          <div ref={scrollRef} data-testid="chat-scroll" data-coordination-id={remoteUiCoordinationId || undefined}
            style={{
              ...responsiveGutterStyle,
              overflowAnchor: 'none',
              ...(hasMessages ? {} : { paddingBottom: (composerH ? composerH + 48 : 160) + 'px' }),
            }}
            className={`flex-1 min-h-0 min-w-0 overflow-y-auto custom-scrollbar flex flex-col pt-20 max-sm:pt-16 ${hasMessages ? 'justify-start' : 'items-center justify-center'}`}>

            {remoteChatActive && (
              <div
                data-testid="chat-remote-status-banner"
                role="status"
                aria-live="polite"
                className={`mb-4 w-full max-w-[800px] px-4 max-sm:px-3 ${hasMessages ? 'mx-auto' : ''}`}
              >
                <div className={`flex flex-wrap items-center gap-2 rounded-[20px] border px-4 py-3 text-[13px] font-medium shadow-sm ${'border-blue-500/20 bg-blue-500/10 text-[#174EA6] dark:border-blue-400/20 dark:bg-blue-400/10 dark:text-[#A8C7FA]'}`}>
                  <Monitor size={16} className="shrink-0" />
                  <span>{remoteStatusLabel}</span>
                  {scopedRemoteConnection?.deviceName && (
                    <span className="min-w-0 truncate opacity-80">· {scopedRemoteConnection.deviceName}</span>
                  )}
                  <span className={`text-[12px] ${scopedRemoteConnection?.sessionRef ? 'opacity-80' : 'animate-pulse motion-reduce:animate-none opacity-90'}`}>
                    {remotePresentation === 'offline' ? t.remoteConversation.offlineHint
                      : remotePresentation === 'reconnecting' ? t.controllerConnection.stateReconnecting
                      : scopedRemoteConnection?.capabilityStatus === 'error'
                      ? t.controllerConnection.remoteCapabilityFailed
                      : (scopedRemoteConnection?.capabilityRevision != null
                        ? t.controllerConnection.remoteSessionReady
                        : (scopedRemoteConnection?.sessionRef ? t.controllerConnection.remoteCapabilityPreparing : t.controllerConnection.remoteSessionPreparing))}
                  </span>
                  <div className="ml-auto flex min-w-0 items-center gap-2">
                    <label className="flex items-center gap-1 text-[12px]">
                      <span className="opacity-70">{t.remoteConversation.modeLabel}</span>
                      <select
                        data-testid="chat-remote-mode"
                        aria-label={t.remoteConversation.modeLabel}
                        value={remoteMode}
                        onChange={(event) => setRemoteMode(event.target.value)}
                        disabled={remoteCapabilityBusy || !remoteLinkReady || remoteModes.length === 0}
                        className="min-w-0 max-w-[140px] rounded-full bg-white/80 px-2 py-1 text-[12px] outline-none dark:bg-[#1F2023]"
                      >
                        {remoteModes.includes('chat') && <option value="chat">{t.remoteConversation.modeChat}</option>}
                        {remoteModes.includes('code') && <option value="code">{t.remoteConversation.modeCode}</option>}
                      </select>
                    </label>
                    {remoteMode === 'code' && (
                      <select
                        data-testid="chat-remote-workspace"
                        aria-label={t.remoteConversation.workspaceLabel}
                        value={remoteWorkspaceId}
                        onChange={(event) => setRemoteWorkspaceId(event.target.value)}
                        disabled={!remoteLinkReady || remoteWorkspaces.length === 0}
                        className="min-w-0 max-w-[180px] rounded-full bg-white/80 px-2 py-1 text-[12px] outline-none dark:bg-[#1F2023]"
                      >
                        <option value="">{t.remoteConversation.workspaceRequired}</option>
                        {remoteWorkspaces.map(workspace => (
                          <option key={workspace.workspaceId} value={workspace.workspaceId}>
                            {workspace.displayName || workspace.workspaceId}
                          </option>
                        ))}
                      </select>
                    )}
                    <span className="shrink-0 text-[12px] opacity-70">{t.remoteConversation.modelPickerTitle}</span>
                    <select
                      data-testid="chat-remote-model-source"
                      aria-label={t.remoteConversation.modelPickerTitle}
                      value={effectiveRemoteModelSource}
                      onChange={(event) => onRemoteModelSourceChange?.(event.target.value)}
                      disabled={remoteCapabilityBusy || !remoteLinkReady}
                      className="min-w-0 max-w-[180px] rounded-full bg-white/80 px-2 py-1 text-[12px] outline-none dark:bg-[#1F2023]"
                    >
                      <option value="controlled_device">{t.remoteConversation.modelSourceDevice}</option>
                      {remoteMode === 'chat' && remoteTemporaryModelAvailable && (
                        <option value="controller_temporary">{t.remoteConversation.modelSourceMine}</option>
                      )}
                    </select>
                    {effectiveRemoteModelSource === 'controller_temporary' && (
                      <select
                        data-testid="chat-remote-selected-model"
                        aria-label={t.remoteConversation.modelPickerTitle}
                        value={selectedRemoteModelId || ''}
                        onChange={(event) => setRemoteModelSelection({
                          key: remoteModelBindingKey, modelId: event.target.value,
                        })}
                        disabled={!remoteLinkReady || exportableRemoteModels.length === 0}
                        className="min-w-0 max-w-[180px] rounded-full bg-white/80 px-2 py-1 text-[12px] outline-none dark:bg-[#1F2023]"
                      >
                        <option value="">{t.remoteConversation.modelPickerTitle}</option>
                        {exportableRemoteModels.map(model => (
                          <option key={model.modelId} value={model.modelId}>{model.displayName || model.modelId}</option>
                        ))}
                      </select>
                    )}
                  </div>
                </div>
                {remoteCapabilities && (remoteSkills.length > 0 || remoteTools.length > 0) && (
                  <div data-testid="chat-remote-capabilities" className="mt-2 rounded-xl border border-blue-500/15 px-3 py-2 text-[12px]">
                    {remoteSkills.length > 0 && (
                      <fieldset className="flex flex-wrap items-center gap-2">
                        <legend className="mr-1 font-medium">{t.remoteConversation.skillsLabel}</legend>
                        {remoteSkills.map(skill => (
                          <label key={skill.skillId} className="inline-flex items-center gap-1">
                            <input type="checkbox" checked={remoteSkillIds.includes(skill.skillId)}
                              onChange={(event) => setRemoteSkillIds(current => event.target.checked
                                ? [...new Set([...current, skill.skillId])]
                                : current.filter(id => id !== skill.skillId))} />
                            <span>{skill.displayName || skill.skillId}</span>
                          </label>
                        ))}
                      </fieldset>
                    )}
                    {remoteTools.length > 0 && (
                      <div className="mt-1" aria-label={t.remoteConversation.toolsLabel}>
                        <span className="font-medium">{t.remoteConversation.toolsLabel}: </span>
                        <span>{remoteTools.map(tool => tool.displayName || tool.toolId).join(' · ')}</span>
                      </div>
                    )}
                  </div>
                )}
                {remoteCapabilityMessage && (
                  <div role="alert" className="mt-2 flex items-center gap-2 px-1 text-[12px] text-[#EA4335]">
                    <span>{remoteCapabilityMessage}</span>
                    {remoteCapabilityMessage && scopedRemoteConnection?.sessionRef && (
                      <button
                        type="button"
                        onClick={requestRemoteCapabilities}
                        disabled={remoteCapabilityBusy}
                        className="rounded-full border border-[#EA4335]/40 px-2 py-0.5 disabled:opacity-60"
                      >
                        {t.controllerConnection.remoteCapabilityRetry}
                      </button>
                    )}
                  </div>
                )}
              </div>
            )}

            {remoteTranscriptActive && remoteSendMessage && (
              <div
                data-testid="chat-remote-send-error"
                role="alert"
                className={`mb-4 w-full max-w-[800px] px-5 text-[12px] text-[#EA4335] max-sm:px-4 ${hasMessages ? 'mx-auto' : ''}`}
              >
                {remoteSendMessage}
              </div>
            )}

            {remoteChatActive && bs?.remoteProxyDraftError === activeSessionId && (
              <div role="alert" className={`mb-4 w-full max-w-[800px] px-5 text-[12px] text-[#EA4335] max-sm:px-4 ${hasMessages ? 'mx-auto' : ''}`}>
                {t.remoteConversation.draftCapacityExceeded}
              </div>
            )}

            {controlledChatActive && (controlledSkills.length > 0 || controlledCapabilityBusy || controlledCapabilityError) && (
              <fieldset
                data-testid="chat-controlled-capabilities"
                className={`mb-4 w-full max-w-[800px] rounded-xl border border-emerald-500/20 px-4 py-3 text-[12px] ${hasMessages ? 'mx-auto' : ''}`}
              >
                <legend className="px-1 font-medium">{t.remoteConversation.skillsLabel}</legend>
                <div className="flex flex-wrap items-center gap-2">
                  {controlledSkills.map(skill => (
                    <label key={skill.skillId} className="inline-flex items-center gap-1">
                      <input
                        type="checkbox"
                        checked={controlledSkillIds.includes(skill.skillId)}
                        onChange={(event) => setControlledSkillIds(current => event.target.checked
                          ? [...new Set([...current, skill.skillId])]
                          : current.filter(id => id !== skill.skillId))}
                      />
                      <span>{skill.displayName || skill.skillId}</span>
                    </label>
                  ))}
                  {controlledCapabilityBusy && (
                    <span role="status">{t.controllerConnection.remoteCapabilityPreparing}</span>
                  )}
                  {controlledCapabilityError && (
                    <span role="alert" className="inline-flex items-center gap-2 text-[#EA4335]">
                      {t.controllerConnection.remoteCapabilityFailed}
                      <button type="button" onClick={() => { void loadControlledCapabilities(); }}
                        disabled={controlledCapabilityBusy}
                        className="rounded-full border border-current px-2 py-0.5 disabled:opacity-60">
                        {t.controllerConnection.remoteCapabilityRetry}
                      </button>
                    </span>
                  )}
                </div>
              </fieldset>
            )}

            {remoteChatActive && (currentRemoteTaskId || remotePermissions.length > 0) && (
              <section data-testid="chat-remote-task-actions" data-coordination-id={remoteUiCoordinationId || undefined} aria-label={t.remoteConversation.taskActions}
                className={`mb-4 w-full max-w-[800px] px-5 text-[12px] max-sm:px-4 ${hasMessages ? 'mx-auto' : ''}`}>
                {currentRemoteTaskId && (
                  <div className="mb-2 flex items-center gap-2 rounded-xl border border-blue-500/20 bg-blue-500/10 px-3 py-2">
                    <span className="min-w-0 flex-1 truncate">{t.remoteConversation.runningTask}: {currentRemoteTaskId}</span>
                    <button type="button" onClick={cancelCurrentRemoteTask} disabled={!!remoteDispatchAction}
                      aria-label={t.remoteConversation.cancelTask}
                      className="rounded-full border border-current px-2 py-1 disabled:opacity-50">
                      {t.remoteConversation.cancelTask}
                    </button>
                  </div>
                )}
                {remotePermissions.map(permission => (
                  <div key={permission.permissionRequestId} role="alert" data-coordination-id={permission.permissionRequestId} className="mb-2 rounded-xl border border-amber-500/30 bg-amber-500/10 px-3 py-2">
                    <div className="font-medium">{t.remoteConversation.permissionRequest}: {t.controlledDevice[permissionOperationCopyKey(permission.operationType)]}</div>
                    {permissionRedactionCopyKey(permission.preview) && <p className="mt-1 whitespace-pre-wrap break-words">{t.controlledDevice[permissionRedactionCopyKey(permission.preview)]}</p>}
                    {permissionRedactionCopyKey(permission.targetScope) && <p className="mt-1 opacity-70">{t.controlledDevice[permissionRedactionCopyKey(permission.targetScope)]}</p>}
                    {permission.state === 'pending' ? (
                      <div className="mt-2 flex gap-2">
                        <button type="button" data-testid="chat-remote-permission-allow" data-coordination-id={permission.permissionRequestId} onClick={() => decideRemotePermission(permission, 'allow')} disabled={!!remoteDispatchAction}
                          className="rounded-full border px-2 py-1 disabled:opacity-50">{t.remoteConversation.permissionAllow}</button>
                        <button type="button" data-testid="chat-remote-permission-deny" data-coordination-id={permission.permissionRequestId} onClick={() => decideRemotePermission(permission, 'deny')} disabled={!!remoteDispatchAction}
                          className="rounded-full border px-2 py-1 disabled:opacity-50">{t.remoteConversation.permissionDeny}</button>
                      </div>
                    ) : <div className="mt-1 text-amber-700 dark:text-amber-300">{t.remoteConversation.permissionTimedOut}</div>}
                  </div>
                ))}
              </section>
            )}

            {remoteChatActive && remoteArtifacts.length > 0 && (
              <section data-testid="chat-remote-artifacts" aria-label={t.remoteConversation.artifacts}
                className={`mb-4 w-full max-w-[800px] px-5 text-[12px] max-sm:px-4 ${hasMessages ? 'mx-auto' : ''}`}>
                <div className="rounded-xl border border-black/10 bg-black/[0.02] px-3 py-2 dark:border-white/10 dark:bg-white/[0.04]">
                  <div className="mb-2 font-medium">{t.remoteConversation.artifacts}</div>
                  {remoteArtifacts.map(artifact => (
                    <div key={artifact.artifactId} className="flex items-center gap-2 border-t border-black/5 py-2 first:border-t-0 dark:border-white/10">
                      <FileText size={15} aria-hidden="true" />
                      <span className="min-w-0 flex-1 truncate">{artifact.displayName}</span>
                      {artifact.previewKind === 'text' && (
                        <button type="button" data-testid="chat-remote-artifact-preview" onClick={() => previewRemoteArtifact(artifact)} disabled={!!remoteDispatchAction}
                          className="rounded-full border px-2 py-1 disabled:opacity-50">{t.remoteConversation.previewArtifact}</button>
                      )}
                      <button type="button" data-testid="chat-remote-artifact-download" onClick={() => saveRemoteArtifact(artifact)} disabled={!!remoteDispatchAction}
                        className="rounded-full border px-2 py-1 disabled:opacity-50">{t.remoteConversation.downloadArtifact}</button>
                      {artifact.previewKind === 'download_only' && (
                        <span className="text-[11px] opacity-70">{t.remoteConversation.artifactDownloadOnly}</span>
                      )}
                      {artifact.previewKind === 'unsupported' && (
                        <span className="text-[11px] opacity-70">{t.remoteConversation.artifactPreviewUnsupported}</span>
                      )}
                    </div>
                  ))}
                  {remoteArtifactPreview && (
                    <div ref={remoteArtifactPreviewDialogRef} role="dialog" aria-modal="true"
                      aria-labelledby="chat-remote-artifact-preview-title" tabIndex={-1}
                      onKeyDown={event => {
                        if (event.key === 'Escape') {
                          event.preventDefault();
                          event.stopPropagation();
                          closeRemoteArtifactPreview();
                          return;
                        }
                        constrainDialogFocus(event, remoteArtifactPreviewDialogRef.current);
                      }}
                      className="mt-2 rounded-lg bg-black/5 p-3 dark:bg-white/5">
                      <div className="flex items-center gap-2 font-medium">
                        <span id="chat-remote-artifact-preview-title" className="min-w-0 flex-1 truncate">{remoteArtifactPreview.displayName}</span>
                        <button ref={remoteArtifactPreviewCloseRef} data-testid="chat-remote-artifact-preview-close" type="button" onClick={closeRemoteArtifactPreview} aria-label={t.uiArtifacts.closePreviewAria}><X size={15} /></button>
                      </div>
                      <pre className="mt-2 max-h-48 overflow-auto whitespace-pre-wrap break-words">{remoteArtifactPreview.text || t.remoteConversation.previewUnavailable}</pre>
                    </div>
                  )}
                </div>
              </section>
            )}

            {remoteChatActive && remoteActionError && (
              <div role="alert" data-testid="chat-remote-action-error" className={`mb-4 w-full max-w-[800px] px-5 text-[12px] text-[#EA4335] max-sm:px-4 ${hasMessages ? 'mx-auto' : ''}`}>
                {remoteActionError === 'result_unknown'
                  ? t.remoteConversation.actionResultUnknown
                  : t.remoteConversation.actionFailed}
              </div>
            )}

            {remoteChatActive && pendingRemoteDispatches.length > 0 && (
              <div data-testid="chat-remote-pending-dispatches" className={`mb-4 w-full max-w-[800px] px-5 text-[12px] max-sm:px-4 ${hasMessages ? 'mx-auto' : ''}`}>
                {pendingRemoteDispatches.map(item => (
                  <div key={item.taskId} role="status" className="mb-2 rounded-xl border border-amber-500/30 bg-amber-500/10 px-3 py-2">
                    <div>{item.state === 'prepared_not_sent'
                      ? (item.origin === 'history' ? t.remoteConversation.preparedPayloadLost : t.remoteConversation.preparedNotSent)
                      : t.remoteConversation.outcomeUnknownNoResend}</div>
                    {item.handle && <div className="mt-1 font-mono opacity-70">{item.handle.taskId}</div>}
                    {item.state === 'prepared_not_sent' && item.handle && (
                      <div className="mt-2 flex gap-2">
                        {item.origin === 'process' && item.identity === remoteSendIdentity && remoteDispatchReady && (
                          <button type="button" onClick={() => commitPreparedRemoteTask(item)} disabled={!!remoteDispatchAction}
                            className="rounded-full border px-2 py-1 disabled:opacity-50">{t.remoteConversation.commitPreparedTask}</button>
                        )}
                        <button type="button" onClick={() => abandonRemoteTask(item)} disabled={!!remoteDispatchAction || !remoteLinkReady}
                          className="rounded-full border px-2 py-1 disabled:opacity-50">{t.remoteConversation.abandonPreparedTask}</button>
                      </div>
                    )}
                  </div>
                ))}
                {activeRemoteDispatchError && <div role="alert">{t.remoteConversation.pendingActionFailed}</div>}
              </div>
            )}

            {showControlledTask && (
              <div
                data-testid="chat-controlled-task-banner"
                role="status"
                aria-live="polite"
                className={`mb-4 w-full max-w-[800px] px-4 max-sm:px-3 ${hasMessages ? 'mx-auto' : ''}`}
              >
                <div className="rounded-[20px] border border-blue-500/20 bg-blue-500/10 px-4 py-3 text-[#174EA6] shadow-sm dark:border-blue-400/20 dark:bg-blue-400/10 dark:text-[#A8C7FA]">
                  <div className="flex items-center gap-2 text-[13px] font-medium">
                    <Monitor size={16} className="shrink-0" />
                    <span>{t.controlledDevice.runningTasks(controlledActiveTasks.length)}</span>
                    <span className="ml-auto text-[12px] opacity-75">
                      {t.remoteConversation[taskStatusKey(controlledActiveTasks[0].status)]}
                    </span>
                  </div>
                  {controlledActiveTasks[0].text && (
                    <p className="mt-2 line-clamp-3 whitespace-pre-wrap break-words text-[13px] text-[#1F1F1F] dark:text-[#E3E3E3]">
                      {controlledActiveTasks[0].text}
                    </p>
                  )}
                </div>
              </div>
            )}

            {!hasMessages && !welcomeToolId && (
              /* Gemini Style Centered Empty State */
              <div className="w-full max-w-[760px] px-4 text-center mb-12 animate-in slide-in-from-bottom-4 duration-500">
                <h1 data-testid="chat-greeting" className={`${isWeb ? 'text-[28px] leading-[1.35] px-2 [text-wrap:balance] sm:text-[44px] sm:leading-normal sm:px-0' : 'text-[34px] md:text-[44px] leading-tight whitespace-normal break-words'} font-normal mb-2 ${'text-[#1F1F1F] dark:text-[#E3E3E3]'}`}>
                  {t.chatGreeting}
                </h1>
                {/* Scene entry cards: replace the scene-tab stack above the
                    input area so the composer stays clean */}
                <SceneCardGrid
                  items={sceneTabs}
                  activeKey={sceneSubtab}
                  onSelect={handleSubtabChange}
                  onImpression={trackWorkSceneImpression}
                />
                {personalWorkbenchSceneActive && (
                  <TemplateCardGrid
                    templates={PERSONAL_WORKBENCH_TEMPLATES}
                    selectedIndex={PERSONAL_WORKBENCH_TEMPLATES.findIndex(template => template.id === personalWorkbenchTemplateId)}
                    onSelect={handlePersonalWorkbenchTemplateSelect}
                    copy={t.uiChatScenes}
                  />
                )}
              </div>
            )}

            {!hasMessages && welcomeToolId && (
              <div className="max-w-[800px] w-full mx-auto mt-8">
                <ViewErrorBoundary t={t}>
                  <React.Suspense fallback={null}>
                    <LazyToolWelcomeCard
                      toolId={welcomeToolId}
                      theme={theme}
                      t={t}
                      onSend={(q) => {
                        // opt-in is unified inside sendChatMessage (R8-2: chip and
                        // free input share one path); this handler only sends. Note
                        // (round-25 minor 7): unlike handleSend, the chip path does
                        // NOT restore the blocked-abort draft — that gap is the
                        // registered chip-prefill item, not an intended match.
                        Promise.resolve(sendChatMessage(q)).catch((err) => {
                          console.warn("[pinvou3][chat-ui] welcome-card send failed", err);
                        });
                      }}
                    />
                  </React.Suspense>
                </ViewErrorBoundary>
              </div>
            )}

            {hasMessages && (
              // relative: the AuxQuoteSelection chip is absolutely positioned inside this column, clamped to its rect.
              <div ref={conversationContentRef} className="relative max-w-[800px] w-full min-w-0 mx-auto space-y-4">
                {/* Keep this explicit 16px value aligned with space-y-4 so
                    crossing the virtualization threshold does not change spacing. */}
                <ConversationTimeline
                    turns={conversationProjection.turns}
                    sessionId={activeSessionId}
                    scrollElementRef={scrollRef}
                    busy={busy}
                    turnGapPx={16}
                    followOutputRef={autoScrollRef}
                    onRetry={(turn) => {
                      if (!busy && turn.userText && bridge.available) {
                        bridge.interaction.editLastTurn(turn.userText, { retryReason: 'manual_regenerate' });
                      }
                    }}
                    copy={t.uiConversation}
                    agentLabel={chatViewCopy.agentName}
                    assistantAvatar={(timelineAssistantAvatar)}
                    renderUser={handleTimelineRenderUser}
                    renderItem={handleTimelineRenderItem}
                    renderToolItem={handleTimelineRenderToolItem}
                    onOpenExternal={openChatExternalUrl}
                  />
                {/* Selection quotes: select text in the main conversation
                    timeline → stage it as a quote of this task's aux chat and
                    open the panel. Availability matches the header entry
                    (sched- sessions have no aux chat). */}
                <AuxQuoteSelection
                  containerRef={conversationContentRef}
                  sessionId={
                    activeSessionId && !activeSessionId.startsWith('sched-') && bridge.available && bridge.auxChat
                      ? activeSessionId
                      : null
                  }
                  copy={t.uiAuxChat}
                  onQuote={openAuxChatPanel}
                />
                {/* 实体占位必须覆盖输入框和其上方渐变区，保证滚到底时最后一张卡
                    完整停在渐变之外，而不是虽然能滚到却被遮罩淡化。 */}
                <div data-testid="chat-bottom-spacer" aria-hidden="true" className="w-full shrink-0"
                  style={{ height: (composerH ? composerH + 64 : 176) + 'px' }} />
              </div>
            )}

          </div>

          {/* 底部渐变蒙层:内容滚到底时在输入框上方柔和淡出(pointer-events-none 不挡滑动/点击;高度跟随输入框 auto-grow)。 */}
          <div className={`pointer-events-none absolute bottom-0 inset-x-0 z-[15] bg-gradient-to-t to-transparent from-30% via-70% ${'from-white via-white/95 dark:from-[#131314] dark:via-[#131314]/95'}`}
            style={{ height: (composerH ? composerH + 48 : 172) + 'px' }} />
          {hasMessages && showScrollBottom && (
            <div className="pointer-events-none absolute inset-x-0 z-[25] flex justify-center"
              style={{ bottom: (composerH ? composerH + 54 : 172) + 'px' }}>
              <button
                type="button"
                onClick={scrollChatToBottom}
                aria-label={t.backToBottom}
                title={t.backToBottom}
                className={`pointer-events-auto w-9 h-9 rounded-full flex items-center justify-center shadow-lg backdrop-blur transition-all hover:-translate-y-0.5 active:translate-y-0 ${
                  'bg-white/95 text-[#1F1F1F] border border-black/10 hover:bg-[#F8FAFF] dark:bg-[#2B2C2F]/95 dark:text-[#E3E3E3] dark:border-white/10 dark:hover:bg-[#34363A]'
                }`}>
                <ChevronDown size={15} />
              </button>
            </div>
          )}
          {hasMessages && chatItems.some((item) => item.type === 'memory_candidate' && !item.resolved) && (
            <div className="pointer-events-none absolute inset-x-0 z-[24]"
              style={{ ...responsiveGutterStyle, bottom: (composerH ? composerH + 28 : 148) + 'px' }}>
              <div className="max-w-[800px] w-full mx-auto flex flex-col items-end gap-3">
                {chatItems
                  .filter((item) => item.type === 'memory_candidate' && !item.resolved)
                  .slice(-2)
                  .map((item) => (
                    <div key={item.id} className="pointer-events-auto w-full flex justify-end">
                      <ChatBubble item={item} sessionId={activeSessionId} theme={theme} t={t} onPrefill={setInputText} onSend={sendChatMessage} editable={false} onOpenEditor={onOpenEditor} onPlanStuckGo={handlePlanStuckGo} isLatestArtifact={false} />
                    </div>
                ))}
              </div>
            </div>
          )}
          {voiceIntroOpen && (
            <VoiceShortcutIntroModal
              isDark={theme === 'dark'}
              copy={t}
              onClose={handleVoiceIntroClose}
              onToggleShortcut={handleVoiceIntroToggleShortcut}
            />
          )}
          {COMPUTER_USE_ENABLED && (
            <ComputerUseDialogs slice={computerUseSlice} copy={computerUseCopy} />
          )}
          {/* Floating Input Area */}
          <div
            ref={composerWrapRef}
            data-testid="chat-composer-wrap"
            className={`absolute ${isWeb ? 'bottom-2 sm:bottom-8' : 'bottom-8'} inset-x-0 z-20`}
            style={responsiveGutterStyle}
          >
            <div className="max-w-[800px] w-full mx-auto">
              {!remoteTranscriptActive && COMPUTER_USE_ENABLED && (
                <ComputerUseBanner slice={computerUseSlice} copy={computerUseCopy} />
              )}
              {!remoteTranscriptActive && !scheduledRunContext && !conversationStarted && (
                <HomeModeSwitcher
                  mode={pinvouMode}
                  codeSupported={codeModeAvailable}
                  isDark={theme === 'dark'}
                  onChange={handleHomeModeChange}
                  copy={t.uiHomeMode}
                />
              )}
            {/* Queued-message overlay. Pin makes an item the next safe local
                send without interrupting the current response; edit preserves
                attachments and scene metadata. Engine-injected steer chips
                are mutated only after the bridge confirms their withdrawal.
                ⚡ remains the explicit interrupt-and-send action. */}
            {!remoteTranscriptActive && (queued.length > 0 || queuedActionError) && (
              <div className={`mb-2 rounded-2xl border shadow-lg backdrop-blur-xl overflow-hidden max-h-[40vh] overflow-y-auto ${'border-black/[0.06] bg-white/90 dark:border-white/10 dark:bg-[#161618]/90'}`}>
                {queued.map((q, index) => (
                  <div key={q.id}
                    className={`flex items-center gap-2 px-3 py-2 text-[12px] text-[#444746] dark:text-[#C4C7C5] ${index > 0 ? 'border-t border-black/[0.06] dark:border-white/10' : ''}`}>
                    <span className="opacity-60 shrink-0">{t.queuedTag}</span>
                    {queuedEdit && queuedEdit.id === q.id ? (
                      <textarea
                        data-testid={`queued-message-edit-${q.id}`}
                        ref={queuedEditInputRef}
                        value={queuedEdit.text}
                        rows={2}
                        aria-label={t.queuedEdit}
                        onChange={(event) => setQueuedEdits(current => ({
                          ...current,
                          [activeSessionId]: { ...current[activeSessionId], text: event.target.value },
                        }))}
                        onKeyDown={(event) => {
                          if (event.key === 'Escape') {
                            setQueuedEdits(current => {
                              const next = { ...current };
                              delete next[activeSessionId];
                              return next;
                            });
                          }
                          if (isPlainEnter(event)) {
                            event.preventDefault();
                            void handleSaveQueuedEdit(q);
                          }
                        }}
                        className="flex-1 min-w-0 resize-none rounded-lg border border-black/10 bg-white/80 px-2 py-1 outline-none focus:border-blue-500 dark:border-white/15 dark:bg-white/5"
                      />
                    ) : (
                      <span className="flex-1 min-w-0 truncate">{q.displayText}</span>
                    )}
                    {queuedEdit && queuedEdit.id === q.id ? (
                      <>
                        <button type="button" onClick={() => void handleSaveQueuedEdit(q)}
                          data-testid={`queued-message-save-${q.id}`}
                          disabled={queuedActionBusy}
                          aria-label={t.queuedSave} title={t.queuedSave}
                          className="w-6 h-6 shrink-0 rounded-full flex items-center justify-center text-emerald-600 hover:bg-emerald-500/10 disabled:opacity-40 dark:text-emerald-400">
                          <Check size={14} />
                        </button>
                        <button type="button" onClick={() => setQueuedEdits(current => {
                          const next = { ...current };
                          delete next[activeSessionId];
                          return next;
                        })}
                          disabled={queuedActionBusy}
                          aria-label={t.queuedEditCancel} title={t.queuedEditCancel}
                          className="w-6 h-6 shrink-0 rounded-full flex items-center justify-center text-[#5F6368] hover:bg-black/5 disabled:opacity-40 dark:text-[#C4C7C5] dark:hover:bg-white/10">
                          <X size={14} />
                        </button>
                      </>
                    ) : (
                      <>
                        <button type="button" onClick={() => handlePrioritizeQueued(q.id)}
                          data-testid={`queued-message-prioritize-${q.id}`}
                          disabled={queuedActionBusy || !!queuedEdit || isMultiAgentReadOnly}
                          aria-label={t.queuedPrioritize} title={t.queuedPrioritize}
                          className="w-6 h-6 shrink-0 rounded-full flex items-center justify-center text-[#5F6368] hover:bg-black/5 disabled:opacity-40 dark:text-[#C4C7C5] dark:hover:bg-white/10">
                          <PinIcon size={14} />
                        </button>
                        <button type="button" onClick={() => {
                          setQueuedActionErrors(current => {
                            const next = { ...current };
                            delete next[activeSessionId];
                            return next;
                          });
                          setQueuedEdits(current => ({
                            ...current,
                            [activeSessionId]: { id: q.id, text: String(q.text || ''), initial: String(q.text || '') },
                          }));
                        }}
                          data-testid={`queued-message-edit-action-${q.id}`}
                          disabled={queuedActionBusy || !!queuedEdit || isMultiAgentReadOnly}
                          aria-label={t.queuedEdit} title={t.queuedEdit}
                          className="w-6 h-6 shrink-0 rounded-full flex items-center justify-center text-[#5F6368] hover:bg-black/5 disabled:opacity-40 dark:text-[#C4C7C5] dark:hover:bg-white/10">
                          <Edit2 size={14} />
                        </button>
                      </>
                    )}
                    {can('interruptSend') && (
                      <button type="button" onClick={() => handleInterruptQueued(q.id)}
                        disabled={queuedActionBusy || !!queuedEdit || isMultiAgentReadOnly}
                        aria-label={t.interruptMsg} title={t.interruptMsgTip}
                        className="w-6 h-6 shrink-0 rounded-full flex items-center justify-center transition-colors text-blue-500 dark:text-blue-400 hover:bg-blue-500/10 active:bg-blue-500/15 disabled:opacity-40">
                        <Zap size={14} />
                      </button>
                    )}
                    <button type="button" onClick={() => bridge.chat.removeQueued(q.id)}
                      disabled={queuedActionBusy || !!queuedEdit || isMultiAgentReadOnly}
                      aria-label={t.queuedCancel} title={t.queuedCancel}
                      className={`w-6 h-6 shrink-0 rounded-full flex items-center justify-center transition-colors text-[#5F6368] dark:text-[#C4C7C5] disabled:opacity-40 ${'hover:bg-[#F0F4F9] dark:hover:bg-[#333537]'}`}>
                      <X size={14} />
                    </button>
                  </div>
                ))}
                {queuedActionError && (
                  <div role="status" className="border-t border-black/[0.06] px-3 py-2 text-[11px] text-amber-700 dark:border-white/10 dark:text-amber-300">
                    {queuedActionError}
                  </div>
                )}
              </div>
            )}
            {/* 模型选择器/知识库挂载已挪进下方底栏(ComposerModelSelector/ComposerKbSelector) */}
            {/* 附件 chips */}
            {remoteChatActive && remoteFiles.length > 0 && (
              <div data-testid="chat-remote-attachments" data-coordination-id={remoteUiCoordinationId || undefined} className="mb-2 flex flex-wrap gap-2 px-2">
                {remoteFiles.map((file, index) => (
                  <div key={`${file.name}:${file.size}:${index}`} className="flex max-w-full items-center gap-2 rounded-full border border-blue-500/20 bg-blue-500/10 px-3 py-1.5 text-[12px]">
                    <Paperclip size={14} aria-hidden="true" />
                    <span className="max-w-[240px] truncate">{file.name}</span>
                    <button type="button" data-testid="chat-remote-attachment-remove" onClick={() => setRemoteFiles(files => files.filter((_, itemIndex) => itemIndex !== index))}
                      disabled={remoteTransfer?.state === 'uploading'} aria-label={t.remoteConversation.removeAttachment(file.name)}
                      className="rounded-full p-0.5 disabled:opacity-50"><X size={13} /></button>
                  </div>
                ))}
              </div>
            )}
            {remoteChatActive && remoteTransfer?.state === 'uploading' && (
              <div role="status" aria-live="polite" className="mb-2 px-3 text-[12px] text-[#1A73E8] dark:text-[#8AB4F8]">
                {t.remoteConversation.uploadingAttachment(remoteTransfer.fileName || '', remoteTransfer.receivedBytes || 0, remoteTransfer.totalBytes || 0)}
              </div>
            )}
            {!remoteChatActive && <AttachmentChips
              attachments={attachments}
              onRemove={id => bridge.attachments.removeAttachment(id)}
              dark={theme === 'dark'}
              parsingLabel={t.attachParsing}
              uploadingLabel={t.attachUploading}
              failedLabel={t.attachFailed}
              removeLabel={t.uiAttachments.remove}
              formatError={formatAttachmentError}
              className="mb-2 px-2"
            />}
            {!remoteTranscriptActive && imageInputWarning && (
              <div data-testid="image-capability-warning"
                className="flex items-center gap-2 mb-2 px-3 py-2 rounded-2xl text-[12px] leading-5 bg-amber-500/10 text-amber-700 dark:text-amber-300">
                <AlertTriangle size={14} className="shrink-0 text-amber-500" />
                <span className="min-w-0">{imageInputWarning}</span>
              </div>
            )}
            {!remoteTranscriptActive && imagePrivacyHint && (
              <div data-testid="image-privacy-hint"
                className="mb-2 px-3 text-[11px] leading-4 text-black/45 dark:text-white/45">
                {imagePrivacyHint}
              </div>
            )}
            <VoiceComposerStatus
              voiceInput={voiceInput}
              voiceMode={voiceMode}
              copy={t}
              chatCopy={chatCopy}
              dark={theme === 'dark'}
              voiceAsrReadyNotice={voiceAsrReadyNotice}
              canInstallLocalAsr={canInstallLocalAsr}
              onGotoSettings={onGotoSettings}
              onRetry={() => handleVoiceTrigger(voiceMode, { source: 'button' })}
              onCancel={handleVoiceCancel}
              onClose={handleVoiceClose}
            />
            {voiceAsrSetup.open && !canInstallLocalAsr && (
              <div className={`flex items-center justify-between gap-3 mb-2 px-3 py-2 rounded-2xl text-[12px] ${'bg-[#E8F0FE] text-[#174EA6] dark:bg-[#1E2B3A] dark:text-[#A8C7FA]'}`}>
                <span>{chatCopy.asrUnavailable}</span>
                <button type="button" onClick={() => { pendingVoiceAfterIntroRef.current = null; bridge.voice.closeVoiceAsrSetup(); }} className={`shrink-0 px-2 py-1 rounded-full font-medium ${'hover:bg-black/5 dark:hover:bg-white/10'}`}>{chatCopy.gotIt}</button>
              </div>
            )}
            {voiceAsrSetup.open && canInstallLocalAsr && !voiceAsrSetup.status?.installable && voiceAsrSetupPublicationReady && (() => {
              const su = voiceAsrSetup;
              const prog = su.progress || {};
              const pct = (prog.stage === 'model' && prog.total) ? Math.floor(prog.downloaded / prog.total * 100) : null;
              const missing = (su.status && su.status.missing) || [];
              const needFfmpeg = missing.indexOf('ffmpeg') >= 0;
              const needModel = missing.indexOf('model') >= 0;
              const needEngine = missing.indexOf('engine') >= 0 || missing.indexOf('runtime') >= 0;
              const modelSizeText = (su.status && su.status.engine && needModel && !needFfmpeg) ? chatCopy.sizeModelOnly : chatCopy.sizeFull;
              return (
                // biome-ignore lint/a11y/useKeyWithClickEvents: background click-to-close layer; keyboard path handled by the dialog's cancel button
                // biome-ignore lint/a11y/noStaticElementInteractions: background click-to-close layer; non-interactive container
                <div data-testid="voice-asr-setup-dialog" className="fixed inset-0 z-[80] flex items-center justify-center p-4 bg-black/45"
                  onClick={() => { if (!su.installing) { pendingVoiceAfterIntroRef.current = null; bridge.voice.closeVoiceAsrSetup(); } }}>
                  {/* biome-ignore lint/a11y/useKeyWithClickEvents: click bubble-stop layer; keyboard events need no bubbling handling */}
                  {/* biome-ignore lint/a11y/noStaticElementInteractions: click bubble-stop layer; non-interactive container */}
                  <div className={`w-full max-w-[440px] rounded-[20px] shadow-2xl p-6 ${'bg-white text-[#1F1F1F] dark:bg-[#1E1F20] dark:text-[#E3E3E3]'}`}
                    onClick={e => e.stopPropagation()}>
                    <h3 className="text-[16px] font-semibold mb-2">
                      {su.installing ? chatCopy.asrDownloadTitle : chatCopy.asrEnableTitle}
                    </h3>
                    {!su.installing && (
                      <p className="text-[13px] leading-relaxed opacity-80 mb-4">
                        {needEngine
                          ? chatCopy.asrRuntimeMissing
                          : chatCopy.asrFirstUse(modelSizeText, needFfmpeg)}
                      </p>
                    )}
                    {su.installing && (
                      <div className="mb-4">
                        <div className="text-[12px] opacity-70 mb-1">
                          {prog.stage === 'model'
                            ? chatCopy.downloadingModel(pct != null ? pct + '%' : '…')
                            : (chatCopy.asrStages[prog.stage] || chatCopy.asrStages.preparing)}
                        </div>
                        <div className={`h-2 rounded-full overflow-hidden ${'bg-black/10 dark:bg-white/10'}`}>
                          <div className="h-full bg-[#0B57D0] transition-all" style={{ width: (pct != null ? pct : 30) + '%' }} />
                        </div>
                      </div>
                    )}
                    {su.error && <div className="text-[13px] text-[#EA4335] mb-3">❌ {su.error}</div>}
                    <div className="flex items-center justify-end gap-2">
                      <button type="button" onClick={() => { pendingVoiceAfterIntroRef.current = null; bridge.voice.cancelVoiceAsrSetup?.(); }} disabled={su.cancelling}
                        className={`text-[13px] px-4 py-2 rounded-full ${'bg-[#E1E5EA] hover:bg-[#D3D9E0] dark:bg-[#333537] dark:hover:bg-[#444746]'} ${su.cancelling ? 'opacity-50' : ''}`}>
                        {su.installing ? (su.cancelling ? chatCopy.cancelling : chatCopy.cancelDownload) : chatCopy.cancel}</button>
                      {!su.installing && (
                        <button onClick={() => bridge.voice.installVoiceAsr()} disabled={!su.status?.installable}
                          className={`text-[13px] font-medium px-4 py-2 rounded-full ${'bg-[#0B57D0] text-white hover:bg-[#1967D2] dark:bg-[#A8C7FA] dark:text-[#041E49] dark:hover:bg-[#C2D7FB]'} ${!su.status?.installable ? 'opacity-50' : ''}`}>
                          {!su.status?.installable ? chatCopy.repairInstall : (needModel ? chatCopy.downloadModel : chatCopy.install)}</button>
                      )}
                    </div>
                  </div>
                </div>
              );
            })()}
            <div className="relative bg-white/80 dark:bg-[#161618]/85 backdrop-blur-2xl border border-black/[0.06] dark:border-white/10 rounded-[28px] shadow-lg focus-within:border-blue-400/50 dark:focus-within:border-blue-500/50 transition-colors px-4 pt-3 pb-2.5">
              <VoiceComposerPillLayer
                voiceInput={voiceInput}
                voiceMode={voiceMode}
                copy={t}
                onCancel={handleVoiceCancel}
                onConfirm={() => handleVoiceTrigger(voiceMode)}
              />
              {sceneCapabilityStatus && (
                <div
                  data-testid="scene-capability-status"
                  className={`mb-2 flex items-center gap-2 rounded-2xl px-3 py-2 text-[13px] ${
                    sceneCapabilityStatus.kind === 'error'
                      ? 'bg-[#FCE8E6] text-[#C5221F] dark:bg-[#3A1F1F] dark:text-[#F28B82]'
                      : sceneCapabilityStatus.kind === 'ready'
                        ? 'bg-[#E6F4EA] text-[#137333] dark:bg-[#10281D] dark:text-[#81C995]'
                        : 'bg-[#E8F0FE] text-[#174EA6] dark:bg-[#1E2B3A] dark:text-[#A8C7FA]'
                  }`}
                >
                  <span className={`h-2 w-2 shrink-0 rounded-full ${
                    sceneCapabilityStatus.kind === 'error'
                      ? 'bg-[#EA4335]'
                      : sceneCapabilityStatus.kind === 'ready'
                        ? 'bg-[#34A853]'
                        : 'bg-[#1A73E8] animate-pulse'
                  }`} />
                  {/* Round-11 m14: no truncate — blocked id lists must stay
                      fully readable (the actionable part was ellipsized). */}
                  <span className="min-w-0 break-words">{sceneCapabilityStatus.text}</span>
                </div>
              )}
              {!scheduledRunContext && !conversationStarted && activeScene && (
                <SceneModeTag
                  scene={activeScene}
                  onClear={handleClearActiveScene}
                  clearLabel={sceneCopy.clear(activeScene.label)}
                />
              )}
              <div className="flex items-center gap-2 mb-0.5">
                <LiveConversationActivityIndicator
                  turn={activeConversationTurn}
                  onRequestAttention={scrollChatToBottom}
                  copy={t.uiConversation}
                />
                {/* 后台 shell 任务胶囊：跟随"处理中"提示行，任务存续期间常驻（轮次结束后仍运行时也保留入口） */}
                <BackgroundTasksIndicator tasks={runningShellTasks} t={t} chatCopy={chatCopy} compact={composerCompact} />
              </div>
              {isMultiAgentReadOnly ? (
                <div
                  role="note"
                  data-testid="multiagent-desktop-only"
                  className="min-h-[48px] px-1 py-3 text-[13px] leading-5 text-[#5F6368] dark:text-[#9AA0A6]"
                >
                  {t.multiAgentDesktopOnly}
                </div>
              ) : (
                <>
              <VoiceEditPreview
                preview={chatVoice.editPreview}
                copy={t}
                onApply={() => chatVoice.applyVoiceEditPreview()}
                onApplyAndSend={() => chatVoice.applyVoiceEditPreview({ send: true })}
                onCancel={chatVoice.cancelVoiceEditPreview}
              />
              <textarea
                ref={composerRef}
                data-testid="chat-composer-input"
                value={inputText}
                onChange={e => handleComposerInputChange(e.target.value)}
                onKeyDown={handleKeyDown}
                onPaste={handlePaste}
                readOnly={remoteChatActive && !remoteLinkReady}
                maxLength={CHAT_INPUT_MAX_LENGTH}
                placeholder={composerPlaceholder}
                rows={1}
                className="w-full bg-transparent resize-none outline-none text-gray-800 dark:text-gray-100 text-[16px] leading-relaxed min-h-[48px] overflow-y-auto hide-scrollbar placeholder:text-gray-400 dark:placeholder:text-gray-500"
              />
              <TextareaContextMenu inputRef={composerRef} setValue={setInputText} theme={theme} t={t} />
              {inputLimitReached && (
                <div role="status" aria-live="polite" data-testid="chat-input-limit-notice"
                  className={`px-1 pb-1 text-[12px] ${'text-[#C5221F] dark:text-[#F28B82]'}`}>
                  {t.chatInputLimitReached(CHAT_INPUT_MAX_LENGTH.toLocaleString())}
                </div>
              )}
              <div className="flex items-center justify-between mt-1.5 gap-2">
                <div className="flex items-center gap-1.5 min-w-0 flex-1">
                  {!remoteTranscriptActive && <>
                    <ComposerAttachButton t={t} compact={composerCompact} />
                  {/* Draft-mode workspace selector (matching the code mode draft
                      selector): desktop only + draft mode; the Web bridge lacks
                      sessions.setDraftWorkspace/pickDraftWorkspace and the
                      method-existence guard hides it. bs.draftWorkspacePath is
                      absent in the Web snapshot; || null covers it. */}
                  {!activeSessionId && can('desktopChrome') && bridge.sessions && typeof bridge.sessions.pickDraftWorkspace === 'function' && (
                    <ComposerWorkspaceSelector
                      copy={t.uiChatWorkspace}
                      draftWorkspacePath={(bs && bs.draftWorkspacePath) || null}
                      onPickWorkspace={() => bridge.sessions.pickDraftWorkspace()}
                      onSelectWorkspace={path => bridge.sessions.setDraftWorkspace(path)}
                    />
                  )}
                  {/* Workspace binding indicator for the active session (read-only
                      chip: directory name + full path in title); bound sessions
                      match the code mode safety posture, styled like the draft-mode
                      selector. */}
                  {shouldShowWorkspaceBindingChip({ activeSessionId, sessionBinding: sessionWorkspaceBinding }) && (
                    <span
                      data-testid="chat-workspace-binding"
                      title={sessionWorkspaceBinding}
                      className="h-7 max-w-[180px] rounded-lg px-2 inline-flex items-center gap-1.5 text-[11px] text-gray-500 dark:text-gray-400"
                    >
                      <FolderOpen size={13} className="shrink-0" />
                      <span className="truncate">{workspaceName(sessionWorkspaceBinding, t.uiChatWorkspace.unknownDirectory)}</span>
                    </span>
                  )}
                  <ComposerModeChip t={t} bs={bs} compact={composerCompact} onSwitch={handleModeChipSwitch} />
                  {/* Scheduled run conversations expose no swarm toggle:
                      the backend's swarm_mode_available excludes them (the
                      engine always assembles plain config there), so the
                      entry hides up front instead of erroring on click. */}
                  <ComposerModelSelector t={t} bs={bs} onGotoSettings={onGotoModelSettings || onGotoSettings} compact={composerCompact} multiAgentAvailable={!scheduledRunContext} />
                  <ComposerToolMenu t={t} onGotoTools={onGotoTools} sessionId={bs && bs.activeSessionId} compact={composerCompact} activeSkill={bs && bs.activeSkill} />
                    <ComposerKbSelector t={t} bs={bs} compact={composerCompact} />
                  </>}
                  {controlledChatActive && !remoteChatActive && (
                    <ComposerAttachButton t={t} compact={composerCompact} />
                  )}
                  {remoteChatActive && (
                    <>
                      <input ref={remoteFileInputRef} data-testid="chat-remote-attachment-input" data-coordination-id={remoteUiCoordinationId || undefined} type="file" multiple className="hidden" onChange={selectRemoteFiles}
                        aria-label={t.remoteConversation.addAttachment} />
                      <button type="button" onClick={() => remoteFileInputRef.current?.click()}
                        disabled={!remoteLinkReady || !!remoteDispatchAction || remoteTransfer?.state === 'uploading'}
                        aria-label={t.remoteConversation.addAttachment} title={t.remoteConversation.addAttachment}
                        className="flex h-9 items-center gap-1 rounded-full px-2 text-[#5F6368] hover:bg-black/5 disabled:opacity-50 dark:text-[#C4C7C5] dark:hover:bg-white/10">
                        <Paperclip size={17} /><span className={composerCompact ? 'sr-only' : ''}>{t.remoteConversation.addAttachment}</span>
                      </button>
                    </>
                  )}
                </div>
                {hasDraftText && (
                  <button onClick={handleClearInput} disabled={!canClearInput} aria-label={t.clearInput} title={t.clearInput}
                    className={`w-9 h-9 shrink-0 rounded-full flex items-center justify-center transition-colors ${
                      canClearInput
                        ? 'text-[#5F6368] hover:bg-black/5 dark:text-[#C4C7C5] dark:hover:bg-white/10'
                        : 'text-gray-400 cursor-not-allowed opacity-60'
                    }`}>
                    <Trash2 size={18} />
                  </button>
                )}
                <VoiceComposerButton
                  refProp={voiceAsrPopoverRef}
                  voiceInput={voiceInput}
                  voiceMode={voiceMode}
                  voiceAsrSetup={voiceAsrSetup}
                  voiceAsrPopoverOpen={voiceAsrPopoverOpen}
                  copy={t}
                  disabled={primaryVoiceDisabled}
                  onClick={handleVoiceClick}
                  onToggleAsrPopover={() => setVoiceAsrPopoverOpen(open => !open)}
                  onCloseAsrPopover={() => setVoiceAsrPopoverOpen(false)}
                  onCancelAsr={() => {
                    setVoiceAsrPopoverOpen(false);
                    // User abandoned the install: clear the pending voice intent so a later
                    // install completion cannot auto-resume the stale intent into a recording
                    // the user never asked for.
                    pendingVoiceAfterIntroRef.current = null;
                    bridge.voice.cancelVoiceAsrSetup?.();
                  }}
                />
                {(() => {
                  // While busy, Stop is always shown (typing mid-generation
                  // must still allow "stop but keep the draft").
                  // The send button while busy = steer into the current turn
                  // (with attachments, local queuing); zap-send moved onto the
                  // queued chips (one per entry), not the send area.
                  // While a voice rewrite preview is open, disable the primary send: sending is
                  // funneled into the preview card (apply and send), so buttons based on
                  // inputText cannot send the raw text or double-send during the preview.

                  const ready = canSend
                    && !sceneCapabilityPreparing
                    && !chatVoice.editPreview
                    && (!remoteChatActive || remoteNewDispatchReady)
                    && (!controlledChatActive || remoteChatActive || controlledLinkReady);
                  const isQueue = busy && ready;
                  return (
                    <div className="flex items-center gap-1">
                      {busy && !remoteTranscriptActive && (
                        <button type="button" onClick={handleCancel} disabled={cancellingSessionIds.has(activeSessionId)}
                          className="w-9 h-9 shrink-0 rounded-full flex items-center justify-center bg-black/5 dark:bg-white/10 text-[#C5221F] dark:text-[#F28B82] hover:bg-black/10 dark:hover:bg-white/20 disabled:opacity-50 disabled:cursor-not-allowed transition-colors">
                          <StopCircle size={20} />
                        </button>
                      )}
                      {(!busy || hasDraftText || hasReadyAttachment) && (
                        <button type="button" data-testid="chat-composer-send" onClick={handleSend} disabled={!ready}
                          aria-label={busy ? t.queueMsg : t.sendMsg}
                          title={busy ? (can('interruptSend') ? t.queueMsgTip : t.queueMsg) : t.sendMsg}
                          className={`w-9 h-9 shrink-0 rounded-full flex items-center justify-center transition-all ${ready ? (isQueue ? 'bg-gradient-to-b from-[#47A1FF] to-[#007AFF] text-white shadow-md ring-2 ring-amber-300 dark:ring-amber-400' : 'bg-gradient-to-b from-[#47A1FF] to-[#007AFF] text-white shadow-md hover:-translate-y-0.5 active:translate-y-0') : 'bg-black/5 dark:bg-white/10 text-gray-400 cursor-not-allowed'}`}>
                          <Send size={17} className="translate-x-[1px]" />
                        </button>
                      )}
                    </div>
                  );
                })()}
              </div>
                </>
              )}
            </div>
            {ctxTokens && ctxTokens.max > 0 && (
              <div className={`mt-1.5 px-5 text-[11px] font-mono ${
                ctxPct >= 0.9 ? 'text-[#C5221F] dark:text-[#F28B82]'
                : ctxPct >= 0.75 ? 'text-[#B06000] dark:text-[#F9AB00]'
                : 'text-[#9AA0A6] dark:text-[#5F6368]'}`}>
                {t.ctxUsage} {ctxTokens.input > 0 ? formatCompactCount(ctxTokens.input) : '—'} / {formatCompactCount(ctxTokens.max)} · {Math.round(ctxPct * 100)}%
              </div>
            )}
            <div className="flex items-center justify-center mt-3">
               <p data-testid="chat-disclaimer" className={`text-[12px] ${'text-[#757575] dark:text-[#8E8E8E]'}`}>{t.disclaimer}</p>
            </div>
            </div>
          </div>
          </div>{/* /对话列 */}

          {pendingChatYoloSwitch && (
            // One-time YOLO confirmation card (global memory) for the first switch of
            // a bound session/draft; confirming continues the switch, cancelling
            // stays in Plan. Mounted outside the conversation column (the card
            // portals itself to body, escaping the composer container's fixed
            // containing block from backdrop-blur — same as the code page).
            <YoloConfirmCard
              theme={theme}
              copy={{
                title: t.uiChatWorkspace.yoloConfirmTitle,
                body: t.uiChatWorkspace.yoloConfirmBody,
                hint: t.uiChatWorkspace.yoloConfirmHint,
                ok: t.uiChatWorkspace.yoloConfirmOk,
                cancel: t.uiChatWorkspace.yoloConfirmCancel,
              }}
              error={chatYoloConfirmError}
              busy={chatYoloConfirmBusy}
              onConfirm={confirmChatYoloSwitch}
              onCancel={() => setPendingChatYoloSwitch(false)}
            />
          )}

          {artifactsVisible && artifactsFullscreen && artifactFullscreenPublicationReady && createPortal(
            <div
              ref={artColRef}
              className="fixed left-0 right-0 bottom-0 z-[1000] pointer-events-auto"
              style={{ top: can('desktopChrome') ? '36px' : 0 }}
              data-testid="artifact-fullscreen-panel">
              <ViewErrorBoundary t={t} variant="panel">
              <PanelSuspense>
              <LazyArtifactsPanel {...artifactsPanelProps} isFullscreen={true} onToggleFullscreen={() => setArtifactsFullscreen(false)} />
              </PanelSuspense>
              </ViewErrorBoundary>
            </div>,
            document.body
          )}

          {artifactsVisible && !artifactsFullscreen && (
            <RightDockPanel
              panelId="artifact-preview"
              visible={rightDockActivePanelId !== 'browser'}
              activationKey={artifactDockActivation}
              dataTestId="artifact-side-panel"
            >
              <ViewErrorBoundary t={t} variant="panel">
              <PanelSuspense>
                <LazyArtifactsPanel {...artifactsPanelProps} isFullscreen={false} onToggleFullscreen={() => setArtifactsFullscreen(true)} />
              </PanelSuspense>
              </ViewErrorBoundary>
            </RightDockPanel>
          )}
          {subagentPanel && (
            <ViewErrorBoundary t={t} variant="panel">
            <PanelSuspense>
            <LazySubagentTranscriptPanel
              sessionId={activeSessionId}
              initialAgentId={subagentPanel.agentId}
              selectionRequestId={subagentPanel.selectionRequestId}
              t={t}
              theme={theme}
              language={modelServiceLanguage}
              modelServiceState={chatModelServiceState}
              onClose={closeSubagentPanel}
            />
            </PanelSuspense>
            </ViewErrorBoundary>
          )}
          {/* sched- run sessions have no aux chat (the entry button is hidden
              too): switching to sched- unmounts the panel with the mount
              condition, and switching back to a normal session re-mounts it
              and re-runs ensure automatically. The bridge conjuncts match the
              entry button exactly (round-26 minor M4): the auxChat-domain
              conjunct keeps the panel unreachable on hosts that serve no
              auxChat surface at all (a stale relay host predating the
              domain, or the unavailable-bridge pet window) even if state
              somehow lingers — the web lane itself implements the domain
              fully these days. */}
          {auxChatPanel && activeSessionId && !activeSessionId.startsWith('sched-')
            && bridge.available && bridge.auxChat && (
            <ViewErrorBoundary t={t} variant="panel">
            <PanelSuspense>
            <LazyAuxChatPanel
              sessionId={activeSessionId}
              activationKey={auxChatPanel.openTick}
              onActiveChange={setAuxChatDockActive}
              t={t}
              theme={theme}
              onClose={closeAuxChatPanel}
            />
            </PanelSuspense>
            </ViewErrorBoundary>
          )}
        </div>
      );
    };

    // ==========================================
    // Chat Bubble (message rendering)
    // ==========================================
    const SelectionCopyButton = ({ hostRef, targetRef, theme, t }) => {
      const [selCopy, setSelCopy] = useState({ visible: false, copied: false, text: '', x: 0, y: 0 });
      const hideTimerRef = useRef(null);

      const hideSelectionCopy = useCallback(() => {
        if (hideTimerRef.current) {
          clearTimeout(hideTimerRef.current);
          hideTimerRef.current = null;
        }
        setSelCopy(s => s.visible ? { ...s, visible: false, copied: false } : s);
      }, []);

      const openSelectionCopyMenu = useCallback((event) => {
        const target = targetRef.current;
        const host = hostRef.current;
        if (!target || !host || !window.getSelection) return false;
        const selection = window.getSelection();
        if (!selection || selection.rangeCount === 0) { hideSelectionCopy(); return false; }
        const text = selection.toString();
        if (!text || !text.trim()) { hideSelectionCopy(); return false; }
        if (!selection.anchorNode || !selection.focusNode) { hideSelectionCopy(); return false; }
        if (!target.contains(selection.anchorNode) || !target.contains(selection.focusNode)) {
          hideSelectionCopy();
          return false;
        }
        const hostRect = host.getBoundingClientRect();
        if (!hostRect) { hideSelectionCopy(); return false; }
        const minX = 4;
        const maxX = Math.max(minX, hostRect.width - 100);
        const x = Math.max(minX, Math.min(event.clientX - hostRect.left, maxX));
        const y = Math.max(4, event.clientY - hostRect.top + 8);
        setSelCopy({ visible: true, copied: false, text: text, x: x, y: y });
        return true;
      }, [hideSelectionCopy, hostRef, targetRef]);

      useEffect(() => {
        return () => {
          if (hideTimerRef.current) clearTimeout(hideTimerRef.current);
        };
      }, []);

      useEffect(() => {
        const target = targetRef.current;
        if (!target) return;
        const onContextMenu = (e) => {
          if (openSelectionCopyMenu(e)) {
            e.preventDefault();
            e.stopPropagation();
          } else {
            hideSelectionCopy();
          }
        };
        target.addEventListener('contextmenu', onContextMenu);
        return () => {
          target.removeEventListener('contextmenu', onContextMenu);
        };
      }, [hideSelectionCopy, openSelectionCopyMenu, targetRef]);

      useEffect(() => {
        if (!selCopy.visible) return;
        const onDown = (e) => {
          if (e.target && e.target.closest && e.target.closest('[data-selection-copy-button]')) return;
          hideSelectionCopy();
        };
        const onKey = (e) => { if (e.key === 'Escape') hideSelectionCopy(); };
        document.addEventListener('mousedown', onDown, true);
        document.addEventListener('keydown', onKey, true);
        return () => {
          document.removeEventListener('mousedown', onDown, true);
          document.removeEventListener('keydown', onKey, true);
        };
      }, [hideSelectionCopy, selCopy.visible]);

      const onCopy = () => {
        copyClipboardText(selCopy.text).then(function (ok) {
          if (!ok) return;
          setSelCopy(s => ({ ...s, copied: true }));
          if (hideTimerRef.current) clearTimeout(hideTimerRef.current);
          hideTimerRef.current = setTimeout(function () {
            hideTimerRef.current = null;
            hideSelectionCopy();
          }, 900);
        });
      };

      if (!selCopy.visible) return null;
      return (
        <button
          type="button"
          data-selection-copy-button="true"
          title={selCopy.copied ? t.copied : t.copyMsg}
          onMouseDown={(e) => { e.preventDefault(); e.stopPropagation(); }}
          onClick={(e) => { e.preventDefault(); e.stopPropagation(); onCopy(); }}
          className={`absolute z-30 h-9 min-w-[92px] px-3 rounded-[10px] flex items-center justify-start gap-2 text-[13px] font-medium shadow-lg backdrop-blur transition-colors ${
            'bg-white text-[#1F1F1F] hover:bg-[#F8FAFF] border border-black/10 dark:bg-[#2B2C2F] dark:text-[#E3E3E3] dark:hover:bg-[#34363A] dark:border-white/10'
          }`}
          style={{ left: selCopy.x + 'px', top: selCopy.y + 'px' }}
        >
          {selCopy.copied ? <Check size={13} className="text-[#34C759]" /> : <Copy size={13} />}
          <span>{selCopy.copied ? t.copied : t.copyMsg}</span>
        </button>
      );
    };

    const TextareaContextMenu = ({ inputRef, setValue, theme, t }) => {
      const [menu, setMenu] = useState({ visible: false, x: 0, y: 0, canCopy: false });

      const closeMenu = useCallback(() => {
        setMenu(m => m.visible ? { ...m, visible: false } : m);
      }, []);

      const selectedText = useCallback(() => {
        const el = inputRef.current;
        if (!el) return '';
        const start = typeof el.selectionStart === 'number' ? el.selectionStart : 0;
        const end = typeof el.selectionEnd === 'number' ? el.selectionEnd : start;
        return start === end ? '' : String(el.value || '').slice(start, end);
      }, [inputRef]);

      const replaceSelection = useCallback((text) => {
        const el = inputRef.current;
        if (!el || !text) return;
        const raw = String(el.value || '');
        const start = typeof el.selectionStart === 'number' ? el.selectionStart : raw.length;
        const end = typeof el.selectionEnd === 'number' ? el.selectionEnd : start;
        const next = raw.slice(0, start) + text + raw.slice(end);
        const cursor = start + text.length;
        setValue(next);
        requestAnimationFrame(function () {
          el.focus();
          try { el.setSelectionRange(cursor, cursor); } catch (e) {}
        });
      }, [inputRef, setValue]);

      useEffect(() => {
        const el = inputRef.current;
        if (!el) return;
        const openMenu = (e) => {
          e.preventDefault();
          e.stopPropagation();
          const start = typeof el.selectionStart === 'number' ? el.selectionStart : 0;
          const end = typeof el.selectionEnd === 'number' ? el.selectionEnd : start;
          const menuW = 136;
          const menuH = 116;
          const x = Math.max(6, Math.min(e.clientX, window.innerWidth - menuW - 6));
          const y = Math.max(6, Math.min(e.clientY, window.innerHeight - menuH - 6));
          setMenu({ visible: true, x: x, y: y, canCopy: start !== end });
        };
        const onContextMenu = (e) => openMenu(e);
        const onMouseDown = (e) => { if (e.button === 2) openMenu(e); };
        el.addEventListener('contextmenu', onContextMenu, true);
        el.addEventListener('mousedown', onMouseDown, true);
        return () => {
          el.removeEventListener('contextmenu', onContextMenu, true);
          el.removeEventListener('mousedown', onMouseDown, true);
        };
      }, [inputRef]);

      useEffect(() => {
        if (!menu.visible) return;
        const onDown = (e) => {
          if (e.target && e.target.closest && e.target.closest('[data-textarea-context-menu]')) return;
          closeMenu();
        };
        const onKey = (e) => { if (e.key === 'Escape') closeMenu(); };
        const onScrollOrResize = () => closeMenu();
        document.addEventListener('mousedown', onDown, true);
        document.addEventListener('keydown', onKey, true);
        window.addEventListener('resize', onScrollOrResize);
        window.addEventListener('scroll', onScrollOrResize, true);
        return () => {
          document.removeEventListener('mousedown', onDown, true);
          document.removeEventListener('keydown', onKey, true);
          window.removeEventListener('resize', onScrollOrResize);
          window.removeEventListener('scroll', onScrollOrResize, true);
        };
      }, [closeMenu, menu.visible]);

      const menuItemCls = (disabled) => `w-full h-9 px-3 flex items-center gap-2 text-left text-[13px] transition-colors ${
        disabled
          ? 'text-black/30 cursor-not-allowed dark:text-white/30'
          : 'text-[#1F1F1F] hover:bg-black/[0.06] dark:text-[#E3E3E3] dark:hover:bg-white/10'
      }`;

      const selectAll = () => {
        const el = inputRef.current;
        if (!el) return;
        el.focus();
        el.select();
        closeMenu();
      };

      const copySelected = () => {
        const tx = selectedText();
        if (!tx) return;
        copyClipboardText(tx).then(function () { closeMenu(); });
      };

      const pasteText = () => {
        readClipboardText().then(function (tx) {
          replaceSelection(tx);
          closeMenu();
        });
      };

      if (!menu.visible) return null;
      return createPortal((
        <div
          data-textarea-context-menu="true"
          className={`w-[136px] overflow-hidden rounded-[12px] py-1 shadow-xl backdrop-blur border ${
            'bg-white border-black/10 dark:bg-[#2B2C2F] dark:border-white/10'
          }`}
          style={{ position: 'fixed', zIndex: 9999, left: menu.x + 'px', top: menu.y + 'px' }}
          onMouseDown={(e) => { e.preventDefault(); e.stopPropagation(); }}
        >
          <button type="button" className={menuItemCls(false)} onClick={selectAll}>
            <span className="w-4 text-center text-[12px]">A</span><span>{t.selectAllMsg}</span>
          </button>
          <button type="button" disabled={!menu.canCopy} className={menuItemCls(!menu.canCopy)} onClick={copySelected}>
            <Copy size={14} /><span>{t.copyMsg}</span>
          </button>
          <button type="button" className={menuItemCls(false)} onClick={pasteText}>
            <ClipboardList size={14} /><span>{t.pasteMsg}</span>
          </button>
        </div>
      ), document.body);
    };

    const UserBubble = ({ item, sessionId, theme, editable, t, conversationVariant }) => {
      const unified = conversationVariant === 'unified';
      const deliveryState = item.deliveryState || '';
      const sceneDisplay = pinvouSceneDisplay(item.pinvouScene, t.uiChat.sceneModes);
      const SceneIcon = sceneDisplay && sceneDisplay.Icon;
      const [editing, setEditing] = useState(false);
      const [val, setVal] = useState(item.text);
      const [copied, copyToClipboard] = useCopyFlash(1200);
      function commit() { const tx = val.trim(); setEditing(false); if (tx && bridge.available) bridge.interaction.editLastTurn(tx); }
      function copyText() {
        copyToClipboard('user-bubble', item.text || '');
      }
      function retryDelivery() {
        if (!item.clientMessageId || !bridge.available || !bridge.chat.retryFirstTurn) return;
        bridge.chat.retryFirstTurn(item.clientMessageId);
      }
      if (editing) {
        return (
          <div className="flex justify-end min-w-0 max-w-full">
            <div className="max-w-[85%] w-full min-w-0">
              <textarea autoFocus value={val} onChange={e => setVal(e.target.value)}
                rows={Math.min(6, Math.max(1, val.split('\n').length))}
                onKeyDown={e => { if (isPlainEnter(e)) { e.preventDefault(); commit(); } else if (e.key === 'Escape') { setEditing(false); setVal(item.text); } }}
                className={`w-full min-w-0 max-w-full break-words [overflow-wrap:anywhere] rounded-[16px] px-4 py-2 text-[15px] outline-none ${
                  unified
                    ? 'bg-[#E9EEF6] text-[#1F1F1F] dark:bg-[#2A2B2E] dark:text-[#E3E3E3]'
                    : 'bg-[#D3E3FD] text-[#1F1F1F] dark:bg-[#004A77] dark:text-[#E3E3E3]'
                }`} />
              <div className="flex gap-2 justify-end mt-1">
                <button className={cardBtnCls()} onClick={() => { setEditing(false); setVal(item.text); }}>{t.cpCancel}</button>
                <button className={cardBtnCls('primary')} onClick={commit}>{t.resend}</button>
              </div>
            </div>
          </div>
        );
      }
      if (item.pinvouTransfer) {
        const isWu = item.pinvouTransfer === '悟';
        const tintCls = isWu
          ? 'text-[#1967D2] dark:text-[#8AB4F8]'
          : 'text-[#7C3AED] dark:text-[#D0BCFF]';
        const tintBg = isWu
          ? 'bg-[#1A73E8]/[0.06] dark:bg-[#1A73E8]/10'
          : 'bg-[#7C3AED]/[0.07] dark:bg-[#D0BCFF]/10';
        return (
          <div className="flex justify-end min-w-0 max-w-full">
            <div className="max-w-[85%] min-w-0">
              <div className={`flex items-center justify-end gap-1 mb-1 text-[11px] font-medium ${tintCls}`}>
                <span>{isWu ? '✨' : '📋'}</span><span>{t.uiChatExtra.transferRevision(item.pinvouTransfer)}</span>
              </div>
              <div className={`min-w-0 max-w-full break-words [overflow-wrap:anywhere] px-5 py-3 rounded-[20px] text-[15px] leading-relaxed whitespace-pre-wrap ${tintBg} text-[#1F1F1F] dark:text-[#E3E3E3]`}>{item.text}</div>
            </div>
          </div>
        );
      }
      const actBtn = 'text-[#9AA0A6] hover:text-[#444746] hover:bg-black/[0.06] dark:text-[#8E8E8E] dark:hover:text-[#E3E3E3] dark:hover:bg-white/10';
      // 附件行拆出正文,附件以独立小气泡显示在正文气泡上方(纯附件消息只显示附件气泡)
      // An A RemoteProxy message is not a local attachment message. A B-authored
      // line that resembles the local attachment marker must stay plain text.
      const { text: bodyText, attachments: attachmentNames } = item.remoteProxy
        ? { text: item.text, attachments: [] }
        : splitAttachmentLine(item.text);
      return (
        <div className="flex justify-end group min-w-0 max-w-full">
          <div className="flex flex-col items-end max-w-[85%] min-w-0 max-w-full">
            {attachmentNames.length > 0 && (
              <div className={`flex max-w-full flex-wrap justify-end gap-1.5 ${bodyText ? 'mb-1.5' : ''}`}>
                {attachmentNames.map((name, index) => {
                  return (
                    <ConversationAttachmentBubble
                      key={`${name}-${index}`}
                      name={name}
                      displayText={item.text}
                      messageIndex={item.messageIndex}
                      attachmentIndex={index}
                      sessionId={sessionId}
                      copyText={copyClipboardText}
                      labels={{
                        open: t.attachmentOpen,
                        download: t.attachmentDownload,
                        copyAddress: t.attachmentCopyAddress,
                        copyName: t.attachmentCopyName,
                        reveal: t.attachmentReveal,
                      }}
                    />
                  );
                })}
              </div>
            )}
            {bodyText && <div className={`min-w-0 max-w-full break-words [overflow-wrap:anywhere] px-4 py-3 rounded-[20px] rounded-br-md text-[14px] leading-6 whitespace-pre-wrap ${
              unified
                ? 'bg-[#E9EEF6] text-[#1F1F1F] dark:bg-[#2A2B2E] dark:text-[#E3E3E3]'
                : 'bg-[#D3E3FD] text-[#1F1F1F] dark:bg-[#004A77] dark:text-[#E3E3E3]'
            }`}>
              {sceneDisplay && (
                <span
                  data-testid="user-message-scene-tag"
                  className={`mr-2 inline-flex align-middle items-center gap-1 rounded-full px-2 py-0.5 text-[12px] font-semibold leading-5 ${
                    'bg-black text-white dark:bg-black/35'
                  }`}
                >
                  {SceneIcon && <SceneIcon size={14} className="shrink-0" />}
                  <span>{sceneDisplay.label}</span>
                </span>
              )}
              {bodyText}
            </div>}
            {item.remoteProxy && <span className="sr-only">{t.remoteConversation.projectionUser}</span>}
            {deliveryState && (
              <div data-testid={`message-delivery-${deliveryState}`} title={item.deliveryError || undefined} className={`mt-1 flex items-center gap-1.5 pr-1 text-[11px] ${
                deliveryState === 'failed' || deliveryState === 'unknown'
                  ? 'text-[#C5221F] dark:text-[#F28B82]'
                  : deliveryState === 'accepted'
                    ? 'text-[#188038] dark:text-[#81C995]'
                    : 'text-[#747775] dark:text-[#9AA0A6]'
              }`}>
                {deliveryState === 'sending' && <span className="h-1.5 w-1.5 rounded-full bg-current animate-pulse" />}
                <span>
                  {deliveryState === 'sending'
                    ? t.messageSending
                    : deliveryState === 'accepted'
                      ? t.messageAccepted
                      : deliveryState === 'unknown'
                        ? t.messageOutcomeUnknown
                        : t.messageFailed}
                </span>
                {deliveryState === 'failed' && (
                  <button type="button" onClick={retryDelivery} className="font-medium underline underline-offset-2">
                    {t.resend}
                  </button>
                )}
              </div>
            )}
            {/* iOS 风操作条：hover 气泡时下方浮现；窄屏无 hover，常显保证触屏可达。复制=所有 query；编辑重发=仅最新(editable)。 */}
            <div className="flex items-center gap-0.5 mt-1 pr-1 opacity-0 group-hover:opacity-100 max-sm:opacity-100 transition-opacity duration-150">
              <button title={copied ? t.copied : t.copyMsg} onClick={copyText}
                className={`w-7 h-7 rounded-lg flex items-center justify-center transition-colors ${actBtn}`}>
                {copied ? <Check size={14} className="text-[#34C759]" /> : <Copy size={14} />}
              </button>
              {editable && !deliveryState && (
                <button title={t.editResend} onClick={() => { setVal(item.text); setEditing(true); }}
                  className={`w-7 h-7 rounded-lg flex items-center justify-center transition-colors ${actBtn}`}>
                  <Edit2 size={14} />
                </button>
              )}
            </div>
          </div>
        </div>
      );
    };

    // ③ 卡牌制造专家: 从助手消息渲染后的 html 里抠出 ```persona-card 草稿块 → 解析成卡。
    function htmlUnescape(s) {
      return String(s).replace(/&lt;/g,'<').replace(/&gt;/g,'>').replace(/&quot;/g,'"').replace(/&#(?:39|x27);/gi,"'").replace(/&amp;/g,'&');
    }
    function highlightedCodeText(s) {
      return htmlUnescape(String(s).replace(/<\/?span\b[^>]*>/gi, ''));
    }
    function asDraft(d) {
      if (!d || typeof d !== 'object' || !d.name || !d.body) return null;
      var dept = (d.dept && DEPT_ORDER.indexOf(d.dept) >= 0) ? d.dept : 'specialized';
      return { name: d.name, dept: dept, emoji: d.emoji || '🃏', color: d.color || '', description: d.description || '', body: d.body };
    }
    function asScheduledTaskDraft(d) {
      if (!d || typeof d !== 'object' || !d.name || !d.prompt || !d.rrule) return null;
      return {
        name: String(d.name),
        prompt: String(d.prompt),
        rrule: String(d.rrule),
        mode: 'yolo',
        paused: !!d.paused,
      };
    }
    // 扫所有 ```代码块,任何能解析成「含 name+body 的 JSON」的就当卡牌草稿。
    // 不强求 ```persona-card 标签 —— 小模型常打 ```json 或不打标签,放宽识别更鲁棒。
    // 形状校验(name+body)避免把别的 JSON 误判成草稿。明确 persona-card 标签的优先。
    // 返回 { draft, html }:html 是把那段原始 JSON 块抹掉后的版本(用户只看友好草稿卡,不看机器载荷)。
    // Shared <pre><code> scan skeleton for the three protocol blocks (persona-card /
    // scheduled-task-draft / card-question): per block, take the code text and validate it
    // via validate(parseLooseJson(raw)); return { payload, html } with html stripped of the
    // chosen raw block. Priority: the first explicitly tagged block (tagPattern hit on the
    // <pre>/<code> attributes) wins immediately; otherwise fall back to the first parseable
    // block. With taggedOnly, only tagged blocks count — no first-block fallback (card-question's
    // existing semantics). A validate throw (non-JSON block) is treated as a skip: parseLooseJson
    // itself never throws; this catches unexpected exceptions from custom validate, matching the
    // old persona-side try/catch.
    function scanProtocolCodeBlocks(html, { tagPattern, validate, taggedOnly = false }) {
      const miss = { payload: null, html };
      if (!html) return miss;
      if (taggedOnly ? !tagPattern.test(html) : !html.includes('{')) return miss;
      const re = /<pre([^>]*)>\s*<code([^>]*)>([\s\S]*?)<\/code>\s*<\/pre>/g;
      let m; let chosen = null; let chosenPayload = null;
      // biome-ignore lint/suspicious/noAssignInExpressions: assignment doubles as the loop condition; refactoring hurts readability
      while ((m = re.exec(html))) {
        const tagged = tagPattern.test(m[1] + m[2]);
        if (taggedOnly && !tagged) continue;
        const raw = highlightedCodeText(m[3]).trim();
        if (raw.charAt(0) !== '{') continue;
        let payload = null;
        try {
          payload = validate(parseLooseJson(raw));
        } catch { /* 非 JSON 块,跳过 */ }
        if (!payload) continue;
        if (tagged) { chosen = m[0]; chosenPayload = payload; break; } // tagged block wins
        if (!chosenPayload) { chosen = m[0]; chosenPayload = payload; }
      }
      if (!chosenPayload) return miss;
      return { payload: chosenPayload, html: html.replace(chosen, '') };
    }
    function parsePersonaDraft(html) {
      const { payload, html: rest } = scanProtocolCodeBlocks(html, {
        tagPattern: /persona-card/i,
        validate: asDraft,
      });
      return { draft: payload, html: rest };
    }
    function parseScheduledTaskDraft(html) {
      const { payload, html: rest } = scanProtocolCodeBlocks(html, {
        tagPattern: /scheduled-task-draft/i,
        validate: asScheduledTaskDraft,
      });
      return { draft: payload, html: rest };
    }
    // 卡牌制造专家追问时,若问题有可选项,会输出一个 ```card-question 块 {question, options[]}。
    // 抠出来 → 渲染成可点击的 iOS 选项卡;点选项即把它作为回答发送。返回 { q, html(抹掉块) }。
    function parseCardQuestion(html) {
      const { payload, html: rest } = scanProtocolCodeBlocks(html, {
        tagPattern: /card-question/i,
        taggedOnly: true,
        validate: (d) => {
          if (!d || !d.question || !Array.isArray(d.options)) return null;
          const opts = d.options.filter(function (o) { return typeof o === 'string' && o.trim(); });
          return opts.length ? { question: String(d.question), options: opts } : null;
        },
      });
      return { q: payload, html: rest };
    }
    // 点选项时实际发送的回答:取"短标签 —— 说明"里的短标签;没分隔符就发整句。
    function optionAnswer(opt) {
      var s = String(opt).split(/\s*(?:——|—|::|:|：|\(|（)/)[0].trim();
      return s || String(opt).trim();
    }
    // 流式中: JSON 还没闭合无法解析,把正在生成的卡牌/选项代码块折叠成占位,避免原始 JSON 一直刷屏。
    function hideStreamingDraft(html, label) {
      if (!html) return html;
      var m = /<pre[^>]*>\s*<code[^>]*(?:persona-card|card-question|scheduled-task-draft)[\s\S]*$/i.exec(html); // persona-card / card-question / scheduled-task-draft 标签块(到末尾)
      if (!m) m = /<pre[^>]*>\s*<code[^>]*>\s*\{[\s\S]*?(?:name|&quot;name|rrule|&quot;rrule)[\s\S]*$/i.exec(html); // 兜底: 以 { 开头且含 name / rrule 的块
      if (!m) return html;
      return html.slice(0, m.index) + '<div style="margin-top:.5em;opacity:.7;font-size:13px">' + (label || '…') + '</div>';
    }

    // eslint-disable-next-line sonarjs/cognitive-complexity -- legacy bubble dispatches rendering by message type; split refactor tracked separately
    const ChatBubble = React.memo(function ChatBubble({ item, sessionId, theme, onPrefill, onSend, editable, onOpenEditor, t, isLatestArtifact, allowScheduledTaskDraft, conversationVariant, showAssistantActions = true, onPlanStuckGo }) {
      const chatCopy = t.uiChat;
      // 后端持久化的记忆状态值是固定中文数据，仅在 UI 边界映射为当前语言；未识别值原样透传
      const memoryStatusLabels = getMemoryStatusLabels(t);
      const localizedMemoryStatus = (label) => memoryStatusLabels[label] || label;
      const assistantSelectionHostRef = useRef(null);
      const assistantSelectionTargetRef = useRef(null);
      // 懒加载语言注册完成会 bump 版本号:legacy assistant 气泡由 item.text 现算
      // markdown(见下),订阅版本号让注册后本组件重渲染,历史消息恢复高亮。
      const syntaxVersion = useSyncExternalStore(subscribeSyntaxHighlight, getSyntaxHighlightVersion);
      void syntaxVersion;

      if (item.remoteProxy && item.type === 'system') {
        const copy = t.remoteConversation;
        const label = copy[remoteProjectionLabelKey(item.remoteKind)] || copy.projectionEvent;
        const systemBody = remoteProxySystemBody(item);
        return (
          <div className="flex justify-center">
            <div data-testid="chat-remote-projection-entry" data-kind={item.remoteKind}
              role="status"
              aria-label={label}
              className="max-w-[85%] min-w-0 rounded-2xl border border-blue-500/20 bg-blue-500/[0.06] px-4 py-3 text-[14px] leading-6 text-[#1F1F1F] forced-colors:border-[CanvasText] dark:text-[#E3E3E3]">
              <div className="text-[11px] font-semibold">{label}</div>
              {systemBody && (
                <div className="mt-1 whitespace-pre-wrap break-words text-[13px]" data-testid="chat-remote-projection-body">
                  {systemBody}
                </div>
              )}
              {item.remoteAttachments?.length > 0 && (
                <div className="mt-1 text-[12px]">{copy.projectionAttachmentCount(item.remoteAttachments.length)}</div>
              )}
            </div>
          </div>
        );
      }

      if (item.type === 'artifact_card') return <ArtifactCard item={item} theme={theme} t={t} isLatest={isLatestArtifact} />;
      if (item.type === 'plan_card') return <PlanCard item={item} t={t} onPrefill={onPrefill} />;
      if (item.type === 'plan_stuck') return <PlanStuckCard item={item} t={t} onGo={onPlanStuckGo} />;
      if (item.type === 'careful_blocked') return <CarefulBlockedCard item={item} t={t} />;
      if (item.type === 'user_input') return <UserInputCard item={item} t={t} />;
      if (item.type === 'user') {
        return <UserBubble item={item} sessionId={sessionId} theme={theme} editable={editable} t={t} conversationVariant={conversationVariant} />;
      }

      if (item.type === 'card_creator_intro') {
        return (
          <div className="flex justify-start" style={{ fontFamily:'-apple-system, BlinkMacSystemFont, "SF Pro Text", "PingFang SC", "Microsoft YaHei", sans-serif' }}>
            <div className="rounded-[14px] px-4 py-3 max-w-[440px] text-[15px] font-medium" style={{ background: theme === 'dark' ? '#1C1C1E' : '#F2F2F7', color: theme === 'dark' ? '#fff' : '#000' }}>{t.cpIntroTitle}</div>
          </div>
        );
      }

      if (item.type === 'assistant') {
        if (item.streaming && !item.html) return null; // 空流式气泡交给 ThinkingBubble 表示
        // 旧会话(legacy)的 assistant 气泡由 item.text 现算 markdown:懒语言注册后
        // (ChatBubble 顶部订阅 syntaxVersion 触发重渲染)历史消息恢复高亮——若
        // 沿用冻结的 item.html,首次渲染时未注册的语言将永久纯文本。现算结果按
        // item 缓存(见 renderLegacyMarkdownCached),重渲染零解析成本。流式消息仍
        // 走 item.html(增量渲染管线);仅存 html 无 text 的旧消息无法现算,保持
        // 原样(其语言在启动核心集内,不受懒注册影响)。
        const html = (!item.streaming && item.text)
          ? renderLegacyMarkdownCached(item, syntaxVersion)
          : (item.html || '');
        const streamingDraftLabel = /scheduled-task-draft/.test(html) ? t.uiChatExtra.draftingScheduled : (t && t.cpDesigning);
        // The three-pass parse chain is a pure function cached per item (see
        // parseAssistantBubblesCached): even when the memo is defeated
        // (streaming delta / syntaxVersion bump) only bubbles that actually
        // changed reparse.
        // Remote content keeps the normal Markdown/code renderer, but must not
        // manufacture local persona/question actions from a B-authored message.
        const { pd, cq } = item.remoteProxy
          ? { pd: { draft: null }, cq: { q: null, html } }
          : parseAssistantBubblesCached(item, html, !!item.streaming, allowScheduledTaskDraft, streamingDraftLabel);
        const assistantCopyAvailable = !item.streaming
          && [item.text, item.html].some(value => String(value || '').trim());
        // 草稿是否已存入(按名字在已加载的卡池里找同名自制卡 → 派生"已存入",免单独持久化)
        const draftSaved = pd.draft && bridge.available && bridge.personas.getPersonas
          && bridge.personas.getPersonas().some(function(c){ return c && c.source === 'user' && c.name === pd.draft.name; });
        return (
          <div className="flex justify-start">
            <div ref={assistantSelectionHostRef} className={`relative ${cq.q ? 'w-full' : 'max-w-[95%]'} light-code dark-code`}>
              <div
                ref={assistantSelectionTargetRef}
                className={`msg-md text-[15px] leading-relaxed ${item.streaming ? 'streaming-cursor' : ''} ${'text-[#1F1F1F] dark:text-[#E3E3E3]'}`}
                onClick={(e) => {
                  // 聊天里的链接(如飞书授权 URL)点击 → 走系统浏览器,别导航主窗口/不可点。
                  const a = e.target && e.target.closest && e.target.closest('a[href]');
                  if (!a) return;
                  const href = a.getAttribute('href') || '';
                  if (/^https?:\/\//i.test(href)) {
                    e.preventDefault();
                    invokeTauri('open_user_external_url', { url: href }).catch(() => {});
                  }
                }}
                dangerouslySetInnerHTML={{ __html: cq.html || '' }}
              />
              {item.remoteProxy && <span className="sr-only">{t.remoteConversation.projectionAssistant}</span>}
              <SelectionCopyButton hostRef={assistantSelectionHostRef} targetRef={assistantSelectionTargetRef} theme={theme} t={t} />
              {cq.q ? (
                <div className="mt-2 w-full" style={{ fontFamily:'-apple-system, BlinkMacSystemFont, "SF Pro Text", "PingFang SC", "Microsoft YaHei", sans-serif' }}>
                  <div className="text-[14px] font-medium mb-2" style={{ color: theme === 'dark' ? '#fff' : '#000' }}>{cq.q.question}</div>
                  <div className="rounded-[14px] overflow-hidden" style={{ background: theme === 'dark' ? '#1C1C1E' : '#fff', border: theme === 'dark' ? 'none' : '0.5px solid rgba(60,60,67,.12)' }}>
                    {cq.q.options.map((opt, i) => (
                      <button key={i} onClick={()=> onSend && onSend(optionAnswer(opt))}
                        className="w-full flex items-center gap-3 px-4 py-3 text-left transition-opacity active:opacity-60 hover:opacity-90"
                        style={i ? { borderTop: '0.5px solid ' + (theme === 'dark' ? 'rgba(84,84,88,.45)' : 'rgba(60,60,67,.12)') } : undefined}>
                        <span className="text-[15px] shrink-0 text-right" style={{ color: '#8E8E93', width: 15, fontVariantNumeric: 'tabular-nums' }}>{i + 1}</span>
                        <span className="text-[15px] flex-1 min-w-0" style={{ color: theme === 'dark' ? '#fff' : '#000' }}>{opt}</span>
                        <ChevronRight size={16} className="shrink-0" style={{ color: '#C7C7CC' }} />
                      </button>
                    ))}
                  </div>
                </div>
              ) : null}
              {pd.draft ? (
                <div className="mt-2 rounded-[14px] p-3 flex items-center gap-3 max-w-[460px]" style={{ background: theme === 'dark' ? '#1C1C1E' : '#F2F2F7' }}>
                  <AppIcon card={pd.draft} cls="w-11 h-11 rounded-[12px]" fb={22} />
                  <div className="min-w-0 flex-1">
                    <div className="text-[15px] font-semibold leading-snug truncate" style={{ color: theme === 'dark' ? '#fff' : '#000' }}>{pd.draft.name}</div>
                    <div className="text-[13px] truncate" style={{ color: theme === 'dark' ? 'rgba(235,235,245,.6)' : 'rgba(60,60,67,.6)' }}>{pd.draft.description || deptLabelFor(t, pd.draft.dept)}</div>
                  </div>
                  {draftSaved
                    ? <span className="shrink-0 inline-flex items-center gap-1 h-8 px-1 text-[13px] font-medium" style={{ color:'#8E8E93' }} title={t.cpDraftSavedTitle}><Check size={15} strokeWidth={2.5} style={{ color:'#34C759' }} />{t.cpDraftSaved}</span>
                    : <button onClick={()=> onOpenEditor && onOpenEditor(pd.draft)} className="shrink-0 px-4 h-8 rounded-full text-[13px] font-semibold text-white" style={{ background: theme === 'dark' ? '#0A84FF' : '#007AFF' }} title={t.cpDraftViewTitle}>{t.cpDraftView}</button>}
                </div>
              ) : null}
              {showAssistantActions && assistantCopyAvailable && <AssistantMessageFooter>
                <AssistantMessageActions
                  resolveText={() => assistantItemCopyText(item, { allowScheduledTaskDraft })}
                  copy={t.uiConversation}
                />
                {item.time && <span className={`text-[11px] ${'text-[#757575] dark:text-[#8E8E8E]'}`}>{item.time}</span>}
              </AssistantMessageFooter>}
            </div>
          </div>
        );
      }

      if (item.type === 'tool') {
        return <ToolCard item={item} sessionId={sessionId} t={t} />;
      }

      if (item.type === 'persona_equip') {
        const c = item.card || {};
        const tc = (typeof deptColor !== 'undefined' ? deptColor(c.dept) : '#ffae1e');
        const deptLabel = deptLabelFor(t, c.dept);
        const cd = personaText(c, t);
        return (
          <div className="flex flex-col gap-1.5" style={{ fontFamily:'-apple-system, BlinkMacSystemFont, "SF Pro Text", "PingFang SC", "Microsoft YaHei", sans-serif' }}>
            <div className="text-[12px] font-medium" style={{ color: '#8E8E93' }}>{t.cpEquipBubbleSys}</div>
            <div className="rounded-[14px] p-4 max-w-[560px]" style={{ background: theme === 'dark' ? '#1C1C1E' : '#F2F2F7' }}>
              <div className="flex items-center gap-3 mb-3">
                <AppIcon card={c} cls="w-11 h-11 rounded-[12px]" fb={22} />
                <div className="text-[15px] font-semibold leading-snug" style={{ color: theme === 'dark' ? '#fff' : '#000' }}>{t.cpEquipBubbleTitle(cd.name)}</div>
              </div>
              <div className="text-[13px] space-y-1" style={{ color: theme === 'dark' ? '#C7C7CC' : '#3C3C43' }}>
                <div>{t.cpDept}: <span style={{ color: theme === 'dark' ? '#0A84FF' : '#007AFF', fontWeight: 600 }}>{deptLabel}</span></div>
                <div>{t.cpDescLabel}: {cd.description}</div>
              </div>
              <div className="text-[12px] mt-2.5" style={{ color: '#8E8E93' }}>{t.cpEquipBubbleNote}</div>
            </div>
          </div>
        );
      }
      if (item.type === 'system') {
        return (
          <div className="flex justify-center">
            <div className={`text-[13px] px-4 py-1.5 rounded-full ${'bg-[#F0F4F9] text-[#757575] dark:bg-[#1E1F20] dark:text-[#8E8E8E]'}`}>
              {item.text}
            </div>
          </div>
        );
      }
      if (item.type === 'memory_notice') {
        const text = item.text || '';
        const quietNotice = item.kind === 'recent_activity' || item.kind === 'recent_work';
        const memoryKind = item.kind === 'recent_work' ? 'recent_activity' : item.kind;
        const meta = chatCopy.memoryMeta[memoryKind] || chatCopy.memoryMeta.preference;
        if (quietNotice) {
          return (
            <div className="flex justify-center">
              <div
                className="inline-flex items-center gap-1.5 max-w-[360px] px-3 py-1.5 rounded-full text-[12px] text-[#AEB4BC]"
                title={text}
                style={{ background: 'rgba(32, 34, 38, 0.54)', border: '1px solid rgba(255,255,255,0.06)' }}
              >
                <Check size={12} className="shrink-0 text-[#30D158]" />
                <span className="font-medium text-[#D5D9DE]">{chatCopy.recordedRecent}</span>
                <span className="truncate">{chatCopy.viewMemory}</span>
              </div>
            </div>
          );
        }
        return (
          <div className="flex justify-end">
            <div
              className="max-w-[420px] w-full rounded-[16px] px-4 py-3 text-[#F2F3F5]"
              style={{
                background: 'rgba(32, 34, 38, 0.86)',
                border: '1px solid rgba(255,255,255,0.08)',
                boxShadow: '0 14px 36px rgba(0,0,0,0.34)',
                backdropFilter: 'blur(16px)',
                WebkitBackdropFilter: 'blur(16px)',
              }}
            >
              <div className="flex items-center gap-2 min-w-0">
                <span className="w-7 h-7 rounded-full flex items-center justify-center shrink-0 bg-[#34C759]/[0.15] text-[#30D158]">
                  <Check size={15} />
                </span>
                <div className="min-w-0">
                  <div className="flex items-center gap-2 min-w-0">
                    <span className="text-[13px] font-semibold leading-tight">{localizedMemoryStatus(item.statusLabel) || chatCopy.memoryUpdated}</span>
                    <span className="text-[11px] px-2 py-0.5 rounded-full bg-white/[0.07] text-[#AEB4BC]">{meta.label}</span>
                  </div>
                  <div className="mt-1 text-[12px] leading-relaxed text-[#AEB4BC]">{meta.notice}</div>
                </div>
              </div>
              {text && (
                <div className="mt-3 ml-9 border-l-2 border-[#0A84FF]/70 pl-3 py-1 text-[13px] leading-relaxed break-words text-[#E8EAED]">
                  “{text}”
                </div>
              )}
            </div>
          </div>
        );
      }
      if (item.type === 'memory_candidate') {
        const resolved = !!item.resolved;
        if (resolved && (item.statusLabel === '已忽略' || item.statusLabel === '不再提示')) return null;
        const text = item.text || '';
        const memoryKind = item.kind === 'recent_work' ? 'recent_activity' : item.kind;
        const meta = chatCopy.memoryMeta[memoryKind] || chatCopy.memoryMeta.preference;
        const localizedStatus = localizedMemoryStatus(item.statusLabel);
        return (
          <div className="flex justify-end">
            <div
              data-testid="memory-candidate-card"
              data-memory-id={item.memoryId || ''}
              className={`max-w-[480px] w-full rounded-[18px] px-4 py-3.5 ${'text-[#F8FAFC] dark:text-[#F2F3F5]'}`}
              style={{
                background: 'rgba(32, 34, 38, 0.92)',
                border: '1px solid rgba(255,255,255,0.08)',
                boxShadow: '0 18px 50px rgba(0,0,0,0.45)',
                backdropFilter: 'blur(18px)',
                WebkitBackdropFilter: 'blur(18px)',
              }}
            >
              <div className="flex items-start justify-between gap-3">
                <div className="flex items-center gap-2 min-w-0">
                  <span className={`w-7 h-7 rounded-full flex items-center justify-center shrink-0 ${resolved ? 'bg-[#34C759]/[0.15] text-[#30D158]' : 'bg-[#0A84FF]/[0.16] text-[#7DBDFF]'}`}>
                    {resolved ? <Check size={15} /> : <Brain size={15} />}
                  </span>
                  <div className="min-w-0">
                    <div className="text-[13px] font-semibold leading-tight">{resolved ? (localizedStatus || chatCopy.processed) : chatCopy.candidate}</div>
                    {!resolved && <div className="text-[12px] leading-tight mt-1 text-[#AEB4BC]">{meta.prompt}</div>}
                  </div>
                </div>
                <div className="shrink-0 flex items-center gap-2">
                  <span className="text-[11px] px-2 py-1 rounded-full bg-white/[0.07] text-[#AEB4BC]">{meta.label}</span>
                  {!resolved && (
                    <button
                      className="w-6 h-6 rounded-full flex items-center justify-center text-[#8E8E93] hover:text-[#F2F3F5] hover:bg-white/[0.08] transition-colors"
                      title={chatCopy.ignoreOnce}
                      data-testid="memory-candidate-dismiss"
                      onClick={() => bridge.available && bridge.memory.ignoreMemoryCandidate && bridge.memory.ignoreMemoryCandidate(item.memoryId, item.id)}
                    >
                      <X size={13} />
                    </button>
                  )}
                </div>
              </div>
              <div className="mt-3 ml-9 border-l-2 border-[#0A84FF]/70 pl-3 py-1 text-[14px] leading-relaxed break-words text-[#F2F3F5]">
                “{text}”
              </div>
              {!resolved && <div className="mt-2 ml-9 text-[12px] leading-relaxed text-[#AEB4BC]">{meta.hint}</div>}
              {!resolved && (
                <div className="mt-3 ml-9 flex flex-wrap items-center gap-2">
                  <button data-testid="memory-candidate-confirm" className="inline-flex items-center gap-1.5 text-[13px] font-medium px-3.5 py-1.5 rounded-full bg-[#0A84FF] text-white hover:bg-[#1677D2] transition-colors" onClick={() => bridge.available && bridge.memory.confirmMemoryCandidate && bridge.memory.confirmMemoryCandidate(item.memoryId, item.id)}><Check size={14} />{chatCopy.remember}</button>
                  <button data-testid="memory-candidate-ignore" className="inline-flex items-center gap-1.5 text-[13px] px-3.5 py-1.5 rounded-full bg-white/[0.08] text-[#E8EAED] hover:bg-white/[0.12] transition-colors" onClick={() => bridge.available && bridge.memory.ignoreMemoryCandidate && bridge.memory.ignoreMemoryCandidate(item.memoryId, item.id)}><X size={14} />{chatCopy.ignoreOnce}</button>
                  <button data-testid="memory-candidate-never" className="text-[13px] px-2 py-1.5 rounded-full text-[#AEB4BC] hover:text-[#F2F3F5] hover:bg-white/[0.08] transition-colors" onClick={() => bridge.available && bridge.memory.neverMemoryCandidate && bridge.memory.neverMemoryCandidate(item.memoryId, item.id)}>{chatCopy.neverAsk}</button>
                </div>
              )}
            </div>
          </div>
        );
      }

      return null;
    });
    // ChatBubble memoization: inputText lives at the ChatView top level, so
    // every keystroke re-renders the whole view; the legacy bubble list is
    // O(n), and after memoization unchanged bubbles only pay a shallow prop
    // compare. Callbacks at call sites are stable references
    // (setInputText/sendChatMessage/onOpenEditor) and items keep stable
    // references from the bridge session data; the syntaxVersion subscription
    // lives inside the component, so the memo cannot block the re-render
    // after lazy language registration.

    // ==========================================
    // Artifact Card — present_artifact 成品卡（点击打开预览）
    // ==========================================
    // 产物类型 → { 角标/标签文字, tile 配色, lucide 内联 SVG 路径 }（零下载；仅无封面紧凑态显图标）。
    // 配色/字形照搬 产物卡图标预览.html（唯一权威）。

export { ChatView };
