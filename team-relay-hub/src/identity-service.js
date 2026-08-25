import { authenticate, CONFIRMED_TRANSFER_CAPABILITY_DAYS, requireMembership } from "./auth.js";
import {
  decryptJoinCode,
  encryptJoinCode,
  newId,
  newJoinCode,
  newSecret,
  normalizeJoinCode,
  sha256,
} from "./crypto.js";
import { inTransaction } from "./db.js";
import { AppError } from "./errors.js";
import {
  membershipFromRow,
  principalFromRow,
  teamFromRow,
  teamMemberFromRow,
} from "./serializers.js";
import { requireJoinCodeEncryptionKey } from "./team-lifecycle-service.js";
import { email, exactKeys, integer, text, uuid } from "./validation.js";

const HOUR_MS = 60 * 60 * 1000;
const DEFAULT_DEVICE_NAME = "Pinvou desktop";

function enrollmentEmail(body) {
  return body.email === undefined ? null : email(body.email);
}

function enrollmentDeviceName(body) {
  return body.device_name === undefined
    ? DEFAULT_DEVICE_NAME
    : text(body.device_name, "device_name", { minimum: 1, maximum: 120 }).trim();
}

async function insertPrincipal(client, principalId, accountEmail, displayName) {
  try {
    return await client.query(
      `INSERT INTO principals (id, email, display_name) VALUES ($1, $2, $3)
       RETURNING id, email, display_name, created_at`,
      [principalId, accountEmail, displayName],
    );
  } catch (error) {
    if (error?.code === "23505" && error?.constraint === "principals_email_unique_idx") {
      throw new AppError(409, "email_already_registered", "This email is already registered on this Team Relay service.");
    }
    throw error;
  }
}

async function lockTeam(client, teamId, mode = "share") {
  const lockClause = mode === "update" ? "FOR UPDATE" : "FOR SHARE";
  const result = await client.query(
    `SELECT id FROM teams WHERE id = $1 AND dissolved_at IS NULL ${lockClause}`,
    [teamId],
  );
  if (!result.rowCount) {
    throw new AppError(404, "resource_not_found", "The team was not found.");
  }
}

async function lockInviteAfterTeam(client, tokenHash) {
  const lookup = await client.query(
    "SELECT team_id FROM invites WHERE token_hash = $1",
    [tokenHash],
  );
  if (!lookup.rowCount) return null;

  const teamResult = await client.query(
    `SELECT id, name, created_by, created_at, dissolved_at,
            (join_password_hash IS NOT NULL) AS join_password_configured
       FROM teams
      WHERE id = $1
      FOR UPDATE`,
    [lookup.rows[0].team_id],
  );
  if (!teamResult.rowCount) return null;

  const inviteResult = await client.query(
    "SELECT * FROM invites WHERE token_hash = $1 AND team_id = $2 FOR UPDATE",
    [tokenHash, teamResult.rows[0].id],
  );
  if (!inviteResult.rowCount) return null;
  return { invite: inviteResult.rows[0], team: teamResult.rows[0] };
}

function requireUsableInvite(lockedInvite) {
  if (!lockedInvite || lockedInvite.team.dissolved_at || lockedInvite.invite.revoked_at) {
    throw new AppError(410, "invite_expired", "The invitation is invalid or expired.");
  }
  if (lockedInvite.invite.claimed_at) {
    throw new AppError(409, "invite_already_claimed", "The invitation was already claimed.");
  }
  if (new Date(lockedInvite.invite.expires_at).getTime() <= Date.now()) {
    throw new AppError(410, "invite_expired", "The invitation is invalid or expired.");
  }
  return lockedInvite;
}

export async function createTeamWithOwnerInvite(pool, { name, expiresInHours = 24 }) {
  const teamName = text(name, "name", { minimum: 1, maximum: 120 }).trim();
  const hours = integer(expiresInHours, "expiresInHours", 1, 168);
  const teamId = newId();
  const inviteId = newId();
  const inviteCode = newSecret();
  const expiresAt = new Date(Date.now() + hours * HOUR_MS);

  await inTransaction(pool, async (client) => {
    await client.query(
      "INSERT INTO teams (id, name, created_by) VALUES ($1, $2, NULL)",
      [teamId, teamName],
    );
    await client.query(
      `INSERT INTO invites (id, team_id, token_hash, role, created_by, expires_at)
       VALUES ($1, $2, $3, 'owner', NULL, $4)`,
      [inviteId, teamId, sha256(inviteCode), expiresAt],
    );
  });

  return { team_id: teamId, invite_id: inviteId, invite_code: inviteCode, expires_at: expiresAt };
}

export async function registerAccount(pool, body) {
  exactKeys(body, ["email", "display_name"], ["device_name"]);
  const accountEmail = email(body.email);
  const displayName = text(body.display_name, "display_name", { minimum: 1, maximum: 80 }).trim();
  const deviceName = enrollmentDeviceName(body);
  const deviceToken = newSecret();

  return inTransaction(pool, async client => {
    const principalResult = await client.query(
      `INSERT INTO principals (id, email, display_name)
       VALUES ($1, $2, $3)
       ON CONFLICT (email) WHERE email IS NOT NULL
       DO UPDATE SET display_name = EXCLUDED.display_name
       WHERE principals.disabled_at IS NULL
       RETURNING id, email, display_name, created_at`,
      [newId(), accountEmail, displayName],
    );
    if (!principalResult.rowCount) {
      throw new AppError(403, "action_not_allowed", "This Team Relay account is disabled.");
    }
    const principal = principalResult.rows[0];
    const deviceResult = await client.query(
      `INSERT INTO devices (id, principal_id, name, token_hash, last_seen_at)
       VALUES ($1, $2, $3, $4, now())
       RETURNING id, principal_id, name, last_seen_at, created_at`,
      [newId(), principal.id, deviceName, sha256(deviceToken)],
    );
    const memberships = await client.query(
      `SELECT m.team_id
         FROM memberships m
         JOIN teams t ON t.id = m.team_id
        WHERE m.principal_id = $1 AND m.removed_at IS NULL AND t.dissolved_at IS NULL`,
      [principal.id],
    );
    return {
      device_token: deviceToken,
      principal: principalFromRow(principal),
      device: deviceResult.rows[0],
      team_ids: memberships.rows.map(row => row.team_id),
    };
  });
}

export async function createEnrollmentWithTeam(pool, body) {
  exactKeys(body, ["team_name", "display_name"], ["email", "device_name"]);
  const teamName = text(body.team_name, "team_name", { minimum: 1, maximum: 120 }).trim();
  const accountEmail = enrollmentEmail(body);
  const displayName = text(body.display_name, "display_name", { minimum: 1, maximum: 80 }).trim();
  const deviceName = enrollmentDeviceName(body);
  const deviceToken = newSecret();
  return inTransaction(pool, async client => {
    const principalId = newId();
    const deviceId = newId();
    const teamId = newId();
    const principalResult = await insertPrincipal(client, principalId, accountEmail, displayName);
    const deviceResult = await client.query(
      `INSERT INTO devices (id, principal_id, name, token_hash, last_seen_at)
       VALUES ($1, $2, $3, $4, now())
       RETURNING id, principal_id, name, last_seen_at, created_at`,
      [deviceId, principalId, deviceName, sha256(deviceToken)],
    );
    const teamResult = await client.query(
      `INSERT INTO teams (id, name, created_by) VALUES ($1, $2, $3)
       RETURNING id, name, created_by, created_at, false AS join_password_configured`,
      [teamId, teamName, principalId],
    );
    const membershipResult = await client.query(
      `INSERT INTO memberships (team_id, principal_id, role) VALUES ($1, $2, 'owner')
       RETURNING team_id, principal_id, role, joined_at`,
      [teamId, principalId],
    );
    return { device_token: deviceToken, principal: principalResult.rows[0], device: deviceResult.rows[0], team: teamFromRow(teamResult.rows[0]), membership: membershipFromRow(membershipResult.rows[0]) };
  });
}

export async function claimEnrollment(pool, body) {
  exactKeys(body, ["invite_code", "display_name"], ["email", "device_name"]);
  const inviteCode = text(body.invite_code, "invite_code", { minimum: 32, maximum: 256 });
  const accountEmail = enrollmentEmail(body);
  const displayName = text(body.display_name, "display_name", { minimum: 1, maximum: 80 }).trim();
  const deviceName = enrollmentDeviceName(body);
  const deviceToken = newSecret();

  return inTransaction(pool, async (client) => {
    const lockedInvite = requireUsableInvite(
      await lockInviteAfterTeam(client, sha256(inviteCode)),
    );
    const { invite, team } = lockedInvite;

    const principalId = newId();
    const deviceId = newId();
    await insertPrincipal(client, principalId, accountEmail, displayName);
    await client.query(
      `INSERT INTO devices (id, principal_id, name, token_hash, last_seen_at)
       VALUES ($1, $2, $3, $4, now())`,
      [deviceId, principalId, deviceName, sha256(deviceToken)],
    );
    const membershipResult = await client.query(
      `INSERT INTO memberships (team_id, principal_id, role)
       VALUES ($1, $2, $3)
       RETURNING team_id, principal_id, role, joined_at`,
      [invite.team_id, principalId, invite.role],
    );
    await client.query(
      `UPDATE invites
          SET claimed_at = now(), claimed_by = $2
        WHERE id = $1`,
      [invite.id, principalId],
    );
    if (invite.role === "owner") {
      const update = await client.query(
        `UPDATE teams SET created_by = $2
          WHERE id = $1 AND created_by IS NULL`,
        [invite.team_id, principalId],
      );
      if (update.rowCount !== 1) {
        throw new AppError(409, "invite_already_claimed", "The owner invitation is no longer valid.");
      }
    }
    const principalResult = await client.query(
      "SELECT id, email, display_name, created_at FROM principals WHERE id = $1",
      [principalId],
    );
    const deviceResult = await client.query(
      `SELECT id, principal_id, name, last_seen_at, created_at
         FROM devices WHERE id = $1`,
      [deviceId],
    );

    return {
      device_token: deviceToken,
      principal: principalResult.rows[0],
      device: deviceResult.rows[0],
      team: {
        id: invite.team_id,
        name: team.name,
        created_by: invite.role === "owner" ? principalId : team.created_by,
        created_at: team.created_at,
        join_password_configured: Boolean(team.join_password_configured),
      },
      membership: membershipFromRow(membershipResult.rows[0]),
    };
  });
}

export async function joinEnrollment(pool, body) {
  exactKeys(body, ["join_password", "display_name"], ["email", "device_name"]);
  const joinPassword = normalizeJoinCode(
    text(body.join_password, "join_password", { minimum: 8, maximum: 256 }),
  );
  const accountEmail = enrollmentEmail(body);
  const displayName = text(body.display_name, "display_name", { minimum: 1, maximum: 80 }).trim();
  const deviceName = enrollmentDeviceName(body);
  const singleUseInvite = await pool.query(
    `SELECT 1
       FROM invites i
      WHERE i.token_hash = $1 AND i.claimed_at IS NULL AND i.revoked_at IS NULL
        AND i.expires_at > now()
      LIMIT 1`,
    [sha256(joinPassword)],
  );
  if (singleUseInvite.rowCount !== 0) {
    return claimEnrollment(pool, {
      invite_code: joinPassword,
      ...(accountEmail ? { email: accountEmail } : {}),
      display_name: displayName,
      device_name: deviceName,
    });
  }
  const deviceToken = newSecret();

  return inTransaction(pool, async (client) => {
    const teamResult = await client.query(
      `SELECT id, name, created_by, created_at
         FROM teams
        WHERE join_password_hash = $1 AND created_by IS NOT NULL AND dissolved_at IS NULL
        FOR SHARE`,
      [sha256(joinPassword)],
    );
    if (teamResult.rowCount === 0) {
      throw new AppError(401, "join_password_invalid", "The team join password is invalid.");
    }
    const team = teamResult.rows[0];
    const principalId = newId();
    const deviceId = newId();
    await insertPrincipal(client, principalId, accountEmail, displayName);
    await client.query(
      `INSERT INTO devices (id, principal_id, name, token_hash, last_seen_at)
       VALUES ($1, $2, $3, $4, now())`,
      [deviceId, principalId, deviceName, sha256(deviceToken)],
    );
    const membershipResult = await client.query(
      `INSERT INTO memberships (team_id, principal_id, role)
       VALUES ($1, $2, 'member')
       RETURNING team_id, principal_id, role, joined_at`,
      [team.id, principalId],
    );
    const principalResult = await client.query(
      "SELECT id, email, display_name, created_at FROM principals WHERE id = $1",
      [principalId],
    );
    const deviceResult = await client.query(
      `SELECT id, principal_id, name, last_seen_at, created_at
         FROM devices WHERE id = $1`,
      [deviceId],
    );
    return {
      device_token: deviceToken,
      principal: principalResult.rows[0],
      device: deviceResult.rows[0],
      team: teamFromRow({ ...team, join_password_configured: true }),
      membership: membershipFromRow(membershipResult.rows[0]),
    };
  });
}

export async function getMe(pool, token) {
  const auth = await authenticate(pool, token);
  const memberships = await pool.query(
    `SELECT m.team_id, m.principal_id, m.role, m.joined_at
       FROM memberships m
       JOIN teams t ON t.id = m.team_id
      WHERE m.principal_id = $1 AND m.removed_at IS NULL AND t.dissolved_at IS NULL
      ORDER BY m.joined_at`,
    [auth.principalId],
  );
  return {
    principal: auth.principal,
    device: auth.device,
    memberships: memberships.rows.map(membershipFromRow),
  };
}

export async function updateProfile(pool, token, body) {
  exactKeys(body, ["display_name"], ["email"]);
  const auth = await authenticate(pool, token);
  const displayName = text(body.display_name, "display_name", { minimum: 1, maximum: 80 }).trim();
  const requestedEmail = body.email === undefined ? null : email(body.email);

  return inTransaction(pool, async client => {
    const current = await client.query(
      "SELECT id, email, display_name, created_at FROM principals WHERE id = $1 FOR UPDATE",
      [auth.principalId],
    );
    const principal = current.rows[0];
    if (principal.email && requestedEmail && principal.email !== requestedEmail) {
      throw new AppError(409, "email_change_not_supported", "The registered email cannot be changed yet.");
    }
    try {
      const updated = await client.query(
        `WITH updated AS (
           UPDATE principals
              SET display_name = $2,
                  email = coalesce(email, $3)
            WHERE id = $1
            RETURNING id, email, display_name, created_at
         )
         SELECT updated.*,
                avatar.content_type AS avatar_content_type,
                avatar.byte_size AS avatar_byte_size,
                avatar.sha256 AS avatar_sha256,
                avatar.updated_at AS avatar_updated_at
           FROM updated
           LEFT JOIN principal_avatars avatar ON avatar.principal_id = updated.id`,
        [auth.principalId, displayName, requestedEmail],
      );
      const memberships = await client.query(
        `SELECT m.team_id
           FROM memberships m
           JOIN teams t ON t.id = m.team_id
          WHERE m.principal_id = $1 AND m.removed_at IS NULL AND t.dissolved_at IS NULL`,
        [auth.principalId],
      );
      return {
        principal: principalFromRow(updated.rows[0]),
        team_ids: memberships.rows.map(row => row.team_id),
      };
    } catch (error) {
      if (error?.code === "23505" && error?.constraint === "principals_email_unique_idx") {
        throw new AppError(409, "email_already_registered", "This email is already registered on this Team Relay service.");
      }
      throw error;
    }
  });
}

export async function listTeams(pool, token) {
  const auth = await authenticate(pool, token);
  const result = await pool.query(
    `SELECT t.id, t.name, t.created_by, t.created_at,
            (t.join_password_hash IS NOT NULL) AS join_password_configured,
            m.role, m.joined_at,
            (SELECT count(*)::integer FROM memberships tm
              WHERE tm.team_id = t.id AND tm.removed_at IS NULL) AS member_count,
            (SELECT count(*)::integer FROM work_threads w
              WHERE w.team_id = t.id AND w.phase NOT IN ('closed', 'cancelled')) AS open_work_count
       FROM memberships m
       JOIN teams t ON t.id = m.team_id
      WHERE m.principal_id = $1 AND m.removed_at IS NULL AND t.dissolved_at IS NULL
      ORDER BY m.joined_at DESC, t.id`,
    [auth.principalId],
  );
  return { teams: result.rows.map(row => ({ ...teamFromRow(row), role: row.role, joined_at: row.joined_at, member_count: row.member_count, open_work_count: row.open_work_count })) };
}

export async function createTeam(pool, token, body) {
  exactKeys(body, ["name"]);
  const name = text(body.name, "name", { minimum: 1, maximum: 120 }).trim();
  const auth = await authenticate(pool, token);
  return inTransaction(pool, async client => {
    const teamId = newId();
    const teamResult = await client.query(
      `INSERT INTO teams (id, name, created_by) VALUES ($1, $2, $3)
       RETURNING id, name, created_by, created_at, false AS join_password_configured`,
      [teamId, name, auth.principalId],
    );
    const membershipResult = await client.query(
      `INSERT INTO memberships (team_id, principal_id, role) VALUES ($1, $2, 'owner')
       RETURNING team_id, principal_id, role, joined_at`,
      [teamId, auth.principalId],
    );
    return { team: teamFromRow(teamResult.rows[0]), membership: membershipFromRow(membershipResult.rows[0]) };
  });
}

export async function joinTeam(pool, token, body) {
  exactKeys(body, ["join_code"]);
  const joinCode = normalizeJoinCode(
    text(body.join_code, "join_code", { minimum: 8, maximum: 256 }),
  );
  const auth = await authenticate(pool, token);
  return inTransaction(pool, async client => {
    const tokenHash = sha256(joinCode);
    const lockedInvite = await lockInviteAfterTeam(client, tokenHash);
    let team;
    let invite = null;
    if (lockedInvite) {
      if (lockedInvite.team.created_by === null) {
        throw new AppError(401, "join_password_invalid", "The team join code is invalid.");
      }
      ({ invite, team } = requireUsableInvite(lockedInvite));
    } else {
      const teamResult = await client.query(
        `SELECT id, name, created_by, created_at, true AS join_password_configured
           FROM teams
          WHERE join_password_hash = $1 AND created_by IS NOT NULL AND dissolved_at IS NULL
          FOR SHARE`,
        [tokenHash],
      );
      if (!teamResult.rowCount) throw new AppError(401, "join_password_invalid", "The team join code is invalid.");
      team = teamResult.rows[0];
    }
    const priorMembership = await client.query(
      `SELECT role, removed_at FROM memberships
        WHERE team_id = $1 AND principal_id = $2
        FOR UPDATE`,
      [team.id, auth.principalId],
    );
    if (
      invite
      && priorMembership.rows[0]?.removed_at
      && new Date(invite.created_at).getTime()
        <= new Date(priorMembership.rows[0].removed_at).getTime()
    ) {
      throw new AppError(410, "invite_expired", "The invitation predates the latest membership removal.");
    }
    const membershipResult = await client.query(
      `INSERT INTO memberships (team_id, principal_id, role, removed_at)
       VALUES ($1, $2, 'member', NULL)
       ON CONFLICT (team_id, principal_id) DO UPDATE
         SET role = CASE
               WHEN memberships.removed_at IS NULL THEN memberships.role
               ELSE EXCLUDED.role
             END,
             removed_at = NULL
       RETURNING team_id, principal_id, role, joined_at`,
      [team.id, auth.principalId],
    );
    if (invite) {
      await client.query(`UPDATE invites SET claimed_at = now(), claimed_by = $2 WHERE id = $1`, [invite.id, auth.principalId]);
    }
    return { team: teamFromRow(team), membership: membershipFromRow(membershipResult.rows[0]) };
  });
}

export async function listTeamMembers(pool, token, rawTeamId) {
  const teamId = uuid(rawTeamId, "team_id");
  const auth = await authenticate(pool, token);
  await requireMembership(pool, teamId, auth.principalId);
  const result = await pool.query(
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
  return { members: result.rows.map(teamMemberFromRow) };
}

export async function createTeamInvite(pool, token, rawTeamId, body) {
  const teamId = uuid(rawTeamId, "team_id");
  exactKeys(body, [], ["expires_in_hours"]);
  const expiresInHours = body.expires_in_hours === undefined
    ? 24
    : integer(body.expires_in_hours, "expires_in_hours", 1, 168);
  const auth = await authenticate(pool, token);
  const inviteId = newId();
  const inviteCode = newSecret();
  const expiresAt = new Date(Date.now() + expiresInHours * HOUR_MS);
  await inTransaction(pool, async client => {
    await lockTeam(client, teamId);
    await requireMembership(client, teamId, auth.principalId);
    await client.query(
      `INSERT INTO invites (id, team_id, token_hash, role, created_by, expires_at, created_at)
       VALUES ($1, $2, $3, 'member', $4, $5, clock_timestamp())`,
      [inviteId, teamId, sha256(inviteCode), auth.principalId, expiresAt],
    );
  });
  return { invite_id: inviteId, invite_code: inviteCode, role: "member", expires_at: expiresAt };
}

export async function getTeamJoinPassword(pool, token, rawTeamId, encryptionKey) {
  const teamId = uuid(rawTeamId, "team_id");
  const auth = await authenticate(pool, token);
  return inTransaction(pool, async client => {
    await lockTeam(client, teamId);
    const membership = await requireMembership(client, teamId, auth.principalId);
    const result = await client.query(
      `SELECT join_password_hash, join_code_ciphertext, join_code_visibility,
              join_password_updated_at AS updated_at
         FROM teams WHERE id = $1`,
      [teamId],
    );
    const row = result.rows[0];
    const configured = Boolean(row.join_password_hash);
    const visible = configured && (row.join_code_visibility === "members" || membership.role === "owner");
    if (!visible || !row.join_code_ciphertext) {
      return {
        configured,
        visible,
        recoverable: Boolean(row.join_code_ciphertext),
        visibility: row.join_code_visibility,
        join_password: null,
        updated_at: row.updated_at,
      };
    }
    try {
      return {
        configured: true,
        visible: true,
        recoverable: true,
        visibility: row.join_code_visibility,
        join_password: decryptJoinCode(
          row.join_code_ciphertext,
          requireJoinCodeEncryptionKey(encryptionKey),
          teamId,
        ),
        updated_at: row.updated_at,
      };
    } catch (error) {
      if (error instanceof AppError) throw error;
      throw new AppError(503, "join_code_storage_unavailable", "The team join code cannot be decrypted.");
    }
  });
}

export async function rotateTeamJoinPassword(pool, token, rawTeamId, body, encryptionKey) {
  const teamId = uuid(rawTeamId, "team_id");
  exactKeys(body, []);
  const auth = await authenticate(pool, token);
  const key = requireJoinCodeEncryptionKey(encryptionKey);
  for (let attempt = 0; attempt < 5; attempt += 1) {
    const joinPassword = newJoinCode();
    try {
      return await inTransaction(pool, async client => {
        await lockTeam(client, teamId, "update");
        await requireMembership(client, teamId, auth.principalId, "owner");
        const result = await client.query(
          `UPDATE teams
              SET join_password_hash = $2,
                  join_password_updated_at = now(),
                  join_password_updated_by = $3,
                  join_code_ciphertext = $4
            WHERE id = $1
            RETURNING join_code_visibility AS visibility,
                      join_password_updated_at AS updated_at`,
          [
            teamId,
            sha256(normalizeJoinCode(joinPassword)),
            auth.principalId,
            encryptJoinCode(joinPassword, key, teamId),
          ],
        );
        return {
          configured: true,
          visible: true,
          recoverable: true,
          visibility: result.rows[0].visibility,
          join_password: joinPassword,
          updated_at: result.rows[0].updated_at,
        };
      });
    } catch (error) {
      if (error?.code === "23505") {
        if (attempt < 4) continue;
        throw new AppError(503, "service_unavailable", "A unique team join code could not be generated.");
      }
      throw error;
    }
  }
  throw new AppError(503, "service_unavailable", "A unique team join code could not be generated.");
}

export async function setTeamJoinPasswordVisibility(pool, token, rawTeamId, body) {
  const teamId = uuid(rawTeamId, "team_id");
  exactKeys(body, ["visibility"]);
  const visibility = text(body.visibility, "visibility", { minimum: 1, maximum: 16 });
  if (!["members", "owners"].includes(visibility)) {
    throw new AppError(400, "invalid_request", "visibility must be members or owners.");
  }
  const auth = await authenticate(pool, token);
  return inTransaction(pool, async client => {
    await lockTeam(client, teamId, "update");
    await requireMembership(client, teamId, auth.principalId, "owner");
    const result = await client.query(
      `UPDATE teams SET join_code_visibility = $2 WHERE id = $1
        RETURNING join_code_visibility AS visibility`,
      [teamId, visibility],
    );
    return result.rows[0];
  });
}

export async function disableTeamJoinPassword(pool, token, rawTeamId) {
  const teamId = uuid(rawTeamId, "team_id");
  const auth = await authenticate(pool, token);
  return inTransaction(pool, async client => {
    await lockTeam(client, teamId, "update");
    await requireMembership(client, teamId, auth.principalId, "owner");
    const result = await client.query(
      `UPDATE teams
          SET join_password_hash = NULL,
              join_password_updated_at = NULL,
              join_password_updated_by = NULL,
              join_code_ciphertext = NULL
        WHERE id = $1
        RETURNING join_code_visibility AS visibility`,
      [teamId],
    );
    return {
      configured: false,
      visible: false,
      recoverable: false,
      visibility: result.rows[0].visibility,
      join_password: null,
      updated_at: null,
    };
  });
}

export async function getTeam(pool, teamId) {
  const result = await pool.query(
    `SELECT id, name, created_by, created_at,
            (join_password_hash IS NOT NULL) AS join_password_configured
       FROM teams WHERE id = $1 AND dissolved_at IS NULL`,
    [teamId],
  );
  if (result.rowCount === 0) {
    throw new AppError(404, "resource_not_found", "The team was not found.");
  }
  return teamFromRow(result.rows[0]);
}
