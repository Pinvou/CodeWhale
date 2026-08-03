import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dict } from '../src/shared/i18n.js';
import '../src/features/settings/settings-i18n.js';
import { errorCode, localizedErrorMessage } from '../src/shared/user-facing-error.js';
import {
  applyConnectorFailure,
  connectorErrorCodeForStep,
  connectorFailure,
  connectorUiStep,
  localizeConnectorAlert,
} from '../src/features/tools/connector-ui-state.js';

const source = relative => readFileSync(new URL(`../src/${relative}`, import.meta.url), 'utf8');
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
  assert.ok(dict[language].expertPoolIndividualTab, `${language}.expertPoolIndividualTab must exist`);
  assert.ok(dict[language].expertPoolTeamTab, `${language}.expertPoolTeamTab must exist`);
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
    'uiWorkflow',
    'uiToolStore',
    'uiPet',
    'uiWebConnection',
    'uiConversation',
    'uiHomeMode',
    'uiAttachments',
    'uiCodex',
    'uiCodexView',
    'uiCodexWorkspace',
    'uiArtifacts',
    'uiToolDetails',
  ]) {
    assert.ok(dict[language][section], `${language}.${section} must exist`);
  }
  assert.ok(dict[language].uiScheduled.createFromTemplate, `${language}.uiScheduled.createFromTemplate must exist`);
  assert.ok(dict[language].uiScheduled.runHistory, `${language}.uiScheduled.runHistory must exist`);
  assert.ok(dict[language].uiSettingsDetail.restartNow, `${language}.uiSettingsDetail.restartNow must exist`);
  assert.ok(dict[language].uiSettingsDetail.deleteModelTitle, `${language}.uiSettingsDetail.deleteModelTitle must exist`);
  assert.ok(dict[language].uiChat.asrDownloadTitle, `${language}.uiChat.asrDownloadTitle must exist`);
  assert.ok(dict[language].uiChat.memoryMeta.preference, `${language}.uiChat.memoryMeta.preference must exist`);
  assert.ok(dict[language].uiChat.sceneModes.documentWriting, `${language}.uiChat.sceneModes.documentWriting must exist`);
  assert.ok(dict[language].uiChat.sceneModes.poster, `${language}.uiChat.sceneModes.poster must exist`);
  assert.ok(dict[language].uiChat.sceneModes.dataVisualization, `${language}.uiChat.sceneModes.dataVisualization must exist`);
  assert.ok(dict[language].uiChat.sceneModes.pptDesign, `${language}.uiChat.sceneModes.pptDesign must exist`);
  assert.ok(dict[language].uiChat.sceneModes.pptUnavailable, `${language}.uiChat.sceneModes.pptUnavailable must exist`);
  assert.ok(dict[language].uiChat.sceneModes.designGeneralPlaceholder, `${language}.uiChat.sceneModes.designGeneralPlaceholder must exist`);
  assert.equal(
    typeof dict[language].uiChat.sceneModes.clear,
    'function',
    `${language}.uiChat.sceneModes.clear must be a function`,
  );
  assert.ok(dict[language].uiChatExtra.draftingScheduled, `${language}.uiChatExtra.draftingScheduled must exist`);
  assert.ok(dict[language].uiChatScenes.attachmentPrompt, `${language}.uiChatScenes.attachmentPrompt must exist`);
  assert.ok(dict[language].uiAccount.availableQuota, `${language}.uiAccount.availableQuota must exist`);
  assert.ok(dict[language].uiAccount.settingsLoadFailed, `${language}.uiAccount.settingsLoadFailed must exist`);
  assert.ok(dict[language].uiAccount.settingsLoadFailedDetail, `${language}.uiAccount.settingsLoadFailedDetail must exist`);
  assert.ok(dict[language].uiAccount.errors.service_unreachable, `${language}.uiAccount.errors.service_unreachable must exist`);
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
  assert.ok(dict[language].uiPlatformMisc.tauriBridge.builtinModelUnavailable, `${language}.uiPlatformMisc.tauriBridge.builtinModelUnavailable must exist`);
}

const main = source('app/main.jsx');
assert.match(main, /emit\(['"]ui:language_changed['"], \{ language: lang \}\)/);
assert.match(main, /<ToolStoreView[^>]*t=\{t\}/);
assert.match(main, /<WebConnectionStatus[^>]*t=\{t\}/);
assert.match(main, /<AccountPopover[\s\S]{0,300}t=\{t\}/);
assert.match(main, /const accountCopy = t\.uiAccount/);
assert.match(main, /accountCopy\.availableQuota/);
assert.match(main, /accountCopy\.loginAndSync/);
assert.match(main, /setLoginErrorCode\(errorCode\(err\) \|\| 'unknown'\)/);
assert.match(main, /localizedErrorMessage\(\{ code: loginErrorCode \}, accountCopy\.errors, accountCopy\.builtinLoginFailed\)/);
assert.match(main, /localizedErrorMessage\(\{ code: accountErrorCode \}, accountCopy\.errors, accountCopy\.syncFailed\)/);
assert.doesNotMatch(main, /setLoginError\(err && err\.message/);
assert.doesNotMatch(main, /setLlmApiError\(e && e\.message/);
assert.doesNotMatch(main, /console\.error\('llm api (?:quota refresh|login) failed:', e\)/);
assert.match(main, /const refreshLlmApiStatus = useCallback\(async \(\) => \{[\s\S]{0,1000}\}, \[\]\);/);
assert.doesNotMatch(main, /\}, \[t\.uiAccount\.syncFailed\]\);/);
assert.match(main, /formatQuotaTokens\(limit, t\.langTag\)/);
assert.match(main, /t\.uiAccount\.viewQuota/);
assert.doesNotMatch(main, />查看可用额度</);
assert.match(main, /<SettingsErrorBoundary[^>]*t=\{t\}/);
assert.match(main, /<CodexAcpView[^>]*t=\{t\}/);
const settingsErrorBoundary = source('features/settings/SettingsErrorBoundary.jsx');
assert.match(settingsErrorBoundary, /accountCopy\.settingsLoadFailed/);
assert.match(settingsErrorBoundary, /accountCopy\.settingsLoadFailedDetail/);
assert.doesNotMatch(settingsErrorBoundary, /this\.state\.error\.message/);
assert.doesNotMatch(settingsErrorBoundary, />设置页加载失败</);
assert.match(main, /t\.uiScheduled\.runStatus\[value\]/);
assert.doesNotMatch(main, /queued:\s*'等待中'/);
assert.match(main, /window\.PinvouTauriBridgeStrings = misc\.tauriBridge/);

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
const workflow = source('features/workflow/WorkflowView.jsx');
assert.match(workflow, /t\.uiWorkflow/);
assert.match(workflow, /const OTHER_ANSWER_LABEL = 'Other'/);
assert.match(workflow, /label: OTHER_ANSWER_LABEL/);
assert.doesNotMatch(workflow, /label:\s*'其他'/);
const toolStore = source('features/tools/ToolStoreView.jsx');
const connectorSsoDialog = source('features/tools/ConnectorSsoDialog.jsx');
assert.match(toolStore, /const storeCopy = t\.uiToolStore/);
assert.match(toolStore, /localizeTool\(baseTool, t\)/);
assert.match(toolStore, /<ConnectorSsoDialog[\s\S]*copy=\{storeCopy\}/);
assert.match(connectorSsoDialog, /copy\.sso\.title/);
assert.match(toolStore, /storeCopy\.internalDetailDesc/);
assert.match(toolStore, /localizeConnectorAlert\(alert, storeCopy, detailCopy\)/);
assert.match(toolStore, /errors=\{storeCopy\.connectorErrors\}/);
assert.match(toolStore, /errors\[flow\.errorCode\]/);
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
const settings = source('features/settings/SettingsView.jsx');
assert.match(settings, /t\.uiSettings/);
assert.match(settings, /const settingsCopy = t\.uiSettingsDetail/);
assert.match(settings, /settingsCopy\.addSearch/);
assert.match(settings, /settingsCopy\.deleteModelTitle/);
assert.match(settings, /item\.title \|\| presetProviderLabel\(p, t\)/);
assert.doesNotMatch(settings, />添加搜索源</);
const chat = source('features/chat/ChatView.jsx');
assert.match(chat, /const chatCopy = t\.uiChat/);
assert.match(chat, /chatCopy\.asrDownloadTitle/);
assert.match(chat, /chatCopy\.memoryMeta/);
assert.match(chat, /chatCopy\.sceneModes/);
assert.match(chat, /sceneCopy\.designGeneralPlaceholder/);
assert.match(chat, /outgoing \|\| t\.uiChatScenes\.attachmentPrompt/);
assert.doesNotMatch(chat, /outgoing \|\| '请根据附件内容继续处理。'/);
assert.doesNotMatch(chat, /label:\s*'公文写作'/);
assert.doesNotMatch(chat, /label:\s*'数据可视化'/);
assert.doesNotMatch(chat, /`取消\$\{scene\.label\}`/);
assert.doesNotMatch(chat, /:\s*'描述你想生成或调整的内容'/);
assert.doesNotMatch(chat, />下载语音识别模型</);
const tauriSettingsBridge = source('platform/tauri/bridge/settings.js');
assert.match(tauriSettingsBridge, /root\.PinvouTauriBridgeStrings/);

assert.equal(errorCode({ code: 'sso_timeout', message: '原始诊断' }), 'sso_timeout');
assert.equal(errorCode(new Error('service_unreachable: raw backend detail')), 'service_unreachable');
assert.equal(errorCode(new Error('raw backend detail')), '');
assert.equal(
  localizedErrorMessage(
    new Error('service_unreachable: raw backend detail'),
    dict.ja.uiAccount.errors,
    dict.ja.uiAccount.syncFailed,
  ),
  dict.ja.uiAccount.errors.service_unreachable,
);
assert.equal(
  localizedErrorMessage(
    new Error('未知的底层诊断'),
    dict.en.uiAccount.errors,
    dict.en.uiAccount.syncFailed,
  ),
  dict.en.uiAccount.syncFailed,
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
assert.equal(failedAuthorizationFlow.errStep, 'qr');
assert.equal(failedAuthorizationFlow.steps.cli, 'done');
assert.equal(failedAuthorizationFlow.steps.qr, 'error');
assert.equal(Object.hasOwn(failedAuthorizationFlow.steps, 'authorize'), false);
assert.equal(Object.hasOwn(failedAuthorizationFlow, 'errorDetail'), false);
const storedAccountErrorCode = errorCode(new Error('service_unreachable: raw backend detail'));
const englishAccountError = localizedErrorMessage(
  { code: storedAccountErrorCode },
  dict.en.uiAccount.errors,
  dict.en.uiAccount.syncFailed,
);
const japaneseAccountError = localizedErrorMessage(
  { code: storedAccountErrorCode },
  dict.ja.uiAccount.errors,
  dict.ja.uiAccount.syncFailed,
);
assert.equal(englishAccountError, dict.en.uiAccount.errors.service_unreachable);
assert.equal(japaneseAccountError, dict.ja.uiAccount.errors.service_unreachable);
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
for (const connector of ['wecom.rs', 'dingtalk.rs', 'tmeet.rs']) {
  assert.match(tauriSource(`features/connectors/${connector}`), /"code": "auth_failed"/);
}
assert.match(source('features/pet/PetSettingsSection.jsx'), /t\.uiPetSettings/);
const conversation = source('features/conversation/ConversationTimeline.jsx');
assert.match(conversation, /conversationCopy\(copy\)/);
assert.doesNotMatch(conversation, />等待授权</);
const codex = source('features/codex/CodexAcpView.jsx');
assert.match(codex, /const codexCopy = t\.uiCodex/);
assert.match(codex, /copy=\{t\.uiConversation\}/);
assert.match(codex, /copy=\{t\.uiCodexWorkspace\}/);
const workspace = source('features/codex/CodexWorkspacePanel.jsx');
assert.match(workspace, /\{copy\.title\}/);
assert.doesNotMatch(workspace, />工作区</);
const personas = source('features/personas/Personas.jsx');
assert.match(personas, /label: t\.expertPoolIndividualTab/);
assert.doesNotMatch(personas, /expertPoolIndividualTab \|\|/);

console.log('UI language coverage tests passed');
