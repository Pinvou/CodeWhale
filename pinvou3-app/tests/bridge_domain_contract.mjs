export const desktopBridgeApi = {
  lifecycle: ['init'],
  state: ['get', 'getMany', 'subscribe', 'subscribeMany'],
  platform: ['loadPlatformCapabilities', 'refreshConnectorAuthGates'],
  chat: ['cancelGeneration', 'cancelShellTask', 'editQueued', 'getComposerDraft', 'interruptAndSend', 'interruptAndSendQueued', 'prefillComposer', 'prioritizeQueued', 'removeQueued', 'restoreTaskDraft', 'retryFirstTurn', 'sendMessage', 'sendMessageToSession', 'setComposerDraft', 'steer'],
  auxChat: ['discard', 'ensure', 'reset', 'send', 'snapshot'],
  voice: ['abandonVoiceResult', 'appendVoiceText', 'beginVoiceSubmission', 'cancelVoiceAsrSetup', 'cancelVoiceInput', 'clearVoiceInput', 'closeVoiceAsrSetup', 'completeVoiceSubmission', 'dismissVoiceInput', 'getVoiceOperationId', 'hasVoiceSubmissionPending', 'installVoiceAsr', 'setVoiceShortcutEnabled', 'startVoiceInput', 'syncVoiceShortcutRecording'],
  knowledge: ['cancelKbModel', 'downloadKbModel', 'kbModelStatus', 'listCollections', 'loadKnowledgeEmbedderAfterFirstFrame', 'mountCollection', 'mountRemoteCollection', 'removeCollection', 'removeRemoteCollection', 'setCollectionEnabled', 'setRemoteCollectionEnabled', 'unmountCollection'],
  scheduled: ['clearScheduledTaskDraft', 'clearScheduledTaskSelection', 'confirmScheduledTaskDraft', 'createScheduledTask', 'deleteScheduledTask', 'dismissScheduledTaskError', 'exitScheduledRunChat', 'loadScheduledTaskRecentRuns', 'loadScheduledTaskRuns', 'loadScheduledTasks', 'openScheduledRunChat', 'pauseScheduledTask', 'pickFolder', 'readScheduledTask', 'refreshScheduledTaskData', 'resumeScheduledTask', 'runScheduledTaskNow', 'selectScheduledTask', 'startScheduledTaskChat', 'toggleScheduledTaskPinned', 'updateScheduledTask'],
  sessions: ['archiveSession', 'createNewSession', 'deleteSession', 'exportSessionArchive', 'getSessionWorkspaceBinding', 'materializeSession', 'onSessionDeleted', 'pickDraftWorkspace', 'renameSession', 'restoreArchivedSession', 'setDraftWorkspace', 'switchToSession', 'toggleSessionPinned'],
  monitor: ['clearMonitorStats', 'startMonitorPolling', 'stopMonitorPolling'],
  settings: ['listBuiltinFeatures', 'loadBehaviorTelemetryStatus', 'saveSearchSettings', 'saveSearchSettingsAndRestart', 'saveSettings', 'saveSettingsAndRestart', 'setBehaviorTelemetryEnabled', 'setBuiltinFeatureEnabled', 'setSelectedPet', 'testSearchProvider'],
  feedback: ['submitFeedback'],
  llmapi: ['ensureLlmApiBinding', 'getLlmApiAdminOverview', 'getLlmApiModels', 'getLlmApiStatus', 'loginLlmApiUser', 'refreshLlmApi', 'retryLlmApiProvisioning', 'saveLlmApiUserSession', 'setLlmApiDefaultModel', 'setLlmApiUserEnabled'],
  vllm: ['discoverLocalVllm'],
  multiAgent: ['listSubagentTranscripts', 'readSubagentTranscript'],
  models: ['deleteModel', 'getEffectiveModelConfig', 'getImageInputCapability', 'loadModels', 'loadSessionModel', 'probeLocalServerKind', 'revealModelApiKey', 'saveModel', 'setActiveModel', 'switchModel', 'testImageInputCapability', 'testModelConnection'],
  interaction: ['acceptPlan', 'cancelUserInput', 'compactNow', 'confirmCodeYolo', 'discardPlan', 'dismissPinvouReview', 'editLastTurn', 'exitPlanToYolo', 'getCodePermissionPrefs', 'inspectPinvou', 'planStuckGo', 'planStuckReplan', 'refreshModeDefaults', 'resolvePinvouReview', 'setDraftMode', 'setModeLane', 'setMultiAgentMode', 'setPlanModeNext', 'submitUserInput', 'summonPinvou', 'syncModeState', 'toggleSuperPerm'],
  rendering: ['renderMarkdown'],
  remoteControl: ['getWebRelaySettings', 'refreshRemoteControlQr', 'refreshRemoteControlStatus', 'resetWebRelayAddress', 'setWebRelayAddress', 'startRemoteControl', 'stopRemoteControl'],
  // Windows 双端远程控制（B 被控端 / A 主控端）。与 webAccess 完全不同域：
  // 只暴露 opaque ID、状态投影与任务编排，模型凭证、路径和密钥都留在 Rust。
  desktopRemoteControl: [
    'cancelRemoteTask', 'controlledAuthorizeWorkspace', 'controlledDisconnectNow',
    'controlledListWorkspaces', 'controlledRefresh', 'controlledStart', 'controlledStatus', 'controlledStop',
    'controllerConnect', 'controllerCreateHistory', 'controllerDisconnect', 'controllerListHistory', 'controllerStatus',
    // A07 has no authenticated model/permission/history/task/transfer ingress yet.
    'isAvailable', 'journalAcknowledge', 'journalReplay', 'journalSnapshot',
    'remoteTaskStatus',
  ],
  artifacts: ['artifactInfo', 'downloadArtifact', 'listDeliverableIndex', 'openArtifactExternal', 'openContainingFolder', 'openExternalUrl', 'openInSystem', 'openScheduledTaskFolder', 'openUserExternalUrl', 'readArtifactImageB64', 'readArtifactText', 'readArtifactThumbnail', 'renderArtifactVisual', 'revealSessionFolder', 'writeArtifactText'],
  attachments: ['addAttachmentByPath', 'addPasteImage', 'addPasteImageFromClipboard', 'clearAttachments', 'openConversationAttachment', 'pickAndAttach', 'removeAttachment', 'resolveConversationAttachment', 'revealConversationAttachment', 'uploadDeviceFiles'],
  resolutions: ['markResolved'],
  files: ['pickFeedbackFiles', 'pickFiles', 'pickFolders', 'pickRebindFolder'],
  personas: ['createPersona', 'deletePersona', 'equipPersona', 'getPersonas', 'loadPersonas', 'postCardCreatorIntro', 'readPersonaBody', 'unequipPersona', 'updatePersona'],
  memory: ['archiveRecentWorkMemory', 'confirmMemoryCandidate', 'deleteMemoryItem', 'deleteMemoryPreference', 'ignoreMemoryCandidate', 'loadMemoryOverview', 'loadOrganizeHistory', 'neverMemoryCandidate', 'organizeMemory', 'saveMemoryProfilePatch', 'updateMemoryItem'],
  updater: ['cancelUpdate', 'checkForUpdate', 'downloadAndInstallUpdate', 'restartApp'],
  dependencies: ['checkDependencies', 'installDependencies'],
  teamRelay: ['avatarSource', 'claim', 'createAction', 'createFirstTeam', 'createInvite', 'createTeam', 'createThread', 'disableJoinPassword', 'disconnect', 'dissolveTeam', 'downloadAttachment', 'getJoinPassword', 'joinTeam', 'leaveTeam', 'listDraftSourceSessions', 'load', 'markThreadRead', 'mountDraftMaterials', 'openPreparedMaterial', 'pickAgentWorkspace', 'pickDraftAttachments', 'prepareTaskWorkspace', 'readDraftSourceSession', 'refresh', 'registerAccount', 'removeAvatar', 'removeMember', 'revealPreparedMaterial', 'rotateJoinPassword', 'setJoinPasswordVisibility', 'setTaskAgentSession', 'setThreadPreference', 'switchTeam', 'taskWorkspaceStatus', 'transferOwner', 'updateProfile', 'uploadAvatar', 'verifyAccess'],
  projects: ['createProject', 'deleteProject', 'loadProjects', 'moveSessionToProject', 'rebindWorkspaceRoot', 'renameProject'],
  // Computer use drives the local machine: the desktop backend exposes it, the
  // web surface carries only rejecting stubs (the RPC allowlist excludes the
  // commands entirely, same policy as browser:*).
  computerUse: ['confirm', 'deny', 'getStatus', 'grant', 'refreshStatus', 'requestPermissions', 'revoke', 'setEnabled', 'stop'],
};

// These methods intentionally depend on desktop lifecycle or local machine
// resources. Web may omit them, but every other desktop method must exist.
export const desktopOnlyBridgeApi = {
  platform: ['loadPlatformCapabilities', 'refreshConnectorAuthGates'],
  voice: ['setVoiceShortcutEnabled', 'syncVoiceShortcutRecording'],
  knowledge: ['loadKnowledgeEmbedderAfterFirstFrame', 'mountRemoteCollection', 'removeRemoteCollection', 'setRemoteCollectionEnabled'],
  llmapi: ['ensureLlmApiBinding', 'getLlmApiAdminOverview', 'getLlmApiModels', 'getLlmApiStatus', 'loginLlmApiUser', 'refreshLlmApi', 'retryLlmApiProvisioning', 'saveLlmApiUserSession', 'setLlmApiDefaultModel', 'setLlmApiUserEnabled'],
  // 多智能体开关是桌面专属操作（ADR-0006）：Web 端只读呈现。
  interaction: ['setMultiAgentMode'],
  // Draft workspace selection needs the system directory dialog (TAURI.dialog)
  // and the create_session workspacePath parameter channel; the Web side has no
  // such backend, so ChatView's method-existence guard skips rendering the
  // selector. Session archive export likewise writes local-disk data through a
  // native save dialog, so all three methods share one desktop-only entry.
  sessions: ['exportSessionArchive', 'pickDraftWorkspace', 'setDraftWorkspace'],
  // The steer channel (remote control / other hosts) and the queued chip's
  // zap-send go through the foundation EnginePool and need the Tauri command
  // channel; web has no such backend.
  chat: ['steer', 'interruptAndSend', 'interruptAndSendQueued'],
  // Clipboard-image paste fallback reads the machine the app window runs on.
  // In a web relay session that is the remote desktop, not the browser user's
  // machine — pasting must stay on the browser's own clipboardData there.
  attachments: ['addPasteImageFromClipboard'],
  // Builtin feature toggles (list_builtin_features / set_builtin_feature_enabled)
  // are desktop Rust command channels with no web backend. list_builtin_features
  // is consumed by ChatView (the session-mention feature gate); the enable
  // switch is the contract hook for the builtin plugin page.
  // saveSearchSettingsAndRestart stays web-expected: Official's dual web host
  // exposes the method (its stub warns and returns false on IS_WEB), so the
  // web surface keeps requiring it.
  settings: ['listBuiltinFeatures', 'setBuiltinFeatureEnabled'],
};

// 整域桌面专属：Web 端连域都不存在（区别于 platform 这类"空域仍在"）。
// 后端 remote_control 漏斗另有权威封禁。projects 是本地会话归档分组
// （~/.pinvou3/projects/projects.json），Web 端没有对应后端。
export const desktopOnlyBridgeDomains = ['multiAgent', 'projects', 'desktopRemoteControl'];

export function expectedWebBridgeApi() {
  return Object.fromEntries(
    Object.entries(desktopBridgeApi)
      .filter(([domain]) => !desktopOnlyBridgeDomains.includes(domain))
      .map(([domain, methods]) => {
        const desktopOnly = new Set(desktopOnlyBridgeApi[domain] || []);
        return [domain, methods.filter(method => !desktopOnly.has(method))];
      }),
  );
}
