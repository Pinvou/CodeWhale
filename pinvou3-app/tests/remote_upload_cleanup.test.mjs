import assert from 'node:assert/strict';
import fs from 'node:fs';
import test from 'node:test';
import { abandonRemoteUpload, uploadRemoteFiles } from '../src/features/remote-control/remote-file-transfer.mjs';

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}
function file() {
  return { name: 'a.txt', type: 'text/plain', size: 1, arrayBuffer: async () => new Uint8Array([97]).buffer };
}
function setup(name) {
  const binding = { localSessionId: `local-${name}`, remoteRef: `ref-${name}` };
  const handle = { taskId: `task-${name}`, clientRequestId: `request-${name}` };
  const options = { ownerIdentity: `owner-${name}`, leaseIdentity: `lease-${name}`, isCurrent: () => true };
  const calls = [];
  const api = {
    attachmentBegin: async () => { calls.push('begin'); return { transferId: `transfer-${name}`, chunkBytes: 1 }; },
    attachmentChunk: async () => { calls.push('chunk'); return { state: 'completed', receivedBytes: 1 }; },
    attachmentAbort: async () => assert.fail('logical abort is not cleanup evidence'),
    attachmentAbortConfirmed: async (local, ref, task, id) => {
      calls.push(['cleanup', local, ref, task, id]);
      return { transfer_id: id, cleanup_confirmed: true };
    },
    abandonRemoteTask: async (local, request, task) => { calls.push(['abandon', local, request, task]); },
  };
  return { binding, handle, options, calls, api };
}
const upload = (s, task = s.handle.taskId, options = s.options) => uploadRemoteFiles(s.api, s.binding, task, [file()], () => {}, options);
const abandon = (s, current = () => true) => abandonRemoteUpload(s.api, s.options.ownerIdentity, s.binding, s.handle, current);
const fenced = s => assert.rejects(upload(s, 'manual-new-task'), { code: 'remote_attachment_fenced' });

async function interruptedBegin(s) {
  let current = true;
  s.api.attachmentBegin = async () => {
    s.calls.push('begin');
    current = false;
    return { transferId: 'interrupted-id', chunkBytes: 1 };
  };
  s.options.isCurrent = () => current;
  await assert.rejects(upload(s), { code: 'result_unknown' });
  s.options.isCurrent = () => true;
}

test('settled navigation batch needs physical cleanup and local abandon before a manual new batch', async () => {
  const s = setup('navigation');
  await interruptedBegin(s);
  await fenced(s);
  assert.equal(await abandon(s), true);
  assert.deepEqual(s.calls, ['begin',
    ['cleanup', s.binding.localSessionId, s.binding.remoteRef, s.handle.taskId, 'interrupted-id'],
    ['abandon', s.binding.localSessionId, s.handle.clientRequestId, s.handle.taskId]]);
  s.api.attachmentBegin = async () => ({ transferId: 'manual-id', chunkBytes: 1 });
  assert.deepEqual(await upload(s, 'manual-new-task'), ['manual-id']);
});

test('local abandon alone never clears an in-flight producer or its late successful callback', async () => {
  const s = setup('inflight');
  const began = deferred();
  const reply = deferred();
  let current = true;
  s.options.isCurrent = () => current;
  s.api.attachmentBegin = () => { began.resolve(); return reply.promise; };
  const running = upload(s);
  await began.promise;
  current = false;
  await abandon(s);
  assert.equal(s.calls.filter(call => Array.isArray(call) && call[0] === 'cleanup').length, 0);
  reply.resolve({ transferId: 'late-id', chunkBytes: 1 });
  await assert.rejects(running, { code: 'result_unknown' });
  s.options.isCurrent = () => true;
  await fenced(s);
});

test('lost begin or chunk ACK, including a hidden second handle, never authorizes cleanup', async () => {
  for (const phase of ['begin', 'chunk', 'second-begin']) {
    const s = setup(`lost-${phase}`);
    let begins = 0;
    s.api.attachmentBegin = async () => {
      begins += 1;
      if (phase === 'begin' || (phase === 'second-begin' && begins === 2)) throw Object.assign(new Error('result_unknown'), { code: 'result_unknown' });
      return { transferId: 'visible-id', chunkBytes: 1 };
    };
    if (phase === 'chunk') s.api.attachmentChunk = async () => { throw Object.assign(new Error('result_unknown'), { code: 'result_unknown' }); };
    await assert.rejects(uploadRemoteFiles(s.api, s.binding, s.handle.taskId,
      phase === 'second-begin' ? [file(), file()] : [file()], () => {}, s.options), { code: 'result_unknown' });
    await abandon(s);
    assert.equal(s.calls.some(call => Array.isArray(call) && call[0] === 'cleanup'), false);
    assert.equal(s.calls.filter(call => Array.isArray(call) && call[0] === 'abandon').length, 1);
    await fenced(s);
  }
});

test('legacy logical replies, missing or mismatched proof, cleanup failure and lost cleanup ACK retain fences', async () => {
  for (const [name, receipt] of [
    ['logical', { aborted: true }], ['absent', { already_absent: true }],
    ['false', { transfer_id: 'interrupted-id', cleanup_confirmed: false }],
    ['wrong-id', { transfer_id: 'another-id', cleanup_confirmed: true }],
    ['failure', null], ['lost-ack', 'result_unknown'],
  ]) {
    const s = setup(`proof-${name}`);
    await interruptedBegin(s);
    s.api.attachmentAbortConfirmed = async () => {
      if (receipt === null) throw new Error('transfer_staging_cleanup_failed');
      if (receipt === 'result_unknown') throw Object.assign(new Error(receipt), { code: receipt });
      return receipt;
    };
    assert.equal(await abandon(s), true);
    assert.equal(s.calls.filter(call => Array.isArray(call) && call[0] === 'abandon').length, 1);
    await fenced(s);
  }
});

test('confirmed cleanup followed by local abandon failure retains proof for explicit retry', async () => {
  const s = setup('local-failure');
  await interruptedBegin(s);
  let abandons = 0;
  s.api.abandonRemoteTask = async () => { if (++abandons === 1) throw new Error('store_failed'); };
  await assert.rejects(abandon(s), /store_failed/);
  await fenced(s);
  assert.equal(await abandon(s), true);
  assert.equal(s.calls.filter(call => Array.isArray(call) && call[0] === 'cleanup').length, 1);
  assert.equal(abandons, 2);
  s.api.attachmentBegin = async () => ({ transferId: 'manual-id', chunkBytes: 1 });
  assert.deepEqual(await upload(s, 'new-task'), ['manual-id']);
});

test('old cleanup callback cannot retire the same key in a replacement lease', async () => {
  const s = setup('replace');
  await interruptedBegin(s);
  const entered = deferred();
  const reply = deferred();
  s.api.attachmentAbortConfirmed = () => { entered.resolve(); return reply.promise; };
  let current = true;
  const old = abandon(s, () => current);
  await entered.promise;
  current = false;
  s.options = { ...s.options, leaseIdentity: 'replacement-lease' };
  s.api.attachmentBegin = async () => ({ transferId: 'replacement-id', chunkBytes: 1 });
  await upload(s);
  reply.resolve({ transfer_id: 'interrupted-id', cleanup_confirmed: true });
  assert.equal(await old, false);
  assert.equal(s.calls.some(call => Array.isArray(call) && call[0] === 'abandon'), false);
  await assert.rejects(upload(s, 'another-task'), { code: 'remote_attachment_completed' });
});

test('completed batch is distinguished from unknown and a different owner cannot retire it', async () => {
  const s = setup('completed');
  await upload(s);
  await assert.rejects(upload(s, 'another-task'), { code: 'remote_attachment_completed' });
  await abandonRemoteUpload(s.api, 'other-owner', s.binding, s.handle);
  await assert.rejects(upload(s, 'another-task'), { code: 'remote_attachment_completed' });
  assert.equal(s.calls.some(call => Array.isArray(call) && call[0] === 'cleanup'), false);
  await abandon(s);
  assert.deepEqual(await upload(s, 'another-task'), ['transfer-completed']);
});

test('ChatView uses acknowledged abandon path and distinct completed copy in all three languages', () => {
  const chat = fs.readFileSync(new URL('../src/features/chat/ChatView.jsx', import.meta.url), 'utf8');
  assert.match(chat, /if \(!await abandonRemoteUpload\(bridge\.desktopRemoteControl, remoteComposerIdentity, remoteBinding, item\.handle,/);
  assert.match(chat, /case 'remote_attachment_completed': return 'attachmentCompleted'/);
  for (const language of ['zh', 'en', 'ja']) {
    const source = fs.readFileSync(new URL(`../src/shared/i18n/${language}.js`, import.meta.url), 'utf8');
    assert.match(source, /attachmentCompleted: ".+"/);
    assert.match(source, /attachmentFenced: ".+"/);
  }
});
