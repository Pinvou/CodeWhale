// Real Chromium, production download module, native Blob and WebCrypto.
// The remote API is controlled fault input; this is not a Relay/two-host test.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { readFile } from 'node:fs/promises';
import http from './helpers/http-fixture.mjs';
import puppeteer from './helpers/puppeteer-fixture.mjs';
import { fixtureScope } from './helpers/fixture-scope.mjs';

const chrome = process.env.CHROME || [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/google-chrome', '/usr/bin/chromium',
].find(file => fs.existsSync(file));
if (!chrome) throw new Error('real browser required for remote download budget regression');
const routes = new Map(await Promise.all([
  ['src/features/remote-control/remote-file-transfer.mjs', 'text/javascript'],
  ['src/shared/chunked-file-upload.js', 'text/javascript'],
].map(async ([name, type]) => [`/${name}`, { type,
  body: await readFile(new URL(`../${name}`, import.meta.url)) }])));
const server = http.createServer((request, response) => {
  if (request.url === '/') {
    response.writeHead(200, { 'content-type': 'text/html' });
    response.end('<!doctype html><meta charset="utf-8"><title>isolated download regression</title>');
    return;
  }
  const entry = routes.get(request.url);
  response.writeHead(entry ? 200 : 404, { 'content-type': entry?.type || 'text/plain' });
  response.end(entry?.body || 'not found');
});
try {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const browser = await puppeteer.launch({ executablePath: chrome, headless: true,
    args: ['--disable-gpu', '--no-first-run', '--no-sandbox', '--disable-breakpad', '--disable-crash-reporter'] });
  const page = await browser.newPage();
  const pageErrors = [];
  page.on('pageerror', error => pageErrors.push(error.message));
  await page.goto(`http://127.0.0.1:${server.address().port}/`);
  const results = await page.evaluate(async () => {
    const { downloadRemoteArtifact } = await import('/src/features/remote-control/remote-file-transfer.mjs');
    const binding = { localSessionId: 'session-one', remoteRef: 'remote-one' };
    const artifact = { taskId: 'task-one', artifactId: 'artifact-one' };
    const NativeBlob = window.Blob;
    let allocations = 0;
    window.Blob = class extends NativeBlob {
      constructor(...args) { super(...args); allocations++; }
    };
    const results = [];
    try {
      for (const mode of ['over-budget', 'empty', 'duplicate', 'gap', 'premature', 'checksum', 'exact-budget', 'recovery']) {
        allocations = 0;
        let calls = 0;
        let aborts = 0;
        let returned = false;
        let error = null;
        const total = mode === 'over-budget' ? 4097 : mode === 'exact-budget' ? 4096 : 1;
        const api = {
          artifactDownloadBegin: async () => ({ transferId: 'download-one', totalBytes: total, chunkBytes: 2 }),
          artifactDownloadChunk: async (...args) => {
            const index = args.at(-1);
            calls++;
            const receipt = { chunkIndex: index, chunkB64: btoa('a'), chunkBytes: 1,
              receivedBytes: index + 1, nextChunk: index + 1, completed: index + 1 === total };
            if (mode === 'empty') Object.assign(receipt, { chunkB64: '', chunkBytes: 0 });
            if (mode === 'duplicate') receipt.chunkIndex = -1;
            if (mode === 'gap') receipt.nextChunk = index + 2;
            if (mode === 'premature') receipt.completed = false;
            return receipt;
          },
          artifactDownloadAbort: async (...args) => {
            if (JSON.stringify(args) !== JSON.stringify(['session-one', 'remote-one', 'task-one', 'artifact-one', 'download-one'])) throw new Error('abort escaped exact binding');
            aborts++;
          },
        };
        try {
          const blob = await downloadRemoteArtifact(api, binding, {
            ...artifact, ...(mode === 'checksum' ? { sha256: '0'.repeat(64) } : {}),
          });
          if (!(blob instanceof NativeBlob) || blob.size !== total || (await blob.text()) !== 'a'.repeat(total)) throw new Error('native browser blob content mismatch');
          returned = true;
        } catch (failure) { error = failure.message; }
        results.push({ mode, calls, aborts, allocations, returned, error });
      }
      return results;
    } finally { window.Blob = NativeBlob; }
  });
  for (const row of results) {
    if (row.mode === 'exact-budget' || row.mode === 'recovery') {
      assert.equal(row.returned, true, JSON.stringify(row));
      assert.equal(row.error, null);
      assert.equal(row.aborts, 0);
      assert.equal(row.allocations, 1);
      assert.equal(row.calls, row.mode === 'exact-budget' ? 4096 : 1);
    } else {
      assert.equal(row.returned, false, JSON.stringify(row));
      assert.equal(row.aborts, 1);
      assert.equal(row.allocations, row.mode === 'checksum' ? 1 : 0);
      assert.equal(row.error, row.mode === 'over-budget' ? 'remote_artifact_chunk_limit'
        : row.mode === 'checksum' ? 'remote_artifact_checksum_mismatch' : 'remote_artifact_chunk_invalid');
      assert.equal(row.calls, row.mode === 'over-budget' ? 4096 : 1);
    }
  }
  assert.deepEqual(pageErrors, []);
  console.log(JSON.stringify({ browser: await browser.version(), cases: results }));
} finally {
  await fixtureScope.close();
}
