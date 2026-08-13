import { constants, createWriteStream } from "node:fs";
import { chmod, link, mkdir, open, rm, writeFile } from "node:fs/promises";
import { randomUUID } from "node:crypto";
import { resolve } from "node:path";
import { pipeline } from "node:stream/promises";
import Busboy from "busboy";
import { authenticate, requireMembership } from "./auth.js";
import { avatarRoot, avatarStoragePath, isPathInside } from "./avatar-storage.js";
import {
  AVATAR_VARIANT_SIZES,
  detectAvatarContent,
  MAX_AVATAR_BYTES,
  processAvatarImage,
} from "./avatar-image.js";
import { newId } from "./crypto.js";
import { inTransaction } from "./db.js";
import { AppError, invalidRequest } from "./errors.js";
import { avatarFromRow } from "./serializers.js";
import { uuid } from "./validation.js";

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
  const maximumBytes = Math.min(config.maxAvatarBytes, MAX_AVATAR_BYTES);
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
        // Busboy emits partsLimit when the parsed count reaches this value.
        parts: 2,
        fileSize: maximumBytes + 1,
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
  let activeFileStream;
  let activeFileWriter;
  const interrupted = invalidRequest("The avatar upload was interrupted.");

  try {
    await new Promise((resolvePromise, rejectPromise) => {
      let settled = false;
      const settle = (callback, value) => {
        if (settled) return;
        settled = true;
        request.off("aborted", onInterrupted);
        request.off("error", onInterrupted);
        callback(value);
      };
      const onInterrupted = () => {
        request.unpipe(parser);
        activeFileStream?.destroy(interrupted);
        activeFileWriter?.destroy(interrupted);
        parser.destroy(interrupted);
        settle(rejectPromise, interrupted);
      };
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
        activeFileStream = stream;
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
        });
        activeFileWriter = createWriteStream(temporaryPath, { flags: "wx", mode: 0o600 });
        filePromise = pipeline(stream, activeFileWriter);
        filePromise.catch(() => {});
      });
      for (const event of ["fieldsLimit", "filesLimit", "partsLimit"]) {
        parser.once(event, () => {
          limitError ||= invalidRequest("The avatar upload has too many parts.");
        });
      }
      parser.once("error", error => settle(rejectPromise, error));
      parser.once("close", () => settle(resolvePromise));
      request.once("aborted", onInterrupted);
      request.once("error", onInterrupted);
      if (request.aborted || request.destroyed) onInterrupted();
      else request.pipe(parser);
    });
  } catch (error) {
    if (filePromise) await filePromise.catch(() => {});
    await rm(temporaryPath, { force: true });
    throw error;
  }

  try {
    if (filePromise) await filePromise;
    if (limitError) throw limitError;
    if (!fileSeen || !filePromise || !filename || byteSize === 0) {
      throw invalidRequest("One non-empty avatar file is required.");
    }
    if (tooLarge || byteSize > maximumBytes) {
      throw new AppError(413, "avatar_too_large", "The avatar exceeds the size limit.");
    }
    return { filename, temporaryPath, byteSize };
  } catch (error) {
    await rm(temporaryPath, { force: true });
    throw error;
  }
}

async function persistAssets(
  config,
  processed,
  fileOperations = { chmod, link, mkdir, rm, writeFile },
) {
  const temporaryDir = await prepareStorage(config);
  const entries = [
    { size: null, asset: processed.main },
    ...AVATAR_VARIANT_SIZES.map(size => ({ size, asset: processed.variants.get(size) })),
  ];
  const stored = [];
  try {
    for (const entry of entries) {
      const id = newId();
      const storageKey = `${id.slice(0, 2)}/${id}`;
      const directory = resolve(avatarRoot(config), id.slice(0, 2));
      const finalPath = avatarStoragePath(config, storageKey);
      const temporaryPath = resolve(temporaryDir, randomUUID());
      try {
        await fileOperations.writeFile(temporaryPath, entry.asset.data, { flag: "wx", mode: 0o600 });
        await fileOperations.mkdir(directory, { recursive: true, mode: 0o700 });
        await fileOperations.chmod(directory, 0o700);
        await fileOperations.link(temporaryPath, finalPath);
        // Register immediately after the hard link becomes observable so every
        // later error includes it in synchronous rollback.
        stored.push({ ...entry, storageKey, finalPath });
        await fileOperations.chmod(finalPath, 0o600);
      } finally {
        await fileOperations.rm(temporaryPath, { force: true });
      }
    }
    return stored;
  } catch (error) {
    await Promise.all(stored
      .map(entry => fileOperations.rm(entry.finalPath, { force: true }).catch(() => {})));
    throw error;
  }
}

async function removeStored(entries) {
  await Promise.all(entries.map(entry => rm(entry.finalPath, { force: true }).catch(() => {})));
}

async function commitAvatarAssets(
  pool,
  principalId,
  stored,
  config,
  runTransaction = inTransaction,
) {
  const main = stored.find(entry => entry.size === null);
  const variants = stored.filter(entry => entry.size !== null);
  let result;
  try {
    result = await runTransaction(pool, async (client) => {
      await client.query("SELECT id FROM principals WHERE id = $1 FOR UPDATE", [principalId]);
      const previousMain = await client.query(
        "SELECT storage_key FROM principal_avatars WHERE principal_id = $1",
        [principalId],
      );
      const previousVariants = await client.query(
        "SELECT storage_key FROM principal_avatar_variants WHERE principal_id = $1",
        [principalId],
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
        [
          principalId,
          main.storageKey,
          main.asset.contentType,
          main.asset.byteSize,
          main.asset.sha256,
        ],
      );
      await client.query("DELETE FROM principal_avatar_variants WHERE principal_id = $1", [principalId]);
      for (const variant of variants) {
        await client.query(
          `INSERT INTO principal_avatar_variants (
             principal_id, size, storage_key, content_type, byte_size, sha256
           ) VALUES ($1, $2, $3, $4, $5, $6)`,
          [
            principalId,
            variant.size,
            variant.storageKey,
            variant.asset.contentType,
            variant.asset.byteSize,
            variant.asset.sha256,
          ],
        );
      }
      const memberships = await client.query(
        "SELECT team_id FROM memberships WHERE principal_id = $1 AND removed_at IS NULL",
        [principalId],
      );
      return {
        avatar: avatarFromRow(updated.rows[0]),
        previousStorageKeys: [
          previousMain.rows[0]?.storage_key,
          ...previousVariants.rows.map(row => row.storage_key),
        ].filter(Boolean),
        teamIds: memberships.rows.map(row => row.team_id),
      };
    });
  } catch (error) {
    await removeStored(stored);
    throw error;
  }

  const currentKeys = new Set(stored.map(entry => entry.storageKey));
  await Promise.all(result.previousStorageKeys
    .filter(storageKey => !currentKeys.has(storageKey))
    // Deletion is best effort after the transaction commits. Any failure leaves
    // an unreferenced file that the scheduled avatar orphan scan will remove.
    .map(storageKey => rm(avatarStoragePath(config, storageKey), { force: true }).catch(() => {})));
  return result;
}

export async function uploadAvatar(pool, token, request, config, processingGate) {
  const auth = await authenticate(pool, token);
  let upload;
  let release;
  try {
    ({ upload, release } = await receiveAvatarWithGate(
      request,
      config,
      processingGate,
      auth.principalId,
    ));
    let stored = [];
    const processed = await processAvatarImage(upload, config);
    stored = await persistAssets(config, processed);
    return await commitAvatarAssets(pool, auth.principalId, stored, config);
  } finally {
    release?.();
    if (upload) await rm(upload.temporaryPath, { force: true });
  }
}

async function receiveAvatarWithGate(request, config, processingGate, principalId) {
  let release;
  try {
    try {
      release = processingGate?.enter(principalId);
    } catch (error) {
      // Drain the rejected body without piping it into storage so the HTTP
      // connection remains reusable and concurrent uploads consume no disk.
      request.resume?.();
      throw error;
    }
    const upload = await receiveAvatarMultipart(request, config);
    return { upload, release };
  } catch (error) {
    release?.();
    throw error;
  }
}

export async function deleteAvatar(pool, token, config) {
  const auth = await authenticate(pool, token);
  const result = await inTransaction(pool, async (client) => {
    await client.query("SELECT id FROM principals WHERE id = $1 FOR UPDATE", [auth.principalId]);
    const variants = await client.query(
      "SELECT storage_key FROM principal_avatar_variants WHERE principal_id = $1",
      [auth.principalId],
    );
    const deleted = await client.query(
      "DELETE FROM principal_avatars WHERE principal_id = $1 RETURNING storage_key",
      [auth.principalId],
    );
    const memberships = await client.query(
      "SELECT team_id FROM memberships WHERE principal_id = $1 AND removed_at IS NULL",
      [auth.principalId],
    );
    return {
      storageKeys: [
        deleted.rows[0]?.storage_key,
        ...variants.rows.map(row => row.storage_key),
      ].filter(Boolean),
      teamIds: memberships.rows.map(row => row.team_id),
    };
  });
  await Promise.all(result.storageKeys
    .map(storageKey => rm(avatarStoragePath(config, storageKey), { force: true }).catch(() => {})));
  return result;
}

function avatarVariantSize(rawSize) {
  if (rawSize === null || rawSize === undefined || rawSize === "") return null;
  if (!/^(48|96)$/.test(String(rawSize))) {
    throw invalidRequest("Avatar size must be 48 or 96.");
  }
  return Number(rawSize);
}

async function authorizedAvatar(pool, token, rawPrincipalId, rawTeamId, rawSize) {
  const principalId = uuid(rawPrincipalId, "principal_id");
  const teamId = uuid(rawTeamId, "team_id");
  const size = avatarVariantSize(rawSize);
  const auth = await authenticate(pool, token);
  await requireMembership(pool, teamId, auth.principalId);
  await requireMembership(pool, teamId, principalId);
  const result = size === null
    ? await pool.query(
      `SELECT principal_id, storage_key, content_type, byte_size, sha256,
              sha256 AS source_sha256, updated_at
         FROM principal_avatars
        WHERE principal_id = $1`,
      [principalId],
    )
    : await pool.query(
      `SELECT avatar.principal_id, variant.storage_key, variant.content_type,
              variant.byte_size, variant.sha256, avatar.sha256 AS source_sha256,
              avatar.updated_at
         FROM principal_avatars avatar
         JOIN principal_avatar_variants variant
           ON variant.principal_id = avatar.principal_id AND variant.size = $2
        WHERE avatar.principal_id = $1`,
      [principalId, size],
    );
  if (result.rowCount === 0) {
    throw new AppError(404, "resource_not_found", "The avatar was not found.");
  }
  return result.rows[0];
}

export async function openAvatarContent(pool, token, principalId, teamId, size, config) {
  const row = await authorizedAvatar(pool, token, principalId, teamId, size);
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
  avatarVariantSize,
  commitAvatarAssets,
  detectAvatarContent,
  isPathInside,
  persistAssets,
  receiveAvatarWithGate,
  receiveAvatarMultipart,
};
