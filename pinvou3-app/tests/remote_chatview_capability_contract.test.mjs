import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { dict } from './helpers/i18n-all.js';
import { remoteProjectionLabelKey } from '../src/features/remote-control/remote-session-adapter.mjs';

const chat = readFileSync(new URL('../src/features/chat/ChatView.jsx', import.meta.url), 'utf8');
const app = readFileSync(new URL('../src/app/main.jsx', import.meta.url), 'utf8');

test('remote status capsule reuses the translated model-source label in every language', () => {
  assert.doesNotMatch(chat, /t\.controllerConnection\.modelSourceDevice/);
  const capsule = chat.slice(chat.indexOf('data-testid="chat-remote-status"'),
    chat.indexOf('data-testid="chat-remote-model-source"'));
  assert.match(capsule, /t\.remoteConversation\.modelSourceDevice/);
  for (const language of ['zh', 'en', 'ja']) {
    assert.equal(typeof dict[language].remoteConversation.modelSourceDevice, 'string');
    assert.ok(dict[language].remoteConversation.modelSourceDevice.trim());
  }
});

function sourceSection(source, startMarker, endMarker) {
  const start = source.indexOf(startMarker);
  assert.notEqual(start, -1, `missing source marker: ${startMarker}`);
  const end = source.indexOf(endMarker, start + startMarker.length);
  assert.notEqual(end, -1, `missing source marker: ${endMarker}`);
  return source.slice(start, end);
}

test('RemoteProxy hides local controls and exposes only verified remote capability actions', () => {
  assert.match(chat, /enabled=\{!remoteChatActive && bridge\.available/);
  assert.match(chat, /if \(isWeb \|\| remoteTranscriptActive\) return;/);
  assert.match(chat, /\{!remoteChatActive && <AttachmentChips/);
  assert.match(chat, /\{!remoteTranscriptActive && <>[\s\S]*?<ComposerAttachButton[\s\S]*?<ComposerModelSelector[\s\S]*?<ComposerToolMenu/);
  assert.match(chat, /activeSessionId && !remoteTranscriptActive && \([\s\S]*?<ChatRightDockSwitcher/);
  assert.match(chat, /artifactsVisible = Boolean\(activeSessionId && artifactsOpen && !remoteTranscriptActive\)/);
  assert.match(chat, /\{!remoteTranscriptActive && \(queued\.length > 0 \|\| queuedActionError\)/);
  assert.equal((chat.match(/data-testid="chat-remote-model-source"/g) || []).length, 1);
  assert.match(chat, /scopedRemoteConnection = remoteChatActive && remoteConnection\?\.localSessionId === activeSessionId/);
  assert.match(chat, /remoteStatusLabel = remotePresentation === 'ready'/);
  assert.match(chat, /remotePresentation === 'offline' \? t\.remoteConversation\.offlineHint/);
  assert.match(chat, /reserveRemoteTask\([\s\S]*?uploadRemoteFiles\([\s\S]*?commitRemoteTask\(/);
  assert.match(chat, /cancelRemoteTask\(/);
  assert.match(chat, /permissionDecide\([\s\S]*?'allow'/);
  assert.match(chat, /permissionDecide\([\s\S]*?'deny'/);
  assert.match(chat, /artifactIndex\([\s\S]*?artifactPreview\([\s\S]*?downloadRemoteArtifact\(/);
  assert.match(chat, /readOnly=\{remoteChatActive && !remoteLinkReady\}/);
});

test('ChatView consumes the authenticated mode, workspace, tool and skill snapshot', () => {
  assert.match(chat, /desktopRemoteControl\.capabilitiesSnapshot\(scopedRemoteConnection\.sessionRef\)/);
  assert.match(chat, /data-testid="chat-remote-mode"/);
  assert.match(chat, /remoteModes\.includes\('chat'\)/);
  assert.match(chat, /remoteModes\.includes\('code'\)/);
  assert.match(chat, /data-testid="chat-remote-workspace"/);
  assert.match(chat, /workspace\?\.codeAllowed === true/);
  assert.match(chat, /data-testid="chat-remote-capabilities"/);
  assert.match(chat, /remoteSkillIds\.includes\(skill\.skillId\)/);
  assert.match(chat, /skill\.scopes\.includes\(remoteMode\)/,
    'A must expose only skills authorized for the selected lane');
  assert.match(chat, /skill\.scopes\.includes\(controlledCapabilities\.sessionLane\)/,
    'B must expose only skills authorized for its trusted lane');
  assert.match(chat, /remoteTools\.map\(tool => tool\.displayName \|\| tool\.toolId\)/);
  assert.match(chat, /newRemoteTaskIntent\([\s\S]*?remoteMode,[\s\S]*?remoteWorkspaceId[\s\S]*?remoteSkillIds/);

  const modelSelection = sourceSection(
    chat, 'const remoteModelBindingKey =', 'const remoteModeReady =',
  );
  assert.match(modelSelection,
    /remoteProxyModelSelectionIdentity\(activeSessionId, scopedRemoteConnection\)/,
    'model selection must use the stable connection/lease identity');
  assert.match(modelSelection, /selectedExportableRemoteModelId\([\s\S]*?bs\?\.activeModelId/,
    'A remote model must default to the model selected in settings');

  const dispatch = sourceSection(
    chat, 'const dispatchChatMessage = useCallback(', '// SessionStore mapping/lease establishment',
  );
  const remoteDispatch = sourceSection(
    dispatch, 'if (remoteChatActive) {', 'const matchedPersonalWorkbenchDraft',
  );
  assert.match(remoteDispatch,
    /if \(remoteModelError\) \{\s*setRemoteSendError\(\{ identity: sendIdentity, code: remoteModelError \}\);\s*return false;/,
    'an empty controller model must fail at the shared send boundary with a visible error');

  const sendMessage = sourceSection(
    chat, 'const remoteSendMessage =', 'const sendChatMessage = useCallback(',
  );
  assert.match(sendMessage,
    /currentRemoteSendError === 'remote_model_required'[\s\S]*?t\.remoteConversation\.modelRequired/,
    'the send-time missing-model error must render its dedicated prompt');
});

test('App initializes and passes the controller configured model source', () => {
  const initialization = sourceSection(
    app, 'const [remoteModelSource, setRemoteModelSource]',
    'const controlledDeviceAutoOpenRef',
  );
  assert.match(initialization,
    /useState\('controller_temporary'\)/,
    'App must initialize remote chat from the controller configured model source');

  const chatRender = sourceSection(
    app, "{currentView === 'chat' && (", "{codexAcpSupported && currentView === 'codex'",
  );
  assert.match(chatRender, /remoteModelSource=\{remoteModelSource\}/,
    'App must pass its initialized model source into the primary ChatView');

  const chatViewDeclaration = sourceSection(chat, 'const ChatView = (', ') => {');
  assert.match(chatViewDeclaration, /remoteModelSource = 'controller_temporary'/,
    'detached ChatViews must use the same default when App does not pass the prop');
});

test('capability renewal preserves draft state while dispatch stays revision-bound', () => {
  assert.match(chat, /remoteProxyComposerIdentity\(activeSessionId, remoteConnection\)/);
  assert.match(chat, /sameRemoteComposerOwner\(\s*sentSessionId, sentRemoteIdentity, activeSessionIdRef\.current, remoteComposerIdentityRef\.current/);
  const resetting = sourceSection(chat, 'setRemoteFiles([]);', 'if (!remoteArtifactPreview)');
  assert.match(resetting, /\[remoteComposerIdentity, remoteTransferGuard\]/);
  assert.doesNotMatch(resetting, /\[remoteSendIdentity\]/);
  assert.match(chat, /item\.identity !== remoteSendIdentity/);
});

test('both upload paths have owner-scoped finally cleanup and guarded progress', () => {
  assert.equal((chat.match(/remoteTransferGuard\.finish\(transferAttempt\)/g) || []).length, 2);
  assert.equal((chat.match(/current\?\.owner === transferAttempt \? null : current/g) || []).length, 2);
  assert.equal((chat.match(/progress => setRemoteTransfer\(current => currentTransfer\(\)/g) || []).length, 2);
});

test('remote model listing reloads per capability revision without changing its stable selection key', () => {
  const loading = sourceSection(
    chat, 'const remoteModelListingRequestIdentity =',
    'bridge.desktopRemoteControl.capabilitiesSnapshot',
  );
  assert.match(loading,
    /remoteModelListingRequestIdentity = remoteBinding \? remoteSendIdentity : ''/,
    'the request lifecycle must use the full send identity, including capability revision');
  assert.match(loading,
    /\[remoteModelBindingKey, remoteModelListingRequestIdentity, remoteTemporaryModelAvailable\]/,
    'capability revision changes must cancel and rerun model listing');
  assert.match(loading, /const listingKey = remoteModelBindingKey;/,
    'listing results must remain owned by the stable model-selection identity');
  assert.equal((loading.match(/key: listingKey/g) || []).length, 2,
    'both a real list (including an empty list) and a current failure must write the stable key');
  assert.equal((loading.match(/if \(!cancelled\) setRemoteModelsForBinding/g) || []).length, 2,
    'a stale success or failure must not overwrite the current revision request');
  assert.match(loading, /return \(\) => \{ cancelled = true; \};/,
    'revision or lease changes must cancel the previous async completion');
});

test('model readiness clears only its own prompt and missing model remains send-reachable', () => {
  const modelReadiness = sourceSection(
    chat, 'const remoteModelError =', 'const remoteDispatchReady =',
  );
  assert.match(modelReadiness,
    /clearResolvedRemoteModelRequiredError\(\s*current, remoteSendIdentity, remoteModelError,?\s*\)/,
    'ChatView must wire the guarded required-model cleanup into state');
  assert.match(modelReadiness, /\}, \[remoteModelError, remoteSendIdentity\]\);/,
    'cleanup must rerun for both readiness and send-owner changes');

  const newDispatchReadiness = sourceSection(
    chat, 'const remoteNewDispatchReady =', 'setRemoteFiles([]);',
  );
  assert.match(newDispatchReadiness,
    /remoteNewDispatchReady = remoteTaskContextReady\s*&& !pendingRemoteDispatches/);
  assert.doesNotMatch(newDispatchReadiness, /remoteModelError/,
    'a missing model must not disable the path that surfaces its dedicated prompt');

  const handleSend = sourceSection(chat, 'async function handleSend()', 'async function handleCancel()');
  const earlyGuard = sourceSection(handleSend, 'if (', 'if (chatVoice && chatVoice.editPreview)');
  assert.match(earlyGuard, /remoteChatActive && !remoteNewDispatchReady/);
  assert.doesNotMatch(earlyGuard, /remoteModelError|remoteDispatchReady/,
    'handleSend must allow a model-only failure to reach dispatchChatMessage');

  const composerReady = sourceSection(
    chat, 'const ready = canSend', 'const isQueue = busy && ready',
  );
  assert.match(composerReady, /!remoteChatActive \|\| remoteNewDispatchReady/);
  assert.doesNotMatch(composerReady, /remoteModelError|remoteDispatchReady/,
    'the send button must remain clickable when only the remote model is missing');
});

test('both ChatViews refresh the exact capability binding before its 30 second expiry', () => {
  for (const field of ['connectionId', 'controllerLeaseId', 'leaseEpoch', 'relayEpoch',
    'remoteControlGeneration', 'remoteSessionRef', 'capabilityRevision']) {
    assert.match(chat, new RegExp(`remoteCapabilities\\?\\.${field}|remoteCapabilities\\.${field}`),
      `A refresh binding must include ${field}`);
  }
  assert.match(chat, /expiresAtMs\) - Date\.now\(\) - 5_000/);
  assert.match(chat, /controllerCapabilitySend\(scopedRemoteConnection\.sessionRef\)/);
  assert.match(chat, /controlledCapabilitiesSnapshot\(controlledSession\.sessionId\)/);
  assert.match(chat, /controlledCapabilities\?\.connectionId[\s\S]*?controlledCapabilities\?\.expiresAtMs/,
    'B timer dependencies must bind the full snapshot identity and expiry');
  assert.match(chat, /controlledCapabilityError[\s\S]*?loadControlledCapabilities\(\)/,
    'B refresh failure must expose a retry path instead of permanently wedging send');
});

test('B capability readiness blocks sending without keeping a completed turn running', () => {
  const busyState = sourceSection(
    chat, 'const busy = remoteChatActive', '// 停止按钮 single-flight',
  );
  assert.match(busyState,
    /controlledChatActive[\s\S]*?localBusy \|\| controlledCapabilityBusy \|\| !controlledCapabilityBound/,
    'B capability refresh and binding readiness must continue to protect the send path');
  assert.match(busyState,
    /const conversationBusy = remoteChatActive\s*\? !!currentRemoteTaskId\s*: \(controlledChatActive \? localBusy : busy\);/,
    'B timeline activity must follow only the actual local generation state');

  const projection = sourceSection(
    chat, 'const conversationProjection = projectDeepSeekConversation({',
    'const { latestArtifactIds, latestArtifactIdsKey',
  );
  assert.match(projection, /busy: conversationBusy,/,
    'the projected active turn must not consume B capability-loading state');
  assert.match(projection, /\[displayChatItems, conversationBusy,/,
    'the projection must recompute when the actual conversation busy state changes');
});

test('only the Rust-verified remote projection renders the single ChatView timeline', () => {
  assert.match(chat, /remoteProxyChatItems\(activeSessionId, remoteProxyHistory\)/);
  assert.match(chat, /if \(item\.remoteProxy && item\.type === 'system'\) \{[\s\S]*?data-testid="chat-remote-projection-entry"/);
  assert.match(chat, /if \(item\.remoteProxy && item\.type === 'system'\) \{[\s\S]*?return \([\s\S]*?\);[\s\S]*?if \(item\.type === 'artifact_card'\)/);
  assert.match(chat, /if \(item\.type === 'assistant'\) \{[\s\S]*?renderLegacyMarkdownCached\(item, syntaxVersion\)/,
    'remote replies keep the ordinary ChatView Markdown and code renderer');
  assert.match(chat, /item\.remoteProxy\s*\? \{ pd: \{ draft: null \}, cq: \{ q: null, html \} \}/,
    'remote Markdown cannot manufacture local persona or question actions');
  assert.match(chat, /item\.remoteProxy\s*\? \{ text: item\.text, attachments: \[\] \}\s*: splitAttachmentLine\(item\.text\)/,
    'remote user text cannot be interpreted as a local attachment action');
  assert.match(chat, /<ToolCard\s+item=\{item\.legacyItem\}\s+sessionId=\{activeSessionId\}\s+t=\{t\}\s+variant="timeline"\s*\/>/,
    'remote tool items must reuse the ordinary B-side timeline tool renderer');
  assert.match(chat, /const conversationBusy = remoteChatActive\s*\? !!currentRemoteTaskId/,
    'hidden remote task status must still drive the ordinary turn lifecycle');
  assert.equal(remoteProjectionLabelKey('approval'), 'projectionApproval');
  assert.equal(remoteProjectionLabelKey('error'), 'projectionError');
  assert.equal(remoteProjectionLabelKey('unrecognized'), 'projectionEvent');
});

test('RemoteProxy system entries exclude tool and lifecycle protocol rows', () => {
  const systemBranch = chat.slice(
    chat.indexOf("if (item.remoteProxy && item.type === 'system')"),
    chat.indexOf("if (item.type === 'artifact_card')"),
  );
  const bodyAllowlist = chat.slice(
    chat.indexOf('const REMOTE_PROXY_SYSTEM_BODY_KINDS'),
    chat.indexOf('function remoteProxySystemBody'),
  );
  for (const kind of ['approval', 'error', 'attachment', 'artifact']) {
    assert.match(bodyAllowlist, new RegExp(`'${kind}'`));
  }
  for (const kind of ['tool', 'status', 'snapshot_marker']) {
    assert.doesNotMatch(bodyAllowlist, new RegExp(`'${kind}'`));
  }
  assert.match(systemBranch, /const systemBody = remoteProxySystemBody\(item\)/);
  assert.match(systemBranch, /data-testid="chat-remote-projection-body"[\s\S]*?\{systemBody\}/);
  assert.doesNotMatch(systemBranch, /dangerouslySetInnerHTML|JSON\.stringify\(item\)/,
    'the presentation branch must not serialize unapproved projection fields');
});

test('remote artifact preview is a keyboard-contained dialog and restores its trigger', () => {
  assert.match(chat, /remoteArtifactPreviewCloseRef\.current\?\.focus\(\)/,
    'opening the preview moves focus into the dialog');
  assert.match(chat, /trigger\?\.isConnected[\s\S]*?trigger\.focus\(\)/,
    'closing the preview restores the invoking control');
  assert.match(chat, /role="dialog" aria-modal="true"[\s\S]*?aria-labelledby="chat-remote-artifact-preview-title"/);
  assert.match(chat, /event\.key === 'Escape'[\s\S]*?closeRemoteArtifactPreview\(\)/);
  assert.match(chat, /constrainDialogFocus\(event, remoteArtifactPreviewDialogRef\.current\)/);
  assert.match(chat, /artifact\.previewKind === 'download_only'[\s\S]*?className="text-\[11px\] opacity-70"/,
    'download-only state must be visible, not screen-reader-only');
  assert.match(chat, /artifact\.previewKind === 'unsupported'[\s\S]*?className="text-\[11px\] opacity-70"/,
    'unsupported preview state must be visible, not screen-reader-only');
});

test('an uncertain remote action uses dedicated non-retry safety guidance', () => {
  assert.match(chat, /function remoteActionFailureCode\(failure\)[\s\S]*?failure\?\.code \|\| failure\?\.message \|\| failure[\s\S]*?=== 'result_unknown'/,
    'error objects and plain bridge rejections must preserve the uncertain-outcome code');
  assert.match(chat, /setRemoteActionError\(remoteActionFailureCode\(failure\)\)/);
  assert.match(chat, /remoteActionError === 'result_unknown'[\s\S]*?t\.remoteConversation\.actionResultUnknown/);
  assert.match(chat, /remoteChatActive && remoteActionError[\s\S]*?<div role="alert"/,
    'the uncertain-outcome guidance must be announced as an alert');
  for (const language of ['zh', 'en', 'ja']) {
    const copy = dict[language].remoteConversation;
    assert.ok(copy.actionResultUnknown, `${language}.actionResultUnknown`);
    assert.notEqual(copy.actionResultUnknown, copy.actionFailed,
      `${language} must distinguish uncertain outcome from an ordinary failure`);
  }
  assert.match(dict.zh.remoteConversation.actionResultUnknown, /核对.*不要.*重试|不要.*重试.*核对/);
  assert.match(dict.en.remoteConversation.actionResultUnknown, /Check.*do not retry/i);
  assert.match(dict.ja.remoteConversation.actionResultUnknown, /確認.*再試行しない/);
});

test('remote event roles and hidden-detail safety copy exist in all three languages', () => {
  for (const language of ['zh', 'en', 'ja']) {
    const copy = dict[language].remoteConversation;
    for (const key of ['projectionUser', 'projectionAssistant', 'projectionTool',
      'projectionApproval', 'projectionStatus', 'projectionError', 'projectionAttachment',
      'projectionArtifact', 'projectionSnapshot', 'projectionEvent']) {
      assert.ok(copy[key], `${language}.${key}`);
    }
    assert.equal(typeof copy.projectionAttachmentCount, 'function');
    assert.ok(copy.projectionAttachmentCount(2));
    for (const key of ['addAttachment', 'cancelTask', 'permissionRequest', 'permissionAllow',
      'permissionDeny', 'permissionTimedOut', 'artifacts', 'previewArtifact', 'downloadArtifact',
      'artifactDownloadOnly', 'artifactPreviewUnsupported', 'modeLabel', 'workspaceRequired',
      'skillsLabel', 'toolsLabel', 'modelRequired', 'actionResultUnknown']) {
      assert.ok(copy[key], `${language}.${key}`);
    }
  }
  assert.equal(dict.zh.remoteConversation.modelRequired, '模型不能为空');
});
