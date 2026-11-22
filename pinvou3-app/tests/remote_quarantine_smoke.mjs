#!/usr/bin/env node
import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || [
  'C:/Program Files/Google/Chrome/Application/chrome.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/usr/bin/chromium', '/usr/bin/google-chrome',
].find(candidate => fs.existsSync(candidate));
if (!chrome) { console.error('SKIP: Chrome/Edge unavailable'); process.exit(2); }
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-remote-quarantine-smoke-'));
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
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/remote_quarantine_smoke.html`;
  const text = value => page.waitForFunction(expected => document.body.innerText.includes(expected),
    { timeout: 10000 }, value);
  const click = label => page.evaluate(value => {
    const button = [...document.querySelectorAll('button')].find(item => item.textContent.trim() === value);
    if (!button || button.disabled) throw new Error(`Unavailable button: ${value}`);
    button.click();
  }, label);
  const count = () => page.evaluate(() => window.quarantineFixture.calls.filter(([command]) => command === 'desktop_remote_proxy_quarantine').length);
  const open = async () => { await click('不可逆只读隔离'); await page.waitForSelector('[role="alertdialog"]'); };
  await page.goto(url, { waitUntil: 'domcontentloaded' });
  await text('待隔离旧会话');
  assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].find(b => b.textContent === '永久归档').disabled), true);
  await open(); assert.equal(await count(), 0);
  await click('取消'); assert.equal(await count(), 0);
  await open(); await page.keyboard.press('Escape');
  await page.waitForFunction(() => !document.querySelector('[role="alertdialog"]'));
  assert.equal(await count(), 0);
  await open();
  assert.equal(await page.evaluate(() => document.activeElement.getAttribute('role')), 'alertdialog');
  await click('确认永久隔离'); await text('已永久只读隔离并释放活动容量');
  assert.equal(await count(), 1);
  assert.deepEqual(await page.evaluate(() => window.quarantineFixture.calls.find(([command]) => command === 'desktop_remote_proxy_quarantine')[1]),
    { localSessionId: 'remote-11111111-1111-4111-8111-111111111111', confirmed: true });
  await click('冻结旧会话'); await text('确认从未尝试发送的请求: 1'); await text('结果未知的请求: 1');
  assert.equal(await page.evaluate(() => document.querySelector('[data-testid="remote-proxy-frozen-facts"]') !== null), true);
  assert.equal(await page.evaluate(() => document.querySelectorAll('textarea,input').length), 0);
  for (const [failure, expected] of [['remote_proxy_archive_online', '请先断开再归档'], ['remote_proxy_archive_unavailable', '归档存储不可用']]) {
    await page.reload({ waitUntil: 'domcontentloaded' }); await text('待隔离旧会话');
    await page.evaluate(value => { window.quarantineFixture.failure = value; }, failure);
    await open(); await click('确认永久隔离'); await text(expected);
    assert.equal(await page.evaluate(() => window.quarantineFixture.sealed), false);
    assert.equal(await page.evaluate(() => window.quarantineFixture.state.desktopRemoteControl.remoteProxyHistory.unattemptedPending.length), 1);
  }
  for (const [property, value, expected] of [['cleanup', false, '活动文件清理待后续写入重试'], ['refreshFails', true, '已永久只读隔离，但列表刷新失败']]) {
    await page.reload({ waitUntil: 'domcontentloaded' }); await text('待隔离旧会话');
    await page.evaluate(([key, next]) => { window.quarantineFixture[key] = next; }, [property, value]);
    await open(); await click('确认永久隔离'); await text(expected);
    assert.equal(await count(), 1);
    assert.equal(await page.evaluate(() => window.quarantineFixture.state.desktopRemoteControl.remoteProxyHistory.quarantined), true);
    assert.equal(await page.evaluate(() => window.quarantineFixture.state.desktopRemoteControl.remoteProxyHistory.unattemptedPending.length), 0);
  }
  assert.deepEqual(errors, []);
  console.log('remote_quarantine_smoke: confirmation/cancel/Escape/frozen facts/native refusal/partial success passed (real public facade + remote feature + mounted panel; unrelated startup inert, native IPC mock; not native end-to-end)');
} finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
  // Remove only the exact disposable browser profile created by this test.
  fs.rmSync(profile, { recursive: true, force: true });
}
