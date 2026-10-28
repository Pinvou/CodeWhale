import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/chromium', '/usr/bin/google-chrome',
].find(candidate => fs.existsSync(candidate));
if (!chrome) { console.error('SKIP: Chrome/Edge unavailable'); process.exit(2); }
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-remote-dispatch-'));
let browser, vite;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error',
    plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true,
    userDataDir: profile, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const baseUrl = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/remote_dispatch_ownership_smoke.html`;
  async function load(scenario = 'commit', language = 'zh') {
    errors.length = 0;
    await page.goto(`${baseUrl}?scenario=${scenario}&lang=${language}`, { waitUntil: 'domcontentloaded' });
    await page.waitForFunction(() => window.dispatchFixture?.select && document.querySelector('textarea'),
      { timeout: 15000 }).catch(error => { throw new Error(`${error.message}; page errors: ${JSON.stringify(errors)}`); });
    assert.deepEqual(errors, []);
  }
  async function retry(id) {
    await page.evaluate(taskId => {
      const fixture = window.dispatchFixture;
      const row = [...document.querySelectorAll('[data-testid="chat-remote-pending-dispatches"] > [role="status"]')]
        .find(item => item.textContent.includes(taskId));
      const button = [...(row?.querySelectorAll('button') || [])]
        .find(item => item.textContent === fixture.copy.remoteConversation.commitPreparedTask);
      if (!button || button.disabled) throw new Error(`Unavailable retry: ${taskId}`);
      button.click();
    }, id);
  }
  const attachmentBusy = () => page.$eval('[data-testid="chat-remote-attachment-input"]', input => input.nextElementSibling.disabled);
  async function preview() {
    await page.waitForFunction(() => {
      const button = document.querySelector('[data-testid="chat-remote-artifact-preview"]');
      return button && !button.disabled && document.querySelector('[data-testid="chat-remote-artifacts"]')
        ?.textContent.includes(`${window.dispatchFixture.current.id}.txt`);
    });
    await page.click('[data-testid="chat-remote-artifact-preview"]');
  }
  const flush = () => page.evaluate(() => new Promise(requestAnimationFrame));
  async function settleCommit(index, state) {
    await page.evaluate(({ index, state }) => {
      const request = window.dispatchFixture.commits[index];
      request.resolve({ state, retryHandle: request.handle });
    }, { index, state });
    await flush();
  }
  async function prepareUploadRetry() {
    await page.$eval('[data-testid="chat-remote-attachment-input"]', input => {
      const transfer = new DataTransfer();
      transfer.items.add(new File(['content'], 'input.txt', { type: 'text/plain' }));
      input.files = transfer.files;
      input.dispatchEvent(new Event('change', { bubbles: true }));
    });
    await page.type('textarea', 'Prepare a task with an attachment');
    await page.click('[data-testid="chat-composer-send"]');
    await page.waitForFunction(() => window.dispatchFixture.calls.some(call => call.kind === 'attachmentBegin')
      && document.querySelector('[data-testid="chat-remote-pending-dispatches"] button'));
  }

  for (const phase of ['commit', 'upload']) {
    await load(phase);
    if (phase === 'upload') {
      await prepareUploadRetry();
    }
    await retry('task-remote-a-1');
    await page.waitForFunction(phase => (phase === 'upload'
      ? window.dispatchFixture.uploads.length : window.dispatchFixture.commits.length) === 1, {}, phase);
    await page.evaluate(() => window.dispatchFixture.select('remote-b'));
    await page.waitForFunction(() => document.querySelector('[data-testid="chat-remote-pending-dispatches"]')
      ?.textContent.includes('task-remote-b'));
    assert.equal(await attachmentBusy(), false, `${phase}: retired UI owner does not block another session`);
    await retry('task-remote-b-1');
    const successor = phase === 'upload' ? 0 : 1;
    await page.waitForFunction(count => window.dispatchFixture.commits.length === count, {}, successor + 1);
    if (phase === 'upload') {
      await page.evaluate(() => window.dispatchFixture.uploads[0].resolve({ transferId: 'old-transfer', chunkBytes: 64 }));
      await page.waitForFunction(() => window.dispatchFixture.calls.some(call => call.kind === 'chunk'));
      await flush();
      assert.equal(await page.evaluate(() => window.dispatchFixture.calls
        .filter(call => call.kind === 'commit' && call.id === 'remote-a').length), 0,
      'retired prepared upload cannot proceed to commit');
    } else {
      await settleCommit(0, 'outcome_unknown');
    }
    assert.equal(await attachmentBusy(), true, `${phase}: old finally cannot clear successor busy`);
    assert.equal(await page.evaluate(() => window.dispatchFixture.cases('remote-a')[0]?.state),
      phase === 'upload' ? 'prepared_not_sent' : 'outcome_unknown', 'retiring UI preserves task evidence');
    await settleCommit(successor, 'queued');
    assert.equal(await attachmentBusy(), false, 'successor releases its own slot');
    assert.equal(await page.evaluate(() => window.dispatchFixture.calls.filter(call => call.kind === 'commit').length),
      phase === 'upload' ? 1 : 2, 'no automatic resend');
    assert.deepEqual(errors, []);
  }

  await load('upload');
  await prepareUploadRetry();
  await retry('task-remote-a-1');
  await page.waitForFunction(() => window.dispatchFixture.uploads.length === 1);
  await page.type('textarea', 'Preserve this draft through capability renewal');
  const draftBeforeRenewal = await page.$eval('textarea', input => input.value);
  await page.evaluate(() => window.dispatchFixture.select('remote-a', 1, 2));
  await page.waitForFunction(() => window.dispatchFixture.current.connection.capabilityRevision === 2);
  await flush();
  assert.equal(await attachmentBusy(), false, 'revision-only renewal retires the old upload UI');
  assert.equal(await page.$eval('textarea', input => input.value), draftBeforeRenewal);
  assert.equal(await page.$eval('[data-testid="chat-remote-attachment-remove"]', button => button.disabled), false);
  assert.equal(await page.$eval('[data-testid="chat-remote-attachments"]', element => element.textContent.includes('input.txt')), true,
    'stable composer retains the selected attachment');
  assert.equal(await page.evaluate(() => document.body.textContent.includes(
    window.dispatchFixture.copy.remoteConversation.uploadingAttachment('input.txt', 0, 7))), false,
  'retired progress is no longer displayed');
  await preview();
  await page.waitForFunction(() => window.dispatchFixture.previews.length === 1);
  await page.evaluate(() => window.dispatchFixture.uploads[0].resolve({ transferId: 'retired-transfer', chunkBytes: 64 }));
  await page.waitForFunction(() => window.dispatchFixture.calls.some(call => call.kind === 'chunk'));
  await flush();
  assert.equal(await attachmentBusy(), true, 'retired upload finally cannot clear successor preview');
  assert.equal(await page.evaluate(() => window.dispatchFixture.commits.length), 0, 'retired upload never commits');
  assert.equal(await page.evaluate(() => window.dispatchFixture.cases('remote-a')
    .find(item => item.taskId === 'task-remote-a-1')?.state), 'prepared_not_sent',
  'retiring an upload preserves the original prepared task');
  await page.evaluate(() => window.dispatchFixture.previews[0].resolve({ text: 'Renewed capability preview' }));
  await flush();
  assert.equal(await attachmentBusy(), false, 'successor releases its own slot');
  assert.deepEqual(errors, []);

  for (const identity of [['remote-a', 2], ['remote-a', 1, 1, 'generation-2'], ['remote-a', 1, 2]]) {
    await load();
    await retry('task-remote-a-1');
    await page.waitForFunction(() => window.dispatchFixture.commits.length === 1);
    await page.evaluate(args => window.dispatchFixture.select(...args), identity);
    await page.waitForFunction(({ lease, revision, generation }) => {
      const current = window.dispatchFixture.current.connection;
      return current.leaseEpoch === lease && current.capabilityRevision === revision && current.generation === generation;
    }, {}, { lease: identity[1], revision: identity[2] || 1, generation: identity[3] || 'generation-1' });
    assert.equal(await attachmentBusy(), false, 'lease/generation/revision retires old UI ownership');
    await preview();
    await page.waitForFunction(() => window.dispatchFixture.previews.length === 1);
    await settleCommit(0, 'outcome_unknown');
    assert.equal(await attachmentBusy(), true, 'old lease completion cannot release a new preview operation');
    await page.evaluate(() => window.dispatchFixture.previews[0].resolve({ text: 'Current preview' }));
    await flush();
    assert.equal(await attachmentBusy(), false);
    assert.deepEqual(errors, []);
  }

  await load();
  await page.waitForSelector('[data-testid="chat-remote-artifact-preview"]');
  await preview();
  await page.waitForFunction(() => window.dispatchFixture.previews.length === 1);
  await page.evaluate(() => window.dispatchFixture.select('remote-b'));
  await page.waitForFunction(() => window.dispatchFixture.current.id === 'remote-b');
  await preview();
  await page.waitForFunction(() => window.dispatchFixture.previews.length === 2);
  await page.evaluate(() => window.dispatchFixture.previews[0].resolve({ text: 'Retired session preview' }));
  await flush();
  assert.equal(await page.evaluate(() => document.body.textContent.includes('Retired session preview')), false);
  assert.equal(await attachmentBusy(), true);
  await page.evaluate(() => window.dispatchFixture.previews[1].resolve({ text: 'Selected session preview' }));
  await page.waitForFunction(() => document.body.textContent.includes('Selected session preview'));
  assert.equal(await attachmentBusy(), false);
  assert.deepEqual(errors, []);

  for (const language of ['zh', 'en', 'ja']) {
    await load('validation', language);
    for (const [kind, key] of [['count', 'attachmentCountInvalid'], ['size', 'attachmentSizeInvalid'],
      ['total', 'attachmentTotalSizeInvalid'], ['metadata', 'attachmentMetadataInvalid']]) {
      await page.$eval('[data-testid="chat-remote-attachment-input"]', (input, kind) => {
        const transfer = new DataTransfer();
        const files = kind === 'count' ? Array.from({ length: 9 }, (_, index) => new File(['a'], `${index}.txt`))
          : kind === 'size' ? [new File([], 'empty.txt')]
            : kind === 'total' ? Array.from({ length: 3 }, (_, index) => new File([new Uint8Array(25 * 1024 * 1024)], `${index}.txt`))
              : [new File(['a'], 'x'.repeat(201))];
        for (const file of files) transfer.items.add(file);
        input.files = transfer.files;
        input.dispatchEvent(new Event('change', { bubbles: true }));
      }, kind);
      await page.waitForFunction(key => document.querySelector('[data-testid="chat-remote-action-error"]')
        ?.textContent.trim() === window.dispatchFixture.copy.remoteConversation[key], {}, key);
      assert.equal(await page.evaluate(() => window.dispatchFixture.calls.length), 0, 'invalid local file is never uploaded');
    }
    await load('controlled', language);
    await page.waitForFunction(() => window.dispatchFixture.controlledSnapshots.length === 1);
    await page.type('textarea', 'Keep this unsent draft');
    assert.equal(await page.$eval('[data-testid="chat-composer-send"]', button => button.disabled), true);
    assert.equal(await page.$eval('[data-testid="chat-composer-send"]', button => button.getAttribute('aria-label')),
      await page.evaluate(() => window.dispatchFixture.copy.sendMsg), 'unready capability is not queue availability');
    assert.equal(await page.$eval('[data-testid="chat-remote-capability-not-ready"]', element => element.textContent.trim()),
      await page.evaluate(() => window.dispatchFixture.copy.remoteConversation.capabilityNotReady));
    await page.keyboard.press('Enter');
    assert.equal(await page.evaluate(() => window.dispatchFixture.calls.filter(call => call.kind === 'controlledSend').length), 0);
    assert.equal(await page.$eval('textarea', input => input.value), 'Keep this unsent draft');
    await page.evaluate(() => window.dispatchFixture.controlledSnapshots[0].resolve({
      sessionId: 'controlled-1', remoteControlGeneration: 'generation-b', sessionLane: 'chat',
      capabilityRevision: 1, expiresAtMs: Date.now() + 3600000, skills: [],
    }));
    await page.waitForFunction(() => !document.querySelector('[data-testid="chat-composer-send"]').disabled);
    await page.click('[data-testid="chat-composer-send"]');
    await page.waitForFunction(() => window.dispatchFixture.calls.filter(call => call.kind === 'controlledSend').length === 1);
    assert.deepEqual(errors, []);
  }
  console.log('remote_dispatch_ownership_smoke: mounted ChatView retry/identity/successor/preview and three-language validation/readiness passed (controlled host bridge)');
} finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
  // This exact disposable profile is owned by this test.
  fs.rmSync(profile, { recursive: true, force: true });
}
