import crypto from "node:crypto";
import { constants } from "node:fs";
import fs from "node:fs/promises";
import { resolve, sep } from "node:path";

const TEXT_KINDS = new Set(["trace", "check", "log", "ui-smoke", "rollback"]);
const CHUNK_SIZE = 64 * 1024;
function fail(code) { throw Object.assign(new Error(code), { code }); }

// Path-bearing references may share bytes (notably A/B's installer). A bare
// digest cannot choose between signed identities, even if their kinds differ.
export function artifactIndex(artifacts) {
  const entries = new Map();
  for (const item of artifacts) {
    const group = entries.get(item.sha256) || [];
    group.push(item);
    entries.set(item.sha256, group);
  }
  return {
    get(sha256) {
      const group = entries.get(sha256);
      if (group?.length > 1) fail("evidence_artifact_digest_ambiguous");
      return group?.[0];
    },
  };
}

function sameFile(left, right) {
  return ["dev", "ino", "size", "mtimeNs", "ctimeNs"].every((key) => left[key] === right[key]);
}

async function checkedPath(root, relative) {
  const metadata = await fs.lstat(root, { bigint: true });
  if (!metadata.isDirectory() || metadata.isSymbolicLink()) fail("evidence_root_unsafe");
  const realRoot = await fs.realpath(root);
  const path = resolve(root, ...relative.split("/"));
  if (path === root || !path.startsWith(`${root}${sep}`)) fail("evidence_artifact_path_escape");
  let cursor = root;
  let target;
  for (const component of relative.split("/")) {
    cursor = resolve(cursor, component);
    target = await fs.lstat(cursor, { bigint: true });
    if (target.isSymbolicLink()) fail("evidence_artifact_path_escape");
  }
  const realTarget = await fs.realpath(path);
  if (!realTarget.startsWith(`${realRoot}${sep}`)) fail("evidence_artifact_path_escape");
  return { path, target, realRoot, realTarget, rootMetadata: metadata };
}

// All consumers, including later JSON parsing, use this boundary. Never reopen
// a previously hashed path with an unbounded readFile: it may have changed.
export async function readEvidenceArtifact(evidenceRoot, item, { maxBytes } = {}) {
  const root = resolve(evidenceRoot);
  const before = await checkedPath(root, item.path);
  const defaultMaxSize = TEXT_KINDS.has(item.kind) ? 32 * 1024 * 1024 : 1024 * 1024 * 1024;
  if (maxBytes !== undefined && (!Number.isSafeInteger(maxBytes) || maxBytes <= 0 || maxBytes > defaultMaxSize)) fail("evidence_artifact_size_invalid");
  const maxSize = maxBytes ?? defaultMaxSize;
  const checkSize = (metadata) => {
    if (!metadata.isFile() || metadata.size <= 0n || metadata.size > BigInt(maxSize)) fail("evidence_artifact_size_invalid");
  };
  checkSize(before.target);
  // A regular file can be replaced by a FIFO after lstat. Where supported,
  // nonblocking open keeps the descriptor type check reachable without a writer.
  const handle = await fs.open(before.path, constants.O_RDONLY | (constants.O_NOFOLLOW || 0) | (constants.O_NONBLOCK || 0));
  try {
    const metadata = await handle.stat({ bigint: true });
    checkSize(metadata);
    if (!sameFile(before.target, metadata)) fail("evidence_artifact_changed");
    const size = Number(metadata.size);
    const hash = crypto.createHash("sha256");
    const chunks = TEXT_KINDS.has(item.kind) ? [] : null;
    const buffer = Buffer.alloc(CHUNK_SIZE);
    let offset = 0;
    while (offset < size) {
      const { bytesRead } = await handle.read(buffer, 0, Math.min(CHUNK_SIZE, size - offset), offset);
      if (!bytesRead) fail("evidence_artifact_changed");
      const chunk = buffer.subarray(0, bytesRead);
      hash.update(chunk);
      if (chunks) chunks.push(Buffer.from(chunk));
      offset += bytesRead;
    }
    if ((await handle.read(buffer, 0, 1, offset)).bytesRead !== 0) fail("evidence_artifact_changed");
    if (!sameFile(metadata, await handle.stat({ bigint: true }))) fail("evidence_artifact_changed");
    const after = await checkedPath(root, item.path);
    if (!sameFile(metadata, after.target) || before.realRoot !== after.realRoot
      || before.realTarget !== after.realTarget
      || before.rootMetadata.dev !== after.rootMetadata.dev || before.rootMetadata.ino !== after.rootMetadata.ino) fail("evidence_artifact_changed");
    if (hash.digest("hex") !== item.sha256) fail("evidence_artifact_digest_mismatch");
    return chunks ? Buffer.concat(chunks, size) : null;
  } finally {
    await handle.close();
  }
}
