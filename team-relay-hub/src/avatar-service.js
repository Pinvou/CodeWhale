import { constants, createWriteStream } from "node:fs";
import { chmod, link, mkdir, open, rm } from "node:fs/promises";
import { createHash, randomUUID } from "node:crypto";
import { extname, isAbsolute, relative, resolve, sep } from "node:path";
import { pipeline } from "node:stream/promises";
import Busboy from "busboy";
import { fileTypeFromFile } from "file-type";
import { authenticate, requireMembership } from "./auth.js";
import { newId } from "./crypto.js";
import { inTransaction } from "./db.js";
import { AppError, invalidRequest } from "./errors.js";
import { avatarFromRow } from "./serializers.js";
import { uuid } from "./validation.js";

const ALLOWED_AVATAR_EXTENSIONS = new Map([
  ["png", "png"],
  ["jpg", "jpg"],
  ["jpeg", "jpg"],
  ["webp", "webp"],
  ["gif", "gif"],
]);
const ALLOWED_AVATAR_CONTENT_TYPES = new Set([
  "image/png",
  "image/jpeg",
  "image/webp",
  "image/gif",
]);
const STORAGE_KEY_PATTERN = /^[0-9a-f]{2}\/[0-9a-f-]{36}$/;

function isPathInside(root, candidate, pathApi = { isAbsolute, relative, sep }) {
  const relativePath = pathApi.relative(root, candidate);
  return relativePath !== ""
    && relativePath !== ".."
    && !relativePath.startsWith(`..${pathApi.sep}`)
    && !pathApi.isAbsolute(relativePath);
}

function avatarRoot(config) {
  return resolve(config.attachmentDir, "avatars");
}

function avatarStoragePath(config, storageKey) {
  if (!STORAGE_KEY_PATTERN.test(storageKey)) throw new Error("invalid avatar storage key");
  const root = avatarRoot(config);
  const candidate = resolve(root, storageKey);
  if (!isPathInside(root, candidate, { isAbsolute, relative, sep })) {
    throw new Error("avatar path escaped storage root");
  }
  return candidate;
}

async function prepareStorage(config) {
  const root = avatarRoot(config);
  const temporaryDir = resolve(root, ".tmp");
  await mkdir(root, { recursive: true, mode: 0o700 });
  await chmod(root, 0o700);
  await mkdir(temporaryDir, { recursive: true, mode: 0o700 });
  await chmod(temporaryDir, 0o700);
  return temporaryDir;
}

function safeFilename(value) {
  if (typeof value !== "string") throw invalidRequest("The avatar filename is required.");
  const normalized = value.normalize("NFC");
  if (
    normalized.length < 1
    || normalized.length > 255
    || /[\u0000-\u001f\u007f/\\]/u.test(normalized)
  ) {
    throw invalidRequest("The avatar filename is invalid.");
  }
  return normalized;
}

async function receiveAvatarMultipart(request, config) {
  const temporaryDir = await prepareStorage(config);
  const temporaryPath = resolve(temporaryDir, randomUUID());
  let parser;
  try {
    parser = Busboy({
      headers: request.headers,
      preservePath: true,
      defParamCharset: "utf8",
      limits: {
        fieldNameSize: 64,
        fieldSize: 1,
        fields: 0,
        files: 1,
        parts: 1,
        fileSize: config.maxAvatarBytes + 1,
        headerPairs: 64,
      },
    });
  } catch {
    throw invalidRequest("A valid multipart/form-data request is required.");
  }

  let fileSeen = false;
  let filePromise;
  let filename;
  let tooLarge = false;
  let limitError;
  let byteSize = 0;
  const hash = createHash("sha256");

  try {
    await new Promise((resolvePromise, rejectPromise) => {
      parser.on("field", () => {
        limitError ||= invalidRequest("The avatar upload cannot contain form fields.");
      });
      parser.on("file", (name, stream, info) => {
        if (name !== "file" || fileSeen) {
          limitError ||= invalidRequest("Exactly one avatar file is required.");
          stream.resume();
          return;
        }
        fileSeen = true;
        try {
          filename = safeFilename(info.filename);
        } catch (error) {
          limitError ||= error;
          stream.resume();
          return;
        }
        stream.once("limit", () => {
          tooLarge = true;
        });
        stream.on("data", (chunk) => {
          byteSize += chunk.length;
          hash.update(chunk);
        });
        filePromise = pipeline(
          stream,
          createWriteStream(temporaryPath, { flags: "wx", mode: 0o600 }),
        );
        filePromise.catch(() => {});
      });
      for (const event of ["fieldsLimit", "filesLimit", "partsLimit"]) {
        parser.once(event, () => {
          limitError ||= invalidRequest("The avatar upload has too many parts.");
        });
      }
      parser.once("error", rejectPromise);
      parser.once("close", resolvePromise);
      request.pipe(parser);
    });
  } catch (error) {
    await rm(temporaryPath, { force: true });
    throw error;
  }

  try {
    if (filePromise) await filePromise;
    if (limitError) throw limitError;
    if (!fileSeen || !filePromise || !filename || byteSize === 0) {
      throw invalidRequest("One non-empty avatar file is required.");
    }
    if (tooLarge || byteSize > config.maxAvatarBytes) {
      throw new AppError(413, "avatar_too_large", "The avatar exceeds the size limit.");
    }
    return {
      filename,
      temporaryPath,
      byteSize,
      sha256: hash.digest("hex"),
    };
  } catch (error) {
    await rm(temporaryPath, { force: true });
    throw error;
  }
}

async function detectAvatarContent(upload) {
  const expected = ALLOWED_AVATAR_EXTENSIONS.get(
    extname(upload.filename).slice(1).toLowerCase(),
  );
  if (!expected) {
    throw new AppError(415, "avatar_type_not_allowed", "The avatar file type is not allowed.");
  }
  let detected;
  try {
    detected = await fileTypeFromFile(upload.temporaryPath);
  } catch {
    detected = undefined;
  }
  if (
    !detected
    || detected.ext !== expected
    || !ALLOWED_AVATAR_CONTENT_TYPES.has(detected.mime)
  ) {
    throw new AppError(415, "avatar_type_not_allowed", "The avatar content does not match its extension.");
  }
  return detected.mime;
}

export async function uploadAvatar(pool, token, request, config) {
  const auth = await authenticate(pool, token);
  const upload = await receiveAvatarMultipart(request, config);
  try {
    const contentType = await detectAvatarContent(upload);
    const avatarId = newId();
    const storageKey = `${avatarId.slice(0, 2)}/${avatarId}`;
    const directory = resolve(avatarRoot(config), avatarId.slice(0, 2));
    const finalPath = avatarStoragePath(config, storageKey);
    await mkdir(directory, { recursive: true, mode: 0o700 });
    await chmod(directory, 0o700);
    await link(upload.temporaryPath, finalPath);
    await rm(upload.temporaryPath, { force: true });
    await chmod(finalPath, 0o600);

    let result;
    try {
      result = await inTransaction(pool, async (client) => {
        await client.query("SELECT id FROM principals WHERE id = $1 FOR UPDATE", [auth.principalId]);
        const previous = await client.query(
          "SELECT storage_key FROM principal_avatars WHERE principal_id = $1",
          [auth.principalId],
        );
        const updated = await client.query(
          `INSERT INTO principal_avatars (
             principal_id, storage_key, content_type, byte_size, sha256
           ) VALUES ($1, $2, $3, $4, $5)
           ON CONFLICT (principal_id) DO UPDATE SET
             storage_key = EXCLUDED.storage_key,
             content_type = EXCLUDED.content_type,
             byte_size = EXCLUDED.byte_size,
             sha256 = EXCLUDED.sha256,
             updated_at = clock_timestamp()
           RETURNING content_type AS avatar_content_type,
                     byte_size AS avatar_byte_size,
                     sha256 AS avatar_sha256,
                     updated_at AS avatar_updated_at`,
          [auth.principalId, storageKey, contentType, upload.byteSize, upload.sha256],
        );
        const memberships = await client.query(
          "SELECT team_id FROM memberships WHERE principal_id = $1 AND removed_at IS NULL",
          [auth.principalId],
        );
        return {
          avatar: avatarFromRow(updated.rows[0]),
          previousStorageKey: previous.rows[0]?.storage_key,
          teamIds: memberships.rows.map((row) => row.team_id),
        };
      });
    } catch (error) {
      await rm(finalPath, { force: true });
      throw error;
    }
    if (result.previousStorageKey && result.previousStorageKey !== storageKey) {
      await rm(avatarStoragePath(config, result.previousStorageKey), { force: true }).catch(() => {});
    }
    return result;
  } catch (error) {
    await rm(upload.temporaryPath, { force: true });
    throw error;
  }
}

export async function deleteAvatar(pool, token, config) {
  const auth = await authenticate(pool, token);
  const result = await inTransaction(pool, async (client) => {
    await client.query("SELECT id FROM principals WHERE id = $1 FOR UPDATE", [auth.principalId]);
    const deleted = await client.query(
      "DELETE FROM principal_avatars WHERE principal_id = $1 RETURNING storage_key",
      [auth.principalId],
    );
    const memberships = await client.query(
      "SELECT team_id FROM memberships WHERE principal_id = $1 AND removed_at IS NULL",
      [auth.principalId],
    );
    return {
      storageKey: deleted.rows[0]?.storage_key,
      teamIds: memberships.rows.map((row) => row.team_id),
    };
  });
  if (result.storageKey) {
    await rm(avatarStoragePath(config, result.storageKey), { force: true }).catch(() => {});
  }
  return result;
}

async function authorizedAvatar(pool, token, rawPrincipalId, rawTeamId) {
  const principalId = uuid(rawPrincipalId, "principal_id");
  const teamId = uuid(rawTeamId, "team_id");
  const auth = await authenticate(pool, token);
  await requireMembership(pool, teamId, auth.principalId);
  await requireMembership(pool, teamId, principalId);
  const result = await pool.query(
    `SELECT principal_id, storage_key, content_type, byte_size, sha256, updated_at
       FROM principal_avatars
      WHERE principal_id = $1`,
    [principalId],
  );
  if (result.rowCount === 0) {
    throw new AppError(404, "resource_not_found", "The avatar was not found.");
  }
  return result.rows[0];
}

export async function openAvatarContent(pool, token, principalId, teamId, config) {
  const row = await authorizedAvatar(pool, token, principalId, teamId);
  let handle;
  try {
    handle = await open(
      avatarStoragePath(config, row.storage_key),
      constants.O_RDONLY | constants.O_NOFOLLOW,
    );
  } catch (error) {
    if (error?.code === "ENOENT" || error?.code === "ELOOP") {
      throw new AppError(503, "service_unavailable", "The avatar storage is inconsistent.");
    }
    throw error;
  }
  const fileStat = await handle.stat();
  if (!fileStat.isFile() || fileStat.size !== Number(row.byte_size)) {
    await handle.close();
    throw new AppError(503, "service_unavailable", "The avatar storage is inconsistent.");
  }
  return { row, handle };
}

export {
  avatarStoragePath,
  detectAvatarContent,
  isPathInside,
};
