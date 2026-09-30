import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import { dict } from './helpers/i18n-all.js';
import { remoteProjectionLabelKey } from '../src/features/remote-control/remote-session-adapter.mjs';

const chat = readFileSync(new URL('../src/features/chat/ChatView.jsx', import.meta.url), 'utf8');

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
  assert.equal(remoteProjectionLabelKey('approval'), 'projectionApproval');
  assert.equal(remoteProjectionLabelKey('error'), 'projectionError');
  assert.equal(remoteProjectionLabelKey('unrecognized'), 'projectionEvent');
});

test('RemoteProxy system entries render only their Rust-sanitized display body', () => {
  const systemBranch = chat.slice(
    chat.indexOf("if (item.remoteProxy && item.type === 'system')"),
    chat.indexOf("if (item.type === 'artifact_card')"),
  );
  const bodyAllowlist = chat.slice(
    chat.indexOf('const REMOTE_PROXY_SYSTEM_BODY_KINDS'),
    chat.indexOf('function remoteProxySystemBody'),
  );
  for (const kind of ['tool', 'approval', 'status', 'error', 'attachment', 'artifact', 'snapshot_marker']) {
    assert.match(bodyAllowlist, new RegExp(`'${kind}'`));
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
      'skillsLabel', 'toolsLabel', 'actionResultUnknown']) {
      assert.ok(copy[key], `${language}.${key}`);
    }
  }
});
