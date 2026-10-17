#!/usr/bin/env node
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
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-remote-archive-smoke-'));
let browser;
let vite;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error',
    plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true,
    userDataDir: profile, args: ['--disable-gpu', '--no-first-run', '--no-default-browser-check'] });
  const page = await browser.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/remote_archive_smoke.html`;
  const text = value => page.waitForFunction(expected => document.body.innerText.includes(expected),
    { timeout: 10000 }, value);
  const click = label => page.evaluate(value => {
    const button = [...document.querySelectorAll('button')].find(item => item.textContent.trim() === value);
    if (!button || button.disabled) throw new Error(`Unavailable button: ${value}`);
    button.click();
  }, label);
  await page.goto(url, { waitUntil: 'domcontentloaded' });
  await text('Hidden offline');
  for (const [code, expected] of [['remote_proxy_archive_online', '请先断开再归档'],
    ['remote_proxy_archive_pending', '仍有未决任务']]) {
    await page.evaluate(value => { window.archiveFixture.failure = value; }, code);
    await click('永久归档'); await text(expected);
    assert.equal(await page.evaluate(() => window.archiveFixture.active.length), 1);
  }
  await page.evaluate(() => { window.archiveFixture.failure = null; });
  await click('永久归档'); await text('归档完成，已释放活动容量');
  await click('Hidden offline'); await text('Preserved history'); await text('result.txt');
  assert.equal(await page.evaluate(() => document.querySelectorAll('textarea,input').length), 0);

  // A committed irreversible archive must not be reported as failed just because
  // the subsequent list refresh fails. No second archive is issued automatically.
  await page.reload({ waitUntil: 'domcontentloaded' }); await text('Hidden offline');
  await page.evaluate(() => { window.archiveFixture.refreshFails = true; });
  await click('永久归档'); await text('已永久归档，但列表刷新失败');
  assert.equal(await page.evaluate(() => window.archiveFixture.calls.filter(call => call[0] === 'archive').length), 1);
  await page.evaluate(() => { window.archiveFixture.refreshFails = false; });
  await click('刷新');
  await page.waitForFunction(() => [...document.querySelectorAll('button')]
    .some(button => button.textContent === 'Hidden offline' && !button.disabled));
  await click('Hidden offline'); await text('Preserved history');
  await page.evaluate(() => {
    window.archiveFixture.archived = Array.from({ length: 257 }, (_, index) => ({
      localSessionId: `remote-proxy-page-${index}`, title: `Archive page ${index}`,
      archived: true, archiveCursor: String(9007199254740993n + BigInt(index)),
    }));
  });
  await click('刷新'); await text('Archive page 255');
  await click('下一页'); await text('Archive page 256');
  assert.equal(await page.evaluate(() => window.archiveFixture.calls.filter(call => call[0] === 'list').at(-1)[1]),
    '9007199254741248', 'pagination cursor must retain exact decimal bytes, not a JS Number');
  await click('上一页'); await text('Archive page 0');
  assert.deepEqual(errors, []);
  console.log('remote_archive_smoke: renderer assertions passed (mock bridge; not native IPC)');
} finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
  // Remove only the exact disposable browser profile created by this test.
  fs.rmSync(profile, { recursive: true, force: true });
}
