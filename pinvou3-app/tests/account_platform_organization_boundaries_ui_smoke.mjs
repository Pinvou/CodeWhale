import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
import { createServer } from 'vite';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; no organization boundary validation performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-f06-boundaries-'));
let vite, browser, page;
const results = [];
const unavailable = { zh: '邀请已失效或不可用，请刷新邀请列表。', en: 'The invitation is expired or unavailable. Refresh invitations.', ja: '招待は期限切れか利用できません。招待一覧を更新してください。' };
const accepted = { zh: '邀请已接受，但当前身份尚未刷新确认。请刷新后继续。', en: 'The invitation was accepted, but the current identity refresh is unconfirmed. Refresh before continuing.', ja: '招待は受諾済みですが、現在のコンテキストの更新は未確認です。更新してから続行してください。' };
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', cacheDir: process.env.F06_VITE_CACHE || path.join(os.tmpdir(), 'pinvou-f06-boundaries-vite'), plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } }); await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] }); page = await browser.newPage(); page.setDefaultTimeout(4000); page.on('dialog', dialog => dialog.accept());
  const errors = []; page.on('pageerror', error => errors.push(error.message));
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-organization.html`;
  const fresh = async (language, mode = '') => { errors.length = 0; await page.goto(url); await page.waitForFunction(() => window.f06); await page.evaluate(({ language, mode }) => { Object.assign(window.f06, { language, boundaries: true, roleMode: mode }); window.f06.authorizationVersion++; window.f06.render(); }, { language, mode }); await page.waitForFunction(() => document.body.textContent.includes('Controlled Unit')); };
  const run = async (name, fn) => { try { await fn(); assert.deepEqual(errors, []); results.push({ name, pass: true }); } catch (error) { results.push({ name, pass: false, error: error.message }); } };
  const accept = async () => { await page.evaluate(() => { const form = [...document.querySelectorAll('form')].find(item => item.querySelector('input[type=password]')); assertInput(form); function assertInput(item) { if (!item) throw new Error('Invitation form absent'); const input = item.querySelector('input'); Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, 'PUBLIC_CONTROLLED_INVITATION'); input.dispatchEvent(new Event('input', { bubbles: true })); } }); await page.waitForFunction(() => [...document.querySelectorAll('form')].find(item => item.querySelector('input[type=password]'))?.querySelector('button')?.disabled === false); await page.evaluate(() => [...document.querySelectorAll('form')].find(item => item.querySelector('input[type=password]')).requestSubmit()); };
  for (const language of ['zh', 'en', 'ja']) {
    await run(`${language}: roles.assign without members.manage exposes only legal role control`, async () => { await fresh(language, 'roles'); await page.waitForFunction(() => document.body.textContent.includes(window.f06.user)); assert.equal(await page.evaluate(() => [...document.querySelectorAll('h4')].some(item => item.textContent === window.f06.copy().replaceRoles)), true); assert.equal(await page.evaluate(() => [...document.querySelectorAll('h4')].some(item => item.textContent === window.f06.copy().invite)), false); await page.evaluate(() => { const form = [...document.querySelectorAll('form')].find(item => item.querySelector('h4')?.textContent === window.f06.copy().replaceRoles); const selects = form.querySelectorAll('select'); selects[0].value = window.f06.user; selects[0].dispatchEvent(new Event('change', { bubbles: true })); selects[1].value = 'MEMBER'; selects[1].dispatchEvent(new Event('change', { bubbles: true })); }); await page.evaluate(() => [...document.querySelectorAll('form')].find(item => item.querySelector('h4')?.textContent === window.f06.copy().replaceRoles).requestSubmit()); await page.waitForSelector('[role=alertdialog]'); await page.click('[role=alertdialog] button:last-child'); await page.waitForFunction(() => [...document.querySelectorAll('[role=status]')].some(item => item.textContent === window.f06.copy().saved)); assert.equal(await page.evaluate(() => { const call = window.f06.calls.find(item => item.path.endsWith('/roles')); return call?.method === 'PUT' && JSON.stringify(JSON.parse(call.body)) === JSON.stringify({ roles: ['MEMBER'] }) && call.key.startsWith('finance-'); }), true); });
    await run(`${language}: members.manage without roles.assign hides role replacement`, async () => { await fresh(language, 'members'); await page.waitForFunction(() => document.body.textContent.includes(window.f06.user)); assert.equal(await page.evaluate(() => [...document.querySelectorAll('h4')].some(item => item.textContent === window.f06.copy().replaceRoles)), false); assert.equal(await page.evaluate(() => [...document.querySelectorAll('h4')].some(item => item.textContent === window.f06.copy().invite)), true); });
    await run(`${language}: expired invitation is definite denial, not unknown identity switch`, async () => { await fresh(language); await page.evaluate(() => { window.f06.invitationError = { status: 410, code: 'INVITATION_EXPIRED' }; }); await accept(); await page.waitForFunction(() => document.querySelector('[role=alert]')); assert.equal(await page.$eval('[role=alert]', el => el.firstChild.textContent), unavailable[language]); assert.equal(await page.evaluate(() => window.f06.calls.filter(call => call.path.endsWith('/invitations/accept')).length), 1); });
    await run(`${language}: unauthenticated invitation rejection does not claim an unknown switch`, async () => { await fresh(language); await page.evaluate(() => { window.f06.invitationError = { status: 401, code: 'AUTH_REQUIRED' }; }); await accept(); await page.waitForFunction(() => document.querySelector('[role=alert]')); assert.equal(await page.$eval('[role=alert]', el => el.firstChild.textContent), await page.evaluate(() => window.f06.copy().permissions)); });
    await run(`${language}: ambiguous invitation outcome remains unconfirmed`, async () => { await fresh(language); await page.evaluate(() => { window.f06.invitationError = { status: 503, code: 'INTERNAL_ERROR' }; }); await accept(); await page.waitForFunction(() => document.querySelector('[role=alert]')); assert.equal(await page.$eval('[role=alert]', el => el.firstChild.textContent), await page.evaluate(() => window.f06.copy().invitationUnknown)); assert.equal(await page.evaluate(() => window.f06.calls.filter(call => call.path.endsWith('/invitations/accept')).length), 1); });
    await run(`${language}: accepted invitation plus failed identity refresh is not a rejection`, async () => { await fresh(language); await page.evaluate(() => { window.f06.refreshFail = true; }); await accept(); await page.waitForFunction(() => document.querySelector('[role=alert]')); const text = await page.$eval('[role=alert]', el => el.firstChild.textContent); assert.equal(text, accepted[language]); assert.equal(await page.evaluate(() => window.f06.calls.filter(call => call.path.endsWith('/invitations/accept')).length), 1); });
  }
  await run('Pending native-shaped organization confirmation cannot dispatch', async () => {
    await fresh('en', 'roles');
    await page.evaluate(() => { const form = [...document.querySelectorAll('form')].find(item => item.querySelector('h4')?.textContent === window.f06.copy().replaceRoles); const selects = form.querySelectorAll('select'); selects[0].value = window.f06.user; selects[0].dispatchEvent(new Event('change', { bubbles: true })); selects[1].value = 'MEMBER'; selects[1].dispatchEvent(new Event('change', { bubbles: true })); });
    await page.evaluate(() => { window.f06.confirmCalls = 0; globalThis.confirm = () => { window.f06.confirmCalls++; return new Promise(() => {}); }; [...document.querySelectorAll('form')].find(item => item.querySelector('h4')?.textContent === window.f06.copy().replaceRoles).requestSubmit(); });
    await page.waitForFunction(() => document.querySelector('[role=alertdialog]') || window.f06.calls.some(call => call.method === 'PUT'));
    assert.equal(await page.evaluate(() => window.f06.calls.filter(call => call.method === 'PUT').length), 0, 'unanswered confirmation must not dispatch');
    assert.equal(await page.evaluate(() => window.f06.confirmCalls), 0, 'do not call the unavailable native shim');
    await page.keyboard.press('Escape'); assert.equal(await page.evaluate(() => window.f06.calls.filter(call => call.method === 'PUT').length), 0);
  });
  const harness = async (language = 'en', extra = {}) => {
    errors.length = 0; await page.goto(url); await page.waitForFunction(() => window.f06);
    await page.evaluate(({ language, extra }) => { Object.assign(window.f06, { language, confirmHarness: true }, extra); window.f06.render(); }, { language, extra });
    await page.waitForSelector('.ap-org-action');
  };
  const start = async () => { await page.evaluate(() => { const button = document.querySelector('.ap-org-action > button'); button.focus(); document.querySelector('.ap-org-action').requestSubmit(); }); await page.waitForSelector('[role=alertdialog]'); };
  const writes = () => page.evaluate(() => window.f06.calls.filter(call => ['POST','PUT','DELETE','PATCH'].includes(call.method)));
  for (const language of ['zh','en','ja']) await run(`${language}: explicit organization confirmation preserves legal empty units and optional root`, async () => {
    await harness(language); await start();
    assert.ok(await page.evaluate(() => { const dialog = document.querySelector('[role=alertdialog]'); return dialog.textContent.includes(window.f06.copy().confirm) && dialog.textContent.includes(window.f06.unit) && document.activeElement.textContent === window.f06.copy().cancelConfirmation; }));
    await page.keyboard.press('Tab'); assert.equal(await page.evaluate(() => document.activeElement.textContent), await page.evaluate(() => window.f06.copy().confirmAction));
    await page.keyboard.press('Tab'); assert.equal(await page.evaluate(() => document.activeElement.textContent), await page.evaluate(() => window.f06.copy().cancelConfirmation));
    await page.click('[role=alertdialog] button:first-child'); await page.waitForFunction(() => !document.querySelector('[role=alertdialog]'));
    assert.equal((await writes()).length, 0); assert.equal(await page.evaluate(() => document.activeElement.textContent), await page.evaluate(() => window.f06.copy().replaceMemberUnits));
    await start(); await page.evaluate(() => { const button = document.querySelector('[role=alertdialog] button:last-child'); button.click(); button.click(); });
    await page.waitForFunction(() => document.querySelector('[role=status]')?.textContent === window.f06.copy().saved);
    const calls = await writes(); assert.equal(calls.length, 1); assert.equal(calls[0].body, '{"unit_ids":[],"expected_version":1}'); assert.ok(calls[0].key.startsWith('finance-'));
  });
  for (const change of ['identity','client','scope','epoch','disabled','permission','unmount','version','values','options']) await run(`Pending organization confirmation invalidates ${change}`, async () => {
    await harness();
    if (change === 'options') { await page.select('.ap-org-action select[multiple]', await page.evaluate(() => window.f06.unit)); }
    await start();
    await page.evaluate(change => {
      document.querySelector('[aria-label=fixture-language]').focus();
      if (change === 'unmount') { window.f06.unmount(); return; }
      if (change === 'identity') window.f06.user = window.f06.other;
      if (change === 'client') window.f06.useAlternateClient = true;
      if (change === 'scope') window.f06.harnessScope = 'different-controlled-scope';
      if (change === 'epoch') window.f06.client.financeSessionEpoch++;
      if (change === 'disabled') window.f06.harnessDisabled = true;
      if (change === 'permission') window.f06.hideHarness = true;
      if (change === 'version') window.f06.harnessVersion = 2;
      if (change === 'options') window.f06.noOptions = true;
      if (change === 'values') { const select = document.querySelector('.ap-org-action select:not([multiple])'); select.value = window.f06.unit; select.dispatchEvent(new Event('change', { bubbles: true })); }
      else window.f06.render();
    }, change);
    await page.waitForFunction(() => !document.querySelector('[role=alertdialog]')); assert.equal((await writes()).length, 0);
    if (change !== 'unmount') assert.equal(await page.evaluate(() => document.activeElement.getAttribute('aria-label')), 'fixture-language');
  });
  await run('No-field organization target change rejects old accept and cancel callbacks', async () => {
    await harness('en', { noFields: true }); await start();
    await page.evaluate(() => { const buttons = document.querySelectorAll('[role=alertdialog] button'), props = button => button[Object.keys(button).find(key => key.startsWith('__reactProps$'))]; window.f06.oldCancel = props(buttons[0]).onClick; window.f06.oldConfirm = props(buttons[1]).onClick; window.f06.harnessTarget = window.f06.other; window.f06.render(); });
    await page.waitForFunction(() => !document.querySelector('[role=alertdialog]')); await start();
    assert.ok(await page.$eval('[role=alertdialog]', el => el.textContent.includes(window.f06.other)));
    await page.evaluate(() => { window.f06.oldConfirm(); window.f06.oldCancel(); }); assert.equal((await writes()).length, 0); assert.ok(await page.$('[role=alertdialog]'));
    await page.click('[role=alertdialog] button:last-child'); await page.waitForFunction(() => document.querySelector('[role=status]')?.textContent === window.f06.copy().saved);
    const calls = await writes(); assert.equal(calls.length, 1); assert.ok(calls[0].path.endsWith(`/role-bindings/${await page.evaluate(() => window.f06.other)}`));
  });
  await run('Unknown organization retry keeps original facts and key without preparation or confirmation', async () => {
    await harness('en', { harnessUnknown: true }); await start(); await page.click('[role=alertdialog] button:last-child');
    await page.waitForFunction(() => document.querySelector('[role=alert]')?.textContent === window.f06.copy().unknown);
    await page.evaluate(() => { window.f06.throwPrepare = true; window.f06.harnessDisabled = true; window.f06.noOptions = true; window.f06.harnessVersion = 9; window.f06.harnessUnknown = false; window.f06.language = 'ja'; globalThis.confirm = () => { throw new Error('Unknown retry must not confirm'); }; window.f06.render(); });
    await page.waitForFunction(() => document.querySelector('.ap-org-action > button')?.textContent === window.f06.copy().retry);
    await page.click('.ap-org-action > button'); await page.waitForFunction(() => document.querySelector('[role=status]')?.textContent === window.f06.copy().saved);
    const calls = await writes(); assert.equal(calls.length, 2); assert.equal(calls[0].body, calls[1].body); assert.equal(calls[0].key, calls[1].key); assert.equal(calls[1].body, '{"unit_ids":[],"expected_version":1}'); assert.equal(await page.$('[role=alertdialog]'), null);
  });
  await run('Unknown organization invocation cannot cross a new actor render before effects', async () => {
    await harness('en', { harnessUnknown: true }); await start(); await page.click('[role=alertdialog] button:last-child'); await page.waitForFunction(() => document.querySelector('[role=alert]')?.textContent === window.f06.copy().unknown);
    await page.evaluate(() => { const form = document.querySelector('.ap-org-action'), props = form[Object.keys(form).find(key => key.startsWith('__reactProps$'))]; window.f06.oldSubmit = props.onSubmit; window.f06.probeOnCommit = props.onSubmit; window.f06.user = window.f06.other; window.f06.render(); });
    await page.waitForFunction(() => window.f06.probeObserved);
    assert.deepEqual(await page.evaluate(() => window.f06.probeObserved), { before: 1, after: 1 }, 'actual layout probe runs before passive identity restoration');
    await page.waitForFunction(() => !document.querySelector('[role=alert]'));
    await page.evaluate(() => window.f06.oldSubmit({ preventDefault() {} }));
    assert.equal((await writes()).length, 1); assert.equal(await page.$('[role=alertdialog]'), null);
  });
  await run('Restored identity waits for a rendered hook snapshot before an unknown retry', async () => {
    await harness('en', { harnessUnknown: true }); await start(); await page.click('[role=alertdialog] button:last-child');
    await page.waitForFunction(() => document.querySelector('[role=alert]')?.textContent === window.f06.copy().unknown);
    await page.evaluate(() => { window.f06.user = window.f06.other; window.f06.submitAfterEffects = true; window.f06.render(); });
    await page.waitForFunction(() => window.f06.passiveObserved);
    assert.deepEqual(await page.evaluate(() => window.f06.passiveObserved), { before: 1, after: 1 }, 'after-effect callback must not combine old facts with a new identity/key');
    assert.equal((await writes()).length, 1); assert.equal(await page.$('[role=alertdialog]'), null);
  });
} finally { if (browser) await browser.close(); if (vite) await vite.close(); fs.rmSync(profile, { recursive: true, force: true }); }
console.log(JSON.stringify({ results, passed: results.filter(item => item.pass).length, failed: results.filter(item => !item.pass).length }));
if (results.some(item => !item.pass)) process.exitCode = 1;
