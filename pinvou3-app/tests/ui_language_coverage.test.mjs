import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { accountPlatformCopy } from '../src/shared/account-platform-i18n.js';
import { dict } from './helpers/i18n-all.js'; // 三语全量断言：浏览器入口惰性装载，测试使用聚合 shim。
import { errorCode, localizedErrorMessage } from '../src/shared/user-facing-error.js';
import {
  applyConnectorFailure,
  connectorErrorCopy,
  connectorErrorCodeForStep,
  connectorFailure,
  connectorUiStep,
  localizeConnectorAlert,
} from '../src/features/tools/connector-ui-state.js';

const source = relative => readFileSync(new URL(`../src/${relative}`, import.meta.url), 'utf8').replace(/\r\n/g, '\n');
// Shape pins must not match text inside comments: a guard commented out in
// the source would otherwise still satisfy every anchor. Round-23 should-fix
// 2 dropped full-line `//` comments (live-verified — commenting out the
// send-registry delete left this suite green); round-24 minor-1 closes the
// two remaining holes at this single strip point, both live-verified against
// that same delete: wrapping it in `/* … */`, and hiding it as a trailing
// `//` comment behind live code. Block-comment contents are blanked with
// newlines preserved (a multi-line comment cannot fuse its neighbours into
// an anchor match) and `//` comment tails are dropped per line. For POSITIVE
// pins over-stripping can only fail loudly, never satisfy one; the same does
// NOT hold for negative pins (round-26 minor M9): stripping inside a future
// string or template literal containing `/*` could silently satisfy a
// doesNotMatch anchor, so every negative pin below runs against the RAW
// text (a pattern absent in the raw text is absent in the strip too).
const stripComments = text => text
  .replace(/\/\*[\s\S]*?\*\//g, comment => comment.replace(/[^\n]/g, ''))
  .split('\n')
  .map(line => line.replace(/\/\/.*$/, ''))
  .join('\n');

// The remaining source-consumption pins in this file are i18n coverage only
// (which components consume which dictionary namespaces, plus zh-literal
// leakage). The AuxChatPanel async-state-machine pins that used to live here
// were replaced by executing controller tests in aux_chat_controller.test.mjs
// (round-30 B1), and the remaining JSX shape pins moved to
// aux_chat_panel_contract.test.mjs — this file is i18n-only again.
const auxChatPanel = stripComments(source('features/aux-chat/AuxChatPanel.jsx'));
const chat = stripComments(source('features/chat/ChatView.jsx'));
const conversation = stripComments(source('features/conversation/ConversationTimeline.jsx'));
const codex = stripComments(source('features/codex/CodexAcpView.jsx'));
// Raw twins for the negative pins (round-26 minor M9, see the stripComments
// header): doesNotMatch anchors must see comments and string contents too.
const chatRaw = source('features/chat/ChatView.jsx');
const conversationRaw = source('features/conversation/ConversationTimeline.jsx');
const tauriSource = relative => readFileSync(new URL(`../src-tauri/src/${relative}`, import.meta.url), 'utf8');
const isRecord = value => value !== null && typeof value === 'object' && !Array.isArray(value);

function assertContainsTranslationKeys(reference, candidate, path) {
  for (const key of Object.keys(reference)) {
    const keyPath = `${path}.${key}`;
    assert.ok(Object.hasOwn(candidate, key), `${keyPath} must exist`);
    if (isRecord(reference[key])) {
      assert.ok(isRecord(candidate[key]), `${keyPath} must be an object`);
      assertContainsTranslationKeys(reference[key], candidate[key], keyPath);
    }
  }
}

assertContainsTranslationKeys(dict.zh, dict.en, 'en');
assertContainsTranslationKeys(dict.zh, dict.ja, 'ja');

for (const language of ['zh', 'en', 'ja']) {
  for (const section of [
    'uiRemote',
    'uiMonitor',
    'uiSettings',
    'uiSettingsDetail',
    'uiPetSettings',
    'uiScheduled',
    'uiChat',
    'uiChatExtra',
    'uiChatScenes',
    'uiAccount',
    'uiChatWorkspace',
    'artifactPreview',
    'uiToolStore',
    'uiModelStore',
    'uiPet',
    'uiWebConnection',
    'uiConversation',
    'uiComputerUse',
    'uiHomeMode',
    'uiAttachments',
    'uiCodex',
    'uiCodexWorkspace',
    'uiAcpProviders',
    'uiProjects',
    'uiArtifacts',
    'uiToolDetails',
    'uiTeamRelay',
    'uiTeamRelayAccess',
    'uiBuiltinPlugins',
    'uiAuxChat',
  ]) {
    assert.ok(dict[language][section], `${language}.${section} must exist`);
  }
  // All 23 uiAuxChat keys are pinned (round-14 minor-3: the list previously
  // covered 13, so sendingHint/bindingHint and the six quote* keys could be
  // deleted from every dictionary with the suite green — and quoteChipCount's
  // absence renders `undefined` at runtime; round-30 D5 added quoteDuplicate
  // for the exact-duplicate notice; M6 removed discardStuck with the stuck
  // family it described — the atomic reset cannot get stuck that way;
  // round-35 MAJOR-3: newTopicConfirmShort (the visible armed label, whose
  // absence would render `undefined` on the destructive confirm) joined the
  // list the round after it shipped — count and list now agree again).
  for (const key of [
    'openLabel', 'panelTitle', 'landingHint', 'emptyState', 'inputPlaceholder',
    'send', 'busyHint', 'bindingHint', 'sendingHint', 'newTopic', 'newTopicConfirm',
    'newTopicConfirmShort',
    'sendFailed', 'ensureFailed', 'discardFailed', 'close',
    'quoteAction', 'quoteChipCount', 'quoteRemove',
    'quoteLimitSingle', 'quoteLimitCount', 'quoteLimitTotal', 'quoteDuplicate',
  ]) {
    assert.ok(dict[language].uiAuxChat[key], `${language}.uiAuxChat.${key} must exist`);
  }
  // Count guard (round-36 minor 9): the pin above lists 23 keys but cannot
  // see additions — a 24th key added to all three dictionaries passes the
  // existence loop silently and drifts the parity surface. Any key change
  // must update this count with the list in the same commit.
  assert.equal(
    Object.keys(dict[language].uiAuxChat).length,
    23,
    `${language}.uiAuxChat must hold exactly the 23 pinned keys`,
  );
  assert.ok(dict[language].uiSettings.providers, `${language}.uiSettings.providers must exist`);
  for (const key of [
    'addProvider', 'switch', 'official', 'current', 'export', 'import',
    'envConflictTitle', 'uninstallTitle', 'sessionProvider', 'faultManage',
    'thirdPartyWarning', 'deleteTitle', 'secretSet', 'notEnabled', 'restoreOfficial',
    'login', 'logout', 'loginWaiting', 'openLoginUrl', 'loginCodePlaceholder', 'submitCode', 'logoutRelayDisabled',
    'cancelInstall', 'installCancelled',
    'modelSlotsTitle', 'modelSlotsHint', 'modelSlotsRequired',
    // 注意：ProviderFormModal 用 `copy[`slot_${slot}`]` 动态取键（CLAUDE_MODEL_SLOT_IDS），
    // 静态扫描查不到消费方，此处断言是防误删的唯一钉子。
    'slot_opus', 'slot_sonnet', 'slot_haiku', 'slot_fable', 'slot_subagent',
    'contextWindow', 'contextWindowHint', 'contextWindowInvalid',
  ]) {
    assert.ok(dict[language].uiAcpProviders[key], `${language}.uiAcpProviders.${key} must exist`);
  }
  assert.ok(dict[language].uiScheduled.createFromTemplate, `${language}.uiScheduled.createFromTemplate must exist`);
  assert.ok(dict[language].uiScheduled.runHistory, `${language}.uiScheduled.runHistory must exist`);
  assert.ok(dict[language].uiSettingsDetail.restartNow, `${language}.uiSettingsDetail.restartNow must exist`);
  assert.ok(dict[language].uiSettingsDetail.deleteModelTitle, `${language}.uiSettingsDetail.deleteModelTitle must exist`);
  assert.ok(dict[language].uiChat.asrDownloadTitle, `${language}.uiChat.asrDownloadTitle must exist`);
  assert.ok(dict[language].uiChat.memoryMeta.preference, `${language}.uiChat.memoryMeta.preference must exist`);
  assert.ok(dict[language].uiChat.sceneModes.personalWorkbench, `${language}.uiChat.sceneModes.personalWorkbench must exist`);
  assert.ok(dict[language].uiChat.sceneModes.documentWriting, `${language}.uiChat.sceneModes.documentWriting must exist`);
  assert.ok(dict[language].uiChat.sceneModes.poster, `${language}.uiChat.sceneModes.poster must exist`);
  assert.ok(dict[language].uiChat.sceneModes.dataVisualization, `${language}.uiChat.sceneModes.dataVisualization must exist`);
  assert.ok(dict[language].uiChat.sceneModes.pptDesign, `${language}.uiChat.sceneModes.pptDesign must exist`);
  // The sceneModes keys retired by the design-lane merge into work must
  // stay deleted.
  for (const deadKey of ['designGeneralPlaceholder']) {
    assert.equal(dict[language].uiChat.sceneModes[deadKey], undefined, `${language}.uiChat.sceneModes.${deadKey} is retired and must stay deleted`);
  }
  assert.ok(dict[language].uiChatView.placeholderSceneAdjust, `${language}.uiChatView.placeholderSceneAdjust must exist`);
  assert.ok(dict[language].uiChatView.placeholderSceneDataViz, `${language}.uiChatView.placeholderSceneDataViz must exist`);
  assert.ok(dict[language].uiChatView.placeholderScenePoster, `${language}.uiChatView.placeholderScenePoster must exist`);
  assert.ok(dict[language].uiChatView.placeholderScenePpt, `${language}.uiChatView.placeholderScenePpt must exist`);
  assert.ok(dict[language].uiChatView.placeholderPersonalWorkbench, `${language}.uiChatView.placeholderPersonalWorkbench must exist`);
  assert.ok(dict[language].uiChatScenes.pptDesign, `${language}.uiChatScenes.pptDesign must exist`);
  assert.equal(
    typeof dict[language].uiChat.sceneModes.clear,
    'function',
    `${language}.uiChat.sceneModes.clear must be a function`,
  );
  assert.ok(dict[language].uiChatExtra.draftingScheduled, `${language}.uiChatExtra.draftingScheduled must exist`);
  assert.ok(dict[language].uiChatScenes.attachmentPrompt, `${language}.uiChatScenes.attachmentPrompt must exist`);
  assert.ok(accountPlatformCopy[language].availableCredits, `${language}.uiAccountPlatform.balance must exist`);
  assert.ok(accountPlatformCopy[language].requestFailed, `${language}.uiAccountPlatform.requestFailed must exist`);
  assert.ok(accountPlatformCopy[language].errors.oidc_credentials_invalid, `${language}.uiAccountPlatform.errors.oidc_credentials_invalid must exist`);
  assert.ok(dict[language].uiToolStore.toolNames.eip, `${language}.uiToolStore.toolNames.eip must exist`);
  assert.ok(dict[language].uiToolStore.toolNames.zhidao, `${language}.uiToolStore.toolNames.zhidao must exist`);
  assert.ok(dict[language].uiToolStore.sso, `${language}.uiToolStore.sso must exist`);
  assert.ok(dict[language].uiToolStore.internalDetailDesc, `${language}.uiToolStore.internalDetailDesc must exist`);
  assert.ok(dict[language].uiToolStore.internalUsageHint, `${language}.uiToolStore.internalUsageHint must exist`);
  assert.ok(dict[language].uiToolStore.connectorErrors.sso_timeout, `${language}.uiToolStore.connectorErrors.sso_timeout must exist`);
  for (const code of ['runtime_prepare_failed', 'cli_install_failed', 'auth_start_failed', 'registration_failed', 'auth_failed', 'skills_enable_failed']) {
    assert.ok(dict[language].uiToolStore.connectorErrors[code], `${language}.uiToolStore.connectorErrors.${code} must exist`);
  }
  for (const toolId of ['eip', 'zhidao']) {
    const tool = dict[language].uiToolDetails.tools[toolId];
    assert.ok(tool, `${language}.uiToolDetails.tools.${toolId} must exist`);
    for (const field of ['title', 'subtitle', 'type', 'latency', 'desc', 'welcomeQueries']) {
      assert.ok(tool[field], `${language}.uiToolDetails.tools.${toolId}.${field} must exist`);
    }
    assert.equal(tool.welcomeQueries.length, 4, `${language}.uiToolDetails.tools.${toolId}.welcomeQueries must contain four prompts`);
  }
  assert.ok(dict[language].uiSettingsDetail.settingsLoadFailed, `${language}.uiSettingsDetail.settingsLoadFailed must exist`);
  // uiMultiAgent 收缩为活键集（ADR-0006）：开关行 + 行内专家卡 + 只读面板。
  // 确认卡/审批链/台账时代的键已随旧入口退役，不再断言存在。
  const multiAgent = dict[language].uiMultiAgent;
  assert.equal(typeof multiAgent.drawerTitle, 'function', `${language}.uiMultiAgent.drawerTitle must be a function`);
  assert.equal(typeof multiAgent.coordinationRow, 'function', `${language}.uiMultiAgent.coordinationRow must be a function`);
  for (const key of ['agentsListSummary', 'childAgentCount', 'expandChildren', 'collapseChildren']) {
    assert.equal(typeof multiAgent[key], 'function', `${language}.uiMultiAgent.${key} must be a function`);
  }
  for (const role of ['scout', 'manager', 'builder', 'reviewer', 'general']) {
    assert.ok(multiAgent.roleCards[role], `${language}.uiMultiAgent.roleCards.${role} must exist`);
  }
  for (const key of ['toggleLabel', 'toggleHint', 'close', 'loadingTranscript', 'emptyTranscript', 'blockedTag', 'panelResize', 'panelResizeHint', 'agentsListTitle', 'agentsEmpty', 'backToAgents', 'spawnedAgentsRowHint', 'runningAgentsTitle', 'runningAgentsCollapse', 'runningAgentsExpand']) {
    assert.ok(multiAgent[key], `${language}.uiMultiAgent.${key} must exist`);
  }
  for (const fnKey of ['spawnedAgentsRow', 'runningAgentsCount']) {
    assert.equal(typeof multiAgent[fnKey], 'function', `${language}.uiMultiAgent.${fnKey} must be a function`);
  }
  for (const cardKey of ['working', 'completed', 'failed', 'spawnFailed', 'interrupted', 'cancelled']) {
    assert.ok(multiAgent.agentCard[cardKey], `${language}.uiMultiAgent.agentCard.${cardKey} must exist`);
  }
  // The ConversationTimeline status badge renders uiConversation.cancelled for
  // the ledger's lowercase cancelled token (case-insensitive match); the key
  // must exist in all three locales.
  assert.ok(dict[language].uiConversation.cancelled, `${language}.uiConversation.cancelled must exist`);
  for (const deadKey of ['confirmTitle', 'impactLabels', 'startDenied', 'stages', 'terminal', 'workerCount', 'advancedEdit', 'planCompileError']) {
    assert.equal(multiAgent[deadKey], undefined, `${language}.uiMultiAgent.${deadKey} is retired and must stay deleted`);
  }
  // agentCard spawning-era keys: 'spawning' died with the inline expert card
  // (the aggregate count row only speaks in past-tense totals).
  assert.equal(multiAgent.agentCard.spawning, undefined, `${language}.uiMultiAgent.agentCard.spawning is retired and must stay deleted`);
  // The bottom-right code-style toggle was replaced by the All/Code pill in
  // the task list header; its tooltip copy must not come back as dead keys.
  for (const deadKey of ['sidebarCodeStyleOn', 'sidebarCodeStyleOff']) {
    assert.equal(dict[language][deadKey], undefined, `${language}.${deadKey} is retired and must stay deleted`);
  }
}

// 三语 key parity:zh 是全集基准,en 必须覆盖 zh 的每个叶子 key(ja 经 en
// spread 兜底,同样断言)。en/ja 历史上比 zh 多出的 settings 相关键属基线
// 固有(zh 侧由 UI 层默认值兜底),故只断言方向性覆盖而非严格相等——漏 key
// 的 en 用户会渲染 undefined,ja 也随 en 一起漏。
{
  const leafPaths = (obj, prefix = '') => {
    const out = [];
    for (const key of Object.keys(obj)) {
      const value = obj[key];
      const path = prefix ? `${prefix}.${key}` : key;
      if (value && typeof value === 'object' && !Array.isArray(value)) out.push(...leafPaths(value, path));
      else out.push(path);
    }
    return out;
  };
  const zhKeys = leafPaths(dict.zh);
  assert.ok(zhKeys.length > 2000, `zh leaf key count looks wrong: ${zhKeys.length}`);
  for (const language of ['en', 'ja']) {
    const known = new Set(leafPaths(dict[language]));
    const missing = zhKeys.filter((key) => !known.has(key));
    assert.deepEqual(
      missing,
      [],
      `${language} dictionary misses zh keys (renders undefined): ${missing.slice(0, 10).join(', ')}`,
    );
  }
}

// uiComputerUse (round-17): type parity across languages — a template
// flattened to a string in any language makes describeConfirmAction's
// function guard fall back to the raw English summary while the generic
// leaf walk above stays green. Also pin that the consent surface and the
// settings section actually consume the namespace.
{
  const typedLeafKeys = (obj, prefix = '') => {
    const out = [];
    for (const key of Object.keys(obj)) {
      const value = obj[key];
      const path = prefix ? `${prefix}.${key}` : key;
      if (value && typeof value === 'object' && !Array.isArray(value)) out.push(...typedLeafKeys(value, path));
      else out.push([path, typeof value]);
    }
    return out;
  };
  const zhTypes = new Map(typedLeafKeys(dict.zh.uiComputerUse));
  assert.ok(zhTypes.size >= 37, `uiComputerUse leaf count looks wrong: ${zhTypes.size}`);
  for (const language of ['en', 'ja']) {
    const types = new Map(typedLeafKeys(dict[language].uiComputerUse));
    for (const [key, type] of zhTypes) {
      assert.equal(types.get(key), type, `${language}.uiComputerUse.${key} must be a ${type} like zh`);
    }
  }
  assert.match(source('features/chat/ChatView.jsx'), /t\.uiComputerUse/);
  assert.match(source('features/settings/SettingsView.jsx'), /uiComputerUse/);
}

const main = source('app/main.jsx');
assert.match(main, /emit\(['"]ui:language_changed['"], \{ language: lang \}\)/);
const viewLoaders = source('app/view-loaders.js');
assert.match(viewLoaders, new RegExp("toolStore: \\(\\) => import\\('\\.\\./features/tools/ToolStoreView\\.jsx'\\)"));
assert.match(main, /<LazyToolStoreView[^>]*t=\{t\}/);
assert.match(main, /<WebConnectionStatus[^>]*t=\{t\}/);
assert.match(main, /<LazyAccountPlatformPopover[\s\S]{0,400}copy=\{accountPlatformCopy\[language\]\}/);
assert.match(main, /title=\{accountPlatformCopy\[language\]\.title\}/);
assert.doesNotMatch(main, /<AccountPopover/);
const accountPlatformPopover = source('features/account-platform/AccountPlatformPopover.jsx');
assert.match(accountPlatformPopover, /<AccountPlatformView \{\.\.\.props\}/);
const accountPlatformView = source('features/account-platform/AccountPlatformView.jsx');
assert.match(accountPlatformView, /copy\.availableCredits/);
assert.match(accountPlatformView, /copy\.reservedCredits/);
assert.match(accountPlatformView, /localizedAccountPlatformError/);
assert.doesNotMatch(accountPlatformPopover, /platformModel|selectPlatformModel|modelSelectionSaved/);
assert.doesNotMatch(accountPlatformPopover, />可用余额</);
assert.equal(accountPlatformCopy.zh.availableCredits, '可用积分');
assert.equal(accountPlatformCopy.zh.reservedCredits, '使用中');
assert.match(main, /<ViewErrorBoundary[^>]*heading=\{t\.uiSettingsDetail\.settingsLoadFailed\}[^>]*detail=\{t\.uiSettingsDetail\.settingsLoadFailedDetail\}[^>]*t=\{t\}/);
assert.match(viewLoaders, new RegExp("codex: \\(\\) => import\\('\\.\\./features/codex/CodexAcpView\\.jsx'\\)"));
// The main window renders codex through the LazyCodexAcpView wrapper (which
// internally consumes the same view-loaders chunk); its error fallback copy
// still flows through i18n.
assert.match(main, /<CodexAcpView[^>]*t=\{t\}/);
// The settings error boundary has been merged into the shared ViewErrorBoundary; its title flows through i18n via the heading prop, and the settings boundary keeps its friendly detail copy (never the raw error message).
const viewErrorBoundary = source('shared/ViewErrorBoundary.jsx');
assert.match(viewErrorBoundary, /this\.props\.heading \|\| copy\.viewLoadFailed/);
assert.match(viewErrorBoundary, /this\.props\.detail \|\| message/);
assert.doesNotMatch(viewErrorBoundary, />设置页加载失败</);
assert.match(main, /t\.uiScheduled\.runStatus\[value\]/);
assert.doesNotMatch(main, /queued:\s*'等待中'/);

const petWindow = source('features/pet/PetWindow.jsx');
assert.match(petWindow, /invokeTauri\(['"]get_settings['"]\)/);
assert.match(petWindow, /listen\(['"]ui:language_changed['"]/);
assert.match(petWindow, /const petCopy = t\.uiPet/);

assert.match(source('features/monitor/MonitorView.jsx'), /t\.uiMonitor/);
const scheduledTasks = source('features/scheduled/ScheduledTasksView.jsx');
assert.match(scheduledTasks, /const scheduledCopy = t\.uiScheduled/);
assert.match(scheduledTasks, /scheduledCopy\.taskName/);
assert.match(scheduledTasks, /scheduledCopy\.runHistory/);
assert.doesNotMatch(scheduledTasks, />立即运行</);
const toolStore = source('features/tools/ToolStoreView.jsx');
const connectorSsoDialog = source('features/tools/ConnectorSsoDialog.jsx');
assert.match(toolStore, /const storeCopy = t\.uiToolStore/);
assert.match(toolStore, /localizeTool\(baseTool, t\)/);
assert.match(toolStore, /<ConnectorSsoDialog[\s\S]*copy=\{storeCopy\}/);
assert.match(connectorSsoDialog, /copy\.sso\.title/);
assert.match(toolStore, /storeCopy\.internalDetailDesc/);
assert.match(toolStore, /localizeConnectorAlert\(alert, storeCopy, detailCopy\)/);
assert.match(toolStore, /errors=\{storeCopy\.connectorErrors\}/);
assert.match(toolStore, /connectorAlert: 'connected'/);
assert.match(toolStore, /connectorAlert: 'failed'/);
assert.match(toolStore, /applyConnectorFailure\(f, p, p\.phase\)/);
assert.doesNotMatch(toolStore, /ensure(?:Feishu|Wecom|Dingtalk|Tmeet)Listeners\(storeCopy\)/);
assert.doesNotMatch(toolStore, /\berr:\s*String\(/);
assert.doesNotMatch(toolStore, /flow\.err\b/);
assert.doesNotMatch(toolStore, /errorDetail/);
assert.doesNotMatch(toolStore, /console\.error\([^\n]*(?:p\.message|failure\.errorDetail)/);
assert.doesNotMatch(toolStore, /console\.error\(['"][^'"]* connect failed:['"],\s*e\)/);
assert.doesNotMatch(toolStore, /ev\.listen\('eip:error'[\s\S]{0,500}subtitle: String\(p\.message/);
assert.doesNotMatch(toolStore, /ev\.listen\('zhidao:error'[\s\S]{0,500}subtitle: String\(p\.message/);
assert.doesNotMatch(toolStore, />登录员工门户（EIP）</);
assert.doesNotMatch(toolStore, /title:\s*'EIP 连接失败'/);
const teamRelay = source('features/team-relay/TeamRelayView.jsx');
assert.match(teamRelay, /const copy = t\.uiTeamRelay/);
assert.doesNotMatch(teamRelay, />团队接力</);
assert.doesNotMatch(teamRelay, />最终交付</);
const teamRelayAccess = source('features/team-relay/TeamRelayAccessSettings.jsx');
assert.match(teamRelayAccess, /copy\.notice/);
assert.match(teamRelayAccess, /type="password"/);
assert.match(teamRelayAccess, /copy\.disable/);
assert.doesNotMatch(teamRelayAccess, /当前功能内测中/);
const settings = source('features/settings/SettingsView.jsx');
assert.match(settings, /t\.uiSettings/);
assert.match(settings, /const settingsCopy = (?:\/\*\* @type \{typeof )?t\.uiSettingsDetail/);
assert.match(settings, /settingsCopy\.addSearch/);
assert.match(settings, /settingsCopy\.deleteModelTitle/);
assert.match(settings, /item\.title \|\| presetProviderLabel\(p, t\)/);
assert.doesNotMatch(settings, />添加搜索源</);
const generalSettings = settings.slice(settings.indexOf('const renderGeneral'), settings.indexOf('const renderModels'));
assert.ok(generalSettings.indexOf('<TeamRelayAccessSettings') > generalSettings.indexOf('<PetSettingsSection'));
assert.match(chat, /const chatCopy = t\.uiChat/);
assert.match(chat, /chatCopy\.asrDownloadTitle/);
assert.match(chat, /chatCopy\.memoryMeta/);
assert.match(chat, /chatCopy\.sceneModes/);
assert.match(chat, /chatViewCopy\.placeholderSceneAdjust/);
assert.match(chat, /chatViewCopy\.placeholderSceneDataViz/);
assert.match(chat, /chatViewCopy\.placeholderScenePoster/);
assert.match(chat, /chatViewCopy\.placeholderScenePpt/);
assert.match(chat, /outgoing \|\| t\.uiChatScenes\.attachmentPrompt/);
assert.doesNotMatch(chatRaw, /outgoing \|\| '请根据附件内容继续处理。'/);
assert.doesNotMatch(chatRaw, /designGeneralPlaceholder/);
assert.doesNotMatch(chatRaw, /label:\s*'个人工作台'/);
assert.doesNotMatch(chatRaw, /label:\s*'公文写作'/);
assert.doesNotMatch(chatRaw, /label:\s*'数据可视化'/);
assert.doesNotMatch(chatRaw, /`取消\$\{scene\.label\}`/);
assert.doesNotMatch(chatRaw, /:\s*'描述你想生成或调整的内容'/);
assert.doesNotMatch(chatRaw, />下载语音识别模型</);
assert.match(auxChatPanel, /const copy = t\.uiAuxChat/);
assert.match(auxChatPanel, /copy=\{conversationCopy\}/);
// Account Platform replaces the retired LLMAPI availability messages;
// localized account failures are pinned below through localizedErrorMessage.

assert.equal(errorCode({ code: 'sso_timeout', message: '原始诊断' }), 'sso_timeout');
assert.equal(errorCode(new Error('oidc_credentials_invalid: raw backend detail')), 'oidc_credentials_invalid');
assert.equal(errorCode(new Error('raw backend detail')), '');
assert.equal(
  localizedErrorMessage(
    new Error('oidc_credentials_invalid: raw backend detail'),
    accountPlatformCopy.ja.errors,
    accountPlatformCopy.ja.requestFailed,
  ),
  accountPlatformCopy.ja.errors.oidc_credentials_invalid,
);
assert.equal(
  localizedErrorMessage(
    new Error('未知的底层诊断'),
    accountPlatformCopy.en.errors,
    accountPlatformCopy.en.requestFailed,
  ),
  accountPlatformCopy.en.requestFailed,
);
assert.equal(connectorErrorCodeForStep('runtime'), 'runtime_prepare_failed');
assert.equal(connectorErrorCodeForStep('cli'), 'cli_install_failed');
assert.equal(connectorErrorCodeForStep('register'), 'registration_failed');
assert.equal(connectorErrorCodeForStep('qr'), 'auth_failed');
assert.deepEqual(
  connectorFailure({ code: 'auth_failed', message: 'raw backend diagnostic' }, 'qr'),
  { errorCode: 'auth_failed' },
);
assert.deepEqual(
  connectorFailure(new Error('raw install diagnostic'), 'cli'),
  { errorCode: 'cli_install_failed' },
);
assert.equal(connectorUiStep({ active: 'cli' }, 'authorize'), 'qr');
assert.equal(connectorUiStep({ active: 'connect' }, 'register'), 'connect');
assert.equal(connectorUiStep({ active: 'qr' }, 'register'), 'qr');
assert.equal(connectorUiStep(null, 'register'), 'connect');
assert.equal(connectorUiStep(null, 'authorize'), 'qr');
const failedAuthorizationFlow = applyConnectorFailure(
  { active: 'cli', steps: { runtime: 'done', cli: 'done' } },
  { code: 'auth_failed', message: 'raw backend diagnostic' },
  'authorize',
);
assert.equal(failedAuthorizationFlow.phase, 'error');
assert.equal(failedAuthorizationFlow.steps.cli, 'done');
assert.equal(failedAuthorizationFlow.steps.qr, 'error');
assert.equal(Object.hasOwn(failedAuthorizationFlow.steps, 'authorize'), false);
assert.equal(Object.hasOwn(failedAuthorizationFlow, 'errorDetail'), false);
const storedAccountErrorCode = errorCode(new Error('oidc_credentials_invalid: raw backend detail'));
const englishAccountError = localizedErrorMessage(
  { code: storedAccountErrorCode },
  accountPlatformCopy.en.errors,
  accountPlatformCopy.en.requestFailed,
);
const japaneseAccountError = localizedErrorMessage(
  { code: storedAccountErrorCode },
  accountPlatformCopy.ja.errors,
  accountPlatformCopy.ja.requestFailed,
);
assert.equal(englishAccountError, accountPlatformCopy.en.errors.oidc_credentials_invalid);
assert.equal(japaneseAccountError, accountPlatformCopy.ja.errors.oidc_credentials_invalid);
assert.notEqual(englishAccountError, japaneseAccountError);
const semanticAlert = { visible: true, connectorAlert: 'failed', errorCode: 'auth_failed', toolId: 'wecom' };
const englishAlert = localizeConnectorAlert(semanticAlert, dict.en.uiToolStore, dict.en.uiToolDetails);
const japaneseAlert = localizeConnectorAlert(semanticAlert, dict.ja.uiToolStore, dict.ja.uiToolDetails);
assert.equal(englishAlert.title, dict.en.uiToolStore.connectFailed(dict.en.uiToolStore.toolNames.wecom));
assert.equal(englishAlert.subtitle, dict.en.uiToolStore.connectorErrors.auth_failed);
assert.equal(japaneseAlert.subtitle, dict.ja.uiToolStore.connectorErrors.auth_failed);
assert.notEqual(englishAlert.subtitle, japaneseAlert.subtitle);
assert.doesNotMatch(englishAlert.subtitle, /raw backend diagnostic/);
const connectedAlert = { visible: true, connectorAlert: 'connected', showEnabled: true, toolId: 'tmeet' };
assert.equal(
  localizeConnectorAlert(connectedAlert, dict.en.uiToolStore, dict.en.uiToolDetails).subtitle,
  dict.en.uiToolDetails.actions.enabled,
);
assert.equal(
  localizeConnectorAlert(connectedAlert, dict.ja.uiToolStore, dict.ja.uiToolDetails).subtitle,
  dict.ja.uiToolDetails.actions.enabled,
);
const eipConnector = tauriSource('features/connectors/eip.rs');
const zhidaoConnector = tauriSource('features/connectors/zhidao.rs');
for (const connector of [eipConnector, zhidaoConnector]) {
  assert.match(connector, /"code": "sso_start_failed"/);
  assert.match(connector, /"code": "sso_timeout"/);
}
const feishuConnector = tauriSource('features/connectors/feishu.rs');
assert.match(feishuConnector, /"code": "registration_failed"/);
assert.match(feishuConnector, /"code": "auth_failed"/);
// dingtalk 走 FlowError 动态 code（默认 auth_failed），不再内联字面量；
// 由下方终态断言块对 feishu/wecom/tmeet 校验事件负载。
for (const connector of ['wecom.rs', 'tmeet.rs']) {
  assert.match(tauriSource(`features/connectors/${connector}`), /"code": "auth_failed"/);
}
assert.match(source('features/pet/PetSettingsSection.jsx'), /t\.uiPetSettings/);
assert.match(conversation, /conversationCopy\(copy\)/);
assert.doesNotMatch(conversationRaw, />等待授权</);
assert.match(codex, /const codexCopy = t\.uiCodex/);
assert.match(codex, /copy=\{t\.uiConversation\}/);
assert.match(codex, /copy=\{t\.uiCodexWorkspace\}/);
const workspace = source('features/codex/CodexWorkspacePanel.jsx');
assert.match(workspace, /\{copy\.title\}/);
assert.doesNotMatch(workspace, />工作区</);
const providersSection = source('features/settings/ProvidersSection.jsx');
assert.match(providersSection, /const copy = t\.uiAcpProviders/);
assert.doesNotMatch(providersSection, />新增 Provider</);
assert.doesNotMatch(providersSection, />切换</);
assert.doesNotMatch(providersSection, />官方登录</);
const settingsViewProviders = source('features/settings/SettingsView.jsx');
assert.match(settingsViewProviders, /t\.uiSettings\.providers/);
assert.match(settingsViewProviders, /<ProvidersSection/);
const providerFormModal = source('features/settings/ProviderFormModal.jsx');
assert.match(providerFormModal, /invokeTauri\(['"]save_acp_provider['"]/);
assert.doesNotMatch(providerFormModal, /placeholder=\{?['"]输入 API Key/);
const codexViewProviders = source('features/codex/CodexAcpView.jsx');
assert.match(codexViewProviders, /set_codex_acp_session_provider/);
assert.match(codexViewProviders, /t\.uiAcpProviders/);
const personas = source('features/personas/Personas.jsx');
assert.match(personas, /\{t\.cpMyCards\}/);
assert.doesNotMatch(personas, /ExpertTeamsPanel|expertPoolTeamTab|expertPoolIndividualTab/);

// 设计检查器字体预设:数据侧 label 保留中文原名(选中态比较键),展示走 labelKey;
// 每个预设都必须声明 labelKey 且三语词典都有该 key,否则 en/ja 用户会看到中文原名。
const fontPresets = source('features/artifacts/DesignInspectorPanel.jsx');
const fontLabelKeys = [...fontPresets.matchAll(/labelKey: '(\w+)'/g)].map(m => m[1]);
assert.ok(fontLabelKeys.length >= 9, `font presets should declare labelKey, found ${fontLabelKeys.length}`);
for (const language of ['zh', 'en', 'ja']) {
  for (const key of fontLabelKeys) {
    assert.ok(dict[language].uiArtifacts[key], `${language}.uiArtifacts.${key} must exist`);
  }
}

// 个人工作台模板 chip:每个模板 id 三语都要有展示名;数据侧 zh title 只是
// 草稿匹配与消息 meta 的正本,不直接展示给 en/ja 用户。
const workbench = source('features/chat/personal-workbench-scene.js');
const workbenchTemplateIds = [...workbench.matchAll(/\n {4}id: '([\w-]+)',/g)].map(m => m[1]);
assert.equal(workbenchTemplateIds.length, 7, `expected 7 workbench templates, found ${workbenchTemplateIds.length}`);
for (const language of ['zh', 'en', 'ja']) {
  for (const id of workbenchTemplateIds) {
    assert.ok(dict[language].uiChatScenes.workbenchTemplates[id], `${language}.uiChatScenes.workbenchTemplates.${id} must exist`);
  }
}

// Connector flow-card failures: stored as error codes and localized at render
// time, so en/ja users never see raw (often Chinese) backend text. The raw
// backend diagnostic is never rendered in the flow card (same border as the
// Official repo's PR #442: the localized category message is the only copy).
// cli_data_access_disabled is dingtalk's org-level CLI block: only an org
// admin can fix it, so the copy carries the remediation instead of the
// generic retry advice.
const connectorErrorCodes = ['runtime_prepare_failed', 'cli_install_failed', 'auth_start_failed', 'registration_failed', 'auth_failed', 'skills_enable_failed', 'cli_data_access_disabled', 'consent_persist_failed', 'unknown'];
for (const language of ['zh', 'en', 'ja']) {
  for (const code of connectorErrorCodes) {
    const copy = dict[language].uiToolStore.connectorErrors[code];
    assert.equal(typeof copy, 'string', `${language}.uiToolStore.connectorErrors.${code} must exist`);
    assert.ok(copy.length > 0, `${language}.uiToolStore.connectorErrors.${code} must not be empty`);
  }
  assert.ok(typeof dict[language].updateCheckFailed === 'string' && dict[language].updateCheckFailed.length > 0, `${language}.updateCheckFailed must exist and not be empty`);
  assert.ok(typeof dict[language].updateInstallFailed === 'string' && dict[language].updateInstallFailed.length > 0, `${language}.updateInstallFailed must exist and not be empty`);
  for (const retired of ['connFailed', 'dingtalkSkillsFailed', 'tmeetAuthIncomplete']) {
    assert.equal(dict[language].uiToolStore[retired], undefined, `${language}.uiToolStore.${retired} is replaced by connectorErrors`);
  }
  // Round-37 F1 (review #455): presence guards for the keys whose deletion
  // previously failed nothing — dropping one from a locale must fail now.
  for (const key of ['installedReady', 'welcomeOptInFailed']) {
    assert.equal(typeof dict[language].uiChat[key], 'string', `${language}.uiChat.${key} must exist and not be empty`);
    assert.ok(dict[language].uiChat[key].length > 0, `${language}.uiChat.${key} must not be empty`);
  }
  for (const key of ['switchedOffPacks', 'notAppliedPacks']) {
    assert.equal(typeof dict[language].uiChatScenes[key], 'function', `${language}.uiChatScenes.${key} must exist`);
  }
  assert.equal(
    typeof dict[language].uiToolStore.connectorErrors.consent_persist_failed,
    'string',
    `${language}.uiToolStore.connectorErrors.consent_persist_failed must exist and not be empty`
  );
}
// The render lookup must resolve own keys only: `constructor`/`toString` pass the
// snake_case code pattern but would resolve to Object.prototype functions.
const connectorErrorsDict = dict.en.uiToolStore.connectorErrors;
assert.equal(connectorErrorCopy(connectorErrorsDict, 'auth_failed'), connectorErrorsDict.auth_failed);
assert.equal(connectorErrorCopy(connectorErrorsDict, 'constructor'), '');
assert.equal(connectorErrorCopy(connectorErrorsDict, 'toString'), '');
assert.equal(connectorErrorCopy(connectorErrorsDict, 'valueOf'), '');
assert.equal(connectorErrorCopy(connectorErrorsDict, 'hasOwnProperty'), '');
assert.equal(connectorErrorCopy(connectorErrorsDict, 42), '');
assert.equal(connectorErrorCopy(connectorErrorsDict, null), '');
assert.equal(connectorErrorCopy(null, 'auth_failed'), '');
assert.equal(errorCode({ code: 'auth_failed', message: 'raw diagnostic' }), 'auth_failed');
assert.equal(errorCode(new Error('cli_install_failed: raw backend detail')), 'cli_install_failed');
assert.equal(errorCode(new Error('raw backend detail')), '');
assert.equal(errorCode({ code: 'Not A Code' }), '');
assert.equal(errorCode(null), '');
assert.equal(errorCode(42), '');
assert.equal(errorCode(':leading-separator'), '');
assert.equal(connectorErrorCodeForStep('runtime'), 'runtime_prepare_failed');
assert.equal(connectorErrorCodeForStep('cli'), 'cli_install_failed');
assert.equal(connectorErrorCodeForStep('register'), 'registration_failed');
assert.equal(connectorErrorCodeForStep('qr'), 'auth_failed');
assert.equal(connectorErrorCodeForStep('bogus'), 'unknown');
assert.deepEqual(connectorFailure({ code: 'auth_failed', message: 'raw backend diagnostic' }, 'qr'), { errorCode: 'auth_failed' });
assert.deepEqual(connectorFailure(new Error('raw install diagnostic'), 'cli'), { errorCode: 'cli_install_failed' });
assert.equal(connectorUiStep({ active: 'cli' }, 'authorize'), 'qr');
assert.equal(connectorUiStep({ active: 'connect' }, 'register'), 'connect');
assert.equal(connectorUiStep({ active: 'qr' }, 'register'), 'qr');
assert.equal(connectorUiStep(null, 'register'), 'connect');
assert.equal(connectorUiStep({ active: 'cli' }), 'cli');
{
  const failed = applyConnectorFailure(
    { active: 'cli', steps: { runtime: 'done', cli: 'done' } },
    { code: 'auth_failed', message: 'raw backend diagnostic' },
    'authorize',
  );
  assert.equal(failed.phase, 'error');
  assert.equal(failed.errorCode, 'auth_failed');
  assert.equal(failed.steps.cli, 'done');
  assert.equal(failed.steps.qr, 'error');
  assert.equal(Object.hasOwn(failed.steps, 'authorize'), false);
  // The raw diagnostic must not be carried on the flow state at all.
  assert.equal(Object.hasOwn(failed, 'err'), false);
  assert.equal(Object.hasOwn(failed, 'detail'), false);
  assert.equal(Object.hasOwn(failed, 'errStep'), false);
}
{
  const toolStore = source('features/tools/ToolStoreView.jsx');
  // All four flow cards must receive the localized-code dictionary — one card
  // losing the prop would silently degrade that connector's failures to the
  // generic connectionIncomplete fallback.
  assert.ok(
    (toolStore.match(/errors=\{storeCopy\.connectorErrors\}/g) || []).length >= 4,
    'all four flow cards must receive the connectorErrors dictionary',
  );
  assert.match(toolStore, /connectorErrorCopy\(errors, flow\.errorCode\) \|\| errors\.unknown \|\| copy\.connectionIncomplete/);
  // The connected toast must read its copy through the latest ref at event
  // time, so a mid-flow language switch shows the toast in the new language
  // (the ref effect has no dep array and is declared before the listeners).
  assert.match(toolStore, /const toastCopyRef = useRef\(null\);/);
  assert.match(toolStore, /toastCopyRef\.current = \{ doneTitle, enabledSubtitle: detailCopy\.actions\.enabled \};/);
  assert.match(toolStore, /toastCopyRef\.current && toastCopyRef\.current\.doneTitle/);
  // The auto-collapse timer must only close a card that already reached
  // 'done': a stale timer from a previous round would destroy a freshly
  // re-opened card and strand the busy slot with it.
  assert.ok(
    (toolStore.match(/setTimeout\(\(\) => conn\.setFlow\(f => \(f && f\.phase === 'done' \? null : f\)\), 1800\)/g) || []).length >= 2,
    'both connected listeners must guard the auto-collapse against a re-opened card',
  );
  assert.match(toolStore, /applyConnectorFailure\(f, p, p\.phase\)/);
  assert.doesNotMatch(toolStore, /\berr:\s*String\(/);
  assert.doesNotMatch(toolStore, /flow\.err\b/);
  assert.doesNotMatch(toolStore, /flow\.detail\b/);
  assert.doesNotMatch(toolStore, /console\.error\([^\n]*connect failed:[^\n]*,\s*e\)/);
  const tauriSource = relative => readFileSync(new URL(`../src-tauri/src/${relative}`, import.meta.url), 'utf8');
  const feishu = tauriSource('features/connectors/feishu.rs');
  assert.match(feishu, /"phase": "register", "code": "registration_failed"/);
  assert.match(feishu, /"phase": "authorize", "code": "auth_failed"/);
  for (const connector of ['wecom.rs', 'tmeet.rs']) {
    assert.match(tauriSource(`features/connectors/${connector}`), /"phase": "authorize", "code": "auth_failed"/, `${connector} error events must carry an error code`);
  }
  // DingTalk's error code travels on its FlowError (the org-level CLI block
  // gets its own `cli_data_access_disabled` code; everything else defaults to
  // `auth_failed`), so pin the emit's code field plus the default category.
  const dingtalk = tauriSource('features/connectors/dingtalk.rs');
  assert.match(dingtalk, /"phase": "authorize", "code": e\.code/, 'dingtalk error events must carry the FlowError code');
  assert.match(dingtalk, /code: "auth_failed"/, 'dingtalk FlowError must default to the auth_failed category');
  assert.match(dingtalk, /code: "cli_data_access_disabled"/, 'dingtalk org CLI block must carry its own stable code');
  // Source contract: every connector's error arm must consult BOTH the cancel
  // flag and the generation (cancel alone is cleared by the next reset), and
  // each file must gate at least two emit paths on the generation (the error
  // emit plus the QR emit) — deleting either wiring must fail here, not just
  // the mechanism unit test.
  for (const connector of ['feishu.rs', 'wecom.rs', 'dingtalk.rs', 'tmeet.rs']) {
    const src = tauriSource(`features/connectors/${connector}`);
    assert.match(src, /conn\.is_cancelled\(ID\)\s*\|\|\s*conn\.flow_stale\(ID, generation\)/, `${connector} error arm must guard on cancel OR a superseded generation`);
    assert.ok(
      (src.match(/flow_stale\(ID, generation\)/g) || []).length >= 2,
      `${connector} must consult the generation on both the error and the QR emit path`,
    );
  }
  // The wrapped QR emits (feishu register + authorize / dingtalk / tmeet) gate
  // on the same pair. Feishu has two QR phases, so require two wraps there:
  // the authorize emit once shipped with only a pre-parse guard, and a cancel
  // landing inside parse + QR rendering could still paint a dead device code
  // onto a newer round's card.
  assert.match(tauriSource('features/connectors/dingtalk.rs'), /!\(conn\.is_cancelled\(ID\) \|\| conn\.flow_stale\(ID, generation\)\)/);
  assert.match(tauriSource('features/connectors/tmeet.rs'), /!\(conn\.is_cancelled\(ID\) \|\| conn\.flow_stale\(ID, generation\)\)/);
  assert.ok(
    (feishu.match(/!\(conn\.is_cancelled\(ID\) \|\| conn\.flow_stale\(ID, generation\)\)/g) || []).length >= 2,
    'both feishu QR emits (register and authorize) must be wrapped in the cancel-or-stale guard at the emit site',
  );
  // Frontend: a late error event must not fabricate a flow card from null.
  assert.match(toolStore, /f \? applyConnectorFailure\(f, p, p\.phase\) : f\)/);
  // Frontend: the same guard family on the remaining fabrication points — a
  // late QR event, a connect-catch rejection after the card was closed, and
  // the connected listeners' done write (the smoke only exercises the error
  // path end-to-end, so these three are pinned at source level).
  assert.match(toolStore, /if \(!f\) return f;/, 'the qr listener must drop events onto a closed flow instead of fabricating a card');
  assert.match(toolStore, /f \? applyConnectorFailure\(f, e, stage\) : f\)/, 'the connect catch must not fabricate a card from a closed flow');
  assert.ok(
    (toolStore.match(/f \? \{ \.\.\.f, phase: 'done'/g) || []).length >= 2,
    'both connected listeners must guard the done write against a closed flow',
  );
  // Source contract: every pid-slot cleanup must be the compare-and-set. A
  // superseded round's plain set_pid(ID, None) would clear the pid a newer
  // round registered, making its cancel tree-kill a no-op (connector_cli.rs
  // unit-tests the CAS mechanism; this pins that every flow uses it).
  for (const connector of ['feishu.rs', 'wecom.rs', 'dingtalk.rs', 'tmeet.rs']) {
    const src = tauriSource(`features/connectors/${connector}`);
    assert.doesNotMatch(src, /set_pid\(ID, None\)/, `${connector} pid cleanup must use the clear_pid_if compare-and-set`);
    assert.match(src, /let pid = child\.id\(\);/, `${connector} must capture its child pid for the CAS clear`);
    assert.match(src, /clear_pid_if\(ID, pid\)/, `${connector} must clear the pid slot via clear_pid_if`);
  }
  // Source contract: a reconnect without an intervening cancel must not orphan
  // the previous round's registered child — begin's reset overwrites the pid
  // slot, which would make that child invisible to cancel's tree-kill and to
  // kill_all_pids at exit. Every begin therefore tree-kills the registered pid
  // before resetting (the smoke cannot exercise real child processes, so this
  // is pinned at source level like the other begin-time wiring).
  for (const connector of ['feishu.rs', 'wecom.rs', 'dingtalk.rs', 'tmeet.rs']) {
    const src = tauriSource(`features/connectors/${connector}`);
    assert.match(src, /if let Some\(pid\) = conn\.cancel\(ID\)/, `${connector} connect_begin must take the previous round's registered pid before reset`);
    assert.match(src, /kill_pid_tree\(pid\)/, `${connector} connect_begin must tree-kill the stale child`);
  }
  assert.match(source('features/settings/SettingsView.jsx'), /item\.title \|\| presetProviderLabel\(p, t\)/);
}

console.log('UI language coverage tests passed');
