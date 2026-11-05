#!/usr/bin/env node
// Production service panel with controlled transport, not native execution evidence.
import assert from 'node:assert/strict';
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';

const app = fileURLToPath(new URL('..', import.meta.url));
process.chdir(app);
const chrome = process.env.CHROME || ['C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe', 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/usr/bin/chromium', '/snap/bin/chromium', '/usr/bin/google-chrome'].find(file => fs.existsSync(file));
if (!chrome) throw new Error('Chrome is required for bundled cmsd UI acceptance');
const output = process.env.CMS_UI_EVIDENCE || path.resolve(app, '../../.qa/cms-bundled-install-browser');
fs.mkdirSync(output, { recursive: true });
const report = { scope: 'Production React dialog with fixture transport', passed: false, checks: [], errors: [] };
const server = await createServer({ root: app, configFile: false, logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null, hmr: false, ws: false } });
let browser;
try {
  await server.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: true, args: ['--no-sandbox', '--disable-gpu'] });
  const page = await browser.newPage();
  page.on('pageerror', error => report.errors.push(error.message));
  const writes = () => page.evaluate(() => window.__calls.filter(c => c.command === 'cms_service_action'));
  async function open(scenario = '', lang = 'zh', width = 390) {
    await page.setViewport({ width, height: 800 });
    await page.goto(server.resolvedUrls.local[0] + `tests/fixtures/cms_bundled_install.html?scenario=${scenario}&lang=${lang}`, { waitUntil: 'networkidle0' });
    await page.waitForFunction(() => window.__calls.some(c => c.command === 'cms_service_status'));
  }
  const ready = () => page.waitForFunction(() => { const b = document.querySelector('[data-testid="cms-bundled-install"]'); return b && !b.disabled; });
  for (const lang of ['zh', 'en', 'ja']) {
    await open('', lang);
    await page.click('[data-testid="cms-service-prepare-install"]'); await ready();
    assert.equal(await page.$eval('[data-testid="cms-bundled-confirm"] [role="dialog"]', n => n.getAttribute('aria-modal')), 'true');
    assert.equal(await page.$eval('[data-testid="cms-service-advanced"]', n => n.open), false);
    assert.equal(await page.$('[data-testid="cms-bundled-confirm"] input'), null);
    assert.deepEqual(await writes(), []);
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
    await page.screenshot({ path: path.join(output, `${lang}-390.png`) });
    await page.focus('[data-testid="cms-bundled-install"]'); await page.keyboard.press('Tab');
    assert.equal(await page.evaluate(() => document.activeElement.dataset.testid), 'cms-bundled-cancel');
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => !document.querySelector('[data-testid="cms-bundled-confirm"]'));
    assert.equal(await page.evaluate(() => document.activeElement.dataset.testid), 'cms-service-prepare-install');
    assert.deepEqual(await writes(), []);
    report.checks.push(`${lang}: centered modal, no advanced steps or checkbox, keyboard trap, Escape cancellation and focus restoration; no writes`);
  }
  await open('slow'); await page.click('[data-testid="cms-service-prepare-install"]');
  await page.waitForFunction(() => typeof window.__releaseBundle === 'function');
  assert.equal(await page.$eval('[data-testid="cms-bundled-install"]', n => n.disabled), true);
  await page.evaluate(() => window.__releaseBundle()); await ready();
  await page.evaluate(() => { const b = document.querySelector('[data-testid="cms-bundled-install"]'); b.click(); b.click(); });
  await page.waitForFunction(() => typeof window.__finishAction === 'function');
  assert.equal((await writes()).length, 1);
  assert.equal((await writes())[0].args.expectedSha256, 'a'.repeat(64));
  await page.keyboard.press('Escape'); assert.ok(await page.$('[data-testid="cms-bundled-confirm"]'));
  await page.evaluate(() => window.__finishAction());
  await page.waitForFunction(() => !document.querySelector('[data-testid="cms-bundled-confirm"]'));
  assert.equal((await writes()).length, 1);
  report.checks.push('Verification must complete before confirmation; double click submits once with verified digest; accepted operation exits dialog');
  for (const scenario of ['missing', 'corrupt', 'wrong-source']) {
    await open(scenario); await page.click('[data-testid="cms-service-prepare-install"]');
    await page.waitForSelector('[data-testid="cms-bundled-confirm"] [role="alert"]');
    assert.equal(await page.$eval('[data-testid="cms-bundled-install"]', n => n.disabled), true);
    assert.deepEqual(await writes(), []);
    await page.click('[data-testid="cms-bundled-cancel"]');
    assert.ok(await page.$('[data-testid="cms-service-prepare-install"]'));
    report.checks.push(`${scenario}: visible verification failure, no installation, cancellation and recovery entry preserved`);
  }
  await open('rejected'); await page.click('[data-testid="cms-service-prepare-install"]'); await ready();
  await page.click('[data-testid="cms-bundled-install"]');
  await page.waitForSelector('[data-testid="cms-bundled-confirm"] [role="alert"]');
  assert.equal((await writes()).length, 1);
  assert.ok(await page.$('[data-testid="cms-bundled-confirm"]'));
  await page.click('[data-testid="cms-bundled-cancel"]');
  report.checks.push('Submission failure stays visible in the modal without automatic resubmission');
  for (const scenario of ['update', 'repair']) {
    await open(scenario); await page.click('[data-testid="cms-service-advanced"] > summary');
    await page.click('[data-testid="cms-service-bundled"]'); await ready();
    await page.click('[data-testid="cms-bundled-install"]');
    await page.waitForFunction(() => typeof window.__finishAction === 'function');
    assert.equal((await writes())[0].args.action, 'update');
    await page.evaluate(() => window.__finishAction());
    report.checks.push(`${scenario}: bundled confirmation uses update/repair rather than first installation`);
  }
  await open(); await page.click('[data-testid="cms-service-advanced"] > summary');
  await page.type('[data-testid="cms-service-package"]', '/fixture/operator-package');
  await page.click('[data-testid="cms-service-inspect"]'); await page.waitForSelector('input[type="checkbox"]');
  assert.equal(await page.$eval('[data-testid="cms-service-install"]', n => n.disabled), true);
  await page.click('input[type="checkbox"]'); await page.click('[data-testid="cms-service-install"]');
  assert.ok(await page.$('[role="alertdialog"]'));
  assert.deepEqual(await writes(), []);
  report.checks.push('Manually selected offline packages still require explicit source acknowledgement and impact confirmation');
  await open(); await page.click('[data-testid="cms-service-prepare-install"]'); await ready();
  await page.evaluate(() => { window.__failStatus = true; });
  await page.waitForFunction(() => document.querySelector('[data-testid="cms-bundled-confirm"] [role="alert"]'));
  assert.equal(await page.$eval('[data-testid="cms-bundled-install"]', n => n.disabled), true);
  await page.click('[data-testid="cms-bundled-cancel"]');
  assert.deepEqual(await writes(), []);
  report.checks.push('A lost live status query blocks confirmation while leaving cancellation available');
  assert.deepEqual(report.errors, []); report.passed = true;
} catch (error) { report.errors.push(error.stack); throw error; }
finally { fs.writeFileSync(path.join(output, 'report.json'), JSON.stringify(report, null, 2)); await browser?.close(); await server.close(); }
console.log(JSON.stringify({ passed: report.passed, checks: report.checks.length, output }));
