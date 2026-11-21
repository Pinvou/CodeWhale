import assert from 'node:assert/strict';
import test from 'node:test';
import { downloadRemoteArtifact } from '../src/features/remote-control/remote-file-transfer.mjs';

const binding = { localSessionId: 'session-one', remoteRef: 'remote-one' };
const artifact = { taskId: 'task-one', artifactId: 'artifact-one', mimeType: 'application/octet-stream' };
function apiFor(totalBytes, next, chunkBytes = 2) {
  const state = { calls: 0, aborts: 0 };
  const api = {
    artifactDownloadBegin: async () => ({ transferId: 'download-one', totalBytes, chunkBytes }),
    artifactDownloadChunk: async (...args) => { state.calls++; return next(args.at(-1)); },
    artifactDownloadAbort: async (...args) => {
      assert.deepEqual(args, ['session-one', 'remote-one', 'task-one', 'artifact-one', 'download-one']);
      state.aborts++;
    },
  };
  return { state, api };
}
const receipt = (index, total) => ({ chunkIndex: index, chunkB64: 'AQ==', chunkBytes: 1,
  receivedBytes: index + 1, nextChunk: index + 1, completed: index + 1 === total });

test('download accepts exactly4096 contiguous small chunks and refuses request4097', async () => {
  const exact = apiFor(4096, index => receipt(index, 4096));
  const blob = await downloadRemoteArtifact(exact.api, binding, artifact);
  assert.equal(blob.size, 4096);
  assert.deepEqual(new Uint8Array(await blob.arrayBuffer()), new Uint8Array(4096).fill(1));
  // Verified bytes are returned after the exact completed receipt is retired.
  assert.deepEqual(exact.state, { calls: 4096, aborts: 1 });
  // Advertised two-byte chunks can fit; one-byte actual reads consume the
  // independent count ceiling before the declared byte ceiling is reached.
  const over = apiFor(4097, index => receipt(index, 4097));
  await assert.rejects(downloadRemoteArtifact(over.api, binding, artifact), /remote_artifact_chunk_limit/);
  assert.deepEqual(over.state, { calls: 4096, aborts: 1 });
});

test('download rejects empty, duplicate, skipped, inconsistent and premature chunks', async t => {
  const changes = {
    empty: { chunkB64: '', chunkBytes: 0 },
    duplicate: { chunkIndex: -1 },
    skipped: { chunkIndex: 1 },
    nextGap: { nextChunk: 2 },
    nextDuplicate: { nextChunk: 0 },
    cumulative: { receivedBytes: 2 },
    premature: { completed: true },
    nonBooleanCompletion: { completed: 'false' },
    encodedOversize: { chunkB64: 'AQ==AQ==' },
  };
  for (const [name, change] of Object.entries(changes)) {
    await t.test(name, async () => {
      const { api, state } = apiFor(2, index => ({ ...receipt(index, 2), ...change }));
      await assert.rejects(downloadRemoteArtifact(api, binding, artifact), /remote_artifact_chunk_invalid/);
      assert.deepEqual(state, { calls: 1, aborts: 1 });
    });
  }
  const notCompleted = apiFor(1, index => ({ ...receipt(index, 1), completed: false }));
  await assert.rejects(downloadRemoteArtifact(notCompleted.api, binding, artifact), /remote_artifact_chunk_invalid/);
  assert.deepEqual(notCompleted.state, { calls: 1, aborts: 1 });
});

test('checksum mismatch aborts the exact completed handle and never returns success', async () => {
  const { api, state } = apiFor(1, index => receipt(index, 1));
  await assert.rejects(downloadRemoteArtifact(api, binding, { ...artifact, sha256: '0'.repeat(64) }), /remote_artifact_checksum_mismatch/);
  assert.deepEqual(state, { calls: 1, aborts: 1 });
});
