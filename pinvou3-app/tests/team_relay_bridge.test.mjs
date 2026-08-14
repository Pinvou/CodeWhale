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
  dialogOpen: async options => options.directory ? '/projects/pinvou' : ['/private/manual/brief.pdf'],
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

assert.deepEqual(Object.keys(feature).sort(), ['claim', 'createAction', 'createFirstTeam', 'createInvite', 'createTeam', 'createThread', 'disableJoinPassword', 'disconnect', 'downloadAttachment', 'getJoinPassword', 'joinTeam', 'listDraftSourceSessions', 'load', 'markThreadRead', 'mountDraftMaterials', 'openPreparedMaterial', 'pickAgentWorkspace', 'pickDraftAttachments', 'prepareTaskWorkspace', 'readDraftSourceSession', 'refresh', 'registerAccount', 'removeMember', 'revealPreparedMaterial', 'rotateJoinPassword', 'setJoinPasswordVisibility', 'setTaskAgentSession', 'setThreadPreference', 'switchTeam', 'taskWorkspaceStatus', 'updateProfile', 'verifyAccess']);
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

console.log('Team Relay bridge tests passed');
