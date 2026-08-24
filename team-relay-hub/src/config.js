import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const packageRoot = resolve(fileURLToPath(new URL("..", import.meta.url)));

function integer(value, fallback, minimum, maximum) {
  const parsed = Number(value ?? fallback);
  if (!Number.isSafeInteger(parsed) || parsed < minimum || parsed > maximum) {
    throw new Error(`expected an integer between ${minimum} and ${maximum}`);
  }
  return parsed;
}

function boolean(value, fallback = false) {
  const normalized = String(value ?? fallback).toLowerCase();
  if (normalized === "true") return true;
  if (normalized === "false") return false;
  throw new Error("expected true or false");
}

function optionalSha256(value, name) {
  const normalized = String(value || "").trim().toLowerCase();
  if (normalized && !/^[0-9a-f]{64}$/.test(normalized)) {
    throw new Error(`${name} must be an SHA-256 hex digest`);
  }
  return normalized;
}

export function normalizeBasePath(value) {
  const trimmed = String(value ?? "").trim();
  if (!trimmed || trimmed === "/") return "";
  if (!trimmed.startsWith("/")) throw new Error("TEAM_RELAY_PUBLIC_BASE_PATH must start with /");
  return trimmed.replace(/\/+$/, "");
}

export function loadConfig(env = process.env) {
  const databaseUrl = String(env.TEAM_RELAY_DATABASE_URL || "").trim();
  if (!databaseUrl) throw new Error("TEAM_RELAY_DATABASE_URL is required");

  return {
    databaseUrl,
    databaseSsl: String(env.TEAM_RELAY_DB_SSL || "false").toLowerCase() === "true",
    host: String(env.TEAM_RELAY_HOST || "127.0.0.1"),
    port: integer(env.TEAM_RELAY_PORT, 8791, 0, 65_535),
    publicBasePath: normalizeBasePath(
      env.TEAM_RELAY_PUBLIC_BASE_PATH ?? "/pinvou3/team-relay",
    ),
    trustProxy: boolean(env.TEAM_RELAY_TRUST_PROXY),
    joinRateLimitFailures: integer(env.TEAM_RELAY_JOIN_RATE_LIMIT_FAILURES, 5, 1, 100),
    joinRateLimitWindowSeconds: integer(
      env.TEAM_RELAY_JOIN_RATE_LIMIT_WINDOW_SECONDS,
      600,
      1,
      86_400,
    ),
    joinRateLimitBlockSeconds: integer(
      env.TEAM_RELAY_JOIN_RATE_LIMIT_BLOCK_SECONDS,
      900,
      1,
      86_400,
    ),
    joinCodeEncryptionKey: String(env.TEAM_RELAY_JOIN_CODE_ENCRYPTION_KEY || "").trim(),
    allowSelfServiceEnrollment: boolean(env.TEAM_RELAY_ALLOW_SELF_SERVICE_ENROLLMENT),
    betaAccessPasswordHash: optionalSha256(
      env.TEAM_RELAY_BETA_ACCESS_PASSWORD_HASH,
      "TEAM_RELAY_BETA_ACCESS_PASSWORD_HASH",
    ),
    maxJsonBytes: integer(env.TEAM_RELAY_MAX_JSON_BYTES, 1024 * 1024, 1024, 4 * 1024 * 1024),
    attachmentDir: resolve(
      env.TEAM_RELAY_ATTACHMENT_DIR || resolve(packageRoot, "data", "attachments"),
    ),
    maxAttachmentBytes: integer(
      env.TEAM_RELAY_MAX_ATTACHMENT_BYTES,
      20 * 1024 * 1024,
      1024,
      20 * 1024 * 1024,
    ),
    maxAvatarBytes: integer(
      env.TEAM_RELAY_MAX_AVATAR_BYTES,
      1_572_864,
      1024,
      1_572_864,
    ),
    avatarProcessingConcurrency: integer(
      env.TEAM_RELAY_AVATAR_PROCESSING_CONCURRENCY,
      2,
      1,
      8,
    ),
    avatarUploadRateLimitAttempts: integer(
      env.TEAM_RELAY_AVATAR_UPLOAD_RATE_LIMIT_ATTEMPTS,
      6,
      1,
      100,
    ),
    avatarUploadRateLimitWindowSeconds: integer(
      env.TEAM_RELAY_AVATAR_UPLOAD_RATE_LIMIT_WINDOW_SECONDS,
      60,
      1,
      3600,
    ),
    maxTeamAttachmentBytes: integer(
      env.TEAM_RELAY_MAX_TEAM_ATTACHMENT_BYTES,
      2 * 1024 * 1024 * 1024,
      1024 * 1024,
      Number.MAX_SAFE_INTEGER,
    ),
    orphanAttachmentHours: integer(env.TEAM_RELAY_ORPHAN_ATTACHMENT_HOURS, 24, 1, 24 * 30),
    closedAttachmentDays: integer(env.TEAM_RELAY_CLOSED_ATTACHMENT_DAYS, 30, 1, 3650),
    migrationDir: resolve(
      env.TEAM_RELAY_MIGRATION_DIR
        || (env.TEAM_RELAY_MIGRATION_PATH ? dirname(env.TEAM_RELAY_MIGRATION_PATH) : "")
        || resolve(packageRoot, "..", "docs", "team-relay", "migrations"),
    ),
  };
}
