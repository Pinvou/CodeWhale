import assert from 'node:assert/strict';
import test from 'node:test';
import { webcrypto } from 'node:crypto';
import {
  downloadRemoteArtifact,
  uploadRemoteFiles,
  validateRemoteFiles,
} from '../src/features/remote-control/remote-file-transfer.mjs';

globalThis.crypto ||= webcrypto;

function file(name, content, type = 'text/plain') {
  const bytes = new TextEncoder().encode(content);
  return { name, type, size: bytes.length, arrayBuffer: async () => bytes.buffer.slice(0) };
}

test('remote attachment upload hashes bytes and commits only B-confirmed transfer IDs', async () => {
  const chunks = [];
  const api = {
    attachmentBegin: async (_local, _remote, taskId, name, type, size, sha256) => {
      assert.equal(taskId, 'task-1');
      assert.equal(name, 'hello.txt');
      assert.equal(type, 'text/plain');
      assert.equal(size, 5);
      assert.equal(sha256, '2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824');
      return { transferId: 'transfer-1', chunkBytes: 2, totalBytes: 5 };
    },
    attachmentChunk: async (_local, _remote, _task, transferId, index, chunkB64, chunkBytes) => {
      chunks.push({ transferId, index, chunkB64, chunkBytes });
      return { state: index === 2 ? 'completed' : 'receiving', receivedBytes: Math.min((index + 1) * 2, 5) };
    },
    attachmentAbort: async () => assert.fail('completed upload must not abort'),
  };
  const progress = [];
  const ids = await uploadRemoteFiles(api, { localSessionId: 'remote-one', remoteRef: 'ref-one' },
    'task-1', [file('hello.txt', 'hello')], item => progress.push(item));
  assert.deepEqual(ids, ['transfer-1']);
  assert.deepEqual(chunks.map(item => item.chunkBytes), [2, 2, 1]);
  assert.equal(progress.at(-1).receivedBytes, 5);
});

test('artifact download preserves ordered chunks and aborts malformed progress', async () => {
  const api = {
    artifactDownloadBegin: async () => ({ transferId: 'download-1', chunkBytes: 2, totalBytes: 5 }),
    artifactDownloadChunk: async (_local, _remote, _task, _artifact, _transfer, index) => ({
      chunkIndex: index,
      chunkB64: Buffer.from(index === 0 ? 'he' : index === 1 ? 'll' : 'o').toString('base64'),
      chunkBytes: index === 2 ? 1 : 2,
      receivedBytes: Math.min((index + 1) * 2, 5),
      nextChunk: index + 1,
      completed: index === 2,
    }),
    artifactDownloadAbort: async () => assert.fail('valid download must not abort'),
  };
  const blob = await downloadRemoteArtifact(api, { localSessionId: 'remote-one', remoteRef: 'ref-one' }, {
    taskId: 'task-1', artifactId: 'artifact-1', mimeType: 'text/plain', displayName: 'hello.txt',
    sha256: '2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824',
  });
  assert.equal(await blob.text(), 'hello');
  assert.throws(() => validateRemoteFiles([]), /remote_attachment_count_invalid/);
});

test('uncertain transfer results remain reconcilable and do not trigger destructive aborts', async () => {
  let uploadAborts = 0;
  const uploadApi = {
    attachmentBegin: async () => ({ transferId: 'upload-unknown', chunkBytes: 5, totalBytes: 5 }),
    attachmentChunk: async () => {
      const error = new Error('result_unknown');
      error.code = 'result_unknown';
      throw error;
    },
    attachmentAbort: async () => { uploadAborts += 1; },
  };
  await assert.rejects(
    uploadRemoteFiles(uploadApi, { localSessionId: 'remote-one', remoteRef: 'ref-one' },
      'task-1', [file('hello.txt', 'hello')]),
    { code: 'result_unknown' },
  );
  assert.equal(uploadAborts, 0);

  let downloadAborts = 0;
  const downloadApi = {
    artifactDownloadBegin: async () => ({ transferId: 'download-unknown', chunkBytes: 2, totalBytes: 5 }),
    artifactDownloadChunk: async () => {
      const error = new Error('result_unknown');
      error.code = 'result_unknown';
      throw error;
    },
    artifactDownloadAbort: async () => { downloadAborts += 1; },
  };
  await assert.rejects(
    downloadRemoteArtifact(downloadApi, { localSessionId: 'remote-one', remoteRef: 'ref-one' }, {
      taskId: 'task-1', artifactId: 'artifact-1', mimeType: 'text/plain', displayName: 'hello.txt',
    }),
    { code: 'result_unknown' },
  );
  assert.equal(downloadAborts, 0);
});
