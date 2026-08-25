import { AppError } from "./errors.js";
import { sha256 } from "./crypto.js";
import { principalFromRow } from "./serializers.js";

export const CONFIRMED_TRANSFER_CAPABILITY_DAYS = 30;

export async function authenticate(pool, token) {
  if (!token) {
    throw new AppError(401, "authentication_required", "A device credential is required.");
  }
  const result = await pool.query(
    `SELECT d.id AS device_id, d.principal_id, d.name AS device_name,
            d.revoked_at, d.created_at AS device_created_at,
            d.relay_protocol_version, d.relay_protocol_seen_at,
            p.id AS principal_id, p.email AS principal_email,
            p.display_name AS principal_display_name,
            p.created_at AS principal_created_at, p.disabled_at,
            avatar.content_type AS principal_avatar_content_type,
            avatar.byte_size AS principal_avatar_byte_size,
            avatar.sha256 AS principal_avatar_sha256,
            avatar.updated_at AS principal_avatar_updated_at
       FROM devices d
       JOIN principals p ON p.id = d.principal_id
       LEFT JOIN principal_avatars avatar ON avatar.principal_id = p.id
      WHERE d.token_hash = $1`,
    [sha256(token)],
  );
  if (result.rowCount === 0) {
    throw new AppError(401, "authentication_required", "The device credential is invalid.");
  }
  const row = result.rows[0];
  if (row.revoked_at || row.disabled_at) {
    throw new AppError(401, "device_revoked", "The device credential was revoked.");
  }
  await pool.query("UPDATE devices SET last_seen_at = now() WHERE id = $1", [row.device_id]);
  return {
    deviceId: row.device_id,
    principalId: row.principal_id,
    principal: principalFromRow(row, "principal_"),
    device: {
      id: row.device_id,
      principal_id: row.principal_id,
      name: row.device_name,
      last_seen_at: new Date(),
      created_at: row.device_created_at,
    },
  };
}

export async function registerDeviceProtocol(pool, auth, protocolVersion) {
  const result = await pool.query(
    `WITH previous AS (
       SELECT id, principal_id,
              (relay_protocol_version = '0.2'
                AND relay_protocol_seen_at >= now() - ($3 * interval '1 day')) AS supported
         FROM devices
        WHERE id = $1
        FOR UPDATE
     ), updated AS (
       UPDATE devices d
          SET relay_protocol_version = $2, relay_protocol_seen_at = now()
         FROM previous p
        WHERE d.id = p.id
       RETURNING d.principal_id, p.supported AS previously_supported
     )
     SELECT principal_id, previously_supported, ($2 = '0.2') AS supported
       FROM updated`,
    [auth.deviceId, protocolVersion, CONFIRMED_TRANSFER_CAPABILITY_DAYS],
  );
  const row = result.rows[0];
  if (!row || row.previously_supported === row.supported) return [];
  const memberships = await pool.query(
    `SELECT m.team_id
       FROM memberships m
       JOIN teams t ON t.id = m.team_id
      WHERE m.principal_id = $1 AND m.removed_at IS NULL AND t.dissolved_at IS NULL`,
    [row.principal_id],
  );
  return memberships.rows.map(item => item.team_id);
}

export async function requireMembership(db, teamId, principalId, requiredRole = undefined) {
  const result = await db.query(
    `SELECT m.team_id, m.principal_id, m.role, m.joined_at
       FROM memberships m
       JOIN principals p ON p.id = m.principal_id
       JOIN teams t ON t.id = m.team_id
      WHERE m.team_id = $1 AND m.principal_id = $2
        AND m.removed_at IS NULL AND p.disabled_at IS NULL AND t.dissolved_at IS NULL`,
    [teamId, principalId],
  );
  if (result.rowCount === 0) {
    throw new AppError(403, "membership_required", "An active team membership is required.");
  }
  const membership = result.rows[0];
  if (requiredRole && membership.role !== requiredRole) {
    throw new AppError(403, "action_not_allowed", "The team role does not permit this action.");
  }
  return membership;
}
