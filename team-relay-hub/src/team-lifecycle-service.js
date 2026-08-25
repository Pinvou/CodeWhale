import { authenticate, requireMembership } from "./auth.js";
import {
  encryptJoinCode,
  newJoinCode,
  normalizeJoinCode,
  sha256,
} from "./crypto.js";
import { inTransaction } from "./db.js";
import { AppError } from "./errors.js";
import { exactKeys, uuid } from "./validation.js";

const OPEN_PHASES = "phase NOT IN ('closed', 'cancelled')";

export function requireJoinCodeEncryptionKey(value) {
  let decoded;
  try {
    decoded = Buffer.from(String(value || ""), "base64url");
  } catch {
    decoded = null;
  }
  if (decoded?.length !== 32) {
    throw new AppError(503, "join_code_storage_unavailable", "Team join code storage is not configured.");
  }
  return value;
}

async function lockActiveTeam(client, teamId) {
  const result = await client.query(
    `SELECT id, join_password_hash
       FROM teams
      WHERE id = $1 AND dissolved_at IS NULL
      FOR UPDATE`,
    [teamId],
  );
  if (!result.rowCount) {
    throw new AppError(404, "resource_not_found", "The active team was not found.");
  }
  return result.rows[0];
}

function responsibilitiesFor(row, principalId) {
  return [
    ["initiator", row.initiator_id],
    ["work_owner", row.owner_id],
    ["current_actor", row.current_actor_id],
    ["return_to", row.return_to_id],
    ["final_recipient", row.final_recipient_id],
    ["pending_sender", row.pending_sender_id],
    ["pending_recipient", row.pending_recipient_id],
  ].filter(([, value]) => value === principalId).map(([role]) => role);
}

function blockingThread(row, principalId = null) {
  return {
    thread_id: row.id,
    title: row.title,
    phase: row.phase,
    ...(principalId ? { responsibilities: responsibilitiesFor(row, principalId) } : {}),
  };
}

async function memberBlockingThreads(client, teamId, principalId) {
  const result = await client.query(
    `SELECT id, title, phase,
            initiator_id, owner_id, current_actor_id, return_to_id,
            final_recipient_id, pending_sender_id, pending_recipient_id
       FROM work_threads
      WHERE team_id = $1 AND ${OPEN_PHASES}
        AND $2 IN (initiator_id, owner_id, current_actor_id, return_to_id,
                   final_recipient_id, pending_sender_id, pending_recipient_id)
      ORDER BY updated_at DESC, id`,
    [teamId, principalId],
  );
  return result.rows.map(row => blockingThread(row, principalId));
}

async function teamBlockingThreads(client, teamId) {
  const result = await client.query(
    `SELECT id, title, phase
       FROM work_threads
      WHERE team_id = $1 AND ${OPEN_PHASES}
      ORDER BY updated_at DESC, id`,
    [teamId],
  );
  return result.rows.map(row => blockingThread(row));
}

async function requireNoMemberOpenWork(client, teamId, principalId) {
  const blockers = await memberBlockingThreads(client, teamId, principalId);
  if (blockers.length) {
    throw new AppError(
      409,
      "member_has_open_work",
      "The member still has required roles in open work threads.",
      { blocking_threads: blockers },
    );
  }
}

async function rotateJoinCodeAfterMembershipEnds(client, teamId, updatedBy, encryptionKey) {
  const replacement = newJoinCode();
  const key = requireJoinCodeEncryptionKey(encryptionKey);
  await client.query(
    `UPDATE teams
        SET join_password_hash = $2,
            join_password_updated_at = now(),
            join_password_updated_by = $3,
            join_code_ciphertext = $4
      WHERE id = $1`,
    [
      teamId,
      sha256(normalizeJoinCode(replacement)),
      updatedBy,
      encryptJoinCode(replacement, key, teamId),
    ],
  );
}

export async function transferTeamOwner(pool, token, rawTeamId, body) {
  const teamId = uuid(rawTeamId, "team_id");
  exactKeys(body, ["new_owner_id"]);
  const newOwnerId = uuid(body.new_owner_id, "new_owner_id");
  const auth = await authenticate(pool, token);
  if (newOwnerId === auth.principalId) {
    throw new AppError(400, "invalid_request", "The new owner must be another active team member.");
  }

  return inTransaction(pool, async client => {
    await lockActiveTeam(client, teamId);
    await requireMembership(client, teamId, auth.principalId, "owner");
    const target = await client.query(
      `SELECT role
         FROM memberships
        WHERE team_id = $1 AND principal_id = $2 AND removed_at IS NULL
        FOR UPDATE`,
      [teamId, newOwnerId],
    );
    if (!target.rowCount || target.rows[0].role !== "member") {
      throw new AppError(404, "resource_not_found", "The new owner must be an active ordinary team member.");
    }
    await requireNoMemberOpenWork(client, teamId, auth.principalId);
    const demoted = await client.query(
      `UPDATE memberships
          SET role = 'member'
        WHERE team_id = $1 AND principal_id = $2 AND role = 'owner' AND removed_at IS NULL`,
      [teamId, auth.principalId],
    );
    const promoted = await client.query(
      `UPDATE memberships
          SET role = 'owner'
        WHERE team_id = $1 AND principal_id = $2 AND role = 'member' AND removed_at IS NULL`,
      [teamId, newOwnerId],
    );
    if (demoted.rowCount !== 1 || promoted.rowCount !== 1) {
      throw new AppError(409, "service_unavailable", "The team owner could not be transferred atomically.");
    }
    return { team_id: teamId, previous_owner_id: auth.principalId, owner_id: newOwnerId };
  });
}

export async function leaveTeam(pool, token, rawTeamId, encryptionKey) {
  const teamId = uuid(rawTeamId, "team_id");
  const auth = await authenticate(pool, token);
  await inTransaction(pool, async client => {
    const team = await lockActiveTeam(client, teamId);
    const membership = await requireMembership(client, teamId, auth.principalId);
    if (membership.role === "owner") {
      throw new AppError(409, "last_team_owner", "The team owner must transfer ownership or dissolve the team before leaving.");
    }
    await requireNoMemberOpenWork(client, teamId, auth.principalId);
    await client.query(
      `UPDATE memberships
          SET removed_at = clock_timestamp()
        WHERE team_id = $1 AND principal_id = $2 AND removed_at IS NULL`,
      [teamId, auth.principalId],
    );
    if (team.join_password_hash) {
      await rotateJoinCodeAfterMembershipEnds(client, teamId, auth.principalId, encryptionKey);
    }
  });
  return { team_id: teamId, principal_id: auth.principalId };
}

export async function removeTeamMember(pool, token, rawTeamId, rawPrincipalId, encryptionKey) {
  const teamId = uuid(rawTeamId, "team_id");
  const principalId = uuid(rawPrincipalId, "principal_id");
  const auth = await authenticate(pool, token);

  await inTransaction(pool, async client => {
    const team = await lockActiveTeam(client, teamId);
    await requireMembership(client, teamId, auth.principalId, "owner");
    const target = await client.query(
      `SELECT role
         FROM memberships
        WHERE team_id = $1 AND principal_id = $2 AND removed_at IS NULL
        FOR UPDATE`,
      [teamId, principalId],
    );
    if (!target.rowCount) {
      throw new AppError(404, "resource_not_found", "The team member was not found.");
    }
    if (target.rows[0].role === "owner") {
      const owners = await client.query(
        `SELECT count(*)::integer AS count
           FROM memberships
          WHERE team_id = $1 AND role = 'owner' AND removed_at IS NULL`,
        [teamId],
      );
      if (owners.rows[0].count <= 1) {
        throw new AppError(409, "last_team_owner", "The last team owner cannot be removed.");
      }
    }
    await requireNoMemberOpenWork(client, teamId, principalId);
    await client.query(
      `UPDATE memberships
          SET removed_at = clock_timestamp()
        WHERE team_id = $1 AND principal_id = $2`,
      [teamId, principalId],
    );
    if (team.join_password_hash) {
      await rotateJoinCodeAfterMembershipEnds(client, teamId, auth.principalId, encryptionKey);
    }
  });
}

export async function dissolveTeam(pool, token, rawTeamId) {
  const teamId = uuid(rawTeamId, "team_id");
  const auth = await authenticate(pool, token);
  return inTransaction(pool, async client => {
    await lockActiveTeam(client, teamId);
    await requireMembership(client, teamId, auth.principalId, "owner");
    const blockers = await teamBlockingThreads(client, teamId);
    if (blockers.length) {
      throw new AppError(
        409,
        "team_has_open_work",
        "The team still has open work threads.",
        { blocking_threads: blockers },
      );
    }
    const members = await client.query(
      `SELECT principal_id
         FROM memberships
        WHERE team_id = $1 AND removed_at IS NULL`,
      [teamId],
    );
    await client.query(
      `UPDATE teams
          SET dissolved_at = clock_timestamp(),
              dissolved_by = $2,
              join_password_hash = NULL,
              join_password_updated_at = NULL,
              join_password_updated_by = NULL,
              join_code_ciphertext = NULL
        WHERE id = $1 AND dissolved_at IS NULL`,
      [teamId, auth.principalId],
    );
    await client.query(
      `UPDATE invites
          SET revoked_at = clock_timestamp()
        WHERE team_id = $1 AND claimed_at IS NULL AND revoked_at IS NULL`,
      [teamId],
    );
    return { team_id: teamId, principal_ids: members.rows.map(row => row.principal_id) };
  });
}
