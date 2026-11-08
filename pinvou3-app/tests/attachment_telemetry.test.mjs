import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import test from 'node:test';
import vm from 'node:vm';
import { parse } from 'acorn';
import { validateBehaviorEventShape } from '../../remote-control-relay/behavior-telemetry-schema.js';

const read = relative => readFileSync(new URL(relative, import.meta.url), 'utf8');
const classifier = read('../src/features/attachments/attachment-telemetry.js');
const uploader = read('../src/shared/chunked-file-upload.js');
const desktop = read('../src/platform/tauri/bridge/artifacts.js');
const web = read('../src/platform/web/bridge.js');
const webFunctions = new Map();
function collectFunctions(node) {
  if (!node || typeof node !== 'object') return;
  if (node.type === 'FunctionDeclaration' && node.id) webFunctions.set(node.id.name, web.slice(node.start, node.end));
  for (const value of Object.values(node)) {
    if (Array.isArray(value)) value.forEach(collectFunctions);
    else if (value && typeof value === 'object') collectFunctions(value);
  }
}
collectFunctions(parse(web, { ecmaVersion: 'latest' }));

function harness(host, { error, missingUploader = false, nativeWait, invalidResult = false } = {}) {
  const state = { activeSessionId: null, attachments: [] };
  const requests = [];
  const calls = [];
  const root = { __PINVOU_TAURI_BRIDGE_FEATURES__: {},
    btoa: value => Buffer.from(value, 'binary').toString('base64') };
  const sandbox = { window: root, Uint8Array, TextEncoder, state, attachIdSeq: 0, IS_WEB: true,
    notify() {}, bt: key => () => key, addSystemItem() {}, canInvoke: () => true,
    basename: value => String(value).split('/').at(-1) };
  const invoke = async (command, args) => {
    calls.push(command);
    if (command.endsWith('track_behavior_event')) { requests.push(args.request); return undefined; }
    if (command === 'artifact_info') return { size: 1 };
    if (['ingest_file', 'web_access_ingest_file', 'ingest_draft_file_chunk', 'web_access_upload_attachment_chunk'].includes(command)) {
      if (nativeWait) await nativeWait;
      if (error) throw error;
      if (invalidResult) return {};
      return host === 'desktop'
        ? { basename: 'PRIVATE_ATTACHMENT.log', kind: 'text', path: 'C:/PRIVATE_PATH/PRIVATE_ATTACHMENT.log',
          markdown: 'PRIVATE_CONTENT', token_estimate: 1, byte_size: 1, warning: null }
        : { basename: 'PRIVATE_ATTACHMENT.log', kind: 'text', handle: 'opaque_handle' };
    }
    return undefined; // cancellation/cleanup is observed without touching storage
  };
  sandbox.invoke = invoke;
  vm.createContext(sandbox);
  vm.runInContext(classifier, sandbox);
  if (!missingUploader) vm.runInContext(uploader, sandbox);
  let api;
  if (host === 'desktop') {
    vm.runInContext(desktop, sandbox);
    api = root.__PINVOU_TAURI_BRIDGE_FEATURES__.artifacts({
      ...sandbox, state, invoke, bt: key => key, sessionStates: {}, isDeliverable: () => false,
    });
  } else {
    const names = ['attachmentTelemetryToken', 'attachmentFileType', 'attachmentSizeBucket',
      'attachmentErrorCategory', 'attachmentPhase', 'beginAttachmentTelemetry', 'finishAttachmentTelemetry',
      'addAttachmentByPath', 'attachmentLimitDisplayError', 'deviceUploadDisplayError',
      'releaseAttachmentOnDesktop', 'removeAttachment', 'uploadDeviceFile', 'uploadDeviceFiles'];
    for (const name of names) assert.ok(webFunctions.has(name), `real Web function ${name} exists`);
    vm.runInContext(names.map(name => webFunctions.get(name)).join('\n'), sandbox);
    api = sandbox;
  }
  return { api, state, calls, requests, facts: root.PinvouAttachmentTelemetry, upload: root.PinvouChunkedFileUpload };
}

function file(size = 1, readError) {
  return { name: 'PRIVATE_ATTACHMENT.log', size, slice: (start, end) => ({
    arrayBuffer: async () => {
      if (readError) throw readError;
      return new Uint8Array(end - start).buffer;
    },
  }) };
}
function completed(h) {
  const terminals = h.requests.filter(request => request.eventName === 'attachment_processing_completed');
  assert.equal(terminals.length, 1, 'one operation has exactly one terminal');
  const terminal = terminals[0];
  const added = h.requests.find(request => request.eventName === 'attachment_added');
  assert.equal(terminal.operationId, added.operationId);
  assert.equal(terminal.attachmentId, added.attachmentId);
  assert.equal(terminal.stage, 'submission');
  assert.equal(terminal.inputType, 'attachment');
  // Exercise the real receiver's unchanged v1 schema, after the Rust adapter's
  // camelCase -> snake_case field projection. No raw error is part of that request.
  for (const request of h.requests) {
    const wire = { schema_version: 1, event_id: 'beh_attachment_test_001', event_source: 'client',
      occurred_at: Date.now(), collection_period_id: 'period_fixture', producer: 'pinvou_agent',
      producer_version: '0.10.17', app_version: '0.10.17', platform: 'windows', arch: 'x86_64',
      channel: 'dev', locale: 'zh-CN' };
    for (const [key, value] of Object.entries(request)) {
      if (value !== undefined) wire[key.replace(/[A-Z]/g, letter => '_' + letter.toLowerCase())] = value;
    }
    assert.deepEqual(validateBehaviorEventShape(wire), { ok: true });
  }
  assert.doesNotMatch(JSON.stringify(h.requests), /PRIVATE_ATTACHMENT|PRIVATE_TOKEN|PRIVATE_PATH|PRIVATE_CONTENT/);
  return terminal;
}

for (const host of ['desktop', 'web']) {
  test(`${host}: path exceptions retain bounded categories, instead of all becoming validation failures`, async () => {
    for (const [error, expected] of [
      [Object.assign(new Error('PRIVATE_PATH PRIVATE_TOKEN'), { code: 'permission_denied' }), 'permission_denied'],
      ['path PRIVATE_PATH is not readable: Access is denied. (os error 5)', 'permission_denied'],
      ['path PRIVATE_PATH crosses sensitive component PRIVATE_TOKEN', 'permission_denied'],
      ['attachment_file_too_large', 'validation_failed'],
      ['附件文件名无效', 'validation_failed'],
      ['path must be absolute: PRIVATE_PATH/timeout', 'validation_failed'],
      ["path must not contain '..' when it cannot be resolved: PRIVATE_PATH/permission denied", 'validation_failed'],
      [new Error('RPC timed out: PRIVATE_TOKEN'), 'timeout'],
      [Object.assign(new Error('PRIVATE_TOKEN'), { code: 'rpc_timeout' }), 'timeout'],
      [Object.assign(new Error('PRIVATE_TOKEN'), { code: 'desktop_capabilities_timeout' }), 'timeout'],
      [Object.assign(new Error('PRIVATE_TOKEN'), { code: 'desktop_capabilities_unavailable' }), 'network_unavailable'],
      [new Error('Failed to fetch'), 'network_unavailable'],
      [new Error('write failed: PRIVATE_PATH PRIVATE_TOKEN'), 'internal_error'],
    ]) {
      const h = harness(host, { error });
      await h.api.addAttachmentByPath('C:/PRIVATE_PATH/PRIVATE_ATTACHMENT.log');
      const terminal = completed(h);
      assert.equal(terminal.errorCategory, expected);
      assert.equal(terminal.step, 'attachment_ingest');
      assert.equal(h.state.attachments[0].status, 'error');
    }
  });

  test(`${host}: native reasons are separated from filenames and exact integrity rejections stay validation`, async () => {
    for (const name of ['neutral', 'timeout', 'permission denied', 'ECONNRESET',
      'is not readable: Permission denied (os error 13)', 'crosses sensitive component .ssh']) {
      for (const error of [`path C:/PRIVATE_PATH/${name}.log is not readable: No such file or directory (os error 2)`,
        `path C:/PRIVATE_PATH/${name} is not a file`,
        `path C:/PRIVATE_PATH/${name} is not readable: PRIVATE_TOKEN`]) {
        const h = harness(host, { error });
        await h.api.addAttachmentByPath('C:/PRIVATE_PATH/PRIVATE_ATTACHMENT.log');
        assert.equal(completed(h).errorCategory, 'internal_error', 'filename is not a failure reason');
      }
    }
    for (const [error, category, pathEntry] of [
      ['path PRIVATE_PATH not under $HOME', 'permission_denied', true],
      ['path PRIVATE_PATH under sensitive system dir SystemRoot', 'permission_denied', true],
      ['path PRIVATE_PATH/is not readable: PRIVATE_TOKEN not under $HOME', 'permission_denied', true],
      ['path PRIVATE_PATH is not readable: Operation not permitted (os error 1)', 'permission_denied', true],
      ['path PRIVATE_PATH is not readable: アクセスが拒否されました。 (os error 5)', 'permission_denied', true],
      ['打开附件暂存文件失败：Operation not permitted (os error 1)', 'permission_denied'],
      ['write uploaded attachment: アクセスが拒否されました。 (os error 5)', 'permission_denied'],
      ['附件完整性校验失败，上传内容在传输中损坏', 'validation_failed'],
      ['附件完整性校验值无效', 'validation_failed'],
      ['web_attachment_integrity_mismatch', 'validation_failed'],
      ['读取附件暂存文件失败：PRIVATE_TOKEN', 'internal_error'],
      ['附件完整性校验任务失败：PRIVATE_TOKEN', 'internal_error'],
      ['打开附件暂存文件失败：Input/output error (os error 5)', 'internal_error'],
      ['unexpected PRIVATE_PATH permission denied', 'internal_error'],
    ]) {
      // Web bootstrap wraps the same native String as Error.message/rpc_failed.
      const nativeError = host === 'web' ? Object.assign(new Error(error), { code: 'rpc_failed' }) : error;
      const h = harness(host, { error: nativeError });
      if (pathEntry) await h.api.addAttachmentByPath('C:/PRIVATE_PATH/PRIVATE_ATTACHMENT.log');
      else await h.api.uploadDeviceFiles([file()]);
      const terminal = completed(h);
      assert.equal(terminal.errorCategory, category);
      assert.equal(terminal.step, pathEntry ? 'attachment_ingest' : 'attachment_commit');
    }
  });

  test(`${host}: empty/oversized validation runs before native upload; missing loader is configuration`, async () => {
    for (const [size, missingUploader, category] of [[0, false, 'validation_failed'],
      [20 * 1024 * 1024 + 1, false, 'validation_failed'], [1, true, 'configuration_missing']]) {
      const h = harness(host, { missingUploader });
      await h.api.uploadDeviceFiles([file(size)]);
      const terminal = completed(h);
      assert.equal(terminal.errorCategory, category);
      assert.equal(terminal.step, 'attachment_validation');
      assert.equal(h.calls.some(command => command.includes('chunk')), false);
    }
  });

  test(`${host}: native upload IO wrappers retain permission facts throughout staging and commit`, async () => {
    const nativeSources = read('../src-tauri/src/features/files/attachment_upload.rs')
      + read('../src-tauri/src/app/commands/remote_control.rs');
    // Existing writers in append_chunk/remove_dir_if_present/hash verification
    // and Web staging: fixtures are their exact path-free IO error templates.
    const prefixes = ['清理附件目录失败：', '创建附件暂存目录失败：', '打开附件暂存文件失败：',
      '写入附件分块失败：', '刷新附件分块失败：', '读取附件暂存文件失败：',
      '创建会话附件目录失败：', '提交会话附件失败：',
      'create attachment upload dir: ', 'write uploaded attachment: '];
    for (const prefix of prefixes) {
      assert.ok(nativeSources.includes(prefix + '{error}'), 'fixture matches an actual native writer');
      for (const [reason, expected] of [['Access is denied. (os error 5)', 'permission_denied'],
        ['Operation not permitted (os error 1)', 'permission_denied'],
        ['Input/output error (os error 5)', 'internal_error'],
        ['not a regular file: PRIVATE_PATH/permission denied', 'internal_error']]) {
        const message = prefix + reason;
        const error = host === 'web' ? Object.assign(new Error(message), { code: 'rpc_failed' }) : message;
        for (const size of prefix === '清理附件目录失败：' ? [1, 256 * 1024 + 1] : [1]) {
          const h = harness(host, { error });
          await h.api.uploadDeviceFiles([file(size)]);
          const terminal = completed(h);
          assert.equal(terminal.errorCategory, expected);
          assert.equal(terminal.step, size > 256 * 1024 ? 'attachment_upload' : 'attachment_commit');
        }
      }
    }
  });

  test(`${host}: actual read, upload, commit and result failures retain the failing step`, async () => {
    const denied = Object.assign(new Error('PRIVATE_PATH'), { code: 'EACCES' });
    for (const [size, readError, error, expectedStep, category, invalidResult] of [
      [1, denied, null, 'attachment_read', 'permission_denied'],
      [256 * 1024 + 1, null, denied, 'attachment_upload', 'permission_denied'],
      [1, null, new Error('disk full PRIVATE_PATH'), 'attachment_commit', 'internal_error'],
      [1, null, null, 'attachment_result', 'internal_error', true],
    ]) {
      const h = harness(host, { error, invalidResult });
      await h.api.uploadDeviceFiles([file(size, readError)]);
      const terminal = completed(h);
      assert.equal(terminal.step, expectedStep);
      assert.equal(terminal.errorCategory, category);
    }
  });

  test(`${host}: successful other-bucket files and user cancellation preserve terminal semantics`, async () => {
    const success = harness(host);
    await success.api.uploadDeviceFiles([file()]);
    const terminal = completed(success);
    assert.equal(terminal.success, true);
    assert.equal(terminal.errorCategory, undefined);
    assert.equal(terminal.step, 'attachment_result');
    assert.equal(terminal.fileTypeBucket, 'other');
    const waiting = Promise.withResolvers();
    const cancelled = harness(host, { nativeWait: waiting.promise });
    const upload = cancelled.api.uploadDeviceFiles([file()]);
    for (let i = 0; i < 30 && !cancelled.calls.some(command => command.includes('chunk')); i += 1) await Promise.resolve();
    assert.ok(cancelled.calls.some(command => command.includes('chunk')));
    cancelled.api.removeAttachment(cancelled.state.attachments[0].id);
    waiting.resolve();
    await upload;
    assert.equal(completed(cancelled).errorCategory, 'user_cancelled');
    assert.equal(cancelled.state.attachments.length, 0);
  });
}

test('unregistered errors stay internal; only typed cancellation is user intent; observers cannot break upload', async () => {
  const h = harness('desktop');
  assert.equal(h.facts.errorCategory('cancelled by upstream PRIVATE_TOKEN'), 'internal_error');
  assert.equal(h.facts.errorCategory({ code: 'PRIVATE_TOKEN', message: 'PRIVATE_PATH' }), 'internal_error');
  assert.equal(h.facts.errorCategory({ code: 'constructor' }), 'internal_error');
  assert.equal(h.facts.errorCategory({ get code() { throw new Error('PRIVATE_TOKEN'); } }), 'internal_error');
  assert.equal(h.facts.errorCategory('permission denied' + ' '.repeat(8192)), 'internal_error');
  const phases = [];
  await h.upload.uploadFile({ file: file(), integrity: false,
    onPhase(phase) { phases.push(phase); throw new Error('observer unavailable'); },
    sendChunk: async () => ({ basename: 'result' }), validateResult: () => true });
  assert.deepEqual(phases, ['validation', 'read', 'read', 'commit', 'result']);
});

test('both startup lanes load the shared feature before their bridges and ship it as a runtime asset', () => {
  const index = read('../src/index.html');
  const helper = index.indexOf('features/attachments/attachment-telemetry.js');
  assert.ok(helper > 0);
  assert.ok(helper < index.indexOf('platform/tauri/bridge/artifacts.js'));
  assert.ok(helper < index.indexOf('platform/web/bridge.js'));
  assert.match(read('../vite.config.mjs'), /'features\/attachments\/attachment-telemetry\.js'/);
});
