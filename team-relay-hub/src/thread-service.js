import {
  authenticate,
  CONFIRMED_TRANSFER_CAPABILITY_DAYS,
  registerDeviceProtocol,
  requireMembership,
} from "./auth.js";
import { bindAttachments } from "./attachment-service.js";
import { newId, requestHash } from "./crypto.js";
import { inTransaction } from "./db.js";
import { normalizeDelivery } from "./delivery-package.js";
import { AppError } from "./errors.js";
import {
  actionFromRow,
  teamFromRow,
  teamMemberFromRow,
  threadFromRow,
  threadPreferenceFromRow,
  threadReadStateFromRow,
} from "./serializers.js";
import { transition } from "./state-machine.js";
import { boolean, exactKeys, integer, text, uuid, uuidArray } from "./validation.js";

async function selectThread(db, threadId, lock = false) {
  const result = await db.query(
    `SELECT id, team_id, title, initiator_id, owner_id, current_actor_id,
            return_to_id, final_recipient_id, pending_transfer_kind,
            pending_sender_id, pending_recipient_id, phase, revision, created_at, updated_at
       FROM work_threads
      WHERE id = $1${lock ? " FOR UPDATE" : ""}`,
    [threadId],
  );
  if (result.rowCount === 0) {
    throw new AppError(404, "resource_not_found", "The work thread was not found.");
  }
  return result.rows[0];
}

async function lockTeamMembershipChanges(db, teamId) {
  const result = await db.query(
    `SELECT id FROM teams WHERE id = $1 AND dissolved_at IS NULL FOR SHARE`,
    [teamId],
  );
  if (!result.rowCount) {
    throw new AppError(404, "resource_not_found", "The team was not found.");
  }
}

async function requireParticipant(db, threadId, principalId) {
  const result = await db.query(
    `SELECT 1 FROM thread_participants
      WHERE thread_id = $1 AND principal_id = $2`,
    [threadId, principalId],
  );
  if (result.rowCount === 0) {
    throw new AppError(403, "thread_access_denied", "The work thread is not visible to this member.");
  }
}

async function supportsConfirmedTransfers(db, principalId) {
  const result = await db.query(
    `SELECT EXISTS (
       SELECT 1
         FROM devices
        WHERE principal_id = $1
          AND revoked_at IS NULL
          AND relay_protocol_version = '0.2'
          AND relay_protocol_seen_at >= now() - ($2 * interval '1 day')
     ) AS supported`,
    [principalId, CONFIRMED_TRANSFER_CAPABILITY_DAYS],
  );
  return result.rows[0].supported;
}

async function registerProtocolAndNotify(pool, auth, protocolVersion, notify) {
  const changedTeamIds = await registerDeviceProtocol(pool, auth, protocolVersion);
  for (const changedTeamId of changedTeamIds) notify(changedTeamId, "membership.changed");
}

async function selectAction(db, actionId) {
  const result = await db.query(
    `SELECT a.*,
            coalesce(array_agg(aa.attachment_id ORDER BY aa.ordinal)
              FILTER (WHERE aa.attachment_id IS NOT NULL), '{}') AS attachment_ids
       FROM relay_actions a
       LEFT JOIN action_attachments aa ON aa.action_id = a.id
      WHERE a.id = $1
      GROUP BY a.id`,
    [actionId],
  );
  if (result.rowCount === 0) throw new Error("inserted action not found");
  return actionFromRow(result.rows[0], result.rows[0].attachment_ids);
}

async function threadDetail(db, threadRow, principalId) {
  const participants = await db.query(
    `SELECT principal_id, material_access_at FROM thread_participants
      WHERE thread_id = $1 ORDER BY first_action_sequence, principal_id`,
    [threadRow.id],
  );
  const actions = await db.query(
    `SELECT a.*,
            coalesce(array_agg(aa.attachment_id ORDER BY aa.ordinal)
              FILTER (WHERE aa.attachment_id IS NOT NULL), '{}') AS attachment_ids
       FROM relay_actions a
       LEFT JOIN action_attachments aa ON aa.action_id = a.id
      WHERE a.thread_id = $1
      GROUP BY a.id
      ORDER BY a.sequence`,
    [threadRow.id],
  );
  const preference = await db.query(
    `SELECT thread_id, pinned_at, hidden_at, updated_at AS preference_updated_at
       FROM thread_preferences
      WHERE thread_id = $1 AND principal_id = $2`,
    [threadRow.id, principalId],
  );
  const readState = await db.query(
    `SELECT tp.thread_id, rs.last_read_sequence, rs.last_read_at,
            count(a.id) FILTER (
              WHERE a.sequence > coalesce(rs.last_read_sequence, 0)
                AND a.sequence >= tp.first_action_sequence
                AND a.actor_id <> $2
            )::integer AS unread_count
       FROM thread_participants tp
       LEFT JOIN thread_read_states rs
         ON rs.thread_id = tp.thread_id AND rs.principal_id = tp.principal_id
       LEFT JOIN relay_actions a ON a.thread_id = tp.thread_id
      WHERE tp.thread_id = $1 AND tp.principal_id = $2
      GROUP BY tp.thread_id, tp.first_action_sequence,
               rs.last_read_sequence, rs.last_read_at`,
    [threadRow.id, principalId],
  );
  const readRow = readState.rows[0] || { id: threadRow.id, unread_count: 0 };
  const materialAccess = participants.rows.find((row) => row.principal_id === principalId)?.material_access_at;
  return {
    thread: threadFromRow(threadRow),
    preference: preference.rowCount
      ? threadPreferenceFromRow(preference.rows[0])
      : threadPreferenceFromRow({ id: threadRow.id }),
    participants: participants.rows.map((row) => row.principal_id),
    actions: actions.rows.map((row) => actionFromRow(row, materialAccess ? row.attachment_ids : [])),
    read_state: threadReadStateFromRow(readRow),
    unread_count: Number(readRow.unread_count || 0),
  };
}

async function findReplay(db, actorId, idempotencyKey, hash) {
  const result = await db.query(
    `SELECT id, thread_id, request_hash
       FROM relay_actions
      WHERE actor_id = $1 AND idempotency_key = $2`,
    [actorId, idempotencyKey],
  );
  if (result.rowCount === 0) return null;
  const existing = result.rows[0];
  if (existing.request_hash !== hash) {
    throw new AppError(
      409,
      "idempotency_key_reused",
      "The idempotency key belongs to a different request.",
    );
  }
  const threadRow = await selectThread(db, existing.thread_id);
  await requireMembership(db, threadRow.team_id, actorId);
  await requireParticipant(db, threadRow.id, actorId);
  return {
    thread: threadFromRow(threadRow),
    action: await selectAction(db, existing.id),
    replayed: true,
  };
}

function createThreadInput(body) {
  exactKeys(body, [
    "team_id",
    "title",
    "assignee_id",
    "message",
    "attachment_ids",
    "idempotency_key",
  ], ["delivery"]);
  const input = {
    teamId: uuid(body.team_id, "team_id"),
    title: text(body.title, "title", { minimum: 1, maximum: 200 }).trim(),
    assigneeId: uuid(body.assignee_id, "assignee_id"),
    message: text(body.message, "message", { maximum: 10_000 }),
    attachmentIds: uuidArray(body.attachment_ids, "attachment_ids"),
    idempotencyKey: uuid(body.idempotency_key, "idempotency_key"),
    delivery: normalizeDelivery(body.delivery, "assign"),
  };
  return input;
}

function actionInput(body) {
  exactKeys(
    body,
    ["kind", "message", "attachment_ids", "expected_revision", "idempotency_key"],
    ["recipient_id", "delivery"],
  );
  const kind = text(body.kind, "kind", { minimum: 1, maximum: 32 });
  if (!new Set([
    "note", "handoff", "accept_transfer", "decline_transfer", "withdraw_transfer",
    "reroute_transfer", "return", "final_deliver", "request_changes", "accept", "cancel",
  ]).has(kind)) {
    throw new AppError(400, "invalid_request", "The action kind is invalid.");
  }
  const input = {
    kind,
    recipientId: body.recipient_id === undefined || body.recipient_id === null
      ? null
      : uuid(body.recipient_id, "recipient_id"),
    message: text(body.message, "message", { maximum: 10_000 }),
    attachmentIds: uuidArray(body.attachment_ids, "attachment_ids"),
    expectedRevision: integer(body.expected_revision, "expected_revision", 1, Number.MAX_SAFE_INTEGER),
    idempotencyKey: uuid(body.idempotency_key, "idempotency_key"),
    delivery: normalizeDelivery(body.delivery, kind),
  };
  if (new Set(["cancel", "decline_transfer", "withdraw_transfer", "reroute_transfer"]).has(kind) && !input.message.trim()) {
    throw new AppError(400, "invalid_request", "A reason is required for this action.");
  }
  return input;
}

export async function createThread(pool, token, body, notify = () => {}, protocolVersion = "0.1") {
  const auth = await authenticate(pool, token);
  await registerProtocolAndNotify(pool, auth, protocolVersion, notify);
  const input = createThreadInput(body);
  if (input.assigneeId === auth.principalId) {
    throw new AppError(400, "invalid_request", "The demo requires assigning work to another member.");
  }
  const hash = requestHash({
    operation: "create_thread",
    team_id: input.teamId,
    title: input.title,
    assignee_id: input.assigneeId,
    message: input.message,
    attachment_ids: input.attachmentIds,
    delivery: input.delivery,
    ...(protocolVersion === "0.2" ? { protocol_version: "0.2" } : {}),
  });

  const perform = () => inTransaction(pool, async (client) => {
    const replay = await findReplay(client, auth.principalId, input.idempotencyKey, hash);
    if (replay) return replay;
    await lockTeamMembershipChanges(client, input.teamId);
    await requireMembership(client, input.teamId, auth.principalId);
    await requireMembership(client, input.teamId, input.assigneeId);
    const confirmedTransfers = protocolVersion === "0.2"
      && await supportsConfirmedTransfers(client, input.assigneeId);
    const transferMode = confirmedTransfers ? "confirmed" : "direct_legacy";
    const threadId = newId();
    const actionId = newId();
    const threadResult = await client.query(
      `INSERT INTO work_threads (
         id, team_id, title, initiator_id, owner_id, current_actor_id,
         return_to_id, final_recipient_id, pending_transfer_kind,
         pending_sender_id, pending_recipient_id, phase, revision
       ) VALUES ($1, $2, $3, $4, $5, $6, $7, $4, $8, $9, $10, $11, 1)
       RETURNING *`,
      [
        threadId,
        input.teamId,
        input.title,
        auth.principalId,
        confirmedTransfers ? auth.principalId : input.assigneeId,
        confirmedTransfers ? auth.principalId : input.assigneeId,
        auth.principalId,
        confirmedTransfers ? "assignment" : null,
        confirmedTransfers ? auth.principalId : null,
        confirmedTransfers ? input.assigneeId : null,
        confirmedTransfers ? "pending_transfer" : "active",
      ],
    );
    await client.query(
      `INSERT INTO thread_participants (thread_id, principal_id, first_action_sequence, material_access_at)
       VALUES ($1, $2, 1, now()), ($1, $3, 1, CASE WHEN $4 THEN NULL ELSE now() END)
       ON CONFLICT (thread_id, principal_id) DO NOTHING`,
      [threadId, auth.principalId, input.assigneeId, confirmedTransfers],
    );
    await client.query(
      `INSERT INTO thread_read_states (principal_id, thread_id, last_read_sequence)
       VALUES ($1, $2, 1)`,
      [auth.principalId, threadId],
    );
    await client.query(
      `INSERT INTO relay_actions (
         id, thread_id, sequence, kind, actor_id, recipient_id, return_to_id,
         message, base_revision, idempotency_key, request_hash, delivery, transfer_mode
       ) VALUES ($1, $2, 1, 'assign', $3, $4, $3, $5, 0, $6, $7, $8, $9)`,
      [
        actionId,
        threadId,
        auth.principalId,
        input.assigneeId,
        input.message,
        input.idempotencyKey,
        hash,
        input.delivery,
        transferMode,
      ],
    );
    await bindAttachments(client, {
      teamId: input.teamId,
      actorId: auth.principalId,
      attachmentIds: input.attachmentIds,
      actionId,
    });
    return {
      thread: threadFromRow(threadResult.rows[0]),
      action: await selectAction(client, actionId),
      transfer_mode: transferMode,
      replayed: false,
    };
  });

  let result;
  try {
    result = await perform();
  } catch (error) {
    if (error?.code !== "23505") throw error;
    result = await findReplay(pool, auth.principalId, input.idempotencyKey, hash);
    if (!result) throw error;
  }
  result.transfer_mode = result.action.transfer_mode;
  if (!result.replayed) {
    notify(input.teamId, "thread.changed", result.thread);
    notify(input.teamId, "thread.attention", {
      id: result.thread.id,
      revision: result.thread.revision,
      principal_id: input.assigneeId,
    });
  }
  return result;
}

export async function createAction(pool, token, rawThreadId, body, notify = () => {}, protocolVersion = "0.1") {
  const threadId = uuid(rawThreadId, "thread_id");
  const auth = await authenticate(pool, token);
  await registerProtocolAndNotify(pool, auth, protocolVersion, notify);
  const input = actionInput(body);
  const hash = requestHash({
    operation: "create_action",
    thread_id: threadId,
    kind: input.kind,
    recipient_id: input.recipientId,
    message: input.message,
    attachment_ids: input.attachmentIds,
    expected_revision: input.expectedRevision,
    delivery: input.delivery,
    ...(protocolVersion === "0.2" ? { protocol_version: "0.2" } : {}),
  });

  let displacedPendingRecipientId = null;

  const perform = () => inTransaction(pool, async (client) => {
    const replay = await findReplay(client, auth.principalId, input.idempotencyKey, hash);
    if (replay) return replay;
    // Every state transition can change whether a Principal is responsible for
    // open work. Serialize it with member exit, owner transfer and dissolution
    // before taking the thread row lock so lifecycle checks see a stable set.
    const initial = await selectThread(client, threadId);
    await lockTeamMembershipChanges(client, initial.team_id);
    const locked = await selectThread(client, threadId, true);
    await requireMembership(client, locked.team_id, auth.principalId);
    await requireParticipant(client, threadId, auth.principalId);
    if (Number(locked.revision) !== input.expectedRevision) {
      throw new AppError(409, "revision_conflict", "The work thread changed before this request.", {
        current_revision: Number(locked.revision),
      });
    }
    if (new Set(["handoff", "reroute_transfer", "accept_transfer"]).has(input.kind)) {
      if (input.recipientId) await requireMembership(client, locked.team_id, input.recipientId);
      if (input.kind === "accept_transfer") {
        await requireMembership(client, locked.team_id, auth.principalId);
      }
    }
    if (new Set(["withdraw_transfer", "reroute_transfer"]).has(input.kind)) {
      displacedPendingRecipientId = locked.pending_recipient_id;
    }
    const recipientSupportsConfirmation = input.recipientId
      ? await supportsConfirmedTransfers(client, input.recipientId)
      : true;
    const confirmedTransfers = protocolVersion === "0.2" && recipientSupportsConfirmation;
    const next = transition(
      locked,
      auth.principalId,
      input.kind,
      input.recipientId,
      confirmedTransfers,
      input.delivery.subtype,
    );
    const nextRevision = Number(locked.revision) + 1;
    const actionId = newId();
    const threadResult = await client.query(
      `UPDATE work_threads
          SET owner_id = $2, current_actor_id = $3, return_to_id = $4, phase = $5,
              pending_transfer_kind = $6, pending_sender_id = $7,
              pending_recipient_id = $8, revision = $9, updated_at = now()
        WHERE id = $1
        RETURNING *`,
      [
        threadId, next.ownerId, next.currentActorId, next.threadReturnToId, next.phase,
        next.pendingTransferKind, next.pendingSenderId, next.pendingRecipientId, nextRevision,
      ],
    );
    if (next.addParticipantId) {
      await client.query(
        `INSERT INTO thread_participants (thread_id, principal_id, first_action_sequence, material_access_at)
         VALUES ($1, $2, $3, NULL)
         ON CONFLICT (thread_id, principal_id) DO NOTHING`,
        [threadId, next.addParticipantId, nextRevision],
      );
    }
    if (next.removeParticipantId) {
      await client.query(
        `DELETE FROM thread_participants
          WHERE thread_id = $1 AND principal_id = $2 AND material_access_at IS NULL`,
        [threadId, next.removeParticipantId],
      );
    }
    if (next.grantMaterialAccessId) {
      await client.query(
        `UPDATE thread_participants SET material_access_at = coalesce(material_access_at, now())
          WHERE thread_id = $1 AND principal_id = $2`,
        [threadId, next.grantMaterialAccessId],
      );
    }
    await client.query(
      `INSERT INTO relay_actions (
         id, thread_id, sequence, kind, actor_id, recipient_id, return_to_id,
         message, base_revision, idempotency_key, request_hash, delivery, transfer_mode
       ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
      [
        actionId,
        threadId,
        nextRevision,
        input.kind,
        auth.principalId,
        next.recipientId,
        next.actionReturnToId,
        input.message,
        input.expectedRevision,
        input.idempotencyKey,
        hash,
        input.delivery,
        next.transferMode,
      ],
    );
    await bindAttachments(client, {
      teamId: locked.team_id,
      actorId: auth.principalId,
      attachmentIds: input.attachmentIds,
      actionId,
    });
    return {
      thread: threadFromRow(threadResult.rows[0]),
      action: await selectAction(client, actionId),
      transfer_mode: next.transferMode,
      replayed: false,
    };
  });

  let result;
  try {
    result = await perform();
  } catch (error) {
    if (error?.code !== "23505") throw error;
    result = await findReplay(pool, auth.principalId, input.idempotencyKey, hash);
    if (!result) throw error;
  }
  result.transfer_mode = result.action.transfer_mode;
  if (!result.replayed) {
    notify(result.thread.team_id, "thread.changed", result.thread);
    const attentionPrincipalIds = new Set([
      result.action.recipient_id,
      result.action.kind === "accept" ? result.thread.owner_id : null,
      displacedPendingRecipientId,
    ]);
    attentionPrincipalIds.delete(null);
    attentionPrincipalIds.delete(undefined);
    attentionPrincipalIds.delete(result.action.actor_id);
    for (const attentionPrincipalId of attentionPrincipalIds) {
      notify(result.thread.team_id, "thread.attention", {
        id: result.thread.id,
        revision: result.thread.revision,
        principal_id: attentionPrincipalId,
      });
    }
  }
  return result;
}

export async function listThreads(pool, token, rawTeamId, view = "open") {
  const teamId = uuid(rawTeamId, "team_id");
  if (!new Set(["open", "closed", "all"]).has(view)) {
    throw new AppError(400, "invalid_request", "view must be open, closed or all.");
  }
  const auth = await authenticate(pool, token);
  await requireMembership(pool, teamId, auth.principalId);
  const phaseClause = view === "open"
    ? "AND w.phase NOT IN ('closed', 'cancelled')"
    : view === "closed"
      ? "AND w.phase IN ('closed', 'cancelled')"
      : "";
  const result = await pool.query(
    `SELECT w.*
       FROM work_threads w
       JOIN thread_participants tp ON tp.thread_id = w.id AND tp.principal_id = $2
      WHERE w.team_id = $1 ${phaseClause}
      ORDER BY w.updated_at DESC, w.id`,
    [teamId, auth.principalId],
  );
  return { threads: result.rows.map(threadFromRow) };
}

export async function getThread(pool, token, rawThreadId) {
  const threadId = uuid(rawThreadId, "thread_id");
  const auth = await authenticate(pool, token);
  const row = await selectThread(pool, threadId);
  await requireMembership(pool, row.team_id, auth.principalId);
  await requireParticipant(pool, threadId, auth.principalId);
  return threadDetail(pool, row, auth.principalId);
}

export async function setThreadPreference(pool, token, rawThreadId, body, notify = () => {}) {
  const threadId = uuid(rawThreadId, "thread_id");
  exactKeys(body, ["pinned", "hidden"]);
  const pinned = boolean(body.pinned, "pinned");
  const hidden = boolean(body.hidden, "hidden");
  const auth = await authenticate(pool, token);
  const result = await inTransaction(pool, async (client) => {
    const initial = await selectThread(client, threadId);
    await lockTeamMembershipChanges(client, initial.team_id);
    const thread = await selectThread(client, threadId, true);
    await requireMembership(client, thread.team_id, auth.principalId);
    await requireParticipant(client, threadId, auth.principalId);
    if (hidden && !new Set(["closed", "cancelled"]).has(thread.phase)) {
      throw new AppError(403, "action_not_allowed", "Only finished work can be hidden.");
    }
    const preference = await client.query(
      `INSERT INTO thread_preferences (principal_id, thread_id, pinned_at, hidden_at, updated_at)
       VALUES ($1, $2, CASE WHEN $3 THEN now() END, CASE WHEN $4 THEN now() END, now())
       ON CONFLICT (principal_id, thread_id) DO UPDATE
         SET pinned_at = CASE
               WHEN $3 AND thread_preferences.pinned_at IS NULL THEN now()
               WHEN $3 THEN thread_preferences.pinned_at
               ELSE NULL
             END,
             hidden_at = CASE
               WHEN $4 AND thread_preferences.hidden_at IS NULL THEN now()
               WHEN $4 THEN thread_preferences.hidden_at
               ELSE NULL
             END,
             updated_at = now()
       RETURNING thread_id, pinned_at, hidden_at, updated_at AS preference_updated_at`,
      [auth.principalId, threadId, pinned, hidden],
    );
    return { teamId: thread.team_id, preference: threadPreferenceFromRow(preference.rows[0]) };
  });
  notify(result.teamId, "thread.preference.changed", { id: threadId, principal_id: auth.principalId });
  return result.preference;
}

export async function markThreadRead(pool, token, rawThreadId, body, notify = () => {}) {
  const threadId = uuid(rawThreadId, "thread_id");
  exactKeys(body, ["read_through_sequence"]);
  const readThroughSequence = integer(
    body.read_through_sequence,
    "read_through_sequence",
    0,
    Number.MAX_SAFE_INTEGER,
  );
  const auth = await authenticate(pool, token);
  const result = await inTransaction(pool, async (client) => {
    const initial = await selectThread(client, threadId);
    await lockTeamMembershipChanges(client, initial.team_id);
    const thread = await selectThread(client, threadId, true);
    await requireMembership(client, thread.team_id, auth.principalId);
    await requireParticipant(client, threadId, auth.principalId);
    if (readThroughSequence > Number(thread.revision)) {
      throw new AppError(400, "invalid_request", "The read cursor exceeds the current thread revision.");
    }
    const readState = await client.query(
      `INSERT INTO thread_read_states (principal_id, thread_id, last_read_sequence, last_read_at)
       VALUES ($1, $2, $3, now())
       ON CONFLICT (principal_id, thread_id) DO UPDATE
         SET last_read_sequence = greatest(thread_read_states.last_read_sequence, $3),
             last_read_at = CASE
               WHEN $3 > thread_read_states.last_read_sequence THEN now()
               ELSE thread_read_states.last_read_at
             END
       RETURNING thread_id, last_read_sequence, last_read_at`,
      [auth.principalId, threadId, readThroughSequence],
    );
    return { teamId: thread.team_id, readState: threadReadStateFromRow(readState.rows[0]) };
  });
  notify(result.teamId, "thread.read.changed", { id: threadId, principal_id: auth.principalId });
  return result.readState;
}

export async function getBootstrap(pool, token, rawTeamId, protocolVersion = "0.1", notify = () => {}) {
  const teamId = uuid(rawTeamId, "team_id");
  const auth = await authenticate(pool, token);
  await registerProtocolAndNotify(pool, auth, protocolVersion, notify);
  await requireMembership(pool, teamId, auth.principalId);
  const teamResult = await pool.query(
    `SELECT id, name, created_by, created_at,
            (join_password_hash IS NOT NULL) AS join_password_configured
       FROM teams WHERE id = $1 AND dissolved_at IS NULL`,
    [teamId],
  );
  if (teamResult.rowCount === 0) {
    throw new AppError(404, "resource_not_found", "The team was not found.");
  }
  const members = await pool.query(
    `SELECT p.id, p.email, p.display_name, p.created_at, m.role,
            max(d.last_seen_at) AS last_seen_at,
            coalesce(bool_or(
              d.relay_protocol_version = '0.2'
              AND d.relay_protocol_seen_at >= now() - ($2 * interval '1 day')
            ), false) AS confirmed_transfers_supported,
            avatar.content_type AS avatar_content_type,
            avatar.byte_size AS avatar_byte_size,
            avatar.sha256 AS avatar_sha256,
            avatar.updated_at AS avatar_updated_at
       FROM memberships m
       JOIN principals p ON p.id = m.principal_id
       LEFT JOIN devices d ON d.principal_id = p.id AND d.revoked_at IS NULL
       LEFT JOIN principal_avatars avatar ON avatar.principal_id = p.id
      WHERE m.team_id = $1 AND m.removed_at IS NULL AND p.disabled_at IS NULL
      GROUP BY p.id, p.email, p.display_name, p.created_at, m.role,
               avatar.content_type, avatar.byte_size, avatar.sha256, avatar.updated_at
      ORDER BY p.created_at, p.id`,
    [teamId, CONFIRMED_TRANSFER_CAPABILITY_DAYS],
  );
  const visibleThreads = await pool.query(
    `SELECT w.*
       FROM work_threads w
       JOIN thread_participants tp ON tp.thread_id = w.id AND tp.principal_id = $2
      WHERE w.team_id = $1
      ORDER BY w.updated_at DESC, w.id`,
    [teamId, auth.principalId],
  );
  const details = [];
  for (const row of visibleThreads.rows) details.push(await threadDetail(pool, row, auth.principalId));
  return {
    protocol_version: protocolVersion,
    generated_at: new Date(),
    team: teamFromRow(teamResult.rows[0]),
    me: auth.principal,
    members: members.rows.map(teamMemberFromRow),
    threads: details,
  };
}
