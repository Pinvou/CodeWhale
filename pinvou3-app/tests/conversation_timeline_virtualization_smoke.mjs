import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';

const appRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || [
  '/snap/bin/chromium',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
  '/usr/bin/google-chrome',
  '/usr/bin/google-chrome-stable',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
].find(candidate => fs.existsSync(candidate));

assert.ok(chrome, 'Chrome/Chromium is required; set CHROME to its executable');

const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-timeline-virtualization-'));
let browser;
let server;

try {
  server = await createServer({
    root: appRoot,
    configFile: false,
    appType: 'mpa',
    logLevel: 'error',
    plugins: [react()],
    server: { host: '127.0.0.1', port: 0, strictPort: false, watch: null },
  });
  await server.listen();
  const address = server.httpServer.address();
  browser = await puppeteer.launch({
    executablePath: chrome,
    headless: 'new',
    userDataDir: profile,
    args: ['--no-sandbox', '--disable-gpu', '--no-first-run', '--no-default-browser-check'],
  });
  const page = await browser.newPage();
  await page.goto(
    `http://127.0.0.1:${address.port}/tests/fixtures/conversation_timeline_performance.html`,
    { waitUntil: 'networkidle0', timeout: 30_000 },
  );
  await page.waitForFunction(() => Boolean(window.__PINVOU_TIMELINE_PERFORMANCE__), { timeout: 10_000 });

  const shortTimeline = await page.evaluate(() => window.__PINVOU_TIMELINE_PERFORMANCE__.run(50));
  assert.equal(shortTimeline.virtualRows, 0, 'short conversations must keep normal document flow');
  assert.ok(shortTimeline.topIndexes.includes(0) && shortTimeline.bottomIndexes.includes(49));

  const longTimeline = await page.evaluate(() => window.__PINVOU_TIMELINE_PERFORMANCE__.run(1000));
  assert.ok(longTimeline.nodes <= 2_500, `resident DOM exceeded budget: ${longTimeline.nodes}`);
  assert.ok(longTimeline.maxResidentNodes <= 2_500, `scrolling DOM exceeded budget: ${longTimeline.maxResidentNodes}`);
  assert.ok(longTimeline.mountMs <= 100, `initial mount exceeded regression ceiling: ${longTimeline.mountMs.toFixed(1)}ms`);
  assert.ok(longTimeline.tailUpdateMs <= 10, `tail update exceeded regression ceiling: ${longTimeline.tailUpdateMs.toFixed(1)}ms`);
  assert.equal(longTimeline.tailUpdated, true, 'the mounted final turn must receive updates');
  assert.ok(longTimeline.topIndexes.includes(0), 'scrolling to the top must mount the first turn');
  assert.ok(longTimeline.middleIndexes.some(index => index > 200 && index < 800), 'middle scrolling must mount middle turns');
  assert.ok(longTimeline.bottomIndexes.includes(999), 'scrolling to the bottom must mount the final turn');

  console.log('conversation timeline virtualization smoke passed', JSON.stringify(longTimeline));
} finally {
  await browser?.close().catch(() => {});
  await server?.close().catch(() => {});
  fs.rmSync(profile, { recursive: true, force: true });
}
