export function avatarFromRow(row, prefix = "avatar_") {
  const sha256 = row[`${prefix}sha256`];
  if (!sha256) return null;
  return {
    content_type: row[`${prefix}content_type`],
    byte_size: Number(row[`${prefix}byte_size`]),
    sha256,
    updated_at: row[`${prefix}updated_at`],
  };
}

export function principalFromRow(row, prefix = "") {
  return {
    id: row[`${prefix}id`],
    email: row[`${prefix}email`] || null,
    display_name: row[`${prefix}display_name`],
    created_at: row[`${prefix}created_at`],
    avatar: avatarFromRow(row, `${prefix}avatar_`),
  };
}

export function emailHint(value) {
  if (!value) return null;
  const [local, domain] = String(value).split("@");
  if (!local || !domain) return null;
  return `${local.slice(0, 1)}***@${domain}`;
}

export function teamFromRow(row) {
  return {
    id: row.id,
    name: row.name,
    created_by: row.created_by,
    created_at: row.created_at,
    join_password_configured: Boolean(row.join_password_configured ?? row.join_password_hash),
  };
}

export function membershipFromRow(row) {
  return {
    team_id: row.team_id,
    principal_id: row.principal_id,
    role: row.role,
    joined_at: row.joined_at,
  };
}

export function teamMemberFromRow(row) {
  return {
    id: row.id,
    display_name: row.display_name,
    email_hint: emailHint(row.email),
    created_at: row.created_at,
    role: row.role,
    last_seen_at: row.last_seen_at,
    confirmed_transfers_supported: Boolean(row.confirmed_transfers_supported),
    avatar: avatarFromRow(row),
  };
}

export function threadFromRow(row) {
  return {
    id: row.id,
    team_id: row.team_id,
    title: row.title,
    initiator_id: row.initiator_id,
    owner_id: row.owner_id,
    current_actor_id: row.current_actor_id,
    return_to_id: row.return_to_id,
    final_recipient_id: row.final_recipient_id,
    pending_transfer_kind: row.pending_transfer_kind || null,
    pending_sender_id: row.pending_sender_id || null,
    pending_recipient_id: row.pending_recipient_id || null,
    phase: row.phase,
    revision: Number(row.revision),
    created_at: row.created_at,
    updated_at: row.updated_at,
  };
}

export function threadPreferenceFromRow(row) {
  return {
    thread_id: row.thread_id || row.id,
    pinned: Boolean(row.pinned_at),
    hidden: Boolean(row.hidden_at),
    pinned_at: row.pinned_at || null,
    hidden_at: row.hidden_at || null,
    updated_at: row.preference_updated_at || row.updated_at || null,
  };
}

export function threadReadStateFromRow(row) {
  return {
    thread_id: row.thread_id || row.id,
    last_read_sequence: Number(row.last_read_sequence || 0),
    last_read_at: row.last_read_at || null,
  };
}

export function actionFromRow(row, attachmentIds = []) {
  return {
    id: row.id,
    thread_id: row.thread_id,
    sequence: Number(row.sequence),
    kind: row.kind,
    actor_id: row.actor_id,
    recipient_id: row.recipient_id,
    return_to_id: row.return_to_id,
    message: row.message,
    base_revision: Number(row.base_revision),
    idempotency_key: row.idempotency_key,
    transfer_mode: row.transfer_mode || null,
    attachment_ids: attachmentIds,
    delivery: row.delivery || {
      schema_version: 1,
      artifact_role: "intermediate",
      expected_next: "",
      open_questions: "",
      git_references: [],
    },
    created_at: row.created_at,
  };
}
