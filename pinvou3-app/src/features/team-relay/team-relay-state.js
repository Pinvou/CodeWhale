export const TEAM_RELAY_VIEWS = ['attention', 'waiting', 'all', 'ended', 'hidden'];
export const TEAM_RELAY_SOURCE_FILTERS = ['all', 'sent', 'received', 'collaborating'];
export const TEAM_RELAY_BOARD_COLUMNS = ['active', 'collaborating', 'acceptance', 'ended'];

function defaultDelivery(kind) {
  const artifactRole = kind === 'assign' || kind === 'request_changes'
    ? 'input'
    : kind === 'final_deliver' || kind === 'accept'
      ? 'final'
      : kind === 'note' || kind === 'cancel'
        ? 'evidence'
        : 'intermediate';
  return { schema_version: 1, artifact_role: artifactRole, subtype: '', expected_next: '', open_questions: '', git_references: [] };
}

function projectDelivery(action) {
  const fallback = defaultDelivery(action.kind);
  const delivery = action.delivery || {};
  const artifactRole = ['input', 'intermediate', 'final', 'evidence'].includes(delivery.artifact_role)
    ? delivery.artifact_role
    : fallback.artifact_role;
  return {
    ...fallback,
    ...delivery,
    artifact_role: artifactRole,
    git_references: Array.isArray(delivery.git_references) ? delivery.git_references : [],
  };
}

function actionKindLabel(action, copy) {
  if (action.kind === 'note' && action.delivery?.subtype === 'progress') {
    return copy.actionKinds.progress;
  }
  return copy.actionKinds[action.kind] || action.kind;
}

export function deriveCategory(thread, meId) {
  if (thread.phase === 'closed' || thread.phase === 'cancelled') return 'ended';
  if (thread.phase === 'awaiting_acceptance' && thread.current_actor_id === meId) return 'receive';
  if (thread.phase === 'active' && thread.current_actor_id === meId) return 'pending';
  return 'waiting';
}

export function canUpdateRelayProgress(thread, meId) {
  return thread?.phase === 'active' && thread.current_actor_id === meId;
}

export function matchesRelayView(task, view, meId) {
  if (view === 'hidden') return task.category === 'hidden';
  if (view === 'ended') return task.category === 'ended';
  if (view === 'attention') return task.category === 'pending' || task.category === 'receive';
  if (view === 'waiting') {
    return task.category === 'waiting'
      && (task.raw.initiator_id === meId || task.raw.owner_id === meId);
  }
  return task.category !== 'hidden';
}

export function matchesSourceFilter(task, filter, meId) {
  if (filter === 'sent') return task.raw.initiator_id === meId;
  if (filter === 'received') return task.raw.initiator_id !== meId && task.raw.owner_id === meId;
  if (filter === 'collaborating') return task.raw.initiator_id !== meId && task.raw.owner_id !== meId;
  return true;
}

export function projectSnapshot(snapshot, copy) {
  if (!snapshot?.bootstrap) return [];
  const { bootstrap, attachments = {} } = snapshot;
  const names = new Map(bootstrap.members.map(member => [member.id, member.display_name]));
  names.set(bootstrap.me.id, bootstrap.me.display_name);
  const nameOf = id => id ? (names.get(id) || id.slice(0, 8)) : copy.dash;
  return bootstrap.threads.map(detail => {
    const thread = detail.thread;
    const preference = detail.preference || {};
    const category = preference.hidden ? 'hidden' : deriveCategory(thread, bootstrap.me.id);
    const current = nameOf(thread.current_actor_id);
    const files = Array.from(new Set(detail.actions.flatMap(action => action.attachment_ids)))
      .map(id => attachments[id])
      .filter(Boolean);
    let banner;
    if (thread.phase === 'cancelled') banner = copy.cancelledBanner;
    else if (category === 'ended' || category === 'hidden') banner = copy.endedBanner;
    else if (category === 'receive') banner = copy.live.awaitingAcceptance(nameOf(thread.owner_id));
    else if (thread.current_actor_id === bootstrap.me.id) banner = copy.live.yourTurn;
    else banner = copy.dynamic.waitingBanner(current);
    const events = detail.actions.map(action => ({
      id: action.id,
      sequence: action.sequence,
      kind: action.kind,
      actorId: action.actor_id,
      actor: nameOf(action.actor_id),
      avatar: nameOf(action.actor_id),
      kindLabel: actionKindLabel(action, copy),
      title: copy.live.actionTitle(actionKindLabel(action, copy), nameOf(action.actor_id), nameOf(action.recipient_id)),
      message: action.message,
      time: action.created_at,
      files: action.attachment_ids.map(id => attachments[id]).filter(Boolean),
      delivery: projectDelivery(action),
    }));
    const acceptedPackage = thread.phase === 'closed'
      ? [...events].reverse().find(event => event.kind === 'final_deliver') || null
      : null;
    return {
      id: thread.id,
      category,
      title: thread.title,
      initiator: nameOf(thread.initiator_id),
      owner: nameOf(thread.owner_id),
      current,
      currentActorId: thread.current_actor_id,
      finalRecipient: nameOf(thread.final_recipient_id),
      status: thread.phase === 'cancelled'
        ? copy.cancelledStatus
        : new Set(['ended', 'hidden']).has(category)
          ? copy.endedStatus
          : copy.dynamic.currentAt(current),
      updated: thread.updated_at,
      files,
      banner,
      revision: thread.revision,
      raw: thread,
      events,
      acceptedPackage,
      pinned: Boolean(preference.pinned),
      hidden: Boolean(preference.hidden),
      preference,
    };
  });
}

export function relayViewCounts(tasks, meId) {
  return TEAM_RELAY_VIEWS.reduce((counts, view) => {
    counts[view] = tasks.filter(task => matchesRelayView(task, view, meId)).length;
    return counts;
  }, {});
}

export function deriveBoardColumn(thread) {
  if (thread.phase === 'closed' || thread.phase === 'cancelled') return 'ended';
  if (thread.phase === 'awaiting_acceptance') return 'acceptance';
  if (thread.phase === 'active' && thread.current_actor_id !== thread.owner_id) return 'collaborating';
  return 'active';
}

export function projectTeamBoard(snapshot, tasks) {
  if (!snapshot?.bootstrap) return { columns: {}, members: [], summary: {} };
  const taskById = new Map((tasks || []).map(task => [task.id, task]));
  const boardTasks = snapshot.bootstrap.threads
    .map(detail => {
      const task = taskById.get(detail.thread.id);
      if (!task) return null;
      const latestEvent = task.events[task.events.length - 1] || null;
      return {
        ...task,
        boardColumn: deriveBoardColumn(detail.thread),
        actionCount: detail.actions.length,
        latestEvent,
        cancelled: detail.thread.phase === 'cancelled',
      };
    })
    .filter(Boolean)
    .sort((left, right) => new Date(right.updated) - new Date(left.updated));
  const columns = Object.fromEntries(TEAM_RELAY_BOARD_COLUMNS.map(column => [
    column,
    boardTasks.filter(task => task.boardColumn === column),
  ]));
  const members = snapshot.bootstrap.members
    .map(member => ({
      ...member,
      isMe: member.id === snapshot.bootstrap.me.id,
      activeCount: boardTasks.filter(task => task.raw.phase === 'active' && task.raw.current_actor_id === member.id).length,
      waitingCount: boardTasks.filter(task => task.raw.phase === 'active' && task.raw.owner_id === member.id && task.raw.current_actor_id !== member.id).length,
      acceptanceCount: boardTasks.filter(task => task.raw.phase === 'awaiting_acceptance' && task.raw.current_actor_id === member.id).length,
      ownedCount: boardTasks.filter(task => !['closed', 'cancelled'].includes(task.raw.phase) && task.raw.owner_id === member.id).length,
    }))
    .sort((left, right) => Number(right.isMe) - Number(left.isMe)
      || (right.activeCount + right.waitingCount + right.acceptanceCount) - (left.activeCount + left.waitingCount + left.acceptanceCount)
      || left.display_name.localeCompare(right.display_name));
  return {
    columns,
    members,
    summary: {
      members: members.length,
      inProgress: columns.active.length + columns.collaborating.length,
      acceptance: columns.acceptance.length,
      completed: boardTasks.filter(task => task.raw.phase === 'closed').length,
    },
  };
}

export function projectMaterialPackages(task, taskWorkspace) {
  const preparedById = new Map((taskWorkspace?.materials || []).map(material => [material.attachment_id, material]));
  const agentSession = taskWorkspace?.agent_session || null;
  const introducedIds = new Set(agentSession?.material_ids || []);
  const lastSyncedSequence = Number(agentSession?.last_synced_sequence || 0);
  return [...(task?.events || [])]
    .filter(event => event.files.length || event.delivery.git_references.length)
    .map(event => ({
      ...event,
      agentContextUpdated: Boolean(agentSession) && Number(event.sequence) > lastSyncedSequence,
      files: event.files.map(file => ({
        file,
        prepared: preparedById.get(file.id) || null,
        introduced: introducedIds.has(file.id),
      })),
    }));
}
