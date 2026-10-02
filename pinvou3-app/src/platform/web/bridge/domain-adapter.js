/**
 * Adapt the Web transport to the same domain API and state slices consumed by
 * the desktop UI. The legacy flat object stays private to this platform layer.
 */
(function () {
  "use strict";

  var platform = window.PinvouPlatform;
  if (!platform || (platform.kind !== "web" && platform.isWeb !== true)) return;

  var flat = window.TauriBridge;
  if (!flat || !flat.available || typeof flat.getState !== "function") return;

  const fields = {
    platform: ["appVersion", "backendOnline", "platformCapabilities"],
    sessions: ["sessions", "archivedSessions", "activeSessionId", "sessionBusy", "draftEpoch", "draftWorkspacePath"],
    chat: ["activeSkill", "artifacts", "artifactChange", "attachments", "busy", "chatItems", "composerDraft", "composerPrefill", "messages", "modeState", "planSnapshot", "queued", "thinking", "tokens", "turnDirtyArtifacts", "turnPresentedArtifacts", "turnTimeline"],
    voice: ["voiceInput", "voiceAsrSetup"],
    knowledge: ["kbModelSetup", "mountedCollection", "mountedCollections", "mountedCollectionsRevision"],
    scheduled: ["scheduledRunContext", "scheduledTaskAutoOpenId", "scheduledTaskBusyAction", "scheduledTaskCreationSessionId", "scheduledTaskDetail", "scheduledTaskDraft", "scheduledTaskError", "scheduledTaskErrorKind", "scheduledTaskLoading", "scheduledTaskPendingGuide", "scheduledTaskRecentRuns", "scheduledTaskRuns", "scheduledTasks", "scheduledTaskSelectionGeneration", "selectedScheduledTaskId"],
    monitor: ["monitor", "monitorError"],
    settings: ["settings", "selectedPet"],
    models: ["activeModelId", "currentSessionModelId", "effectiveModelConfig", "savedModels"],
    llmapi: ["llmApiModels", "llmApiStatus"],
    vllm: [],
    interaction: ["pinvouModal", "pinvouReviews", "pinvouSummoning", "superPermEnabled"],
    computerUse: ["computerUse"],
    personas: ["activePersona", "personaEvents", "personaPool"],
    memory: ["memory"],
    remoteControl: ["webAccess"],
    // projects 域为桌面专属(整域不出现在 bridgeDomainContract 的 Web 域面),
    // 但 APP_BRIDGE_STATE_DOMAINS 的订阅列表双端共享;Web 端补一个空桩
    // (没有 bridge.projects 方法面,列表恒空),防止启动期 getMany 抛
    // "Unknown Tauri bridge state slice: projects"(栈内 #448 的根因)。
    projects: ["projectsList"],
    // desktopRemoteControl 整域桌面专属（Windows 双端远控）：Web 端补空桩，
    // 字段集必须与桌面 STATE_SLICE_FIELDS 完全一致（见 bridge_state_domains）。
    desktopRemoteControl: [
      "controlledDevice",
      "controllerConnection",
      "remoteCapabilities",
      "remoteModels",
      "remoteHistories",
      "tasks"
    ],
    updater: ["updateCancelling", "updateCheckError", "updateChecking", "updateDownloading", "updateError", "updateInfo", "updateProgress", "updateReady"],
    dependencies: ["deps", "depsChecking", "depsInstallError", "depsInstalling"]
    ,teamRelay: ["teamRelay"]
  };

  function clone(value) {
    if (typeof structuredClone === "function") {
      try { return structuredClone(value); } catch (_) {} // safari14-ok: typeof-guarded with JSON fallback
    }
    return JSON.parse(JSON.stringify(value));
  }

  function pick(full, domainName) {
    var names = fields[domainName];
    if (!names) throw new Error("Unknown Tauri bridge state slice: " + domainName);
    var result = {};
    names.forEach(function (name) { result[name] = full[name]; });
    return result;
  }

  // The Web transport structurally shares unchanged subtrees between its
  // immutable snapshots. Compare only this domain's registered fields so a
  // chat publication does not hand settings/models subscribers a fresh outer
  // object and force an unrelated React render. Callbacks still run for every
  // transport publication, matching the desktop bridge contract; React can
  // bail out through the stable snapshot identity.
  function stablePick(domainName) {
    var names = fields[domainName];
    if (!names) throw new Error("Unknown Tauri bridge state slice: " + domainName);
    var lastSlice = null;
    return function (full) {
      if (lastSlice) {
        var isUnchanged = true;
        for (var fieldIndex = 0; fieldIndex < names.length; fieldIndex++) {
          var name = names[fieldIndex];
          if (!Object.is(full[name], lastSlice[name])) {
            isUnchanged = false;
            break;
          }
        }
        if (isUnchanged) return lastSlice;
      }
      lastSlice = Object.freeze(pick(full, domainName));
      return lastSlice;
    };
  }

  function get(domainName) {
    return clone(pick(flat.getState(), domainName));
  }

  function getMany(domains) {
    if (!Array.isArray(domains) || domains.length === 0) throw new Error("Tauri bridge state.getMany requires at least one domain");
    var full = flat.getState();
    var result = {};
    domains.forEach(function (domainName) { Object.assign(result, pick(full, domainName)); });
    return clone(result);
  }

  function subscribe(domainName, callback) {
    get(domainName);
    var stable = stablePick(domainName);
    return flat.subscribe(function (full) {
      callback(stable(full));
    });
  }

  function subscribeMany(domains, callback) {
    getMany(domains);
    var stables = {};
    domains.forEach(function (domainName) { stables[domainName] = stablePick(domainName); });
    var lastSlices = [];
    var lastResult = null;
    return flat.subscribe(function (full) {
      var hasChanged = !lastResult;
      domains.forEach(function (domainName, index) {
        var slice = stables[domainName](full);
        if (slice !== lastSlices[index]) {
          lastSlices[index] = slice;
          hasChanged = true;
        }
      });
      if (hasChanged) {
        var result = {};
        for (var sliceIndex = 0; sliceIndex < lastSlices.length; sliceIndex++) {
          Object.assign(result, lastSlices[sliceIndex]);
        }
        lastResult = Object.freeze(result);
      }
      callback(lastResult);
    });
  }

  function domain(names, aliases) {
    var result = {};
    names.forEach(function (name) { if (typeof flat[name] === "function") result[name] = flat[name]; });
    Object.keys(aliases || {}).forEach(function (name) {
      var fn = flat[aliases[name]];
      if (typeof fn === "function") result[name] = fn;
    });
    return result;
  }

  function unsupportedTeamRelay() {
    return Promise.reject(new Error("Team Relay is only available in the Pinvou desktop app"));
  }
  // Computer use controls the desktop's own screen, mouse and keyboard, so it
  // must never be driven from the remote web client (the desktop RPC funnel
  // keeps the commands off the access-policy allowlist as well). The domain
  // still exists with rejecting stubs so shared UI code can feature-detect it.
  function computerUseUnsupported() {
    return Promise.reject(new Error("computer use is not supported on the web client"));
  }
  const computerUseStubs = {};
  ["getStatus", "refreshStatus", "grant", "revoke", "stop", "confirm", "deny", "setEnabled", "requestPermissions"]
    .forEach(function (name) { computerUseStubs[name] = computerUseUnsupported; });

  window.TauriBridge = {
    available: true,
    lifecycle: { init: flat.init },
    state: { get: get, getMany: getMany, subscribe: subscribe, subscribeMany: subscribeMany },
    platform: {},
    chat: domain(["sendMessage", "sendMessageToSession", "getComposerDraft", "setComposerDraft", "retryFirstTurn", "prefillComposer", "restoreTaskDraft", "removeQueued", "prioritizeQueued", "editQueued", "cancelGeneration", "cancelShellTask"]),
    voice: domain(["startVoiceInput", "installVoiceAsr", "cancelVoiceAsrSetup", "closeVoiceAsrSetup", "cancelVoiceInput", "clearVoiceInput", "abandonVoiceResult", "appendVoiceText", "getVoiceOperationId", "beginVoiceSubmission", "completeVoiceSubmission", "dismissVoiceInput", "hasVoiceSubmissionPending"]),
    knowledge: domain(["downloadKbModel", "cancelKbModel", "mountCollection", "setCollectionEnabled", "removeCollection", "unmountCollection", "listCollections", "kbModelStatus"]),
    scheduled: domain(["loadScheduledTasks", "readScheduledTask", "loadScheduledTaskRuns", "loadScheduledTaskRecentRuns", "selectScheduledTask", "refreshScheduledTaskData", "clearScheduledTaskSelection", "dismissScheduledTaskError", "createScheduledTask", "updateScheduledTask", "pauseScheduledTask", "resumeScheduledTask", "toggleScheduledTaskPinned", "deleteScheduledTask", "runScheduledTaskNow", "pickFolder", "startScheduledTaskChat", "confirmScheduledTaskDraft", "clearScheduledTaskDraft", "openScheduledRunChat", "exitScheduledRunChat"]),
    sessions: domain(["createNewSession", "materializeSession", "switchToSession", "deleteSession", "renameSession", "toggleSessionPinned", "archiveSession", "restoreArchivedSession", "getSessionWorkspaceBinding"]),
    monitor: domain(["startMonitorPolling", "stopMonitorPolling", "clearMonitorStats"]),
    settings: domain(["loadBehaviorTelemetryStatus", "setBehaviorTelemetryEnabled", "setSelectedPet", "saveSettings", "saveSettingsAndRestart", "saveSearchSettings", "saveSearchSettingsAndRestart", "testSearchProvider"]),
    feedback: domain(["submitFeedback"]),
    llmapi: {},
    vllm: domain(["discoverLocalVllm"]),
    models: domain(["getEffectiveModelConfig", "loadModels", "saveModel", "revealModelApiKey", "deleteModel", "setActiveModel", "loadSessionModel", "switchModel", "testModelConnection", "getImageInputCapability", "testImageInputCapability", "probeLocalServerKind"]),
    interaction: domain(["toggleSuperPerm", "acceptPlan", "discardPlan", "exitPlanToYolo", "setPlanModeNext", "setDraftMode", "setModeLane", "refreshModeDefaults", "getCodePermissionPrefs", "confirmCodeYolo", "syncModeState", "planStuckReplan", "planStuckGo", "submitUserInput", "cancelUserInput", "summonPinvou", "inspectPinvou", "resolvePinvouReview", "dismissPinvouReview", "editLastTurn", "compactNow"]),
    rendering: domain(["renderMarkdown"]),
    remoteControl: domain(["getWebRelaySettings", "setWebRelayAddress", "resetWebRelayAddress"], {
      startRemoteControl: "enableWebAccess",
      stopRemoteControl: "disableWebAccess",
      refreshRemoteControlQr: "rotateWebAccessLink",
      refreshRemoteControlStatus: "refreshWebAccessStatus"
    }),
    artifacts: domain(["artifactInfo", "readArtifactText", "writeArtifactText", "readArtifactImageB64", "readArtifactThumbnail", "renderArtifactVisual", "openContainingFolder", "revealSessionFolder", "openScheduledTaskFolder", "openInSystem", "openArtifactExternal", "downloadArtifact", "listDeliverableIndex", "openExternalUrl", "openUserExternalUrl"]),
    attachments: domain(["addAttachmentByPath", "addPasteImage", "removeAttachment", "clearAttachments", "pickAndAttach", "uploadDeviceFiles", "resolveConversationAttachment", "openConversationAttachment", "revealConversationAttachment"]),
    resolutions: domain(["markResolved"]),
    files: domain(["pickFiles", "pickFolders", "pickRebindFolder", "pickFeedbackFiles"]),
    personas: domain(["loadPersonas", "getPersonas", "readPersonaBody", "equipPersona", "unequipPersona", "postCardCreatorIntro", "createPersona", "updatePersona", "deletePersona"]),
    memory: domain(["loadMemoryOverview", "saveMemoryProfilePatch", "deleteMemoryPreference", "updateMemoryItem", "deleteMemoryItem", "archiveRecentWorkMemory", "confirmMemoryCandidate", "ignoreMemoryCandidate", "neverMemoryCandidate", "organizeMemory", "loadOrganizeHistory"]),
    updater: domain(["checkForUpdate", "downloadAndInstallUpdate", "cancelUpdate", "restartApp"]),
    dependencies: domain(["checkDependencies", "installDependencies"]),
    teamRelay: {
      load: unsupportedTeamRelay,
      refresh: unsupportedTeamRelay,
      verifyAccess: unsupportedTeamRelay,
      registerAccount: unsupportedTeamRelay,
      claim: unsupportedTeamRelay,
      createFirstTeam: unsupportedTeamRelay,
      updateProfile: unsupportedTeamRelay,
      avatarSource: unsupportedTeamRelay,
      uploadAvatar: unsupportedTeamRelay,
      removeAvatar: unsupportedTeamRelay,
      createTeam: unsupportedTeamRelay,
      joinTeam: unsupportedTeamRelay,
      switchTeam: unsupportedTeamRelay,
      removeMember: unsupportedTeamRelay,
      transferOwner: unsupportedTeamRelay,
      leaveTeam: unsupportedTeamRelay,
      dissolveTeam: unsupportedTeamRelay,
      createInvite: unsupportedTeamRelay,
      getJoinPassword: unsupportedTeamRelay,
      rotateJoinPassword: unsupportedTeamRelay,
      setJoinPasswordVisibility: unsupportedTeamRelay,
      disableJoinPassword: unsupportedTeamRelay,
      createThread: unsupportedTeamRelay,
      createAction: unsupportedTeamRelay,
      setThreadPreference: unsupportedTeamRelay,
      markThreadRead: unsupportedTeamRelay,
      listDraftSourceSessions: unsupportedTeamRelay,
      readDraftSourceSession: unsupportedTeamRelay,
      mountDraftMaterials: unsupportedTeamRelay,
      pickDraftAttachments: unsupportedTeamRelay,
      downloadAttachment: unsupportedTeamRelay,
      taskWorkspaceStatus: unsupportedTeamRelay,
      prepareTaskWorkspace: unsupportedTeamRelay,
      setTaskAgentSession: unsupportedTeamRelay,
      pickAgentWorkspace: unsupportedTeamRelay,
      openPreparedMaterial: unsupportedTeamRelay,
      revealPreparedMaterial: unsupportedTeamRelay,
      disconnect: unsupportedTeamRelay
    },
    computerUse: computerUseStubs
  };
})();
