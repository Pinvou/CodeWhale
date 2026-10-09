import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import assert from 'node:assert/strict';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || process.env.CHROMIUM || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', '/snap/bin/chromium', '/usr/bin/chromium', '/usr/bin/chromium-browser', '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome', '/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge'].find(fs.existsSync);
if (!chrome) throw new Error('Browser unavailable');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-f05-selection-')); let vite; let browser; let page; let passed = 0; let activeCase = 'setup';
try {
 vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } }); await vite.listen();
 browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] }); page = await browser.newPage();
 const errors = []; page.on('pageerror', error => errors.push(error.name));
 const fresh = async () => { errors.length = 0; await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f05-model-selection.html`); await page.waitForFunction(() => window.f05 && document.body.textContent.includes('Public selectable model')); };
 const choose = async () => { await page.evaluate(() => { const row = [...document.querySelectorAll('div')].find(el => el.className.includes('grid-cols-') && el.textContent.includes('Public selectable model')); row.querySelector('button').click(); }); };
 const run = async (name, fn) => { activeCase=name; await fn(); assert.deepEqual(errors, []); passed++; console.log(`PASS ${name}`); };
 await run('explicit catalog selection refreshes real effective configuration before returning to chat', async () => { await fresh(); assert.equal(await page.evaluate(() => window.f05.gate()), true); await choose(); await page.waitForFunction(() => window.f05.calls.includes('list')); assert.equal(await page.evaluate(() => window.f05.gate()), false, 'selected model must clear first-use gate'); assert.deepEqual(await page.evaluate(() => window.f05.calls), ['select:public-model-a','list','effective']); });
 await run('catalog selection controls and copy are available in all three shared locales', async () => { for (const language of ['zh', 'en', 'ja']) { await fresh(); await page.evaluate(async lang => { window.f05.language=lang; await window.f05.render(); },language); await page.waitForFunction(() => [...document.querySelectorAll('button')].some(el => el.title === window.f05.copy().setDefault)); assert.equal(await page.evaluate(() => { const row=[...document.querySelectorAll('div')].find(el => el.className.includes('grid-cols-') && el.textContent.includes('Public unavailable model')); return row.querySelector('button').disabled; }), true); } });
 await run('failed selection preserves first-use gate and does not refresh invented selection', async () => { await fresh(); await page.evaluate(() => window.f05.fail=true); await choose(); await page.waitForFunction(() => document.body.textContent.includes(window.f05.copy().platformModelsUnavailable)); assert.equal(await page.evaluate(() => window.f05.gate()), true); assert.deepEqual(await page.evaluate(() => window.f05.calls), ['select:public-model-a']); });
 console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
} catch (error) { console.error(`FAIL after ${passed} PASS in ${activeCase}: ${error.name} ${error instanceof assert.AssertionError ? error.message : ""}`); process.exitCode=1; }
finally { if (browser) await browser.close(); if (vite) await vite.close(); if (fs.existsSync(profile)) fs.rmSync(profile,{recursive:true,force:true}); }
