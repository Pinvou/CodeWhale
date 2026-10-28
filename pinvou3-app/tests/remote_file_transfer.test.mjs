import assert from 'node:assert/strict';
import test from 'node:test';
import {
  downloadRemoteArtifact,
  createRemoteTransferGuard,
  uploadRemoteFiles,
  retireCompletedRemoteUpload,
  abortCompletedRemoteUpload,
  validateRemoteFiles,
} from '../src/features/remote-control/remote-file-transfer.mjs';

function file(name, content, type = 'text/plain') {
  const bytes = new TextEncoder().encode(content);
  return { name, type, size: bytes.length, arrayBuffer: async () => bytes.buffer.slice(0) };
}

// Validation consumes File metadata before reading or uploading any bytes.
// These fixtures exercise that public validator boundary, not file transfer.
const metadataFile = size => ({ name: 'boundary.bin', type: 'application/octet-stream', size });

test('remote attachment validation accepts exactly 20 MiB in one file', () => {
  const selected = [metadataFile(20 * 1024 * 1024)];
  assert.deepEqual(validateRemoteFiles(selected), selected);
});

test('a later definite failure revokes only known handles from its batch before retry', async () => {
  const binding = { localSessionId: 'local-definite', remoteRef: 'remote-definite' };
  const owner = { ownerIdentity: 'lease-definite', isCurrent: () => true };
  const files = [file('a.txt', 'a'), file('b.txt', 'b')];
  const aborted = [];
  let starts = 0;
  const api = {
    attachmentBegin: async () => { starts += 1; if (starts === 2) throw new Error('begin_rejected'); return { transferId: `t-${starts}`, chunkBytes: 1 }; },
    attachmentChunk: async () => ({ state: 'completed', receivedBytes: 1 }),
    attachmentAbort: async (_local, _remote, task, id) => { aborted.push([task, id]); return { aborted: true }; },
  };
  await assert.rejects(uploadRemoteFiles(api, binding, 'task-definite', files, () => {}, owner), /begin_rejected/);
  assert.deepEqual(aborted, [['task-definite', 't-1']]);
  assert.deepEqual(await uploadRemoteFiles(api, binding, 'task-definite', files, () => {}, owner), ['t-3', 't-4']);
  retireCompletedRemoteUpload(owner.ownerIdentity, binding, 'task-definite');
});

test('unknown outcomes and failed cleanup fence the attempt before any retry sends', async () => {
  for (const mode of ['unknown', 'cleanup-fails']) {
    const binding = { localSessionId: `local-${mode}`, remoteRef: 'remote' };
    const owner = { ownerIdentity: `lease-${mode}`, leaseIdentity: `lease-${mode}`, isCurrent: () => true };
    const files = [file('a.txt', 'a'), file('b.txt', 'b')];
    let starts = 0;
    let aborts = 0;
    const api = {
      attachmentBegin: async () => {
        starts += 1;
        if (starts === 2) { const error = new Error(mode); if (mode === 'unknown') error.code = 'result_unknown'; throw error; }
        return { transferId: 'owned-1', chunkBytes: 1 };
      },
      attachmentChunk: async () => ({ state: 'completed', receivedBytes: 1 }),
      attachmentAbort: async () => { aborts += 1; throw new Error('cleanup failed'); },
    };
    await assert.rejects(uploadRemoteFiles(api, binding, `task-${mode}`, files, () => {}, owner), { code: 'result_unknown' });
    assert.equal(aborts, mode === 'unknown' ? 0 : 1);
    await assert.rejects(uploadRemoteFiles(api, binding, `task-${mode}`, files, () => {}, owner), { code: 'result_unknown' });
    assert.equal(starts, 2, 'retry must not create another transfer');
  }
});

test('an uncommitted completed batch is reused only with the same file objects and owner', async () => {
  const binding = { localSessionId: 'local-cached', remoteRef: 'remote-cached' };
  const owner = { ownerIdentity: 'lease-cached', isCurrent: () => true };
  const files = [file('a.txt', 'a')];
  let starts = 0;
  const api = {
    attachmentBegin: async () => { starts += 1; return { transferId: 'owned-1', chunkBytes: 1 }; },
    attachmentChunk: async () => ({ state: 'completed', receivedBytes: 1 }),
  };
  assert.deepEqual(await uploadRemoteFiles(api, binding, 'task-cached', files, () => {}, owner), ['owned-1']);
  assert.deepEqual(await uploadRemoteFiles(api, binding, 'task-cached', files, () => {}, owner), ['owned-1']);
  await assert.rejects(uploadRemoteFiles(api, binding, 'task-cached', [file('a.txt', 'a')], () => {}, owner), { code: 'result_unknown' });
  assert.equal(starts, 1);
  retireCompletedRemoteUpload(owner.ownerIdentity, binding, 'task-cached');
});

test('abandoning a completed batch revokes its exact unconsumed handles', async () => {
  const binding = { localSessionId: 'local-abandon', remoteRef: 'remote' };
  const owner = { ownerIdentity: 'owner-abandon', leaseIdentity: 'lease-abandon', isCurrent: () => true };
  const files = [file('a.txt', 'a')];
  const aborts = [];
  const api = {
    attachmentBegin: async () => ({ transferId: 'abandon-1', chunkBytes: 1 }),
    attachmentChunk: async () => ({ state: 'completed', receivedBytes: 1 }),
    attachmentAbort: async (_local, _ref, task, id) => { aborts.push([task, id]); return { aborted: true }; },
  };
  await uploadRemoteFiles(api, binding, 'task-abandon', files, () => {}, owner);
  assert.equal(await abortCompletedRemoteUpload(api, owner.ownerIdentity, binding, 'task-abandon'), true);
  assert.deepEqual(aborts, [['task-abandon', 'abandon-1']]);
  assert.equal(await abortCompletedRemoteUpload(api, owner.ownerIdentity, binding, 'task-abandon'), false);
});

test('abandoning an unknown upload cannot bypass the binding fence with a new task or File object', async () => {
  const binding = { localSessionId: 'local-fence', remoteRef: 'remote-fence' };
  const opts = { ownerIdentity: 'owner-old', leaseIdentity: 'lease-fence', isCurrent: () => true };
  let starts = 0;
  const api = {
    attachmentBegin: async () => { starts += 1; const error = new Error('unknown'); error.code = 'result_unknown'; throw error; },
    attachmentAbort: async () => assert.fail('an unknown upload cannot be aborted safely'),
  };
  await assert.rejects(uploadRemoteFiles(api, binding, 'old-task', [file('a.txt', 'a')], () => {}, opts), { code: 'result_unknown' });
  assert.equal(await abortCompletedRemoteUpload(api, opts.ownerIdentity, binding, 'old-task'), false);
  await assert.rejects(uploadRemoteFiles(api, binding, 'new-task', [file('a.txt', 'a')], () => {}, { ...opts, ownerIdentity: 'new-owner' }), { code: 'result_unknown' });
  assert.equal(starts, 1);
  const fresh = { attachmentBegin: async () => ({ transferId: 'new-id', chunkBytes: 1 }), attachmentChunk: async () => ({ state: 'completed', receivedBytes: 1 }) };
  assert.deepEqual(await uploadRemoteFiles(fresh, binding, 'new-task', [file('a.txt', 'a')], () => {}, { ...opts, leaseIdentity: 'lease-next' }), ['new-id']);
  retireCompletedRemoteUpload(opts.ownerIdentity, binding, 'new-task');
});

test('authenticated logical absence during cleanup permits a new batch', async () => {
  const binding = { localSessionId: 'local-absent', remoteRef: 'remote-absent' };
  const opts = { ownerIdentity: 'owner-absent', leaseIdentity: 'lease-absent', isCurrent: () => true };
  let begins = 0;
  let aborts = 0;
  const api = {
    attachmentBegin: async () => ({ transferId: 'transfer-' + ++begins, chunkBytes: 1 }),
    attachmentChunk: async () => { if (begins === 1) throw new Error('staging_failed'); return { state: 'completed', receivedBytes: 1 }; },
    attachmentAbort: async () => { aborts += 1; return { aborted: false, already_absent: true }; },
  };
  await assert.rejects(uploadRemoteFiles(api, binding, 'task-1', [file('a.txt', 'a')], () => {}, opts), /staging_failed/);
  assert.equal(aborts, 1);
  assert.deepEqual(await uploadRemoteFiles(api, binding, 'task-2', [file('a.txt', 'a')], () => {}, opts), ['transfer-2']);
  retireCompletedRemoteUpload(opts.ownerIdentity, binding, 'task-2');
});

test('unknown batches cannot exhaust capacity after a verified lease change', async () => {
  const binding = { localSessionId: 'local-capacity', remoteRef: 'remote' };
  const files = [file('a.txt', 'a')];
  let starts = 0;
  const unknown = { attachmentBegin: async () => { starts += 1; const error = new Error('unknown'); error.code = 'result_unknown'; throw error; } };
  for (let i = 0; i < 130; i += 1) {
    await assert.rejects(uploadRemoteFiles(unknown, { ...binding, localSessionId: `local-capacity-${i}` }, `task-${i}`, files, () => {},
      { ownerIdentity: 'owner-old', leaseIdentity: 'lease-old', isCurrent: () => true }), { code: 'result_unknown' });
  }
  assert.equal(starts, 128, 'the old generation reaches and enforces its bound');
  const fresh = {
    attachmentBegin: async () => ({ transferId: 'fresh-1', chunkBytes: 1 }),
    attachmentChunk: async () => ({ state: 'completed', receivedBytes: 1 }),
  };
  assert.deepEqual(await uploadRemoteFiles(fresh, binding, 'fresh-task', files, () => {},
    { ownerIdentity: 'owner-new', leaseIdentity: 'lease-new', isCurrent: () => true }), ['fresh-1']);
  retireCompletedRemoteUpload('owner-new', binding, 'fresh-task');
});

test('remote attachment validation rejects 20 MiB plus one byte in one file', () => {
  assert.throws(() => validateRemoteFiles([metadataFile(20 * 1024 * 1024 + 1)]),
    /remote_attachment_size_invalid/);
});

test('oversized file in a batch is rejected before any file is read or uploaded', async () => {
  let reads = 0;
  let begins = 0;
  const selected = [
    { ...metadataFile(1), arrayBuffer: async () => { reads += 1; return new ArrayBuffer(1); } },
    { ...metadataFile(20 * 1024 * 1024 + 1), arrayBuffer: async () => { reads += 1; return new ArrayBuffer(1); } },
  ];
  await assert.rejects(uploadRemoteFiles({ attachmentBegin: async () => { begins += 1; } },
    { localSessionId: 'local', remoteRef: 'remote' }, 'task', selected), /remote_attachment_size_invalid/);
  assert.equal(reads, 0);
  assert.equal(begins, 0);
});

test('remote attachment validation accepts exactly 64 MiB across valid files', () => {
  const selected = [metadataFile(20 * 1024 * 1024), metadataFile(20 * 1024 * 1024), metadataFile(20 * 1024 * 1024), metadataFile(4 * 1024 * 1024)];
  assert.deepEqual(validateRemoteFiles(selected), selected);
});

test('remote attachment validation rejects 64 MiB plus one byte across valid files', () => {
  const selected = [metadataFile(20 * 1024 * 1024), metadataFile(20 * 1024 * 1024), metadataFile(20 * 1024 * 1024), metadataFile(4 * 1024 * 1024 + 1)];
  assert.throws(() => validateRemoteFiles(selected), /remote_attachment_total_size_invalid/);
});

test('failed uploads clear their own loading state without clearing a newer attempt', async () => {
  const guard = createRemoteTransferGuard();
  const first = guard.start('lease/revision-1');
  assert.equal(guard.isCurrent(first, 'lease/revision-1'), true);
  assert.equal(guard.isCurrent(first, 'lease/revision-2'), false);
  // Finally still clears this upload after a snapshot renews.
  assert.equal(guard.finish(first), true);
  assert.equal(guard.isCurrent(first, 'lease/revision-1'), false);
  const old = guard.start('lease/revision-2');
  const later = guard.start('lease/revision-2');
  assert.equal(guard.isCurrent(old, 'lease/revision-2'), false);
  assert.equal(guard.finish(old), false);
  assert.equal(guard.isCurrent(later, 'lease/revision-2'), true);
  guard.invalidate();
  assert.equal(guard.isCurrent(later, 'lease/revision-2'), false);
  assert.equal(guard.finish(later), false);
});

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

test('downloads enforce their independent 32 MiB bound and abort on violation', async () => {
  let aborts = 0;
  const api = {
    artifactDownloadBegin: async () => ({
      transferId: 'download-oversize', chunkBytes: 4, totalBytes: 33 * 1024 * 1024,
    }),
    artifactDownloadChunk: async () => assert.fail('an over-limit begin must not request chunks'),
    artifactDownloadAbort: async () => { aborts += 1; },
  };
  await assert.rejects(
    downloadRemoteArtifact(api, { localSessionId: 'remote-one', remoteRef: 'ref-one' }, {
      taskId: 'task-1', artifactId: 'artifact-1', mimeType: 'text/plain', displayName: 'big.bin',
    }),
    /remote_artifact_size_limit/,
  );
  assert.equal(aborts, 1);
});

test('a download whose chunks exceed the declared total stops before unbounded growth', async () => {
  const chunk = 'AAAA'; // 3 decoded bytes per chunk; the stream keeps going past the declared total.
  let calls = 0;
  let aborts = 0;
  const api = {
    artifactDownloadBegin: async () => ({
      transferId: 'download-lying-stream', chunkBytes: 3, totalBytes: 3,
    }),
    artifactDownloadChunk: async () => {
      calls += 1;
      return { chunkB64: chunk, chunkBytes: 3, completed: false, nextChunk: calls };
    },
    artifactDownloadAbort: async () => { aborts += 1; },
  };
  await assert.rejects(
    downloadRemoteArtifact(api, { localSessionId: 'remote-one', remoteRef: 'ref-one' }, {
      taskId: 'task-1', artifactId: 'artifact-1', mimeType: 'text/plain', displayName: 'lie.bin',
    }),
    /remote_artifact_size_limit/,
  );
  assert.equal(calls, 2, 'the second chunk crosses the declared total and must stop');
  assert.equal(aborts, 1);
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
