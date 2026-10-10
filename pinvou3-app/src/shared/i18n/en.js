// English UI dictionary. Loaded lazily via ensureLanguage("en"). Must cover every zh
// leaf key — enforced by the parity assertion in tests/ui_language_coverage.test.mjs.
// ja falls back to en via the spread inside i18n/ja.js, so a missing en key breaks both.
// The i18n.js aggregate shim and Node contract tests (ui_language_coverage) statically import all three;
// the split only affects browser chunks.

/* eslint-disable sonarjs/no-nested-template-literals, sonarjs/no-unenclosed-multiline-block -- compact i18n dictionary: nested templates mirror the copy structure one-to-one; credential field names are false-positive secrets */
import { browserEn } from './browser.js';
import { CHAT_RECOVERY_COPY } from './chat-recovery.js';

const conversationEn = {
  completed:'Completed', failed:'Failed', interrupted:'Interrupted', cancelled:'Cancelled', limitReached:'Limit reached', processing:'Processing', processingActive:'Processing',
  waitingPermission:'Waiting for permission', waitingInput:'Waiting for your input', waitingInputShort:'Waiting for input',
  goLatest:label=>`${label}. Go to latest message`,
  elapsed:milliseconds=>{ const seconds=Math.max(0,Math.floor(milliseconds/1000)); if(seconds<60)return `${seconds}s`; const minutes=Math.floor(seconds/60); const remaining=seconds%60; return remaining?`${minutes}m ${remaining}s`:`${minutes}m`; },
  segments:count=>`${count} commands`, running:'Running', executionFailed:'Execution failed', executionFinished:'Finished',
  command:'Command', workingDirectory:'Working directory', output:'Output', webContent:'Web content', iwencaiNews:'iWenCai News', webSearch:'Web search', search:'Search', textContent:'Text', webPage:'Web page', shellCommand:'Run shell command',
  results:count=>`${count} results`, recognizedResults:count=>`${count} results found`, returnedResults:'Results returned', inProgress:'In progress',
  searchCompacted:'Search results were passed to the Agent; no stable entries could be extracted from the compacted result.',
  resultSummaryOnly:'Only recognizable result summaries are shown to reduce context usage.', collapseRaw:'Hide raw data', viewRaw:'View raw data', rawData:'Raw data',
  requestFailed:'Request failed', returned:'Returned', contentTruncated:'Content truncated', contentPreview:'Content preview',
  responseTruncated:'The response exceeded the fetch limit. The Agent received the truncated content.',
  fileChange:'File change', tool:'Tool', arguments:'Arguments', result:'Result', executing:'Executing', executionSteps:'Execution steps',
  items:count=>`${count} items`, failedItems:count=>`${count} failed`, thinking:'Thinking', thoughtCompleted:'Thinking complete', plan:'Plan',
  permissionRequest:agent=>`${agent} requests permission`, protectedOperation:'Run a protected operation', operationArguments:'Operation arguments',
  allowOnce:'Allow once', allowSession:'Allow for this session', reject:'Reject', handled:'Handled', expired:'This request has expired',
  usage:(input,output)=>`Input ${input} · Output ${output}`, contextUsage:(used,size)=>`Context ${used} / ${size}`,
  attachment:'Attachment', operations:(count,failedCount)=>`${count} operations${failedCount?` · ${failedCount} failed`:''}`, copyReply:'Copy response', copyReplySuccess:'Copied', copyReplyFailed:'Copy failed',
  exportReply:'Export', exportReplyTitle:'Export response', exportMarkdown:'Markdown document', exportMarkdownHint:'Default; preserves source formatting', exportHtml:'HTML document', exportHtmlHint:'Best for viewing in a browser', exportSuccess:'Exported', exportFailed:'Export failed', exportTooLarge:'The response exceeds the export size limit',
  shareReply:'Share', shareReplyTitle:'Share response', shareSystem:'System share', shareSystemHint:'Choose an installed communication app', shareApps:'Copy content and open app', shareSuccess:'Shared', shareCopiedOpen:app=>`Content copied. Tried to open ${app}; paste there to send.`, shareCopiedWeb:app=>`Content copied. Open ${app} and paste to send.`, shareFailed:'Share failed', shareUnavailable:'System share is unavailable; content was copied', shareTargets:{wechat:'WeChat',wecom:'WeCom',feishu:'Feishu',dingtalk:'DingTalk',qq:'QQ'},
  technicalDetails:'Technical details',
};

const codexEn = {
  unknownDirectory:'Unknown directory', temporaryWorkspace:'Temporary workspace', projectMissing:'Original project directory no longer exists',
  applyingConfig:'Applying configuration…', stop:'Stop', workspace:'Workspace', workspaceTitle:'View Agent workspace files and changes',
  branches:'Branches', branchTooltip:'View and switch Git branches',
  branchSwitchDirtyConfirm:(branch, count)=>`The workspace has ${count} uncommitted change(s). Choose how to switch to "${branch}":`,
  branchSwitchCancel:'Cancel',
  branchSwitchCarry:'Carry changes', branchSwitchCarryHint:'Carry changes over to the target branch; if they conflict, the switch fails and your changes stay untouched',
  branchSwitchStash:'Stash and switch', branchSwitchStashHint:'Stash changes first (git stash -u) and restore them automatically after switching; the stash is kept if the restore conflicts',
  branchSwitchCommit:'Commit and switch', branchSwitchCommitHint:'Commit all changes (including untracked files) on the current branch, then switch',
  branchSwitchCommitPlaceholder:'Commit message (required)',
  branchSwitchCommitPrompt:(branch)=>`Commit all uncommitted changes on the current branch, then switch to "${branch}":`,
  branchSwitchBusyError:'The Agent is running. Wait for the current task to finish before switching branches',
  recreatePrefix:'The original project directory no longer exists. ', recreate:'Recreate session',
  welcomeTitle:'Use a coding Agent for coding tasks',
  activeHint:'Tool calls, reasoning, plans, and permission requests are shown with their original ACP semantics, separate from Pinvou work-session messages.',
  nativeActiveHint:'Tool calls, reasoning, and confirmations are shown with Pinvou Engine semantics, on the same engine lane as work sessions.',
  draftHint:'Start typing to create a temporary session, or choose a project directory from the lower-left corner of the composer.',
  nativeDraftHint:'Start typing to create a temporary session, or choose a project directory from the lower-left corner so Pinvou works directly in it.',
  attentionLatest:'A request needs attention. Go to latest', latest:'Go to latest', removeReference:path=>`Remove workspace reference ${path}`,
  model:'Model', permissionMode:'Permission mode', sessionModeTitle:'Session mode reported by the ACP Agent',
  relayNoModelHint:'The active relay provider has no model configured: official models will not work through it. Set a model for this provider in Settings first.',
  configLabels:{ mode:'Permission mode', collaboration_mode:'Collaboration mode', model:'Model', reasoning_effort:'Reasoning effort', 'fast-mode':'Fast mode' },
  agentCommands:'Agent commands',
  placeholder:'Ask the coding Agent to edit code, run commands, or explain the repository…', temporarySession:'Temporary session', untitledSession:'Untitled code session',
  chooseProject:'Choose project directory', chooseProjectDesc:'Let the Agent work directly in a real project',
  temporarySessionDesc:'Use an isolated directory managed by Pinvou', recentProjects:'Recent projects',
  nativeBlockedNotice:'A dangerous command was blocked and not executed',
  rewindChip:n=>n>0?`Rewind to turn ${n}`:'Rewind to before turn 1',
  rewindChipConversationOnly:n=>n>0?`Rewind conversation to turn ${n}`:'Clear conversation only',
  rewindDialogTitle:'Confirm rewind',
  rewindChangesToUndo:'Code changes to undo', rewindConversationLabel:'Conversation',
  rewindConversationTarget:n=>n>0?`The conversation will be truncated to the end of turn ${n}; later turns are removed`:'The entire conversation will be cleared',
  rewindConversationOnlyNote:'No code snapshot is available for this turn; only the conversation is rewound and your code stays unchanged.',
  rewindPreRestoreNote:'The current code state is saved as a rollback point first, so you can rewind again to undo this.',
  rewindNoChanges:'No file changes after this point',
  rewindAdded:count=>`${count} added`, rewindModified:count=>`${count} modified`, rewindDeleted:count=>`${count} deleted`, rewindOther:count=>`${count} other`,
  rewindStatus:{added:'Added',modified:'Modified',deleted:'Deleted',renamed:'Renamed',copied:'Copied',gitlink:'Nested repo (not rewound)',other:'Other'},
  rewindMoreFiles:count=>`${count} more file(s)`,
  rewindLoading:'Loading changes…', rewindPreviewFailed:'Failed to load the change preview',
  rewindConfirm:'Rewind', rewindCancel:'Cancel', rewindBusy:'Rewinding…',
  rewindInFlightBusy:'Another session still has a rewind or undo in progress; please wait for it to finish.',
  rewindRetryReload:'Retry reload', rewindReloadRetryNote:'The operation took effect, but reloading the session failed. Retrying only reloads the session; it does not repeat the operation.',
  rewindNoticeRestored:n=>n>0?`Rewound to the end of turn ${n}: code and conversation restored. The pre-rewind code state was saved as a rollback point, so you can undo this.`:'Rewound to before turn 1: code and conversation restored. The pre-rewind code state was saved as a rollback point, so you can undo this.',
  rewindNoticeDegraded:'Conversation rewound; the code snapshot for that point is unavailable (possibly evicted), so the code was not changed.',
  rewindNoticeConversationOnly:'Conversation rewound; the code was not changed.',
  rewindNoticeCompaction:'Note: this session went through conversation compaction, so the model context may still contain earlier summaries after the rewind.',
  rewindUndo:'Undo rewind', rewindUndoTitle:'Confirm undo rewind',
  rewindUndoBody:n=>`This restores the code to its pre-rewind state and brings back the ${n} truncated turn(s) of conversation.`,
  rewindUndoBodyConversationOnly:n=>`That rewind only truncated the conversation (your code was not changed); this brings back the ${n} truncated turn(s) of conversation and leaves the code as is.`,
  rewindUndoConfirm:'Undo rewind', rewindUndoBusy:'Undoing…',
  rewindUndoDone:'Rewind undone: code and conversation are back to their pre-rewind state.',
  compactStart:'Compacting context', compactDone:'Context compacted', compactFail:'Context compaction failed', compactCancel:'Context compaction canceled',
  nativeToolGateDecision:(tool,decision,reason,risk,agent)=>`Permission gate ${decision === 'allowed' ? 'allowed' : decision === 'denied' ? 'denied' : 'could not review'} ${tool || 'tool'}${agent ? ` for agent ${agent}` : ''}${risk ? ` · risk ${risk}` : ''}${reason ? ` · ${reason}` : ''}`,
  nativeCompactInactive:'The session engine is not running yet. Send a message before compacting the context',
  nativeUsageTitle:(used,pct)=>`Context used: ${used}${pct == null ? '' : ` (${pct}%)`}. Click to compact now and free up session memory`,
  nativeMemory:'Memory', nativeMemoryTitle:'Memories active in this session',
  // Native-lane Plan approval card: status/failure copy mirrors the bridge BT table.
  nativePlanApproved:'✅ Approved', nativePlanDiscarded:'🚪 Plan discarded',
  nativePlanSuperseded:'📜 Superseded by a newer plan', nativePlanHistorical:'📜 Past plan',
  nativePlanAcceptFailed:'⚠️ accept_plan failed: ', nativePlanDiscardFailed:'⚠️ discard_plan failed: ',
  addAttachment:'Add attachment', commandsAvailable:'Commands reported by the Agent', commandsAfterSession:'Agent commands sync after a session is created',
  connectedSuffix:'connected', notReadySuffix:'not ready',
  notSet:'Not set', choiceTitle:'The Agent needs your choice', submit:'Submit', cancel:'Cancel', submitted:'Submitted', canceled:'Canceled', inputExpired:'This input request has expired',
  checking:'Checking ACP Agents…', bridgeUnavailable:'Codex ACP Bridge unavailable',
  viewLoading:'Loading code mode…', viewLoadFailed:'Failed to load code mode, possibly due to an unstable network connection.', viewRetry:'Retry',
  bridgeRepair:'Repair or reinstall Pinvou. In development, run npm run prepare:codex-bridge.',
  cliMissing:agent=>`${agent} CLI not detected`, cliOutdated:(version,minVersion)=>`Detected old version ${version} (requires ≥ ${minVersion})`,
  cliUpdateAvailable:(agent,version,latest)=>`${agent} is ${version || 'unknown'}; the latest official version is ${latest || 'unknown'}. Upgrading is recommended`,
  cliUpdateRequired:(agent,version,latest)=>latest?`${agent} is ${version || 'unknown'} and must be upgraded to ${latest}`:`${agent}${version ? ` ${version}` : ''} must be upgraded before you can continue`,
  packageManagerUpgradeHint:source=>source==='brew'?'This version was installed via Homebrew and will be upgraded with the same package manager—no second copy will be installed':source==='npm'?'This version was installed via npm and will be upgraded with the same package manager—no second copy will be installed':'It will be upgraded with the corresponding package manager, without installing a second copy',
  upgrade:'Upgrade', declineUpgrade:'Not now',
  officialScriptHint:agent=>`Run the official ${agent} install script (no administrator rights required)`, confirmInstall:'Install', installing:'Installing…',
  manualInstallHint:agent=>`Install the ${agent} CLI manually, then choose "Check again"`, manageAgentOnDesktop:agent=>`Install, upgrade, or authorize ${agent} on the target desktop, then check again here`,
  waitingLogin:'Waiting for Codex authorization', notLoggedIn:'Codex is not signed in',
  waitingAgentLogin:agent=>`Waiting for ${agent} authorization`, agentNotLoggedIn:agent=>`${agent} is not signed in`,
  finishAgentAuth:agent=>`Complete ${agent} authorization in the browser; Pinvou will connect automatically`,
  agentLoginHint:agent=>`Authorize with your ${agent} account`,
  deviceCode:code=>`Device code: ${code}`, authorizationCodePlaceholder:'Paste the Claude authorization code from your browser',
  submitAuthorizationCode:'Submit authorization code', loginRequiredBeforeSend:'Sign in to the Agent before sending a message',
  finishBrowserAuth:'Complete ChatGPT authorization in the browser; Pinvou will connect automatically',
  openingAuth:'Opening the Agent authorization page…', loginHint:'Authorize with Codex CLI / your ChatGPT account',
  reopenAuth:'Reopen authorization page', waitAuth:'Waiting for authorization…', authorize:'Authorize',
  chooseProjectDialog:'Choose project directory', addAttachmentDialog:'Add attachments',
  attachmentsParsing:'Attachments are still being parsed. Please wait', sessionSyncing:'ACP session configuration is still syncing. Please wait',
  operationFailed:'ACP Agent operation failed. Please retry', showRawErrors:true,
  recheck:'Check again', setupRequired:'Agent is not ready',
  setupHints:{ kimi_cli_missing:'Install the Kimi Code CLI first', kimi_auth_required:'Authorize with your Kimi account using the device code', claude_cli_missing:'Install the Claude Code CLI first and make sure claude is on PATH (on macOS: brew install --cask claude-code)', claude_auth_required:'Authorize with your Claude account in the browser, or set ANTHROPIC_API_KEY' },
  accountAndService:'Account and service status',
  accountAuthorized:'Account authorized', accountNotAuthorized:'Account not authorized', serviceAbnormal:'Service issue',
  switchAccount:'Switch account', switchAccountAffectsSessions:'The new account applies to every session using this Agent',
  entitlementUnavailable:agent=>`${agent} membership is unavailable`,
  quotaUnavailable:agent=>`${agent} quota is exhausted`,
  authorizationExpired:agent=>`${agent} authorization expired`,
  serviceUnavailable:agent=>`${agent} is temporarily unavailable`,
  accountRecoveryHint:'This account may be expired or out of quota. Switch accounts to continue; existing sessions and messages are preserved.',
  serviceRecoveryHint:'Check the service again. If the issue continues, retry later or switch accounts.',
  errorDetails:'View error details', dismissNotice:'Dismiss',
  agentUnresponsive:agent=>`${agent} has stopped responding`,
  agentStderr:agent=>`${agent} reported a problem`,
  agentStallHint:'No new events in this turn yet. You can keep waiting, or press Stop to settle it.',
  agentStallCancelHint:'The agent is still unresponsive, so Stop was requested automatically. Waiting for it to settle.',
  agentStallSettledHint:'The agent stopped responding for too long, so this turn was settled as interrupted. You can send again or continue.',
  agentCancelTimeoutHint:'Stop was not acknowledged by the agent, so this turn was settled as interrupted.',
  agentStderrHint:'Agent adapter error shown with sensitive details redacted; the original is recorded only in the session log.',
  agentStderrNoDetailHint:'The agent adapter reported a problem; raw details remain available only in the desktop session log.',
  agentRestarted:agent=>`${agent} session restarted`,
  agentStallRestartHint:'The same session stopped responding repeatedly, so your next message will continue in a fresh agent session; whether the earlier conversation comes along depends on that agent\'s resume support, and we will tell you which one happened after the switch.',
  agentRestartHint:'The previous agent session stopped responding repeatedly, so a fresh one took over; the conversation history is kept, and any unfinished turn needs to be sent again.',
  agentRestartFreshHint:'The previous agent session stopped responding repeatedly, so a fresh one took over. This agent could not restore the earlier conversation, so please restate the context you need; any unfinished turn needs to be sent again.',
  runtimeSources:{ system:'System CLI', override:'Custom CLI', bundled:'Bundled bridge', legacy_bundled:'Bundled Codex' },
};

const acpProvidersEn = {
  sectionDesc:'Configure third-party relay providers (API URL, model, and key) and switch between them or back to official login with one click. Switching affects every entry point that uses this CLI on this machine (terminal, IDE plugins).',
  agentCodex:'Codex', agentClaude:'Claude Code', agentKimi:'Kimi',
  official:'Official login', current:'Active', external:'External relay config detected', externalDesc:'The config file contains a relay configuration that cannot be attributed to a current provider; it may have been written by an external tool or manually. App switching will not overwrite it.', keyOk:'Key saved', keyNone:'No key', notEnabled:'Not enabled',
  switch:'Switch', switchOk:'Switched', restoredOfficial:'Official login restored', restoreOfficial:'Switch to official', deleted:'Deleted', uninstallDone:'Uninstalled', currentOverriddenByEnv:'Switched · overridden by env', effectiveTitle:'Effective config (from CLI config file)', noAnthropicEndpointWarning:'This provider has no Anthropic-compatible endpoint. Claude Code only supports the Anthropic protocol. Confirm the endpoint works or use another relay.', wireResponses:'Responses',
  edit:'Edit', delete:'Delete', addProvider:'Add provider',
  providerName:'Name', providerNamePlaceholder:'e.g. My relay', providerOther:'Other',
  presetAnthropic:'Anthropic Official', presetOpenai:'OpenAI Official', presetMoonshot:'Moonshot Kimi', presetKimiCode:'Kimi Code', presetDeepseek:'DeepSeek', presetZhipu:'Zhipu GLM', presetQwen:'Tongyi Qwen', presetDoubao:'Doubao', presetMinimax:'MiniMax', presetXai:'xAI Grok', presetOpenrouter:'OpenRouter', presetSiliconflow:'SiliconFlow', presetGroq:'Groq',
  baseUrl:'Base URL', baseUrlPlaceholder:'https://api.example.com/v1',
  wireApi:'Wire protocol', wireAnthropic:'Anthropic-compatible', wireOpenai:'OpenAI-compatible', wireKimi:'Kimi native',
  model:'Model', modelPlaceholder:'e.g. claude-sonnet-4-5 / gpt-5.2', modelOptional:'Optional',
  modelSlotsTitle:'Fine-grained models (opus / sonnet / haiku / fable / subagent)', modelSlotsHint:'Auto-filled from the model above; edit individually if needed. All are required — empty slots fall back to official traffic.', modelSlotsRequired:'All five fine-grained model slots are required',
  slot_opus:'Opus slot', slot_sonnet:'Sonnet slot', slot_haiku:'Haiku slot', slot_fable:'Fable slot', slot_subagent:'Subagent',
  secretSet:'Set (value hidden)',
  contextWindow:'Context window', contextWindowPlaceholder:'e.g. 200000 / 1048576', contextWindowHint:'Leave empty for the CLI default (200000). A wrong value truncates conversations early—when in doubt, leave it empty.', contextWindowInvalid:'Context window must be a positive integer',
  apiKey:'API Key', apiKeyPlaceholder:'Enter API Key', apiKeyHint:'A saved key is required to switch',
  showKey:'Show', hideKey:'Hide',
  thirdPartyWarning:'Pinvou never uploads your API Key.',
  cancel:'Cancel', save:'Save', saving:'Saving…',
  deleteTitle:'Delete provider?', deleteDesc:name=>`This removes ${name} and its saved key. If it is active, official login will be restored automatically.`, deleteConfirm:'Delete',
  deleteKeyConfirm:'Clearing the API Key field will delete the saved key. Continue?', apiKeyEmptyConfirm:'No API key entered: switching to this provider later requires a saved key. Save anyway?',
  empty:'No providers yet', emptyHint:'Add a third-party relay, then switch to it with one click.',
  export:'Export', import:'Import',
  exportWarningTitle:'The export file contains plaintext keys', exportWarningDesc:'The export file contains API keys in plaintext. Keep it safe and do not share it. Select the text to copy it manually.', exportDone:'Exported', selectAll:'Select all',
  importWarningTitle:'The import file may contain plaintext keys', importWarningDesc:'Make sure the JSON comes from a trusted source: it may contain API keys in plaintext. Do not forward it to others after importing.',
  importDone:p=>`Import finished: ${p.imported} added${p.idConflicts?`, ${p.idConflicts} id conflicts regenerated`:''}${p.skipped?`, ${p.skipped} invalid entries skipped`:''}`,
  envConflictTitle:'Environment variable override detected', envConflictDesc:'These environment variables are set and take precedence over config files: switching providers or restoring official login may not take effect.',
  envConflictVars:'Variables set',
  cliSection:'CLI status', version:'Version', authenticated:'Authenticated', notAuthenticated:'Not authenticated', notInstalled:'Not installed',
  install:'Install', installing:'Installing…', cancelInstall:'Cancel install', installCancelled:'Install cancelled', update:'Update', uninstall:'Uninstall', recheck:'Check again',
  installCmd:'Command', installLatest:'Latest output', phaseChecking:'Checking…', phaseInstalling:'Installing…', phaseDone:'Done', phaseFailed:'Failed', phaseCancelled:'Cancelled',
  rechecking:'Checking again…',
  login:'Log in', logout:'Log out', loggedOut:'Logged out', logoutRelayDisabled:'A relay is active. Switch back to official login before logging out.', loginWaiting:'Finish signing in from your browser…', openLoginUrl:'Open sign-in link', loginCodePlaceholder:'Paste the authorization code', submitCode:'Submit',
  uninstallTitle:'Uninstall {agent}?', uninstallDesc:'The CLI will be uninstalled according to its install source (brew / npm / official script). Uninstall is blocked while sessions are running.',
  uninstallCleanup:'Also delete config files, saved providers, and keys', uninstallCleanupHint:'Unchecked by default; deletion cannot be undone',
  uninstallBusy:'Uninstalling…', loading:'Loading…', loadFailed:'Failed to load', retry:'Retry', configUnreadable:'Config file unreadable', configUnreadableDesc:'The CLI config file for this agent cannot be parsed. Switching is blocked to avoid overwriting your config; fix or remove the file and try again.',
  sessionProvider:'Session provider', sessionProviderDesc:'Provider pinned to this session; defaults to the global setting', sessionOfficial:'Follow global',
  faultManage:'Manage providers',
};

export const dictEn = {
  steerFailedLost: CHAT_RECOVERY_COPY.en.steerFailedLost,
  ...browserEn,
        langTag: 'en',
        acpAgent: 'ACP Agent',
        newChat: 'New chat', currentChat: 'Current chat', searchChats: 'Search chats', scheduledPlans: 'Scheduled tasks', monitor: 'Status', workflow: 'Workflow', multiAgent: 'Swarm Mode', multiAgentDesktopOnly: 'This swarm conversation is currently available only in the desktop app.', toolStore: 'Plugin Center', modelStore: 'Model Store', cardPool: 'Card Deck', recent: 'History', pinnedTasks: 'Pinned tasks', regularTasks: 'Tasks', sidebarTaskList: 'Tasks', sidebarTaskFilter: 'Filter', sidebarTaskFilterAll: 'All', sidebarTaskFilterPinned: 'Pinned', sidebarTaskFilterCode: 'Projects', sidebarTaskFilterCodeSessions: 'Code sessions', sidebarTaskFilterScheduled: 'Scheduled tasks', sidebarTaskSort: 'Sort', sidebarTaskSortPinnedFirst: 'Pinned first', sidebarTaskSortRecent: 'Recent', sidebarTaskEmpty: 'No tasks', sidebarViewAll: 'View all', sidebarSection: 'Task list', sidebarDateGrouping: 'Group by date', sidebarDateGroupingDesc: 'Group tasks by date; today stays expanded', sidebarNavExpand: 'Expand navigation', sidebarNavCollapse: 'Collapse navigation', sidebarCollapseAll: 'Collapse', sidebarExpandAll: 'Expand', sidebarTaskStyle: 'Style', sidebarResize: 'Drag to resize the sidebar; double-click to reset', settings: 'Settings', localEnv: 'Local Env', megacubeSite: 'Open MegaCube site', tearoffTitle: 'Detached', tearoffHint: 'Pop out to its own window', mobileMore: 'More',        previewTaskDailyBrief: 'Daily Briefing', previewTaskFollowUp: 'Follow-up Monitor', previewTaskSalesWeekly: 'Sales Leads Weekly',
        archiveSession: 'Archive', archiveConfirmTitle: 'Archive this task?', archiveConfirmMessage: 'This task will leave the sidebar.', archiveConfirmDetail: 'The conversation and artifacts stay saved, and can be restored on the chat manager page.', archiveConfirmAction: 'Archive',
        archiveSuccess: 'Archived', archiveSuccessView: 'View',
        exportSessionArchive: 'Export session log', exportSessionDone: path => `Session log exported to ${path}`, exportSessionFailed: 'Failed to export the session log',
        sessionBatchFailed: n => `${n} task operation${n === 1 ? '' : 's'} failed. Please retry.`,
        archivedTasks: 'Tasks archived in Settings', archivedTasksDesc: '', archivedEmpty: 'No archived tasks yet', restoreArchived: 'Restore to list',
        archivedDeleteTitle: 'Delete archived task?', archivedDeleteMessage: 'This permanently deletes the task and its contents. This cannot be undone.', archivedDeleteAction: 'Delete',
        knowledge: 'Knowledge', remoteKnowledge: 'Shared Knowledge',
        remoteKbHeroTitle: 'Shared knowledge', remoteKbHeroStep1: 'Connect a knowledge server', remoteKbHeroStep2: 'Manage documents in one place', remoteKbHeroStep3: 'Use them across devices', remoteKbHeroClient: 'PINVOU client', remoteKbHeroNetwork: 'Secure network', remoteKbHeroServer: 'Knowledge server', remoteKbHeroDevices: 'Authorized devices',
        remoteKbDesc: 'Create shared knowledge on this device, or join your team’s existing library.', remoteKbRefresh: 'Refresh', remoteKbConnectTitle: 'Join shared knowledge', remoteKbInvitePlaceholder: 'Paste a pinvou-knowledge:// invitation', remoteKbDeviceName: 'Enter your name', remoteKbConnect: 'Connect', remoteKbConnected: 'Joined shared knowledge', remoteKbServers: 'Shared knowledge', remoteKbNoServers: 'No shared knowledge yet', remoteKbReady: 'Available', remoteKbNotReady: 'Search unavailable', remoteKbOffline: 'Offline', remoteKbManage: 'Manage', remoteKbReadOnly: 'Read only', remoteKbSelectServer: 'Join or select shared knowledge first', remoteKbShowTrash: 'Show trash', remoteKbDisconnect: 'Disconnect', remoteKbRemoveConfirm: 'Disconnecting removes this connection and all chat attachments on this device. Continue?', remoteKbCollections: 'Shared collections', remoteKbCollectionName: 'New collection name', remoteKbCreate: 'Create', remoteKbCollectionCreated: 'Collection created', remoteKbNoCollections: 'No collections yet', remoteKbDocuments: 'documents', remoteKbChunks: 'chunks', remoteKbRestore: 'Restore', remoteKbTrash: 'Move to trash', remoteKbTrashConfirm: 'Move this collection to trash? Its documents remain recoverable.', remoteKbFiles: 'Documents', remoteKbUpload: 'Upload files', remoteKbUploaded: 'Documents uploaded', remoteKbNoDocuments: 'No documents in this collection', remoteKbStatusPending: 'Processing', remoteKbStatusReady: 'Available', remoteKbStatusFailed: 'Processing failed', remoteKbStatusUnknown: 'Unknown status', remoteKbDownload: 'Download original', remoteKbDownloaded: 'File downloaded', remoteKbTestSearch: 'Test search', remoteKbSearchPlaceholder: 'Enter a question or keywords', remoteKbSearch: 'Search', remoteKbDesktopOnly: 'Shared knowledge requires the PINVOU desktop app.',
        remoteKbAddServer: 'Join shared knowledge', remoteKbHideConnect: 'Hide connection', remoteKbNewCollection: 'New collection', remoteKbCancel: 'Cancel', remoteKbClose: 'Close', remoteKbCollapse: 'Hide', remoteKbDone: 'Done', remoteKbUploading: 'Processing…', remoteKbUploadTitle: 'Upload documents', remoteKbStartUpload: 'Upload', remoteKbRetryFailed: 'Retry failed', remoteKbUploadingFile: 'Uploading', remoteKbUploadDone: 'Done', remoteKbUploadPendingIndex: 'Processing', remoteKbUploadStillIndexing: 'Still processing', remoteKbUploadIndexFailed: 'Processing failed', remoteKbUploadFailed: 'Upload failed', remoteKbUploadSuccess: '{count} documents uploaded', remoteKbUploadSummary: '{done} done · {processing} processing · {failed} failed', remoteKbUploadRefreshFailed: 'Could not refresh the list: {error}', remoteKbFileName: 'Name', remoteKbStatus: 'Status', remoteKbSize: 'Size',
        remoteKbReplace: 'Update document', remoteKbReplaced: 'Document updated', remoteKbLoadMoreDocuments: 'Load more', remoteKbLoadingMoreDocuments: 'Loading…',
        remoteKbNearby: 'Nearby shared knowledge', remoteKbDiscovering: 'Securely checking nearby shared knowledge…', remoteKbNearbyEmpty: 'No available LAN knowledge servers found', remoteKbManualConnect: 'Connect manually', remoteKbVerify: 'Check identity', remoteKbVerifyIdentityDesc: 'Compare this code with “Service and maintenance” on the host. Request access only when the codes match exactly.', remoteKbIdentityCode: 'Service identity code', remoteKbConfirmIdentity: 'Codes match — request access', remoteKbBack: 'Back', remoteKbLan: 'LAN', remoteKbTailnet: 'Tailscale', remoteKbHostIdentity: 'Service identity', remoteKbHostIdentityDesc: 'When a new device connects for the first time, compare this code on both devices.', remoteKbPublishLocal: 'Publish local collection', remoteKbPublishDesc: 'Copy the selected collection and its source documents. Local and shared copies remain independent.', remoteKbPublishChoose: 'Choose a local collection', remoteKbPublishContinue: 'Prepare', remoteKbPublishEmpty: 'This local collection has no documents yet', remoteKbNoLocalCollections: 'No local collections are available to publish',
        remoteKbJoinSource: 'Share link or private address', remoteKbJoinSourcePlaceholder: 'Paste a share link or enter 192.168.1.20:3210', remoteKbJoinHint: 'Supports LAN addresses, Tailscale 100.x addresses, and *.ts.net:port. Private direct connections require identity verification first.', remoteKbShareLinkOnly: 'Paste a pinvou-knowledge://share… link generated by the owner.', remoteKbJoinRequested: 'Join request sent', remoteKbPendingTitle: 'Waiting for approval', remoteKbPendingDesc: 'PINVOU joins automatically after owner approval.', remoteKbCancelRequest: 'Cancel', remoteKbOwner: 'Owner', remoteKbGovern: 'Manage', remoteKbGovernTitle: 'Sharing and members', remoteKbPeopleTab: 'People and invites', remoteKbServiceTab: 'Service and maintenance',
        remoteKbShareTitle: 'Invite your team', remoteKbShareDesc: 'Share links are valid for 24 hours by default and can accept multiple requests.', remoteKbCreateShare: 'Create link', remoteKbAutoApproveRead: 'Auto-approve read-only requests while this link is active', remoteKbCopy: 'Copy link', remoteKbLinkCopied: 'Share link copied', remoteKbRecoveryCopied: 'Recovery code copied', remoteKbCopyFailed: 'Copy failed. Select the text and copy it manually.', remoteKbActiveShares: '{count} active share links', remoteKbRequestsTitle: 'Join requests', remoteKbNoRequests: 'No pending requests', remoteKbApproveRead: 'Read only', remoteKbApproveManage: 'Manage', remoteKbReject: 'Reject', remoteKbStopShare: 'Stop', remoteKbAutoReadShort: 'Auto read-only', remoteKbMembersTitle: 'Members', remoteKbThisDevice: 'This device', remoteKbRevoked: 'Revoked', remoteKbRestoreAccess: 'Restore', remoteKbRevokeAccess: 'Revoke', remoteKbMemberAccess: 'Member access', remoteKbRemoveMember: 'Remove member “{name}”', remoteKbRemoveMemberAction: 'Remove member', remoteKbRemoveMemberConfirm: 'Permanently remove “{name}”?',
        remoteKbPromoteOwner: 'Make owner', remoteKbDemoteOwner: 'Remove owner', remoteKbPromoteOwnerConfirm: 'Make “{name}” an owner? They can manage sharing and members.', remoteKbDemoteOwnerConfirm: 'Remove owner access from “{name}”?', remoteKbModelTitle: 'Semantic search model', remoteKbModelReady: 'Available', remoteKbModelMissing: 'Not downloaded', remoteKbModelDownloading: 'Downloading; this continues after PINVOU closes', remoteKbDownloadModel: 'Download', remoteKbModelDownloadingAction: 'Downloading', remoteKbPermanentDelete: 'Delete permanently', remoteKbPermanentDeleteConfirm: 'Permanently delete “{name}”? This cannot be undone.',
        remoteKbOtherNetwork: 'Other network address (optional)', remoteKbOtherNetworkPlaceholder: 'For example, 100.64.12.34:3210 or cube.ts.net:3210',
        remoteKbCreateHost: 'Create shared knowledge', remoteKbHostCreated: 'Shared knowledge created', remoteKbReconnectHost: 'Reconnect this host', remoteKbHostReconnected: 'Local owner access restored', remoteKbUpgradeHost: 'Upgrade service', remoteKbHostUpgraded: 'Shared knowledge service upgraded', remoteKbClientOutdatedTitle: 'Update PINVOU first', remoteKbClientOutdatedDesc: 'PINVOU {appVersion} is older than the host service {serviceVersion}. Service downgrade is blocked to protect your data.', remoteKbHostProgressCreateTitle: 'Creating shared knowledge', remoteKbHostProgressReconnectTitle: 'Restoring the local connection', remoteKbHostProgressUpgradeTitle: 'Upgrading shared knowledge', remoteKbHostProgressPrepare: 'Preparing installation files', remoteKbHostProgressInstall: 'Waiting for system approval and installing the service', remoteKbHostProgressConnect: 'Connecting to the local service', remoteKbHostProgressComplete: 'Complete', remoteKbHostProgressFailed: 'Operation did not finish', remoteKbHostProgressHint: 'Approve the system prompt if it appears. This usually takes less than a minute.', remoteKbHostProgressRetry: 'Retry', remoteKbNoLanEndpoint: 'No team-accessible LAN address was found. Connect this computer to the LAN and retry.', remoteKbHostSettings: 'Host settings', remoteKbHostSettingsDesc: 'Back up, restore, or manage this computer\'s host service.', remoteKbDangerZone: 'Stop or delete', remoteKbDangerZoneDesc: 'Stopping hosting keeps existing data. Deleting the service and data cannot be undone.', remoteKbBackup: 'Create backup', remoteKbRestoreBackup: 'Restore backup', remoteKbRecoveryTitle: 'Save recovery code', remoteKbRecoveryDesc: 'The backup is encrypted. Restoring on this computer needs no code; moving to another host requires this code. Store it separately from the backup.', remoteKbCopyRecovery: 'Copy recovery code', remoteKbRestoreTitle: 'Restore shared knowledge', remoteKbRestoreDesc: 'Leave the recovery code blank to restore on this host, or enter it to migrate to this host.', remoteKbRecoveryPlaceholder: 'Paste the recovery code for migration (leave blank on this host)', remoteKbSameHostMode: 'A same-host restore keeps the current service identity and members, and restores knowledge content only.', remoteKbMigrationMode: 'Migration imports knowledge content under a new service identity. Existing members must join again.', remoteKbRestoreAction: 'Restore', remoteKbRestoreConfirm: 'Restore knowledge content on this host? Current content will be replaced while the service identity and members stay unchanged.', remoteKbMigrateConfirm: 'Migrate this backup to the current host? Current content will be replaced and a new service identity and owner will be created.', remoteKbRestored: 'Shared knowledge restored', remoteKbMigrated: 'Shared knowledge migrated to this host', remoteKbRemoveHost: 'Stop hosting', remoteKbDeleteHost: 'Delete service and data', remoteKbRemoveHostConfirm: 'Stop the shared knowledge service on this computer? Existing data will be kept for a later reinstall.', remoteKbDeleteHostConfirm: 'Permanently delete the shared knowledge service and all data on this computer? This cannot be undone.', remoteKbHostRemoved: 'Shared knowledge hosting stopped; data was kept', remoteKbHostDeleted: 'Shared knowledge service and data deleted',
        outputs: 'Outputs',
        kbMount: 'Knowledge', kbMountTitle: 'Manage collections attached to this chat', kbMountPick: 'Pick a collection', kbMountNone: 'No collections yet — create or connect one in Knowledge', kbMountRemove: 'Remove all', kbMountRemoveOne: 'Remove this collection', kbMountEnable: 'Enable', kbMountDisable: 'Disable', kbMountEnabled: 'Enabled', kbMountDisabled: 'Disabled', kbMountCount: n => `${n} knowledge bases`, kbMountedTag: 'Mounted', kbMountNoModel: 'Install the embedding model first (Knowledge → Local Knowledge) to attach a local collection', kbMountNotReady: 'The embedding model is still loading or failed to load, so collections cannot be attached yet',
        modeYolo: 'YOLO', modePlan: 'Plan', modeSwitchTitle: 'Switch mode', modeYoloDesc: 'Act directly', modePlanDesc: 'Plan first, then execute',
        modeYoloConfirmTitle: 'Switch to YOLO mode?', modeYoloConfirmBody: 'In this mode the model reads and writes your project directory fully automatically and can run shell commands, without step-by-step approvals.', modeYoloConfirmHint: 'Remembered globally once confirmed — you will not be asked again.', modeYoloConfirmOk: 'Confirm', modeYoloConfirmCancel: 'Cancel',
        kbTitle: 'Knowledge', kbDesc: 'Manage local and shared knowledge in one place',
        kbRescan: 'Rescan', kbScanning: 'Scanning', kbDeduping: 'Deduping', kbScanNow: 'Scan this PC',
        kbTotalFiles: 'Indexed files', kbDupFiles: 'Duplicates', kbWasted: 'Reclaimable',
        kbTabSearch: 'Search', kbTabDups: 'Duplicates',
        kbSearchPlaceholder: 'Try: pdf from last week, videos over 10mb', kbSearchBtn: 'Search',
        kbResults: 'Results', kbNoResults: 'No matching files', kbScanningHint: 'Indexing… scanned',
        kbEmptyHint: 'No index yet. Scan this PC to instantly search all files and find duplicates.',
        kbBuildDups: 'Find duplicates', kbDupHint: 'No dedup data yet. Click "Find duplicates" to scan — reads files on demand to fingerprint.',
        kbDupNote: 'Files with identical content (delete action coming in a later version)', kbNoDups: 'No duplicates found', kbDupCopies: 'copies',
        kbOpen: 'Open', kbOpenFolder: 'Open folder',
        kbPageTitle: 'Knowledge', kbFilesSub: 'Search and browse local files, and turn them into an AI-citable knowledge base.', kbKbSub: 'Turn local files into AI-citable knowledge; answers cite their sources.',
        kbSubFiles: 'Local Files', kbSubKb: 'Local Knowledge', kbSubRemote: 'Shared Knowledge', kbSubOutput: 'Outputs',
        kbOutSub: 'Everything you and PINVOU have made, all in one place.',
        kbOutSearch: 'Search outputs', kbOutSearchList: 'Search by name / source chat',
        kbOutCatAll: 'All', kbOutCatWeb: 'Web', kbOutCatDoc: 'Docs', kbOutCatImg: 'Poster', kbOutCatPpt: 'PPT',
        kbOutCount: (n) => `${n} items`, kbOutSort: 'Recently modified',
        kbOutGroupToday: 'Today', kbOutGroupWeek: 'This week', kbOutGroupEarlier: 'Earlier', kbOutGroupCount: (n) => `${n} outputs`,
        kbOutMonthLabel: (y, m) => `${y}-${String(m).padStart(2, '0')}`, kbOutGroupUnknown: 'Unknown time',
        kbOutColName: 'Name', kbOutColType: 'Type', kbOutColSource: 'Source chat', kbOutColTime: 'Modified', kbOutColActions: 'Actions',
        kbOutTodayPrefix: 'Today', kbOutWeekdays: ['Sun','Mon','Tue','Wed','Thu','Fri','Sat'],
        kbOutPreview: 'Preview', kbOutOpenChat: 'Open in chat', kbOutContinue: 'Continue', kbOutNewProject: 'New project', kbOutGallery: 'Gallery', kbOutList: 'List', kbOutOpenExt: 'Open in system', kbOutOpenFolder: 'Open folder',
        kbOutEmpty: 'No outputs yet', kbOutEmptyHint: 'Chat with PINVOU — things you make together will appear here.',
        kbOutContinuePrefill: (name) => `Continue refining "${name}"`, kbOutRequirementLabel: 'My request:', kbOutFileCount: (n) => `${n} files`,
        kbCatAll: 'All', kbCatDoc: 'Docs', kbCatSheet: 'Sheets', kbCatPpt: 'Slides', kbCatPdf: 'PDF', kbCatImg: 'Images', kbCatZip: 'Archives',
        kbRecent: 'Recently modified', kbAddToKb: 'Add to knowledge', kbAdd: 'Add', kbAddFiles: 'Add files', kbAddFolder: 'Add folder', kbIndexing: 'Parsing',
        kbBrowseByType: 'Browse by type', kbMonitored: 'Click a type to filter below', kbItemUnit: '', kbMonitoredDirs: 'Watching {n} folders', kbManageFavLink: 'Manage →', kbRecentFiles: 'Recent files', kbAllFiles: 'All files', kbSortByModified: 'Sorted by last modified', kbColName: 'Name', kbColLoc: 'Location', kbColSize: 'Size', kbColTime: 'Modified', kbManageFav: 'Manage →', kbEnter: 'Enter', kbCollUnit: 'knowledge bases', kbColColl: 'Knowledge base', kbStatus: 'Status',
        kbBannerTitle: 'AI Knowledge · Build your own knowledge base', kbNewColl: 'New collection', kbEditColl: 'Edit collection', kbSave: 'Save',
        kbStep1: 'Pick local files', kbStep2: 'AI parse & index', kbStep3: 'Cited in chat',
        kbMyColls: 'My collections', kbNoColls: 'No collections yet. Click "New collection" to start.', kbNoCollsShort: 'No collections yet — create one first.',
        kbDocs: 'docs', kbUncat: 'Uncategorized', kbCollFiles: 'Files in knowledge', kbBlocks: 'blocks', kbSkipped: 'skipped',
        kbStReady: 'Ready', kbStIndexing: 'Parsing', kbStPending: 'Pending',
        kbImportInterrupted: 'Unfinished import found', kbImportInterruptedHint: 'Progress from the previous run was saved. Continue from the unfinished file or chunk.', kbResumeImport: 'Continue import', kbCancelImport: 'Cancel task',
        kbImportProgress: 'File progress', kbCurrentFile: 'Current file:', kbChunkProgress: 'Chunk progress', kbFailedFiles: 'Failed files', kbRetryFile: 'Retry', kbImportErrors: 'failed', kbImportDoneWithErrors: 'Import finished with errors',
        kbResumeImportFailed: 'Failed to continue import', kbCancelImportFailed: 'Failed to cancel import', kbRetryImportFailed: 'Failed to retry file',
        kbLoadMoreFailedFiles: 'Load more failed files', kbLoadingFailedFiles: 'Loading…', kbLoadFailedFilesFailed: 'Failed to load failed files',
        kbDeleteColl: 'Delete collection', kbRemove: 'Remove', kbCollEmpty: 'No documents yet. Click "Add files".', kbNoCollFiles: 'No files in any knowledge base yet', kbAllColls: 'All', kbDelCollConfirm: 'Delete collection "{n}"?', kbDelCollWarn: 'Only removes the index for its {c} documents. Your original files on disk are NOT deleted.', kbDelete: 'Delete', kbRemoveDocConfirm: 'Remove "{n}" from the knowledge base?', kbRemoveDocWarn: 'Only deletes its index here. Your original file on disk is not affected.', kbRemoveFailed: 'Could not remove document', kbRemoveQ: 'Remove?',
        kbRetrPlaceholder: 'Search inside this collection…', kbModeSearch: 'Search', kbModeAsk: 'Ask AI', kbAskPlaceholder: 'Ask this collection… (AI answers from its content, with citations)', kbAskBtn: 'Ask', kbAsking: 'Thinking…', kbAnswer: 'Answer', kbSources: 'Sources', kbNoContext: 'Nothing indexed in this collection yet — add files and wait for indexing.', kbAskError: 'Failed to answer — check the local model service.', kbCollNamePh: 'Collection name', kbCollCatPh: 'Category (optional)', kbCancel: 'Cancel', kbCreate: 'Create',
        kbEmbedOn: 'Semantic search on', kbEmbedOff: 'Semantic search off · full-text only',
        kbModelTitle: 'Embedding model not installed', kbModelDesc: 'Vectorizing and semantic search rely on a local embedding model. Download and deploy it to start building knowledge bases the AI can answer from.',
        kbModelLoadingTitle: 'Loading embedding model', kbModelLoadingDesc: 'Local semantic search is initializing. Knowledge bases will become available automatically when it finishes.', kbModelFailedTitle: 'Embedding model failed to load', kbModelFailedDesc: 'Retry loading first. If the local files are incomplete or damaged, download and repair the model.',
        kbModelPkgName: 'PINVOU Embedding Model', kbModelPkgSub: 'Multilingual vector model · int8 quantized · CPU-only, fully local',
        kbModelChipDownload: '~{n} MB download', kbModelChipInstalled: '~{n} MB when installed', kbModelChipOffline: 'Fully offline', kbModelChipLang: 'Chinese / multilingual',
        kbModelItem1: 'bge-m3 multilingual vector model (int8 ONNX)', kbModelItem2: 'Tokenizer & model config files', kbModelItem3: 'CPU inference, no network or GPU',
        kbModelDownloadBtn: 'Download & deploy', kbModelFoot: 'Install once, then offline · keep using other features while it downloads',
        kbModelRetryBtn: 'Retry loading', kbModelRepairBtn: 'Download & repair', kbModelLoading: 'Loading model…',
        kbModelNotLoadedTitle: 'Model installed · not loaded yet', kbModelNotLoadedDesc: 'The embedding model is on this device but has not been loaded into memory yet (nothing needed semantic search yet; an interrupted install also stops here). Load it now, or just import files — loading starts automatically.', kbModelLoadNowBtn: 'Load now',
        kbModelStageDownload: 'Downloading model files…', kbModelStageVerify: 'Verifying…', kbModelStagePrepare: 'Preparing model…', kbModelStageDone: 'Done',
        sysStatus: 'System Status', sysDesc: 'Real-time performance metrics', gpu: 'GPU', gpuUnavail: 'Unavailable', noSmi: 'nvidia-smi not found',
        cpu: 'CPU', cpuUnavail: 'CPU info unavailable', cpuTotal: 'Total Usage', cpuProcess: 'App Usage', cpuLogical: 'logical processors',
        vram: 'VRAM Usage', core: 'Core Usage', temp: 'Temp', power: 'Power', unifiedMem: 'Unified Memory',
        ram: 'System RAM', used: 'Used', total: 'Total', swap: 'Swap Space',
        vllm: 'Inference Engine', ctx: 'Context Length', queue: 'Task Queue', kv: 'KV Hit Rate', ttft: 'TTFT', tps: 'TPS (Generation Speed)', tokTotal: 'Total Tokens · gen/prompt', statsSince: 'Since %t', statsAge: ' · %m min running', statsJustReset: ' · just reset', statsLifetime: 'Lifetime total', app: 'App Service',
        curVer: 'App Version', uptime: 'Uptime', runningMemory: 'Active Memory', temporaryMemory: 'Temporary Memory', localProcessor: 'Local Compute', processorLoad: 'Processor Load', graphicsLoad: 'Graphics Load', sharedMemory: 'Shared Memory', deviceTemp: 'Device Temp', currentModel: 'Current Model', localRunning: 'Local', remoteService: 'Remote', available: 'Available', unavailable: 'Unavailable', historyReuse: 'Reuse Rate', modelReadAmount: 'Model Reads', modelOutputAmount: 'Model Outputs', modelReadHint: 'More input means slower responses and higher usage', modelOutputHint: 'More output means longer replies and higher usage', resetCount: 'Reset Count', contextHint: 'Conversation range kept in memory', queueHint: 'Running / waiting', ttftHint: 'Wait before the first word', tpsHint: 'Reply output speed', reuseHint: 'Higher is faster and saves tokens', totalMemory: 'Total Memory', memoryPressure: 'Memory Pressure', normal: 'Normal',
        appearance: 'Appearance & Language', lang: 'Language', theme: 'Theme', themeDesc: 'Choose your color style',
        light: 'Light', dark: 'Dark', followSystem: 'Follow System',
        notifications: 'Notifications',
        taskCompletedNotif: 'Task completion alert',
        taskCompletedNotifDesc: 'Send a system notification when a task finishes',
        searchBackend: 'Web Search', searchSource: 'Search Source', searchBackendDesc: 'Source used by the LLM when invoking the Web search action',
        searchKey: 'API Key',
        searchKeyHintBing: 'Bing needs no API key, but Chinese search results are poor',
        searchKeyHintMetaso: 'Leave empty for community shared key (~100/day, multi-user)',
        searchKeyHintBocha: 'Required, search will fail without it',
        searchKeyHintBaidu: 'Required (Baidu Qianfan API Key), search will fail without it',
        searchKeyHintTavily: 'Required (tvly- prefix), overseas service, needs stable external network',
        searchKeyPlaceholder: 'Paste API key',
        searchRestartHint: 'Configuration changed — saving will restart the app',
        searchGetKey: 'Get your own key',
        searchGetKeyMetasoSteps: 'Free · 1-min QR-code sign-up (WeChat/phone) · Personal quota',
        searchGetKeyBochaSteps: 'Free · Get sk- prefixed key from console after sign-up',
        searchGetKeyBaiduSteps: 'Create API Key in Qianfan console · Pay-as-you-go with free tier',
        searchGetKeyTavilySteps: 'Overseas · Get tvly- key after sign-up · Free monthly tier, needs external network',
        modelBackend: 'Model & Backend',
        modelBackendDesc: 'Choose a local model service or a third-party API for inference',
        modelPreset: 'Model Source',
        modelPresetLocalVllm: 'Local model (default)',
        modelPresetDeepseek: 'DeepSeek',
        modelPresetKimi: 'Kimi',
        modelPresetOpenaiCompatible: 'OpenAI Compatible',
        modelPresetOpenaiResponses: 'OpenAI Responses Compatible',
        modelPresetOpenai: 'OpenAI',
        modelPresetAnthropic: 'Anthropic Claude',
        modelPresetGemini: 'Google Gemini',
        modelPresetXai: 'xAI Grok',
        modelPresetQwen: 'Tongyi Qianwen',
        modelPresetDoubao: 'Doubao',
        modelPresetMinimax: 'MiniMax',
        modelPresetGlm: 'Zhipu GLM',
        modelPresetMimo: 'Xiaomi MiMo',
        customModelName: 'Model Name',
        customBaseUrl: 'API Base URL',
        modelContextWindow: 'Context Window (tokens)',
        modelMaxOutput: 'Max Output (tokens)',
        customApiKey: 'API Key',
        credEnvOverride: 'Env override', credUnavailable: 'Credential unavailable', credConfigured: 'Configured', credNotConfigured: 'Not configured', credEnterNewKey: 'Enter new key to replace', credKeep: 'Keep', credReplace: 'Replace', credDeleteKey: 'Delete Key', clearSearch: 'Clear search', clearInput: 'Clear input', sendMsg: 'Send', apiKeyGateTitle: 'API Key required', apiKeyGateDesc: 'The current model is a cloud model. Configure an API Key to start chatting.', apiKeyGateBtn: 'Configure',
        detectLocalVllm: 'Detect local model services',
        detectingLocalVllm: 'Detecting…',
        useVllmConfig: 'Use config',
        vllmDetectFound: n => `${n} available instance${n === 1 ? '' : 's'} found`,
        vllmDetectNone: 'No local model service found',
        vllmDetectError: e => `Detection failed: ${e}`,
        vllmDetectOffline: 'Offline',
        vllmDetectReady: 'Ready',
        vllmDetectBusy: 'Busy',
        vllmDetectMismatch: 'Mismatch',
        vllmDetectedModel: 'Model',
        vllmDetectedContext: 'Context',
        vllmDetectHint: 'Checks only common vLLM, Ollama, and LM Studio ports on 127.0.0.1 / localhost. Remember to save after picking a candidate.',
        confirmAndRestart: 'Save & Restart',
        addModel: '＋ Add Model', editModel: 'Edit', deleteModelBtn: 'Delete', setActiveModel: 'Set Default', modelActiveTag: 'Default',
        modelFormAddTitle: 'Add Model', modelFormEditTitle: 'Edit Model', modelDisplayName: 'Display Name',
        testConnection: 'Test Connection', testingConn: 'Testing…', modelSaveBtn: 'Save', manageModels: 'Manage models…',
        switchModelTitle: 'Switch model', modelSwitchBusy: 'Busy generating, cannot switch', modelNonePick: 'Select model', modelDeleteMin: 'Keep at least one model',
        modelGroupPreset: 'Presets', modelGroupCustom: 'Custom',
        thinkingDepth: 'Thinking depth', thinkingDepthTiers: { off: 'Off', low: 'Low', medium: 'Medium', high: 'High', max: 'Max' },
        saveModelFailed: 'Failed to save thinking depth. Please try again',
        modelEnvLocked: f => `Locked by env vars: ${f}; settings won't apply`, deleteModelConfirm: n => `Delete model "${n}"?`,
        modelDirtyHint: 'Configuration changed — saving will restart the app',
        langRestartHint: 'Interface language switched; restart the app to apply it to the assistant',
        sysPerm: 'System Permissions', sudo: 'Advanced execution (Sudo)', sudoDesc: 'Allow the assistant to run advanced commands such as environment configuration',
        versionUpdate: 'Version & Update',
        uiRemote: {
          title:'Mobile Remote Control', desc:'Once enabled, the remote link can browse local files and run a code Agent in selected folders. Share it only with people you trust.', browser:'Browser connection', stop:'Stop', qrAlt:'Remote control QR code', qrHint:'Scan with your phone or copy the link below', link:'Remote control link', linkHint:'The QR code and link are identical and remain valid after restart. Refreshing or stopping access revokes the old link immediately.', generating:'Generating remote control link…', notStarted:'Remote control is not enabled.', copy:'Copy Link', refresh:'Refresh QR Code', enable:'Enable Access', allowWorkspace:'Allow Local Folders', refreshTitle:'Refresh QR code?', refreshDesc:'The old QR code and link will become invalid immediately, and the current browser will disconnect. Scan the new QR code or copy the new link to reconnect.', refreshing:'Refreshing…', updated:'Remote control status updated.', unavailable:'Remote control is temporarily unavailable. Try again.',
          status:{ idle:['Not enabled','Enable access to create a persistent remote control link.'], starting:['Starting','Creating the remote control connection.'], connecting_relay:['Connecting','Connecting to the relay…'], waiting_web_client:['Waiting for browser','Paste the link into a browser on your phone or computer.'], web_client_connected:['Browser connected','A browser is connected to this desktop.'], web_client_disconnected:['Browser disconnected','The link remains valid and can reconnect at any time.'], revoked:['Link revoked','Enable remote control again.'], stopped:['Stopped','Open this panel to enable access again.'], error:['Connection error','Remote control is temporarily unavailable. Try again.'] },
        },
        uiMonitor: { bridgeNotReady:'Bridge not ready', readFailed:'Read failed', reading:'Loading', configError:'Configuration error', unverified:'Unverified', authFailed:'Authentication failed', bridgeError:'Monitor bridge is not ready. Open this page in the Tauri app window.', readError:e=>`Monitor read failed: ${e}`, activity:'Activity' },

        uiSettings: { common:'Common', system:'System', general:'General', model:'Models', search:'Search', memory:'Memory', community:'User Community', permissions:'Permissions & Environment', data:'Data Management', update:'Updates', help:'Help & Feedback', appearance:'Appearance', language:'Interface Language', languageDesc:'Change the app display language', theme:'Theme', themeDesc:'Follow the system, or choose light / dark', notifications:'Notifications', taskNotice:'Task Completion Alerts', taskNoticeDesc:'Show a system notification when a task finishes', voiceShortcuts:'Voice Shortcuts', voiceShortcutEnable:'Enable Alt voice shortcut', voiceShortcutEnableDesc:'Alt starts voice input. When the input box already has text, Alt edits the current text. Off by default.', voiceShortcutHelp:'View Alt voice shortcut guide', voiceShortcutUnsupportedDesc:'The global Alt shortcut is only supported on Windows. On this platform, Alt voice input works while the app window is focused.', voicePostprocess:'Smart organize', voicePostprocessDesc:'Sends the recognized text — together with any existing input-box draft — to your configured model service to fix errors and organize it into structured input. When off, only on-device rule-based fixes are applied.', voiceShortcutWebDesc:'In the browser, Alt voice input works inside the app window. The global shortcut is unavailable, and only plain transcription is provided — no smart organize or voice editing.', desktopAssistant:'Desktop Assistant', pet:'Desktop Companion', petDesc:'Show a persistent companion on the desktop', version:'Version', currentVersion:'Current Version', beta:'Beta', releaseNotes:'Release Notes', noReleaseNotes:'No release notes', downloading:p=>`Downloading update ${p}%`, installingUpdate:'Installing update…', installerStarted:'Installer Started', cancelDownload:'Cancel Download', providers:'ACP' },
        uiSettingsDetail: { modelSection:'Models', settingsLoadFailed:'Failed to load settings', addModel:'Add model', envManaged:'Model settings managed by environment variables remain visible here, but must be changed in the environment.', localModel:'Local model', defaultTag:'Default', edit:'Edit', delete:'Delete', noModels:'No models', localDefaultName:'Local Qwen3.6', searchList:'Search sources', setDefault:'Set as default', addSearch:'Add search source', searchDescriptions:{ bing:'Built-in search', metaso:'Chinese search service', bocha:'Search service', baidu:'Qianfan AI Search', tavily:'International search service' }, searchSourceHint:'Built-in search works without a key; if the selected service fails, searches fall back to it automatically. Adding a Chinese search service improves long Chinese-language queries.', memoryTypes:{ preference:'Long-term preference', work_context:'Work context', current_focus:'Current focus', recent_activity:'Recent activity' }, memoryDetail:'Memory details', content:'Content', editTitle:label=>`Edit ${label}`, userCallName:'Your name', assistantNickname:'Assistant nickname', callNameDesc:'How the assistant addresses you', assistantNameDesc:'How you address the assistant', unnamedMemory:'Untitled memory', view:'View', enableMemory:'Enable memory', enableMemoryDesc:'PINVOU remembers names, preferences, work context, and recent activity', profile:'Profile', notSet:'Not set', longMemory:'Long-term memory', shortMemory:'Short-term memory', noLongMemory:'No long-term memories', noShortMemory:'No short-term memories', hiddenTasks:'Hidden tasks', archivedAt:date=>`Archived ${date}`, restore:'Restore', noHiddenTasks:'No hidden tasks', noHiddenTasksDesc:'Archived tasks appear here', system:'System', advancedPermission:'Advanced execution permission', advancedPermissionDesc:'Allow the assistant to run advanced commands such as environment configuration', missing:'Missing', installMissing:'Install missing dependencies', feedbackTitle:'Submit an issue or suggestion', feedbackDesc:'Attach images or videos; a privacy notice is shown before submission', submitFeedback:'Submit feedback', editSearch:'Edit search source', apiKeyPlaceholder:'Enter API Key', hide:'Hide', show:'Show', cancel:'Cancel', save:'Save', restartSearchTitle:'Restart to apply search settings?', restartLanguageTitle:'Restart to apply language settings?', restartSearchDesc:'Restart the app before the assistant can use the updated search source or credentials.', restartLanguageDesc:'The interface language changed. Restart to also update the assistant response language.', later:'Later', restartNow:'Restart now', deleteModelTitle:'Delete model?', deleteModelDesc:'This removes the model configuration and its saved credentials.', deleteModel:'Delete model', deleteSearchTitle:'Delete search source?', deleteSearchDesc:name=>`This removes ${name} and its saved credentials.`, deleteSearch:'Delete search source', addSearchDesc:'Choose a search source, then enter the required credentials', closeSettings:'Close settings', chooseModelDesc:'Choose a model, then enter the required credentials', collapse:'Collapse', change:'Change', customModel:'Custom model', localModelName:name=>name ? `Local ${name}` : 'Local model', apiKeyReadFailed:'Failed to read API Key', localModelId:'Local model ID', modelId:'Model ID', modelIdPlaceholder:'Enter model ID', customModelTitle:provider=>`Custom ${provider} model`, customModelDesc:'Enter a model ID manually', customLocalDesc:'Enter the model ID exposed by the local service', customCompatibleDesc:'Enter the model ID and service URL manually', modelDescriptions:{ '本地服务默认模型':'Default local service model', '高能力模型':'High-capability model', '快速响应':'Fast responses', '最新通用模型':'Latest general-purpose model', '代码场景':'Coding tasks', '高速代码场景':'High-speed coding tasks', '稳定可用':'Stable release', '高性价比':'Cost-effective', '通用能力':'General-purpose', '兼容保留':'Compatibility option', '高速响应':'High-speed responses', '轻量模型':'Lightweight model' } },

        uiPetSettings: { choose:'Choose a companion', placeholder:'Coming soon', preparing:'Preparing animation', animationFailed:'Animation failed to load', coverFailed:'Cover failed to load', retry:'Retry', pets:{ lingling:{name:'Lingling',description:'Star-patterned tail, always lively'}, langlang:{name:'Langlang',description:'Black outfit and glasses, calmly by your side'}, 'ace-taffy':{name:'Ace Taffy',description:'Pink hair and crown, cheering you on'} } },
        uiScheduled: { title:'Scheduled Tasks', subtitle:'Let PINVOU automatically handle recurring work', newTask:'New Task', templates:'Recommended Templates', myTasks:'My Tasks', loading:'Loading scheduled tasks…', empty:'No matching scheduled tasks', view:n=>`View scheduled task: ${n}`, useTemplate:n=>`Use ${n} template`, running:'Task is running', unread:'Unviewed run conversation', navUnreadAria:'Scheduled tasks have unviewed run conversations', closeError:'Dismiss error', deleteTitle:'Delete scheduled task?', deleteDescription:n=>`“${n}” will stop running and be removed from the task list. Previous run history will be kept.`, cancel:'Cancel', delete:'Delete', save:'Save', filterAll:'All', filterActive:'Enabled', filterPaused:'Paused', active:'Active', paused:'Paused', enabled:'Enabled', unknown:'Unknown', notScheduled:'Not scheduled', noSchedule:'No schedule', waitingDispatch:'Waiting to run', soon:'Starting soon', nextRun:(time,remaining)=>`Next: ${time} (${remaining})`, date:(month,day)=>`${month}/${day} `, daysAfter:(days,hours)=>`in ${days}d${hours ? ` ${hours}h` : ''}`, hoursAfter:(hours,minutes)=>`in ${hours}h${minutes ? ` ${minutes}m` : ''}`, minutesAfter:(minutes,seconds)=>`in ${minutes}m ${seconds}s`, secondsAfter:seconds=>`in ${seconds}s`, runStatus:{ queued:'Queued', running:'Running', completed:'Completed', failed:'Failed', canceled:'Canceled' }, weekdays:[['Monday','Mon'],['Tuesday','Tue'],['Wednesday','Wed'],['Thursday','Thu'],['Friday','Fri'],['Saturday','Sat'],['Sunday','Sun']], hourCount:n=>`${n} hour${n === 1 ? '' : 's'}`, choose:'Choose', autoModel:'Auto select', currentModel:'Current model', reselectModel:n=>`Choose another model · ${n}`, repeatOptions:{ workdays:'Weekdays', daily:'Daily', weekly:'Weekly', hourly:'Hourly', once:'Once', custom:'Custom' }, everyHours:n=>`Every ${n} hours`, startsAt:time=>`from ${time}`, repeat:'Repeat', interval:'Interval', dateLabel:'Days', startTime:'Start time', time:'Time', chooseRepeat:'Choose repeat frequency', chooseInterval:'Choose hourly interval', chooseDate:'Choose run days', chooseStartTime:'Choose start time', chooseRunTime:'Choose run time', setStart:'Set start', createFromTemplate:'Create from template', closeCreate:'Close new task', taskName:'Task name', taskNamePlaceholder:'For example: Daily data backup', taskPrompt:'Instructions', taskPromptPlaceholder:'Describe what should be done on each run…', saveTask:'Save task', editTask:'Edit task', closeDetail:'Close task details', taskNameAria:'Scheduled task name', taskPromptAria:'Scheduled task instructions', aiModel:'AI model', chooseModel:'Choose scheduled task model', runningStatus:'Status', nextExecution:'Next run', enableTask:'Enable task', runNow:'Run now', openFolder:'Open folder', runHistory:'Run history', records:n=>`${n} record${n === 1 ? '' : 's'}`, noRecords:'No records', openRun:'Open run conversation', noOpenRun:'This run has no conversation to open', openRunLabel:s=>`Open run: ${s}`, runInProgress:'Run in progress', viewRunResult:'Open the conversation to view results', noRunSession:'No conversation available', noRunHistory:'No run history yet', saveState:{ saving:'Saving…', saved:'Saved', invalid:'Name and instructions are required', error:'Save failed' }, pause:n=>`Pause ${n}`, resume:n=>`Resume ${n}`, templateMap:{ 'daily-brief':{name:'Daily Briefing',schedule:'Daily at 8:00',description:'Summarize important news, industry updates, and connected workplace announcements',prompt:'Summarize important news and industry updates from the past 24 hours with sources and links; when Lark or WeCom is connected, include company announcements. Do not scan user directories; save results to the task workspace.'}, 'follow-up-monitor':{name:'Follow-up Monitor',schedule:'Weekdays at 9:00',description:'Review overdue and upcoming items, highlighting risks and next steps',prompt:'Summarize overdue, due-today, and upcoming (next 3 workdays) items from connected Lark or WeCom, with risks and next steps by priority. Query and organize only — do not send, approve, or modify; do not scan user directories.'}, 'weekly-review':{name:'Weekly Review',schedule:'Friday at 16:00',description:'Create a structured weekly report from this week’s workplace activity',prompt:'Create a weekly report from this week’s calendar, to-dos, and workplace messages in connected Lark or WeCom, covering progress, carry-over items, risks, and next week’s plan. Do not scan user directories or send automatically.'}, 'memory-organize':{name:'Memory Organizer',schedule:'Weekdays at 9:30',description:'Keep long-term memory tidy on a schedule: merge duplicates, prune stale items, fix wording',prompt:'Periodically organize my long-term memory: merge duplicate entries, remove outdated or invalid items, and fix vague wording so memory stays concise and accurate. This task runs automatically without opening a conversation; it only organizes memory and does not send messages or make other changes.'} } },
        uiChat: { installedReady:'Installed · enabled on your first question', welcomeOptInFailed:'Failed to enable the tool; this question runs without it. Retry from the composer tools list', ready:'Ready', sceneModes:{ personalWorkbench:'Personal workbench', documentWriting:'Document writing', poster:'Poster', dataVisualization:'Data visualization', pptDesign:'PPT design', clear:label=>`Clear ${label}` }, askMe:'Try asking', naturalQuestion:'Ask in natural language.', backRuns:'Back to scheduled task run history', scheduledRun:'Scheduled task run', runRecords:'Run history', asrUnavailable:'Speech recognition is not installed. Install it in the desktop app and try again.', gotIt:'Got it', asrDownloadTitle:'Download speech recognition model', asrEnableTitle:'Enable local speech recognition', asrRuntimeMissing:'The local speech runtime is missing. Repair or reinstall the app; models can only be downloaded when the runtime is present.', asrReadyNotice:'Voice model is ready. Click the mic to start recording.', asrFirstUse:(size,ffmpeg)=>`First use requires a speech recognition download (${size}${ffmpeg ? ' + ffmpeg' : ''}). The model and recognition run on this device and audio never leaves it; with smart organize enabled, the recognized text (plus any existing input-box draft) is sent to your configured model service for correction and organizing.`, sizeModelOnly:'about 254 MB', sizeFull:'about 174–254 MB', asrStages:{ ffmpeg:'Installing ffmpeg (a system authorization dialog may appear)…', verify:'Verifying model integrity…', cancelling:'Canceling download…', done:'Done', cancelled:'Canceled', failed:'Download failed. Try again.', preparing:'Preparing…' }, downloadingModel:p=>`Downloading model ${p}`, cancelling:'Canceling…', cancelDownload:'Cancel download', cancel:'Cancel', repairInstall:'Repair installation', downloadModel:'Download model', install:'Install', designingCard:'🃏 Designing card…', recordedRecent:'Recent activity recorded', viewMemory:'View it in Memory', memoryUpdated:'Memory updated', processed:'Processed', candidate:'Memory suggestion', remember:'Remember', ignoreOnce:'Ignore this time', neverAsk:'Do not ask again', memoryMeta:{ current_focus:{label:'Current focus',prompt:'I can remember this current focus',hint:'I’ll use it to understand the work you are currently advancing.',notice:'Future chats can refer to this recent item.'}, recent_activity:{label:'Recent activity',prompt:'I can remember this recent activity',hint:'I’ll use it to understand what you just completed.',notice:'Future chats can refer to this completed work.'}, work_context:{label:'Work context',prompt:'I can remember this work context',hint:'I’ll use it to understand your longer-term work context.',notice:'Future chats can refer to this long-term context.'}, profile:{label:'Name',prompt:'I can remember this name',hint:'I’ll use this name when talking with you.',notice:'Future chats will use this name.'}, preference:{label:'Preference',prompt:'I can remember this preference',hint:'I’ll adjust future responses using this preference.',notice:'Future chats can refer to this preference.'} } },
        uiChatExtra: { transferRevision:type=>`Pinvou · ${type} · Revision handoff`, draftingScheduled:'⏰ Preparing scheduled task draft…' },
        expertPoolIndividualTab:'Individual Experts', expertPoolTeamTab:'Expert Teams',
        uiWorkflow: { pick:'Choose a workflow template to begin', empty:'No workflows are available. Check the bundled workflow resources.', workflow:'Workflow', stopFailed:e=>`Failed to stop workflow: ${e}`, completed:'🎉 Workflow complete' },
        uiModelStore: {
          title:'Model Store', subtitle:'Browse, download, and deploy large models on this machine.',
          tabStore:'Marketplace', tabLibrary:'My Models',
          sectionInstalled:'On this device', sectionAvailable:'Available',
          filterAll:'All', filterInstalled:'Installed', filterRunning:'Running',
          statusAvailable:'Available',
          linkRowRemoved:(name,n)=>`Also removed ${n} PINVOU model entr${n===1?'y':'ies'} for "${name}"`,
          linkRowStale:(name,e)=>`"${name}" was uninstalled, but its entry in the PINVOU model list could not be removed (${e}). That entry no longer works — delete it in Settings or switch to another model.`,
          uninstallStopFirst:n=>`"${n}" is running. Stop it first, then delete. Deleting while it runs can remove the weights before the service is fully stopped, leaving a process no screen can stop.`,
          uninstallLinksLeft:n=>`"${n}" was uninstalled, but the entries pointing at it in the PINVOU model list could not be cleaned up automatically. They now point at a local service that no longer exists — please delete them in Settings.`,
          emptyInstalled:'No models downloaded on this device yet',
          emptyInstalledHint:'Pick one from Available below. Once downloaded you can deploy it and use it in PINVOU directly.',
          search:'Search models, providers, or engines', clearSearch:'Clear search', refresh:'Refresh', retry:'Retry', loading:'Loading…',
          loadFailed:e=>`Failed to load: ${e}`,
          storeLoadFailed:e=>`Failed to load the store listing: ${e}`,
          libraryLoadFailed:e=>`Failed to load the local model library: ${e}`,
          guideNotInstalledTitle:'Local model management must be installed on this machine',
          guideNotInstalledHint:'Downloads and on-device deployment are handled by the Local Model Management component, which is not installed on this device yet.',
          guideStoppedTitle:'Local model management is not running',
          guideStoppedHint:'Start it to browse the marketplace, download models, and deploy them on this machine.',
          guideUnsupportedTitle:'The model store is not available on this platform',
          accessLocalOnly:'This is a loopback address — it only works on this device. Letting other machines on your network connect requires the API gateway, which is not part of this version yet.',
          guideLoadingTitle:'Starting local model management…',
          guideErrorTitle:'Local model management is unavailable',
          guideStart:'Start', guideStarting:'Starting…', guideStartFailed:e=>`Failed to start: ${e}`, guideVersion:v=>`Version ${v}`,
          storeEmpty:'No models are listed in the store yet', storeSearchEmpty:'No matching models', cardNoDescription:'(No description)',
          download:'Download', downloading:'Downloading…', downloaded:'Downloaded',
          taskDownload:n=>`Downloading "${n}"`, downloadDone:n=>`"${n}" downloaded; deploy it from My Models`, downloadCancelled:'Download cancelled',
          detailProvider:'Provider', detailEngine:'Inference engine', detailQuantization:'Quantization', detailSize:'Size',
          detailSizeDual:'(one copy per device)', detailClose:'Close',
          // Fallback title for the detail dialog while loading or after a failed
          // load: the title bar (and the close button it carries) must never vanish.
          detailTitle:'Model details',
          topologyDual:'Dual-device', topologySingle:'Single device',
          openDetail:'View details',
          dualNotice:'This is a dual-device deployment: two machines connected by a QSFP direct cable. Whichever device downloads and deploys becomes the primary; the other is the secondary.',
          dualNoticeDownload:'Downloading only needs this device — no direct cable and no secondary machine required.',
          dualNoticeDeploy:'Deploying asks once for the secondary machine’s management IP, username, and password. Networking, package push, pairing, weight distribution, and startup are then fully automatic.',
          weightVerifyTitle:'Weight integrity: no per-file checksum',
          weightVerifyNone:'This card calls the download finished as soon as every file is present. It never compares them against official checksums, so a copy that was swapped out on the download source would go unnoticed here.',
          weightVerifyManifest:'This card does check every file against a checksum, but that checksum comes from the same place as the weights. It catches a corrupted transfer or a later edit; it cannot catch a source that was already bad.',
          weightVerifyAdvice:'This does not mean the card is compromised — every built-in card comes from the research team. To be safer: download over a network you trust, don’t point the mirror setting at an unfamiliar address, and if the weights were copied from someone else’s USB drive, download them again instead.',
          libraryEmpty:'No models downloaded yet — take a look at the marketplace',
          statusRunning:'Running', statusDeployed:'Deployed', statusPending:'Not deployed',
          deploy:'Deploy', deploying:'Deploying…', redeploy:'Redeploy',
          enable:'Enable', enabling:'Enabling…', stop:'Stop', stopping:'Stopping…', uninstall:'Uninstall', uninstalling:'Uninstalling…',
          taskDeploy:n=>`Deploying "${n}"`, taskEnable:n=>`Enabling "${n}"`, taskStop:n=>`Stopping "${n}"`, taskUninstall:n=>`Uninstalling "${n}"`,
          deployDone:n=>`"${n}" deployed`, enableDone:n=>`"${n}" enabled`, stopDone:n=>`"${n}" stopped`, uninstallDonePurged:(n,s)=>`"${n}" uninstalled, weights deleted, ${s} freed`, uninstallDoneResidual:(n,c)=>`"${n}" uninstalled, but ${c} weight path(s) could not be deleted; no disk space was freed`, uninstallDoneKept:n=>`"${n}" uninstalled — weights kept and the card stays in your library, ready to redeploy`,
          corrected:n=>`The run state of "${n}" was corrected to not running based on the actual container state`,
          profileTitle:'Choose a deployment profile', profileStart:'Start deployment',
          uninstallTitle:'Uninstall or delete model',
          uninstallIntro:n=>`What do you want to do with "${n}"? Both options stop the service and remove its deployment configuration. The engine container image is kept either way — other models on the same engine still need it.`,
          uninstallOptionKeep:s=>`Uninstall: takes the model off the engine only. The downloaded weights (about ${s}) stay on disk and the card stays in your model library, so you can redeploy it with one click instead of downloading everything again.`,
          uninstallOptionPurge:s=>`Delete: removes the model weights (about ${s}) as well, reclaiming that disk space, and takes the card out of your model library. This cannot be undone — using the model again means downloading it from scratch.`,
          uninstallKeep:'Uninstall (keep weights)',
          uninstallPurge:'Delete (with weights)',
          uninstallIntroDownloaded:n=>`"${n}" is only downloaded on this machine — it has never been deployed to an engine, so there is no service to uninstall. Deleting removes the model weights from disk and takes the card out of your model library.`,
          forceRemoveEntry:'Force-remove from list',
          forceRemoveTitle:'Force-remove from list?',
          forceRemoveIntro:(n,s)=>`Uninstalling "${n}" did not finish, so its record is still in the model library and the card still shows up in the list. Force-remove deletes that record so the card disappears. The card records a size of ${s} — that space will not be reclaimed.`,
          forceRemovePointRecordOnly:'Only the library record and the API keys for this model are deleted. Not a single byte is removed from disk.',
          forceRemovePointResidual:'Downloaded weights and deployment packages stay on disk exactly as they are, so the space will not come back. Directories confirmed to still exist will be listed after removal.',
          forceRemovePointAfter:'You will have to clean up those files yourself, or fix the environment first (for example, get the secondary machine back online) and run a normal uninstall again. This cannot be undone.',
          forceRemoveConfirm:'Force-remove',
          forceRemoveResultTitle:'Removed from the list',
          forceRemoveResidualTitle:n=>`Directories still on disk (${n})`,
          forceRemoveDone:(n,c)=>`"${n}" removed from the list. No files were deleted; ${c} leftover path${c===1?'':'s'} confirmed on disk`,
          forceRemoveFailed:e=>`Force-remove failed: ${e}`,
          detailStatus:'Status', detailEndpoint:'Local endpoint',
          runtimeTitle:'Runtime', runtimeEmpty:'No runtime data yet',
          accessTitle:'Connect an agent app', accessModelId:'Model ID', accessApiUrl:'API endpoint', accessApiKey:'API key',
          accessGatewayOff:'The API gateway is off. Start it to get an endpoint reachable on the local network.',
          startGateway:'Start gateway', gatewayStarted:'API gateway started',
          keysEmpty:'No keys yet — use "Create key"', keysHint:'Create multiple keys to hand out per caller. Revoking a single key is not supported yet: existing keys only stop working when the card itself leaves your model library ("Delete (with weights)" or "Force-remove from list").',
          addKey:'Create key', keyCreated:'New API key created',
          copy:'Copy', copied:'Copied to clipboard', copyFailed:'Copy failed — select and copy manually',
          notRunningHint:'This model is not enabled, so agents calling it now will get "model not enabled".',
          tasksTitle:'In progress', taskRunning:'Working…', taskCancelling:'Cancelling…', taskCancelled: 'Cancelled', taskWaitingInput:'Waiting for your input…',
          taskCancel:'Cancel', taskClose:'Close', taskExpand:'Show log', taskCollapse:'Hide log', taskNoOutput:'(No output)',
          taskProgress:(done,total)=>`${done} / ${total}`, taskSpeed:s=>`${s}/s`, taskEta:d=>`${d} left`,
          etaSec:n=>`${n}s`, etaMin:n=>`${n} min`, etaHour:(h,m)=>`${h} h ${m} min`,
          promptTitle:'A few details are needed', promptSubmit:'OK', promptCancel:'Cancel', promptYes:'Yes', promptNo:'No',

          linkUse: 'Use in PINVOU',
          linkDone: 'Connected to PINVOU. You can start chatting now.',
          linkFailed: 'Failed to connect',
          linkNoRecord: 'Model details are not loaded yet. Please retry.',
          linkNotRunning: 'Enable this model first, then connect it to PINVOU.',
          linkNotChatModel: 'This is not a chat model — it does not expose an OpenAI-compatible chat endpoint — so it cannot be used as a PINVOU chat model. External programs can still call it through its own API.',
          linkNotLocalEndpoint: 'The endpoint this model reports is not on this machine, so it cannot be connected: every message you send would be forwarded to that host. Check whether this card comes from a source you trust.',
          linkNoModelName: 'This model has no model name and cannot be connected.',
          linkNoKey: 'Could not obtain an API key',
          linkBridgeMissing: 'Writing model settings is not supported here',
          linkKeyLabel: 'PINVOU',
          linkModelName: (name) => `Local · ${name}`,
          // —— Startup reclamation of leftover processes ——
          // Download processes left behind by a previous hard kill keep a lock on
          // the weights directory and silently block the next download (16 hours
          // once, on real hardware). Say so when they are reclaimed -- and
          // especially when they cannot be.
          startupCleanupKilled: (n) => `Cleaned up ${n} leftover background task(s) from the previous session`,
          startupCleanupStuck: (n) => `${n} leftover background task(s) run as root and could not be cleaned up automatically; they may stall new downloads`,
          // —— Storage and import scan ——
          // If the weights directory does not match where the user actually put
          // the model, a download burns dozens of hours and nothing on screen
          // looks wrong. Both settings have to be visible and editable.
          storageButton: 'Storage',
          storageTitle: 'Storage and import',
          storageModelsDirLabel: 'Model weights directory',
          // These two (storageModelsDirHint / storageScanHint) must not assert either
          // direction: the host does not decide whether reuse happens.
          //
          // The split is: the host answers "where to look" (roots go out via
          // LMM_IMPORT_DIRS, hits via LMM_IMPORT_CANDIDATES, and both land in the run
          // log); the card script answers "is this copy good, and do I use it". So the
          // two sets of cards behave differently:
          //   - Official store cards: already consume LMM_IMPORT_DIRS (shipped to
          //     production 2026-09-21; 7 of the 7 listed cards). They reuse a complete
          //     local copy instead of downloading again.
          //   - Built-in test store cards (data/test-store/ compiled into the binary):
          //     not wired up, still download. That copy is stale w.r.t. production.
          //
          // Both earlier versions of this copy lied, in opposite directions. Describe
          // what the host does and leave reuse to the card, and neither side lies.
          storageModelsDirHint: 'Changing this does not move weights you already downloaded. Models in the old directory stay on this machine, but it is no longer searched automatically — add the old path under "Additional search directories" to keep reusing them.',
          storageFree: (size) => `${size} free`,
          storageImportLabel: 'Additional search directories',
          storageImportHint: 'One absolute path per line. Add the places we cannot guess, such as a second data drive.',
          storageScanLabel: 'Where downloads look first',
          storageScanHint: 'Searched in this order. Results are written to the run log and handed to the model card to verify. Reuse is up to the card script: official store cards reuse a complete local copy, while the built-in test store cards do not support this yet and still download over the network. USB and external drives are included automatically.',
          storageRemovable: 'removable',
          storageSave: 'Save',
          storageCancel: 'Cancel',
          storageSaved: 'Saved',
          // Shown when the settings could not be read: Save is disabled, because an
          // empty "additional search directories" box would submit as an empty list
          // and silently wipe the directories the user had saved.
          storageLoadFailedHint: 'Could not read your current storage settings, so saving is disabled to avoid overwriting the search directories you already saved. Close this dialog and open it again.',
          // —— Test model store (hidden entry: tap the version line 7 times) ——
          testStoreUnlockTitle: 'Enable the test model store',
          testStoreUnlockHint: 'Test store cards and scripts ship inside the app and do not depend on the store server. They exist only to verify the scripts themselves.',
          testStorePasswordPlaceholder: 'Passphrase',
          testStoreUnlockConfirm: 'Enable',
          testStoreUnlockCancel: 'Cancel',
          testStoreWrongPassword: 'Wrong passphrase',
          testStoreUnlocked: 'Test model store enabled. Switch the data source above.',
          testStoreBanner: 'Test model store is on (for development checks only; cards ship inside the app)',
          testStoreDisable: 'Turn off',
          testStoreDisabled: 'Test model store turned off. Enabling it again needs the passphrase.',
          sourceOfficial: 'Official store',
          sourceTest: 'Test store',
        },
        uiToolStore: { title:'Plugin Center', search:'Search connectors, skills, and plugins', myTools:'My Plugins', installedOnly:'Installed only', results:'Search Results', back:'Back to Plugin Center', internal:'Internal · Employees only', internalTitle:'H3C Workplace\nOne-click Access', internalDesc:'Connect to internal systems with your SSO identity—no key required', internalTools:'H3C Internal Plugins', internalCount:n=>`${n} plugins · Intranet required`, view:'View', modePlain:'Plain', modeCode:'Code', modeVisibilityHint:'Configure plugin visibility per session mode', manageVisibility:'Manage Visibility', doneManagingVisibility:'Done', guide:{ title:'Plugin Guide', close:'Close', dragTitle:'Drag to install', dragDesc:'Drag a standard skill, a single .md, an MCP server, or a Pinvou plugin .zip directly into this window to import it, or click "Upload" in the top-right to choose a file.', typesTitle:'Supported forms', types:['Standard skill (SKILL.md folder)','Single .md skill file','MCP server','Pinvou plugin package (recommended)'], formatsNote:'Standard skills, .md, MCP, and Pinvou plugin packages are all importable; the Pinvou plugin package is just the recommended standard form, and the rest are auto-normalized.', dragHintShort:'Drag in a package to add a custom plugin', introTitle:'What is a Pinvou plugin package', introDesc:'One package = one card = one switch. Pack skills, MCP servers (or their combinations) into a zip; on import the type is auto-detected from content, validated, and registered, then toggled and uninstalled uniformly in the store and at runtime.', specTitle:'Plugin package spec', specDesc:'See the "Plugin Package Spec" for the full rules: zip layout, plugin.json (schema v1), mcp manifest.json, SKILL.md, icons, and naming safety.', downloadSpec:'Download spec (Markdown)', downloadHint:'Download and share with third-party package authors; the desktop app opens a save dialog.' } },
        artifactPreview: { download:'↓ Download', openExternal:'↗ Open externally', loading:'Loading…', fileMissing:'File does not exist or has been deleted', readFailed:e=>`Read failed: ${e}`, imageReadFailed:e=>`Image read failed: ${e}`, previewUnsupported:'Preview is not supported for this type', visualWarningTruncatedPages:'Long document: only the first 30 pages are rendered', visualWarningConvertFailed:'This file cannot be previewed visually (conversion failed or a renderer component is missing)', downloadArtifact:'↓ Download artifact', openExternalArtifact:'↗ Open externally' },
        uiToolDetails: { tools:{
          ima:{ title:'Tencent ima', subtitle:'Manage ima notes and knowledge bases through OpenAPI', latency:'Cloud', desc:'Connect the Tencent ima OpenAPI Skill to search, read, create, and append notes, or search and browse knowledge bases and import web pages or content. Credentials stay on this device.', configTitle:'Connect Tencent ima', configDescription:'Credentials stay on this device and are used only to enable the IMA OpenAPI Skill.', configDocLabel:'Get Client ID / API Key', welcomeQueries:['Search my ima knowledge bases','List my ima notes','Create an ima note from this content','Find the product plan in my ima knowledge base'] },
          tmeet:{ title:'Tencent Meeting', subtitle:'Manage meetings, recordings, minutes, and attendance reports as yourself', type:'CLI + Official Skill', latency:'Cloud', desc:'Connect the official Tencent Meeting CLI and skill to create, query, update, and cancel meetings, and access attendance reports, recordings, transcripts, and AI minutes. Connecting opens the authorization page for QR-code sign-in.', welcomeQueries:['Create a Tencent Meeting for me','Show my Tencent Meeting recordings','Show the latest AI meeting minutes','List the attendees of this Tencent Meeting'] },
        } },
        uiPet: { waiting:'Input needed', failed:'Needs attention', review:'Ready to review', running:'Working', sendFailed:'Send failed', noMain:'Cannot connect to the main window', scheduledDone:'Scheduled task complete', done:'Completed', reply:'Reply', collapseReply:'Collapse reply', expandReply:'Expand reply', collapse:'Collapse', expand:'Expand', replyPlaceholder:'Type a reply…', sendReply:'Send reply', back:'Back to PINVOU', ready:n=>`${n} is ready`, backHint:'Click to return to PINVOU', drag:n=>`Click to return to PINVOU; drag ${n} to move`, openScheduled:n=>`Open the latest run of scheduled task ${n}`, closeScheduled:'Dismiss scheduled task completion', openChat:n=>`Open ${n} chat`, closeNotice:n=>`Dismiss ${n} notification`, replyTo:n=>`Reply to ${n}`, expandActivities:n=>`Expand ${n} activities`, collapseActivities:'Collapse activity cards', expandActivity:'Expand activities', collapseActivity:'Collapse activities', loadFailed:'Companion failed to load', retry:'Click to retry', hide:'Hide Companion' },
        uiWebConnection: { idle:['Preparing remote control','Initializing browser connection…'], connecting:['Connecting to desktop','Interrupted connections retry automatically; unconfirmed actions are not repeated.'], desktop_offline:['Desktop is offline','Keep this page open; it will reconnect when the desktop returns.'], credentials_missing:['Incomplete link','Enable remote control on the desktop and paste the complete link.'], denied:['Access denied','The link is invalid or was refreshed. Copy a new link from the desktop.'], revoked:['Access stopped','The desktop stopped this remote control link.'], replaced:['Taken over by another browser','Only one active browser is allowed. Refresh this page to take over again.'], incompatible_desktop:['Desktop version incompatible','Update the desktop app, then reopen this link.'], error:['Connection error','Remote control will keep trying to reconnect.'] },
        uiConversation: conversationEn,
        uiComputerUse: {
          settingsSection:'Computer use',
          settingsToggle:'Computer use (allow the agent to see and control your screen)',
          settingsHint:'When enabled, the agent can take screenshots and control your mouse and keyboard, which carries a risk of unintended actions. The agent asks for your consent again before the first control of each session, and consequential actions need per-action confirmation.',
          platformUnsupportedHint:'Computer use is not available on this platform: there is no computer-use backend for this operating system.',
          settingsStoppedHint:'Computer use is stopped. Turn it off and back on to resume agent control.',
          permTitle:'macOS system permissions',
          permScreenRecording:'Screen Recording (screenshots)',
          permAccessibility:'Accessibility (mouse & keyboard control)',
          permGranted:'granted',
          permNotGranted:'not granted',
          permRequest:'Open system permission dialogs',
          permRestartHint:'The system applies a grant only to a newly launched process: after granting, fully quit and reopen the app.',
          bannerTitle:'Agent is controlling your computer',
          bannerNote:'The agent can see your screen and move the mouse and keyboard. You can stop it at any time.',
          bannerStop:'Stop control',
          grantTitle:'The agent requests control of your computer',
          grantDesc:'If you allow it, the agent can see your screen and operate the mouse and keyboard for this session. Only allow this if you trust the current task; consequential actions still ask for confirmation one by one.',
          grantAllow:'Allow control for this session',
          grantDeny:'Deny',
          confirmTitle:'Confirm this action',
          confirmActionLabel:'Action',
          confirmElementLabel:'Target element',
          confirmOnce:'Allow this once',
          confirmDeny:'Deny',
          fullTextWarning:'The agent will type exactly this text.',
          textTooLongToPreview:'The text is too long to preview in full.',
          buttonName:{ left:'Left', right:'Right', middle:'Middle' },
          confirmClick1:'click', confirmClick2:'double-click', confirmClick3:'triple-click',
          confirmClick:(button, verb)=>`${button} ${verb}`,
          confirmClickAt:(button, verb, point)=>`${button} ${verb} at ${point}`,
          confirmTypeCount:count=>count===1?`Type ${count} character`:`Type ${count} characters`,
          confirmKeyChord:chord=>`Press ${chord}`,
          confirmHoldKey:(chord, ms)=>`Hold ${chord} for ${ms} ms`,
          confirmKeyChordMasked:(chord, count)=>chord?(count===1?`Press ${chord} + ${count} masked character`:`Press ${chord} + ${count} masked characters`):(count===1?`Press ${count} masked character`:`Press ${count} masked characters`),
          confirmHoldKeyMasked:(chord, count, ms)=>chord?(count===1?`Hold ${chord} + ${count} masked character for ${ms} ms`:`Hold ${chord} + ${count} masked characters for ${ms} ms`):(count===1?`Hold ${count} masked character for ${ms} ms`:`Hold ${count} masked characters for ${ms} ms`),
          confirmDrag:(from, to)=>`Drag from ${from} to ${to}`,
          confirmMouseDown:button=>`Press and hold the ${button} mouse button`,
          confirmMouseUp:button=>`Release the ${button} mouse button`,
          screenshotCaption:'Screenshot',
          screenshotLoading:'Loading screenshot…',
          actionFailed:e=>`Action failed: ${e}`,
        },
        uiMultiAgent: {
          toggleLabel:'Swarm Mode',
          toggleHint:'Let this conversation delegate to subagents (swarm collaboration); while on, the subagent cap in advanced settings is ignored; turning it off recycles the engine and cancels any subagents still running',
          spawnedAgentsRow:count=>`Pinvou spawned ${count} agent${count === 1 ? '' : 's'}`,
          spawnedAgentsRowHint:'Click to view the subagent list',
          runningAgentsTitle:'Running agents',
          runningAgentsCount:count=>`${count} running`,
          runningAgentsCollapse:'Collapse running agents', runningAgentsExpand:'Expand running agents',
          drawerTitle:role=>`Subagent · ${role}`, close:'Close', loadingTranscript:'Loading conversation…',
          emptyTranscript:'No saved conversation yet. The subagent may have just started.',
          panelResize:'Resize panel', panelResizeHint:'Drag to resize; double-click to reset',
          agentCard:{ working:'Working', completed:'Completed', failed:'Failed', spawnFailed:'Dispatch failed', interrupted:'Interrupted', cancelled:'Cancelled' },
          agentsListTitle:'Subagents', agentsEmpty:'No subagents have been dispatched in this conversation yet.', backToAgents:'Back to subagent list',
          agentsListSummary:(roots,total)=>roots === total ? `${total} total` : `${roots} direct · ${total} total`,
          childAgentCount:count=>`${count} child${count === 1 ? '' : 'ren'}`, expandChildren:name=>`Expand children of ${name}`, collapseChildren:name=>`Collapse children of ${name}`,
          coordinationRow:action=>`Coordination · ${action}`,
          roleCards:{ scout:'Research expert', manager:'Planning expert', builder:'Execution expert', reviewer:'Review expert', general:'General-purpose agent' },
          blockedTag:'Blocked', pendingTag:'Queued',
          agentPending:'This subagent is queued or starting; no conversation has been recorded yet.',
          agentNoTranscript:error=>error ? `The subagent ended before creating a conversation record: ${error}` : 'The subagent ended without creating a conversation record.',
          showEarlierTranscript:count=>`Show ${count} earlier execution item${count === 1 ? '' : 's'}`,
          listReadFailed:'⚠️ Failed to read the subagent list; retrying automatically…',
          webActionHint:'Swarm conversations are read-only on the web; perform this action on desktop.',
          transcriptReadFailed:'⚠️ Failed to read the conversation; retrying automatically…',
        },
        uiAuxChat: {
          openLabel:'Auxiliary chat',
          panelTitle:'Auxiliary chat',
          landingHint:'This conversation is independent of the main task: questions and answers here do not affect the main task’s execution or context.',
          emptyState:'Nothing here yet. Ask the auxiliary session a question to start a separate discussion.',
          inputPlaceholder:'Type a question. Enter to send, Shift+Enter for a new line',
          send:'Send',
          busyHint:'The auxiliary session is replying; please wait…',
          bindingHint:'Preparing the auxiliary session…',
          sendingHint:'Sending…',
          newTopic:'New topic',
          newTopicConfirm:'Click again to confirm: discard this auxiliary conversation and start over',
          newTopicConfirmShort:'Tap again to confirm',
          sendFailed:'Failed to send. Please try again.',
          ensureFailed:'Failed to start the auxiliary session. Start a new topic or try again later.',
          discardFailed:'Failed to restart the auxiliary chat; the previous transcript may have been discarded. Switch tasks or reopen the panel to reload it, or retry “New topic”.',
          close:'Close',
          quoteAction:'Quote to auxiliary chat',
          quoteChipCount:count=>`${count} quoted ${count === 1 ? 'selection' : 'selections'}`,
          quoteRemove:'Remove this quote',
          quoteLimitSingle:'A selection can contain at most 8,000 characters',
          quoteLimitCount:'At most 8 selections can be quoted at once',
          quoteLimitTotal:'Quoted selections cannot exceed 16,000 characters in total',
          quoteDuplicate:'This selection is already quoted and will not be added again',
        },
        uiHomeMode: { work:'Work', code:'Code', providerSettings:'Provider settings' },
        uiAttachments: { parsing:'Parsing', failed:'Failed', remove:name=>`Remove attachment ${name}`, dropRelease:'Drop to attach', dropWebTitle:'Add anything', dropWebHint:'Drop any file here to add it to the conversation', fileTooLarge:'The file exceeds the 20 MB attachment limit and cannot be added', archiveTooManyEntries:'The archive contains more than 50 entries and cannot be added', archiveExpandedTooLarge:'The archive expands beyond 100 MB and cannot be added', archiveUnsafeEntry:'The archive contains unsafe links or paths and cannot be added' },
        uiCodex: codexEn,
        uiAcpProviders: acpProvidersEn,
        uiProjects: { convertToProject:'Convert to project', projectNamePlaceholder:'Project name', renameProject:'Rename project', deleteProject:'Delete project', deleteConfirmLabel:'Delete this project?', deleteProjectHint:'Conversations are kept and return to ungrouped', moveToProject:'Move to project…', moveToUngrouped:'Move to ungrouped', alreadyUngrouped:'Already ungrouped', folderUnavailable:'Folder unavailable', rebindFolder:'Rebind', moveConfirmTitle:'Move conversation into this project?', moveConfirmBody:(project, folder)=>`The conversation moves into "${project}". Its folder ${folder} stays outside the project; only this conversation is filed.`, moveConfirm:'Move', searchPlaceholder:'Search projects', noProjects:'No projects yet', currentProject:'Current project', movedNotice:'Conversation moved', movedNoticeWithFolder:folder=>`Conversation moved; folder ${folder} added`, noMatchProject:'No matching projects', opFailed:'Project operation failed' , rebindTitle:'Rebind project folder', rebindConfirm:'Move and rebind', rebindSessionsHint:()=>'Every session under this folder will switch to the new directory', rebindSuccess:n=>`Rebound ${n} session(s)`, rebindUpToDate:'No sessions needed rebinding', rebindOldExistsWarn:'The original folder still exists; the two locations will point to different directories. Continue anyway?', rebindPartial:(ok, failed)=>`Rebound ${ok} sessions; ${failed} failed - retry the rest`, rebindBusyAfter:n=>`${n} sessions started a new turn during the rebind; if they misbehave, retry once when idle` , rebindCarryoverPending:n=>`${n} session(s) were moved but their runtime could not be switched yet; retry once when idle`, rebindRetryRemaining:'Retry remaining', rebindFailedSessions:'Failed sessions', rebindInProgress:'Another folder rebind is already in progress' , rebindBusyHint:'Those sessions are still running a turn. Wait for them to go idle, then retry', rebindToRoot:'That destination is the filesystem root. Pick an ordinary folder instead', rebindToNested:'The two folders cannot sit inside one another', rebindToUnusable:'That destination folder cannot be used. Pick another one', rebindRootsConflict:'That folder overlaps another project’s folder. Pick one outside the other projects’ folders', rebindRootsPersist:'Could not save the rebind to disk. Check free space and permissions, then retry', rebindRuntimeStarting:'An agent process is starting up; session state could not be checked yet. Retry in a moment', rebindLegacyTableUnsynced:'Nothing was moved: the session records could not be synced. Make the folder writable and retry', rebindLegacyTableCorrupt:'Nothing was moved: the legacy session table is corrupt. Repair or remove it, then retry', rebindRootsExpand:'Show all unavailable folders', rebindRootsCollapse:'Show only the first unavailable folder' },
        uiCodexWorkspace: { changes:{added:'Added',modified:'Modified',deleted:'Deleted',renamed:'Renamed',copied:'Copied',conflict:'Conflict',untracked:'Untracked',unknown:'File'}, origins:{session:'This session',preexisting:'Existed before session',preexisting_modified:'Existed before session · modified in this session',unknown:'Source not recorded'}, addedPath:path=>`Added ${path}`, addPath:path=>`Add ${path} to conversation`, added:'Added to conversation', add:'Add to conversation', back:'Back to workspace list', copyPath:'Copy relative path', reveal:'Show in file manager', open:'Open with system application', reading:'Reading…', noDiff:'No text diff to display',  tooLarge:'The file is too large for an embedded preview.', unsupported:'This file does not support embedded preview.', openHint:'You can open it with a system application.', truncated:'Content is too large; only the beginning is shown.', resize:'Resize workspace', resizeHint:'Drag to resize; double-click to restore default', title:'Workspace', temporary:'Temporary workspace', refresh:'Refresh workspace', close:'Close workspace', files:'Files', changed:'Changes', search:'Search files', noFiles:'No matching files', noBaseline:'This older session has no creation baseline, so changes made by this session cannot be determined.', branch:'Branch', staged:'Staged', noChanges:'No workspace changes', copyContent:'Copy content', copied:'Copied', closeViewer:'Close preview', loadFailed:'Failed to load file', resizeWidth:'Resize viewer width', resizeHeight:'Resize viewer height', resizeCorner:'Resize viewer; double-click to restore default', fontDecrease:'Decrease font size', fontIncrease:'Increase font size', openInNewWindow:'Open in code reader', diffSuffix:'(diff)', readerTitle:'Code Reader', readerEmpty:'Choose "Open in code reader" from a workspace file preview; files accumulate here as tabs.', closeTab:'Close tab', noSessionChanges:'Once a session is created, changes made by the session will be listed here.' },

        checkUpdate: 'Check for Updates', checking: 'Checking…', upToDate: 'Up to date', updateInstallFailed: 'Update failed',
        newVersionFound: 'New version available', updateNotes: 'Release Notes',
        downloadInstall: 'Download & Install', downloading: 'Downloading…', installing: 'Installing…',
        downloadInstallRestart: 'Update & Restart',
        cancel: 'Cancel', cancelling: 'Cancelling…',
        updateComplete: 'Update complete, restart to apply', restartNow: 'Restart Now',
        updateInstallerStarted: 'Installer started, the app will close automatically',
        updateCheckFailed: 'Check failed',
        depCheckTitle: 'Dependency Check', depChecking: 'Checking…', depRecheck: 'Check again',
        depAllOk: 'All dependencies ready', depMissingSuffix: ' dependency(ies) missing (those features will be unavailable)',
        depInstalling: 'Installing…', depInstallBtn: 'Install', depGoInstall: 'Install', depInstallNote: 'Installs missing deps via elevated apt (needs network or an apt source)', depInstallNoteWindows: 'Downloads missing models; installs LibreOffice through elevated winget (requires network)', depInstallNoteMac: 'Installs missing deps via Homebrew (needs network; install Homebrew first from brew.sh if missing)',
        dep_pdf: 'PDF text extraction', dep_office_modern: 'Office docs (docx)', dep_office_legacy: 'Legacy Office / slides & sheets',
        dep_ocr: 'Image / scanned-PDF OCR', dep_archive: 'Archives (zip/rar/7z)', dep_email: 'Email (.eml / .msg)', dep_voice_asr: 'Local speech recognition',
        dep_voice_asr_model: 'Speech recognition model (SenseVoice q8)', dep_knowledge_embedding_model: 'Knowledge embedding model (bge-m3)',
        depHint_email_manual: 'Manual install required: run `sudo cpan -i Email::Outlook::Message` in Terminal (no Homebrew formula). See https://metacpan.org/pod/Email::Outlook::Message',
        communityQrAlt: 'Pinvou QQ group QR code', communityQrPlaceholder: 'QR placeholder', communityChannelTag: 'Chinese QQ community',
        communityQrHint: 'Scan with mobile QQ to join, or copy the group ID to search manually.',
        communityGroupLabel: 'Group ID', communityGroupPending: 'To be added', communityCopyGroup: 'Copy group ID', communityCopied: 'Group ID copied',
        communityDiscussions: 'GitHub Discussions',
        communitySupportNotice: 'The QQ group is a community space, not an official support or ticket channel. Continue to submit reproducible issues through feedback, and never post suspected vulnerabilities in the group.',
        feedbackTitle: 'Help & Feedback', feedbackDesc: 'Send issues or suggestions with screenshots or short videos.',
        feedbackOpen: 'Send feedback', feedbackDialogTitle: 'Send feedback', feedbackType: 'Type',
        feedbackIssue: 'Issue', feedbackSuggestion: 'Suggestion',
        feedbackSubject: 'Title', feedbackSubjectPh: 'Short summary (optional)',
        feedbackBody: 'Details', feedbackBodyPh: 'Describe what happened, what you expected, or your suggestion',
        feedbackAttachments: 'Attachments', feedbackAddAttachment: 'Add image or video', feedbackNoAttachments: 'No attachments',
        feedbackAttachmentHint: 'Supports png, jpg, jpeg, gif, webp, mp4, mov, webm; up to 5 files.',
        feedbackPrivacy: 'This sends your text, selected files, and non-sensitive app context. Chats and keys are not attached automatically.',
        feedbackSubmit: 'Submit', feedbackSubmitting: 'Submitting…', feedbackSubmitted: 'Feedback submitted. Thank you.',
        feedbackRetry: 'Retry', feedbackCloseConfirm: 'Feedback draft has not been submitted. Close it?', feedbackCloseAnyway: 'Close anyway',
        feedbackBodyRequired: 'Please enter feedback details.', feedbackUnsupportedFile: 'Unsupported attachment format.',
        feedbackTooManyFiles: 'You can attach up to 5 files.', feedbackPickUnavailable: 'File picker is unavailable.',
        chatGreeting: 'Hi — what would you like to work on today?', artifacts: 'Artifacts', placeholder: 'Ask PINVOU or enter a command', disclaimer: 'PINVOU may display inaccurate info, so double-check.', ctxUsage: 'Context',
        chatInputLimitReached: n => `The ${n}-character limit has been reached.`,
        searchPlaceholder: 'Search chats', searchNoResults: 'No matching chats', searchDateAll: 'All',
        searchBatchManage: 'Select', searchBatchDone: 'Done', searchSelectAll: 'Select all', searchSelectedCount: n => `${n} selected`,
        searchArchivedEntry: 'Archived', searchArchivedEmpty: 'No archived chats', searchRestore: 'Restore', searchArchivedAt: d => `Archived ${d}`, searchPanelChats: 'Chats',
        // —— Card Pool (cp*) ——
        cpSubtitle: 'Expert Capability Deck',
        cpTagline: "Equip an expert card so the AI handles this chat to that field's professional standard.",
        cpTaglineTitle: "Equipping an expert card injects the field's methodology, workflow and delivery standards into the current chat.",
        cpTarget: 'Equip Target', cpBannerEquipped: 'Equipped', cpBannerNone: 'Not equipped',
        cpNoSession: 'No active chat — create or open one first',
        cpNewCard: 'New Card', cpNewCardTitle: 'Create a custom card manually',
        cpLoading: 'Loading card deck…', cpLoadError: 'Failed to load card deck',
        cpMyCards: 'My Cards', cpDept: 'Department', cpResetFilter: 'Reset Filters',
        cpSearchPh: 'Search experts: name / description / department',
        cpNoMatch: 'No matching experts — try widening the filters',
        cpBadgeEquipped: 'Equipped', cpBadgeUser: 'Custom', cpNoDesc: '(no description)',
        cpEdit: '✎ Edit', cpDelQ: 'Del?', cpDelCardTitle: 'Delete this custom card',
        cpUnequip: 'Remove', cpEquip: 'Equip',
        cpShowMore: n => `Show more (${n})`,
        cpCurrentChat: 'current chat',
        cpToastDeleted: n => `Deleted: ${n}`, cpToastDelFailed: 'Delete failed',
        cpToastUnequipped: n => `Removed: ${n}`,
        cpToastEquipped: (target, n) => `Equipped to "${target}": ${n}`,
        cpToastSaved: n => `Saved: ${n}`, cpToastCreated: n => `Created: ${n}`,
        cpSavedTitle: 'Saved to card deck', cpSavedDesc: n => `"${n}" added to your cards`, cpSavedLater: 'Not now', cpSavedView: 'View',
        cpFullBody: 'Full persona (injected once when equipped)',
        cpBodyLoading: 'Loading persona body…', cpBodyLoadFailed: '(failed to load body)',
        cpEquippedTap: 'Equipped · tap to remove',
        cpAll: 'All', cpEquipShort: 'Equip', cpDetailUnequip: 'Unequip', cpReqPh: 'Required', cpMarkdownHint: 'Markdown supported', cpDelThisConfirm: 'Confirm delete',
        cpEmptyHint: 'Try another search term, or tap + to create a new card.', cpMenuEdit: 'Edit',
        cpAICreate: 'AI Card', bnrTitleHi: 'Generate an expert card in one tap', bnrStart: 'Start →', bnrStep1: 'Describe', bnrStep2: 'AI generates', bnrStep3: 'Add to pool',
        chooserRecommend: 'Recommended', chooserAIDesc: 'Describe what you need; AI generates the card', chooserManualTitle: 'Manual', chooserManualDesc: 'Fill in name and persona yourself',
        bnrFaces: [['Product','Product design'],['ML Engineer','AI models'],['Growth','Growth ops'],['UI Designer','Interface'],['Data Analyst','Insights']],
        cpEditCard: 'Edit Card',
        cpFieldName: 'Name', cpFieldDesc: 'One-line description', cpFieldDescPh: 'Shown on the card — 15 characters or fewer',
        cpFieldBody: 'Persona Body', cpFieldBodyHint: '(full capability profile injected to the AI: duties / workflow / rules / deliverables)',
        cpNamePh: 'e.g. Short-video Script Expert',
        cpBodyPh: '# Role\n\nYou are a ...\n\n## Core Duties\n- ...\n\n## Workflow\n1. ...\n\n## Key Rules\n- ...',
        cpErrName: 'Card name is required', cpErrBody: 'Persona body is required',
        cpErrSave: e => `Save failed: ${e}`,
        cpConfirmDelete: 'Delete?', cpDelete: 'Delete', cpCancel: 'Cancel', cpDeleteThis: 'Delete card',
        cpSaving: 'Saving…', cpSaveEdit: 'Save Changes', cpCreate: 'Create Card',
        cpLanyardEquip: 'Equip Card', cpLanyardEquipTitle: 'Equip a card',
        cpLanyardSwap: 'Click to switch expert', cpLanyardRemove: 'Remove',
        cpDraftLabel: 'Card Draft', cpDraftSaved: 'Saved', cpDraftSavedTitle: 'A custom card with this name already exists',
        cpDraftView: 'View / Save', cpDraftViewTitle: 'View full content and save to card deck',
        cpDesigning: '🃏 Designing card…',
        cpIntroTitle: 'What kind of expert card do you want? Just tell me what you need.',
        cpEquipBubbleSys: 'System',
        cpEquipBubbleTitle: n => `Boss equipped an expert card for you: ${n}`,
        cpDescLabel: 'Description',
        cpEquipBubbleNote: "Full capability profile injected — the AI will handle following tasks with this expert's methodology.",
        cpTargetMarkTitle: 'Equip target · experts picked in the card deck are injected into this chat',
        riGenerating: 'Generating…', riDelQ: 'Delete?', riDelConfirm: 'Confirm delete', riRename: 'Rename', riPin: 'Pin', riUnpin: 'Unpin', riOpenFolder: 'Open folder', riAwaitingInput: 'Awaiting your input', riMore: 'More',
        // —— Chat & global chrome ——
        appTitle: 'PINVOU AI Assistant (Beta)', winMin: 'Minimize', winMax: 'Maximize', winClose: 'Close',
        sidebarCollapse: 'Collapse sidebar', sidebarExpand: 'Expand sidebar',
        skillPrefill: name => `I want to use ${name} to: `,
        queuedTag: 'Queued', queuedCancel: 'Remove', queuedPrioritize: 'Make next', queuedEdit: 'Edit queued message',
        queuedSave: 'Save', queuedEditCancel: 'Cancel editing', queuedActionFailed: 'The action did not complete; the queued message was left unchanged. Try again later.', queuedEmpty: 'Message cannot be empty', queuedEditInterrupted: 'The queued message left the queue before this edit was saved; the modified text was discarded',
        queueMsg: 'Send to chat', queueMsgTip: 'While generating: text goes into this turn when possible (otherwise queued); attachments always queue',
        interruptMsg: 'Interrupt & send', interruptMsgTip: 'Cancel current AI task and send immediately',
        attachParsing: 'Parsing…', attachFailed: 'Failed', attachProtectedLocation: 'File location is protected', attachAdd: 'Attach file',
        attachFromDevice: 'Upload from this device', attachFromHost: 'Choose from desktop instance', attachUploading: pct => `Uploading ${pct}%`,
        attachmentOpen: 'Open with default app', attachmentDownload: 'Download attachment', attachmentCopyAddress: 'Copy address',
        attachmentCopyName: 'Copy filename', attachmentReveal: 'Show in file manager',
        voiceStart: 'Voice input', voiceStop: 'Stop recording', voiceCancel: 'Cancel voice input', voiceCancelShort: 'Cancel', voiceRetry: 'Retry', voiceRetryAgain: 'Try again',
        voiceRequesting: 'Requesting microphone permission…', voiceRecording: 'Recording, press again to stop', voiceTranscribing: 'Transcribing voice…', voiceStopHint: 'Press Alt (Option) again to stop',
        voicePostprocessing: 'Polishing voice text…', voiceTaskPostprocessing: 'Rewriting voice task…',
        voiceCompleted: 'Voice text added', voiceTaskSent: 'Voice task sent', voiceClose: 'Close voice notice', voiceGotoDeps: 'Open dependency check',
        voiceDictationMode: 'Insert into input', voiceTaskMode: 'Send task', voiceInputFailed: 'Voice input failed',
        voiceEmptyResultTitle: 'No speech detected', voiceEmptyResultHint: 'Move closer to the microphone, or try again in a quieter place.',
        voiceEditMode: 'Edit current input', voiceStructuredPostprocessing: 'Structuring voice text…',
        voiceEditPostprocessing: 'Editing current input…', voiceEditPreviewReady: 'Voice edit ready to review', voiceEditNoChange: 'Voice edit made no changes',
        voiceEditPreviewTitle: 'Voice edit preview',
        voiceEditOriginal: 'Original', voiceEditResult: 'Edited', voiceEditApply: 'Apply edit',
        voiceEditApplyAndSend: 'Apply and send', voiceEditCancel: 'Cancel edit',
        voiceIntroTitle: 'Use voice input two ways',
        voiceIntroClose: 'Close voice shortcut intro',
        voiceIntroShortcutLabel: 'Shortcut',
        voiceIntroComboLabel: 'With text',
        voiceIntroModeLabel: 'Mode',
        voiceIntroDictationMode: 'Voice input',
        voiceIntroDictationSteps: ['Press Alt to start recording', 'Press Alt again to stop', 'The transcript is inserted into the input box for editing'],
        voiceIntroTaskMode: 'Voice edit',
        voiceIntroTaskSteps: ['Press Alt when the input box has text', 'Press Alt again to stop recording', 'Review the edited text, then send it when ready (needs Smart Organize on; with it off the gesture stays dictation)'],
        voiceIntroTaskExampleLabel: 'Example edit',
        voiceIntroTaskExample: '"Turn this into three concise bullets and make the tone more formal."',
        voiceIntroGotIt: 'Not now, continue voice',
        voiceIntroDone: 'Got it',
        voiceIntroStart: 'Got it, start voice input',
        voiceIntroEnable: 'Enable and continue',
        voiceShortcutEnableTitle: 'Enable voice shortcuts',
        composerTools: 'Tools', composerModelLabel: name => `Model: ${name}`, composerConnectedServices: 'Connected services', composerConnected: 'Connected', composerInstalledTools: 'Connected tools', composerInstalledSkills: 'Skills', composerNoTools: 'No tools installed', composerToolOn: 'On', composerManageTools: 'Manage tools',
        composerMode: 'Skill', composerModeNone: 'No skills available',
        composerBuiltinAuto: 'Built-in · Auto', composerSkillInUse: 'In use', composerSkillAllDisabled: 'All skills for this session type are off — enable them above', composerModeControlled: 'Controlled by mode', composerReadonlySwitch: 'Controlled by mode, cannot be toggled manually',
        composerProjectSkills: 'Project skills', composerProjectSkillsDesc: 'Scan built-in project skills', composerProjectSkillsWarning: 'Project skill files may contain malicious instructions (prompt injection) — enable only for trusted projects', composerProjectSkillsHelpTitle: 'About project skills', composerProjectSkillsHelpBody: 'When enabled, code sessions bound to a project scan the project\'s convention skill directories, and the model can load and use those skills.', composerProjectSkillsHelpDirsLabel: 'Scanned directories (highest priority first)', composerProjectSkillsHelpDirs: '.agents/skills\n.pinvou/skills\nskills\n.opencode/skills\n.claude/skills\n.cursor/skills\n.codewhale/skills',
        resend: 'Resend', editResend: 'Edit & resend', messageSending: 'Sending…', messageAccepted: 'Sent', messageFailed: 'Send failed', messageOutcomeUnknown: 'Send status is unknown. Check the conversation list.', selectAllMsg: 'Select all', copyMsg: 'Copy', pasteMsg: 'Paste', copied: 'Copied', backToBottom: 'Back to bottom',
        artifactLabel: 'Artifact', clickToOpen: 'Open →',
        toolRunning: 'Running...', toolDone: 'Done', toolFailed: 'Failed', shellCancelFailed: 'Cancel failed',
        tsDone: 'completed', tsInProgress: 'in progress', tsPending: 'pending', tsLine: 'L',
        receiptNote: '⋯ Long output — showing the beginning only', receiptEmpty: '(long output, not expanded)',
        listDirCount: n => `${n} entries`,
        grepHits: n => `${n} matches`, grepFiles: n => ` · ${n} files searched`, grepTruncated: ' · truncated',
        shellStdoutTrunc: ' · stdout truncated',
        todoProgress: pct => `Progress ${pct}%`,
        planReady: '✨ Plan ready', planEmpty: '(empty plan)', planLabel: '📋 Plan', planTodos: '✅ Todos',
        planNext: 'Next:', planGo: '✅ Do it', planEdit: '✏️ Revise', planDrop: '🚪 Never mind', planRevisePrefill: 'Revise the plan: ',
        stuckExec: tries => `🛑 The AI is stuck (auto-retried ${tries} times without producing real files). You can:`,
        stuckPlanPre: '⚠️ In Plan mode the AI called', stuckPlanPost: 'but the whitelist blocked it. Plan mode is discussion-only. Two ways out:',
        handled: 'Handled',
        stuckReplan: '📋 Ask AI to re-plan', stuckSelf: "I'll do it myself", stuckGo: 'Just do it (skip planning)',
        // Pinvou review card. 品/悟 glyphs are brand marks, kept; surrounding text follows language.
        pvLoadingPin: 'Pinvou 品 · Review', pvLoadingWu: 'Pinvou 悟 · Explore',
        pvLoadingPinSub: 'Checking point by point, hunting for issues', pvLoadingWuSub: 'Comparing completeness dimensions, finding gaps',
        pvLoadingHint: isLocal => isLocal ? 'Local model is thinking, usually 5–30s, hang tight…' : 'The model is thinking, usually a few seconds…',
        pvFail: 'Failed: ', pvSkipped: 'Skipped', pvPerspective: ' view', pvAlsoInvolves: 'also covers',
        pvVerdictPass: '✓ Passed, ready to ship', pvFramework: 'Completeness framework', pvDims: ' dims', pvSuggest: 'Suggest ', pvNeedsVerify: 'Needs check',
        pvActAdopt: 'Adopt', pvActAsk: 'Have AI ask me', pvActVerify: 'Have AI verify', pvActConfirmed: "It's fine",
        pvActModify: 'Have AI fix', pvActAccept: 'Accept as is', pvActFill: 'Fill in', pvActSkip: 'Skip for now',
        pvHandToAi: n => `Hand to AI (${n})`, pvSkip: 'Skip', pvSkipTitle: 'Skip this review card for now',
        pvBtnPinTitle: '品 · Review: surface risks, omissions, and drift from your intent — fix each via AI or accept as is',
        pvBtnWuTitle: '悟 · Explore: a domain expert lists dimensions and ideas the artifact could still add; add picks in one click',
        pvBtnPinLabel: '品 · Review', pvBtnWuLabel: '悟 · Explore',
        cbTitle: '🛑 Blocked a dangerous operation', cbWant: 'The AI wants to run:', cbWhy: 'Why it was blocked',
        cbNote: "This built-in safety guard is always on. The AI is never allowed to do this; if you really want it, run it yourself.",
        cbTechShow: 'Technical details', cbTechHide: 'Hide technical details', cbCmdUnknown: '(unknown command)',
        rsRoot: 'Tried to delete the system root — would destroy the entire system',
        rsHome: 'Tried to delete your home directory — almost all personal files live there, unrecoverable',
        rsRecursive: 'Recursively deletes a whole folder and everything inside it, unrecoverable',
        rsForce: 'Force delete — skips the trash, unrecoverable',
        rsForkbomb: 'Would instantly exhaust system resources and freeze the machine',
        rsOverwrite: 'Would overwrite disk data directly',
        rsFormat: 'Would format the disk — all data gone',
        rsDefault: 'This command could damage your files or system',
        uiqTitle: 'Pinvou needs your input',
        uiOtherTitle: '💬 Other (write your own)', uiOtherDesc: 'If none of the options fit, say it in your own words', uiOtherPh: 'Type what you want to say...', uiSubmit: 'Submit',
        uiSubmitted: '✓ Submitted', uiCancelled: '✕ Cancelled', uiSubmitting: 'Submitting…', uiSubmitFailed: e => `⚠️ Submit failed: ${e}`,
        apEmpty: 'No artifacts yet', apOpenDir: 'Open containing folder', apOpenExt: 'Open externally', apSelect: 'Select an artifact to preview',
        apLoading: 'Loading...', apMissing: 'File missing or deleted', apReadFail: e => `Read failed: ${e}`,
        apNoImg: 'Inline image preview not supported yet', apNoPreview: 'Preview not supported for this type', apOpenExtBtn: '↗ Open externally',
        apTabList: 'Artifacts', apTabPreview: 'Preview', apPreviewHint: 'Pick a file in the Artifacts tab to preview',
        apBtnOpen: 'Open', apBtnLocate: 'Show in folder', apLocLabel: 'Location', apMtimeLabel: 'Modified', apLastMod: 'Modified',
        apConverting: 'Generating preview…', apConvertFail: 'Preview failed', apUnsupported: 'Inline preview not available for this type; use Open below',
        apKinds: { md:'Markdown', html:'HTML document', image:'Image', pdf:'PDF document', docx:'Office document', xlsx:'Spreadsheet', legacy_office:'Office document', text:'Text file', binary:'File' },
        apMdSaving: 'Saving...', apMdDirty: 'Unsaved', apMdSaveFailed: 'Save failed',
        apMdExternalUpdateBlocked: 'The file changed externally. Your unsaved edits were kept and not overwritten.',
        apMdExternalRemovalBlocked: 'The file was deleted externally. Your unsaved edits remain in this editor; copy them or restore the original file before leaving.',
        apMdAiEdit: 'AI edit', apMdAiInstructionPlaceholder: 'How should AI edit this?',
        apMdComposerReplaceConfirm: 'This will fill the chat input with the AI edit instruction. Any existing draft may be replaced. Continue?',
        apMdAiPrompt: ({ path, selectedText, instruction, fence = '```' }) => `Please edit the selected content in this Markdown artifact.\n\nFile path: ${path}\n\nSelected content:\n${fence}markdown\n${selectedText}\n${fence}\n\nEdit request:\n${instruction}`,
        depts: { academic:'Academic', design:'Design', engineering:'Engineering', finance:'Finance', 'game-development':'Game Dev', gis:'GIS', hr:'HR', legal:'Legal', marketing:'Marketing', 'paid-media':'Paid Media', product:'Product', 'project-management':'Project Mgmt', sales:'Sales', security:'Security', 'spatial-computing':'Spatial Computing', specialized:'Specialized', 'supply-chain':'Supply Chain', support:'Support', testing:'Testing', tool:'Tools' }
};

dictEn.uiToolDetails.flow = {
  incomplete:name=>`${name} connection incomplete`, connected:name=>`${name} connected`, connecting:name=>`Connecting ${name}`,
  cancel:'Cancel', extracting:pct=>`Extracting ${pct}%`, elapsed:seconds=>`${seconds}s elapsed`, installStarting:'Online install: starting…', browserOpened:'Browser sign-in page opened',
  browserHint:'Confirm the sign-in in your browser. Reopen the page if it did not appear.', reopen:'Reopen', qrAlt:name=>`${name} QR code`,
  authorizeStep:'Step 2 of 2: scan to authorize', registerStep:'Step 1 of 2: scan to register the app',
  scanLogin:name=>`Sign in to ${name}`, scanHint:name=>`Scan with the ${name} app and confirm`, userCode:'Verification code',
  openBrowser:'Open in browser ↗', connectionIncomplete:'Connection incomplete', close:'Close', retry:'Retry',
};

dictEn.uiToolDetails.actions = { connectedTmeet:'Tencent Meeting connected', enabled:'Connected. Tool switches are off by default; turn this on in the composer tools list', disconnectedTmeet:'Tencent Meeting disconnected', connectingIma:'Connecting Tencent ima', validatingIma:'Validating OpenAPI credentials…', connectedIma:'Tencent ima connected', imaEnabled:'IMA connected. Tool switches are off by default; turn this on in the composer tools list.', imaFailed:'IMA connection failed', imaSkillsFailed:err=>`IMA connection did not complete: the skills were installed but persisting their default-off consent state failed, so this connection was rolled back (reconnect required); after reconnecting, switch the skills off in the composer tools list if they come up enabled: ${err}`, disconnectedIma:'Tencent ima disconnected', operationFailed:'Operation failed. Please retry' };

dictEn.uiToolDetails.tmeetSteps = [{key:'runtime',label:'Prepare runtime',sub:'Use the app-provided Node runtime'},{key:'cli',label:'Install connector',sub:'tmeet · about 40 seconds on first use'},{key:'qr',label:'Sign in',sub:'Tencent Meeting authorization page'}];

// Retained for parity with the private implementation this tree aligns with; it has
// no consumer in the public tree (the flow card always renders localized copy).
dictEn.uiToolDetails.showRawErrors = false;

Object.assign(dictEn.uiToolDetails.tools, {
  'session-reader':{ title:'Session Reader', subtitle:'Read-only access to other local sessions', latency:'Local', desc:'Lets AI read other Pinvou session histories on this device in paged, read-only form: after you @-reference a session in the composer, AI fetches its contents on demand (installed by default; reads local session files only, no network, nothing uploaded). Built-in plugins cannot be uninstalled; session reading can be turned off via the builtin feature switch.', welcomeQueries:['Summarize the session I referenced','Where did my last session leave off'] },
  weather:{ title:'Amap Weather', subtitle:'Real-time weather and multi-day forecasts from Amap', desc:'Query real-time weather and multi-day forecasts for cities nationwide through the Amap Web Service API. Enter your own Amap Web Service API Key; the key is written only to this device\u2019s system credentials.', configTitle:'Amap Weather Key', configDescription:'The Key is stored only in this device\u2019s credentials and is not written to mcp.json.', configDocLabel:'Create a Web Service Key', configFields:[{key:'AMAP_KEY', label:'API Key', helpText:'Choose the \u201CWeb Service\u201D type.', placeholder:'Paste your Amap Web Service Key'}], welcomeQueries:['Weather in Hangzhou today','Will it rain in Beijing this week','What to wear in Shanghai tomorrow'] },
  iwencai:{ title:'iWenCai', subtitle:'A-share quotes, financials, screening, macro, and news', desc:'Provides 12 financial query tools based on the official iWenCai API. Enter your own iWenCai API Key; the key is written only to this device\u2019s system credentials.', configTitle:'iWenCai Key', configDescription:'The Key is stored only in this device\u2019s credentials and is not written to mcp.json.', configDocLabel:'Open iWenCai SkillHub', configFields:[{key:'IWENCAI_API_KEY', label:'API Key', helpText:'Open any official Skill and copy it from \u201CInstallation\u201D.', placeholder:'Paste your IWENCAI_API_KEY'}], welcomeQueries:['Latest Moutai share price','How is the market doing today','Bank stocks with P/E below 10','Recent rate-cut news'] },
  card3:{ title:'QQ Mail API', subtitle:'Smart email sending/receiving and thread summarization', desc:'Provides standard interfaces for sending, receiving, searching, and organizing email. Combined with an LLM, you can read mail in natural language, summarize long threads, and automatically archive and manage folders.' },
  card5:{ title:'Lexiang Docs Connector', subtitle:'Full-text search across enterprise knowledge docs', desc:'Search, create, and manage documents in Lexiang knowledge bases through the API. Supports batch Markdown import, tag-based organization, and real-time subscriptions to team document updates.' },
  'tencent-docs':{ title:'Tencent Docs MCP', subtitle:'Official remote MCP: read/write online docs, sheets, and slides with collaboration', latency:'Cloud', desc:'Connect the official Tencent Docs remote MCP (general docs plus PPT/Word/Excel precision-editing services): create, read, and edit online documents, spreadsheets, slides, mind maps, and smart sheets under your own account; manage knowledge-base spaces and files; clip web pages and upload local files to the cloud. The official skill (scenario routing + tool API reference) is set up automatically at install time. Requires your Tencent Docs personal token (scan a QR code with QQ/WeChat on the official page); the token is written only to this device\u2019s system credentials.', configTitle:'Connect Tencent Docs', configDescription:'The token is stored only in this device\u2019s system credentials, never written to mcp.json, and never enters conversations.', configDocLabel:'Get a Tencent Docs token', configFields:[{key:'TENCENT_DOCS_TOKEN', label:'Personal token', helpText:'Open the authorization page, sign in by scanning a QR code with QQ or WeChat, and copy the token. Do not add a Bearer prefix.', placeholder:'Paste your Tencent Docs personal token'}], welcomeQueries:['List my recent Tencent Docs','Turn this weekly report into a Tencent smart document','Create an online spreadsheet to track expenses','Read and analyze this Tencent Docs spreadsheet'] },
  'wecom-bot':{ title:'WeCom Group Bot', subtitle:'Push messages and notifications to WeCom group chats', latency:'Cloud', desc:'Connect the official WeCom group-bot webhook (message push API): let AI push text (with @mentions), Markdown, news cards, images, and files to the bound group chat. Enter your own group-bot webhook key; the key is written only to this device\u2019s system credentials. Data goes through WeCom servers (optional online feature, opt-in).', configTitle:'WeCom Group Bot Key', configDescription:'The Key is stored only in this device\u2019s credentials and is not written to mcp.json.', configDocUrl:'https://developer.work.weixin.qq.com/document/path/91770', configDocLabel:'Open the official group-bot docs', configFields:[{key:'WECOM_BOT_KEY', label:'Webhook Key', helpText:'In a WeCom group, tap \u201c\u2026\u201d (top right) \u2192 Group Bots \u2192 Add a bot, then copy its webhook URL.', placeholder:'Paste the bot key or the full webhook URL'}], welcomeQueries:['Push today\u2019s weather bulletin to my group','Format this weekly report as Markdown and send it to the WeCom group','@everyone: don\u2019t forget the 3 p.m. meeting','Send this file to the WeCom group'] },
  feishu:{ title:'Feishu (Lark)', subtitle:'Manage Feishu docs, calendars, Bases, spreadsheets, and messages as yourself', type:'CLI + Official Skill', latency:'Cloud', desc:'Connect the official Feishu CLI and official domain skills (MIT): let AI read and write cloud docs, view and edit calendars, work with Bases and spreadsheets, send and receive messages, and manage knowledge bases and tasks as yourself. Click \u201cConnect Feishu\u201d to authorize in the browser with one click\u2014no key required. Data goes through the Feishu cloud OpenAPI (optional online feature, opt-in).', welcomeQueries:['Read a Feishu doc and make a PPT for me','Summarize my Feishu docs','Check my Feishu calendar today','Show my Feishu to-do tasks'] },
  wecom:{ title:'WeCom', subtitle:'Manage WeCom messages, mail, docs, meetings, and schedules as yourself', type:'CLI + Official Skill', latency:'Cloud', desc:'Connect the official WeCom CLI (@wecom/cli, MIT) and official domain skills: let AI send and receive messages and mail, read and write docs, smart sheets, spreadsheets, and smart pages, manage Drive files, create and query meetings and schedules, manage to-dos, and look up contacts as yourself. Click \u201cConnect\u201d and authorize by scanning a QR code with the WeCom app\u2014no key required. Data goes through the WeCom cloud (optional online feature, opt-in).', welcomeQueries:['Turn this content into a WeCom smart doc','Read the content of one of my WeCom docs','Create a new sub-sheet in a WeCom smart sheet','Query data in a WeCom smart sheet'] },
  dingtalk:{ title:'DingTalk', subtitle:'Manage DingTalk docs, calendars, sheets, and messages as yourself', type:'CLI + Official Skill', latency:'Cloud', desc:'Connect the official DingTalk Workspace CLI (dws, Apache-2.0) and official skills: let AI read and write DingTalk docs, view and edit calendars, work with AI sheets and online sheets, send and receive group messages, and handle to-dos, approvals, logs, and mail as yourself. Click \u201cConnect\u201d and authorize by scanning a QR code with the DingTalk app\u2014no key required.', welcomeQueries:['Read my DingTalk docs','Check my DingTalk schedule today','Query data in a DingTalk AI sheet','Show my DingTalk to-dos'] },
  card11:{ title:'TAPD Agile R&D', subtitle:'Automated tracking of defects and iterations', desc:'A core R&D management tool. Lets AI query project iteration progress, automatically break down requirement items, and update defect status, digitizing the full R&D lifecycle from requirements to release.' },
  card12:{ title:'CNB Cloud-Native Pipeline', subtitle:'Code repositories and CI/CD orchestration', desc:'Brings cloud-native development capabilities to the model. Use natural language to search code repositories, file issues, review PRs, and trigger and monitor pipeline deployments.' },
  qcc:{ title:'Qichacha', subtitle:'Authorized queries of business registration data', latency:'Cloud', desc:'Connect the qcc-company remote MCP from the Qichacha agent data platform. Clicking \u201CConnect\u201D authorizes with your Qichacha account via OAuth by default; you may optionally paste a Qichacha API Key instead\u2014it is stored only in this device\u2019s system credentials.', configTitle:'Qichacha API Key (optional)', configDescription:'The API Key is stored only in this device\u2019s system credentials and is never written to mcp.json. Leave it empty to connect with OAuth.', configFields:[{key:'QCC_API_KEY', label:'Qichacha API Key', helpText:'Optional. OAuth is the default; fill this in only for API-key access.', placeholder:'Paste your Qichacha API Key (optional)'}], welcomeQueries:['Look up Huawei\u2019s business registration info','Tencent\u2019s business registration details','BYD\u2019s outbound investments','Alibaba\u2019s shareholder structure'] },
  'patsnap-search':{ title:'Patsnap Patents & Literature', subtitle:'Unified global patent and paper search, with detail retrieval by publication number', latency:'Cloud', desc:'Connect the Patsnap remote MCP to run unified searches across global patent and literature databases, with natural-language, semantic, and keyword search plus multi-dimensional filtering, and pull Markdown details by patent publication number or result URL. You need to enter a Patsnap API Key; the key is written only to this device\u2019s system credentials, and mcp.json stores only an environment-variable placeholder.', configTitle:'Enter your Patsnap API Key', configDescription:'The API Key is stored only in this device\u2019s system credentials and is never written to mcp.json in plain text; it is sent in the Authorization header when connecting to Patsnap services.', configDocLabel:'How to get an API Key', configFields:[{key:'PATSNAP_API_KEY', label:'Patsnap API Key', helpText:'Get it from the Patsnap open platform or the MCP/API credentials provided by your enterprise administrator.', placeholder:'Paste your Patsnap API Key'}], welcomeQueries:['Search patents and papers on solid-state battery electrolytes','Find core CRISPR delivery-system patents and literature from the past five years','Get patent details for publication CN109123456A','Analyze CATL\u2019s patent layout in sodium-ion batteries'] },
  'canva-mcp':{ title:'Canva', subtitle:'Poster, presentation, cover, and brand template design', latency:'Cloud', desc:'Connect the Canva remote MCP. Generate and edit posters, presentations, Xiaohongshu covers, brand templates, and more with natural language; clicking \u201cConnect\u201d opens the browser to authorize with your Canva account\u2014no API Key required. Design instructions, assets, folders, and brand template content are sent to the Canva remote MCP service.', welcomeQueries:['Generate a product launch poster','Make a three-page product intro presentation','Design a Xiaohongshu cover','Create an event promo image from a brand template'] },
  obsidian:{ title:'Obsidian Knowledge Base', subtitle:'Search and manage local Obsidian notes; read and write your personal knowledge', desc:'Connect your local Obsidian vault to the model. Supports full-text search, reading, creating, editing, renaming (with automatic wikilink fixing), and deletion\u2014so AI can build on and maintain the knowledge you have accumulated. The currently open vault is detected automatically; no manual setup. Notes stay on this device and so does the model; your knowledge and compute never leave the device.', welcomeQueries:['Search my notes','Create a note to record today\u2019s ideas','What documents are in my knowledge base?','Summarize my notes'] },
  'yuandian-mcp':{ title:'Huayu Yuandian Legal Data', subtitle:'Laws and regulations, case documents, and corporate judicial risk queries', latency:'Cloud', desc:'Connect the Huayu Yuandian open platform remote MCP. Search legal data such as laws and regulations, adjudicated cases, and corporate judicial risks; clicking \u201cConnect\u201d opens the browser to authorize with your Yuandian account\u2014no API Key required.', welcomeQueries:['Search cases about labor contract termination','Look up regulations on corporate equity liability','Analyze a company\u2019s judicial risks','Find recent adjudication viewpoints'] },
  pptx:{ title:'PPT Generation', subtitle:'Editable PowerPoint generated locally, with theme templates, real charts, and covers', latency:'Local', desc:'Say \u201cmake a PPT / report\u201d and the AI drafts an outline for your confirmation, then picks a theme (9 available) and generates an editable .pptx\u2014real charts, with a cover thumbnail, fully local and your data never leaves the device. The first install downloads the python-pptx dependency (requires network).', welcomeQueries:['Make a Q2 quarterly report PPT','Make a product intro PPT','Make a project proposal presentation','Make a company intro PPT'] },
  gongwen:{ title:'Official Document Writing', subtitle:'Party/government official documents as GB/T 9704-compliant .docx', latency:'Local', desc:'Say \u201cwrite a notice / draft an opinion\u201d and the AI writes the content following the document-type structure and fixed phrasing, then the renderer applies the national standard format for party/government documents (FangZheng XiaoBiaoSong title, FangSong_GB2312 body, standard margins, red header and red separator line) to produce a .docx\u2014fully local, your data never leaves the device. Works best with the \u201cOfficial Document Writing\u201d skill. The first install downloads the python-docx dependency (requires network).', welcomeQueries:['Draft a notice on issuing management measures','Write an implementation opinion on strengthening certain work','Draft a meeting notice','Write a situation report'] },
});

// Built-in plugins read-only section (docs/builtin-toolset-contract.md §3.1):
// section title / read-only badge / audit field labels / security level copy /
// data-access scope copy. Unknown dataAccess scope keys fall back to the raw key
// on the card and are not enumerated here.
dictEn.uiBuiltinPlugins = {
  sectionTitle:'Built-in Plugins',
  kindLabel:'Type',
  pageIntro:'Plugins shipped with the app and updated together with it. This page only shows what they can do and what data they access — they cannot be uninstalled or turned off here.',
  readonlyBadge:'Built-in · Always on', readonlyModeBadge:'Built-in · Mode-controlled',
  toolsLabel:'Tools',
  securityLabel:'Security level',
  versionLabel:'Version',
  versionNote:'Updates with the app',
  dataAccessLabel:'Data access',
  levels:{ L0:'Read-only: modifies no state; content is marked untrusted', L1:'Write: produces user-visible side effects (e.g. send messages, create tasks)', L2:'Destructive: irreversible delete/overwrite; requires explicit authorization' },
  dataAccess:{ 'sessions.read':'Local session store (read-only)' },
};

// Generic built-in feature degradation copy (docs/builtin-toolset-contract.md
// §3.3 hook): reused by future feature settings pages for the "feature turned
// off" fallback.

Object.assign(dictEn.uiCodexWorkspace, { showRawErrors:false, operationFailed:'Workspace operation failed. Please retry' });

dictEn.uiComponents = { openNavigation:'Open navigation' };

dictEn.uiToolRender = {
  running: 'Running', done: 'Done', failed: 'Failed',
  other: 'Other', toolUnknown: '(unknown)',
};

dictEn.uiChatView = {
  placeholderSceneAdjust:'Describe how you want to adjust the selected element',
  placeholderSceneDataViz:'Paste data or describe metrics to generate a visualization dashboard',
  placeholderScenePoster:'Describe the visual poster you want to generate or adjust',
  placeholderScenePpt:'Describe the PPT topic you want to generate or the changes to make',
  placeholderPersonalWorkbench:'Choose a template, then edit the full prompt',
  placeholderWorkDocument:'Describe the document topic, type, sender/recipient, and key requirements',
  agentName:'品悟',
  designEditGroup:'Edit',
  designElementFallback:'element',
  designAdjustSelected:(label, raw)=>`Adjust the currently selected ${label}: ${raw}`,
};

dictEn.uiMainApp = { petSummon:'Summon Companion', closeNavigation:'Close navigation', superPermFailed:'Unable to enable advanced execution permission', searchTestUnavailable:'Search sources cannot be tested in this environment', searchSaveFailed:'Failed to save search settings. Please try again', petSessionMissing:'Target session does not exist', petTaskStartFailed:'Task failed to start', detachedSessionMissing:'The target Coding session does not exist or was deleted', detachedSessionLoadFailed:'Failed to load the Coding session. Close this window and try again', panelLoadFailed:err=>`Panel failed to load: ${err}`, viewLoadFailed:'Failed to load this page. Check your network and try again', viewReload:'Reload' };

dictEn.uiToolCommon = {
  open: 'Open', coverAlt: 'Cover',
  pinAriaLabel: 'Smart review', wuAriaLabel: 'Deep check',
  diffCollapse: 'Collapse', diffExpand: 'Expand full diff', diffCollapseAria: 'Collapse diff',
  today: 'Today', tomorrow: 'Tomorrow', dayAfterTomorrow: 'Day after tomorrow', dayN: n => `Day ${n}`,
  weatherFallback: 'Weather', currentCity: 'Current city',
  forecastToday: "Today's forecast", forecastTomorrow: "Tomorrow's forecast",
  humidity: 'Humidity', wind: 'Wind', windLevel: w => `Level ${w}`,
  tempHigh: 'High', tempLow: 'Low',
  stockOpen: 'Open', stockHigh: 'High', stockLow: 'Low',
  builtinEnabled: 'Built-in · Mode-controlled', comingSoon: 'Coming soon',
  disconnect: 'Disconnect', uninstall: 'Uninstall', reauthorize: 'Reauthorize',
  connect: 'Connect', configure: 'Configure', install: 'Install', update: 'Update',
  repair: 'Repair', editDisplay: 'Edit',
};

dictEn.uiChatScenes = {
  missingCapabilities: (ids) => `Missing capabilities: ${ids}`, switchedOffPacks: (ids) => `These tools are currently switched off — enable them in the composer tools list and retry: ${ids}`, notAppliedPacks: (ids) => `These packs are not active yet — they may still be installing. Please retry: ${ids}`,
  documentWriting: {
label: 'Document writing',
preparing: 'Preparing document writing capability...',
ready: 'Document writing enabled, starting generation',
failure: 'Document writing capability could not be prepared. Please try again later.',
  },
  dataVisualization: {
label: 'Data visualization',
preparing: 'Preparing data visualization capability...',
ready: 'Data visualization enabled, starting generation',
failure: 'Data visualization capability could not be prepared. Please try again later.',
  },
  pptDesign: {
label: 'PPT design',
preparing: 'Preparing PPT generation capability...',
ready: 'PPT generation enabled, starting generation',
failure: 'PPT generation capability could not be prepared. Please try again later.',
  },
  // Display names for personal-workbench template chips. The zh `title` in
  // personal-workbench-scene.js stays canonical (draft matching + message meta);
  // rendering resolves the localized name by template id.
  workbenchTemplates: {
    'life-log': 'Life Log',
    'personal-ledger': 'Personal Ledger',
    'study-plan': 'Study Plan',
    'task-board': 'Task Board',
    'job-tracker': 'Job Tracker',
    'travel-planner': 'Travel Planner',
    'fitness-checkin': 'Fitness Check-in',
  },
};

Object.assign(dictEn.uiScheduled, {
  createFromChat: 'Create with AI Chat',
  chooseHour: 'Select hour',
  chooseMinute: 'Select minute',
  daySeparator: ', ',
});

dictEn.uiKnowledge = {
  downloadOutput: 'Download output',
  filePathLabel: 'File path: ',
};

Object.assign(dictEn.uiConversation, {
  feedbackPositive: 'Helpful',
  feedbackNegative: 'Not helpful',
  retryReply: 'Regenerate',
  callingTool: name => `Calling ${name}`,
  copyCode: 'Copy code',
  inputPlaceholder: 'Enter a value',
  otherAnswer: 'Other',
  otherPlaceholder: 'Other',
});

Object.assign(dictEn.uiPet, {
  activityThinking: 'Thinking…',
  activityProcessing: 'Processing…',
  activityUsingTool: n => `Using ${n}`,
  activityCallingTool: 'Calling a tool…',
  activityContinuing: 'Continuing…',
  activityInputNeeded: 'Your input is needed',
  activityTaskFailed: 'The task ran into a problem',
  activityTaskDone: 'Task completed',
  activityStartFailed: 'The task could not be started',
  activityTitleFallback: 'Current task',
  loadFailedRetry: (a, b) => `${a}, ${b}`,
});

dictEn.uiPlatformMisc = {
  htmlLang: 'en',
  appTitle: 'PINVOU AI Assistant (Beta)',
  petTitle: 'PINVOU Desktop Companion',
  hostFilePicker: {
pickFolderTitle: 'Choose a folder on the desktop', pickFileTitle: 'Choose files on the desktop',
close: 'Close', goUp: 'Up one level', loadingPath: 'Reading desktop folder…', loading: 'Loading…',
cancel: 'Cancel', chooseThisFolder: 'Choose this folder', choose: 'Choose',
currentFolder: path => `Current folder: ${path}`, selectedCount: n => `${n} selected`,
thisComputer: 'This PC', home: 'Home', emptyFolder: 'Nothing to select in this folder',
loadFailed: err => `Failed to read: ${err}`, workspaceNotAuthorized: 'Allow remote access to local folders on the desktop first', alreadyOpen: 'A file picker is already open',
  },
  webClientErrors: {
stateNotReady: 'Remote control state is not ready; desktop events cannot be processed yet',
unnegotiatedEvent: event => `The desktop sent an unnegotiated remote control event: ${event}`,
rpcFailed: 'Remote call failed',
incompatibleDesktop: 'The desktop version does not support remote control. Update the desktop app first.',
unsupportedCommand: command => `The current desktop does not support this remote control command: ${command}`,
commandNotAllowed: command => `Remote control does not allow invoking ${command}`,
invalidRequestId: 'Invalid remote call request ID',
requestInFlight: 'A remote call request is already in progress',
invokeTimeout: command => `Remote call timed out: ${command}`,
  },
};

dictEn.uiSettingsView = {
  profileCallName: name => `Name: ${name}`,
  profileAssistantAlias: alias => `Assistant nickname: ${alias}`,
  memoryTabRecent: 'Recent memory',
  memoryDeleteConfirm: 'This memory will no longer be used after deletion. Delete it?',
  memoryTimeSaved: 'Saved',
  memoryTimeToday: 'Updated today',
  memoryTimeDaysAgo: n => (n === 1 ? 'Updated 1 day ago' : `Updated ${n} days ago`),
  memoryTimeDate: (month, day) => `Updated ${month}/${day}`,
  memoryConfidenceAuto: 'Auto-organized',
  memoryConfidenceHigh: 'High confidence',
  memoryConfidenceMid: 'Medium confidence',
  memoryConfidenceLow: 'Low confidence',
  memorySource: 'Source: conversation',
  memoryMoreActions: 'More actions',
  memoryArchive: 'Archive',
  memoryCardTitle: 'Memory',
  memoryEnabled: 'Enabled',
  memoryDisabled: 'Off',
  memoryLoading: 'Loading memory',
  memorySummaryWithProfile: (profile, total) => `${profile} · ${total} active memories.`,
  memorySummary: total => `PINVOU remembers your preferences, work context, and recent activities so follow-up conversations pick up the context more easily. ${total} active memories recorded.`,
  memoryOffDesc: 'Once enabled, PINVOU can remember how to address you, your stable preferences, work context, and recent activities, so you repeat yourself less.',
  memoryTurnOff: 'Disable memory',
  memoryTurnOn: 'Enable memory',
  memoryViewManage: 'View and manage',
  memoryCenterTitle: 'Memory center',
  memoryCenterDesc: 'Memories are organized automatically by AI; manual management is rarely needed.',
  memorySyncing: 'Syncing',
  memorySync: 'Sync memory',
  memoryOrganize: 'Organize with AI',
  memoryOrganizeDesc: 'Merge duplicates and prune stale entries to keep long-term memory concise and accurate',
  memoryOrganizing: 'Organizing',
  memoryOrganizeNoChange: 'Memory is already tidy; nothing was changed this time',
  memoryOrganizeSummary: (merged, updated, deleted) => `Merged ${merged}, updated ${updated}, deleted ${deleted}`,
  memoryOrganizeFailed: reason => `Failed to organize memory: ${reason}`,
  memoryLastOrganized: time => `Last organized: ${time}`,
  memoryOffNotice: 'Once memory is enabled, PINVOU uses this information in conversations and automatically organizes new long-term and recent memories.',
  memoryLongTermTabDesc: 'Name, long-term preferences, and work context',
  memoryRecentTabDesc: 'Current focus and recent activity',
  memoryItemCount: n => (n === 1 ? '1 item' : `${n} items`),
  memorySearchPlaceholder: 'Search memory',
  memoryCallNameLabel: 'Assistant calls me',
  memoryCallNamePlaceholder: 'e.g. Alex',
  memoryAssistantAliasLabel: 'I call the assistant',
  memoryAssistantAliasPlaceholder: 'e.g. Buddy',
  memoryNoMatchLongTerm: 'No matching long-term memories.',
  memoryEmptyLongTerm: 'No long-term preferences or work context yet.',
  memoryLongTermHint: 'Long-term memories are kept to understand your stable preferences, work context, and how you like to be addressed. They never expire automatically, and you can edit or delete them anytime.',
  memoryNoMatchRecent: 'No matching recent memories.',
  memoryEmptyRecent: 'No current focus or recent activity yet.',
  memoryRecentHint: 'Recent memories record what is in progress or just finished, helping PINVOU pick up the context. They fade automatically over time, and you can archive or delete them manually.',
  memoryEditDesc: 'Changes take effect immediately for future memory injection.',
  visualDesignSkillName: 'Visual Design',
  visualDesignSkillDesc: 'Design system → web pages, banners, posters, résumés…',
  serviceFeishu: 'Feishu (Lark)',
  serviceWecom: 'WeCom',
  serviceDingtalk: 'DingTalk',
  serviceTmeet: 'Tencent Meeting',
  detectFailed: 'Detection failed',
  modelLabel: 'Model',
  codingPlanModelIdPlaceholder: 'e.g. glm-5',
  searchProviderMetaso: 'Metaso',
  searchProviderBocha: 'Bocha',
  searchProviderBaidu: 'Baidu',
};

dictEn.uiArtifacts = {
  zoomFit:'Fit window', zoomActual:'Actual size', zoomCustom:'Custom', zoomOut:'Zoom out', zoomIn:'Zoom in',
  switchArtifact:'Switch artifact', currentMtime:time=>`Last modified ${time}`,
  closePreviewAria:'Close artifact preview', closePreviewTitle:'Close preview',
  fsExitEdit:'Exit edit mode and return to side preview', fsEnterEdit:'Enter edit mode: enlarge preview and edit selected elements',
  fsExitFullBack:'Exit fullscreen and return to side preview', fsEnterPreview:'Show artifact preview fullscreen', fsExit:'Exit fullscreen', fsEnter:'Fullscreen',
  fsExitEditShort:'Exit edit', fsEditMode:'Edit mode',
  aiRefreshed:'Artifact refreshed', aiCanContinue:'You can keep describing changes', aiCallingTool:name=>`Calling tool ${name}`,
  aiThinking:'Thinking', aiWaitingModel:'Waiting for the model', aiSent:'Sent, waiting to be processed',
  aiUpdated:'Updated', aiNoUpdate:'No update', aiStopped:'Stopped', aiAdjusting:suffix=>`Adjusting${suffix}`,
  aiPhElement:'Describe how to adjust the selected element', aiPhDesign:'Describe how to adjust this design',
  send:'Send', previewRefreshed:'Preview refreshed', canContinue:'Keep describing', stop:'Stop',
  diTypes:{ element:'Element', graphic:'Graphic', button:'Button', link:'Link', input:'Input control', text:'Text', card:'Card', container:'Container' },
  diNoSelection:'Nothing selected', diSelected:type=>`Selected ${type}`,
  diFontSystem:'System default', diFontCustom:'Custom font', diFont:'Font',
  diFontYaHei:'Microsoft YaHei', diFontPingFang:'PingFang SC', diFontSimSun:'SimSun', diFontSimHei:'SimHei', diFontKaiTi:'KaiTi', diFontFangSong:'FangSong', diFontSourceHanSans:'Source Han Sans SC', diFontSourceHanSerif:'Source Han Serif SC',
  diPickColor:color=>`Pick color ${color}`, diClear:'Clear', diDone:'Done',
  diChangeFallback:'Design change', diSelectHint:'Select an element in the preview', diCollapseDetails:'Hide details', diViewDetails:'View details',
  diChangeGroupEdit:'Edit', diChangeGroupTextEdit:'Text edit', diChangeGroupResize:'Resize', diChangeGroupMove:'Move', diChangeTypeText:'Text',
  diClearChanges:'Clear changes', diNoLocation:'No location info',
  diSecCommon:'Common edits', diText:'Text', diFontSize:'Font size', diFontWeight:'Font weight', diAlign:'Align',
  diOptDefault:'Default', diAlignLeft:'Left', diOptCenter:'Center', diAlignRight:'Right', diAlignJustify:'Justify', diTextColor:'Text color',
  diSecAppearance:'Appearance', diBgColor:'Background', diBorderColor:'Border color', diRadius:'Corner radius', diOpacity:'Opacity', diBgImage:'Background image',
  diSecSize:'Size', diWidth:'Width', diHeight:'Height',
  diSecAdvanced:'Advanced styles', diCollapse:'Collapse', diExpand:'Expand',
  diLineHeight:'Line height', diLetterSpacing:'Letter spacing', diBgSize:'Background size', diBgSizePlaceholder:'cover / contain', diBgPosition:'Background position',
  diRepeat:'Repeat', diRepeatNone:'No repeat', diRepeatX:'Repeat horizontally', diRepeatY:'Repeat vertically',
  diMinWidth:'Min width', diMaxWidth:'Max width', diMinHeight:'Min height', diMaxHeight:'Max height',
  diMarginTop:'Margin top', diMarginRight:'Margin right', diMarginBottom:'Margin bottom', diMarginLeft:'Margin left',
  diPaddingTop:'Padding top', diPaddingRight:'Padding right', diPaddingBottom:'Padding bottom', diPaddingLeft:'Padding left',
  diGap:'Gap', diRowGap:'Row gap', diColumnGap:'Column gap',
  diDisplay:'Display', diDisplayBlock:'Block', diDisplayFlex:'Flex', diDisplayGrid:'Grid', diDisplayInline:'Inline', diDisplayInlineBlock:'Inline-block', diOptHidden:'Hidden',
  diDirection:'Direction', diDirRow:'Row', diDirRowReverse:'Row reverse', diDirColumn:'Column', diDirColumnReverse:'Column reverse',
  diJustify:'Justify content', diOptStart:'Start', diOptEnd:'End', diJustifyBetween:'Space between', diJustifyAround:'Space around',
  diAlignItems:'Align items', diStretch:'Stretch',
  diOverflow:'Overflow', diOptVisible:'Visible', diOverflowClip:'Clip', diOverflowScroll:'Scroll', diOptAuto:'Auto',
  diPosition:'Position', diPosRelative:'Relative', diPosAbsolute:'Absolute', diPosFixed:'Fixed', diPosSticky:'Sticky',
  diTop:'Top', diRight:'Right', diBottom:'Bottom', diLeft:'Left', diZIndex:'Z-index',
  diVisibility:'Visibility', diVisCollapse:'Collapse', diCursor:'Cursor',
  diChangesLog:n=>`Design changes: ${n}`, diEmpty:'empty',
};

Object.assign(dictEn.uiAttachments, { uploading:pct=>`Uploading ${pct}%`, deviceUploadTooLarge:name=>`${name} exceeds the 20 MB attachment limit`, deviceUploadEmpty:name=>`${name} is empty and cannot be attached`, deviceUploadUnavailable:'Uploading from this device is currently unavailable', deviceUploadInvalid:name=>`${name} is not a valid attachment`, deviceUploadFailed:name=>`${name} could not be uploaded. Try again.`, deviceUploadDigestInvalid:'The attachment integrity digest was invalid. Try again', deviceUploadIntegrityMismatch:'The attachment content was corrupted in transit. Upload it again' });

Object.assign(dictEn.uiToolStore, {
  toolNames:{ feishu:'Feishu', wecom:'WeCom', dingtalk:'DingTalk', tmeet:'Tencent Meeting', ima:'Tencent ima' },
  connected:'Connected', installed:'Installed', builtin:'Built-in', internalDirect:'Direct intranet', keyRequired:'Key required', localLatency:'Local',
  cancel:'Cancel', ok:'Got it', newChat:'New Chat',
  installHint:'New tools default to off — enable them in the composer tools list', removeHint:'Removed; new conversations will no longer load this tool', firstUseOnlineInstall:'The first connection downloads and verifies the official CLI',
  obsidianGuide:{
not_installed:{ title:'Obsidian required', body:'"Obsidian Vault" works together with Obsidian. It is not installed yet—install Obsidian and create a vault to connect.', primary:'Download Obsidian', retry:'I have installed it—check again' },
no_vault:{ title:'No vault yet', body:'Obsidian was detected, but no vault has been created yet. Create a vault in Obsidian, then connect.', primary:null, retry:'I have created one—check again' },
vault_missing:{ title:'Vault folder missing', body:'The previous vault folder could not be found. Reopen it in Obsidian or create a new vault, then connect.', primary:null, retry:'Check again' },
desktopHint:'Install Obsidian on the desktop and create a vault first, then check again here.',
  },
  configTitle:name=>`Configure ${name}`, configConnect:'Connect', configInstall:'Install', configDocDefault:'View setup guide', configHelpFeishu:'No app yet? Create one on the Feishu Open Platform →',
  connectFailed:name=>`${name} connection failed`, connectTitle:name=>`Connect ${name}`,
  connectBusy:name=>`${name} is still finishing the previous sign-in`,
  connectBusyHint:'It may complete on its own. Wait a few seconds, then press Connect again.',
  connectedBanner:name=>`${name} connected · tool switches default to off — enable it in the composer tools list`,
  connectedBannerIma:'Tencent ima connected · tool switches default to off — enable it in the composer tools list',
  connectedTool:name=>`${name} connected`, disconnectedTool:name=>`${name} disconnected`,
  connectedQuoted:name=>`${name} connected`, installedQuoted:name=>`${name} installed`, uninstalledQuoted:name=>`${name} uninstalled`,
  uploadedSkill:'User-uploaded skill',
  oauthBrowserTimeout:'No browser authorization callback received. Confirm whether authorization finished, or try again later.',
  stoppingAuth:'Stopping the browser authorization wait…', authWaitCancelled:'Stopped waiting for browser authorization; you can authorize again later.', cancelFailed:'Cancel failed—retry available; the authorization wait is still running.',
  oauthConfigError:'OAuth configuration error', oauthNoServerName:name=>`"${name}" does not declare an MCP server name, so authorization cannot start.`,
  connectingTool:name=>`Connecting ${name}`, installingTool:name=>`Installing ${name}`,
  writingMcpConfig:'Writing MCP configuration…', validatingApiKey:'Validating the API key and remote tools…', downloadingDeps:'The first install downloads dependencies—please wait…',
  waitingBrowserAuth:'Waiting for browser authorization to complete.', browserOpenedWaiting:'Browser opened; waiting for authorization…',
  operationFailedWith:e=>`Operation failed: ${e}`,
  importingSkill:'Importing plugin package…', validatingSkillPackage:'Validating and extracting', importFailedWith:e=>`Import failed: ${e}`,
  editDisplayTitle:name=>`Edit display info for "${name}"`, displayNameLabel:'Display name', displayNamePlaceholder:'Leave empty to fall back to the default name',
  displayDescriptionLabel:'Display description', displayDescriptionPlaceholder:'Leave empty to fall back to the default description',
  editDisplayHint:'Only changes how the card looks; the package id and contents are untouched. Single-skill packages sync the description into SKILL.md (clearing restores the original).',
  editDisplaySave:'Save', editDisplaySaved:'Display settings saved',
  uploadSkillPackage:'Upload Plugin Package', dropSkillZipHere:'Drop to import a plugin package (zip) or skill file (.md)', zipTooLarge:mb=>`File exceeds the ${mb}MiB limit`, visibilityLoadFailed:'Failed to load visibility settings. Please retry (saved settings are unchanged).',
  updatedQuoted:name=>`"${name}" updated`, updateSkillTitle:name=>`Update "${name}"`,
  updateSkillOverwriteHint:'This overwrites the skill with the latest store version. Your local changes to it will be lost.',
  notConnectedYet:name=>`"${name}" is not connected yet.`,
  wecomScanHint:'Scan with the WeCom app', wecomQrAlt:'WeCom sign-in QR code', openBrowserFailed:"Couldn't open the browser—please retry", waitingAuth:'Waiting for authorization…', openInBrowser:'Open in browser',
  mini:{ scan:'Scan pending', install:pct=>`Installing ${pct}%`, extract:pct=>`Extracting ${pct}%`, connecting:'Connecting', title:'Tap to view progress' },
  feishuSteps:[
{ key:'runtime', label:'Prepare runtime', sub:'Use the app-provided Node runtime' },
{ key:'cli', label:'Install connector', sub:'lark-cli · about 40 seconds on first use' },
{ key:'connect', label:'Connect & authorize', sub:'Create app identity' },
{ key:'qr', label:'Scan to sign in', sub:'Scan with the Feishu app' },
  ],
  wecomSteps:[
{ key:'runtime', label:'Prepare runtime', sub:'Use the app-provided Node runtime' },
{ key:'cli', label:'Install connector', sub:'wecom-cli · about 40 seconds on first use' },
{ key:'qr', label:'Scan to sign in', sub:'Scan with the WeCom app' },
  ],
  dingtalkSteps:[
{ key:'runtime', label:'Prepare runtime', sub:'Use the app-provided Node runtime' },
{ key:'cli', label:'Install connector', sub:'dws · about 40 seconds on first use' },
{ key:'qr', label:'Scan to sign in', sub:'Scan with the DingTalk app' },
  ],
  // Connector flow-card failures, keyed by the error code the backend attaches (or the failed step's fallback code).
  connectorErrors:{
    runtime_prepare_failed:'Failed to prepare the connector runtime. Try again.',
    cli_install_failed:'Failed to install the connector. Check your network and try again.',
    auth_start_failed:'Unable to start sign-in. Try again.',
    registration_failed:'App registration was not completed. Reconnect and try again.',
    auth_failed:'Sign-in authorization was not completed. Reconnect and try again.',
    consent_persist_failed:'Authorization completed, but persisting the default-off consent failed. Turn the connector off in the composer tools list, then reconnect.',
    skills_enable_failed:'Authorization completed, but enabling the skill failed. Try again.',
    unknown:'Connection failed. Try again later.',
  },
  emptyNoMatch:'No matching tools found', emptyNoInstalled:'No tools installed yet', emptyNoTools:'No tools found',
  emptyNoMatchHint:'Try a different keyword, or check the spelling.', emptyNoInstalledHint:'Install connectors or skills from the store and they will appear here.', emptyNoInstalledHintReadonly:'No tools or skills installed on the desktop yet.',
  emptyNoToolsHint:'Try another search term or check the API documentation.',
  detailInterfaceType:'Interface Type', detailOfficialSupport:'Official support', detailVersion:'Version', detailStableRelease:'Stable release', detailLatency:'Avg Latency', detailGlobalAccel:'Global acceleration',
  aboutTitle:'About',
  oauthOutcome:{
connectedTitle:name=>`Connected to ${name}`, timeoutTitle:name=>`${name} authorization timed out`, cancelledTitle:name=>`${name} authorization cancelled`,
serviceErrorTitle:name=>`${name} authorization service error`, failedTitle:name=>`${name} authorization failed`,
tokenMissing:'The authorization flow returned success, but no OAuth token was detected locally; the tool will not be enabled. Please authorize again.', incomplete:'Authorization incomplete—please reconnect.',
  },
});

Object.assign(dictEn.uiChatView, { memStatusRemembered:'Remembered', memStatusArchived:'Archived', memStatusDeleted:'Deleted', memStatusArchivedNotice:'Memory archived', memStatusDeletedNotice:'Memory deleted' });

Object.assign(dictEn.uiToolStore, { storeData: {
  skills: {
'government-writing': { title:'Official Document Writing', subtitle:'Statutory document types like notices and opinions, with phrasing templates, hierarchical numbering, and self-checks', desc:'Draft standardized party and government official documents (notices, opinions, …): built-in document-type skeletons, a fixed phrasing library, hierarchical numbering, and ledger-style self-checks produce structured official content. Pair with the "Official Document Writing" plugin in the Plugin Center to export GB/T 9704-compliant .docx directly.', latency:'Local' },
pptx: { title:'PPT Generation', subtitle:'Generate editable PowerPoint locally, with theme templates, real charts, and covers', desc:'Generate editable PowerPoint locally: theme templates, real charts, and covers included—enter a topic to quickly produce a structured presentation.', latency:'Local' },
visualizer: { title:'Data Visualization', subtitle:'Chart.js dashboards / chart analysis / HTML visualization', desc:'Turn structured data, table summaries, and business metrics into HTML visualization dashboards that match the Pinvou host experience. Chart.js, accessible canvas, custom legends, and flat colors by default, delivered as .html artifact cards.', latency:'Local' },
'package-author': { title:'Plugin Package Standardization', subtitle:'Standardize skills/MCP/functions into an uploadable plugin package', desc:'Turn scattered skills (SKILL.md), MCP servers, or their combinations into a standard plugin package the Pinvou store can import: add plugin.json, mcp/manifest.json, SKILL.md frontmatter, generate an icon, validate naming and layout, then output a directory or zip.', latency:'Local' },
'skill-author': { title:'Skill Creation', subtitle:'Turn a one-line description into a proper SKILL.md skill', desc:'Turn a user\'s one-line description into a usable skill (SKILL.md directory): generate the name/description/instructions, and validate naming and structure. When a deliverable uploadable package is wanted, it can continue with the "Plugin Package Standardization" rules to add plugin.json and an icon, export a standard package, then ask the user whether to install.', latency:'Local' },
'tencent-docs-skill': { title:'Tencent Docs', subtitle:'Create, edit, and manage online docs / sheets / slides / smart sheets', desc:'Official Tencent Docs MCP skill (v1.0.41 adaptation): pair with the "Tencent Docs MCP" connector in the Plugin Center. Ships the official category routing (smart docs / Word / Excel / PPT / mind maps / flowcharts / smart sheets) and the full tool API reference. The token is written by the connector into this device\'s system credentials.', latency:'Cloud' },
'visual-design': { title:'Visual Design', subtitle:'Design-system-grade webpages / banners / posters / resumes', desc:'Built-in automatic skill: the model loads it on demand to produce webpages, banners, posters, resumes, and more with design-system-grade aesthetics. No installation needed—always available.', version:'Built-in', latency:'Local' },
  },
  categories: { all:'All', collab:'Collaboration', docs:'Docs & Knowledge', dev:'Development', finance:'Financial Data', life:'Daily Life', other:'Other' },
} });

Object.assign(dictEn.uiToolStore, {
  groupByType:'By Type', groupByBusiness:'By Domain',
  typeGroups:{ bundle:'Plugin Bundles', mcp:'MCP', skill:'Skills', cli:'CLI Integrations', api:'API & Webhook', upcoming:'Coming Soon' },
  recycleBin:'Recycle Bin', recycleBinTitle:'Plugin Recycle Bin', recycleBinEmpty:'Recycle bin is empty', recycleBinEmptyHint:'Uploaded plugins you uninstall are moved here; you can restore or permanently delete them.',
  recycleBinLoadFailed:'Failed to load the recycle bin. Please retry.', recycleBinLoading:'Loading recycle bin…', recycledAt:time=>`Recycled at ${time}`,
  recycleRestore:'Restore', recycleRestoreUnavailable:'Package files are missing and cannot be restored', recyclePurge:'Delete Permanently',
  recyclePurgeTitle:name=>`Permanently delete "${name}"?`, recyclePurgeHint:'This cannot be undone. The plugin package files will be permanently removed.',
  recycleRestored:name=>`"${name}" restored`, recycleRestoredCredentials:name=>`"${name}" restored. Its credentials were removed on uninstall—please fill them in again.`, recyclePurged:name=>`"${name}" permanently deleted`,
  recycleExport:'Export', recycleExportUnavailable:'Package files are missing and cannot be exported', recycleExported:name=>`Exported as ${name}`,
  movedToRecycleBinQuoted:name=>`"${name}" uninstalled and moved to the recycle bin`,
});

Object.assign(dictEn, {
  remoteKbAddContent:'Add', remoteKbUploadFolder:'Add folders',
  remoteKbDocumentTrashConfirm:'Move “{name}” to trash?', remoteKbDocumentTrashHint:'You can restore it from “Show trash”.', remoteKbDocumentTrashed:'Moved to trash', remoteKbDocumentRestored:'Document restored',
  remoteKbUploadExisting:'Already exists', remoteKbUploadExistingIndexing:'Already exists, processing', remoteKbUploadExistingStillIndexing:'Already exists, still processing', remoteKbUploadExistingFailed:'Already exists, processing failed',
  remoteKbUploadSuccess:'Uploaded: {count}', remoteKbUploadExistingSummary:'Already exists: {count}', remoteKbUploadProcessingSummary:'Processing: {count}', remoteKbUploadFailedSummary:'Failed: {count}',
  remoteKbFolderSummary:'{count} documents found', remoteKbFolderSkipped:'{count} skipped', remoteKbFolderEmpty:'No supported documents were found in this folder', remoteKbFolderLimitExceeded:'A folder import can contain at most 10,000 documents',
});

// Draft-state working directory selector for normal chat
// (features/chat/ComposerWorkspaceSelector.jsx), mirroring the code-mode
// draft selector copy in uiCodex.
dictEn.uiChatWorkspace = {
  defaultWorkspace:'Default workspace', defaultWorkspaceDesc:'Use a Pinvou-managed session directory',
  chooseDirectory:'Choose directory…', chooseDirectoryDesc:'Let the Agent work directly in that directory',
  recentDirectories:'Recent', unknownDirectory:'Unknown directory',
  // One-time confirmation card for the first YOLO switch on a session bound to
  // a working directory (mirrors uiCodex modeYoloConfirm*, but says "working
  // directory" instead of "project directory", hence separate keys).
  yoloConfirmTitle:'Switch to YOLO mode?', yoloConfirmBody:'In this mode the model reads and writes your working directory fully automatically and can run shell commands, without step-by-step approvals.',
  yoloConfirmHint:'Remembered globally once confirmed — you will not be asked again.', yoloConfirmOk:'Confirm', yoloConfirmCancel:'Cancel',
};

dictEn.uiSettingsDetail.memoryLoadFailed = 'Failed to load memory profile. Please try again.';

dictEn.uiSettingsDetail.memorySaveFailed = 'Failed to save memory profile. Please try again.';

dictEn.uiSettingsDetail.memoryDeleteFailed = 'Failed to delete memory. Please try again.';

dictEn.uiSettingsDetail.memorySourceUnavailable = 'Some memory sources are temporarily unavailable. The last successfully loaded content is preserved.';

dictEn.uiSettingsDetail.memoryRuntimeRefreshFailed = 'Memory was saved, but the current session context could not be refreshed.';

dictEn.uiSettingsDetail.memorySnapshotRefreshFailed = 'Memory was loaded, but the overview snapshot could not be refreshed.';

dictEn.uiSettingsDetail.memoryTopicCleanupRequired = 'Memory was updated. The previous topic data will be cleaned up automatically after it is released.';

Object.assign(dictEn.uiSettingsDetail, {
  reasoningEffort:'Thinking depth', reasoningEffortTiers:{ off:'Off', low:'Low', medium:'Medium', high:'High', max:'Max' },
  reasoningProbePending:'Detecting server type…', reasoningProbeUnsupported:'This endpoint does not support thinking-depth control',
  reasoningThinkingAlwaysOn:'This model always thinks and cannot be turned off',
  saving:'Saving', localDetectionTargets:'Detect vLLM, Ollama, LM Studio, and other local servers',
  redetect:'Check again', detect:'Detect', noRunningLocalModel:'No running local model detected', localPortLabel:'Port', localPortPlaceholder:'e.g. 8080', localPortInvalid:'Invalid port: enter a number between 1 and 65535', modelNotLoadedTag:'Not loaded', modelNotLoadedHint:'Not in memory yet; it will load automatically on first use',
  add:'Add', manualLocalModel:'Add a local model manually',
  manualLocalModelDesc:'Enter the API URL and model ID', cloudModels:'Cloud models',
  localModels:'Local models', toolCalling:'Tool calling', apiKeyRequired:'Requires API Key',
  testBeforeSave:'Test whether the service is available before saving', modelContextWindowPlaceholder:'e.g. 1048576', modelContextWindowHint:'Leave empty for the system default (uncatalogued models fall back to a 128K conservative value). For a newer model, enter its official window, e.g. 1048576 for 1M.',
  addProvider:provider=>`Add ${provider}`, editProvider:provider=>`Edit ${provider}`,
  autoDetectLocalModel:'Automatically detect local models',
  codingPlanTestUnavailable:'This provider endpoint cannot be tested right now, but you can still save the configuration',
  connectionMessages:{ ok:'Connection successful; service available', auth_invalid:'Invalid API Key. Check it and try again', auth_forbidden:'This API Key does not have access', billing:'Account balance is insufficient. Add balance or switch to another model', rate_limited:'Too many requests or insufficient quota. Try again later', redirect:'The endpoint redirected; availability could not be confirmed', request_invalid:'The request format was not accepted. Check the model configuration', timeout:'Connection timed out. Check the network or whether a local service is running', tls_error:'Security certificate verification failed. Check the proxy or network environment', dns_failed:'Could not resolve the service address. Check the network', connection_refused:'Could not connect. Make sure the local model service is running', server_unavailable:'Service is temporarily unavailable. Try again later', endpoint_not_found:'Endpoint unavailable', method_not_allowed:'The endpoint does not support test requests', http_error:'Connection failed. Check the configuration and retry', unknown:'Connection failed. Try again later' },
  catalogSections:{ coding_plan:'Coding Plan', official_api:'Official APIs', aggregator:'Aggregators', custom:'Custom compatible endpoints' },
  providerCatalog:{
    local:{title:'Local Model',desc:'Default model for the local service'}, glm_coding_plan:{title:'Zhipu Coding Plan / GLM Coding Plan',configTitle:'Zhipu Coding Plan',desc:'Dedicated endpoint for coding and agent tasks'},
    tencent_coding_plan:{title:'Tencent Cloud Coding Plan',configTitle:'Tencent Cloud Coding Plan',desc:'Tencent Cloud coding-plan endpoint'},
    tencent_token_plan:{title:'Tencent Cloud Token Plan',configTitle:'Tencent Cloud Token Plan',desc:'Dedicated gateway for Tencent Cloud TokenHub Token Plan subscriptions'},
    kimi_coding_plan:{title:'Kimi Coding Plan',configTitle:'Kimi Coding Plan',desc:'Dedicated endpoint for Kimi coding tasks'},
    deepseek:{title:'DeepSeek',configTitle:'DeepSeek',desc:'Official DeepSeek API'}, kimi:{title:'Kimi China',configTitle:'Kimi',desc:'Official Moonshot API'},
    glm:{title:'Zhipu Open Platform / GLM API',configTitle:'GLM API',desc:'Standard Zhipu Open Platform API'},
    minimax:{title:'MiniMax China',configTitle:'MiniMax',desc:'Official MiniMax API'}, mimo:{title:'MiMo',desc:'Official Xiaomi MiMo API'},
    qwen:{title:'Qwen',desc:'Alibaba Cloud DashScope-compatible API'}, doubao:{title:'Doubao',desc:'Official Volcengine Ark API'},
    glm_coding_plan_global:{title:'Zhipu Coding Plan Global / GLM Coding Plan Global',configTitle:'Zhipu Coding Plan Global',desc:'Dedicated z.ai endpoint for coding and agent tasks'},
    kimi_global:{title:'Kimi Global',configTitle:'Kimi Global',desc:'Moonshot international API'},
    glm_global:{title:'Zhipu GLM API (z.ai)',configTitle:'GLM API (z.ai)',desc:'Zhipu international z.ai API'},
    minimax_global:{title:'MiniMax Global',configTitle:'MiniMax Global',desc:'MiniMax international API (keys are region-locked)'},
    qwen_token_plan:{title:'Qwen Token Plan',configTitle:'Qwen Token Plan',desc:'Dedicated gateway for Alibaba Token Plan subscriptions'},
    qwen_global:{title:'Qwen International',configTitle:'Qwen International',desc:'Alibaba Cloud Model Studio international API'},
    openai:{title:'OpenAI',configTitle:'OpenAI',desc:'Official OpenAI API'},
    anthropic:{title:'Anthropic Claude',configTitle:'Anthropic Claude',desc:'Official Anthropic API (native Messages protocol)'},
    gemini:{title:'Google Gemini',configTitle:'Google Gemini',desc:'Gemini API (OpenAI-compatible endpoint)'},
    xai:{title:'xAI Grok',configTitle:'xAI Grok',desc:'Official xAI API'},
    volcengine_coding_plan:{title:'Volcengine Ark Coding Plan',configTitle:'Volcengine Ark Coding Plan',desc:'Dedicated endpoint for the Volcengine Ark coding plan'},
    qwen_coding_plan:{title:'Qwen Coding Plan',configTitle:'Qwen Coding Plan',desc:'Dedicated Alibaba Model Studio Coding Plan endpoint'},
    openrouter:{title:'OpenRouter',configTitle:'OpenRouter',desc:'Official OpenRouter aggregator API'},
    siliconflow:{title:'SiliconFlow China',configTitle:'SiliconFlow',desc:'Official SiliconFlow China API'},
    siliconflow_global:{title:'SiliconFlow Global',configTitle:'SiliconFlow Global',desc:'SiliconFlow international API (keys are region-locked)'},
    openai_compatible:{title:'OpenAI Compatible',desc:'Custom OpenAI-compatible endpoint'},
    openai_responses:{title:'OpenAI Responses Compatible',desc:'Custom OpenAI Responses-compatible endpoint'},
  },
  imageCapability:'Image input', imageCapabilityEnabled:'Supports images', imageCapabilityDisabled:'No image support', imageCapabilityPinvou:'Handled automatically',
  visionModel:'Vision model', visionModelNone:'None', visionModelDesc:'Used to analyze images when the current model cannot see them',
  imagePrivacyDesc:'With cloud models, images are sent to the model provider you selected; with local models, images never leave this device.',
  imageCapabilityTest:'Test image input', imageCapabilityTestHint:'Send a solid-color test image to check whether the model can see images. This calls the model and may incur charges',
  imageCapabilityTestSupported:'Supports images', imageCapabilityTestUnsupported:'No image recognition',
  imageCapabilityTestUnverified:'Failed to recognize the image; reason unknown',
  imageCapabilityTestError:'Test failed. Check the connection and API Key first',
  imageCapabilityTestReply:summary=>`Model reply: ${summary}`,
  imageCapabilityTestEnableHint:'You can set image input to “Supports images” above',
  imageCapabilitySaveFailed:error=>`Save failed: ${error}`,
  visionModelProbing:'Testing image input…',
  visionModelProbeError:summary=>`This model failed the image recognition probe and cannot be used as a vision model. Check its configuration and retry${summary ? `: ${summary}` : ''}`,
});

Object.assign(dictEn.uiAttachments, {
  imageUnsupported:'The current model does not support images. Switch to an image-capable model, or configure a vision model in model settings.',
  imageUnknown:'Image input capability of the current model is unknown. If it supports images, set image input to “Supports images” in model settings; you can also configure a vision model.',
  imageCloudUpload:'Images will be sent to the model provider you selected along with your message.',
  imageCloudUploadVision:'Images will be sent to the vision model provider you configured for analysis.',
});

Object.assign(dictEn.uiSettingsDetail.modelDescriptions, {
  '本地服务默认模型':'Default model for the local service',
  'Coding Plan 自动模型':'Coding Plan automatic model',
  '自动模型，智能路由':'Automatic model with smart routing',
  'Hy 套餐专属模型':'Exclusive to the Hy plan',
  'K2.8 Preview，全档 1M 上下文':'K2.8 Preview, 1M context on all tiers',
  'K3 长上下文模型':'K3 long-context model', '高速编码模型':'High-speed coding model',
  'K3 256K 上下文，价格更低':'K3 with 256K context at a lower price',
  '官方已转 Legacy，兼容保留':'Officially legacy; kept for compatibility',
  '官方已转 Legacy，兼容高速':'Officially legacy; high-speed compatibility',
  '最新旗舰':'Latest flagship', '上代旗舰推理':'Previous-generation flagship reasoning',
  '均衡性价比':'Balanced and cost-effective', '快速高性价比':'Fast and cost-effective',
  '快速高性价比，夜间同样四折':'Fast and cost-effective, also 40% credit cost at night',
  '旗舰推理与编码':'Flagship reasoning and coding', '均衡智能与成本':'Balanced intelligence and cost',
  '低成本高并发':'Low cost, high volume', '上代旗舰':'Previous-generation flagship',
  '快速经济':'Fast and economical',
  '速度与智能均衡':'Balanced speed and intelligence',
  '最新 Flash，均衡高性价比':'Latest Flash, balanced and cost-effective',
  '旗舰推理（预览）':'Flagship reasoning (preview)',
  // Model catalog entries (src/features/settings/model-catalog.js items[].desc)
  '旗舰编码模型，全套餐支持':'Flagship coding model, all plans', '原生多模态编码模型，额度三倍':'Native multimodal coding model, 3x quota',
  '极速多模态，200 tokens/s':'Fast multimodal, 200 tokens/s',
  '历史模型，请求自动切换至 GLM-5.3':'Legacy model, requests auto-switch to GLM-5.3', '历史模型，自动切换至 GLM-5.3-Flash':'Legacy model, auto-switch to GLM-5.3-Flash',
  '历史模型，请求自动路由至 GLM-5.3':'Legacy model, requests auto-route to GLM-5.3', '历史模型，自动路由至 GLM-5.3-Flash':'Legacy model, auto-routes to GLM-5.3-Flash',
  '旗舰编码模型，官方将于 2026-10-09 下线':'Flagship coding model, will be retired on 2026-10-09', '官方将于 2026-10-09 下线':'Will be retired on 2026-10-09',
  '通用推理，官方将于 2026-10-09 下线':'General-purpose reasoning, will be retired on 2026-10-09',
  '官方将于 2026-10-21 下线':'Will be retired on 2026-10-21',
  '多模态高性价比':'Multimodal and cost-effective', '最新多模态高性价比':'Latest multimodal, cost-effective',
  '新预览旗舰，仅订阅渠道':'New preview flagship, subscription channels only',
  'Kimi 最新旗舰':'Latest Kimi flagship', 'Kimi 编码模型':'Kimi coding model', 'MiniMax 最新旗舰':'Latest MiniMax flagship',
  'V4.1-Flash 主力，1M 上下文，支持图片输入':'V4.1-Flash mainline, 1M context, image input', '在售；官方确认 2026-09-14 后继续提供且计费不变':'On sale; officially continues past 2026-09-14 with billing unchanged',
  '最新旗舰，强制思考':'Latest flagship, thinking always on', '最新旗舰，1M 上下文多模态':'Latest flagship, 1M context, multimodal',
  '最新旗舰，1M 上下文':'Latest flagship, 1M context',
  'Hy4 预览':'Hy4 preview',
  '上代旗舰推理（纯文本）':'Previous-generation flagship reasoning (text-only)', '上代快速款':'Previous-generation fast model',
  '正式旗舰，夜间 22:00-08:00 四折（个人版）':'GA flagship, 40% credit cost 22:00-08:00 (personal plan)',
  '轻量兼容款，支持图像输入':'Lightweight compatibility option, image input',
  '最新推荐，统一模型 ID 自动升级':'Latest recommended, auto-updating under one Model ID', '低成本低时延，效果比肩 2-1-pro':'Low cost, low latency, near 2-1-pro quality',
  '编程特化（预览），官方即将下线':'Coding-specialized (preview); retiring soon per official docs', '稳定通用，官方即将下线':'Stable general-purpose model; retiring soon per official docs', '最强旗舰':'Strongest flagship',
  '编码与 Agent 新旗舰（默认）':'New coding/agent flagship (default)',
  '低价高效':'Low-cost and efficient',
  'GPT-5.6 家族旗舰，推理与编码':'GPT-5.6 family flagship, reasoning and coding',
  '最强旗舰，高难推理与长程 Agent':'Strongest flagship, hard reasoning and long-running agents',
  '官方默认推荐，复杂 Agent 编码':'Official default recommendation, complex agentic coding',
  '上代旗舰，兼容保留':'Previous-generation flagship, kept for compatibility',
  '最快，200K 上下文':'Fastest, 200K context', '上一代 Flash':'Previous-generation Flash',
  '基线速度，兼容保留':'Baseline speed, kept for compatibility',
  '旗舰，编码与 Agent 默认推荐':'Flagship, default pick for coding and agents', '上代旗舰，编码与 Agent':'Previous-generation flagship, coding and agents',
  '4.20 推理，1M 上下文':'4.20 reasoning, 1M context', '4.20 非推理，1M 上下文':'4.20 non-reasoning, 1M context',
  '快速可靠，强工具调用':'Fast and reliable, strong tool calling', '代码 Agent，256K 上下文':'Coding agent, 256K context',
  '全模态，低成本':'Full-modal, low cost', '旗舰效果，极速输出':'Flagship quality, ultra-fast output',
});

dictEn.uiSettingsDetail.customCodingPlanDesc = 'Enter a Coding Plan model ID manually';
dictEn.uiSettingsDetail.customTokenPlanDesc = 'Enter a Token Plan model ID manually';
dictEn.uiSettingsDetail.modelAlias = 'Alias';
dictEn.uiSettingsDetail.modelAliasPlaceholder = 'Optional, e.g. Daily assistant';

dictEn.uiSettingsDetail.customModelTitles = {
  glm:'Custom GLM model', qwen:'Custom Qwen model',
  openai_compatible:'Custom compatible model', openai_responses:'Custom Responses model', glm_coding_plan:'Custom GLM Coding Plan model',
  tencent_coding_plan:'Custom Tencent Cloud Coding Plan model', tencent_token_plan:'Custom Tencent Cloud Token Plan model', kimi_coding_plan:'Custom Kimi Coding Plan model',
  volcengine_coding_plan:'Custom Volcengine Ark Coding Plan model', qwen_coding_plan:'Custom Qwen Coding Plan model',
  openrouter:'Custom OpenRouter model', siliconflow:'Custom SiliconFlow model', siliconflow_global:'Custom SiliconFlow model',
};

// Official 主线在词典拆分基线之后新增或调整的文案。
function buildTeamRelayAgentPrompt(language, task, workspace, options = {}) {
  const copy = {
    zh: { task: '任务', notes: '要做什么', goal: '交付目标', questions: '待确认', materials: '相关材料', git: '代码参考' },
    en: { task: 'Task', notes: 'What to do', goal: 'Delivery goal', questions: 'To confirm', materials: 'Related materials', git: 'Code reference' },
    ja: { task: 'タスク', notes: '作業内容', goal: '完了目標', questions: '要確認', materials: '関連資料', git: 'コード参照' },
  }[language];
  const events = Array.isArray(options.events) ? options.events : (task.events || []);
  const uniqueLines = values => [...new Set(values.map(value => String(value || '').trim()).filter(Boolean))];
  const noteLines = uniqueLines(events.map(event => event.message));
  const goalLines = uniqueLines(events.map(event => event.delivery?.expected_next));
  const questionLines = uniqueLines(events.map(event => event.delivery?.open_questions));
  const materialLines = uniqueLines((workspace?.materials || []).map(file => file.local_path ? `${file.original_name || file.local_path}：${file.local_path}` : file.original_name));
  const references = events.flatMap(event => event.delivery?.git_references || []);
  const gitLines = uniqueLines(references.map(reference => `${reference.repository_url || ''}${reference.commit_sha ? ` @ ${reference.commit_sha}` : ''}${reference.branch ? ` · ${reference.branch}` : ''}${reference.paths?.length ? `\n  ${reference.paths.join(' · ')}` : ''}`));
  const sections = [
    `${copy.task}：${task.title}`,
    noteLines.length ? `${copy.notes}：\n${noteLines.map(line => `- ${line}`).join('\n')}` : '',
    goalLines.length ? `${copy.goal}：\n${goalLines.map(line => `- ${line}`).join('\n')}` : '',
    questionLines.length ? `${copy.questions}：\n${questionLines.map(line => `- ${line}`).join('\n')}` : '',
    materialLines.length ? `${copy.materials}：\n${materialLines.map(line => `- ${line}`).join('\n')}` : '',
    gitLines.length ? `${copy.git}：\n${gitLines.map(line => `- ${line}`).join('\n')}` : '',
  ];
  return sections.filter(Boolean).join('\n\n');
}

dictEn["teamRelay"] = "Team Relay";
dictEn["uiTeamRelay"] = {
  "inviteMembers": "Invite members",
  "syncNow": "Sync now",
  "syncingNow": "Syncing…",
  "sending": "Sending…",
  "brand": "Pinvou Relay",
  "title": "Team Relay",
  "subtitle": "Assign work, keep it moving, and bring delivery home",
  "assign": "Start relay",
  "inbox": "Work",
  "workList": "Work list",
  "search": "Search",
  "empty": "No work here yet",
  "selectWork": "Select work to view its relay thread",
  "close": "Close",
  "done": "Done",
  "cancel": "Cancel",
  "more": "More",
  "backToList": "Back to work list",
  "batonPath": "Relay route",
  "history": "Relay history",
  "toggleTheme": "Toggle light or dark theme",
  "workId": id => `Relay ${id}`,
  "liveBadge": "Connected",
  "syncing": "Syncing team work…",
  "retry": "Sync again",
  "createJoinPassword": "Team join code",
  "rotateJoinPassword": "Rotate team join code",
  "rotateJoinPasswordConfirm": "Generating a new code immediately invalidates the current team join code. Continue?",
  "joinPasswordReady": "Team join code",
  "joinPasswordHint": "Send members this 8-character code; they can join through the default Team Relay service. Rotating invalidates the old code.",
  "copyJoinPassword": "Copy join code",
  "copiedJoinPassword": "Join code copied",
  "addFiles": "Add local files",
  "noFiles": "No files selected",
  "uploading": "Uploading…",
  "download": "Download",
  "returning": "Return to previous owner",
  "agentOpenFailed": "Could not open a private Agent session. Try again shortly.",
  "unsupportedTitle": "Team Relay is unavailable here",
  "unsupportedBody": "Open this page in the Pinvou desktop app. The Web host does not connect to the team service.",
  "joinCode": {
    "memberVisibility": "All team members can view and share this code. It remains valid until an owner rotates or pauses it.",
    "ownerVisibility": "Only team owners can currently view this join code.",
    "ownerOnly": "An owner has hidden the team join code. Ask an owner for access.",
    "disabled": "Team joining is paused. Existing join codes cannot be used.",
    "legacyOwner": "This code was created by an older Hub and cannot be shown again. Rotate it once to make it persistently viewable.",
    "legacyMember": "An owner must rotate the current code before it can be viewed.",
    "rotate": "Rotate code",
    "enable": "Enable joining",
    "hideFromMembers": "Hide from members",
    "showToMembers": "Show to members",
    "disable": "Pause joining",
    "disableConfirm": "Pausing immediately invalidates the current code and prevents new members from joining. Continue?"
  },
  "returnDefault": "The collaboration is complete. Return it to the previous owner.",
  "acceptDefault": "Delivery confirmed. Approve and close this work.",
  "acceptTransferDefault": "I accept this task or relay.",
  "pendingConfirmation": "Waiting for confirmation",
  "errorMessages": {
    "hub_unreachable": "Could not reach the Team Relay service. Check the network and try again.",
    "hub_timeout": "The Team Relay service timed out. Try again shortly.",
    "resource_not_found": "The Team Relay service does not provide this feature yet. Update it and try again.",
    "authentication_required": "This local credential is no longer valid. Reconnect to Team Relay.",
    "membership_required": "You are no longer in this team. Switch to another team.",
    "join_password_invalid": "The team join code is incorrect. Check it with a member.",
    "join_code_storage_unavailable": "The join code cannot be read securely. Ask the service administrator to check its encryption configuration.",
    "email_already_registered": "An account already uses this email. Sign in before joining or creating a team.",
    "email_change_not_supported": "Changing the registered email is not supported yet.",
    "rate_limited": "Too many join code attempts. Try again later.",
    "invite_expired": "The invite is invalid or expired. Ask for a new one.",
    "invite_already_claimed": "This invite code has already been used.",
    "member_has_open_work": "This member still has required responsibilities in open work. Finish or hand them off before retrying.",
    "last_team_owner": "The team owner cannot leave directly. Transfer ownership or dissolve the team first.",
    "new_owner_unavailable": "This member's account is disabled and cannot take over as team owner.",
    "team_has_open_work": "The team still has open work. Finish all of it before dissolving the team.",
    "revision_conflict": "This work changed moments ago. It has been refreshed; review and retry.",
    "action_not_allowed": "This action is not allowed in the current relay state.",
    "attachment_too_large": "The file exceeds the 20 MiB limit.",
    "attachment_type_not_allowed": "This file type is not supported.",
    "credential_store_failed": "The local credential store could not be accessed securely. Check the system credential store.",
    "team_relay_settings_delete_failed": "Could not clear the local Team Relay settings. Check the data-directory permissions and try again.",
    "avatar_too_large": "The avatar exceeds the 1.5 MiB limit.",
    "avatar_processed_too_large": "The safely processed avatar still exceeds 1.5 MiB. Resize or compress it and try again.",
    "avatar_type_not_allowed": "Avatars support PNG, JPEG, WebP, and GIF only, and the content must match the extension.",
    "avatar_image_invalid": "The avatar is damaged or could not be fully decoded. Export it again and retry.",
    "avatar_image_limits_exceeded": "The avatar dimensions, GIF frame count, or total animated pixels exceed the safe limit.",
    "avatar_upload_in_progress": "An avatar for this account is already being processed. Try again shortly.",
    "avatar_processing_busy": "Avatar processing is busy. Try again shortly.",
    "avatar_upload_rate_limited": "The avatar was updated too often. Try again shortly.",
    "avatar_unreadable": "The selected avatar could not be read. Choose it again.",
    "avatar_invalid_name": "The avatar filename is invalid. Rename it and try again."
  },
  "setup": {
    "title": "Sign in to Team Relay",
    "body": "Use your email and name to enter Team Relay. You can join or create a team afterward.",
    "service": "Team Relay service",
    "defaultService": "Pinvou default Team Relay service",
    "connectedService": "Connected",
    "email": "Email",
    "emailPlaceholder": "name@example.com",
    "displayName": "Your name",
    "defaultDeviceName": "Pinvou desktop",
    "submit": "Enter Team Relay",
    "privacy": "Each email identifies one account and cannot be changed after it is bound. Team members only see your name."
  },
  "profile": {
    "open": "Account and avatar",
    "title": "Account and avatar",
    "subtitle": "Your avatar and name are shown in every team you join.",
    "email": "Email account",
    "displayName": "Your name",
    "emailLocked": "Email identifies your account and cannot be changed in this version.",
    "bindEmail": "This is a legacy identity. Bind an email now; it cannot be changed in this version.",
    "confirm": "Save",
    "updated": "Name updated",
    "logout": "Sign out",
    "logoutTitle": "Sign out of Team Relay?",
    "logoutBody": "This removes the Team Relay credential from this device and stops syncing. Your account, team memberships, and relay history remain, and other devices are unaffected.",
    "logoutConfirm": "Sign out",
    "loggingOut": "Signing out…",
    "avatar": "Relay avatar",
    "avatarHint": "Up to 1.5 MiB. Static images are resized to 512px; GIFs to 384px with at most 60 frames and 4M total pixels. Without one, your name initial is shown.",
    "uploadAvatar": "Upload avatar",
    "changeAvatar": "Change avatar",
    "removeAvatar": "Remove",
    "removeAvatarConfirm": "Remove the custom avatar and use your name character?",
    "avatarUpdated": "Avatar updated",
    "avatarRemoved": "Name avatar restored"
  },
  "actionKinds": {
    "assign": "Task sent",
    "note": "Note",
    "progress": "Progress update",
    "handoff": "Collaboration invitation sent",
    "accept_transfer": "Task or relay accepted",
    "decline_transfer": "Request declined",
    "withdraw_transfer": "Request withdrawn",
    "reroute_transfer": "Recipient changed",
    "return": "Returned to previous owner",
    "final_deliver": "Final delivery",
    "request_changes": "Changes requested",
    "accept": "Approved",
    "cancel": "Task cancelled"
  },
  "live": {
    "actionTitle": (kind, actor, recipient) => recipient && recipient !== '—' ? `${actor} · ${kind} → ${recipient}` : `${actor} · ${kind}`,
    "yourTurn": {
      "title": "It is your turn",
      "body": "Work on this item, invite a teammate to collaborate, or return and deliver it."
    },
    "assignmentOffer": sender => ({ title: `${sender} assigned you a task`, body: 'Accept to become its owner and receive full material access, or decline with a reason.' }),
    "handoffOffer": sender => ({ title: `${sender} invited you to take the relay`, body: 'Accept to become the current actor. Return the result to the person who invited you when done.' }),
    "waitingTransfer": recipient => ({ title: `Waiting for ${recipient}`, body: 'The baton stays with you until they accept. You can withdraw or choose someone else.' }),
    "needsReassignment": {
      "title": "This task needs a new owner",
      "body": "The previous recipient declined or the assignment was withdrawn. Choose another owner."
    },
    "awaitingAcceptance": owner => ({ title: 'Waiting for your approval', body: `${owner} sent a final delivery. Approve and close it, or request changes.` })
  },
  "categories": {
    "attention": "Needs my attention",
    "waiting": "Waiting on others",
    "all": "All relays",
    "ended": "Ended",
    "hidden": "Archived"
  },
  "statuses": {
    "offer": "Needs confirmation",
    "reroute": "Needs reassignment",
    "pending": "To do",
    "receive": "To approve",
    "waiting": "Waiting",
    "ended": "Ended",
    "hidden": "Archived"
  },
  "sourceFilterLabel": "Filter by source",
  "sourceFilters": {
    "all": "All",
    "sent": "Started by me",
    "received": "Assigned to me",
    "collaborating": "Collaborating"
  },
  "views": {
    "label": "Relay view",
    "personal": "My work",
    "teams": "Teams",
    "board": "Team details"
  },
  "unread": {
    "nav": "Team Relay has updates or pending actions",
    "tasks": n => `${n} tasks have updates`,
    "updates": n => `${n} unread updates`
  },
  "teams": {
    "switchLabel": "Choose team",
    "filterLabel": "Team filter",
    "allTeams": "All teams",
    "create": "New team",
    "join": "Join team",
    "directoryTitle": "My teams",
    "directorySubtitle": "Teams define collaboration scope; they no longer limit what appears in My work.",
    "joinedCount": "Joined teams",
    "totalOpenWork": "Active team relays",
    "teamSummary": (members, work) => `${members} members · ${work} active`,
    "openTeam": "View team",
    "backToDirectory": "Back to teams",
    "emptyTitle": "You have not joined a team yet",
    "emptyBody": "Use a team join code to enter an existing team, or create a new one.",
    "openWork": n => `${n} active`,
    "created": "Team created",
    "joined": "Joined team",
    "createDialog": {
      "title": "Create a temporary team",
      "subtitle": "Start a lightweight work group for a project or collaboration.",
      "name": "Team name",
      "placeholder": "For example: Release preparation",
      "hint": "You become the team owner and can invite collaborators.",
      "confirm": "Create and enter"
    },
    "joinDialog": {
      "title": "Join a team",
      "subtitle": "Use the team join code shared by a team owner.",
      "code": "Team join code",
      "placeholder": "For example: K7M4-P9Q2",
      "hint": "Codes are case-insensitive and the hyphen is optional. After joining, you can see members and assigned work.",
      "confirm": "Join team"
    }
  },
  "board": {
    "title": "Team members",
    "subtitle": "See team roles, current relay workload, and acceptance status.",
    "sidebar": {
      "title": "Team"
    },
    "summary": {
      "members": "Members",
      "inProgress": "In relay",
      "acceptance": "To accept",
      "completed": "Ended"
    },
    "groups": {
      "owners": "Team owner",
      "members": "Team members"
    },
    "memberCount": n => `${n} member${n === 1 ? '' : 's'}`,
    "me": "Me",
    "roles": {
      "owner": "Owner",
      "member": "Member"
    },
    "busy": "In relay",
    "busyCount": n => `${n} active`,
    "available": "No current blockers.",
    "availableShort": "Available",
    "memberWorkSummary": member => `${member.activeCount} working, ${member.waitingCount} waiting for return, and ${member.acceptanceCount} to accept.`,
    "loadTitle": "Team workload",
    "loadSubtitle": "Sorted by open work and acceptance",
    "load": {
      "active": "Working",
      "waiting": "Waiting for return",
      "acceptance": "To accept"
    },
    "queue": {
      "title": "Relay queue",
      "count": n => `${n} item${n === 1 ? '' : 's'}`,
      "empty": "No relay is currently in progress",
      "hint": "Queue and reminders use the current team’s authoritative relay state."
    },
    "attention": {
      "title": "Acceptance reminders",
      "count": n => `${n} item${n === 1 ? '' : 's'} to accept`,
      "body": title => `“${title}” has been delivered and is ready for review.`,
      "empty": "Nothing is waiting for acceptance"
    },
    "ownedCount": n => `Owns ${n} work item${n === 1 ? '' : 's'}`,
    "lastSeen": time => `Last seen ${time}`,
    "noLastSeen": "No presence yet",
    "inviteMember": "Team join code",
    "removeMember": "Remove",
    "removeConfirm": name => `Remove “${name}” from this team?`,
    "removed": name => `Removed “${name}” from the team`,
    "removedAndCodeRotated": name => `Removed “${name}” and rotated the team join code`,
    "transferOwner": "Transfer owner",
    "transferConfirm": name => `Transfer team ownership to “${name}”? You will become an ordinary member.`,
    "transferred": name => `Team ownership transferred to ${name}`,
    "leaveTeam": "Leave team",
    "leaveConfirm": name => `Leave “${name}”? You will lose access and the current join code will be rotated immediately.`,
    "left": name => `Left ${name}`,
    "dissolveTeam": "Dissolve team",
    "dissolveConfirm": name => `Permanently dissolve “${name}”? Every member immediately loses access and the join code becomes invalid. Server history is retained but currently has no recovery or viewing entry. This cannot be undone.`,
    "dissolved": name => `Dissolved ${name}`
  },
  "taskInfo": "Task details",
  "initiator": "Initiator",
  "owner": "Owner",
  "currentActor": "Current actor",
  "finalRecipient": "Final recipient",
  "attachments": n => `${n} attachment${n === 1 ? '' : 's'}`,
  "dash": "—",
  "materialInspector": {
    "title": "Task materials",
    "summary": (files, packages) => `${files} files · ${packages} packages`,
    "empty": "Full materials appear after you accept the task or relay",
    "agentUpdate": "New context not yet synced to the current Agent",
    "unavailable": "Material expired",
    "openPrepared": "Open material",
    "revealPrepared": "Open containing folder",
    "download": "Download locally",
    "introduced": "Added to Agent"
  },
  "useMyAgent": "Use my Agent",
  "continueWithAgent": "Continue Agent work",
  "updateProgress": "Update progress",
  "askForHelp": "Invite collaborator",
  "acceptTask": "Accept task",
  "declineTask": "Decline task",
  "acceptHandoff": "Accept relay",
  "declineHandoff": "Decline relay",
  "withdrawAssignment": "Withdraw assignment",
  "withdrawHandoff": "Withdraw invitation",
  "withdrawTransfer": "Withdraw request",
  "rerouteTransfer": "Change recipient",
  "finalDelivery": "Final delivery",
  "requestSupplement": "Request changes",
  "acceptDelivery": "Approve and close",
  "pinned": "Pinned",
  "pinWork": "Pin work",
  "unpinWork": "Unpin work",
  "hideWork": "Archive work",
  "restoreWork": "Restore to list",
  "cancelWork": "Cancel task",
  "privacyNotice": "Only explicitly selected files and notes travel with the relay. Your private workspace and Agent chats are never shared automatically.",
  "preparedMaterials": (ready, total) => `${ready}/${total} files prepared`,
  "prepareAllMaterials": "Prepare all materials",
  "preparingMaterials": "Preparing…",
  "sourceSession": "Bring in from a session (optional)",
  "sourceSessionsLoading": "Loading sessions…",
  "noSourceSession": "No linked session",
  "chatSessions": "Pinvou chats",
  "codingSessions": "Pinvou Coding chats",
  "sourceSessionsFailed": "Could not load sessions. You can still assign without one.",
  "sourceSessionLoading": "Reading session outputs and workspace status…",
  "sourceSessionFailed": "Could not read this session. Retry or clear it.",
  "chatSession": "Chat session",
  "codingSession": "Coding session",
  "selectedSourceMaterials": n => `${n} material(s) included`,
  "chatSessionMaterialHint": n => n ? `All ${n} recognized session output(s) are selected by default. Remove any you do not want to send.` : 'This session has no deliverable outputs.',
  "noGitRemote": "No shareable Git remote found",
  "noGitCommit": "No pinned commit available",
  "detachedHead": "Branch not detected",
  "codingChanges": n => `${n} workspace change(s)`,
  "codingSessionChanges": n => `${n} session-related`,
  "codingStaged": n => `${n} staged`,
  "dirtyGitWarning": n => `The Git reference is pinned to the current commit and does not include the ${n} uncommitted file(s) below.`,
  "unshareableGitWarning": "A reliable pinned Git reference is unavailable. Only the explicitly selected files below will be sent.",
  "sourceSessionPrivacy": "Only the listed materials and code reference are sent—never the session transcript, absolute paths, Git credentials, or full diffs.",
  "deliveryGoalPlaceholder": "Describe the delivery outcome and how the recipient can tell it is complete",
  "uploadAttachments": "Local attachments (optional)",
  "selectedAttachmentCount": n => `${n} file(s) selected`,
  "attachmentUploadHint": "Files upload only after confirmation. Canceling uploads nothing.",
  "attachmentLimit": n => `A relay can send at most ${n} files. Remove some materials.`,
  "materialMountFailed": "File upload failed and the relay was not submitted. Check the files and retry.",
  "uploadingAndSending": "Uploading and sending…",
  "removeMaterial": name => `Remove ${name}`,
  "codingGitReferenceNote": (total, related) => `The Coding Session workspace has ${total} uncommitted change(s); ${related} related path(s) are recorded in this reference.`,
  "selectFiles": "Materials sent with work",
  "workTitle": "Work title",
  "workTitlePlaceholder": "For example: Prepare release notes",
  "assignTo": "Assign to",
  "handoffTo": "Invite collaborator",
  "currentWork": "Current work",
  "selectedContext": n => `The relay brief and the names of ${n} shared material${n === 1 ? '' : 's'} will be included`,
  "agentPreviewTitle": "Continue in my private workspace",
  "agentPreviewBody": "Pinvou prepares the relay context, then creates or opens this work item’s private Agent session. Opening it again reuses the same session.",
  "preparedForAgent": n => `${n} shared file${n === 1 ? '' : 's'}`,
  "linkedAgentSession": "Linked Agent session for this work",
  "agentSessionChoice": "Choose a session",
  "reuseAgentSession": "Continue current session",
  "useNewAgentSession": "Create new session",
  "useNewAgentSessionHint": "Keep the current session and create a new one for this work.",
  "agentWorkMode": "Work mode",
  "chatWorkMode": "Pinvou chat",
  "pinvouCodeWorkMode": "Pinvou Code mode",
  "codeAgent": "Code Agent",
  "defaultPinvouAgent": "Pinvou (default)",
  "otherAcpAgents": "Other ACP Agents",
  "agentNotReady": "not ready",
  "localCodeWorkspace": "Local code workspace (optional)",
  "localCodeWorkspaceHint": "This relay includes a Git reference. You may choose an existing local checkout. Pinvou never clones, checks out, or pulls automatically.",
  "localCodeWorkspaceOptionalHint": "Without a folder, a temporary code workspace is used.",
  "chooseWorkspace": "Choose code folder",
  "changeWorkspace": "Change folder",
  "agentMaterials": "Materials to bring into the session",
  "firstMaterialSummary": n => `${n} material(s) selected by default`,
  "continueMaterialSummary": n => n ? `${n} new material(s) selected by default` : 'No new materials',
  "introducedMaterialSummary": n => `${n} material${n === 1 ? '' : 's'} already in this session; no need to select again`,
  "alreadyIntroduced": "already added",
  "selectAll": "Select all",
  "clearSelection": "Clear",
  "gitContext": "Git context in this relay",
  "contextUpdated": "new context",
  "agentPrivacy": "Only the task, goal, and selected materials are prefilled, and nothing is sent automatically. Add the specific requirements before sending.",
  "agentPrepareFailed": "Could not prepare materials or open the Agent session. Check the materials and workspace, then retry.",
  "agentSessionTitle": title => `Relay · ${title}`,
  "buildAgentPrompt": (task, workspace, options) => buildTeamRelayAgentPrompt('en', task, workspace, options),
  "packageId": sequence => `Package P${sequence}`,
  "acceptedPackage": sequence => `Accepted final package P${sequence}`,
  "acceptedPackageSummary": (files, gitRefs) => gitRefs ? `${files} file snapshot(s) and ${gitRefs} historical pinned Git reference(s)` : `${files} file snapshot(s)`,
  "artifactRoleLabel": "Material role",
  "artifactRoles": {
    "input": "Input",
    "intermediate": "Intermediate result",
    "final": "Final result",
    "evidence": "Evidence"
  },
  "expectedNext": "Expected next step",
  "expectedNextPlaceholder": "What should the recipient do with this package?",
  "openQuestions": "Open questions",
  "openQuestionsPlaceholder": "Items that still need a decision or clarification",
  "gitReference": "Git delivery reference",
  "mutableGitWarning": "No pinned commit; this reference can change",
  "endedStatus": "Ended",
  "endedBanner": {
    "title": "Work ended",
    "body": "The final delivery was accepted. The relay history and shared materials remain in the record."
  },
  "cancelledStatus": "Cancelled",
  "cancelledBanner": {
    "title": "Work cancelled",
    "body": "The initiator stopped this work. Its reason, relay history, and shared materials remain in the record."
  },
  "dynamic": {
    "currentAt": name => `Currently with ${name}`,
    "waitingBanner": name => ({ title: `It is ${name}'s turn`, body: `${name} is working and should return the work through this relay when done.` })
  },
  "dialogs": {
    "assign": {
      "title": "Assign task",
      "subtitle": "The recipient becomes the owner and receives full materials only after accepting.",
      "routeLabel": "Task owner",
      "taskLabel": "What needs doing",
      "acceptanceLabel": "Delivery goal",
      "messagePlaceholder": "For example: Improve the particle page and fix known bugs",
      "artifactRole": "input",
      "confirm": "Send task"
    },
    "progress": {
      "title": "Update progress",
      "subtitle": "Record progress and materials without handing the relay to someone else.",
      "routeLabel": "Relay stays with",
      "taskLabel": "Current progress",
      "acceptanceLabel": "Next step (optional)",
      "acceptancePlaceholder": "For example: Continue validating the Windows installer",
      "acceptanceOptional": true,
      "messagePlaceholder": "Summarize completed work, current activity, validation, or blockers",
      "artifactRole": "evidence",
      "confirm": "Post update"
    },
    "handoff": {
      "title": "Invite collaborator",
      "subtitle": "After accepting, they become the current actor; you remain accountable and receive the result back.",
      "routeLabel": "Invite collaborator",
      "taskLabel": "What help is needed",
      "acceptanceLabel": "How to return it",
      "messagePlaceholder": "Describe the part to add, verify, or handle",
      "artifactRole": "intermediate",
      "confirm": "Send collaboration invite"
    },
    "return": {
      "title": "Return to previous owner",
      "subtitle": "Return the result and materials to the person who sent this invitation.",
      "routeLabel": "Return to",
      "taskLabel": "What was completed",
      "acceptanceLabel": "What they should do next",
      "messagePlaceholder": "Summarize changes, locations, validation, and limitations",
      "artifactRole": "intermediate",
      "confirm": "Return"
    },
    "deliver": {
      "title": "Final delivery",
      "subtitle": "Deliver the completed result for acceptance or changes.",
      "routeLabel": "Deliver to",
      "taskLabel": "What is being delivered",
      "acceptanceLabel": "How to accept it",
      "messagePlaceholder": "Summarize the result, validation, usage, and known limitations",
      "artifactRole": "final",
      "confirm": "Deliver"
    },
    "supplement": {
      "title": "Request changes",
      "subtitle": "Keep the work open and return it to its owner in the same thread.",
      "routeLabel": "Return to",
      "taskLabel": "What needs changing",
      "acceptanceLabel": "Resubmission standard",
      "messagePlaceholder": "Describe missing, incorrect, or unverified parts",
      "artifactRole": "input",
      "confirm": "Return for changes"
    },
    "cancelWork": {
      "title": "Cancel this work",
      "subtitle": "The relay will stop, but its complete history and materials remain.",
      "reasonLabel": "Cancellation reason",
      "messagePlaceholder": "Explain why this work should no longer continue",
      "auditHint": "The reason becomes a team-visible, immutable relay record.",
      "confirm": "Cancel work"
    },
    "declineTransfer": {
      "title": "Decline request",
      "subtitle": "The task or relay will not move to you, and the sender will receive your feedback.",
      "reasonLabel": "Reason",
      "messagePlaceholder": "Explain why you cannot take this work",
      "auditHint": "The reason is kept in the immutable relay history.",
      "confirm": "Decline"
    },
    "withdrawTransfer": {
      "title": "Withdraw request",
      "subtitle": "The recipient has not accepted yet. You can choose someone else afterward.",
      "reasonLabel": "Reason",
      "messagePlaceholder": "Explain why the request is being withdrawn",
      "auditHint": "The sent record remains visible in relay history.",
      "confirm": "Withdraw"
    },
    "rerouteTransfer": {
      "title": "Change recipient",
      "subtitle": "The previous request expires and the new recipient must confirm.",
      "routeLabel": "New recipient",
      "reasonLabel": "Reason",
      "messagePlaceholder": "Explain why the recipient is changing",
      "auditHint": "The change is kept in immutable relay history.",
      "confirm": "Send to new recipient"
    },
    "agent": {
      "title": "Continue with my Agent",
      "subtitle": "Work privately without sharing your Agent conversation.",
      "confirm": "Open private session"
    }
  },
  "legacyTransfers": {
    "askForHelp": "Transfer directly (legacy)",
    "assignedToast": "Task assigned directly",
    "handedOffToast": "Transferred directly; waiting for return",
    "reroutedToast": "Recipient changed and the transfer took effect immediately",
    "dialogs": {
      "assign": {
        "subtitle": "This Hub does not support acceptance yet. The recipient becomes the owner immediately.",
        "confirm": "Assign directly"
      },
      "handoff": {
        "subtitle": "This Hub does not support relay confirmation yet. The recipient becomes the current actor immediately.",
        "confirm": "Transfer now"
      }
    },
    "recipientDialogs": {
      "assign": {
        "subtitle": "The recipient has not recently used a client that supports acceptance. They will become the owner immediately.",
        "confirm": "Assign directly"
      },
      "handoff": {
        "subtitle": "The recipient has not recently used a client that supports relay confirmation. They will become the current actor immediately.",
        "confirm": "Invite directly"
      },
      "rerouteTransfer": {
        "subtitle": "The new recipient has not recently used a confirmation-capable client. The relay will move immediately.",
        "confirm": "Change and apply now"
      }
    }
  },
  "toasts": {
    "assigned": "Task sent; waiting for acceptance",
    "transferAccepted": "Accepted; the baton is now with you",
    "transferDeclined": "Declined and the sender was notified",
    "transferWithdrawn": "Withdrawn; you can choose another recipient",
    "transferRerouted": "Sent to the new recipient",
    "progressUpdated": "Progress updated; the relay is still with you",
    "handedOff": "Relay invitation sent; the baton stays with you until acceptance",
    "delivered": "Final delivery sent",
    "supplementRequested": "Returned to owner for changes",
    "returned": "The collaborator returned the work; it is your turn",
    "accepted": "Delivery approved and work closed",
    "cancelled": "Task cancelled and history retained",
    "pinned": "Pinned for you",
    "unpinned": "Unpinned",
    "hidden": "Archived; restore it from Archived",
    "restored": "Restored to the work list"
  },
  "blockingWork": (message, threads) => `${message} Blocking work: ${threads.join('; ')}`,
  "blockingThread": thread => {
    const labels = { "active": "In progress", "pending_transfer": "Waiting for confirmation", "awaiting_reassignment": "Needs reassignment", "awaiting_acceptance": "Awaiting approval" };
    return labels[thread.phase] ? `${thread.title || thread.thread_id} (${labels[thread.phase]})` : `${thread.title || thread.thread_id}`;
  },
  "lifecycleSyncPending": "The operation is complete, but team data has not synced yet. Sync again to load the latest state."
};
dictEn["kbOutWeekdays"] = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
dictEn["builtinModelSelect"] = "Backend model";
dictEn["builtinModelRefresh"] = "Refresh";
dictEn["builtinModelLoading"] = "Syncing…";
dictEn["builtinModelEmpty"] = "No backend models";
dictEn["uiRemote"]["status"]["idle"] = ["Not enabled", "Enable access to create a persistent remote control link."];
dictEn["uiRemote"]["status"]["starting"] = ["Starting", "Creating the remote control connection."];
dictEn["uiRemote"]["status"]["connecting_relay"] = ["Connecting", "Connecting to the relay…"];
dictEn["uiRemote"]["status"]["waiting_web_client"] = ["Waiting for browser", "Paste the link into a browser on your phone or computer."];
dictEn["uiRemote"]["status"]["web_client_connected"] = ["Browser connected", "A browser is connected to this desktop."];
dictEn["uiRemote"]["status"]["web_client_disconnected"] = ["Browser disconnected", "The link remains valid and can reconnect at any time."];
dictEn["uiRemote"]["status"]["revoked"] = ["Link revoked", "Enable remote control again."];
dictEn["uiRemote"]["status"]["stopped"] = ["Stopped", "Open this panel to enable access again."];
dictEn["uiRemote"]["status"]["error"] = ["Connection error", "Remote control is temporarily unavailable. Try again."];
dictEn["uiSettingsDetail"]["builtinModel"] = "Built-in model";
dictEn["uiSettingsDetail"]["settingsLoadFailedDetail"] = "Settings cannot be displayed right now. Reopen the page and try again.";
dictEn["uiScheduled"]["weekdays"] = [["Monday", "Mon"], ["Tuesday", "Tue"], ["Wednesday", "Wed"], ["Thursday", "Thu"], ["Friday", "Fri"], ["Saturday", "Sat"], ["Sunday", "Sun"]];
dictEn["uiAccount"] = {
  "backendUser": "Backend user",
  "backendAccount": "Backend account",
  "close": "Close",
  "availableQuota": "Available quota",
  "refreshQuota": "Refresh quota",
  "monthlyQuota": "Monthly quota:",
  "used": "Used:",
  "remaining": "Remaining:",
  "syncing": "Syncing…",
  "username": "New API username",
  "password": "New API password",
  "loginFailed": "Login failed",
  "loginUnavailable": "Built-in model sign-in is unavailable",
  "builtinLoginFailed": "Built-in model sign-in failed",
  "loggingIn": "Signing in…",
  "loginAndSync": "Sign in and sync quota",
  "syncFailed": "Quota sync failed",
  "viewQuota": "View available quota",
  "errors": {
    "login_unavailable": "Built-in model sign-in is unavailable",
    "not_logged_in": "Sign in to the built-in model account first",
    "unsupported_platform": "Built-in model accounts are not supported on this system",
    "device_not_bound": "This device is not bound yet",
    "device_binding_failed": "Device binding failed",
    "admin_credential_missing": "The built-in model service configuration is incomplete",
    "provisioning_failed": "Built-in model setup failed",
    "service_unreachable": "The built-in model service is temporarily unreachable",
    "rate_limited": "Too many requests. Try again later",
    "service_disabled": "The built-in model service is disabled",
    "unavailable": "The built-in model service is temporarily unavailable",
    "user_not_found": "The built-in model account could not be found",
    "permission_denied": "This account does not have access"
  },
  "status": {
    "unavailable": "Temporarily unavailable",
    "ready": "Enabled",
    "not_started": "Not enabled",
    "querying_user": "Checking account",
    "creating_token": "Enabling",
    "configuring_policy": "Configuring",
    "disabled": "Disabled",
    "failed": "Setup failed",
    "unknown": "Syncing"
  },
  "settingsLoadFailed": "Failed to load settings",
  "settingsLoadFailedDetail": "Settings cannot be displayed right now. Reopen the page and try again."
};
dictEn["uiToolStore"]["guide"]["types"] = ["Standard skill (SKILL.md folder)", "Single .md skill file", "MCP server", "Pinvou plugin package (recommended)"];
dictEn["uiToolStore"]["connectorErrors"] = {
    "consent_persist_failed": "Authorization completed, but persisting the default-off consent failed. Turn the connector off in the composer tools list, then reconnect.",
  "runtime_prepare_failed": "Failed to prepare the connector runtime. Try again.",
  "cli_install_failed": "Failed to install the connector. Check your network and try again.",
  "auth_start_failed": "Unable to start sign-in. Try again.",
  "registration_failed": "App registration was not completed. Reconnect and try again.",
  "auth_failed": "Sign-in authorization was not completed. Reconnect and try again.",
  "skills_enable_failed": "Authorization completed, but enabling the skill failed. Try again.",
  "sso_start_failed": "Unable to start SSO sign-in. Check your intranet connection and try again.",
  "sso_timeout": "SSO sign-in timed out. Reconnect and try again.",
"cli_data_access_disabled": "Your organization has not enabled CLI data access for DingTalk. Ask your organization admin to enable “Allow members to access their personal data via CLI” in the DingTalk open-platform developer settings, then sign in again.",
  "unknown": "Connection failed. Try again later."
};
dictEn["uiToolStore"]["sso"] = {
  "title": name=>`Sign in to ${name}`,
  "hint": "Scan the code or finish SSO sign-in in your browser. The tool connects automatically after sign-in (intranet required).",
  "qrAlt": name=>`${name} sign-in QR code`,
  "generating": "Generating QR code…",
  "openBrowser": "Open sign-in in browser",
  "waiting": "Waiting for sign-in…",
  "throttled": seconds => `SSO sign-in is rate-limited. The QR code appears in about ${seconds}s…`,
  "previousFlow": "Waiting for the previous sign-in to finish…"
};
dictEn["uiToolStore"]["internalDetailDesc"] = "Connect directly to H3C internal systems with your SSO identity. Sign in in the browser or by QR code; no key is required, and data stays on the corporate intranet.";
dictEn["uiToolStore"]["internalUsageHint"] = "Corporate intranet required · On first connection, complete SSO in the browser or by QR code; no key required";
dictEn["uiToolStore"]["feishuSteps"] = [{
    "key": "runtime",
    "label": "Prepare runtime",
    "sub": "Use the app-provided Node runtime"
  }, {
    "key": "cli",
    "label": "Install connector",
    "sub": "lark-cli · about 40 seconds on first use"
  }, {
    "key": "connect",
    "label": "Connect & authorize",
    "sub": "Create app identity"
  }, {
    "key": "qr",
    "label": "Scan to sign in",
    "sub": "Scan with the Feishu app"
  }];
dictEn["uiToolStore"]["wecomSteps"] = [{
    "key": "runtime",
    "label": "Prepare runtime",
    "sub": "Use the app-provided Node runtime"
  }, {
    "key": "cli",
    "label": "Install connector",
    "sub": "wecom-cli · about 40 seconds on first use"
  }, {
    "key": "qr",
    "label": "Scan to sign in",
    "sub": "Scan with the WeCom app"
  }];
dictEn["uiToolStore"]["dingtalkSteps"] = [{
    "key": "runtime",
    "label": "Prepare runtime",
    "sub": "Use the app-provided Node runtime"
  }, {
    "key": "cli",
    "label": "Install connector",
    "sub": "dws · about 40 seconds on first use"
  }, {
    "key": "qr",
    "label": "Scan to sign in",
    "sub": "Scan with the DingTalk app"
  }];
dictEn["uiToolStore"]["toolNames"]["eip"] = "Employee Portal (EIP)";
dictEn["uiToolStore"]["toolNames"]["zhidao"] = "Zhidao Knowledge Base";
dictEn["uiToolStore"]["skillLabel"] = "Skill";
dictEn["uiToolStore"]["storeData"]["categories"]["h3c"] = "H3C Internal";
dictEn["uiToolDetails"]["tools"]["ima"]["welcomeQueries"] = ["Search my ima knowledge bases", "List my ima notes", "Create an ima note from this content", "Find the product plan in my ima knowledge base"];
dictEn["uiToolDetails"]["tools"]["tmeet"]["welcomeQueries"] = ["Create a Tencent Meeting for me", "Show my Tencent Meeting recordings", "Show the latest AI meeting minutes", "List the attendees of this Tencent Meeting"];
dictEn["uiToolDetails"]["tools"]["eip"] = {
  "title": "H3C Employee Portal (EIP)",
  "subtitle": "Use your SSO identity to check attendance, leave, overtime, approvals, contacts, news, and spending",
  "type": "CLI + Official Skill",
  "latency": "Intranet",
  "desc": "Connect to the H3C EIP employee portal with your SSO identity to check attendance, leave balances, overtime, pending approvals, colleague details, company news, cafeteria spending, and reimbursements. Sign in through the browser; no key is required. Corporate intranet access is required.",
  "welcomeQueries": ["Check my attendance exceptions this month", "How many annual leave days do I have", "Show my pending approvals", "Find a colleague’s contact details"]
};
dictEn["uiToolDetails"]["tools"]["zhidao"] = {
  "title": "H3C Zhidao · Internal Knowledge Base",
  "subtitle": "Search H3C internal knowledge and resources with your SSO identity",
  "type": "CLI + Official Skill",
  "latency": "Intranet",
  "desc": "Connect to the H3C Zhidao internal knowledge base with your SSO identity to search knowledge entries, repositories, and documents, and answer questions using internal company knowledge. Sign in by QR code or through the browser; no key is required. Corporate intranet access is required.",
  "welcomeQueries": ["Find the switch configuration standards in Zhidao", "Search the internal repository for security solutions", "Find troubleshooting documents for this issue", "Find internal knowledge about this product"]
};
dictEn["uiToolDetails"]["tools"]["weather"]["configFields"] = [{
    "key": "AMAP_KEY",
    "label": "API Key",
    "helpText": "Choose the “Web Service” type.",
    "placeholder": "Paste your Amap Web Service Key"
  }];
dictEn["uiToolDetails"]["tools"]["weather"]["welcomeQueries"] = ["Weather in Hangzhou today", "Will it rain in Beijing this week", "What to wear in Shanghai tomorrow"];
dictEn["uiToolDetails"]["tools"]["iwencai"]["configFields"] = [{
    "key": "IWENCAI_API_KEY",
    "label": "API Key",
    "helpText": "Open any official Skill and copy it from “Installation”.",
    "placeholder": "Paste your IWENCAI_API_KEY"
  }];
dictEn["uiToolDetails"]["tools"]["iwencai"]["welcomeQueries"] = ["Latest Moutai share price", "How is the market doing today", "Bank stocks with P/E below 10", "Recent rate-cut news"];
dictEn["uiToolDetails"]["tools"]["tencent-docs"]["configFields"] = [{
    "key": "TENCENT_DOCS_TOKEN",
    "label": "Personal token",
    "helpText": "Open the authorization page, sign in by scanning a QR code with QQ or WeChat, and copy the token. Do not add a Bearer prefix.",
    "placeholder": "Paste your Tencent Docs personal token"
  }];
dictEn["uiToolDetails"]["tools"]["tencent-docs"]["welcomeQueries"] = ["List my recent Tencent Docs", "Turn this weekly report into a Tencent smart document", "Create an online spreadsheet to track expenses", "Read and analyze this Tencent Docs spreadsheet"];
dictEn["uiToolDetails"]["tools"]["wecom-bot"]["configFields"] = [{
    "key": "WECOM_BOT_KEY",
    "label": "Webhook Key",
    "helpText": "In a WeCom group, tap “…” (top right) → Group Bots → Add a bot, then copy its webhook URL.",
    "placeholder": "Paste the bot key or the full webhook URL"
  }];
dictEn["uiToolDetails"]["tools"]["wecom-bot"]["welcomeQueries"] = ["Push today’s weather bulletin to my group", "Format this weekly report as Markdown and send it to the WeCom group", "@everyone: don’t forget the 3 p.m. meeting", "Send this file to the WeCom group"];
dictEn["uiToolDetails"]["tools"]["feishu"]["welcomeQueries"] = ["Read a Feishu doc and make a PPT for me", "Summarize my Feishu docs", "Check my Feishu calendar today", "Show my Feishu to-do tasks"];
dictEn["uiToolDetails"]["tools"]["wecom"]["welcomeQueries"] = ["Turn this content into a WeCom smart doc", "Read the content of one of my WeCom docs", "Create a new sub-sheet in a WeCom smart sheet", "Query data in a WeCom smart sheet"];
dictEn["uiToolDetails"]["tools"]["dingtalk"]["welcomeQueries"] = ["Read my DingTalk docs", "Check my DingTalk schedule today", "Query data in a DingTalk AI sheet", "Show my DingTalk to-dos"];
dictEn["uiToolDetails"]["tools"]["qcc"]["welcomeQueries"] = ["Look up Huawei’s business registration info", "Tencent’s business registration details", "BYD’s outbound investments", "Alibaba’s shareholder structure"];
dictEn["uiToolDetails"]["tools"]["patsnap-search"]["configFields"] = [{
    "key": "PATSNAP_API_KEY",
    "label": "Patsnap API Key",
    "helpText": "Get it from the Patsnap open platform or the MCP/API credentials provided by your enterprise administrator.",
    "placeholder": "Paste your Patsnap API Key"
  }];
dictEn["uiToolDetails"]["tools"]["patsnap-search"]["welcomeQueries"] = ["Search patents and papers on solid-state battery electrolytes", "Find core CRISPR delivery-system patents and literature from the past five years", "Get patent details for publication CN109123456A", "Analyze CATL’s patent layout in sodium-ion batteries"];
dictEn["uiToolDetails"]["tools"]["canva-mcp"]["welcomeQueries"] = ["Generate a product launch poster", "Make a three-page product intro presentation", "Design a Xiaohongshu cover", "Create an event promo image from a brand template"];
dictEn["uiToolDetails"]["tools"]["obsidian"]["welcomeQueries"] = ["Search my notes", "Create a note to record today’s ideas", "What documents are in my knowledge base?", "Summarize my notes"];
dictEn["uiToolDetails"]["tools"]["yuandian-mcp"]["welcomeQueries"] = ["Search cases about labor contract termination", "Look up regulations on corporate equity liability", "Analyze a company’s judicial risks", "Find recent adjudication viewpoints"];
dictEn["uiToolDetails"]["tools"]["pptx"]["welcomeQueries"] = ["Make a Q2 quarterly report PPT", "Make a product intro PPT", "Make a project proposal presentation", "Make a company intro PPT"];
dictEn["uiToolDetails"]["tools"]["gongwen"]["welcomeQueries"] = ["Draft a notice on issuing management measures", "Write an implementation opinion on strengthening certain work", "Draft a meeting notice", "Write a situation report"];
dictEn["uiToolDetails"]["tmeetSteps"] = [{
    "key": "runtime",
    "label": "Prepare runtime",
    "sub": "Use the app-provided Node runtime"
  }, {
    "key": "cli",
    "label": "Install connector",
    "sub": "tmeet · about 40 seconds on first use"
  }, {
    "key": "qr",
    "label": "Sign in",
    "sub": "Tencent Meeting authorization page"
  }];
dictEn["uiWebConnection"]["idle"] = ["Preparing remote control", "Initializing browser connection…"];
dictEn["uiWebConnection"]["connecting"] = ["Connecting to desktop", "Interrupted connections retry automatically; unconfirmed actions are not repeated."];
dictEn["uiWebConnection"]["desktop_offline"] = ["Desktop is offline", "Keep this page open; it will reconnect when the desktop returns."];
dictEn["uiWebConnection"]["credentials_missing"] = ["Incomplete link", "Enable remote control on the desktop and paste the complete link."];
dictEn["uiWebConnection"]["denied"] = ["Access denied", "The link is invalid or was refreshed. Copy a new link from the desktop."];
dictEn["uiWebConnection"]["revoked"] = ["Access stopped", "The desktop stopped this remote control link."];
dictEn["uiWebConnection"]["replaced"] = ["Taken over by another browser", "Only one active browser is allowed. Refresh this page to take over again."];
dictEn["uiWebConnection"]["incompatible_desktop"] = ["Desktop version incompatible", "Update the desktop app, then reopen this link."];
dictEn["uiWebConnection"]["error"] = ["Connection error", "Remote control will keep trying to reconnect."];
dictEn["uiCodex"]["commandMenuClose"] = "Close Agent command menu";
dictEn["uiCodex"]["workspaceMenuClose"] = "Close working-directory menu";
dictEn["uiCodex"]["nativeMemoryClose"] = "Close memory panel";
dictEn["uiCodex"]["closeAccountMenu"] = "Close account menu";
dictEn["bnrFaces"] = [["Product", "Product design"], ["ML Engineer", "AI models"], ["Growth", "Growth ops"], ["UI Designer", "Interface"], ["Data Analyst", "Insights"]];
dictEn["uiChatScenes"]["attachmentPrompt"] = "Continue based on the attached content.";
dictEn["uiPlatformMisc"]["tauriBridge"] = {
  "builtinModelUnavailable": "The built-in model is unavailable for this account. Select another model first."
};
dictEn["uiSettingsView"]["serviceEip"] = "H3C Employee Portal (EIP)";
dictEn["uiSettingsView"]["serviceZhidao"] = "H3C Zhidao";
dictEn["uiTeamRelayAccess"] = {
  "section": "Beta Features",
  "title": "Team Relay",
  "desc": "Show the Team Relay entry in the sidebar after access is enabled.",
  "open": "Enable Relay",
  "dialogTitle": "Enable Team Relay",
  "notice": "This feature is currently in closed beta and is not generally available. Contact customer support for access.",
  "passwordLabel": "Access password",
  "passwordPlaceholder": "Enter the access password",
  "invalid": "Incorrect password. Try again.",
  "unavailable": "The setting could not be saved. Try again.",
  "cancel": "Cancel",
  "confirm": "Enable",
  "opening": "Verifying…",
  "close": "Close",
  "enabledTitle": "Team Relay enabled",
  "enabledDesc": "The sidebar entry is visible and will remain enabled after PINVOU restarts.",
  "disable": "Disable Relay",
  "disabling": "Disabling…",
  "disableUnavailable": "Team Relay could not be disabled. Try again."
};
const behaviorSettingsEn = /** @type {Record<string, string | ((days: number) => string)>} */ (dictEn.uiSettingsDetail);
behaviorSettingsEn.behaviorAnalyticsTitle = "Privacy and usage analytics";
behaviorSettingsEn.behaviorAnalyticsEntryTitle = "Data Management";
behaviorSettingsEn.behaviorAnalyticsEntryDesc = "Manage product improvement and data usage settings";
behaviorSettingsEn.behaviorAnalyticsBack = "Back to Help & Feedback";
behaviorSettingsEn.behaviorAnalyticsLabel = "Help improve PINVOU";
behaviorSettingsEn.behaviorAnalyticsDesc = "Only controlled feature outcomes, timing, and error categories are collected; conversations, file contents, paths, and credentials are never collected.";
behaviorSettingsEn.behaviorAnalyticsNotConfigured = "Usage analytics is not configured in this build and remains off.";
behaviorSettingsEn.behaviorAnalyticsEnableConfirm = "PINVOU will collect controlled feature usage and quality data to improve the product. Conversation text, file contents, paths, tool arguments, and credentials are never collected. Enable analytics?";
behaviorSettingsEn.behaviorAnalyticsDisableConfirm = "Collection stops immediately and pending events are cleared. An offline withdrawal will retry when connectivity returns. Turn analytics off?";
behaviorSettingsEn.behaviorAnalyticsStatusEnabled = "On";
behaviorSettingsEn.behaviorAnalyticsStatusDisabled = "Off";
behaviorSettingsEn.behaviorAnalyticsStatusWithdrawalPending = "Remote withdrawal pending";
behaviorSettingsEn.behaviorAnalyticsActionFailed = "The operation failed. Try again later.";
// features/chat background task indicator entries (uiChat exists, merge instead of overwrite)
Object.assign(dictEn.uiChat, {
  bgTasks: 'Background Tasks',
  bgTasksRunning: n => `${n} background task${n === 1 ? '' : 's'} running`,
});
// restored: baseline catalog descriptions (referenced by the model catalog data)
Object.assign(dictEn.uiSettingsDetail.modelDescriptions, { '本地服务默认模型': 'Default model for the local service', '高能力模型': 'High-capability model', '快速响应': 'Fast responses', '最新通用模型': 'Latest general-purpose model', '代码场景': 'Coding tasks', '高速代码场景': 'High-speed coding tasks', '稳定可用': 'Stable release', '高性价比': 'Cost-effective', '通用能力': 'General-purpose', '兼容保留': 'Compatibility option', '高速响应': 'High-speed responses', '轻量模型': 'Lightweight model'
});
