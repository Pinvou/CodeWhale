import assert from "node:assert/strict";
import { mkdir, mkdtemp, readFile, rm, utimes, writeFile } from "node:fs/promises";
import { createHash, randomUUID } from "node:crypto";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import pg from "pg";
import sharp from "sharp";
import { cleanupAttachments } from "../src/attachment-cleanup.js";
import { createHubServer } from "../src/http.js";
import { createTeamWithOwnerInvite } from "../src/identity-service.js";

const { Pool } = pg;
const databaseUrl = process.env.TEAM_RELAY_TEST_DATABASE_URL;

test("A to B to C to B to A survives refresh and preserves idempotency", {
  skip: !databaseUrl && "set TEAM_RELAY_TEST_DATABASE_URL to an isolated *_test database",
}, async (t) => {
  const pool = new Pool({ connectionString: databaseUrl });
  const database = await pool.query("SELECT current_database() AS name");
  assert.match(database.rows[0].name, /_test$/, "integration test requires an isolated *_test database");
  await pool.query("DROP SCHEMA public CASCADE; CREATE SCHEMA public");
  const confirmedTransferMigration = "0009_confirmed_transfers.sql";
  const migrationNames = ["0001_demo.sql", "0002_delivery_packages.sql", "0003_thread_management.sql", "0004_fixed_join_password.sql", "0005_short_join_code.sql", "0006_persistent_join_code.sql", "0007_principal_email.sql", "0008_thread_read_states.sql", confirmedTransferMigration, "0010_principal_avatars.sql", "0011_avatar_hardening.sql", "0012_avatar_reduced_motion.sql", "0013_team_lifecycle.sql"];
  async function applyMigration(migrationName) {
    const migration = await readFile(
      new URL(`../../docs/team-relay/migrations/${migrationName}`, import.meta.url),
      "utf8",
    );
    await pool.query(migration);
  }
  for (const migrationName of migrationNames) {
    if (migrationName === confirmedTransferMigration) break;
    await applyMigration(migrationName);
  }
  const migrationPrincipalA = randomUUID();
  const migrationPrincipalB = randomUUID();
  const migrationTeam = randomUUID();
  const migrationThread = randomUUID();
  await pool.query(
    "INSERT INTO principals (id, email, display_name) VALUES ($1, $2, 'Migration A'), ($3, $4, 'Migration B')",
    [migrationPrincipalA, "migration-a@example.com", migrationPrincipalB, "migration-b@example.com"],
  );
  await pool.query(
    "INSERT INTO devices (id, principal_id, name, token_hash) VALUES ($1, $2, 'Legacy device', $3)",
    [randomUUID(), migrationPrincipalA, createHash("sha256").update("migration-device").digest("hex")],
  );
  await pool.query(
    "INSERT INTO teams (id, name, created_by) VALUES ($1, 'Migration team', $2)",
    [migrationTeam, migrationPrincipalA],
  );
  await pool.query(
    "INSERT INTO memberships (team_id, principal_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member')",
    [migrationTeam, migrationPrincipalA, migrationPrincipalB],
  );
  await pool.query(
    `INSERT INTO work_threads (id, team_id, title, initiator_id, owner_id, current_actor_id, return_to_id, final_recipient_id, phase, revision)
     VALUES ($1, $2, 'Existing work', $3, $4, $4, $3, $3, 'active', 1)`,
    [migrationThread, migrationTeam, migrationPrincipalA, migrationPrincipalB],
  );
  await pool.query(
    "INSERT INTO thread_participants (thread_id, principal_id, first_action_sequence) VALUES ($1, $2, 1), ($1, $3, 1)",
    [migrationThread, migrationPrincipalA, migrationPrincipalB],
  );
  await pool.query(
    `INSERT INTO relay_actions (id, thread_id, sequence, kind, actor_id, recipient_id, return_to_id, message, base_revision, idempotency_key, request_hash)
     VALUES ($1, $2, 1, 'assign', $3, $4, $3, 'Existing assignment', 0, $5, $6)`,
    [
      randomUUID(),
      migrationThread,
      migrationPrincipalA,
      migrationPrincipalB,
      randomUUID(),
      createHash("sha256").update("migration-request").digest("hex"),
    ],
  );
  await applyMigration(confirmedTransferMigration);
  const migratedCompatibility = await pool.query(
    `SELECT a.transfer_mode, d.relay_protocol_version, tp.material_access_at
       FROM relay_actions a
       JOIN devices d ON d.principal_id = a.actor_id
       JOIN thread_participants tp ON tp.thread_id = a.thread_id AND tp.principal_id = a.recipient_id
      WHERE a.thread_id = $1`,
    [migrationThread],
  );
  assert.equal(migratedCompatibility.rows[0].transfer_mode, "direct_legacy");
  assert.equal(migratedCompatibility.rows[0].relay_protocol_version, "0.1");
  assert.ok(migratedCompatibility.rows[0].material_access_at, "existing participants keep material access");
  await pool.query("DROP SCHEMA public CASCADE; CREATE SCHEMA public");
  for (const migrationName of migrationNames) await applyMigration(migrationName);
  const attachmentDir = await mkdtemp(join(tmpdir(), "pinvou-team-relay-attachments-"));

  const config = {
    publicBasePath: "/pinvou3/team-relay",
    allowSelfServiceEnrollment: true,
    betaAccessPasswordHash: createHash("sha256").update("test-only-access-code").digest("hex"),
    maxJsonBytes: 1024 * 1024,
    attachmentDir,
    maxAttachmentBytes: 2048,
    maxAvatarBytes: 2048,
    maxTeamAttachmentBytes: 1600,
    orphanAttachmentHours: 1,
    closedAttachmentDays: 30,
    trustProxy: true,
    joinRateLimitFailures: 5,
    joinRateLimitWindowSeconds: 600,
    joinRateLimitBlockSeconds: 900,
    joinCodeEncryptionKey: Buffer.alloc(32, 7).toString("base64url"),
  };
  const server = createHubServer({ pool, config });
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  const origin = `http://127.0.0.1:${address.port}${config.publicBasePath}`;
  t.after(async () => {
    server.closeEventStreams();
    await new Promise((resolve) => server.close(resolve));
    await pool.end();
    await rm(attachmentDir, { recursive: true, force: true });
  });

  async function api(path, { method = "GET", token, body, headers = {} } = {}) {
    const response = await fetch(`${origin}${path}`, {
      method,
      headers: {
        ...headers,
        ...(token ? { Authorization: `Bearer ${token}` } : {}),
        ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const value = response.status === 204 ? undefined : await response.json();
    const result = { status: response.status, value };
    const retryAfter = response.headers.get("retry-after");
    if (retryAfter) result.retryAfter = retryAfter;
    return result;
  }

  let emailSequence = 0;
  const nextEmail = () => `relay-member-${++emailSequence}@example.com`;

  async function claim(inviteCode, displayName, accountEmail = nextEmail()) {
    const result = await api("/v1/enrollments/claim", {
      method: "POST",
      body: {
        invite_code: inviteCode,
        email: accountEmail,
        display_name: displayName,
        device_name: `${displayName} test device`,
      },
    });
    assert.equal(result.status, 201);
    return result.value;
  }

  async function joinTeam(joinPassword, displayName, accountEmail = nextEmail()) {
    const result = await api("/v1/enrollments/join", {
      method: "POST",
      body: {
        join_password: joinPassword,
        email: accountEmail,
        display_name: displayName,
        device_name: `${displayName} test device`,
      },
    });
    assert.equal(result.status, 201);
    return result.value;
  }

  async function action(member, threadId, kind, expectedRevision, options = {}) {
    return api(`/v1/threads/${threadId}/actions`, {
      method: "POST",
      token: member.device_token,
      headers: options.headers || {},
      body: {
        kind,
        message: options.message || "",
        attachment_ids: options.attachmentIds || [],
        expected_revision: expectedRevision,
        idempotency_key: options.idempotencyKey || randomUUID(),
        ...(options.delivery ? { delivery: options.delivery } : {}),
        ...(options.recipientId ? { recipient_id: options.recipientId } : {}),
      },
    });
  }

  async function upload(member, teamId, content, filename, uploadId = randomUUID()) {
    const form = new FormData();
    form.append("team_id", teamId);
    form.append("file", new Blob([content]), filename);
    const response = await fetch(`${origin}/v1/attachments`, {
      method: "POST",
      headers: {
        Authorization: `Bearer ${member.device_token}`,
        "Idempotency-Key": uploadId,
      },
      body: form,
    });
    return { status: response.status, value: await response.json(), uploadId };
  }

  async function uploadAvatar(member, content, filename) {
    const form = new FormData();
    form.append("file", new Blob([content]), filename);
    const response = await fetch(`${origin}/v1/me/avatar`, {
      method: "PUT",
      headers: { Authorization: `Bearer ${member.device_token}` },
      body: form,
    });
    return { status: response.status, value: await response.json() };
  }

  async function uploadTruncatedAvatar(member) {
    const boundary = "pinvou-truncated-avatar-boundary";
    const response = await fetch(`${origin}/v1/me/avatar`, {
      method: "PUT",
      headers: {
        Authorization: `Bearer ${member.device_token}`,
        "Content-Type": `multipart/form-data; boundary=${boundary}`,
      },
      body: Buffer.from(
        `--${boundary}\r\nContent-Disposition: form-data; name="file"; filename="avatar.gif"\r\nContent-Type: image/gif\r\n\r\ntruncated`,
      ),
    });
    return { status: response.status, value: await response.json() };
  }

  async function readAvatar(member, principalId, teamId, size, reducedMotion = false) {
    const response = await fetch(
      `${origin}/v1/principals/${principalId}/avatar?team_id=${teamId}${size ? `&size=${size}` : ""}${reducedMotion ? "&reduced_motion=true" : ""}`,
      { headers: { Authorization: `Bearer ${member.device_token}` } },
    );
    return {
      status: response.status,
      contentType: response.headers.get("content-type"),
      digest: response.headers.get("digest"),
      sourceSha256: response.headers.get("x-avatar-source-sha256"),
      bytes: Buffer.from(await response.arrayBuffer()),
    };
  }

  async function readSseUntil(reader, pattern) {
    const decoder = new TextDecoder();
    let buffered = "";
    for (let index = 0; index < 10; index += 1) {
      let timeout;
      const chunk = await Promise.race([
        reader.read(),
        new Promise((_, reject) => {
          timeout = setTimeout(() => reject(new Error(`timed out waiting for SSE ${pattern}`)), 2000);
        }),
      ]).finally(() => clearTimeout(timeout));
      if (chunk.done) break;
      buffered += decoder.decode(chunk.value, { stream: true });
      if (buffered.includes(pattern)) return buffered;
    }
    throw new Error(`SSE stream ended before ${pattern}`);
  }

  async function waitForSseEnd(reader) {
    let timeout;
    const chunk = await Promise.race([
      reader.read(),
      new Promise((_, reject) => {
        timeout = setTimeout(() => reject(new Error("timed out waiting for SSE stream to close")), 2000);
      }),
    ]).finally(() => clearTimeout(timeout));
    assert.equal(chunk.done, true, "revoked memberships must close existing event streams");
  }

  async function assertStillPending(promise, message) {
    const state = await Promise.race([
      promise.then(() => "settled", () => "settled"),
      new Promise(resolve => setTimeout(() => resolve("pending"), 250)),
    ]);
    assert.equal(state, "pending", message);
  }

  async function removeMemberWhileOperationWaits(teamId, principalId, operation, message) {
    const guard = await pool.connect();
    try {
      await guard.query("BEGIN");
      await guard.query("SELECT id FROM teams WHERE id = $1 FOR UPDATE", [teamId]);
      const pendingOperation = operation();
      await assertStillPending(pendingOperation, message);
      await guard.query(
        "UPDATE memberships SET removed_at = clock_timestamp() WHERE team_id = $1 AND principal_id = $2",
        [teamId, principalId],
      );
      await guard.query("COMMIT");
      return await pendingOperation;
    } finally {
      await guard.query("ROLLBACK").catch(() => {});
      guard.release();
    }
  }

  async function assertInviteOperationLocksTeamFirst(teamId, inviteId, operation, message) {
    const teamGuard = await pool.connect();
    const inviteInspector = await pool.connect();
    let pendingOperation;
    let inviteLockError = null;
    try {
      await teamGuard.query("BEGIN");
      await teamGuard.query("SELECT id FROM teams WHERE id = $1 FOR UPDATE", [teamId]);
      pendingOperation = operation();
      await assertStillPending(pendingOperation, message);

      await inviteInspector.query("BEGIN");
      try {
        await inviteInspector.query("SELECT id FROM invites WHERE id = $1 FOR UPDATE NOWAIT", [inviteId]);
      } catch (error) {
        inviteLockError = error;
      } finally {
        await inviteInspector.query("ROLLBACK").catch(() => {});
      }

      await teamGuard.query("COMMIT");
      const result = await pendingOperation;
      assert.equal(inviteLockError, null, "an invite operation waiting on its team must not lock the invite first");
      return result;
    } finally {
      await teamGuard.query("ROLLBACK").catch(() => {});
      teamGuard.release();
      inviteInspector.release();
      if (pendingOperation) await pendingOperation.catch(() => {});
    }
  }

  const health = await api("/v1/healthz");
  assert.deepEqual(health, { status: 200, value: { status: "ok", database: "ready" } });

  const rejectedBetaAccess = await api("/v1/beta-access/verify", { method: "POST", body: { access_code: "incorrect" } });
  assert.equal(rejectedBetaAccess.status, 403);
  assert.equal(rejectedBetaAccess.value.error.code, "beta_access_invalid");
  const acceptedBetaAccess = await api("/v1/beta-access/verify", { method: "POST", body: { access_code: "test-only-access-code" } });
  assert.deepEqual(acceptedBetaAccess, { status: 200, value: { authorized: true } });

  const accountEnrollment = await api("/v1/enrollments/account", { method: "POST", body: { email: "account@example.com", display_name: "Account member", device_name: "First device" } });
  assert.equal(accountEnrollment.status, 201);
  const accountTeams = await api("/v1/teams", { token: accountEnrollment.value.device_token });
  assert.deepEqual(accountTeams.value.teams, []);
  const repeatedAccountEnrollment = await api("/v1/enrollments/account", { method: "POST", body: { email: " ACCOUNT@example.com ", display_name: "Updated account member", device_name: "Second device" } });
  assert.equal(repeatedAccountEnrollment.status, 201);
  assert.equal(repeatedAccountEnrollment.value.principal.id, accountEnrollment.value.principal.id);
  assert.notEqual(repeatedAccountEnrollment.value.device_token, accountEnrollment.value.device_token);
  const repeatedAccountMe = await api("/v1/me", { token: repeatedAccountEnrollment.value.device_token });
  assert.equal(repeatedAccountMe.value.principal.display_name, "Updated account member");
  const accountCount = await pool.query("SELECT count(*)::integer AS count FROM principals WHERE email = 'account@example.com'");
  assert.equal(accountCount.rows[0].count, 1);

  const selfService = await api("/v1/enrollments/create-team", { method: "POST", body: { team_name: "Self-service team", email: "starter@example.com", display_name: "Starter", device_name: "Starter device" } });
  assert.equal(selfService.status, 201);
  assert.equal(selfService.value.membership.role, "owner");
  const legacyEnrollment = await api("/v1/enrollments/create-team", { method: "POST", body: { team_name: "Legacy team", display_name: "Legacy member" } });
  assert.equal(legacyEnrollment.status, 201);
  assert.equal(legacyEnrollment.value.principal.email, null);
  const boundLegacyEmail = await api("/v1/me", { method: "PATCH", token: legacyEnrollment.value.device_token, body: { email: "legacy@example.com", display_name: "Legacy member" } });
  assert.equal(boundLegacyEmail.status, 200);
  assert.equal(boundLegacyEmail.value.principal.email, "legacy@example.com");
  const selfServiceTeams = await api("/v1/teams", { token: selfService.value.device_token });
  assert.deepEqual(selfServiceTeams.value.teams.map(team => team.name), ["Self-service team"]);

  const initial = await createTeamWithOwnerInvite(pool, { name: "Demo team" });
  const a = await joinTeam(initial.invite_code, "A");
  const duplicateClaim = await api("/v1/enrollments/claim", {
    method: "POST",
    body: {
      invite_code: initial.invite_code,
      display_name: "Other A",
      device_name: "duplicate device",
    },
  });
  assert.equal(duplicateClaim.status, 409);
  assert.equal(duplicateClaim.value.error.code, "invite_already_claimed");

  assert.equal(a.team.join_password_configured, false);
  const firstPassword = await api(`/v1/teams/${initial.team_id}/join-password`, {
    method: "POST",
    token: a.device_token,
    body: {},
  });
  assert.equal(firstPassword.status, 200);
  assert.equal(typeof firstPassword.value.join_password, "string");
  assert.match(firstPassword.value.join_password, /^[23456789ABCDEFGHJKMNPQRSTVWXYZ]{4}-[23456789ABCDEFGHJKMNPQRSTVWXYZ]{4}$/);
  assert.equal(firstPassword.value.configured, true);
  assert.equal(firstPassword.value.visibility, "members");
  const firstPasswordRead = await api(`/v1/teams/${initial.team_id}/join-password`, {
    token: a.device_token,
  });
  assert.equal(firstPasswordRead.status, 200);
  assert.equal(firstPasswordRead.value.join_password, firstPassword.value.join_password);
  const wrongPassword = await api("/v1/enrollments/join", {
    method: "POST",
    body: {
      join_password: "wrong-password-value-with-at-least-32-characters",
      display_name: "Wrong",
      device_name: "wrong device",
    },
  });
  assert.equal(wrongPassword.status, 401);
  assert.equal(wrongPassword.value.error.code, "join_password_invalid");
  const secondPassword = await api(`/v1/teams/${initial.team_id}/join-password`, {
    method: "POST",
    token: a.device_token,
    body: {},
  });
  assert.equal(secondPassword.status, 200);
  assert.notEqual(secondPassword.value.join_password, firstPassword.value.join_password);
  const oldPassword = await api("/v1/enrollments/join", {
    method: "POST",
    body: {
      join_password: firstPassword.value.join_password,
      display_name: "Old",
      device_name: "old password device",
    },
  });
  assert.equal(oldPassword.status, 401);
  assert.equal(oldPassword.value.error.code, "join_password_invalid");
  const b = await joinTeam(secondPassword.value.join_password, "B");
  const c = await joinTeam(secondPassword.value.join_password.toLowerCase().replace("-", ""), "C");
  assert.equal(b.team.join_password_configured, true);
  const memberPasswordRead = await api(`/v1/teams/${initial.team_id}/join-password`, {
    token: b.device_token,
  });
  assert.equal(memberPasswordRead.status, 200);
  assert.equal(memberPasswordRead.value.join_password, secondPassword.value.join_password);
  const ownerOnlyPassword = await api(`/v1/teams/${initial.team_id}/join-password`, {
    method: "PATCH",
    token: a.device_token,
    body: { visibility: "owners" },
  });
  assert.equal(ownerOnlyPassword.status, 200);
  assert.equal(ownerOnlyPassword.value.visibility, "owners");
  assert.equal(ownerOnlyPassword.value.join_password, secondPassword.value.join_password);
  const hiddenFromMember = await api(`/v1/teams/${initial.team_id}/join-password`, {
    token: b.device_token,
  });
  assert.deepEqual(hiddenFromMember.value, {
    configured: true,
    visible: false,
    recoverable: true,
    visibility: "owners",
    join_password: null,
    updated_at: secondPassword.value.updated_at,
  });
  const memberCannotChangeVisibility = await api(`/v1/teams/${initial.team_id}/join-password`, {
    method: "PATCH",
    token: b.device_token,
    body: { visibility: "members" },
  });
  assert.equal(memberCannotChangeVisibility.status, 403);
  const memberVisiblePassword = await api(`/v1/teams/${initial.team_id}/join-password`, {
    method: "PATCH",
    token: a.device_token,
    body: { visibility: "members" },
  });
  assert.equal(memberVisiblePassword.status, 200);
  assert.equal(memberVisiblePassword.value.visibility, "members");
  const disabledPassword = await api(`/v1/teams/${initial.team_id}/join-password`, {
    method: "DELETE",
    token: a.device_token,
  });
  assert.equal(disabledPassword.status, 200);
  assert.equal(disabledPassword.value.configured, false);
  const disabledJoin = await api("/v1/enrollments/join", {
    method: "POST",
    headers: { "X-Real-IP": "203.0.113.30" },
    body: {
      join_password: secondPassword.value.join_password,
      display_name: "Paused",
      device_name: "paused join device",
    },
  });
  assert.equal(disabledJoin.status, 401);
  const activePassword = await api(`/v1/teams/${initial.team_id}/join-password`, {
    method: "POST",
    token: a.device_token,
    body: {},
  });
  assert.equal(activePassword.status, 200);
  assert.notEqual(activePassword.value.join_password, secondPassword.value.join_password);
  const ownerRejoin = await api("/v1/teams/join", {
    method: "POST",
    token: a.device_token,
    body: { join_code: activePassword.value.join_password },
  });
  assert.equal(ownerRejoin.status, 200);
  assert.equal(ownerRejoin.value.membership.role, "owner", "an active owner must not be demoted by rejoining");
  const nonOwnerRotation = await api(`/v1/teams/${initial.team_id}/join-password`, {
    method: "POST",
    token: b.device_token,
    body: {},
  });
  assert.equal(nonOwnerRotation.status, 403);
  const duplicateEmail = await api("/v1/enrollments/join", {
    method: "POST",
    body: {
      join_password: activePassword.value.join_password,
      email: b.principal.email.toUpperCase(),
      display_name: "Other B",
    },
  });
  assert.equal(duplicateEmail.status, 409);
  assert.equal(duplicateEmail.value.error.code, "email_already_registered");
  const renamedB = await api("/v1/me", {
    method: "PATCH",
    token: b.device_token,
    body: { display_name: "A" },
  });
  assert.equal(renamedB.status, 200);
  assert.equal(renamedB.value.principal.display_name, "A", "display names may be duplicated");
  const members = await api(`/v1/teams/${initial.team_id}/members`, { token: a.device_token });
  assert.equal(members.status, 200);
  assert.deepEqual(members.value.members.map((member) => member.display_name), ["A", "A", "C"]);
  assert.equal(members.value.members[1].email, undefined, "raw member email must stay private");
  assert.match(members.value.members[1].email_hint, /^r\*\*\*@example\.com$/);
  const restoredB = await api("/v1/me", {
    method: "PATCH",
    token: b.device_token,
    body: { display_name: "B" },
  });
  assert.equal(restoredB.status, 200);

  const animatedPixels = Buffer.from([
    255, 0, 0, 255, 255, 0, 0, 255, 255, 0, 0, 255, 255, 0, 0, 255,
    0, 0, 255, 255, 0, 0, 255, 255, 0, 0, 255, 255, 0, 0, 255, 255,
  ]);
  const animatedGif = await sharp(animatedPixels, {
    raw: { width: 2, height: 4, channels: 4, pageHeight: 2 },
  }).gif({ delay: [80, 120], loop: 0 }).toBuffer();
  const truncatedAvatar = await uploadTruncatedAvatar(a);
  assert.equal(truncatedAvatar.status, 400);
  assert.equal(truncatedAvatar.value.error.code, "invalid_request");
  const uploadedAvatar = await uploadAvatar(a, animatedGif, "avatar.gif");
  assert.equal(uploadedAvatar.status, 200);
  assert.equal(uploadedAvatar.value.avatar.content_type, "image/gif");
  assert.ok(uploadedAvatar.value.avatar.byte_size <= config.maxAvatarBytes);
  const avatarBootstrap = await api(`/v1/bootstrap?team_id=${initial.team_id}`, { token: b.device_token });
  const avatarMember = avatarBootstrap.value.members.find(member => member.id === a.principal.id);
  assert.equal(avatarMember.avatar.sha256, uploadedAvatar.value.avatar.sha256);
  const downloadedAvatar = await readAvatar(b, a.principal.id, initial.team_id);
  assert.equal(downloadedAvatar.status, 200);
  assert.equal(downloadedAvatar.contentType, "image/gif");
  assert.equal(downloadedAvatar.sourceSha256, uploadedAvatar.value.avatar.sha256);
  assert.equal((await sharp(downloadedAvatar.bytes, { animated: true }).metadata()).pages, 2);
  const thumbnailAvatar = await readAvatar(b, a.principal.id, initial.team_id, 48);
  assert.equal(thumbnailAvatar.status, 200);
  assert.equal(thumbnailAvatar.contentType, "image/gif");
  assert.equal(thumbnailAvatar.sourceSha256, uploadedAvatar.value.avatar.sha256);
  assert.equal((await sharp(thumbnailAvatar.bytes, { animated: true }).metadata()).pages, 2);
  const reducedMotionAvatar = await readAvatar(b, a.principal.id, initial.team_id, 48, true);
  assert.equal(reducedMotionAvatar.status, 200);
  assert.equal(reducedMotionAvatar.contentType, "image/png");
  assert.equal(reducedMotionAvatar.sourceSha256, uploadedAvatar.value.avatar.sha256);
  assert.equal((await sharp(reducedMotionAvatar.bytes).metadata()).pages ?? 1, 1);
  const invalidAvatarSize = await readAvatar(b, a.principal.id, initial.team_id, 64);
  assert.equal(invalidAvatarSize.status, 400);
  const outsideTeamAvatar = await readAvatar(accountEnrollment.value, a.principal.id, initial.team_id);
  assert.equal(outsideTeamAvatar.status, 403, "avatars are visible only inside a shared active team");
  const rejectedAvatar = await uploadAvatar(a, Buffer.from("<svg></svg>"), "avatar.svg");
  assert.equal(rejectedAvatar.status, 415);
  assert.equal(rejectedAvatar.value.error.code, "avatar_type_not_allowed");
  const removedAvatar = await api("/v1/me/avatar", { method: "DELETE", token: a.device_token });
  assert.equal(removedAvatar.status, 204);
  const missingAvatar = await readAvatar(b, a.principal.id, initial.team_id);
  assert.equal(missingAvatar.status, 404);

  const createdTeam = await api("/v1/teams", { method: "POST", token: a.device_token, body: { name: "Release group" } });
  assert.equal(createdTeam.status, 201);
  assert.equal(createdTeam.value.membership.principal_id, a.principal.id);
  assert.equal(createdTeam.value.membership.role, "owner");
  const aTeams = await api("/v1/teams", { token: a.device_token });
  assert.equal(aTeams.status, 200);
  assert.deepEqual(aTeams.value.teams.map(team => team.name), ["Release group", "Demo team"]);
  const secondaryInvite = await api(`/v1/teams/${createdTeam.value.team.id}/invites`, { method: "POST", token: a.device_token, body: { expires_in_hours: 24 } });
  assert.equal(secondaryInvite.status, 201);
  const joinedSecondTeam = await api("/v1/teams/join", { method: "POST", token: b.device_token, body: { join_code: secondaryInvite.value.invite_code } });
  assert.equal(joinedSecondTeam.status, 200);
  assert.equal(joinedSecondTeam.value.membership.principal_id, b.principal.id, "joining a second team must reuse the principal");
  const inviteFromMember = await api(`/v1/teams/${createdTeam.value.team.id}/invites`, { method: "POST", token: b.device_token, body: { expires_in_hours: 24 } });
  assert.equal(inviteFromMember.status, 201, "temporary teams allow members to invite collaborators");
  const d = await joinTeam(inviteFromMember.value.invite_code, "D");
  assert.equal(d.team.created_by, a.principal.id);
  const secondBootstrap = await api(`/v1/bootstrap?team_id=${createdTeam.value.team.id}`, { token: b.device_token });
  assert.equal(secondBootstrap.status, 200);
  assert.deepEqual(secondBootstrap.value.members.map(member => member.display_name), ["A", "B", "D"]);

  const lockOrderAccount = await api("/v1/enrollments/account", {
    method: "POST",
    body: { email: nextEmail(), display_name: "Lock order member", device_name: "Lock order test device" },
  });
  assert.equal(lockOrderAccount.status, 201);
  const lockOrderInvite = await api(`/v1/teams/${createdTeam.value.team.id}/invites`, {
    method: "POST",
    token: a.device_token,
    body: { expires_in_hours: 24 },
  });
  assert.equal(lockOrderInvite.status, 201);
  const lockOrderedJoin = await assertInviteOperationLocksTeamFirst(
    createdTeam.value.team.id,
    lockOrderInvite.value.invite_id,
    () => api("/v1/teams/join", {
      method: "POST",
      token: lockOrderAccount.value.device_token,
      body: { join_code: lockOrderInvite.value.invite_code },
    }),
    "an invite join must wait for the team lock before locking its invite",
  );
  assert.equal(lockOrderedJoin.status, 200);
  const removeLockOrderMember = await api(
    `/v1/teams/${createdTeam.value.team.id}/members/${lockOrderAccount.value.principal.id}`,
    { method: "DELETE", token: a.device_token },
  );
  assert.equal(removeLockOrderMember.status, 204);

  const inviteCountBeforeRemovalRace = await pool.query(
    "SELECT count(*)::integer AS count FROM invites WHERE team_id = $1",
    [createdTeam.value.team.id],
  );
  const rejectedInvite = await removeMemberWhileOperationWaits(
    createdTeam.value.team.id,
    d.principal.id,
    () => api(`/v1/teams/${createdTeam.value.team.id}/invites`, {
      method: "POST",
      token: d.device_token,
      body: { expires_in_hours: 24 },
    }),
    "invite creation must wait while team membership changes are locked",
  );
  assert.equal(rejectedInvite.status, 403);
  assert.equal(rejectedInvite.value.error.code, "membership_required");
  const inviteCountAfterRemovalRace = await pool.query(
    "SELECT count(*)::integer AS count FROM invites WHERE team_id = $1",
    [createdTeam.value.team.id],
  );
  assert.equal(inviteCountAfterRemovalRace.rows[0].count, inviteCountBeforeRemovalRace.rows[0].count);

  await pool.query(
    "UPDATE memberships SET removed_at = NULL, role = 'member' WHERE team_id = $1 AND principal_id = $2",
    [createdTeam.value.team.id, d.principal.id],
  );
  await pool.query(
    "UPDATE memberships SET role = 'owner' WHERE team_id = $1 AND principal_id = $2",
    [createdTeam.value.team.id, b.principal.id],
  );
  const joinPasswordBeforeRotationRace = await pool.query(
    "SELECT join_password_hash FROM teams WHERE id = $1",
    [createdTeam.value.team.id],
  );
  const rejectedRotation = await removeMemberWhileOperationWaits(
    createdTeam.value.team.id,
    b.principal.id,
    () => api(`/v1/teams/${createdTeam.value.team.id}/join-password`, {
      method: "POST",
      token: b.device_token,
      body: {},
    }),
    "join code rotation must wait while team membership changes are locked",
  );
  assert.equal(rejectedRotation.status, 403);
  assert.equal(rejectedRotation.value.error.code, "membership_required");
  const joinPasswordAfterRotationRace = await pool.query(
    "SELECT join_password_hash FROM teams WHERE id = $1",
    [createdTeam.value.team.id],
  );
  assert.equal(
    joinPasswordAfterRotationRace.rows[0].join_password_hash,
    joinPasswordBeforeRotationRace.rows[0].join_password_hash,
  );
  await pool.query(
    "UPDATE memberships SET removed_at = NULL, role = 'member' WHERE team_id = $1 AND principal_id = $2",
    [createdTeam.value.team.id, b.principal.id],
  );

  const rejectedJoinPasswordRead = await removeMemberWhileOperationWaits(
    createdTeam.value.team.id,
    d.principal.id,
    () => api(`/v1/teams/${createdTeam.value.team.id}/join-password`, {
      token: d.device_token,
    }),
    "join code reads must wait while team membership changes are locked",
  );
  assert.equal(rejectedJoinPasswordRead.status, 403);
  assert.equal(rejectedJoinPasswordRead.value.error.code, "membership_required");
  await pool.query(
    "UPDATE memberships SET removed_at = NULL, role = 'member' WHERE team_id = $1 AND principal_id = $2",
    [createdTeam.value.team.id, d.principal.id],
  );

  const attachmentCountBeforeRemovalRace = await pool.query(
    "SELECT count(*)::integer AS count FROM attachments WHERE team_id = $1",
    [createdTeam.value.team.id],
  );
  const rejectedUpload = await removeMemberWhileOperationWaits(
    createdTeam.value.team.id,
    d.principal.id,
    () => upload(
      d,
      createdTeam.value.team.id,
      "must not survive membership removal",
      "removed-member.txt",
    ),
    "attachment upload must wait while team membership changes are locked",
  );
  assert.equal(rejectedUpload.status, 403);
  assert.equal(rejectedUpload.value.error.code, "membership_required");
  const attachmentCountAfterRemovalRace = await pool.query(
    "SELECT count(*)::integer AS count FROM attachments WHERE team_id = $1",
    [createdTeam.value.team.id],
  );
  assert.equal(
    attachmentCountAfterRemovalRace.rows[0].count,
    attachmentCountBeforeRemovalRace.rows[0].count,
  );
  await pool.query(
    "UPDATE memberships SET removed_at = NULL, role = 'member' WHERE team_id = $1 AND principal_id = $2",
    [createdTeam.value.team.id, d.principal.id],
  );

  const raceThread = await api("/v1/threads", {
    method: "POST",
    token: a.device_token,
    body: {
      team_id: createdTeam.value.team.id,
      title: "Membership race guard",
      assignee_id: b.principal.id,
      message: "Verify membership serialization.",
      attachment_ids: [],
      idempotency_key: randomUUID(),
    },
  });
  assert.equal(raceThread.status, 201);

  const handoffGuard = await pool.connect();
  try {
    await handoffGuard.query("BEGIN");
    await handoffGuard.query("SELECT id FROM teams WHERE id = $1 FOR UPDATE", [createdTeam.value.team.id]);
    const concurrentHandoff = action(b, raceThread.value.thread.id, "handoff", 1, {
      recipientId: d.principal.id,
    });
    await assertStillPending(
      concurrentHandoff,
      "handoff must wait while team membership changes are locked",
    );
    await handoffGuard.query(
      "UPDATE memberships SET removed_at = now() WHERE team_id = $1 AND principal_id = $2",
      [createdTeam.value.team.id, d.principal.id],
    );
    await handoffGuard.query("COMMIT");
    const rejectedHandoff = await concurrentHandoff;
    assert.equal(rejectedHandoff.status, 403);
    assert.equal(rejectedHandoff.value.error.code, "membership_required");
  } finally {
    await handoffGuard.query("ROLLBACK").catch(() => {});
    handoffGuard.release();
  }
  const raceThreadAfterRejectedHandoff = await api(`/v1/threads/${raceThread.value.thread.id}`, {
    token: b.device_token,
  });
  assert.equal(raceThreadAfterRejectedHandoff.value.thread.revision, 1);
  assert.equal(raceThreadAfterRejectedHandoff.value.thread.current_actor_id, b.principal.id);

  await pool.query(
    "UPDATE memberships SET removed_at = NULL, role = 'member' WHERE team_id = $1 AND principal_id = $2",
    [createdTeam.value.team.id, d.principal.id],
  );
  const assignmentGuard = await pool.connect();
  try {
    await assignmentGuard.query("BEGIN");
    await assignmentGuard.query("SELECT id FROM teams WHERE id = $1 FOR UPDATE", [createdTeam.value.team.id]);
    const concurrentAssignment = api("/v1/threads", {
      method: "POST",
      token: a.device_token,
      body: {
        team_id: createdTeam.value.team.id,
        title: "Rejected concurrent assignment",
        assignee_id: d.principal.id,
        message: "This must not race member removal.",
        attachment_ids: [],
        idempotency_key: randomUUID(),
      },
    });
    await assertStillPending(
      concurrentAssignment,
      "thread assignment must wait while team membership changes are locked",
    );
    await assignmentGuard.query(
      "UPDATE memberships SET removed_at = now() WHERE team_id = $1 AND principal_id = $2",
      [createdTeam.value.team.id, d.principal.id],
    );
    await assignmentGuard.query("COMMIT");
    const rejectedAssignment = await concurrentAssignment;
    assert.equal(rejectedAssignment.status, 403);
    assert.equal(rejectedAssignment.value.error.code, "membership_required");
  } finally {
    await assignmentGuard.query("ROLLBACK").catch(() => {});
    assignmentGuard.release();
  }

  await pool.query(
    "UPDATE memberships SET removed_at = NULL, role = 'member' WHERE team_id = $1 AND principal_id = $2",
    [createdTeam.value.team.id, d.principal.id],
  );
  const removalGuard = await pool.connect();
  try {
    await removalGuard.query("BEGIN");
    await removalGuard.query("SELECT id FROM teams WHERE id = $1 FOR SHARE", [createdTeam.value.team.id]);
    const concurrentRemoval = api(
      `/v1/teams/${createdTeam.value.team.id}/members/${d.principal.id}`,
      { method: "DELETE", token: a.device_token },
    );
    await assertStillPending(
      concurrentRemoval,
      "member removal must wait for in-flight assignment guards",
    );
    await removalGuard.query("COMMIT");
    const completedRemoval = await concurrentRemoval;
    assert.equal(completedRemoval.status, 204);
  } finally {
    await removalGuard.query("ROLLBACK").catch(() => {});
    removalGuard.release();
  }

  const planText = "# Team plan\n\nInitial material.\n";
  const planUploadKey = randomUUID();
  const planUpload = await upload(a, initial.team_id, planText, "plan.md", planUploadKey);
  assert.equal(planUpload.status, 201, JSON.stringify(planUpload.value));
  assert.equal(planUpload.value.content_type, "text/markdown; charset=utf-8");
  assert.equal(planUpload.value.bound, false);
  assert.equal(planUpload.value.available, true);

  const uploadReplay = await upload(a, initial.team_id, planText, "plan.md", planUploadKey);
  assert.equal(uploadReplay.status, 200);
  assert.equal(uploadReplay.value.id, planUpload.value.id);
  const uploadConflict = await upload(a, initial.team_id, "different", "plan.md", planUploadKey);
  assert.equal(uploadConflict.status, 409);
  assert.equal(uploadConflict.value.error.code, "idempotency_key_reused");
  const concurrentUploadKey = randomUUID();
  const concurrentUploads = await Promise.all([
    upload(a, initial.team_id, "same content", "concurrent.txt", concurrentUploadKey),
    upload(a, initial.team_id, "same content", "concurrent.txt", concurrentUploadKey),
  ]);
  assert.deepEqual(concurrentUploads.map((item) => item.status).sort(), [200, 201]);
  assert.equal(concurrentUploads[0].value.id, concurrentUploads[1].value.id);

  const unboundOwner = await api(`/v1/attachments/${planUpload.value.id}`, {
    token: a.device_token,
  });
  assert.equal(unboundOwner.status, 200);
  const unboundOtherMember = await api(`/v1/attachments/${planUpload.value.id}`, {
    token: b.device_token,
  });
  assert.equal(unboundOtherMember.status, 403);

  const disguisedExecutable = await upload(
    a,
    initial.team_id,
    new Uint8Array([0x7f, 0x45, 0x4c, 0x46, 0, 1, 2, 3]),
    "report.txt",
  );
  assert.equal(disguisedExecutable.status, 415);
  assert.equal(disguisedExecutable.value.error.code, "attachment_type_not_allowed");
  const htmlSnapshot = await upload(a, initial.team_id, "<!doctype html><title>Relay preview</title>", "index.html");
  assert.equal(htmlSnapshot.status, 201);
  assert.equal(htmlSnapshot.value.content_type, "text/plain; charset=utf-8");
  const pathFilename = await upload(a, initial.team_id, "safe text", "../escape.txt");
  assert.equal(pathFilename.status, 400);
  assert.equal(pathFilename.value.error.code, "invalid_request");
  const onePixelPng = Buffer.from(
    "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=",
    "base64",
  );
  const imageUpload = await upload(a, initial.team_id, onePixelPng, "pixel.png");
  assert.equal(imageUpload.status, 201);
  assert.equal(imageUpload.value.content_type, "image/png");
  const ownerOnlyUpload = await upload(a, initial.team_id, "owner draft", "owner-only.md");
  assert.equal(ownerOnlyUpload.status, 201);
  const oversized = await upload(a, initial.team_id, "x".repeat(2049), "oversized.txt");
  assert.equal(oversized.status, 413);
  assert.equal(oversized.value.error.code, "attachment_too_large");

  const largeOrphan = await upload(a, initial.team_id, "x".repeat(1200), "research.txt");
  assert.equal(largeOrphan.status, 201);
  const teamCapacity = await upload(a, initial.team_id, "x".repeat(500), "more.txt");
  assert.equal(teamCapacity.status, 413);
  assert.equal(teamCapacity.value.error.code, "team_attachment_capacity_exceeded");
  config.maxTeamAttachmentBytes = 5000;
  const exactLimit = await upload(a, initial.team_id, "x".repeat(2048), "exact-limit.txt");
  assert.equal(exactLimit.status, 201);
  config.maxTeamAttachmentBytes = 1600;

  const createKey = randomUUID();
  const createBody = {
    team_id: initial.team_id,
    title: "Prepare team writing plan",
    assignee_id: b.principal.id,
    message: "Please prepare the first version.",
    attachment_ids: [planUpload.value.id],
    delivery: {
      schema_version: 1,
      artifact_role: "input",
      expected_next: "Draft the plan and return it.",
      open_questions: "Confirm the second example.",
      git_references: [{
        repository_url: "https://github.com/pinvou/example",
        commit_sha: "abcdef1234567",
        branch: "main",
        pull_request_url: "https://github.com/pinvou/example/pull/1",
        paths: ["docs/plan.md"],
        note: "Starting point",
      }],
    },
    idempotency_key: createKey,
  };
  const created = await api("/v1/threads", {
    method: "POST",
    token: a.device_token,
    body: createBody,
  });
  assert.equal(created.status, 201);
  assert.equal(created.value.thread.revision, 1);
  assert.equal(created.value.thread.owner_id, b.principal.id);
  assert.equal(created.value.replayed, false);
  assert.deepEqual(created.value.action.attachment_ids, [planUpload.value.id]);
  assert.equal(created.value.action.delivery.artifact_role, "input");
  assert.equal(created.value.action.delivery.git_references[0].commit_sha, "abcdef1234567");
  const threadId = created.value.thread.id;

  const aInitialReadState = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: a.device_token,
  });
  const bInitialReadState = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: b.device_token,
  });
  assert.equal(aInitialReadState.value.threads[0].unread_count, 0, "the assigning member must not notify themselves");
  assert.equal(aInitialReadState.value.threads[0].read_state.last_read_sequence, 1);
  assert.equal(bInitialReadState.value.threads[0].unread_count, 1, "a new assignment is the recipient's first unread update");
  assert.equal(bInitialReadState.value.threads[0].read_state.last_read_sequence, 0);

  const bMarkedRead = await api(`/v1/threads/${threadId}/read`, {
    method: "PUT",
    token: b.device_token,
    body: { read_through_sequence: 1 },
  });
  assert.equal(bMarkedRead.status, 200);
  assert.equal(bMarkedRead.value.last_read_sequence, 1);
  const bAfterRead = await api(`/v1/bootstrap?team_id=${initial.team_id}`, { token: b.device_token });
  assert.equal(bAfterRead.value.threads[0].unread_count, 0);
  const futureRead = await api(`/v1/threads/${threadId}/read`, {
    method: "PUT",
    token: b.device_token,
    body: { read_through_sequence: 2 },
  });
  assert.equal(futureRead.status, 400);
  assert.equal(futureRead.value.error.code, "invalid_request");
  const nonParticipantRead = await api(`/v1/threads/${threadId}/read`, {
    method: "PUT",
    token: c.device_token,
    body: { read_through_sequence: 1 },
  });
  assert.equal(nonParticipantRead.status, 403);
  assert.equal(nonParticipantRead.value.error.code, "thread_access_denied");

  const bDownload = await fetch(`${origin}/v1/attachments/${planUpload.value.id}/content`, {
    headers: { Authorization: `Bearer ${b.device_token}` },
  });
  assert.equal(bDownload.status, 200);
  assert.equal(await bDownload.text(), planText);
  assert.match(bDownload.headers.get("digest"), /^sha-256=/);
  const cBeforeHandoff = await api(`/v1/attachments/${planUpload.value.id}`, {
    token: c.device_token,
  });
  assert.equal(cBeforeHandoff.status, 403);

  const bindAnotherMembersFile = await action(b, threadId, "note", 1, {
    message: "cannot bind A's private upload",
    attachmentIds: [ownerOnlyUpload.value.id],
  });
  assert.equal(bindAnotherMembersFile.status, 422);
  assert.equal(bindAnotherMembersFile.value.error.code, "attachment_not_ready");
  const unchangedAfterRejectedBinding = await api(`/v1/threads/${threadId}`, {
    token: b.device_token,
  });
  assert.equal(unchangedAfterRejectedBinding.value.thread.revision, 1);

  const createReplay = await api("/v1/threads", {
    method: "POST",
    token: a.device_token,
    body: createBody,
  });
  assert.equal(createReplay.status, 200);
  assert.equal(createReplay.value.action.id, created.value.action.id);
  assert.equal(createReplay.value.replayed, true);

  const removeActiveOwner = await api(
    `/v1/teams/${initial.team_id}/members/${b.principal.id}`,
    { method: "DELETE", token: a.device_token },
  );
  assert.equal(removeActiveOwner.status, 409);
  assert.equal(removeActiveOwner.value.error.code, "member_has_open_work");
  assert.deepEqual(removeActiveOwner.value.error.details.blocking_threads, [{
    thread_id: threadId,
    title: "Prepare team writing plan",
    phase: "active",
    responsibilities: ["work_owner", "current_actor"],
  }]);
  const ownerTransferWithOpenWork = await api(`/v1/teams/${initial.team_id}/owner-transfer`, {
    method: "POST",
    token: a.device_token,
    body: { new_owner_id: c.principal.id },
  });
  assert.equal(ownerTransferWithOpenWork.status, 200, "ownership transfer must not alter or block existing work responsibilities");
  const restoreOwner = await api(`/v1/teams/${initial.team_id}/owner-transfer`, {
    method: "POST",
    token: c.device_token,
    body: { new_owner_id: a.principal.id },
  });
  assert.equal(restoreOwner.status, 200);
  await pool.query("UPDATE principals SET disabled_at = clock_timestamp() WHERE id = $1", [c.principal.id]);
  const disabledOwnerTransfer = await api(`/v1/teams/${initial.team_id}/owner-transfer`, {
    method: "POST",
    token: a.device_token,
    body: { new_owner_id: c.principal.id },
  });
  assert.equal(disabledOwnerTransfer.status, 404);
  assert.equal(disabledOwnerTransfer.value.error.code, "resource_not_found");
  await pool.query("UPDATE principals SET disabled_at = NULL WHERE id = $1", [c.principal.id]);
  const blockedTeamDissolution = await api(`/v1/teams/${initial.team_id}`, {
    method: "DELETE",
    token: a.device_token,
  });
  assert.equal(blockedTeamDissolution.status, 409);
  assert.equal(blockedTeamDissolution.value.error.code, "team_has_open_work");
  assert.deepEqual(blockedTeamDissolution.value.error.details.blocking_threads, [{
    thread_id: threadId,
    title: "Prepare team writing plan",
    phase: "active",
  }]);

  const sseController = new AbortController();
  t.after(() => sseController.abort());
  const sse = await fetch(`${origin}/v1/events`, {
    headers: { Authorization: `Bearer ${a.device_token}` },
    signal: sseController.signal,
  });
  assert.equal(sse.status, 200);
  const sseReader = sse.body.getReader();
  await readSseUntil(sseReader, "event: ready");

  const handoffKey = randomUUID();
  const handedOff = await action(b, threadId, "handoff", 1, {
    recipientId: c.principal.id,
    idempotencyKey: handoffKey,
    message: "Please add two cases and return to me.",
  });
  assert.equal(handedOff.status, 201);
  assert.equal(handedOff.value.thread.current_actor_id, c.principal.id);
  assert.equal(handedOff.value.thread.owner_id, b.principal.id);
  const aAfterHandoff = await api(`/v1/bootstrap?team_id=${initial.team_id}`, { token: a.device_token });
  const bAfterOwnHandoff = await api(`/v1/bootstrap?team_id=${initial.team_id}`, { token: b.device_token });
  const cAfterJoiningThread = await api(`/v1/bootstrap?team_id=${initial.team_id}`, { token: c.device_token });
  assert.equal(aAfterHandoff.value.threads[0].unread_count, 1);
  assert.equal(bAfterOwnHandoff.value.threads[0].unread_count, 0, "an actor's own update must not notify that actor");
  assert.equal(cAfterJoiningThread.value.threads[0].unread_count, 1, "a collaborator starts unread at the handoff that added them");
  const bStaleRead = await api(`/v1/threads/${threadId}/read`, {
    method: "PUT",
    token: b.device_token,
    body: { read_through_sequence: 0 },
  });
  assert.equal(bStaleRead.status, 200);
  assert.equal(bStaleRead.value.last_read_sequence, 1, "a stale device must not move the read cursor backwards");
  const cAfterHandoff = await api(`/v1/attachments/${planUpload.value.id}`, {
    token: c.device_token,
  });
  assert.equal(cAfterHandoff.status, 200);
  const changedEvent = await readSseUntil(sseReader, "event: thread.changed");
  assert.match(changedEvent, new RegExp(`"thread_id":"${threadId}"`));
  sseController.abort();

  const nestedHandoff = await action(b, threadId, "handoff", 2, {
    recipientId: a.principal.id,
  });
  assert.equal(nestedHandoff.status, 403);
  assert.equal(nestedHandoff.value.error.code, "action_not_allowed");

  const collaboratorFinal = await action(c, threadId, "final_deliver", 2);
  assert.equal(collaboratorFinal.status, 403);
  assert.equal(collaboratorFinal.value.error.code, "action_not_allowed");

  const returned = await action(c, threadId, "return", 2, { message: "Cases added." });
  assert.equal(returned.status, 201);
  assert.equal(returned.value.thread.current_actor_id, b.principal.id);
  assert.equal(returned.value.thread.return_to_id, a.principal.id);
  assert.equal(returned.value.action.delivery.artifact_role, "intermediate");

  const note = await action(a, threadId, "note", 3, { message: "Keep the comparison concise." });
  assert.equal(note.status, 201);
  assert.equal(note.value.thread.revision, 4);
  const stale = await action(b, threadId, "note", 3, { message: "stale note" });
  assert.equal(stale.status, 409);
  assert.equal(stale.value.error.code, "revision_conflict");
  assert.equal(stale.value.error.details.current_revision, 4);

  const firstDeliveryKey = randomUUID();
  const firstDeliveryPackage = {
    schema_version: 1,
    artifact_role: "final",
    expected_next: "Review this exact revision.",
    open_questions: "",
    git_references: [],
  };
  const delivered = await action(b, threadId, "final_deliver", 4, {
    idempotencyKey: firstDeliveryKey,
    message: "First delivery.",
    delivery: firstDeliveryPackage,
  });
  assert.equal(delivered.status, 201);
  assert.equal(delivered.value.thread.phase, "awaiting_acceptance");
  assert.equal(delivered.value.thread.current_actor_id, a.principal.id);
  assert.equal(delivered.value.action.delivery.expected_next, "Review this exact revision.");

  const changes = await action(a, threadId, "request_changes", 5, {
    message: "Please clarify the second case.",
  });
  assert.equal(changes.status, 201);
  assert.equal(changes.value.thread.current_actor_id, b.principal.id);

  const oldDeliveryReplay = await action(b, threadId, "final_deliver", 4, {
    idempotencyKey: firstDeliveryKey,
    message: "First delivery.",
    delivery: firstDeliveryPackage,
  });
  assert.equal(oldDeliveryReplay.status, 200);
  assert.equal(oldDeliveryReplay.value.replayed, true);
  assert.equal(oldDeliveryReplay.value.action.id, delivered.value.action.id);
  assert.equal(oldDeliveryReplay.value.thread.revision, 6);
  assert.equal(oldDeliveryReplay.value.thread.phase, "active");

  const cBootstrap = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: c.device_token,
  });
  assert.equal(cBootstrap.status, 200);
  assert.equal(cBootstrap.value.threads[0].thread.current_actor_id, b.principal.id);
  assert.deepEqual(
    cBootstrap.value.threads[0].actions.map((item) => item.kind),
    ["assign", "handoff", "return", "note", "final_deliver", "request_changes"],
  );

  const redelivered = await action(b, threadId, "final_deliver", 6, {
    message: "Clarified delivery.",
  });
  assert.equal(redelivered.status, 201);
  const accepted = await action(a, threadId, "accept", 7, { message: "Accepted." });
  assert.equal(accepted.status, 201);
  assert.equal(accepted.value.thread.phase, "closed");
  assert.equal(accepted.value.thread.revision, 8);
  assert.equal(accepted.value.thread.current_actor_id, null);

  const protocol02 = { "X-Pinvou-Team-Relay-Protocol": "0.2" };
  for (const member of [a, b, c]) {
    const capabilityBootstrap = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
      token: member.device_token,
      headers: protocol02,
    });
    assert.equal(capabilityBootstrap.status, 200);
  }
  const capabilityView = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: a.device_token,
    headers: protocol02,
  });
  assert.equal(
    capabilityView.value.members.find(member => member.id === b.principal.id).confirmed_transfers_supported,
    true,
  );
  const legacyRecipient = await joinTeam(activePassword.value.join_password, "Legacy recipient");
  const directForLegacy = await api("/v1/threads", {
    method: "POST",
    token: a.device_token,
    headers: protocol02,
    body: {
      team_id: initial.team_id,
      title: "Mixed version assignment",
      assignee_id: legacyRecipient.principal.id,
      message: "This must not wait for an unsupported client.",
      attachment_ids: [],
      idempotency_key: randomUUID(),
    },
  });
  assert.equal(directForLegacy.status, 201);
  assert.equal(directForLegacy.value.transfer_mode, "direct_legacy");
  assert.equal(directForLegacy.value.action.transfer_mode, "direct_legacy");
  assert.equal(directForLegacy.value.thread.phase, "active");
  assert.equal(directForLegacy.value.thread.owner_id, legacyRecipient.principal.id);
  const cancelDirectLegacy = await action(a, directForLegacy.value.thread.id, "cancel", 1, {
    headers: protocol02,
    message: "Mixed version behavior verified.",
  });
  assert.equal(cancelDirectLegacy.status, 201);

  const confirmedCreateKey = randomUUID();
  const confirmedCreateBody = {
    team_id: initial.team_id,
    title: "Confirm each transfer",
    assignee_id: b.principal.id,
    message: "Accept before material access moves.",
    attachment_ids: [ownerOnlyUpload.value.id],
    idempotency_key: confirmedCreateKey,
  };
  const confirmed = await api("/v1/threads", {
    method: "POST",
    token: a.device_token,
    headers: protocol02,
    body: confirmedCreateBody,
  });
  assert.equal(confirmed.status, 201);
  assert.equal(confirmed.value.transfer_mode, "confirmed");
  assert.equal(confirmed.value.action.transfer_mode, "confirmed");
  assert.equal(confirmed.value.thread.phase, "pending_transfer");
  assert.equal(confirmed.value.thread.owner_id, a.principal.id);
  assert.equal(confirmed.value.thread.current_actor_id, a.principal.id);
  assert.equal(confirmed.value.thread.pending_transfer_kind, "assignment");
  assert.equal(confirmed.value.thread.pending_recipient_id, b.principal.id);
  const confirmedThreadId = confirmed.value.thread.id;
  const confirmedReplay = await api("/v1/threads", {
    method: "POST",
    token: a.device_token,
    headers: protocol02,
    body: confirmedCreateBody,
  });
  assert.equal(confirmedReplay.status, 200);
  assert.equal(confirmedReplay.value.replayed, true);
  assert.equal(confirmedReplay.value.thread.id, confirmedThreadId);
  const crossProtocolReplay = await api("/v1/threads", {
    method: "POST",
    token: a.device_token,
    body: confirmedCreateBody,
  });
  assert.equal(crossProtocolReplay.status, 409);
  assert.equal(crossProtocolReplay.value.error.code, "idempotency_key_reused");

  const bPendingBootstrap = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: b.device_token,
    headers: protocol02,
  });
  const bPending = bPendingBootstrap.value.threads.find((detail) => detail.thread.id === confirmedThreadId);
  assert.deepEqual(bPending.actions[0].attachment_ids, [], "a pending recipient only receives the task summary");
  const bPendingAttachment = await api(`/v1/attachments/${ownerOnlyUpload.value.id}`, { token: b.device_token });
  assert.equal(bPendingAttachment.status, 403);

  const reroutedToC = await action(a, confirmedThreadId, "reroute_transfer", 1, {
    headers: protocol02,
    recipientId: c.principal.id,
    message: "C is a better first recipient.",
  });
  assert.equal(reroutedToC.status, 201);
  assert.equal(reroutedToC.value.thread.pending_recipient_id, c.principal.id);
  const bAfterReroute = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: b.device_token,
    headers: protocol02,
  });
  assert.equal(
    bAfterReroute.value.threads.some(detail => detail.thread.id === confirmedThreadId),
    false,
    "an unaccepted candidate loses visibility after rerouting",
  );
  const staleRecipientAccept = await action(b, confirmedThreadId, "accept_transfer", 2, {
    headers: protocol02,
    message: "Too late.",
  });
  assert.equal(staleRecipientAccept.status, 403);
  const cDeclined = await action(c, confirmedThreadId, "decline_transfer", 2, {
    headers: protocol02,
    message: "This belongs with B.",
  });
  assert.equal(cDeclined.status, 201);
  assert.equal(cDeclined.value.thread.phase, "awaiting_reassignment");
  const cAfterDecline = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: c.device_token,
    headers: protocol02,
  });
  assert.equal(
    cAfterDecline.value.threads.some(detail => detail.thread.id === confirmedThreadId),
    false,
    "an unaccepted candidate loses visibility after declining",
  );

  const reroutedToB = await action(a, confirmedThreadId, "reroute_transfer", 3, {
    headers: protocol02,
    recipientId: b.principal.id,
    message: "Returning to the original owner.",
  });
  assert.equal(reroutedToB.status, 201);
  const bAccepted = await action(b, confirmedThreadId, "accept_transfer", 4, {
    headers: protocol02,
    message: "Accepted.",
  });
  assert.equal(bAccepted.status, 201);
  assert.equal(bAccepted.value.thread.phase, "active");
  assert.equal(bAccepted.value.thread.owner_id, b.principal.id);
  const bAcceptedAttachment = await api(`/v1/attachments/${ownerOnlyUpload.value.id}`, { token: b.device_token });
  assert.equal(bAcceptedAttachment.status, 200, "acceptance grants access to all immutable task materials");

  const directLegacyCollaboration = await action(b, confirmedThreadId, "handoff", 5, {
    headers: protocol02,
    recipientId: legacyRecipient.principal.id,
    message: "Collaborate from an older client.",
  });
  assert.equal(directLegacyCollaboration.status, 201);
  assert.equal(directLegacyCollaboration.value.transfer_mode, "direct_legacy");
  assert.equal(directLegacyCollaboration.value.thread.phase, "active");
  assert.equal(directLegacyCollaboration.value.thread.current_actor_id, legacyRecipient.principal.id);
  const legacyCollaborationReturn = await action(
    legacyRecipient,
    confirmedThreadId,
    "return",
    6,
    { message: "Returning from the legacy client." },
  );
  assert.equal(legacyCollaborationReturn.status, 201);
  assert.equal(legacyCollaborationReturn.value.thread.current_actor_id, b.principal.id);

  const firstCollaborationInvite = await action(b, confirmedThreadId, "handoff", 7, {
    headers: protocol02,
    recipientId: c.principal.id,
    message: "Please collaborate.",
  });
  assert.equal(firstCollaborationInvite.status, 201);
  assert.equal(firstCollaborationInvite.value.thread.phase, "pending_transfer");
  assert.equal(firstCollaborationInvite.value.thread.current_actor_id, b.principal.id, "the baton stays with the owner before acceptance");
  const withdrawnCollaboration = await action(b, confirmedThreadId, "withdraw_transfer", 8, {
    headers: protocol02,
    message: "The scope changed.",
  });
  assert.equal(withdrawnCollaboration.status, 201);
  assert.equal(withdrawnCollaboration.value.thread.phase, "active");
  assert.equal(withdrawnCollaboration.value.thread.current_actor_id, b.principal.id);
  const cAfterWithdrawal = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: c.device_token,
    headers: protocol02,
  });
  assert.equal(
    cAfterWithdrawal.value.threads.some(detail => detail.thread.id === confirmedThreadId),
    false,
    "a first-time collaborator loses visibility when the invitation is withdrawn",
  );
  const secondCollaborationInvite = await action(b, confirmedThreadId, "handoff", 9, {
    headers: protocol02,
    recipientId: c.principal.id,
    message: "The updated scope is ready.",
  });
  assert.equal(secondCollaborationInvite.status, 201);
  const cAcceptedRelay = await action(c, confirmedThreadId, "accept_transfer", 10, {
    headers: protocol02,
    message: "I can take this relay.",
  });
  assert.equal(cAcceptedRelay.status, 201);
  assert.equal(cAcceptedRelay.value.thread.owner_id, b.principal.id);
  assert.equal(cAcceptedRelay.value.thread.current_actor_id, c.principal.id);
  const confirmedReturned = await action(c, confirmedThreadId, "return", 11, {
    headers: protocol02,
    message: "Returning to the previous relay owner.",
  });
  assert.equal(confirmedReturned.status, 201);
  assert.equal(confirmedReturned.value.thread.current_actor_id, b.principal.id);
  const confirmedCancelled = await action(a, confirmedThreadId, "cancel", 12, {
    headers: protocol02,
    message: "Confirmed transfer test complete.",
  });
  assert.equal(confirmedCancelled.status, 201);
  assert.equal(confirmedCancelled.value.thread.phase, "cancelled");

  const pendingForLegacyReroute = await api("/v1/threads", {
    method: "POST",
    token: a.device_token,
    headers: protocol02,
    body: {
      team_id: initial.team_id,
      title: "Reroute to a legacy recipient",
      assignee_id: b.principal.id,
      message: "Start with a confirmation-capable member.",
      attachment_ids: [],
      idempotency_key: randomUUID(),
    },
  });
  assert.equal(pendingForLegacyReroute.value.transfer_mode, "confirmed");
  const directLegacyReroute = await action(
    a,
    pendingForLegacyReroute.value.thread.id,
    "reroute_transfer",
    1,
    {
      headers: protocol02,
      recipientId: legacyRecipient.principal.id,
      message: "The legacy recipient should not be blocked on confirmation.",
    },
  );
  assert.equal(directLegacyReroute.status, 201);
  assert.equal(directLegacyReroute.value.transfer_mode, "direct_legacy");
  assert.equal(directLegacyReroute.value.thread.phase, "active");
  assert.equal(directLegacyReroute.value.thread.owner_id, legacyRecipient.principal.id);
  const cancelLegacyReroute = await action(
    a,
    pendingForLegacyReroute.value.thread.id,
    "cancel",
    2,
    { headers: protocol02, message: "Legacy reroute behavior verified." },
  );
  assert.equal(cancelLegacyReroute.status, 201);

  await pool.query(
    "UPDATE devices SET relay_protocol_seen_at = now() - interval '31 days' WHERE principal_id = $1",
    [b.principal.id],
  );
  const expiredCapabilityView = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: a.device_token,
    headers: protocol02,
  });
  assert.equal(
    expiredCapabilityView.value.members.find(member => member.id === b.principal.id).confirmed_transfers_supported,
    false,
  );
  const directForExpiredCapability = await api("/v1/threads", {
    method: "POST",
    token: a.device_token,
    headers: protocol02,
    body: {
      team_id: initial.team_id,
      title: "Expired recipient capability",
      assignee_id: b.principal.id,
      message: "An expired capability must fail safe to direct transfer.",
      attachment_ids: [],
      idempotency_key: randomUUID(),
    },
  });
  assert.equal(directForExpiredCapability.value.transfer_mode, "direct_legacy");
  const cancelExpiredCapability = await action(
    a,
    directForExpiredCapability.value.thread.id,
    "cancel",
    1,
    { headers: protocol02, message: "Capability expiry verified." },
  );
  assert.equal(cancelExpiredCapability.status, 201);
  const refreshedBCapability = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: b.device_token,
    headers: protocol02,
  });
  assert.equal(
    refreshedBCapability.value.members.find(member => member.id === b.principal.id).confirmed_transfers_supported,
    true,
  );

  const bPinned = await api(`/v1/threads/${threadId}/preference`, {
    method: "PUT",
    token: b.device_token,
    body: { pinned: true, hidden: false },
  });
  assert.equal(bPinned.status, 200);
  assert.equal(bPinned.value.pinned, true);
  const bPreferences = await api(`/v1/bootstrap?team_id=${initial.team_id}`, { token: b.device_token });
  const aPreferences = await api(`/v1/bootstrap?team_id=${initial.team_id}`, { token: a.device_token });
  assert.equal(bPreferences.value.threads.find(item => item.thread.id === threadId).preference.pinned, true);
  assert.equal(aPreferences.value.threads.find(item => item.thread.id === threadId).preference.pinned, false);

  const bHidden = await api(`/v1/threads/${threadId}/preference`, {
    method: "PUT",
    token: b.device_token,
    body: { pinned: true, hidden: true },
  });
  assert.equal(bHidden.status, 200);
  assert.equal(bHidden.value.hidden, true);

  const cancellable = await api("/v1/threads", {
    method: "POST",
    token: a.device_token,
    body: {
      team_id: initial.team_id,
      title: "Cancel this work",
      assignee_id: b.principal.id,
      message: "This request is no longer needed.",
      attachment_ids: [],
      idempotency_key: randomUUID(),
    },
  });
  assert.equal(cancellable.status, 201);
  const activeHide = await api(`/v1/threads/${cancellable.value.thread.id}/preference`, {
    method: "PUT",
    token: b.device_token,
    body: { pinned: false, hidden: true },
  });
  assert.equal(activeHide.status, 403);
  assert.equal(activeHide.value.error.code, "action_not_allowed");
  const ownerCancel = await action(b, cancellable.value.thread.id, "cancel", 1, { message: "Not my decision." });
  assert.equal(ownerCancel.status, 403);
  const cancelled = await action(a, cancellable.value.thread.id, "cancel", 1, { message: "Priority changed." });
  assert.equal(cancelled.status, 201);
  assert.equal(cancelled.value.thread.phase, "cancelled");
  assert.equal(cancelled.value.action.kind, "cancel");
  assert.equal(cancelled.value.action.message, "Priority changed.");

  const finalThread = await api(`/v1/threads/${threadId}`, { token: b.device_token });
  assert.equal(finalThread.status, 200);
  assert.equal(finalThread.value.actions.length, 8);
  assert.equal(new Set(finalThread.value.actions.map((item) => item.sequence)).size, 8);
  assert.deepEqual(finalThread.value.participants.sort(), [
    a.principal.id,
    b.principal.id,
    c.principal.id,
  ].sort());

  await pool.query(
    "UPDATE attachments SET created_at = now() - interval '2 hours' WHERE id = ANY($1::uuid[])",
    [[largeOrphan.value.id, exactLimit.value.id]],
  );
  await pool.query(
    "UPDATE work_threads SET updated_at = now() - interval '31 days' WHERE id = $1",
    [threadId],
  );
  const temporaryDir = join(attachmentDir, ".tmp");
  await mkdir(temporaryDir, { recursive: true });
  const staleTemporary = join(temporaryDir, randomUUID());
  await writeFile(staleTemporary, "interrupted upload");
  const staleTime = new Date(Date.now() - 2 * 60 * 60 * 1000);
  await utimes(staleTemporary, staleTime, staleTime);
  const cleaned = await cleanupAttachments(pool, config);
  assert.deepEqual(cleaned, {
    temporary: 1,
    orphaned: 2,
    expired: 1,
    avatarTemporary: 0,
    avatarOrphaned: 0,
  });
  const orphanMetadata = await api(`/v1/attachments/${largeOrphan.value.id}`, {
    token: a.device_token,
  });
  assert.equal(orphanMetadata.status, 404);
  const expiredMetadata = await api(`/v1/attachments/${planUpload.value.id}`, {
    token: a.device_token,
  });
  assert.equal(expiredMetadata.status, 200);
  assert.equal(expiredMetadata.value.available, false);
  const expiredContent = await api(`/v1/attachments/${planUpload.value.id}/content`, {
    token: a.device_token,
  });
  assert.equal(expiredContent.status, 410);
  assert.equal(expiredContent.value.error.code, "attachment_expired");

  const removeLastTeamOwner = await api(
    `/v1/teams/${initial.team_id}/members/${a.principal.id}`,
    { method: "DELETE", token: a.device_token },
  );
  assert.equal(removeLastTeamOwner.status, 409);
  assert.equal(removeLastTeamOwner.value.error.code, "last_team_owner");

  const removedSseController = new AbortController();
  t.after(() => removedSseController.abort());
  const removedSse = await fetch(`${origin}/v1/events?team_id=${initial.team_id}`, {
    headers: { Authorization: `Bearer ${c.device_token}` },
    signal: removedSseController.signal,
  });
  assert.equal(removedSse.status, 200);
  const removedSseReader = removedSse.body.getReader();
  await readSseUntil(removedSseReader, "event: ready");
  const inviteBeforeRemoval = await api(`/v1/teams/${initial.team_id}/invites`, {
    method: "POST",
    token: c.device_token,
    body: { expires_in_hours: 24 },
  });
  assert.equal(inviteBeforeRemoval.status, 201);
  const removeC = await api(
    `/v1/teams/${initial.team_id}/members/${c.principal.id}`,
    { method: "DELETE", token: a.device_token },
  );
  assert.equal(removeC.status, 204);
  await waitForSseEnd(removedSseReader);
  const passwordAfterRemoval = await api(`/v1/teams/${initial.team_id}/join-password`, {
    token: a.device_token,
  });
  assert.equal(passwordAfterRemoval.status, 200);
  assert.notEqual(passwordAfterRemoval.value.join_password, activePassword.value.join_password);
  const removedMemberKnownCode = await api("/v1/teams/join", {
    method: "POST",
    token: c.device_token,
    headers: { "X-Real-IP": "203.0.113.31" },
    body: { join_code: activePassword.value.join_password },
  });
  assert.equal(removedMemberKnownCode.status, 401);
  assert.equal(removedMemberKnownCode.value.error.code, "join_password_invalid");
  const removedMemberOldInvite = await api("/v1/teams/join", {
    method: "POST",
    token: c.device_token,
    body: { join_code: inviteBeforeRemoval.value.invite_code },
  });
  assert.equal(removedMemberOldInvite.status, 410);
  assert.equal(removedMemberOldInvite.value.error.code, "invite_expired");
  const thirdPartyOldInvite = await api("/v1/enrollments/claim", {
    method: "POST",
    body: {
      invite_code: inviteBeforeRemoval.value.invite_code,
      email: nextEmail(),
      display_name: "Revoked invite user",
      device_name: "Revoked invite test device",
    },
  });
  assert.equal(thirdPartyOldInvite.status, 410, "unused invites created by a removed member must be revoked");
  assert.equal(thirdPartyOldInvite.value.error.code, "invite_expired");
  const removedBootstrap = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: c.device_token,
  });
  assert.equal(removedBootstrap.status, 403);
  assert.equal(removedBootstrap.value.error.code, "membership_required");

  const ownerTransfer = await api(`/v1/teams/${initial.team_id}/owner-transfer`, {
    method: "POST",
    token: a.device_token,
    body: { new_owner_id: b.principal.id },
  });
  assert.equal(ownerTransfer.status, 200);
  assert.deepEqual(ownerTransfer.value, {
    team_id: initial.team_id,
    previous_owner_id: a.principal.id,
    owner_id: b.principal.id,
  });
  const membersAfterTransfer = await api(`/v1/teams/${initial.team_id}/members`, {
    token: b.device_token,
  });
  assert.equal(membersAfterTransfer.value.members.find(member => member.id === a.principal.id).role, "member");
  assert.equal(membersAfterTransfer.value.members.find(member => member.id === b.principal.id).role, "owner");

  const joinCodeBeforeLeave = passwordAfterRemoval.value.join_password;
  const inviteBeforeLeave = await api(`/v1/teams/${initial.team_id}/invites`, {
    method: "POST",
    token: a.device_token,
    body: { expires_in_hours: 24 },
  });
  assert.equal(inviteBeforeLeave.status, 201);
  const leaveA = await api(`/v1/teams/${initial.team_id}/members/me`, {
    method: "DELETE",
    token: a.device_token,
  });
  assert.equal(leaveA.status, 204);
  const teamsAfterLeave = await api("/v1/teams", { token: a.device_token });
  assert.deepEqual(teamsAfterLeave.value.teams, []);
  const oldCodeAfterLeave = await api("/v1/teams/join", {
    method: "POST",
    token: a.device_token,
    body: { join_code: joinCodeBeforeLeave },
  });
  assert.equal(oldCodeAfterLeave.status, 401);
  assert.equal(oldCodeAfterLeave.value.error.code, "join_password_invalid");
  const thirdPartyInviteAfterLeave = await api("/v1/enrollments/claim", {
    method: "POST",
    body: {
      invite_code: inviteBeforeLeave.value.invite_code,
      email: nextEmail(),
      display_name: "Departed inviter user",
      device_name: "Departed inviter test device",
    },
  });
  assert.equal(thirdPartyInviteAfterLeave.status, 410, "unused invites created by a departed member must be revoked");
  assert.equal(thirdPartyInviteAfterLeave.value.error.code, "invite_expired");

  const passwordBeforeDissolution = await api(
    `/v1/teams/${initial.team_id}/join-password`,
    { token: b.device_token },
  );
  assert.equal(passwordBeforeDissolution.status, 200);
  const avatarBeforeDissolution = await uploadAvatar(b, animatedGif, "owner-avatar.gif");
  assert.equal(avatarBeforeDissolution.status, 200);
  const dissolveSseController = new AbortController();
  t.after(() => dissolveSseController.abort());
  const dissolveSse = await fetch(`${origin}/v1/events`, {
    headers: { Authorization: `Bearer ${b.device_token}` },
    signal: dissolveSseController.signal,
  });
  assert.equal(dissolveSse.status, 200);
  const dissolveSseReader = dissolveSse.body.getReader();
  await readSseUntil(dissolveSseReader, "event: ready");
  const dissolve = await api(`/v1/teams/${initial.team_id}`, {
    method: "DELETE",
    token: b.device_token,
  });
  assert.equal(dissolve.status, 204);
  await readSseUntil(dissolveSseReader, "event: team.dissolved");
  await waitForSseEnd(dissolveSseReader);
  const teamsAfterDissolution = await api("/v1/teams", { token: b.device_token });
  assert.deepEqual(teamsAfterDissolution.value.teams, []);
  const meAfterDissolution = await api("/v1/me", { token: b.device_token });
  assert.deepEqual(meAfterDissolution.value.memberships, []);
  const dissolvedBootstrap = await api(`/v1/bootstrap?team_id=${initial.team_id}`, {
    token: b.device_token,
  });
  assert.equal(dissolvedBootstrap.status, 403);
  assert.equal(dissolvedBootstrap.value.error.code, "membership_required");
  const dissolvedThread = await api(`/v1/threads/${threadId}`, { token: b.device_token });
  assert.equal(dissolvedThread.status, 403);
  assert.equal(dissolvedThread.value.error.code, "membership_required");
  const dissolvedAttachment = await api(`/v1/attachments/${planUpload.value.id}`, {
    token: b.device_token,
  });
  assert.equal(dissolvedAttachment.status, 404);
  assert.equal(dissolvedAttachment.value.error.code, "resource_not_found");
  const dissolvedAvatar = await downloadAvatar(
    b.device_token,
    b.principal.id,
    initial.team_id,
  );
  assert.equal(dissolvedAvatar.status, 403);
  const dissolvedJoinCode = await api("/v1/teams/join", {
    method: "POST",
    token: a.device_token,
    body: { join_code: passwordBeforeDissolution.value.join_password },
  });
  assert.equal(dissolvedJoinCode.status, 401);
  assert.equal(dissolvedJoinCode.value.error.code, "join_password_invalid");
  const historicalTeam = await pool.query(
    "SELECT dissolved_at, dissolved_by FROM teams WHERE id = $1",
    [initial.team_id],
  );
  assert.ok(historicalTeam.rows[0].dissolved_at);
  assert.equal(historicalTeam.rows[0].dissolved_by, b.principal.id);
  const historicalThread = await pool.query("SELECT phase FROM work_threads WHERE id = $1", [threadId]);
  assert.equal(historicalThread.rows[0].phase, "closed");

  for (let attempt = 0; attempt < 5; attempt += 1) {
    const invalidJoin = await api("/v1/enrollments/join", {
      method: "POST",
      headers: { "X-Real-IP": "203.0.113.25" },
      body: {
        join_password: "0000-0000",
        display_name: "Rate limited",
        device_name: "rate limit test device",
      },
    });
    assert.equal(invalidJoin.status, 401);
  }
  const rateLimitedJoin = await api("/v1/enrollments/join", {
    method: "POST",
    headers: { "X-Real-IP": "203.0.113.25" },
    body: {
      join_password: secondPassword.value.join_password,
      display_name: "Blocked",
      device_name: "blocked test device",
    },
  });
  assert.equal(rateLimitedJoin.status, 429);
  assert.equal(rateLimitedJoin.value.error.code, "rate_limited");
  assert.equal(rateLimitedJoin.retryAfter, "900");
});
