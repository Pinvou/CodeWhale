import assert from 'node:assert/strict';
import { spawnSync } from 'node:child_process';
import { spawn } from 'node:child_process';
import { createServer } from 'node:http';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { WebSocketServer } from 'ws';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { test } from 'node:test';

const scriptUrl = new URL('../scripts/remote-control-ui-smoke.mjs', import.meta.url);
const scriptPath = fileURLToPath(scriptUrl);
const chatViewPath = fileURLToPath(new URL('../src/features/chat/ChatView.jsx', import.meta.url));

test('real remote-control UI smoke refuses to run without a live WebView2 CDP target', () => {
  const result = spawnSync(process.execPath, [scriptPath], { encoding: 'utf8' });
  assert.notEqual(result.status, 0);
  assert.match(`${result.stdout}${result.stderr}`, /ui_smoke_arguments_missing/u);
});

test('real UI smoke drives CDP against a Tauri page and covers every release phase without a synthetic bridge', () => {
  const source = readFileSync(scriptPath, 'utf8');
  const chatViewSource = readFileSync(chatViewPath, 'utf8');
  assert.match(source, /webSocketDebuggerUrl/u);
  assert.match(source, /Input\.dispatchKeyEvent/u);
  assert.match(source, /Input\.dispatchMouseEvent/u);
  assert.match(source, /DOM\.setFileInputFiles/u);
  assert.match(source, /data-coordination-id/u);
  assert.match(source, /ui_smoke_coordination_ambiguous/u);
  assert.match(source, /coordination_digest/u);
  assert.match(source, /attachment_sha256/u);
  assert.match(source, /submit_attachment_via_cdp/u);
  assert.doesNotMatch(source, /\.click\(\)/u);
  assert.match(source, /approval-allow/u);
  assert.match(source, /approval-deny/u);
  assert.match(source, /click_permission_/u);
  assert.match(source, /open_artifact_preview/u);
  assert.match(source, /download_artifact_expected_failure/u);
  assert.match(source, /retry_artifact_download/u);
  assert.match(source, /tauri:\/\//u);
  assert.match(source, /remote-control-settings-page/u);
  for (const surface of [
    'controlled-device-panel', 'chat-remote-status', 'chat-remote-task-actions',
    'chat-remote-attachments', 'chat-remote-artifacts', 'chat-scroll',
  ]) assert.ok(source.includes(surface), `missing real UI surface: ${surface}`);
  assert.doesNotMatch(source, /PinvouPlatform\s*=|TauriBridge\s*=|mock/i);
  assert.match(chatViewSource, /data-coordination-id/u);
  assert.match(chatViewSource, /pendingPermissionCoordinationIds/u);
  assert.match(chatViewSource, /attachmentCoordinationIds/u);
});

async function runAgainstFakeCdp(t, { phase, pendingCount = 1, malformedRecovery = false, absentAttachment = false, disabledDownload = false }) {
  const commands = []; let selectorLengthReads = 0;
  const server = createServer((request, response) => {
    if (request.url === '/json/list') {
      response.setHeader('content-type', 'application/json');
      response.end(JSON.stringify([{ type: 'page', url: 'tauri://localhost', webSocketDebuggerUrl: `ws://127.0.0.1:${server.address().port}/cdp` }]));
    } else { response.statusCode = 404; response.end(); }
  });
  const sockets = new WebSocketServer({ noServer: true });
  server.on('upgrade', (request, socket, head) => sockets.handleUpgrade(request, socket, head, (client) => sockets.emit('connection', client)));
  sockets.on('connection', (client) => client.on('message', (bytes) => {
    const command = JSON.parse(String(bytes)); commands.push(command);
    let result = {};
    if (command.method === 'Runtime.evaluate') {
      const expression = command.params.expression;
      // Parse the actual page expression before producing fixture values. CDP
      // reports syntax failures as exceptionDetails, never a successful value.
      try {
        const checked = malformedRecovery && expression.includes('Boolean(document.querySelector') && expression.includes('chat-remote-action-error')
          ? expression.slice(0, -1) : expression;
        new Function(`return (${checked});`);
      } catch {
        client.send(JSON.stringify({ id: command.id, result: { exceptionDetails: { text: 'SyntaxError' } } }));
        return;
      }
      let value = true;
      if (expression.includes('exact: document.querySelectorAll')) value = { exact: 1, pending: pendingCount };
      else if (expression.includes('getBoundingClientRect')) value = { x: 20, y: 20 };
      else if (expression.includes('querySelectorAll') && expression.endsWith('.length')) { selectorLengthReads += 1; value = selectorLengthReads === 1 ? 1 : 0; }
      else if (expression.includes('Boolean(document.querySelector')) {
        value = (expression.includes('chat-remote-attachment-input') && !absentAttachment)
          || (expression.includes('chat-remote-artifact-download') && expression.includes(':not(:disabled)') && !disabledDownload);
      }
      // A DOM remote object has no result.value with returnByValue; don't let a
      // canned true conceal returning a node rather than a page-side Boolean.
      result = expression.startsWith('document.querySelector(') && expression.endsWith(')')
        ? { result: { type: 'object', subtype: 'node', description: 'HTMLInputElement' } }
        : { result: { value } };
    } else if (command.method === 'DOM.getDocument') result = { root: { nodeId: 1 } };
    else if (command.method === 'DOM.querySelector') result = { nodeId: 2 };
    client.send(JSON.stringify({ id: command.id, result }));
  }));
  await new Promise((resolvePromise) => server.listen(0, '127.0.0.1', resolvePromise));
  const root = await mkdtemp(join(tmpdir(), 'pinvou-ui-cdp-')); t.after(async () => { sockets.close(); server.close(); await rm(root, { recursive: true, force: true }); });
  const output = join(root, `${phase}.json`); const coordination = 'a'.repeat(64);
  const args = [scriptPath, '--cdp-url', `http://127.0.0.1:${server.address().port}`, '--phase', phase];
  if (new Set(['approval-allow', 'approval-deny', 'attachment']).has(phase)) args.push('--coordination-id', coordination);
  args.push('--output', output);
  const child = spawn(process.execPath, args, { stdio: ['ignore', 'pipe', 'pipe'] });
  let stdout = ''; let stderr = ''; child.stdout.on('data', (data) => { stdout += data; }); child.stderr.on('data', (data) => { stderr += data; });
  const status = await new Promise((resolvePromise) => child.on('close', resolvePromise));
  return { status, stdout, stderr, commands, output };
}

test('fake CDP integration proves exact coordination and protocol-level approval actions', async (t) => {
  const result = await runAgainstFakeCdp(t, { phase: 'approval-allow' });
  assert.equal(result.status, 0, result.stderr);
  const evidence = JSON.parse(await readFile(result.output, 'utf8'));
  assert.deepEqual(evidence.actions, ['click_permission_allow', 'assert_permission_allow_applied']);
  assert.ok(result.commands.some((item) => item.method === 'Input.dispatchMouseEvent' && item.params.type === 'mousePressed'));
  assert.ok(result.commands.some((item) => item.method === 'Runtime.evaluate' && item.params.expression.includes('a'.repeat(64))));
});

test('fake CDP integration drives attachment selection and submit through protocol commands', async (t) => {
  const result = await runAgainstFakeCdp(t, { phase: 'attachment' });
  assert.equal(result.status, 0, result.stderr);
  const evidence = JSON.parse(await readFile(result.output, 'utf8'));
  assert.deepEqual(evidence.actions, ['set_attachment_via_cdp', 'assert_attachment_selected', 'submit_attachment_via_cdp', 'assert_attachment_submitted']);
  assert.match(evidence.coordination_digest, /^[a-f0-9]{64}$/u);
  assert.match(evidence.attachment_sha256, /^[a-f0-9]{64}$/u);
  assert.ok(result.commands.some((item) => item.method === 'DOM.setFileInputFiles' && item.params.files.length === 1));
  assert.ok(result.commands.some((item) => item.method === 'Input.dispatchMouseEvent' && item.params.type === 'mousePressed'));
});

test('fake CDP integration drives artifact preview, failure, retry, and recovery', async (t) => {
  const result = await runAgainstFakeCdp(t, { phase: 'artifact' });
  assert.equal(result.status, 0, result.stderr);
  const evidence = JSON.parse(await readFile(result.output, 'utf8'));
  assert.deepEqual(evidence.actions, ['open_artifact_preview', 'assert_artifact_preview', 'close_artifact_preview', 'download_artifact_expected_failure', 'retry_artifact_download', 'assert_artifact_recovery_ready']);
  assert.ok(result.commands.filter((item) => item.method === 'Input.dispatchMouseEvent' && item.params.type === 'mousePressed').length >= 4);
});

test('fake CDP integration rejects multiple pending controls for one coordination id', async (t) => {
  const result = await runAgainstFakeCdp(t, { phase: 'approval-deny', pendingCount: 2 });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /ui_smoke_coordination_ambiguous/u);
});

test('fake CDP rejects malformed page expressions without creating pass evidence', async (t) => {
  const result = await runAgainstFakeCdp(t, { phase: 'artifact', malformedRecovery: true });
  assert.notEqual(result.status, 0);
  assert.match(result.stderr, /ui_smoke_page_evaluation_failed/u);
  await assert.rejects(readFile(result.output), { code: 'ENOENT' });
});

test('fake CDP rejects an absent attachment input and a disabled artifact download', async (t) => {
  for (const scenario of [
    { phase: 'attachment', absentAttachment: true, code: 'ui_smoke_attachment_input_missing' },
    { phase: 'artifact', disabledDownload: true, code: 'ui_smoke_artifact_download_stuck' },
  ]) {
    const result = await runAgainstFakeCdp(t, scenario);
    assert.notEqual(result.status, 0);
    assert.ok(result.stderr.includes(scenario.code), result.stderr);
    await assert.rejects(readFile(result.output), { code: 'ENOENT' });
  }
});
