const MAX_FILE_BYTES = 32 * 1024 * 1024;
const MAX_TOTAL_BYTES = 64 * 1024 * 1024;
const MAX_FILES = 8;

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

export async function uploadRemoteFiles(api, binding, taskId, files, onProgress = () => {}) {
  const selected = validateRemoteFiles(files);
  const attachmentIds = [];
  for (let fileIndex = 0; fileIndex < selected.length; fileIndex += 1) {
    const file = selected[fileIndex];
    const buffer = await file.arrayBuffer();
    const bytes = new Uint8Array(buffer);
    const sha256 = hex(new Uint8Array(await crypto.subtle.digest('SHA-256', buffer)));
    const begin = await api.attachmentBegin(
      binding.localSessionId, binding.remoteRef, taskId,
      file.name, file.type || 'application/octet-stream', bytes.length, sha256,
    );
    let completed = false;
    try {
      const chunkBytes = Math.max(1, Math.min(Number(begin.chunkBytes) || 1, 256 * 1024));
      for (let offset = 0, chunkIndex = 0; offset < bytes.length; offset += chunkBytes, chunkIndex += 1) {
        const chunk = bytes.subarray(offset, Math.min(offset + chunkBytes, bytes.length));
        const receipt = await api.attachmentChunk(
          binding.localSessionId, binding.remoteRef, taskId, begin.transferId,
          chunkIndex, bytesToBase64(chunk), chunk.length,
        );
        onProgress({ fileIndex, fileName: file.name, receivedBytes: receipt.receivedBytes, totalBytes: bytes.length });
        completed = receipt.state === 'completed';
      }
      if (!completed) throw new Error('remote_attachment_incomplete');
      attachmentIds.push(begin.transferId);
    } catch (error) {
      if (error?.code !== 'result_unknown') {
        await api.attachmentAbort(binding.localSessionId, binding.remoteRef, taskId, begin.transferId).catch(() => {});
      }
      throw error;
    }
  }
  return attachmentIds;
}

export async function downloadRemoteArtifact(api, binding, artifact) {
  const begin = await api.artifactDownloadBegin(
    binding.localSessionId, binding.remoteRef, artifact.taskId, artifact.artifactId,
  );
  const chunks = [];
  let nextChunk = 0;
  try {
    for (;;) {
      const receipt = await api.artifactDownloadChunk(
        binding.localSessionId, binding.remoteRef, artifact.taskId, artifact.artifactId,
        begin.transferId, nextChunk,
      );
      const bytes = base64ToBytes(receipt.chunkB64);
      if (bytes.length !== receipt.chunkBytes) throw new Error('remote_artifact_chunk_invalid');
      chunks.push(bytes);
      if (receipt.completed) break;
      if (!Number.isSafeInteger(receipt.nextChunk) || receipt.nextChunk <= nextChunk) {
        throw new Error('remote_artifact_chunk_invalid');
      }
      nextChunk = receipt.nextChunk;
    }
  } catch (error) {
    if (error?.code !== 'result_unknown') {
      await api.artifactDownloadAbort(
        binding.localSessionId, binding.remoteRef, artifact.taskId, artifact.artifactId, begin.transferId,
      ).catch(() => {});
    }
    throw error;
  }
  const blob = new Blob(chunks, { type: artifact.mimeType || 'application/octet-stream' });
  if (blob.size !== begin.totalBytes) throw new Error('remote_artifact_size_mismatch');
  if (typeof artifact.sha256 === 'string' && /^[a-f0-9]{64}$/.test(artifact.sha256)) {
    const digest = hex(new Uint8Array(await crypto.subtle.digest('SHA-256', await blob.arrayBuffer())));
    if (digest !== artifact.sha256) throw new Error('remote_artifact_checksum_mismatch');
  }
  return blob;
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
