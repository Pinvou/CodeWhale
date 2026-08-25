import http from "node:http";
import { randomUUID, timingSafeEqual } from "node:crypto";
import { authenticate, requireMembership } from "./auth.js";
import {
  getAttachment,
  openAttachmentContent,
  uploadAttachment,
} from "./attachment-service.js";
import { deleteAvatar, openAvatarContent, uploadAvatar } from "./avatar-service.js";
import { AvatarProcessingGate, AvatarUploadGate } from "./avatar-processing-gate.js";
import { asAppError, AppError } from "./errors.js";
import { sha256 } from "./crypto.js";
import { JoinRateLimiter } from "./join-rate-limiter.js";
import {
  claimEnrollment,
  createEnrollmentWithTeam,
  createTeam,
  createTeamInvite,
  disableTeamJoinPassword,
  getMe,
  getTeamJoinPassword,
  joinEnrollment,
  joinTeam,
  listTeams,
  listTeamMembers,
  registerAccount,
  rotateTeamJoinPassword,
  setTeamJoinPasswordVisibility,
  updateProfile,
} from "./identity-service.js";
import {
  dissolveTeam,
  leaveTeam,
  removeTeamMember,
  transferTeamOwner,
} from "./team-lifecycle-service.js";
import {
  createAction,
  createThread,
  getBootstrap,
  getThread,
  listThreads,
  markThreadRead,
  setThreadPreference,
} from "./thread-service.js";
import { exactKeys, text, uuid } from "./validation.js";

function requestedProtocol(request) {
  return request.headers["x-pinvou-team-relay-protocol"] === "0.2" ? "0.2" : "0.1";
}

function bearerToken(request) {
  const header = request.headers.authorization;
  if (typeof header !== "string") return null;
  const match = /^Bearer ([A-Za-z0-9_-]{32,256})$/.exec(header);
  return match?.[1] || null;
}

function sendJson(response, status, value, requestId, extraHeaders = {}) {
  if (response.headersSent) return;
  const body = value === undefined ? "" : JSON.stringify(value);
  response.writeHead(status, {
    "Content-Type": "application/json; charset=utf-8",
    "Content-Length": Buffer.byteLength(body),
    "Cache-Control": "no-store",
    "X-Content-Type-Options": "nosniff",
    "X-Request-Id": requestId,
    ...extraHeaders,
  });
  response.end(body);
}

function clientAddress(request, trustProxy) {
  if (trustProxy) {
    const forwarded = request.headers["x-real-ip"];
    if (typeof forwarded === "string" && forwarded.length <= 64 && forwarded.trim()) {
      return forwarded.trim();
    }
  }
  return request.socket.remoteAddress || "unknown";
}

async function readJson(request, maximumBytes) {
  const contentType = String(request.headers["content-type"] || "").split(";", 1)[0].trim();
  if (contentType !== "application/json") {
    throw new AppError(400, "invalid_request", "Content-Type must be application/json.");
  }
  const chunks = [];
  let bytes = 0;
  for await (const chunk of request) {
    bytes += chunk.length;
    if (bytes > maximumBytes) {
      throw new AppError(413, "invalid_request", "The JSON request body is too large.");
    }
    chunks.push(chunk);
  }
  try {
    return JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    throw new AppError(400, "invalid_request", "The JSON request body is invalid.");
  }
}

class EventBroker {
  constructor() {
    this.teams = new Map();
  }

  subscribe(teamId, principalId, response) {
    let responses = this.teams.get(teamId);
    if (!responses) {
      responses = new Set();
      this.teams.set(teamId, responses);
    }
    const subscription = { principalId, response };
    responses.add(subscription);
    return () => {
      responses.delete(subscription);
      if (responses.size === 0) this.teams.delete(teamId);
    };
  }

  publish(teamId, event, resource = {}) {
    const data = JSON.stringify({
      team_id: teamId,
      thread_id: resource.id,
      revision: resource.revision,
    });
    for (const subscription of this.teams.get(teamId) || []) {
      if (resource.principal_id && subscription.principalId !== resource.principal_id) continue;
      try {
        subscription.response.write(`event: ${event}\ndata: ${data}\n\n`);
      } catch {
        subscription.response.destroy();
      }
    }
  }

  revoke(teamId, principalId) {
    const responses = this.teams.get(teamId);
    if (!responses) return;
    for (const subscription of [...responses]) {
      if (subscription.principalId !== principalId) continue;
      responses.delete(subscription);
      try {
        subscription.response.end();
      } catch {
        subscription.response.destroy();
      }
    }
    if (responses.size === 0) this.teams.delete(teamId);
  }

  revokeTeam(teamId) {
    const responses = this.teams.get(teamId);
    if (!responses) return;
    this.teams.delete(teamId);
    for (const subscription of responses) {
      try {
        subscription.response.end();
      } catch {
        subscription.response.destroy();
      }
    }
  }

  closeAll() {
    for (const responses of this.teams.values()) {
      for (const subscription of responses) subscription.response.end();
    }
    this.teams.clear();
  }
}

function pathMatch(pathname, pattern) {
  const match = pattern.exec(pathname);
  return match ? match.slice(1) : null;
}

function contentDisposition(filename) {
  return `attachment; filename="download"; filename*=UTF-8''${encodeURIComponent(filename)}`;
}

export function createHubServer({ pool, config }) {
  const events = new EventBroker();
  const notify = events.publish.bind(events);
  const joinRateLimiter = new JoinRateLimiter({
    maximumFailures: config.joinRateLimitFailures ?? 5,
    windowMs: (config.joinRateLimitWindowSeconds ?? 600) * 1000,
    blockMs: (config.joinRateLimitBlockSeconds ?? 900) * 1000,
  });
  const betaAccessRateLimiter = new JoinRateLimiter({
    maximumFailures: config.joinRateLimitFailures ?? 5,
    windowMs: (config.joinRateLimitWindowSeconds ?? 600) * 1000,
    blockMs: (config.joinRateLimitBlockSeconds ?? 900) * 1000,
  });
  const avatarUploadGate = new AvatarUploadGate({
    maximumAttempts: config.avatarUploadRateLimitAttempts ?? 6,
    windowMs: (config.avatarUploadRateLimitWindowSeconds ?? 60) * 1000,
  });
  const avatarProcessingGate = new AvatarProcessingGate({
    maximumConcurrent: config.avatarProcessingConcurrency ?? 2,
  });

  async function verifyBetaAccess(request) {
    if (!config.betaAccessPasswordHash) {
      throw new AppError(403, "action_not_allowed", "Beta access verification is disabled on this Hub.");
    }
    const rateLimitKey = `beta-access:${clientAddress(request, config.trustProxy)}`;
    betaAccessRateLimiter.check(rateLimitKey);
    const body = exactKeys(await readJson(request, config.maxJsonBytes), ["access_code"]);
    const accessCode = text(body.access_code, "access_code", { minimum: 1, maximum: 256 }).trim();
    const actual = Buffer.from(sha256(accessCode), "hex");
    const expected = Buffer.from(config.betaAccessPasswordHash, "hex");
    if (actual.length !== expected.length || !timingSafeEqual(actual, expected)) {
      betaAccessRateLimiter.recordFailure(rateLimitKey);
      throw new AppError(403, "beta_access_invalid", "The beta access code is invalid.");
    }
    betaAccessRateLimiter.recordSuccess(rateLimitKey);
    return { authorized: true };
  }

  async function joinWithRateLimit(request, operation, authenticatedKey = null) {
    const rateLimitKey = authenticatedKey || `ip:${clientAddress(request, config.trustProxy)}`;
    joinRateLimiter.check(rateLimitKey);
    try {
      const result = await operation();
      joinRateLimiter.recordSuccess(rateLimitKey);
      return result;
    } catch (error) {
      if (asAppError(error).code === "join_password_invalid") {
        joinRateLimiter.recordFailure(rateLimitKey);
      }
      throw error;
    }
  }

  const server = http.createServer(async (request, response) => {
    const requestId = `req_${randomUUID()}`;
    response.on("error", () => {});
    try {
      const url = new URL(request.url || "/", "http://team-relay.local");
      if (config.publicBasePath && !url.pathname.startsWith(`${config.publicBasePath}/`)) {
        throw new AppError(404, "resource_not_found", "The endpoint was not found.");
      }
      const pathname = config.publicBasePath
        ? url.pathname.slice(config.publicBasePath.length)
        : url.pathname;
      const method = request.method || "GET";
      const token = bearerToken(request);

      if (method === "GET" && pathname === "/v1/healthz") {
        await pool.query("SELECT 1");
        sendJson(response, 200, { status: "ok", database: "ready" }, requestId);
        return;
      }
      if (method === "POST" && pathname === "/v1/beta-access/verify") {
        sendJson(response, 200, await verifyBetaAccess(request), requestId);
        return;
      }
      if (method === "POST" && pathname === "/v1/enrollments/claim") {
        const result = await claimEnrollment(pool, await readJson(request, config.maxJsonBytes));
        notify(result.team.id, "membership.changed");
        sendJson(response, 201, result, requestId);
        return;
      }
      if (method === "POST" && pathname === "/v1/enrollments/account") {
        if (!config.allowSelfServiceEnrollment) {
          throw new AppError(403, "action_not_allowed", "Self-service account enrollment is disabled on this Hub.");
        }
        const result = await registerAccount(pool, await readJson(request, config.maxJsonBytes));
        for (const teamId of result.team_ids) notify(teamId, "profile.changed");
        const { team_ids: _teamIds, ...responseBody } = result;
        sendJson(response, 201, responseBody, requestId);
        return;
      }
      if (method === "POST" && pathname === "/v1/enrollments/create-team") {
        if (!config.allowSelfServiceEnrollment) {
          throw new AppError(403, "action_not_allowed", "Self-service team enrollment is disabled on this Hub.");
        }
        const result = await createEnrollmentWithTeam(pool, await readJson(request, config.maxJsonBytes));
        notify(result.team.id, "membership.changed");
        sendJson(response, 201, result, requestId);
        return;
      }
      if (method === "POST" && pathname === "/v1/enrollments/join") {
        const body = await readJson(request, config.maxJsonBytes);
        const result = await joinWithRateLimit(request, () => joinEnrollment(pool, body));
        notify(result.team.id, "membership.changed");
        sendJson(response, 201, result, requestId);
        return;
      }
      if (method === "GET" && pathname === "/v1/me") {
        sendJson(response, 200, await getMe(pool, token), requestId);
        return;
      }
      if (method === "PATCH" && pathname === "/v1/me") {
        const result = await updateProfile(pool, token, await readJson(request, config.maxJsonBytes));
        for (const teamId of result.team_ids) notify(teamId, "profile.changed");
        sendJson(response, 200, { principal: result.principal }, requestId);
        return;
      }
      if (method === "PUT" && pathname === "/v1/me/avatar") {
        const result = await uploadAvatar(
          pool,
          token,
          request,
          config,
          avatarUploadGate,
          avatarProcessingGate,
        );
        for (const teamId of result.teamIds) notify(teamId, "profile.changed");
        sendJson(response, 200, { avatar: result.avatar }, requestId);
        return;
      }
      if (method === "DELETE" && pathname === "/v1/me/avatar") {
        const result = await deleteAvatar(pool, token, config);
        for (const teamId of result.teamIds) notify(teamId, "profile.changed");
        response.writeHead(204, {
          "Cache-Control": "no-store",
          "X-Request-Id": requestId,
        });
        response.end();
        return;
      }
      if (method === "GET" && pathname === "/v1/teams") {
        sendJson(response, 200, await listTeams(pool, token), requestId);
        return;
      }
      if (method === "POST" && pathname === "/v1/teams") {
        sendJson(response, 201, await createTeam(pool, token, await readJson(request, config.maxJsonBytes)), requestId);
        return;
      }
      if (method === "POST" && pathname === "/v1/teams/join") {
        const body = await readJson(request, config.maxJsonBytes);
        const result = await joinWithRateLimit(
          request,
          () => joinTeam(pool, token, body),
          token ? `device:${sha256(token)}` : null,
        );
        notify(result.team.id, "membership.changed");
        sendJson(response, 200, result, requestId);
        return;
      }

      let params = pathMatch(pathname, /^\/v1\/teams\/([^/]+)\/members$/);
      if (params && method === "GET") {
        sendJson(response, 200, await listTeamMembers(pool, token, params[0]), requestId);
        return;
      }
      params = pathMatch(pathname, /^\/v1\/teams\/([^/]+)\/invites$/);
      if (params && method === "POST") {
        const body = await readJson(request, config.maxJsonBytes);
        sendJson(response, 201, await createTeamInvite(pool, token, params[0], body), requestId);
        return;
      }
      params = pathMatch(pathname, /^\/v1\/teams\/([^/]+)\/join-password$/);
      if (params && method === "GET") {
        sendJson(
          response,
          200,
          await getTeamJoinPassword(pool, token, params[0], config.joinCodeEncryptionKey),
          requestId,
        );
        return;
      }
      if (params && method === "POST") {
        const body = await readJson(request, config.maxJsonBytes);
        sendJson(
          response,
          200,
          await rotateTeamJoinPassword(
            pool,
            token,
            params[0],
            body,
            config.joinCodeEncryptionKey,
          ),
          requestId,
        );
        return;
      }
      if (params && method === "PATCH") {
        const body = await readJson(request, config.maxJsonBytes);
        await setTeamJoinPasswordVisibility(pool, token, params[0], body);
        sendJson(
          response,
          200,
          await getTeamJoinPassword(pool, token, params[0], config.joinCodeEncryptionKey),
          requestId,
        );
        return;
      }
      if (params && method === "DELETE") {
        sendJson(
          response,
          200,
          await disableTeamJoinPassword(pool, token, params[0]),
          requestId,
        );
        return;
      }
      params = pathMatch(pathname, /^\/v1\/teams\/([^/]+)\/owner-transfer$/);
      if (params && method === "POST") {
        const teamId = uuid(params[0], "team_id");
        const result = await transferTeamOwner(
          pool,
          token,
          teamId,
          await readJson(request, config.maxJsonBytes),
        );
        notify(teamId, "membership.changed");
        sendJson(response, 200, result, requestId);
        return;
      }
      params = pathMatch(pathname, /^\/v1\/teams\/([^/]+)\/members\/me$/);
      if (params && method === "DELETE") {
        const result = await leaveTeam(pool, token, params[0], config.joinCodeEncryptionKey);
        events.revoke(result.team_id, result.principal_id);
        notify(result.team_id, "membership.changed");
        response.writeHead(204, { "X-Request-Id": requestId, "Cache-Control": "no-store" });
        response.end();
        return;
      }
      params = pathMatch(pathname, /^\/v1\/teams\/([^/]+)\/members\/([^/]+)$/);
      if (params && method === "DELETE") {
        const teamId = uuid(params[0], "team_id");
        const principalId = uuid(params[1], "principal_id");
        await removeTeamMember(
          pool,
          token,
          teamId,
          principalId,
          config.joinCodeEncryptionKey,
        );
        events.revoke(teamId, principalId);
        notify(teamId, "membership.changed");
        response.writeHead(204, { "X-Request-Id": requestId, "Cache-Control": "no-store" });
        response.end();
        return;
      }
      params = pathMatch(pathname, /^\/v1\/teams\/([^/]+)$/);
      if (params && method === "DELETE") {
        const result = await dissolveTeam(pool, token, params[0]);
        notify(result.team_id, "team.dissolved");
        events.revokeTeam(result.team_id);
        response.writeHead(204, { "X-Request-Id": requestId, "Cache-Control": "no-store" });
        response.end();
        return;
      }

      params = pathMatch(pathname, /^\/v1\/principals\/([^/]+)\/avatar$/);
      if (params && method === "GET") {
        const { row, handle } = await openAvatarContent(
          pool,
          token,
          params[0],
          url.searchParams.get("team_id"),
          url.searchParams.get("size"),
          url.searchParams.get("reduced_motion"),
          config,
        );
        response.writeHead(200, {
          "Content-Type": row.content_type,
          "Content-Length": String(row.byte_size),
          "Content-Disposition": "inline",
          Digest: `sha-256=${Buffer.from(row.sha256, "hex").toString("base64")}`,
          ETag: `"${row.sha256}"`,
          "X-Avatar-Source-SHA256": row.source_sha256,
          "Cache-Control": "private, no-store",
          "Content-Security-Policy": "default-src 'none'; sandbox",
          "X-Content-Type-Options": "nosniff",
          "X-Request-Id": requestId,
        });
        const stream = handle.createReadStream();
        stream.once("error", () => response.destroy());
        stream.pipe(response);
        return;
      }

      if (method === "GET" && pathname === "/v1/threads") {
        sendJson(
          response,
          200,
          await listThreads(pool, token, url.searchParams.get("team_id"), url.searchParams.get("view") || "open"),
          requestId,
        );
        return;
      }
      if (method === "POST" && pathname === "/v1/threads") {
        const body = await readJson(request, config.maxJsonBytes);
        const result = await createThread(pool, token, body, notify, requestedProtocol(request));
        sendJson(response, result.replayed ? 200 : 201, result, requestId);
        return;
      }
      params = pathMatch(pathname, /^\/v1\/threads\/([^/]+)$/);
      if (params && method === "GET") {
        sendJson(response, 200, await getThread(pool, token, params[0]), requestId);
        return;
      }
      params = pathMatch(pathname, /^\/v1\/threads\/([^/]+)\/actions$/);
      if (params && method === "POST") {
        const body = await readJson(request, config.maxJsonBytes);
        const result = await createAction(pool, token, params[0], body, notify, requestedProtocol(request));
        sendJson(response, result.replayed ? 200 : 201, result, requestId);
        return;
      }
      params = pathMatch(pathname, /^\/v1\/threads\/([^/]+)\/preference$/);
      if (params && method === "PUT") {
        const body = await readJson(request, config.maxJsonBytes);
        sendJson(response, 200, await setThreadPreference(pool, token, params[0], body, notify), requestId);
        return;
      }
      params = pathMatch(pathname, /^\/v1\/threads\/([^/]+)\/read$/);
      if (params && method === "PUT") {
        const body = await readJson(request, config.maxJsonBytes);
        sendJson(response, 200, await markThreadRead(pool, token, params[0], body, notify), requestId);
        return;
      }
      if (method === "POST" && pathname === "/v1/attachments") {
        const result = await uploadAttachment(
          pool,
          token,
          request.headers["idempotency-key"],
          request,
          config,
        );
        sendJson(response, result.replayed ? 200 : 201, result.attachment, requestId);
        return;
      }
      params = pathMatch(pathname, /^\/v1\/attachments\/([^/]+)$/);
      if (params && method === "GET") {
        sendJson(response, 200, await getAttachment(pool, token, params[0]), requestId);
        return;
      }
      params = pathMatch(pathname, /^\/v1\/attachments\/([^/]+)\/content$/);
      if (params && method === "GET") {
        const { row, handle } = await openAttachmentContent(pool, token, params[0], config);
        response.writeHead(200, {
          "Content-Type": row.content_type,
          "Content-Length": String(row.byte_size),
          "Content-Disposition": contentDisposition(row.original_name),
          Digest: `sha-256=${Buffer.from(row.sha256, "hex").toString("base64")}`,
          "Cache-Control": "private, no-store",
          "X-Content-Type-Options": "nosniff",
          "X-Request-Id": requestId,
        });
        const stream = handle.createReadStream();
        stream.once("error", () => response.destroy());
        stream.pipe(response);
        return;
      }
      if (method === "GET" && pathname === "/v1/bootstrap") {
        sendJson(
          response,
          200,
          await getBootstrap(
            pool,
            token,
            url.searchParams.get("team_id"),
            requestedProtocol(request),
            notify,
          ),
          requestId,
        );
        return;
      }
      if (method === "GET" && pathname === "/v1/events") {
        const auth = await authenticate(pool, token);
        const requestedTeamId = url.searchParams.get("team_id");
        let teamIds;
        if (requestedTeamId) {
          const teamId = uuid(requestedTeamId, "team_id");
          await requireMembership(pool, teamId, auth.principalId);
          teamIds = [teamId];
        } else {
          const memberships = await listTeams(pool, token);
          teamIds = memberships.teams.map(team => team.id);
        }
        response.writeHead(200, {
          "Content-Type": "text/event-stream; charset=utf-8",
          "Cache-Control": "no-cache, no-transform",
          Connection: "keep-alive",
          "X-Accel-Buffering": "no",
          "X-Request-Id": requestId,
        });
        response.write("retry: 3000\nevent: ready\ndata: {}\n\n");
        const unsubscribes = teamIds.map(teamId => events.subscribe(teamId, auth.principalId, response));
        const heartbeat = setInterval(() => response.write(": keepalive\n\n"), 20_000);
        heartbeat.unref();
        const cleanup = () => {
          clearInterval(heartbeat);
          for (const unsubscribe of unsubscribes) unsubscribe();
        };
        request.once("close", cleanup);
        response.once("close", cleanup);
        return;
      }

      throw new AppError(404, "resource_not_found", "The endpoint was not found.");
    } catch (error) {
      const appError = asAppError(error);
      const retryAfterSeconds = appError.details?.retry_after_seconds;
      sendJson(response, appError.status, {
        error: {
          code: appError.code,
          message: appError.message,
          request_id: requestId,
          ...(appError.details ? { details: appError.details } : {}),
        },
      }, requestId, retryAfterSeconds ? { "Retry-After": String(retryAfterSeconds) } : {});
    }
  });
  server.closeEventStreams = () => events.closeAll();
  return server;
}
