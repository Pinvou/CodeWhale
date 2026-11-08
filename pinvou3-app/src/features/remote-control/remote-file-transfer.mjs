import '../../shared/chunked-file-upload.js';

// B's staged attachment ingestion shares the device upload limit. Artifact
// downloads use an independent transfer limit below.
const MAX_FILE_BYTES = (typeof window !== 'undefined' ? window : globalThis).PinvouChunkedFileUpload.MAX_FILE_BYTES;
const MAX_TOTAL_BYTES = 64 * 1024 * 1024;
const MAX_FILES = 8;
// Downloads have their own 32 MiB transfer bound. Without it a misbehaving or compromised B
// could keep a `completed:false` chunk stream going and grow this WebView's
// memory without bound.
const MAX_DOWNLOAD_BYTES = 32 * 1024 * 1024;
// Unknown outcomes are retained for the lifetime of this process. Losing a
// begin reply can also lose the server-minted handle, so retrying the batch
// cannot safely be treated as a fresh operation.
const uploadAttempts = new Map();
const MAX_UPLOAD_ATTEMPTS = 128;
let currentUploadLease = '';

function uncertainUpload() {
  const error = new Error('result_unknown');
  error.code = 'result_unknown';
  return error;
}

function uploadAttemptKey(owner, binding, taskId) {
  return JSON.stringify([owner, binding.localSessionId, binding.remoteRef, taskId]);
}

export function retireCompletedRemoteUpload(owner, binding, taskId) {
  const key = uploadAttemptKey(String(owner || ''), binding, taskId);
  if (uploadAttempts.get(key)?.state === 'completed') uploadAttempts.delete(key);
}

export async function abortCompletedRemoteUpload(api, owner, binding, taskId, isCurrent = () => true) {
  const key = uploadAttemptKey(String(owner || ''), binding, taskId);
  const attempt = uploadAttempts.get(key);
  if (!attempt || attempt.state !== 'completed') return false;
  attempt.state = 'cleaning';
  for (const transferId of attempt.knownIds) {
    if (!isCurrent()) { attempt.state = 'unknown'; return false; }
    try {
      const receipt = await api.attachmentAbort(binding.localSessionId, binding.remoteRef, taskId, transferId);
      if (!receipt || (receipt.aborted !== true && receipt.already_absent !== true)) { attempt.state = 'unknown'; return false; }
    } catch (_) {
      attempt.state = 'unknown';
      return false;
    }
  }
  if (uploadAttempts.get(key) === attempt) uploadAttempts.delete(key);
  return true;
}

// Operation identity is revision-bound; finishing an old operation may never
// clear a newer upload. Tokens are local UI ownership, not remote authority.
export function createRemoteTransferGuard() {
  let owner = null;
  return {
    start(identity) {
      owner = Object.freeze({ identity });
      return owner;
    },
    isCurrent(attempt, identity) {
      return !!attempt && owner === attempt && attempt.identity === identity;
    },
    finish(attempt) {
      if (!attempt || owner !== attempt) return false;
      owner = null;
      return true;
    },
    invalidate() { owner = null; },
  };
}

function hex(bytes) {
  return Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('');
}

function bytesToBase64(bytes) {
  let binary = '';
  const step = 0x8000;
  for (let offset = 0; offset < bytes.length; offset += step) {
    binary += String.fromCharCode(...bytes.subarray(offset, offset + step));
  }
  return btoa(binary);
}

function base64ToBytes(value) {
  const binary = atob(value);
  const bytes = new Uint8Array(binary.length);
  for (let index = 0; index < binary.length; index += 1) bytes[index] = binary.charCodeAt(index);
  return bytes;
}

export function validateRemoteFiles(files) {
  const selected = Array.from(files || []);
  if (!selected.length || selected.length > MAX_FILES) throw new Error('remote_attachment_count_invalid');
  if (selected.some(file => !file || !Number.isSafeInteger(file.size) || file.size <= 0 || file.size > MAX_FILE_BYTES)) {
    throw new Error('remote_attachment_size_invalid');
  }
  if (selected.reduce((total, file) => total + file.size, 0) > MAX_TOTAL_BYTES) {
    throw new Error('remote_attachment_total_size_invalid');
  }
  if (selected.some(file => typeof file.name !== 'string' || !file.name
    || [...file.name].length > 200 || String(file.type || 'application/octet-stream').length > 120)) {
    throw new Error('remote_attachment_metadata_invalid');
  }
  return selected;
}

export async function uploadRemoteFiles(api, binding, taskId, files, onProgress = () => {}, options = {}) {
  const selected = validateRemoteFiles(files);
  const owner = String(options.ownerIdentity || '');
  const lease = String(options.leaseIdentity || '');
  const key = uploadAttemptKey(owner, binding, taskId);
  const isCurrent = typeof options.isCurrent === 'function' ? options.isCurrent : () => true;
  if (!isCurrent()) throw uncertainUpload();
  if (lease && lease !== currentUploadLease) {
    for (const [oldKey, oldAttempt] of uploadAttempts) {
      if (oldAttempt.lease && oldAttempt.lease !== lease) uploadAttempts.delete(oldKey);
    }
    currentUploadLease = lease;
  }
  const previous = uploadAttempts.get(key);
  if (previous?.state === 'completed' && previous.files.length === selected.length
    && previous.files.every((file, index) => file === selected[index]) && isCurrent()) return previous.knownIds.slice();
  // Abandoning a task does not prove an uncertain upload was absent. Fence the
  // binding across task IDs, including reselected File objects, until the
  // connection changes or the exact completed batch is safely retired.
  const matching = Array.from(uploadAttempts.values()).find(attempt =>
    attempt.lease === lease && attempt.localSessionId === binding.localSessionId
    && attempt.remoteRef === binding.remoteRef);
  if (previous?.state === 'unknown' || matching?.state === 'unknown') {
    const error = new Error('remote_attachment_fenced');
    error.code = 'remote_attachment_fenced';
    throw error;
  }
  if (previous || matching) throw uncertainUpload();
  if (uploadAttempts.size >= MAX_UPLOAD_ATTEMPTS) {
    const error = new Error('remote_attachment_capacity');
    error.code = 'remote_attachment_capacity';
    throw error;
  }
  const attempt = { state: 'uploading', knownIds: [], files: selected, lease,
    localSessionId: binding.localSessionId, remoteRef: binding.remoteRef };
  uploadAttempts.set(key, attempt);
  const attachmentIds = [];
  try {
  for (let fileIndex = 0; fileIndex < selected.length; fileIndex += 1) {
    if (!isCurrent()) throw uncertainUpload();
    const file = selected[fileIndex];
    const buffer = await file.arrayBuffer();
    if (!isCurrent()) throw uncertainUpload();
    const bytes = new Uint8Array(buffer);
    const sha256 = hex(new Uint8Array(await crypto.subtle.digest('SHA-256', buffer)));
    if (!isCurrent()) throw uncertainUpload();
    const begin = await api.attachmentBegin(
      binding.localSessionId, binding.remoteRef, taskId,
      file.name, file.type || 'application/octet-stream', bytes.length, sha256,
    );
    if (!begin || typeof begin.transferId !== 'string' || !begin.transferId) throw uncertainUpload();
    attempt.knownIds.push(begin.transferId);
    if (!isCurrent()) throw uncertainUpload();
    let completed = false;
    const chunkBytes = Math.max(1, Math.min(Number(begin.chunkBytes) || 1, 256 * 1024));
      for (let offset = 0, chunkIndex = 0; offset < bytes.length; offset += chunkBytes, chunkIndex += 1) {
        if (!isCurrent()) throw uncertainUpload();
        const chunk = bytes.subarray(offset, Math.min(offset + chunkBytes, bytes.length));
        const receipt = await api.attachmentChunk(
          binding.localSessionId, binding.remoteRef, taskId, begin.transferId,
          chunkIndex, bytesToBase64(chunk), chunk.length,
        );
        if (!isCurrent()) throw uncertainUpload();
        onProgress({ fileIndex, fileName: file.name, receivedBytes: receipt.receivedBytes, totalBytes: bytes.length });
        completed = receipt.state === 'completed';
      }
      if (!completed) throw new Error('remote_attachment_incomplete');
      attachmentIds.push(begin.transferId);

  }
  if (!isCurrent()) throw uncertainUpload();
  attempt.state = 'completed';
  if (!owner) uploadAttempts.delete(key);
  return attachmentIds;
  } catch (error) {
    if (error?.code === 'result_unknown' || !isCurrent()) {
      attempt.state = 'unknown';
      throw uncertainUpload();
    }
    // No task commit has been attempted: only this batch's known, unconsumed
    // handles may be revoked, and a failed or unknown abort remains fenced.
    for (const transferId of attempt.knownIds) {
      if (!isCurrent()) { attempt.state = 'unknown'; throw uncertainUpload(); }
      try {
        const receipt = await api.attachmentAbort(binding.localSessionId, binding.remoteRef, taskId, transferId);
        if (!receipt || (receipt.aborted !== true && receipt.already_absent !== true)) throw uncertainUpload();
      } catch (_) {
        attempt.state = 'unknown';
        throw uncertainUpload();
      }
    }
    uploadAttempts.delete(key);
    throw error;
  }
}

export async function downloadRemoteArtifact(api, binding, artifact) {
  const begin = await api.artifactDownloadBegin(
    binding.localSessionId, binding.remoteRef, artifact.taskId, artifact.artifactId,
  );
  const chunks = [];
  let nextChunk = 0;
  let receivedBytes = 0;
  try {
    if (!Number.isSafeInteger(begin.totalBytes) || begin.totalBytes < 0
      || begin.totalBytes > MAX_DOWNLOAD_BYTES) {
      throw new Error('remote_artifact_size_limit');
    }
    for (;;) {
      const receipt = await api.artifactDownloadChunk(
        binding.localSessionId, binding.remoteRef, artifact.taskId, artifact.artifactId,
        begin.transferId, nextChunk,
      );
      const bytes = base64ToBytes(receipt.chunkB64);
      if (bytes.length !== receipt.chunkBytes) throw new Error('remote_artifact_chunk_invalid');
      receivedBytes += bytes.length;
      if (receivedBytes > begin.totalBytes || receivedBytes > MAX_DOWNLOAD_BYTES) {
        throw new Error('remote_artifact_size_limit');
      }
      chunks.push(bytes);
      if (receipt.completed) break;
      if (!Number.isSafeInteger(receipt.nextChunk) || receipt.nextChunk <= nextChunk) {
        throw new Error('remote_artifact_chunk_invalid');
      }
      nextChunk = receipt.nextChunk;
    }
    const blob = new Blob(chunks, { type: artifact.mimeType || 'application/octet-stream' });
    if (blob.size !== begin.totalBytes) throw new Error('remote_artifact_size_mismatch');
    if (typeof artifact.sha256 === 'string' && /^[a-f0-9]{64}$/.test(artifact.sha256)) {
      const digest = hex(new Uint8Array(await crypto.subtle.digest('SHA-256', await blob.arrayBuffer())));
      if (digest !== artifact.sha256) throw new Error('remote_artifact_checksum_mismatch');
    }
    // Bytes are now verified locally. Retire the server's final-response
    // receipt through the existing exact-bound cleanup operation. A lost
    // cleanup reply cannot invalidate the completed local download; TTL is
    // the fallback for that receipt.
    await api.artifactDownloadAbort(
      binding.localSessionId, binding.remoteRef, artifact.taskId, artifact.artifactId, begin.transferId,
    ).catch(() => {});
    return blob;
  } catch (error) {
    if (error?.code !== 'result_unknown') {
      await api.artifactDownloadAbort(
        binding.localSessionId, binding.remoteRef, artifact.taskId, artifact.artifactId, begin.transferId,
      ).catch(() => {});
    }
    throw error;
  }
}

export function saveRemoteArtifactBlob(blob, fileName) {
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement('a');
  anchor.href = url;
  anchor.download = fileName || 'artifact';
  anchor.style.display = 'none';
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  setTimeout(() => URL.revokeObjectURL(url), 0);
}
