import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dict } from '../src/shared/i18n.js';
import {
  canUpdateRelayProgress, deriveBoardColumn, deriveCategory, matchesRelayView,
  matchesSourceFilter, projectMaterialPackages, projectSnapshot, projectTeamBoard,
  relayViewCounts,
} from '../src/features/team-relay/team-relay-state.js';
import { disableTeamRelayAccess, loadTeamRelayAccess, unlockTeamRelayAccess } from '../src/features/team-relay/team-relay-access.js';

assert.equal(loadTeamRelayAccess({ getItem: () => null }), false);
assert.equal(loadTeamRelayAccess({ getItem: () => 'enabled-v1' }), true);
assert.equal(loadTeamRelayAccess({ getItem: () => { throw new Error('storage unavailable'); } }), false);
const rejectedAccessWrites = [];
assert.equal(await unlockTeamRelayAccess('incorrect-access-code', async () => false, {
  getItem: () => null,
  setItem: (...args) => rejectedAccessWrites.push(args),
}), false);
assert.deepEqual(rejectedAccessWrites, []);
const acceptedAccessWrites = [];
assert.equal(await unlockTeamRelayAccess('test-only-access-code', async code => code === 'test-only-access-code', {
  setItem: (...args) => acceptedAccessWrites.push(args),
}), true);
assert.deepEqual(acceptedAccessWrites, [['pinvou.team-relay.beta-access', 'enabled-v1']]);
let persistedAccess = 'enabled-v1';
const persistedAccessStorage = {
  getItem: () => persistedAccess,
  removeItem: () => { persistedAccess = null; },
};
assert.equal(loadTeamRelayAccess(persistedAccessStorage), true);
disableTeamRelayAccess(persistedAccessStorage);
assert.equal(loadTeamRelayAccess(persistedAccessStorage), false);

assert.equal(deriveCategory({ phase: 'closed' }, 'me'), 'ended');
assert.equal(deriveCategory({ phase: 'cancelled' }, 'me'), 'ended');
assert.equal(deriveCategory({ phase: 'awaiting_acceptance', current_actor_id: 'me' }, 'me'), 'receive');
assert.equal(deriveCategory({ phase: 'active', current_actor_id: 'me' }, 'me'), 'pending');
assert.equal(deriveCategory({ phase: 'active', current_actor_id: 'other', return_to_id: 'me' }, 'me'), 'waiting');
assert.equal(deriveCategory({ phase: 'active', current_actor_id: 'other', initiator_id: 'me' }, 'me'), 'waiting');
assert.equal(deriveCategory({ phase: 'awaiting_acceptance', current_actor_id: 'other' }, 'me'), 'waiting');
assert.equal(deriveBoardColumn({ phase: 'active', current_actor_id: 'owner', owner_id: 'owner' }), 'active');
assert.equal(deriveBoardColumn({ phase: 'active', current_actor_id: 'helper', owner_id: 'owner' }), 'collaborating');
assert.equal(deriveBoardColumn({ phase: 'awaiting_acceptance' }), 'acceptance');
assert.equal(deriveBoardColumn({ phase: 'closed' }), 'ended');
assert.equal(canUpdateRelayProgress({ phase: 'active', current_actor_id: 'me' }, 'me'), true);
assert.equal(canUpdateRelayProgress({ phase: 'active', current_actor_id: 'other' }, 'me'), false);
assert.equal(canUpdateRelayProgress({ phase: 'awaiting_acceptance', current_actor_id: 'me' }, 'me'), false);
assert.equal(canUpdateRelayProgress({ phase: 'closed', current_actor_id: null }, 'me'), false);

const snapshot = {
  bootstrap: {
    protocol_version: '0.1', team: { id: 'team', name: 'Demo' }, me: { id: 'me', display_name: '我' },
    members: [{ id: 'me', display_name: '我', role: 'owner' }, { id: 'b', display_name: 'B', role: 'member' }],
    threads: [{
      thread: { id: 'thread', team_id: 'team', title: '方案', initiator_id: 'me', owner_id: 'b', current_actor_id: 'b', return_to_id: 'me', final_recipient_id: 'me', phase: 'active', revision: 1, created_at: '2026-08-04T00:00:00Z', updated_at: '2026-08-04T00:00:00Z' },
      participants: ['me', 'b'],
      actions: [{ id: 'action', thread_id: 'thread', sequence: 1, kind: 'assign', actor_id: 'me', recipient_id: 'b', return_to_id: 'me', message: '请处理', base_revision: 0, idempotency_key: 'key', attachment_ids: ['file'], created_at: '2026-08-04T00:00:00Z' }],
    }],
  },
  attachments: { file: { id: 'file', original_name: '方案.md' } },
};
const tasks = projectSnapshot(snapshot, dict.zh.uiTeamRelay);
assert.equal(tasks.length, 1);
assert.equal(tasks[0].category, 'waiting');
assert.equal(tasks[0].owner, 'B');
assert.equal(tasks[0].files[0].original_name, '方案.md');
assert.equal(tasks[0].events[0].delivery.artifact_role, 'input');
assert.equal(tasks[0].events[0].delivery.subtype, '');
assert.equal(tasks[0].events[0].sequence, 1);
assert.equal(tasks[0].teamId, 'team');
assert.equal(tasks[0].teamName, 'Demo');

const secondBootstrap = JSON.parse(JSON.stringify(snapshot.bootstrap));
secondBootstrap.team = { id: 'team-two', name: 'Second team' };
secondBootstrap.threads[0].thread.id = 'thread-two';
secondBootstrap.threads[0].thread.team_id = 'team-two';
secondBootstrap.threads[0].thread.title = '跨团队任务';
secondBootstrap.threads[0].actions[0].id = 'action-two';
secondBootstrap.threads[0].actions[0].thread_id = 'thread-two';
const crossTeamTasks = projectSnapshot({ ...snapshot, team_bootstraps: [snapshot.bootstrap, secondBootstrap] }, dict.zh.uiTeamRelay);
assert.deepEqual(crossTeamTasks.map(task => [task.id, task.teamId, task.teamName]), [
  ['thread', 'team', 'Demo'],
  ['thread-two', 'team-two', 'Second team'],
]);
assert.deepEqual(relayViewCounts(tasks, 'me'), { attention: 0, waiting: 1, all: 1, ended: 0, hidden: 0 });
assert.equal(matchesSourceFilter(tasks[0], 'sent', 'me'), true);
assert.equal(matchesSourceFilter(tasks[0], 'received', 'me'), false);
assert.equal(matchesSourceFilter(tasks[0], 'collaborating', 'me'), false);
assert.equal(matchesRelayView({ category: 'pending', raw: { initiator_id: 'other', owner_id: 'me' } }, 'attention', 'me'), true);
assert.equal(matchesSourceFilter({ raw: { initiator_id: 'other', owner_id: 'me' } }, 'received', 'me'), true);
assert.equal(matchesSourceFilter({ raw: { initiator_id: 'other', owner_id: 'other' } }, 'collaborating', 'me'), true);
assert.equal(matchesRelayView({ category: 'waiting', raw: { initiator_id: 'other', owner_id: 'other' } }, 'waiting', 'me'), false);
assert.equal(matchesRelayView({ category: 'waiting', raw: { initiator_id: 'other', owner_id: 'other' } }, 'all', 'me'), true);
const board = projectTeamBoard(snapshot, tasks);
assert.deepEqual(board.summary, { members: 2, inProgress: 1, acceptance: 0, completed: 0 });
assert.equal(board.columns.active[0].id, 'thread');
assert.equal(board.members[0].isMe, true);
assert.equal(board.members.find(member => member.id === 'b').activeCount, 1);
assert.equal(board.members.find(member => member.id === 'b').waitingCount, 0);
assert.equal(board.members.find(member => member.id === 'b').ownedCount, 1);

const noteSnapshot = JSON.parse(JSON.stringify(snapshot));
noteSnapshot.bootstrap.threads[0].actions.push(
  { id: 'legacy-note', thread_id: 'thread', sequence: 2, kind: 'note', actor_id: 'me', recipient_id: null, return_to_id: null, message: '旧版备注', base_revision: 1, idempotency_key: 'legacy-note-key', attachment_ids: [], created_at: '2026-08-04T01:00:00Z' },
  { id: 'progress-note', thread_id: 'thread', sequence: 3, kind: 'note', actor_id: 'b', recipient_id: null, return_to_id: null, message: '当前进展', base_revision: 2, idempotency_key: 'progress-note-key', attachment_ids: [], delivery: { schema_version: 1, artifact_role: 'evidence', subtype: 'progress', expected_next: '', open_questions: '', git_references: [] }, created_at: '2026-08-04T02:00:00Z' },
);
const noteTask = projectSnapshot(noteSnapshot, dict.zh.uiTeamRelay)[0];
assert.equal(noteTask.events.find(event => event.id === 'legacy-note').kindLabel, '补充说明');
assert.match(noteTask.events.find(event => event.id === 'legacy-note').title, /补充说明/);
assert.equal(noteTask.events.find(event => event.id === 'progress-note').kindLabel, '进展更新');
assert.match(noteTask.events.find(event => event.id === 'progress-note').title, /进展更新/);
const acceptanceNoteSnapshot = JSON.parse(JSON.stringify(noteSnapshot));
acceptanceNoteSnapshot.bootstrap.threads[0].thread.phase = 'awaiting_acceptance';
acceptanceNoteSnapshot.bootstrap.threads[0].thread.current_actor_id = 'me';
acceptanceNoteSnapshot.bootstrap.threads[0].actions.push({ id: 'acceptance-note', thread_id: 'thread', sequence: 4, kind: 'note', actor_id: 'me', recipient_id: null, return_to_id: null, message: '验收备注', base_revision: 3, idempotency_key: 'acceptance-note-key', attachment_ids: [], delivery: { schema_version: 1, artifact_role: 'evidence', expected_next: '', open_questions: '', git_references: [] }, created_at: '2026-08-04T03:00:00Z' });
const acceptanceNote = projectSnapshot(acceptanceNoteSnapshot, dict.zh.uiTeamRelay)[0].events.find(event => event.id === 'acceptance-note');
assert.equal(acceptanceNote.kindLabel, '补充说明');

const materialTask = JSON.parse(JSON.stringify(tasks[0]));
materialTask.events.push({
  id: 'git-action', sequence: 2, kind: 'handoff', actor: 'B', time: '2026-08-04T01:00:00Z', files: [],
  delivery: { schema_version: 1, artifact_role: 'intermediate', expected_next: '', open_questions: '', git_references: [{ repository_url: 'https://github.com/Pinvou/pinvou-agent', commit_sha: 'abc', branch: 'feat/relay', paths: [] }] },
});
const materialPackages = projectMaterialPackages(materialTask, {
  materials: [{ attachment_id: 'file', local_path: '/private/relay/方案.md' }],
  agent_session: { material_ids: ['file'], last_synced_sequence: 1 },
});
assert.deepEqual(materialPackages.map(item => item.id), ['action', 'git-action']);
assert.equal(materialPackages[1].agentContextUpdated, true);
assert.equal(materialPackages[0].files[0].prepared.local_path, '/private/relay/方案.md');
assert.equal(materialPackages[0].files[0].introduced, true);

const closedSnapshot = JSON.parse(JSON.stringify(snapshot));
closedSnapshot.bootstrap.threads[0].thread.phase = 'closed';
closedSnapshot.bootstrap.threads[0].thread.current_actor_id = null;
closedSnapshot.bootstrap.threads[0].thread.return_to_id = null;
closedSnapshot.bootstrap.threads[0].actions.push({
  id: 'delivery', thread_id: 'thread', sequence: 2, kind: 'final_deliver', actor_id: 'b', recipient_id: 'me', return_to_id: null, message: '最终成果', base_revision: 1, idempotency_key: 'delivery-key', attachment_ids: [], delivery: { schema_version: 1, artifact_role: 'final', expected_next: '请验收', open_questions: '', git_references: [] }, created_at: '2026-08-04T01:00:00Z',
});
closedSnapshot.bootstrap.threads[0].actions.push({
  id: 'accept', thread_id: 'thread', sequence: 3, kind: 'accept', actor_id: 'me', recipient_id: null, return_to_id: null, message: '接受', base_revision: 2, idempotency_key: 'accept-key', attachment_ids: [], created_at: '2026-08-04T02:00:00Z',
});
const closedTask = projectSnapshot(closedSnapshot, dict.zh.uiTeamRelay)[0];
assert.equal(closedTask.acceptedPackage.id, 'delivery');
assert.equal(closedTask.acceptedPackage.sequence, 2);
assert.equal(matchesRelayView(closedTask, 'all', 'me'), true);
assert.equal(matchesRelayView(closedTask, 'ended', 'me'), true);
closedSnapshot.bootstrap.threads[0].preference = { pinned: true, hidden: true };
const hiddenTask = projectSnapshot(closedSnapshot, dict.zh.uiTeamRelay)[0];
assert.equal(hiddenTask.category, 'hidden');
assert.equal(hiddenTask.pinned, true);
assert.equal(matchesRelayView(hiddenTask, 'all', 'me'), false);
assert.equal(matchesRelayView(hiddenTask, 'hidden', 'me'), true);

for (const language of ['zh', 'en', 'ja']) {
  const copy = dict[language].uiTeamRelay;
  assert.ok(copy.setup.title);
  assert.ok(copy.setup.email);
  assert.ok(copy.setup.displayName);
  assert.ok(copy.setup.submit);
  assert.ok(copy.setup.defaultDeviceName);
  assert.ok(copy.teams.emptyTitle);
  assert.ok(copy.teams.emptyBody);
  assert.ok(copy.errorMessages.resource_not_found);
  assert.ok(copy.createJoinPassword);
  assert.ok(copy.rotateJoinPassword);
  assert.ok(copy.joinPasswordReady);
  assert.ok(copy.inviteMembers);
  assert.ok(copy.syncNow);
  assert.ok(copy.syncingNow);
  assert.ok(copy.sending);
  assert.ok(copy.joinCode.memberVisibility);
  assert.ok(copy.joinCode.ownerOnly);
  assert.ok(copy.joinCode.disableConfirm);
  assert.ok(copy.categories.attention);
  assert.ok(copy.categories.all);
  assert.ok(copy.statuses.receive);
  assert.ok(copy.sourceFilters.sent);
  assert.ok(copy.sourceFilters.received);
  assert.ok(copy.sourceFilters.collaborating);
  assert.ok(copy.errorMessages.join_password_invalid);
  assert.ok(copy.errorMessages.join_code_storage_unavailable);
  assert.ok(copy.errorMessages.rate_limited);
  assert.ok(copy.actionKinds.handoff);
  assert.ok(copy.actionKinds.note);
  assert.ok(copy.actionKinds.progress);
  assert.ok(copy.actionKinds.cancel);
  assert.equal(typeof copy.live.actionTitle, 'function');
  assert.equal(typeof copy.buildAgentPrompt, 'function');
  assert.ok(copy.materialInspector.title);
  assert.equal(typeof copy.materialInspector.summary, 'function');
  assert.ok(copy.materialInspector.agentUpdate);
  assert.ok(copy.materialInspector.revealPrepared);
  assert.ok(copy.taskInfo);
  assert.ok(copy.views.board);
  assert.ok(copy.views.teams);
  assert.ok(copy.teams.create);
  assert.equal(typeof copy.teams.openWork, 'function');
  assert.ok(copy.teams.createDialog.confirm);
  assert.ok(copy.teams.joinDialog.confirm);
  assert.ok(copy.board.title);
  assert.ok(copy.board.groups.owners);
  assert.ok(copy.board.load.waiting);
  assert.equal(typeof copy.board.memberCount, 'function');
  assert.equal(typeof copy.board.ownedCount, 'function');
  assert.ok(copy.board.inviteMember);
  assert.equal(typeof copy.board.removeConfirm, 'function');
  assert.equal(typeof copy.preparedMaterials, 'function');
  assert.equal(typeof copy.agentSessionTitle, 'function');
  assert.ok(copy.useNewAgentSession);
  assert.ok(copy.useNewAgentSessionHint);
  assert.ok(copy.agentSessionChoice);
  assert.ok(copy.setup.service);
  assert.ok(copy.setup.email);
  assert.ok(copy.profile.title);
  assert.ok(copy.profile.displayName);
  assert.ok(copy.reuseAgentSession);
  assert.equal(typeof copy.introducedMaterialSummary, 'function');
  assert.ok(copy.pinvouCodeWorkMode);
  assert.ok(copy.codeAgent);
  assert.ok(copy.defaultPinvouAgent);
  assert.ok(copy.otherAcpAgents);
  assert.equal(typeof copy.continueMaterialSummary, 'function');
  assert.ok(copy.sourceSession);
  assert.ok(copy.noSourceSession);
  assert.ok(copy.chatSessions);
  assert.ok(copy.codingSessions);
  assert.ok(copy.deliveryGoalPlaceholder);
  assert.ok(copy.uploadAttachments);
  assert.equal(typeof copy.selectedSourceMaterials, 'function');
  assert.equal(typeof copy.chatSessionMaterialHint, 'function');
  assert.equal(typeof copy.attachmentLimit, 'function');
  assert.equal(typeof copy.codingGitReferenceNote, 'function');
  assert.equal(typeof copy.acceptedPackage, 'function');
  assert.doesNotMatch(copy.acceptedPackageSummary(1, 0), /Git/);
  assert.match(copy.acceptedPackageSummary(1, 1), /Git/);
  assert.equal(copy.dialogs.return.artifactRole, 'intermediate');
  assert.equal(copy.dialogs.progress.artifactRole, 'evidence');
  assert.equal(copy.dialogs.progress.acceptanceOptional, true);
  assert.ok(copy.dialogs.progress.acceptancePlaceholder);
  assert.ok(copy.dialogs.assign.routeLabel);
  assert.ok(copy.dialogs.handoff.taskLabel);
  assert.ok(copy.dialogs.deliver.acceptanceLabel);
  assert.ok(copy.dialogs.cancelWork.reasonLabel);
  const agentPrompt = copy.buildAgentPrompt(tasks[0], { materials: [{ original_name: '方案.md', local_path: '/private/relay/方案.md' }] });
  assert.match(agentPrompt, /方案|Proposal|仕事/);
  assert.match(agentPrompt, /\/private\/relay\/方案\.md/);
  assert.doesNotMatch(agentPrompt, /团队接力|Team Relay|チームリレー/);
  assert.doesNotMatch(agentPrompt, /负责人|Final recipient|最終提出先/);
  const gitTask = JSON.parse(JSON.stringify(tasks[0]));
  gitTask.events[0].delivery.expected_next = '页面可正常运行';
  gitTask.events[0].delivery.open_questions = '需要确认兼容范围';
  gitTask.events[0].delivery.git_references = [{ repository_url: 'https://github.com/Pinvou/pinvou-agent', commit_sha: '0123456789abcdef0123456789abcdef01234567', branch: 'feat/relay', paths: ['src/relay.js'] }];
  const incrementalPrompt = copy.buildAgentPrompt(gitTask, { materials: [] }, { incremental: true, events: gitTask.events });
  assert.match(incrementalPrompt, /github\.com\/Pinvou\/pinvou-agent/);
  assert.match(incrementalPrompt, /0123456789abcdef/);
  assert.match(incrementalPrompt, /页面可正常运行/);
  assert.match(incrementalPrompt, /需要确认兼容范围/);
}

const mainSource = readFileSync(new URL('../src/app/main.jsx', import.meta.url), 'utf8');
assert.match(mainSource, /const \[teamRelayEnabled, setTeamRelayEnabled\] = useState\(\(\) => loadTeamRelayAccess\(\)\)/);
assert.match(mainSource, /teamRelayEnabled && platformCapabilities\.teamRelaySupported === true/);
assert.match(mainSource, /handleDisableTeamRelay/);
assert.match(mainSource, /teamRelayAvailable && currentView === 'teamRelay'/);
assert.doesNotMatch(mainSource, /const TEAM_RELAY_ENABLED = true/);
assert.match(mainSource, /currentView === 'teamRelay'/);

const viewSource = readFileSync(new URL('../src/features/team-relay/TeamRelayView.jsx', import.meta.url), 'utf8');
const teamBoardSource = readFileSync(new URL('../src/features/team-relay/TeamBoard.jsx', import.meta.url), 'utf8');
const accessSettingsSource = readFileSync(new URL('../src/features/team-relay/TeamRelayAccessSettings.jsx', import.meta.url), 'utf8');
const accessSource = readFileSync(new URL('../src/features/team-relay/team-relay-access.js', import.meta.url), 'utf8');
assert.match(accessSettingsSource, /type="password"/);
assert.match(accessSettingsSource, /copy\.notice/);
assert.match(accessSettingsSource, /onUnlock\(accessCode\)/);
assert.match(accessSettingsSource, /onDisable\(\)/);
assert.match(accessSettingsSource, /copy\.disable/);
assert.doesNotMatch(accessSource, /setItem\([^,]+,\s*accessCode/);
assert.doesNotMatch(accessSource, /ACCESS_CODE_DIGEST|sha256Hex|digestMatches/);
assert.match(mainSource, /bridge\.teamRelay\.verifyAccess/);
assert.match(viewSource, /TauriBridge/);
assert.doesNotMatch(viewSource, /createPreviewTasks|simulateReturn/);
assert.match(viewSource, /data-testid="team-relay-page"/);
assert.match(viewSource, /listDraftSourceSessions/);
assert.match(viewSource, /readDraftSourceSession/);
assert.match(viewSource, /pickDraftAttachments/);
assert.match(viewSource, /team-relay-session-select/);
assert.match(viewSource, /function RelaySelect/);
assert.match(viewSource, /team-relay-select-menu/);
assert.match(viewSource, /team-relay-session-summary/);
assert.match(viewSource, /team-relay-code-reference/);
assert.match(viewSource, /team-relay-direct-attachments/);
assert.match(viewSource, /team-relay-direct-footer/);
assert.match(viewSource, /canUpdateRelayProgress\(task\.raw, meId\)/);
assert.match(viewSource, /onDialog\('progress'\)/);
assert.match(viewSource, /dialog === 'progress'[^\n]+runAction\('note'[^\n]+subtype: 'progress'/);
assert.match(readFileSync(new URL('../src/features/team-relay/MaterialInspector.jsx', import.meta.url), 'utf8'), /item\.kindLabel/);
assert.match(viewSource, /required=\{!dialogCopy\.acceptanceOptional\}/);
assert.match(viewSource, /team-relay-sync-button/);
assert.match(viewSource, /bridge\.teamRelay\.refresh\(\)/);
assert.match(viewSource, /attachmentCount \? copy\.uploadingAndSending : copy\.sending/);
assert.match(viewSource, /team-relay-compose-grid/);
assert.match(viewSource, /team-relay-compose-context/);
assert.match(viewSource, /team-relay-compose-request/);
assert.match(viewSource, /team-relay-prompt-card/);
assert.match(viewSource, /source_message/);
assert.match(viewSource, /source_expected_next/);
assert.match(viewSource, /mountDraftMaterials/);
assert.match(viewSource, /prepareTaskWorkspace/);
assert.match(viewSource, /setTaskAgentSession/);
assert.match(viewSource, /taskWorkspaceStatus/);
assert.match(viewSource, /taskWorkspace\.agent_session/);
assert.match(viewSource, /onLoadCodeAgents/);
assert.match(viewSource, /agent_session\?\.agent_id \|\| 'pinvou'/);
assert.match(viewSource, /selectedAgentMaterialIds/);
assert.match(viewSource, /selectableAgentFiles/);
assert.match(viewSource, /useNewAgentSession/);
assert.match(viewSource, /use_new_session: useNewAgentSession/);
assert.match(viewSource, /payload\.use_new_session \? null : \(workspace\.agent_session \|\| null\)/);
assert.match(viewSource, /team-relay-agent-session-choice/);
assert.match(viewSource, /last_synced_sequence/);
assert.match(viewSource, /MaterialInspector/);
assert.match(viewSource, /team-relay-workspace/);
assert.match(viewSource, /team-relay-package-/);
assert.match(viewSource, /final_recipient_id !== task\.raw\.initiator_id/);
assert.match(viewSource, /team-relay-task-info/);
assert.match(viewSource, /TeamBoard/);
assert.match(viewSource, /team-relay-view-switch/);
assert.match(viewSource, /const openTeam = teamId => \{[\s\S]*setSelectedTeamId\(teamId\);[\s\S]*setViewMode\('board'\);[\s\S]*bridge\.teamRelay\.switchTeam\(teamId\)/);
assert.match(viewSource, /rotateJoinPassword\(\)/);
assert.match(viewSource, /getJoinPassword\(\)/);
assert.match(viewSource, /joinCodeRequestRef\.current/);
assert.match(viewSource, /event\?\.preventDefault\(\);[\s\S]*event\?\.stopPropagation\(\);[\s\S]*setOpen\(false\);[\s\S]*onChange\(option\.value\)/);
assert.match(viewSource, /team-relay-invite-heading/);
assert.match(viewSource, /Date\.now\(\) - joinCodeFetchedAtRef\.current < 2000/);
assert.match(viewSource, /setJoinPasswordVisibility\(visibility\)/);
assert.match(viewSource, /disableJoinPassword\(\)/);
assert.doesNotMatch(viewSource, /createInvite\(24\)/);
assert.doesNotMatch(viewSource, /name="join_password"/);
assert.match(viewSource, /name="join_code" required autoFocus minLength="8"/);
assert.doesNotMatch(viewSource, /name="hub_url"/);
assert.match(viewSource, /DEFAULT_TEAM_RELAY_SERVICE_URL/);
assert.match(viewSource, /name="email" type="email" required/);
assert.doesNotMatch(viewSource, /name="device_name"/);
assert.match(viewSource, /input\.device_name = localDeviceName/);
assert.match(viewSource, /function AccountLogin/);
assert.match(viewSource, /registerAccount\(input\)/);
assert.match(viewSource, /function NoTeam/);
assert.match(viewSource, /function ProfileDialog/);
assert.match(viewSource, /updateProfile/);
assert.doesNotMatch(viewSource, /onDialog\('materials'\)|type === 'materials'/);
assert.match(viewSource, /codingGitReference/);
assert.match(viewSource, /git_references: gitReference/);
assert.doesNotMatch(viewSource, /organizeDraft|buildTeamRelayDraftPrompt|parseTeamRelayDraft|includeSourceMetadata|previewDraft/);
assert.doesNotMatch(viewSource, /git_repository_url|git_commit_sha|addGitReference/);
assert.match(teamBoardSource, /team-relay-member-card/);
assert.match(teamBoardSource, /aria-pressed=\{joinCodeOpen\}/);
assert.match(teamBoardSource, /team-relay-team-overview-actions[\s\S]*onClick=\{onInvite\}/);
assert.doesNotMatch(teamBoardSource, /<Plus size=\{15\}/);
assert.match(teamBoardSource, /copy\.board\.groups\.owners/);
assert.doesNotMatch(teamBoardSource, /team-relay-kanban|BoardTaskCard/);

const i18nSource = readFileSync(new URL('../src/shared/i18n.js', import.meta.url), 'utf8');
assert.doesNotMatch(i18nSource, /peopleOptions|\bsample:\s*\{/);

assert.match(mainSource, /materializeSession/);
assert.match(mainSource, /existing\.session_id/);
assert.match(mainSource, /create_codex_acp_session/);
assert.match(mainSource, /addAttachmentByPath/);

console.log('Team Relay live client contract tests passed');
