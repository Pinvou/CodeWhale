import { constants, createWriteStream } from "node:fs";
import { chmod, link, mkdir, open, rm } from "node:fs/promises";
import { createHash, randomUUID } from "node:crypto";
import { extname, isAbsolute, relative, resolve, sep } from "node:path";
import { pipeline } from "node:stream/promises";
import { Transform } from "node:stream";
import Busboy from "busboy";
import { fileTypeFromFile } from "file-type";
import { authenticate, requireMembership } from "./auth.js";
import { newId } from "./crypto.js";
import { inTransaction } from "./db.js";
import { AppError, invalidRequest } from "./errors.js";
import { uuid } from "./validation.js";

const ALLOWED_BINARY_EXTENSIONS = new Map([
  ["pdf", "pdf"],
  ["docx", "docx"],
  ["xlsx", "xlsx"],
  ["pptx", "pptx"],
  ["png", "png"],
  ["jpg", "jpg"],
  ["jpeg", "jpg"],
  ["webp", "webp"],
  ["zip", "zip"],
]);
const TEXT_CONTENT_TYPES = new Map([
  ["md", "text/markdown; charset=utf-8"],
  ["txt", "text/plain; charset=utf-8"],
  ["csv", "text/csv; charset=utf-8"],
  // Code is transported as a download-only snapshot. Use text/plain even for
  // HTML/JS so the Hub never serves an uploaded program as executable content.
  ["html", "text/plain; charset=utf-8"],
  ["htm", "text/plain; charset=utf-8"],
  ["css", "text/plain; charset=utf-8"],
  ["js", "text/plain; charset=utf-8"],
  ["mjs", "text/plain; charset=utf-8"],
  ["cjs", "text/plain; charset=utf-8"],
  ["jsx", "text/plain; charset=utf-8"],
  ["ts", "text/plain; charset=utf-8"],
  ["tsx", "text/plain; charset=utf-8"],
  ["json", "text/plain; charset=utf-8"],
  ["yaml", "text/plain; charset=utf-8"],
  ["yml", "text/plain; charset=utf-8"],
]);
const STORAGE_KEY_PATTERN = /^[0-9a-f]{2}\/[0-9a-f-]{36}$/;

function attachmentFromRow(row) {
  return {
    id: row.id,
    team_id: row.team_id,
    uploaded_by: row.uploaded_by,
    original_name: row.original_name,
    content_type: row.content_type,
    byte_size: Number(row.byte_size),
    sha256: row.sha256,
    bound: Boolean(row.bound_at),
    available: !row.deleted_at,
    created_at: row.created_at,
  };
}

function safeDisplayName(value) {
  if (typeof value !== "string") throw invalidRequest("The upload filename is required.");
  const normalized = value.normalize("NFC");
  if (
    normalized.length < 1
    || normalized.length > 255
    || /[\u0000-\u001f\u007f/\\]/u.test(normalized)
  ) {
    throw invalidRequest("The upload filename is invalid.");
  }
  return normalized;
}

function isPathInside(root, candidate, pathApi = { isAbsolute, relative, sep }) {
  const relativePath = pathApi.relative(root, candidate);
  return relativePath !== ""
    && relativePath !== ".."
    && !relativePath.startsWith(`..${pathApi.sep}`)
    && !pathApi.isAbsolute(relativePath);
}

function storagePath(config, storageKey) {
  if (!STORAGE_KEY_PATTERN.test(storageKey)) throw new Error("invalid attachment storage key");
  const root = resolve(config.attachmentDir);
  const candidate = resolve(root, storageKey);
  if (!isPathInside(root, candidate, { isAbsolute, relative, sep })) {
    throw new Error("attachment path escaped storage root");
  }
  return candidate;
}

async function prepareStorage(config) {
  const temporaryDir = resolve(config.attachmentDir, ".tmp");
  await mkdir(config.attachmentDir, { recursive: true, mode: 0o700 });
  await chmod(config.attachmentDir, 0o700);
  await mkdir(temporaryDir, { recursive: true, mode: 0o700 });
  await chmod(temporaryDir, 0o700);
  return temporaryDir;
}

async function receiveMultipart(request, config) {
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
        fieldSize: 128,
        fields: 1,
        files: 1,
        parts: 3,
        fileSize: config.maxAttachmentBytes + 1,
        headerPairs: 64,
      },
    });
  } catch {
    throw invalidRequest("A valid multipart/form-data request is required.");
  }

  let teamId;
  let fileSeen = false;
  let filePromise;
  let filename;
  let tooLarge = false;
  let limitError;
  let byteSize = 0;
  const hash = createHash("sha256");
  const textDecoder = new TextDecoder("utf-8", { fatal: true });
  let isUtf8 = true;
  let containsNul = false;

  try {
    await new Promise((resolvePromise, rejectPromise) => {
      parser.on("field", (name, value, info) => {
        if (name !== "team_id" || info.nameTruncated || info.valueTruncated || teamId !== undefined) {
          limitError ||= invalidRequest("The multipart fields are invalid.");
          return;
        }
        teamId = value;
      });
      parser.on("file", (name, stream, info) => {
        if (name !== "file" || fileSeen) {
          limitError ||= invalidRequest("Exactly one file field is required.");
          stream.resume();
          return;
        }
        fileSeen = true;
        try {
          filename = safeDisplayName(info.filename);
        } catch (error) {
          limitError ||= error;
          stream.resume();
          return;
        }
        stream.once("limit", () => {
          tooLarge = true;
        });
        const inspect = new Transform({
          transform(chunk, _encoding, callback) {
            byteSize += chunk.length;
            hash.update(chunk);
            if (chunk.includes(0)) containsNul = true;
            if (isUtf8) {
              try {
                textDecoder.decode(chunk, { stream: true });
              } catch {
                isUtf8 = false;
              }
            }
            callback(null, chunk);
          },
          flush(callback) {
            if (isUtf8) {
              try {
                textDecoder.decode();
              } catch {
                isUtf8 = false;
              }
            }
            callback();
          },
        });
        filePromise = pipeline(
          stream,
          inspect,
          createWriteStream(temporaryPath, { flags: "wx", mode: 0o600 }),
        );
        filePromise.catch(() => {});
      });
      for (const event of ["fieldsLimit", "filesLimit", "partsLimit"]) {
        parser.once(event, () => {
          limitError ||= invalidRequest("The multipart request has too many parts.");
        });
      }
      parser.once("error", rejectPromise);
      parser.once("close", () => resolvePromise());
      request.pipe(parser);
    });
  } catch (error) {
    await rm(temporaryPath, { force: true });
    throw error;
  }

  try {
    if (filePromise) await filePromise;
    if (limitError) throw limitError;
    if (!fileSeen || !filePromise || teamId === undefined) {
      throw invalidRequest("team_id and one file are required.");
    }
    if (tooLarge || byteSize > config.maxAttachmentBytes) {
      throw new AppError(413, "attachment_too_large", "The attachment exceeds the size limit.");
    }
    return {
      teamId: uuid(teamId, "team_id"),
      filename,
      temporaryPath,
      byteSize,
      sha256: hash.digest("hex"),
      isUtf8,
      containsNul,
    };
  } catch (error) {
    await rm(temporaryPath, { force: true });
    throw error;
  }
}

async function detectContent(upload) {
  const extension = extname(upload.filename).slice(1).toLowerCase();
  if (TEXT_CONTENT_TYPES.has(extension)) {
    if (!upload.isUtf8 || upload.containsNul) {
      throw new AppError(
        415,
        "attachment_type_not_allowed",
        "Text attachments must contain valid UTF-8 text without NUL bytes.",
      );
    }
    let detected;
    try {
      detected = await fileTypeFromFile(upload.temporaryPath);
    } catch {
      detected = undefined;
    }
    if (detected) {
      throw new AppError(415, "attachment_type_not_allowed", "The file content does not match its extension.");
    }
    return TEXT_CONTENT_TYPES.get(extension);
  }

  const expected = ALLOWED_BINARY_EXTENSIONS.get(extension);
  if (!expected) {
    throw new AppError(415, "attachment_type_not_allowed", "The attachment extension is not allowed.");
  }
  let detected;
  try {
    detected = await fileTypeFromFile(upload.temporaryPath);
  } catch {
    detected = undefined;
  }
  if (!detected || detected.ext !== expected) {
    throw new AppError(415, "attachment_type_not_allowed", "The file content does not match its extension.");
  }
  return detected.mime;
}

async function existingUpload(db, actorId, uploadId) {
  const result = await db.query(
    `SELECT * FROM attachments
      WHERE uploaded_by = $1 AND upload_id = $2`,
    [actorId, uploadId],
  );
  return result.rows[0] || null;
}

function replayOrConflict(existing, upload, teamId) {
  if (existing.team_id !== teamId || existing.sha256 !== upload.sha256) {
    throw new AppError(
      409,
      "idempotency_key_reused",
      "The upload idempotency key belongs to different content.",
    );
  }
  return attachmentFromRow(existing);
}

export async function uploadAttachment(pool, token, uploadIdValue, request, config) {
  const uploadId = uuid(uploadIdValue, "Idempotency-Key");
  const auth = await authenticate(pool, token);
  const upload = await receiveMultipart(request, config);
  try {
    await requireMembership(pool, upload.teamId, auth.principalId);
    const contentType = await detectContent(upload);

    const attachmentId = newId();
    const storageKey = `${attachmentId.slice(0, 2)}/${attachmentId}`;
    const finalPath = storagePath(config, storageKey);
    await mkdir(resolve(config.attachmentDir, attachmentId.slice(0, 2)), {
      recursive: true,
      mode: 0o700,
    });
    await chmod(resolve(config.attachmentDir, attachmentId.slice(0, 2)), 0o700);
    await link(upload.temporaryPath, finalPath);
    await rm(upload.temporaryPath, { force: true });
    await chmod(finalPath, 0o600);

    try {
      const result = await inTransaction(pool, async (client) => {
        const team = await client.query(
          "SELECT id FROM teams WHERE id = $1 FOR UPDATE",
          [upload.teamId],
        );
        if (!team.rowCount) {
          throw new AppError(404, "resource_not_found", "The team was not found.");
        }
        await requireMembership(client, upload.teamId, auth.principalId);
        const raced = await existingUpload(client, auth.principalId, uploadId);
        if (raced) return { row: raced, replayed: true };
        const usage = await client.query(
          `SELECT coalesce(sum(byte_size), 0)::bigint AS bytes
             FROM attachments
            WHERE team_id = $1 AND deleted_at IS NULL`,
          [upload.teamId],
        );
        if (Number(usage.rows[0].bytes) + upload.byteSize > config.maxTeamAttachmentBytes) {
          throw new AppError(
            413,
            "team_attachment_capacity_exceeded",
            "The team attachment capacity would be exceeded.",
          );
        }
        const inserted = await client.query(
          `INSERT INTO attachments (
             id, team_id, uploaded_by, upload_id, storage_key, original_name,
             content_type, byte_size, sha256
           ) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
           RETURNING *`,
          [
            attachmentId,
            upload.teamId,
            auth.principalId,
            uploadId,
            storageKey,
            upload.filename,
            contentType,
            upload.byteSize,
            upload.sha256,
          ],
        );
        return { row: inserted.rows[0], replayed: false };
      });
      if (result.replayed) {
        await rm(finalPath, { force: true });
        return {
          attachment: replayOrConflict(result.row, upload, upload.teamId),
          replayed: true,
        };
      }
      return { attachment: attachmentFromRow(result.row), replayed: false };
    } catch (error) {
      await rm(finalPath, { force: true });
      if (error?.code === "23505") {
        const raced = await existingUpload(pool, auth.principalId, uploadId);
        if (raced) {
          return {
            attachment: replayOrConflict(raced, upload, upload.teamId),
            replayed: true,
          };
        }
      }
      throw error;
    }
  } catch (error) {
    await rm(upload.temporaryPath, { force: true });
    throw error;
  }
}

export async function bindAttachments(client, { teamId, actorId, attachmentIds, actionId }) {
  if (attachmentIds.length === 0) return;
  const result = await client.query(
    `SELECT * FROM attachments
      WHERE id = ANY($1::uuid[])
      FOR UPDATE`,
    [attachmentIds],
  );
  const byId = new Map(result.rows.map((row) => [row.id, row]));
  for (const attachmentId of attachmentIds) {
    const row = byId.get(attachmentId);
    if (
      !row
      || row.team_id !== teamId
      || row.uploaded_by !== actorId
      || row.bound_at
      || row.deleted_at
    ) {
      throw new AppError(
        422,
        "attachment_not_ready",
        "An attachment is unavailable, already bound or owned by another member.",
      );
    }
  }
  for (const [ordinal, attachmentId] of attachmentIds.entries()) {
    await client.query(
      `INSERT INTO action_attachments (action_id, attachment_id, ordinal)
       VALUES ($1, $2, $3)`,
      [actionId, attachmentId, ordinal],
    );
  }
  await client.query(
    "UPDATE attachments SET bound_at = now() WHERE id = ANY($1::uuid[])",
    [attachmentIds],
  );
}

async function authorizedAttachment(pool, token, rawAttachmentId) {
  const attachmentId = uuid(rawAttachmentId, "attachment_id");
  const auth = await authenticate(pool, token);
  const result = await pool.query(
    `SELECT a.*, aa.action_id, ra.thread_id
       FROM attachments a
       JOIN memberships m
         ON m.team_id = a.team_id AND m.principal_id = $2 AND m.removed_at IS NULL
       LEFT JOIN action_attachments aa ON aa.attachment_id = a.id
       LEFT JOIN relay_actions ra ON ra.id = aa.action_id
      WHERE a.id = $1`,
    [attachmentId, auth.principalId],
  );
  if (result.rowCount === 0) {
    throw new AppError(404, "resource_not_found", "The attachment was not found.");
  }
  const row = result.rows[0];
  if (!row.action_id) {
    if (row.uploaded_by !== auth.principalId) {
      throw new AppError(403, "thread_access_denied", "The attachment is not visible to this member.");
    }
  } else {
    const participant = await pool.query(
      `SELECT 1 FROM thread_participants
        WHERE thread_id = $1 AND principal_id = $2 AND material_access_at IS NOT NULL`,
      [row.thread_id, auth.principalId],
    );
    if (participant.rowCount === 0) {
      throw new AppError(403, "thread_access_denied", "The attachment is not visible to this member.");
    }
  }
  return row;
}

export async function getAttachment(pool, token, attachmentId) {
  return attachmentFromRow(await authorizedAttachment(pool, token, attachmentId));
}

export async function openAttachmentContent(pool, token, attachmentId, config) {
  const row = await authorizedAttachment(pool, token, attachmentId);
  if (row.deleted_at) {
    throw new AppError(410, "attachment_expired", "The attachment content is no longer retained.");
  }
  const handle = await open(storagePath(config, row.storage_key), constants.O_RDONLY | constants.O_NOFOLLOW);
  const fileStat = await handle.stat();
  if (!fileStat.isFile() || fileStat.size !== Number(row.byte_size)) {
    await handle.close();
    throw new AppError(503, "service_unavailable", "The attachment storage is inconsistent.");
  }
  return { row, handle };
}

export { attachmentFromRow, isPathInside, storagePath };
