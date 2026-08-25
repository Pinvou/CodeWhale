import assert from 'node:assert/strict';
import fs from 'node:fs';
import vm from 'node:vm';

const source = fs.readFileSync(new URL('../src/platform/tauri/bridge/team-relay.js', import.meta.url), 'utf8');
const windowObject = {
  __PINVOU_TAURI_BRIDGE_FEATURES__: {},
  crypto: { randomUUID: (() => { let id = 0; return () => `00000000-0000-4000-8000-${String(++id).padStart(12, '0')}`; })() },
  localStorage: {
    values: {},
    getItem(key) { return this.values[key] || null; },
    setItem(key, value) { this.values[key] = String(value); },
  },
  setTimeout,
  clearTimeout,
};
vm.runInContext(source, vm.createContext({ window: windowObject, console, setTimeout, clearTimeout }));

const calls = [];
let createAttempts = 0;
let deferredBootstrap = null;
const eventHandlers = new Map();
const state = { sessions: [{ id: 'chat-source', title: 'Product discussion', updated_at: '2026-08-06T01:00:00Z' }], teamRelay: { status: { configured: false }, snapshot: null } };
const teamSnapshot = (id, displayName = 'Me') => ({ bootstrap: { protocol_version: '0.1', team: { id, name: id }, threads: [], members: [], me: { id: 'me', email: 'me@example.com', display_name: displayName } }, attachments: {}, teams: [{ id, name: id, role: 'owner', member_count: 1, open_work_count: 0 }] });
const feature = windowObject.__PINVOU_TAURI_BRIDGE_FEATURES__['team-relay']({
  state,
  notify() {},
  async refreshHistoryList() {},
  listen(name, handler) { eventHandlers.set(name, handler); return Promise.resolve(() => {}); },
  dialogOpen: async options => options.directory
    ? '/projects/pinvou'
    : options.filters?.[0]?.extensions?.includes('gif')
      ? ['/private/avatar/liu.gif']
      : ['/private/manual/brief.pdf'],
  dialogSave: async () => null,
  async invoke(command, args) {
    calls.push({ command, args });
    if (command === 'team_relay_verify_beta_access') return args.accessCode === 'test-only-access-code';
    if (command === 'team_relay_status') return { configured: true, hub_url: 'https://hub.test', team_id: 'team' };
    if (command === 'team_relay_account') return { principal: { id: 'me', email: 'me@example.com', display_name: 'Me' }, teams: [] };
    if (command === 'team_relay_register_account') return { principal: { id: 'me', email: args.input.email, display_name: args.input.display_name }, teams: args.input.email === 'existing@example.com' ? [{ id: 'existing-team', name: 'Existing team' }] : [] };
    if (command === 'team_relay_bootstrap') {
      if (deferredBootstrap) await deferredBootstrap;
      return teamSnapshot('team');
    }
    if (command === 'team_relay_create_first_team') return teamSnapshot('first-team');
    if (command === 'team_relay_update_profile') return teamSnapshot('team', args.input.display_name);
    if (command === 'team_relay_avatar_data_url') return 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yw=';
    if (command === 'team_relay_upload_avatar') return teamSnapshot('team');
    if (command === 'team_relay_remove_avatar') return teamSnapshot('team');
    if (command === 'team_relay_create_team') return teamSnapshot('created-team');
    if (command === 'team_relay_join_team') return teamSnapshot('joined-team');
    if (command === 'team_relay_switch_team') {
      if (args.teamId === 'legacy-team') {
        const snapshot = teamSnapshot('legacy-team');
        snapshot.bootstrap.threads = [{
          thread: { id: 'legacy-thread', revision: 1 },
          actions: [{ sequence: 1, actor_id: 'other' }],
        }];
        return snapshot;
      }
      return teamSnapshot(args.teamId);
    }
    if (command === 'team_relay_remove_member') return null;
    if (command === 'team_relay_transfer_owner') {
      const snapshot = teamSnapshot('team-two');
      return { committed: true, active_team_id: 'team-two', snapshot, account: { principal: snapshot.bootstrap.me, teams: snapshot.teams }, sync_error: null };
    }
    if (command === 'team_relay_leave_team') {
      const snapshot = teamSnapshot('fallback-team');
      return { committed: true, active_team_id: 'fallback-team', snapshot, account: { principal: snapshot.bootstrap.me, teams: snapshot.teams }, sync_error: null };
    }
    if (command === 'team_relay_dissolve_team') {
      return { committed: true, active_team_id: null, snapshot: null, account: { principal: { id: 'me', email: 'me@example.com', display_name: 'Me' }, teams: [] }, sync_error: null };
    }
    if (command === 'team_relay_get_join_password') return { configured: true, visible: true, recoverable: true, visibility: 'members', join_password: 'K7M4-P9Q2', updated_at: '2026-08-10T00:00:00Z' };
    if (command === 'team_relay_rotate_join_password') return { configured: true, visible: true, recoverable: true, visibility: 'members', join_password: 'R8N5-X4C7', updated_at: '2026-08-11T00:00:00Z' };
    if (command === 'team_relay_set_join_password_visibility') return { configured: true, visible: true, recoverable: true, visibility: args.visibility, join_password: 'R8N5-X4C7', updated_at: '2026-08-11T00:00:00Z' };
    if (command === 'team_relay_disable_join_password') return { configured: false, visible: false, recoverable: false, visibility: 'members', join_password: null, updated_at: null };
    if (command === 'team_relay_mark_thread_read') {
      if (args.threadId === 'legacy-thread') throw new Error('resource_not_found: The endpoint was not found.');
      return { thread_id: args.threadId, last_read_sequence: args.input.read_through_sequence, last_read_at: '2026-08-13T00:00:00Z' };
    }
    if (command === 'team_relay_create_thread') {
      createAttempts += 1;
      if (createAttempts === 1) throw new Error('connection lost');
      return { replayed: true };
    }
    if (command === 'team_relay_task_workspace_status') return { thread_id: args.threadId, root_path: '/private/relay/thread', materials: [], agent_session: null };
    if (command === 'team_relay_prepare_task_workspace') return { thread_id: args.threadId, root_path: '/private/relay/thread', materials: [{ attachment_id: 'attachment-1', original_name: 'brief.pdf', local_path: '/private/relay/thread/materials/P01-brief.pdf', byte_size: 5, sha256: 'abc', prepared_at: '2026-08-07T00:00:00Z' }], agent_session: null };
    if (command === 'team_relay_set_task_agent_session') return { thread_id: args.threadId, root_path: '/private/relay/thread', materials: [], agent_session: { ...args.input, linked_at: '2026-08-07T00:00:00Z' } };
    if (command === 'list_codex_acp_sessions') return [{ id: 'coding-source', title: 'Implement relay UI', updated_at: '2026-08-06T02:00:00Z', agent_name: 'Codex', workspace_available: true, workspace_kind: 'project', workspace_path: '/projects/pinvou' }];
    if (command === 'load_session') return { messages: [
      { role: 'user', content: [{ type: 'text', text: 'Private request.' }] },
      { role: 'assistant', content: [{ type: 'text', text: 'Private answer.' }] },
    ], artifacts: [{ storage_path: '/private/session/交付方案.md', byte_size: 2048 }] };
    if (command === 'list_workspace_files') return [
      '/private/session/交付方案.md',
      '/private/session/workspace/index.html',
      '/private/session/workspace/script.js',
      '/private/session/workspace/styles.css',
    ];
    if (command === 'get_codex_workspace_changes') return {
      git: true,
      branch: 'feat/team-relay',
      repositoryUrl: 'https://github.com/pinvou/pinvou-agent',
      commitSha: '0123456789abcdef0123456789abcdef01234567',
      baselineAvailable: true,
      changes: [{ relativePath: 'src/relay.js', status: 'modified', staged: true, origin: 'session' }],
    };
    if (command === 'team_relay_upload_attachment') return { id: `attachment-${args.uploadId}`, original_name: String(args.path).split('/').pop() };
    return null;
  },
});

assert.deepEqual(Object.keys(feature).sort(), ['avatarSource', 'claim', 'createAction', 'createFirstTeam', 'createInvite', 'createTeam', 'createThread', 'disableJoinPassword', 'disconnect', 'dissolveTeam', 'downloadAttachment', 'getJoinPassword', 'joinTeam', 'leaveTeam', 'listDraftSourceSessions', 'load', 'markThreadRead', 'mountDraftMaterials', 'openPreparedMaterial', 'pickAgentWorkspace', 'pickDraftAttachments', 'prepareTaskWorkspace', 'readDraftSourceSession', 'refresh', 'registerAccount', 'removeAvatar', 'removeMember', 'revealPreparedMaterial', 'rotateJoinPassword', 'setJoinPasswordVisibility', 'setTaskAgentSession', 'setThreadPreference', 'switchTeam', 'taskWorkspaceStatus', 'transferOwner', 'updateProfile', 'uploadAvatar', 'verifyAccess']);
assert.equal(await feature.verifyAccess('test-only-access-code'), true);
assert.equal(await feature.verifyAccess('incorrect'), false);
await feature.load();
assert.equal(state.teamRelay.snapshot.bootstrap.protocol_version, '0.1');
const bootstrapCallsBeforeJoinCodeRead = calls.filter(call => call.command === 'team_relay_bootstrap').length;
const joinCodeReadsBefore = calls.filter(call => call.command === 'team_relay_get_join_password').length;
const [currentPassword, repeatedCurrentPassword] = await Promise.all([feature.getJoinPassword(), feature.getJoinPassword()]);
assert.equal(currentPassword.join_password, 'K7M4-P9Q2');
assert.equal(repeatedCurrentPassword.join_password, 'K7M4-P9Q2');
assert.equal(calls.filter(call => call.command === 'team_relay_get_join_password').length, joinCodeReadsBefore + 1, 'concurrent join code reads must be coalesced');
assert.equal(calls.filter(call => call.command === 'team_relay_bootstrap').length, bootstrapCallsBeforeJoinCodeRead, 'reading the join code must not trigger a full bootstrap');
await feature.registerAccount({ hub_url: 'https://hub.test', email: 'new@example.com', display_name: 'New account' });
assert.equal(state.teamRelay.account.principal.email, 'new@example.com');
assert.equal(state.teamRelay.snapshot, null);
await feature.registerAccount({ hub_url: 'https://hub.test', email: 'existing@example.com', display_name: 'Existing account' });
assert.equal(state.teamRelay.snapshot.bootstrap.team.id, 'existing-team');
assert.ok(calls.some(call => call.command === 'team_relay_switch_team' && call.args.teamId === 'existing-team'));
await feature.createTeam('Release group');
assert.equal(state.teamRelay.snapshot.bootstrap.team.id, 'created-team');
await feature.joinTeam('x'.repeat(32));
assert.equal(state.teamRelay.snapshot.bootstrap.team.id, 'joined-team');
await feature.switchTeam('team-two');
assert.equal(state.teamRelay.snapshot.bootstrap.team.id, 'team-two');
assert.equal(state.teamRelay.status.team_id, 'team-two');
await feature.switchTeam('legacy-team');
assert.equal(state.teamRelay.snapshot.bootstrap.threads[0].unread_count, 1, 'an old Hub snapshot must show its latest incoming action as unread');
await feature.markThreadRead('legacy-thread', 1);
assert.equal(state.teamRelay.snapshot.bootstrap.threads[0].unread_count, 0, 'old Hub fallback read state must clear the badge locally');
assert.equal(state.teamRelay.error, null, 'an unsupported old Hub read endpoint must not surface as an application error');
await feature.switchTeam('team-two');
await feature.updateProfile({ display_name: 'New name' });
assert.equal(state.teamRelay.snapshot.bootstrap.me.display_name, 'New name');
const avatarMetadata = { sha256: 'a'.repeat(64), content_type: 'image/gif', byte_size: 32 };
const [avatarSource, cachedAvatarSource] = await Promise.all([
  feature.avatarSource('member-one', avatarMetadata, 48),
  feature.avatarSource('member-one', avatarMetadata, 48),
]);
assert.equal(avatarSource, 'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yw=');
assert.equal(cachedAvatarSource, avatarSource);
assert.equal(calls.filter(call => call.command === 'team_relay_avatar_data_url').length, 1, 'concurrent avatar reads must share one authenticated request');
assert.equal(calls.find(call => call.command === 'team_relay_avatar_data_url').args.displaySize, 48);
await feature.avatarSource('member-one', avatarMetadata, 48, true);
const reducedMotionCall = calls.find(call => call.command === 'team_relay_avatar_data_url' && call.args.reducedMotion === true);
assert.equal(reducedMotionCall.args.displaySize, 48);
assert.equal(calls.filter(call => call.command === 'team_relay_avatar_data_url').length, 2, 'static and animated variants must have independent bounded cache entries');
await feature.avatarSource('member-one', avatarMetadata, 48);
assert.equal(calls.filter(call => call.command === 'team_relay_avatar_data_url').length, 2, 'returning to normal motion must reuse the animated cache entry');
await feature.avatarSource('member-one', avatarMetadata, 96);
assert.equal(calls.filter(call => call.command === 'team_relay_avatar_data_url').length, 3, '48px and 96px variants must have independent bounded cache entries');
await feature.avatarSource('member-original', avatarMetadata, null);
const originalAvatarCall = calls.find(call => call.command === 'team_relay_avatar_data_url' && call.args.principalId === 'member-original');
assert.equal(Object.hasOwn(originalAvatarCall.args, 'displaySize'), false, 'the profile preview must be able to request the processed original');
await feature.avatarSource('shared-member', avatarMetadata, 48, false, 'team-one');
await feature.avatarSource('shared-member', avatarMetadata, 48, false, 'team-two');
const scopedAvatarCalls = calls.filter(call => call.command === 'team_relay_avatar_data_url' && call.args.principalId === 'shared-member');
assert.deepEqual(
  scopedAvatarCalls.map(call => call.args.teamId),
  ['team-one', 'team-two'],
  'each avatar read must use the team that authorized the rendered member',
);
assert.equal(scopedAvatarCalls.length, 2, 'avatar cache entries must not cross team authorization scopes');
await feature.uploadAvatar();
assert.ok(calls.some(call => call.command === 'team_relay_upload_avatar' && call.args.path === '/private/avatar/liu.gif'));
await feature.removeAvatar();
assert.ok(calls.some(call => call.command === 'team_relay_remove_avatar'));
await feature.removeMember('member-two');
assert.ok(calls.some(call => call.command === 'team_relay_remove_member' && call.args.principalId === 'member-two'));
assert.ok(calls.some(call => call.command === 'team_relay_get_join_password'));
const rotatedPassword = await feature.rotateJoinPassword();
assert.equal(rotatedPassword.join_password, 'R8N5-X4C7');
assert.ok(calls.some(call => call.command === 'team_relay_rotate_join_password'));
const hiddenPassword = await feature.setJoinPasswordVisibility('owners');
assert.equal(hiddenPassword.visibility, 'owners');
assert.ok(calls.some(call => call.command === 'team_relay_set_join_password_visibility' && call.args.visibility === 'owners'));
const disabledPassword = await feature.disableJoinPassword();
assert.equal(disabledPassword.configured, false);
assert.ok(calls.some(call => call.command === 'team_relay_disable_join_password'));

const request = { team_id: 'team-two', title: 'Proposal', assignee_id: 'b', message: 'Please draft', attachment_ids: [] };
await assert.rejects(feature.createThread(request), /connection lost/);
await feature.createThread(request);
const writes = calls.filter(call => call.command === 'team_relay_create_thread');
assert.equal(writes.length, 2);
assert.equal(writes[0].args.input.idempotency_key, writes[1].args.input.idempotency_key, 'manual retry must reuse the idempotency key');
await feature.setThreadPreference('thread', { pinned: true, hidden: false });
assert.ok(calls.some(call => call.command === 'team_relay_set_thread_preference' && call.args.threadId === 'thread'));
state.teamRelay.snapshot = {
  bootstrap: {
    team: { id: 'team-two', name: 'Team two' },
    me: { id: 'me', display_name: 'Me' },
    members: [],
    threads: [{
      thread: { id: 'thread', revision: 3 },
      actions: [
        { sequence: 1, actor_id: 'other' },
        { sequence: 2, actor_id: 'other' },
        { sequence: 3, actor_id: 'me' },
      ],
      read_state: { thread_id: 'thread', last_read_sequence: 1, last_read_at: null },
      unread_count: 1,
    }],
  },
  team_bootstraps: [],
  attachments: {},
  teams: [],
};
await feature.markThreadRead('thread', 3);
assert.equal(state.teamRelay.snapshot.bootstrap.threads[0].unread_count, 0);
assert.equal(state.teamRelay.snapshot.bootstrap.threads[0].read_state.last_read_sequence, 3);
assert.ok(calls.some(call => call.command === 'team_relay_mark_thread_read' && call.args.input.read_through_sequence === 3));
const emptyWorkspace = await feature.taskWorkspaceStatus('thread', 'team-two');
assert.equal(emptyWorkspace.thread_id, 'thread');
assert.ok(calls.some(call => call.command === 'team_relay_task_workspace_status' && call.args.teamId === 'team-two'));
const preparedWorkspace = await feature.prepareTaskWorkspace('thread');
assert.equal(preparedWorkspace.materials[0].local_path, '/private/relay/thread/materials/P01-brief.pdf');
assert.equal(state.teamRelay.taskWorkspaces.thread.materials.length, 1);
const linkedWorkspace = await feature.setTaskAgentSession('thread', { kind: 'coding', session_id: 'coding-source', agent_id: 'claude', workspace_path: '/projects/pinvou', material_ids: ['attachment-1'], last_synced_sequence: 3 }, 'team-two');
assert.equal(linkedWorkspace.agent_session.session_id, 'coding-source');
assert.equal(linkedWorkspace.agent_session.agent_id, 'claude');
assert.deepEqual(linkedWorkspace.agent_session.material_ids, ['attachment-1']);
assert.equal(linkedWorkspace.agent_session.last_synced_sequence, 3);
assert.equal(await feature.pickAgentWorkspace(), '/projects/pinvou');
await feature.openPreparedMaterial('/private/relay/thread/materials/P01-brief.pdf');
assert.ok(calls.some(call => call.command === 'open_in_system' && call.args.path === '/private/relay/thread/materials/P01-brief.pdf'));
await feature.openPreparedMaterial('/private/relay/thread/materials/P02-report.html');
assert.ok(calls.some(call => call.command === 'open_artifact_window' && call.args.path === '/private/relay/thread/materials/P02-report.html' && call.args.sessionId === null));
await feature.revealPreparedMaterial('thread', 'attachment-1', 'team-two');
assert.ok(calls.some(call => call.command === 'team_relay_reveal_prepared_material' && call.args.threadId === 'thread' && call.args.attachmentId === 'attachment-1'));
assert.ok(calls.some(call => call.command === 'team_relay_reveal_prepared_material' && call.args.teamId === 'team-two'));

const eventHandler = eventHandlers.get('team_relay:changed');
assert.equal(typeof eventHandler, 'function');
eventHandler();
await new Promise(resolve => setTimeout(resolve, 180));
assert.ok(calls.filter(call => call.command === 'team_relay_bootstrap').length >= 2, 'SSE hint must trigger an authoritative refresh');

const progressHandler = eventHandlers.get('team_relay:upload-progress');
assert.equal(typeof progressHandler, 'function');
progressHandler({ payload: { upload_id: 'upload', sent_bytes: 5, total_bytes: 10 } });
assert.deepEqual(state.teamRelay.uploadProgress, { upload_id: 'upload', sent_bytes: 5, total_bytes: 10 });

const sources = await feature.listDraftSourceSessions();
assert.deepEqual(sources.map(session => `${session.kind}:${session.id}`), ['coding:coding-source', 'chat:chat-source']);
const chatContext = await feature.readDraftSourceSession(sources.find(session => session.id === 'chat-source'));
assert.equal(Object.hasOwn(chatContext, 'messages'), false, 'private conversation text must not enter a relay draft');
assert.equal(chatContext.artifacts.length, 4);
assert.equal(chatContext.materials.length, 4);
assert.equal(chatContext.materials[0].name, '交付方案.md');
assert.equal(chatContext.materials[1].name, 'index.html');
assert.equal(chatContext.materials[1].source, 'session_workspace');

const codingContext = await feature.readDraftSourceSession(sources.find(session => session.id === 'coding-source'));
assert.equal(Object.hasOwn(codingContext, 'messages'), false, 'Coding conversation text must remain private');
assert.deepEqual(JSON.parse(JSON.stringify(codingContext.workspace)), {
  git: true,
  branch: 'feat/team-relay',
  repositoryUrl: 'https://github.com/pinvou/pinvou-agent',
  commitSha: '0123456789abcdef0123456789abcdef01234567',
  baselineAvailable: true,
  changes: [{ relativePath: 'src/relay.js', status: 'modified', staged: true, origin: 'session' }],
});
assert.equal(codingContext.materials.length, 1);
assert.equal(codingContext.materials[0].origin, 'session');
assert.equal(codingContext.materials[0].localPath, '/projects/pinvou/src/relay.js');
assert.ok(calls.some(call => call.command === 'load_session' && call.args.setActive === false));
assert.ok(calls.some(call => call.command === 'get_codex_workspace_changes' && call.args.sessionId === 'coding-source'));

const mounted = await feature.mountDraftMaterials(codingContext.materials, 'team-two');
assert.equal(mounted[0].original_name, 'relay.js');
assert.ok(calls.some(call => call.command === 'team_relay_upload_attachment' && call.args.teamId === 'team-two'));
const mountedChatFile = await feature.mountDraftMaterials([chatContext.materials[1]]);
assert.equal(mountedChatFile[0].original_name, 'index.html');
const uploadCountBeforePick = calls.filter(call => call.command === 'team_relay_upload_attachment').length;
const picked = await feature.pickDraftAttachments();
assert.equal(picked.length, 1);
assert.equal(picked[0].name, 'brief.pdf');
assert.equal(picked[0].source, 'manual');
assert.equal(calls.filter(call => call.command === 'team_relay_upload_attachment').length, uploadCountBeforePick, 'picking a file must not upload before confirmation');
await feature.mountDraftMaterials(picked);
assert.ok(calls.some(call => call.command === 'team_relay_upload_attachment' && call.args.path === '/private/manual/brief.pdf'));

await feature.transferOwner('member-two');
assert.ok(calls.some(call => call.command === 'team_relay_transfer_owner' && call.args.newOwnerId === 'member-two'));
assert.equal(state.teamRelay.snapshot.bootstrap.team.id, 'team-two');
assert.equal(state.teamRelay.syncPending, false);
await feature.leaveTeam();
assert.ok(calls.some(call => call.command === 'team_relay_leave_team'));
assert.equal(state.teamRelay.snapshot.bootstrap.team.id, 'fallback-team');
await feature.dissolveTeam();
assert.ok(calls.some(call => call.command === 'team_relay_dissolve_team'));
assert.equal(state.teamRelay.snapshot, null);
assert.equal(state.teamRelay.status.team_id, null);
assert.deepEqual(state.teamRelay.account.teams, []);

await feature.disconnect();
assert.ok(calls.some(call => call.command === 'team_relay_disconnect'));
assert.deepEqual(JSON.parse(JSON.stringify(state.teamRelay.status)), { configured: false, hub_url: null, team_id: null });
assert.equal(state.teamRelay.snapshot, null);
assert.equal(state.teamRelay.account, null);
assert.deepEqual(Object.keys(state.teamRelay.taskWorkspaces), []);
assert.equal(state.teamRelay.mutating, false);

let releaseBootstrap;
deferredBootstrap = new Promise(resolve => { releaseBootstrap = resolve; });
const staleLoad = feature.load();
await new Promise(resolve => setTimeout(resolve, 0));
await feature.disconnect();
releaseBootstrap();
await staleLoad;
assert.equal(state.teamRelay.status.configured, false, 'an in-flight refresh must not restore signed-out state');
function isolatedRelayFeature(invoke) {
  return windowObject.__PINVOU_TAURI_BRIDGE_FEATURES__['team-relay']({
    state: { sessions: [], teamRelay: { status: { configured: true, team_id: 'team' }, snapshot: teamSnapshot('team') } },
    notify() {},
    listen() { return Promise.resolve(() => {}); },
    async invoke(command, args) { return invoke(command, args); },
  });
}

let committedRefreshAvailable = false;
const committedState = { sessions: [], teamRelay: { status: { configured: true, team_id: 'team' }, snapshot: teamSnapshot('team'), account: { principal: { id: 'me' }, teams: [] }, error: null, syncPending: false } };
const committedFeature = windowObject.__PINVOU_TAURI_BRIDGE_FEATURES__['team-relay']({
  state: committedState,
  notify() {},
  listen() { return Promise.resolve(() => {}); },
  async invoke(command) {
    if (command === 'team_relay_transfer_owner') return { committed: true, active_team_id: 'team', snapshot: null, account: null, sync_error: 'hub refresh failed' };
    if (command === 'team_relay_leave_team') return { committed: true, active_team_id: null, snapshot: null, account: null, sync_error: 'hub refresh failed' };
    if (command === 'team_relay_status') {
      if (!committedRefreshAvailable) throw new Error('connection lost');
      return { configured: true, hub_url: 'https://hub.test', team_id: null };
    }
    if (command === 'team_relay_account') return { principal: { id: 'me' }, teams: [] };
    return null;
  },
});
const committedTransfer = await committedFeature.transferOwner('member-two');
assert.equal(committedTransfer.committed, true, 'a committed mutation must resolve even when refresh fails');
assert.equal(committedState.teamRelay.status.team_id, 'team');
assert.equal(committedState.teamRelay.snapshot.bootstrap.team.id, 'team', 'transfer keeps the cached team until background refresh succeeds');
assert.equal(committedState.teamRelay.error, null, 'a post-commit refresh failure must not be reported as a mutation failure');
assert.equal(committedState.teamRelay.syncPending, true, 'a post-commit refresh failure must remain visible');
const committedLeave = await committedFeature.leaveTeam();
assert.equal(committedLeave.committed, true);
assert.equal(committedState.teamRelay.status.team_id, null);
assert.equal(committedState.teamRelay.snapshot, null, 'leave clears the invalid current team before background recovery');
assert.equal(committedState.teamRelay.error, null);
assert.equal(committedState.teamRelay.syncPending, true);
committedRefreshAvailable = true;
await committedFeature.refresh();
assert.equal(committedState.teamRelay.syncPending, false, 'authoritative refresh clears the visible sync warning');

const recoveredState = { sessions: [], teamRelay: { status: { configured: true, team_id: 'removed-team' }, snapshot: null } };
const recoveredFeature = windowObject.__PINVOU_TAURI_BRIDGE_FEATURES__['team-relay']({
  state: recoveredState,
  notify() {},
  listen() { return Promise.resolve(() => {}); },
  async invoke(command) {
    if (command === 'team_relay_status') return { configured: true, hub_url: 'https://hub.test', team_id: 'removed-team' };
    if (command === 'team_relay_bootstrap') return teamSnapshot('newest-active-team');
    return null;
  },
});
await recoveredFeature.load();
assert.equal(recoveredState.teamRelay.status.team_id, 'newest-active-team', 'bootstrap recovery must replace a stale saved team id');

const clearedState = { sessions: [], teamRelay: { status: { configured: true, team_id: 'dissolved-team' }, snapshot: teamSnapshot('dissolved-team') } };
const clearedFeature = windowObject.__PINVOU_TAURI_BRIDGE_FEATURES__['team-relay']({
  state: clearedState,
  notify() {},
  listen() { return Promise.resolve(() => {}); },
  async invoke(command) {
    if (command === 'team_relay_status') return { configured: true, hub_url: 'https://hub.test', team_id: 'dissolved-team' };
    if (command === 'team_relay_bootstrap') throw new Error('team_relay_team_required: create or join a team first');
    if (command === 'team_relay_account') return { principal: { id: 'me', display_name: 'Me' }, teams: [] };
    return null;
  },
});
await clearedFeature.load();
assert.equal(clearedState.teamRelay.status.team_id, null, 'no valid membership must clear the stale team id');
assert.equal(clearedState.teamRelay.snapshot, null, 'no valid membership must clear the stale snapshot');

const versionCalls = [];
const versionedFeature = isolatedRelayFeature(async (command, args) => {
  versionCalls.push({ command, args });
  return command === 'team_relay_avatar_data_url'
    ? `data:image/gif;base64,${args.expectedSha256.slice(0, 8)}`
    : null;
});
for (let version = 0; version < 50; version += 1) {
  await versionedFeature.avatarSource('changing-member', {
    sha256: version.toString(16).padStart(64, '0'),
  }, 48);
}
const latestDigest = (49).toString(16).padStart(64, '0');
await versionedFeature.avatarSource('changing-member', { sha256: latestDigest }, 48);
assert.equal(versionCalls.filter(call => call.command === 'team_relay_avatar_data_url').length, 50, 'only the current digest should remain cached after 50 changes');
await versionedFeature.avatarSource('changing-member', { sha256: '0'.repeat(64) }, 48);
assert.equal(versionCalls.filter(call => call.command === 'team_relay_avatar_data_url').length, 51, 'an obsolete digest must have been evicted');

const lruCalls = [];
const largeAvatarPayload = 'A'.repeat(9 * 1024 * 1024);
const byteLimitedFeature = isolatedRelayFeature(async (command, args) => {
  lruCalls.push({ command, args });
  return command === 'team_relay_avatar_data_url'
    ? `data:image/gif;base64,${args.principalId}:${largeAvatarPayload}`
    : null;
});
const digestA = 'a'.repeat(64);
const digestB = 'b'.repeat(64);
await byteLimitedFeature.avatarSource('large-a', { sha256: digestA }, 96);
await byteLimitedFeature.avatarSource('large-b', { sha256: digestB }, 96);
await byteLimitedFeature.avatarSource('large-a', { sha256: digestA }, 96);
assert.equal(lruCalls.filter(call => call.command === 'team_relay_avatar_data_url' && call.args.principalId === 'large-a').length, 2, 'the 32 MiB byte budget must evict the least recently used source');

const entryCalls = [];
const entryLimitedFeature = isolatedRelayFeature(async (command, args) => {
  entryCalls.push({ command, args });
  return command === 'team_relay_avatar_data_url' ? `data:image/gif;base64,${args.principalId}` : null;
});
for (let member = 0; member < 256; member += 1) {
  await entryLimitedFeature.avatarSource(`entry-${member}`, { sha256: member.toString(16).padStart(64, '0') }, 48);
}
await entryLimitedFeature.avatarSource('entry-0', { sha256: '0'.repeat(64) }, 48);
await entryLimitedFeature.avatarSource('entry-256', { sha256: (256).toString(16).padStart(64, '0') }, 48);
await entryLimitedFeature.avatarSource('entry-1', { sha256: '1'.padStart(64, '0') }, 48);
await entryLimitedFeature.avatarSource('entry-0', { sha256: '0'.repeat(64) }, 48);
assert.equal(entryCalls.filter(call => call.command === 'team_relay_avatar_data_url' && call.args.principalId === 'entry-1').length, 2, 'the entry budget must evict the true LRU entry');
assert.equal(entryCalls.filter(call => call.command === 'team_relay_avatar_data_url' && call.args.principalId === 'entry-0').length, 1, 'a recently read entry must stay cached');

const raceCalls = [];
const raceResolvers = new Map();
const raceFeature = isolatedRelayFeature((command, args) => {
  raceCalls.push({ command, args });
  if (command !== 'team_relay_avatar_data_url') return null;
  const priorCalls = raceCalls.filter(call => call.command === command && call.args.expectedSha256 === args.expectedSha256).length;
  if (priorCalls > 1) return `data:image/gif;base64,retry-${args.expectedSha256.slice(0, 1)}`;
  return new Promise(resolve => raceResolvers.set(args.expectedSha256, resolve));
});
const oldDigest = 'c'.repeat(64);
const newDigest = 'd'.repeat(64);
const oldRequest = raceFeature.avatarSource('racing-member', { sha256: oldDigest }, 48);
const newRequest = raceFeature.avatarSource('racing-member', { sha256: newDigest }, 48);
raceResolvers.get(newDigest)('data:image/gif;base64,new');
await newRequest;
raceResolvers.get(oldDigest)('data:image/gif;base64,old');
await oldRequest;
await raceFeature.avatarSource('racing-member', { sha256: newDigest }, 48);
assert.equal(raceCalls.filter(call => call.command === 'team_relay_avatar_data_url' && call.args.expectedSha256 === newDigest).length, 1, 'the current version must remain cached');
await raceFeature.avatarSource('racing-member', { sha256: oldDigest }, 48);
assert.equal(raceCalls.filter(call => call.command === 'team_relay_avatar_data_url' && call.args.expectedSha256 === oldDigest).length, 2, 'a late obsolete response must never re-enter the cache');

const lifecycleCalls = [];
const lifecycleFeature = isolatedRelayFeature(async (command, args) => {
  lifecycleCalls.push({ command, args });
  if (command === 'team_relay_avatar_data_url') return 'data:image/gif;base64,lifecycle';
  if (command === 'team_relay_switch_team') return teamSnapshot(args.teamId);
  return null;
});
const lifecycleAvatar = { sha256: 'e'.repeat(64) };
await lifecycleFeature.avatarSource('lifecycle-member', lifecycleAvatar, 48);
await lifecycleFeature.switchTeam('other-team');
await lifecycleFeature.avatarSource('lifecycle-member', lifecycleAvatar, 48);
await lifecycleFeature.disconnect();
await lifecycleFeature.avatarSource('lifecycle-member', lifecycleAvatar, 48);
assert.equal(lifecycleCalls.filter(call => call.command === 'team_relay_avatar_data_url').length, 3, 'team switches and disconnects must clear every cached source');

const transitionCalls = [];
let finishSwitch;
const transitionFeature = isolatedRelayFeature((command, args) => {
  transitionCalls.push({ command, args });
  if (command === 'team_relay_avatar_data_url') return 'data:image/gif;base64,transition';
  if (command === 'team_relay_switch_team') {
    return new Promise(resolve => { finishSwitch = () => resolve(teamSnapshot(args.teamId)); });
  }
  return null;
});
await transitionFeature.avatarSource('transition-member', lifecycleAvatar, 48);
const pendingSwitch = transitionFeature.switchTeam('transition-team');
await transitionFeature.avatarSource('transition-member', lifecycleAvatar, 48);
finishSwitch();
await pendingSwitch;
await transitionFeature.avatarSource('transition-member', lifecycleAvatar, 48);
assert.equal(transitionCalls.filter(call => call.command === 'team_relay_avatar_data_url').length, 3, 'avatar reads completed during a team transition must be cleared when the transition settles');

console.log('Team Relay bridge tests passed');
