import { lstat, readdir, rm } from "node:fs/promises";
import { resolve } from "node:path";
import { inTransaction } from "./db.js";
import { storagePath } from "./attachment-service.js";
import { avatarStoragePath } from "./avatar-storage.js";

const UUID_FILE_PATTERN = /^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i;
const AVATAR_SHARD_PATTERN = /^[0-9a-f]{2}$/;

async function removeStaleTemporaryFilesAt(temporaryDir, cutoff) {
  let names;
  try {
    names = await readdir(temporaryDir);
  } catch (error) {
    if (error?.code === "ENOENT") return 0;
    throw error;
  }
  let removed = 0;
  for (const name of names) {
    if (!UUID_FILE_PATTERN.test(name)) continue;
    const path = resolve(temporaryDir, name);
    let metadata;
    try {
      metadata = await lstat(path);
    } catch (error) {
      if (error?.code === "ENOENT") continue;
      throw error;
    }
    if ((!metadata.isFile() && !metadata.isSymbolicLink()) || metadata.mtimeMs >= cutoff) continue;
    await rm(path, { force: true });
    removed += 1;
  }
  return removed;
}

async function removeStaleTemporaryFiles(config) {
  const cutoff = Date.now() - config.orphanAttachmentHours * 60 * 60 * 1000;
  return removeStaleTemporaryFilesAt(resolve(config.attachmentDir, ".tmp"), cutoff);
}

async function removeAvatarOrphans(pool, config) {
  const root = resolve(config.attachmentDir, "avatars");
  const temporary = await removeStaleTemporaryFilesAt(
    resolve(root, ".tmp"),
    Date.now() - config.orphanAttachmentHours * 60 * 60 * 1000,
  );
  let shards;
  try {
    shards = await readdir(root, { withFileTypes: true });
  } catch (error) {
    if (error?.code === "ENOENT") return { temporary, orphaned: 0 };
    throw error;
  }

  const cutoff = Date.now() - config.orphanAttachmentHours * 60 * 60 * 1000;
  let candidates = [];
  let orphaned = 0;
  async function flushCandidates() {
    if (candidates.length === 0) return;
    const referenced = await pool.query(
      `SELECT storage_key FROM principal_avatars WHERE storage_key = ANY($1::text[])
        UNION
       SELECT storage_key FROM principal_avatar_variants WHERE storage_key = ANY($1::text[])`,
      [candidates.map(candidate => candidate.storageKey)],
    );
    const referencedKeys = new Set(referenced.rows.map(row => row.storage_key));
    for (const candidate of candidates) {
      if (orphaned >= 500) break;
      if (!referencedKeys.has(candidate.storageKey)) {
        await rm(candidate.path, { force: true });
        orphaned += 1;
      }
    }
    candidates = [];
  }

  for (const shard of shards.sort((left, right) => left.name.localeCompare(right.name))) {
    if (orphaned >= 500) break;
    if (!shard.isDirectory() || !AVATAR_SHARD_PATTERN.test(shard.name)) continue;
    const shardPath = resolve(root, shard.name);
    const names = (await readdir(shardPath)).sort();
    for (const name of names) {
      if (!UUID_FILE_PATTERN.test(name)) continue;
      const storageKey = `${shard.name}/${name}`;
      const path = avatarStoragePath(config, storageKey);
      let metadata;
      try {
        metadata = await lstat(path);
      } catch (error) {
        if (error?.code === "ENOENT") continue;
        throw error;
      }
      if ((!metadata.isFile() && !metadata.isSymbolicLink()) || metadata.mtimeMs >= cutoff) continue;
      candidates.push({ storageKey, path });
      if (candidates.length >= 500) await flushCandidates();
      if (orphaned >= 500) break;
    }
  }
  if (orphaned < 500) await flushCandidates();
  return { temporary, orphaned };
}

async function removeOrphan(pool, config, candidate) {
  return inTransaction(pool, async (client) => {
    const locked = await client.query(
      `SELECT id, storage_key FROM attachments
        WHERE id = $1 AND bound_at IS NULL AND deleted_at IS NULL
          AND created_at < now() - ($2 * interval '1 hour')
        FOR UPDATE`,
      [candidate.id, config.orphanAttachmentHours],
    );
    if (locked.rowCount === 0) return false;
    await rm(storagePath(config, locked.rows[0].storage_key), { force: true });
    await client.query("DELETE FROM attachments WHERE id = $1", [candidate.id]);
    return true;
  });
}

export async function expireClosed(pool, config, candidate, removeFile = rm) {
  const available = await pool.query(
    `SELECT storage_key FROM attachments
      WHERE id = $1 AND deleted_at IS NULL`,
    [candidate.id],
  );
  if (available.rowCount === 0) return false;
  await removeFile(storagePath(config, available.rows[0].storage_key), { force: true });
  const marked = await pool.query(
    `UPDATE attachments SET deleted_at = now()
      WHERE id = $1 AND deleted_at IS NULL
      RETURNING id`,
    [candidate.id],
  );
  return marked.rowCount !== 0;
}

export async function cleanupAttachments(pool, config) {
  const temporary = await removeStaleTemporaryFiles(config);
  const avatars = await removeAvatarOrphans(pool, config);
  const orphanCandidates = await pool.query(
    `SELECT id, storage_key FROM attachments
      WHERE bound_at IS NULL AND deleted_at IS NULL
        AND created_at < now() - ($1 * interval '1 hour')
      ORDER BY created_at
      LIMIT 500`,
    [config.orphanAttachmentHours],
  );
  let orphaned = 0;
  for (const candidate of orphanCandidates.rows) {
    if (await removeOrphan(pool, config, candidate)) orphaned += 1;
  }

  const closedCandidates = await pool.query(
    `SELECT DISTINCT a.id, a.storage_key
       FROM attachments a
       JOIN action_attachments aa ON aa.attachment_id = a.id
       JOIN relay_actions ra ON ra.id = aa.action_id
       JOIN work_threads w ON w.id = ra.thread_id
      WHERE a.deleted_at IS NULL AND w.phase IN ('closed', 'cancelled')
        AND w.updated_at < now() - ($1 * interval '1 day')
      ORDER BY a.id
      LIMIT 500`,
    [config.closedAttachmentDays],
  );
  let expired = 0;
  for (const candidate of closedCandidates.rows) {
    if (await expireClosed(pool, config, candidate)) expired += 1;
  }
  return {
    temporary,
    orphaned,
    expired,
    avatarTemporary: avatars.temporary,
    avatarOrphaned: avatars.orphaned,
  };
}

export { removeAvatarOrphans, removeStaleTemporaryFilesAt };
