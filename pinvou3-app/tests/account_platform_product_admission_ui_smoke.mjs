import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || ['C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome'].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; product admission UI was not validated');
const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-admission-ui-'));
const original = path.join(directory, 'controlled.png');
fs.writeFileSync(original, Buffer.from('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+aNbQAAAAASUVORK5CYII=', 'base64'));
let vite, browser; let passed = 0;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } }); await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: path.join(directory, 'profile'), args: ['--disable-gpu', '--no-first-run'] });
  for (const language of ['zh', 'en', 'ja']) {
    const page = await browser.newPage(); page.setDefaultTimeout(10000); const errors = []; page.on('pageerror', error => errors.push(error.message));
    await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-product-admission.html`);
    await page.waitForSelector('input[type=file]'); await page.select('[aria-label=fixture-language]', language);
    const click = async name => {
      await page.waitForFunction(name => [...document.querySelectorAll('button')].some(button => button.textContent === (window.admissionFixture.labels[name] || window.admissionFixture.copy[name]) && !button.disabled), {}, name);
      await page.evaluate(name => [...document.querySelectorAll('button')].find(button => button.textContent === (window.admissionFixture.labels[name] || window.admissionFixture.copy[name]) && !button.disabled).click(), name);
    };
    await (await page.$('input[type=file]')).uploadFile(original);
    await page.$eval('input[type=datetime-local]', input => { const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set; setter.call(input, '2099-10-31T10:00'); input.dispatchEvent(new Event('input', { bubbles: true })); });
    await click('upload'); await page.waitForFunction(() => document.querySelector('dd')?.textContent.length === 64);
    assert.equal(await page.evaluate(() => window.admissionFixture.calls.filter(value => value.url.endsWith('/decisions')).length), 0, 'upload must not cast a vote');
    await page.type('textarea', 'Controlled frozen material reason'); await click('submit');
    await page.waitForSelector('pre'); assert.ok(await page.$eval('pre', element => element.textContent.includes('controlled-sku')));
    await page.type('textarea', 'Checked same original and frozen facts'); await click('approve');
    await page.waitForFunction(() => document.querySelector('[role=alert]')?.textContent === window.admissionFixture.copy.unknown);
    await click('retry');
    await page.waitForFunction(() => document.querySelector('article li')?.textContent.includes('44444444-4444-4444-8444-444444444444'));
    const retries = await page.evaluate(() => window.admissionFixture.calls.filter(value => value.url.endsWith('/decisions')));
    assert.equal(retries.length, 2); assert.equal(retries[0].key, retries[1].key); assert.equal(retries[0].body, retries[1].body);
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].find(button => button.textContent === window.admissionFixture.labels.approve).disabled), true);
    await page.evaluate(() => window.admissionFixture.switchActor()); await click('review'); await page.waitForSelector('pre');
    await page.type('textarea', 'Second independent review'); await click('approve');
    await page.waitForFunction(() => document.querySelector('article p')?.textContent.includes('VERIFIED'));
    const votes = await page.evaluate(() => window.admissionFixture.calls.filter(value => value.url.endsWith('/decisions')));
    assert.equal(JSON.parse(votes[0].body).submission_sha, JSON.parse(votes[2].body).submission_sha);
    assert.notEqual(votes[0].key, votes[2].key); assert.deepEqual(errors, []);
    passed++; console.log(`PASS ${language} explicit two-person UI, frozen facts, unknown same-key replay`); await page.close();
  }
  console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP (controlled HTTP fixture, not database/KMS acceptance)`);
} catch (error) { console.error(error); process.exitCode = 1; }
finally { if (browser) await browser.close(); if (vite) await vite.close(); fs.rmSync(directory, { recursive: true, force: true }); }
