import { runSMSConflictCases } from './account_platform_sms_conflict_cases.mjs';
import { registrationRepairCopy } from '../src/shared/i18n.js';
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
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/snap/bin/chromium', '/usr/bin/chromium', '/usr/bin/chromium-browser',
  '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
].find(fs.existsSync);
if (!chrome) throw new Error('No browser available; UI validation not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-f01-ui-'));
let vite; let browser; let page; let passed = 0;
const assert = (condition, message) => { if (!condition) throw new Error(message); };
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  page = await browser.newPage(); const errors = []; const duplicateKeys = [];
  page.on('pageerror', error => errors.push(error.message)); page.on('dialog', dialog => dialog.accept());
  page.on('console', message => {
    if (/Encountered two children with the same key/.test(message.text())) duplicateKeys.push(message.text());
  });
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f01-ui.html`;
  const fresh = async kind => { errors.length = 0; await page.goto(url); await page.waitForFunction(() => window.f01 && document.body.textContent.includes('account-a')); if (kind === 'admin') { await page.evaluate(() => window.f01.render('admin')); await page.waitForSelector('tbody tr'); } };
  const clickText = async text => { const clicked = await page.evaluate(value => { const button = [...document.querySelectorAll('button')].find(item => item.textContent.trim() === value); if (!button || button.disabled) return false; button.click(); return true; }, text); assert(clicked, `button unavailable: ${text}`); };
  const waitText = async text => page.waitForFunction(value => document.body.textContent.includes(value), {}, text);
  const caseRun = async (name, action) => { await action(); assert(errors.length === 0, `React errors: ${errors.join(',')}`); passed++; console.log(`PASS ${name}`); };
  await caseRun('personal exact int64 balance through production BFF', async () => { await fresh(); await waitText('922,337,203,685,477.5807'); await waitText('-1.0001'); });
  await caseRun('missing balance permission does not query token', async () => { await fresh(); await page.evaluate(() => { globalThis.f01.balanceAllowed = false; globalThis.f01.calls.length = 0; globalThis.f01.switchIdentity(); }); await waitText('account-b'); assert(await page.evaluate(() => !globalThis.f01.calls.some(call => call.url.includes('/me/balance'))), 'unauthorized balance queried'); });
  await caseRun('balance unavailability is visible and never substituted with zero', async () => { await fresh(); await page.evaluate(() => { globalThis.f01.balanceFail = true; globalThis.f01.switchIdentity(); }); await page.waitForSelector('[role=alert]'); assert(await page.evaluate(() => document.querySelector('[role=alert]').textContent.length > 0), 'empty error'); assert(await page.evaluate(() => !document.body.textContent.includes('922,337,203')), 'old balance retained'); });
  for (const language of ['zh', 'en', 'ja']) for (const malformed of ['missing', 'null']) await caseRun(`${language}: ${malformed} bootstrap context shows recoverable error before child workspaces`, async () => {
    await fresh(); await page.select('header select', language);
    await page.evaluate(malformed => { globalThis.f01.badBootstrapContext = malformed; globalThis.f01.calls.length = 0; }, malformed);
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.refresh));
    await page.waitForFunction(() => document.querySelector('.ap-login [role=alert]')?.textContent === globalThis.f01.copy().account.requestFailed);
    assert(await page.evaluate(() => !document.querySelector('.ap-app') && !globalThis.f01.calls.some(call => call.url.includes('/me/balance') || call.url.includes('/me/organizations'))), 'invalid context mounted child or queried financial data');
    assert(await page.evaluate(() => document.querySelector('.ap-login a')?.textContent === globalThis.f01.copy().account.login), 'existing login recovery missing');
    await page.evaluate(() => { globalThis.f01.badBootstrapContext = ''; });
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.retry));
    await waitText('account-a'); await waitText('922,337,203,685,477.5807');
    assert(await page.evaluate(() => !globalThis.f01.calls.some(call => /models|catalog/.test(call.url))), 'personal balance unexpectedly required model facilities');
  });
  await caseRun('retired profile creation cannot complete against a new identity', async () => { await fresh(); await page.evaluate(() => { globalThis.f01.delayedProfile = true; }); const input = await page.$('input[autocomplete=username]'); await input.click({ clickCount: 3 }); await input.type('renamed-a'); const text = await page.evaluate(() => globalThis.f01.copy().account.confirmChange); await clickText(text); await page.waitForFunction(() => typeof globalThis.f01.releaseProfile === 'function'); await page.evaluate(() => globalThis.f01.switchIdentity()); await waitText('account-b'); await page.evaluate(() => globalThis.f01.releaseProfile()); assert(await page.evaluate(() => !globalThis.f01.calls.some(call => call.url.endsWith('/actions/complete'))), 'old workflow continued'); });
  await caseRun('profile completion with failed logout remains recoverable without navigation', async () => { await fresh(); await page.evaluate(() => { globalThis.f01.completedProfile = true; globalThis.f01.logoutFail = true; }); const input = await page.$('input[autocomplete=username]'); await input.click({ clickCount: 3 }); await input.type('renamed-a'); await clickText(await page.evaluate(() => globalThis.f01.copy().account.confirmChange)); await page.waitForSelector('[role=alert]'); assert(page.url() === url, 'navigated despite failed logout'); });
  for (const language of ['zh', 'en', 'ja']) await caseRun(`${language}: admin bootstrap context failure preserves existing error retry`, async () => {
    await fresh(); await page.select('header select', language);
    await page.evaluate(() => { globalThis.f01.badBootstrapContext = 'missing'; globalThis.f01.calls.length = 0; globalThis.f01.render('admin'); });
    await page.waitForFunction(() => document.querySelector('.adm-shell [role=alert]')?.firstChild?.textContent === globalThis.f01.copy().admin.requestFailed);
    assert(await page.evaluate(() => !document.querySelector('tbody') && !document.querySelector('.adm-shell nav') && !globalThis.f01.calls.some(call => call.url.includes('/admin/v1/users'))), 'invalid admin bootstrap mounted management workspace');
    await page.evaluate(() => { globalThis.f01.badBootstrapContext = ''; });
    await clickText(await page.evaluate(() => globalThis.f01.copy().admin.retry));
    await page.waitForSelector('tbody tr'); await waitText('account-a');
  });
  await caseRun('admin users keep enable and disable with no direct delete', async () => { await fresh('admin'); assert(await page.evaluate(() => ![...document.querySelectorAll('button')].some(button => /delete/i.test(button.textContent))), 'delete exposed'); await clickText(await page.evaluate(() => globalThis.f01.copy().admin.accounts.disable)); await waitText('DISABLED'); await clickText(await page.evaluate(() => globalThis.f01.copy().admin.accounts.enable)); await waitText('ACTIVE'); assert(await page.evaluate(() => globalThis.f01.calls.filter(call => call.url.endsWith('/status')).every(call => call.method === 'PATCH')), 'wrong status method'); });
  for (const language of ['zh','en','ja']) await caseRun(`${language}: unknown identity manual query uses original retry route without compensation`, async () => {
    await fresh('admin'); await page.select('header select',language);
    await page.evaluate(() => { globalThis.f01.unknownRegistration=true;globalThis.f01.calls.length=0; });
    await clickText(await page.evaluate(() => globalThis.f01.copy().admin.userManagement.registrationRepair));
    const row='tr[data-operation-status="IDENTITY_CREATING"]'; await page.waitForSelector(`${row} button`);
    const text=await page.$eval(`${row} button`,node=>node.textContent);
    assert(text===registrationRepairCopy[language].stages.IDENTITY_CREATING,'unknown action is not a localized identity query');
    assert(await page.$$eval(`${row} button`,nodes=>nodes.length)===1,'unknown offered compensation');
    await page.evaluate(() => { globalThis.f01.calls.length = 0; });
    await page.click(`${row} button`); await page.waitForFunction(()=>globalThis.f01.calls.some(call=>call.url.endsWith('/actions/retry')));
    await page.waitForNetworkIdle({ idleTime: 100 });
    assert(await page.evaluate(() => {
      // Bootstrap is a read-only POST that language refresh may legitimately finish here.
      // Every other POST in this action window must be the single original operation query.
      const posts = globalThis.f01.calls.filter(call => call.method === 'POST' && call.url !== '/api/account/v1/account/bootstrap');
      return posts.length === 1 && posts[0].url === `/api/admin/account/admin/v1/registration-operations/${globalThis.f01.id}/actions/retry`;
    }), 'query duplicated a command, requested compensation, or sent a new or foreign operation');
  });
  await caseRun('registration retry and compensation are actual wired actions', async () => { await fresh('admin'); await clickText(await page.evaluate(() => globalThis.f01.copy().admin.userManagement.registrationRepair)); const operationID = await page.evaluate(() => globalThis.f01.id);
    const row = `tr[data-operation-id="${operationID}"][data-operation-status="REPAIR_REQUIRED"]`;
    await page.waitForSelector(row, { visible: true });
    const language = await page.$eval('.adm-shell > header select', select => select.value);
    const status = await page.$eval(`${row} > td:nth-child(2)`, cell => cell.firstChild.textContent.trim());
    assert(status === registrationRepairCopy[language].statuses.REPAIR_REQUIRED, 'registration repair row lost its localized visible status');
    await page.$eval(row, (element, label) => [...element.querySelectorAll('button')].find(button => button.textContent.trim() === label)?.click(), await page.evaluate(() => globalThis.f01.copy().admin.userManagement.retry)); await page.waitForFunction(() => globalThis.f01.calls.some(call => call.url.endsWith('/actions/retry'))); await page.waitForFunction(row => [...document.querySelector(row)?.querySelectorAll('button') || []].some(button => button.textContent.trim() === globalThis.f01.copy().admin.userManagement.compensate && !button.disabled), {}, row);
    await page.$eval(row, (element, label) => [...element.querySelectorAll('button')].find(button => button.textContent.trim() === label)?.click(), await page.evaluate(() => globalThis.f01.copy().admin.userManagement.compensate)); await page.waitForFunction(() => globalThis.f01.calls.some(call => call.url.endsWith('/actions/compensate'))); });
  await caseRun('profile queue loads on first navigation and rejects wrong retry ACK', async () => { await fresh('admin'); await page.evaluate(() => { globalThis.f01.badRetry = true; }); await clickText(await page.evaluate(() => globalThis.f01.copy().admin.userManagement.profileRepair)); await waitText('REPAIR_REQUIRED'); await clickText(await page.evaluate(() => globalThis.f01.copy().admin.userManagement.retry)); await page.waitForSelector('[role=alert]'); assert(await page.evaluate(() => document.querySelector('[role=alert]').textContent.length > 0), 'wrong ACK silently accepted'); });
  await caseRun('queue 503 shows error rather than fake empty list', async () => { await fresh('admin'); await page.evaluate(() => { globalThis.f01.adminError = true; }); await clickText(await page.evaluate(() => globalThis.f01.copy().admin.userManagement.profileRepair)); await page.waitForSelector('[role=alert]'); assert(await page.evaluate(() => !document.querySelector('tbody')), 'fake collection displayed'); });
  await caseRun('SMS unknown ACK keeps the original key and body across language change', async () => { await fresh('admin'); await clickText(await page.evaluate(() => globalThis.f01.copy().admin.settings.title)); await page.waitForSelector('textarea'); await page.type('textarea', 'controlled correction reason'); await page.evaluate(() => { globalThis.f01.smsFail = true; }); await page.click('button[type=submit]'); await page.waitForFunction(() => globalThis.f01.smsCalls.length === 1); await page.waitForSelector('[role=alert]'); await page.select('.adm-shell > header select', 'ja'); await page.evaluate(() => { globalThis.f01.smsFail = false; }); await page.click('button[type=submit]'); await page.waitForFunction(() => globalThis.f01.smsCalls.length === 2); assert(await page.evaluate(() => globalThis.f01.smsCalls[0].key === globalThis.f01.smsCalls[1].key && JSON.stringify(globalThis.f01.smsCalls[0].command) === JSON.stringify(globalThis.f01.smsCalls[1].command)), 'lost ACK changed immutable facts'); await waitText(await page.evaluate(() => globalThis.f01.copy().admin.settings.sms.saved)); });
  await caseRun('all three languages display real personal labels', async () => { await fresh(); for (const language of ['ja', 'zh', 'en']) { await page.select('header select', language); await waitText(await page.evaluate(() => globalThis.f01.copy().account.profileSettings)); } });
  const desktopGuest = async () => { await fresh(); await page.evaluate(() => { globalThis.f01.authenticated = false; globalThis.f01.render('desktop'); }); await page.waitForSelector('.ap-login input[autocomplete=username]'); };
  await caseRun('desktop rejected password displays its localized reason without provider text', async () => {
    await desktopGuest();
    await page.evaluate(() => { globalThis.f01.loginError = 'oidc_credentials_invalid: SYNTHETIC_PROVIDER_DETAIL'; });
    await page.type('input[autocomplete=username]', 'controluser'); await page.type('input[type=password]', 'ControlPassword9');
    await page.click('form button:not([type=button])');
    await waitText(await page.evaluate(() => globalThis.f01.copy().account.errors.oidc_credentials_invalid));
    assert(await page.evaluate(() => !document.body.textContent.includes('SYNTHETIC_PROVIDER_DETAIL')), 'provider detail was exposed');
  });
  await caseRun('desktop SMS rate limit displays localized error and disables sending during cooldown', async () => {
    await desktopGuest();
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.authModes.sms));
    await page.type('input[autocomplete=tel-national]', '13800000000');
    await page.evaluate(() => { globalThis.f01.smsError = 'mobile_auth_failed: SMS_RATE_LIMITED:120'; });
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.sendCode)); await page.waitForSelector('[role=dialog]');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm));
    await waitText(await page.evaluate(() => globalThis.f01.copy().account.smsRateLimited));
    assert(await page.evaluate(() => [...document.querySelectorAll('.ap-login button')].some(button => button.disabled && /1\d\d/.test(button.textContent))), 'SMS rate limit did not disable its resend button');
  });
  const fillRegistration = async () => {
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.register));
    await page.type('input[autocomplete=username]', 'controluser'); await page.type('input[autocomplete=tel-national]', '13800000000');
    const passwordInputs = await page.$$('input[type=password]'); await passwordInputs[0].type('ControlPassword9'); await passwordInputs[1].type('ControlPassword9');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.sendCode)); await page.waitForSelector('[role=dialog]');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm)); await page.waitForFunction(() => globalThis.f01.nativeCalls.some(call => call[0] === 'sms'));
    await page.type('input[autocomplete=one-time-code]', '000000');
  };
  const registrationCaptcha = async language => {
    await fresh();
    await page.select('header select', language);
    await page.evaluate(() => { globalThis.f01.authenticated = false; globalThis.f01.render('desktop'); });
    await page.waitForSelector('.ap-login input[autocomplete=username]');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.register));
    await page.type('input[autocomplete=username]', 'controluser'); await page.type('input[autocomplete=tel-national]', '13800000000');
    const inputs = await page.$$('input[type=password]'); await inputs[0].type('ControlPassword9'); await inputs[1].type('ControlPassword9');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.sendCode)); await page.waitForSelector('[role=dialog]');
    assert(await page.evaluate(() => JSON.stringify(globalThis.f01.nativeCalls.map(call => call[0])) === JSON.stringify(['password-reference', 'captcha'])), 'availability or SMS requested before challenge completion');
  };
  for (const language of ['zh', 'en', 'ja']) for (const code of ['USERNAME_ALREADY_REGISTERED', 'MOBILE_ALREADY_REGISTERED', 'PASSWORD_REFERENCE_INVALID', 'PASSWORD_REFERENCE_CONFLICT', 'UPSTREAM_UNAVAILABLE']) await caseRun(`${language}: registration ${code} consumes challenge and safely prepares retry`, async () => {
    await registrationCaptcha(language);
    await page.evaluate(code => { globalThis.f01.smsError = { code }; }, code);
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm));
    await page.waitForFunction(() => !document.querySelector('[role=dialog]') && document.querySelector('[role=alert]'));
    const message = await page.evaluate(code => {
      const copy = globalThis.f01.copy().account;
      return code === 'USERNAME_ALREADY_REGISTERED' ? copy.usernameAlreadyRegistered : code === 'MOBILE_ALREADY_REGISTERED' ? copy.mobileAlreadyRegistered : copy.errors[code] || copy.requestFailed;
    }, code);
    assert(await page.$eval('[role=alert]', node => node.textContent) === message, 'registration error was not localized');
    assert(await page.evaluate(() => !globalThis.f01.nativeCalls.some(call => call[0] === 'register')), 'registration followed failed precheck');
    await page.evaluate(() => { globalThis.f01.smsError = null; });
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.sendCode)); await page.waitForSelector('[role=dialog]');
    const invalidReference = code.startsWith('PASSWORD_REFERENCE_');
    assert(await page.evaluate(() => globalThis.f01.nativeCalls.filter(call => call[0] === 'password-reference').length) === (invalidReference ? 2 : 1), 'password handle retry ownership changed');
    assert(await page.evaluate(() => globalThis.f01.nativeCalls.filter(call => call[0] === 'captcha').length === 2 && globalThis.f01.nativeCalls.filter(call => call[0] === 'sms').length === 1), 'retry reused proof or sent SMS before confirmation');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm));
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert(await page.evaluate(() => { const requests = globalThis.f01.nativeCalls.filter(call => call[0] === 'sms'); return requests.length === 2 && requests.every(([, body]) => body.password_handle === 'control-password-handle' && body.password_policy_version === 'control-policy-v1' && !('password' in body)); }), 'retry lost password reference or exposed plaintext');
  });
  await caseRun('retired registration SMS error cannot mutate the next account page', async () => {
    await registrationCaptcha('en');
    await page.evaluate(() => { globalThis.f01.delaySMS = true; globalThis.f01.smsError = { code: 'PASSWORD_REFERENCE_INVALID' }; });
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm));
    await page.waitForFunction(() => typeof globalThis.f01.releaseSMS === 'function');
    await page.evaluate(() => { globalThis.f01.authenticated = true; globalThis.f01.switchIdentity(); }); await waitText('account-b');
    await page.evaluate(() => globalThis.f01.releaseSMS());
    await page.waitForFunction(() => !document.querySelector('.ap-login') && !document.querySelector('[role=dialog]'));
    assert(await page.evaluate(() => !document.body.textContent.includes(globalThis.f01.copy().account.requestFailed) && !document.querySelector('.ap-login [role=alert]') && globalThis.f01.nativeCalls.filter(call => call[0] === 'sms').length === 1), 'retired response contaminated current account');
  });
  await caseRun('typed passwords require an explicit reveal and blur hides without shifting submit', async () => {
    await desktopGuest(); await page.type('input[autocomplete=username]', 'controluser'); await page.type('input[type=password]', 'ControlPassword9');
    assert(await page.evaluate(() => document.querySelector('[data-password-reveal-output]').hidden), 'typed password automatically revealed');
    await page.click('[data-password-input] > button');
    await page.waitForFunction(() => !document.querySelector('[data-password-reveal-output]').hidden);
    assert(await page.evaluate(() => getComputedStyle(document.querySelector('[data-password-reveal-output]')).position === 'absolute'), 'mirror changes layout');
    await page.click('form button:not([type=button])'); await waitText('account-a');
    assert(await page.evaluate(() => globalThis.f01.nativeCalls.some(call => call[0] === 'login')), 'reveal/blur prevented submission');
  });
  await caseRun('desktop normal password login uses the explicit Native transport', async () => { await desktopGuest(); await page.type('input[autocomplete=username]', 'controluser'); await page.type('input[type=password]', 'ControlPassword9'); await page.click('form button:not([type=button])'); await waitText('account-a'); assert(await page.evaluate(() => globalThis.f01.nativeCalls.some(call => call[0] === 'login')), 'login not sent'); });
  await caseRun('retired login completion cannot notify the host about the previous identity', async () => {
    await desktopGuest(); await page.evaluate(() => { globalThis.f01.delayNextBalance = true; });
    await page.type('input[autocomplete=username]', 'controluser'); await page.type('input[type=password]', 'ControlPassword9'); await page.click('form button:not([type=button])');
    await page.waitForFunction(() => typeof globalThis.f01.releaseBalance === 'function');
    await page.evaluate(() => globalThis.f01.switchIdentity()); await waitText('account-b');
    await page.evaluate(() => globalThis.f01.releaseBalance());
    assert(await page.evaluate(() => !globalThis.f01.authenticatedCallbacks.includes(globalThis.f01.id)), 'retired account notified the host');
  });
  await caseRun('desktop registration keeps password reference and SMS recovery facts through completion', async () => { await desktopGuest(); await fillRegistration(); await page.click('form button:not([type=button])'); await waitText('account-a'); assert(await page.evaluate(() => { const body = globalThis.f01.nativeCalls.find(call => call[0] === 'register')?.[1]; return body?.password_handle === 'control-password-handle' && body.password_policy_version === 'control-policy-v1' && body.registration_operation_id === globalThis.f01.id && body.registration_query_token === 'control-query-proof' && !('password' in body); }), 'registration facts lost'); });
  await caseRun('registration unknown ACK reads durable recovery and prevents a second new registration', async () => { await desktopGuest(); await fillRegistration(); await page.evaluate(() => { globalThis.f01.registerUnknown = true; }); await page.click('form button:not([type=button])'); await waitText('REPAIR_REQUIRED'); assert(await page.evaluate(() => [...document.querySelectorAll('nav button')].every(button => button.disabled)), 'unknown registration allowed restart'); assert(await page.evaluate(() => globalThis.f01.nativeCalls.filter(call => call[0] === 'register').length === 1), 'duplicate registration sent'); });
  await caseRun('persisted registration repair is recovered before opening a fresh registration', async () => { await fresh(); await page.evaluate(() => { globalThis.f01.authenticated = false; globalThis.f01.registration = { registration_operation_id: globalThis.f01.id, status: 'REPAIR_REQUIRED' }; globalThis.f01.render('desktop'); }); await waitText('REPAIR_REQUIRED'); assert(await page.evaluate(() => globalThis.f01.nativeCalls.filter(call => call[0] === 'register').length === 0), 'new registration used for recovery'); });
  await caseRun('desktop forgot password remains Native prepare/reset rather than plaintext Account API', async () => { await desktopGuest(); await clickText(await page.evaluate(() => globalThis.f01.copy().account.authModes.forgot)); await page.type('input[autocomplete=tel-national]', '13800000000'); const inputs = await page.$$('input[type=password]'); await inputs[0].type('ControlPassword9'); await inputs[1].type('ControlPassword9'); await clickText(await page.evaluate(() => globalThis.f01.copy().account.sendCode)); await page.waitForSelector('[role=dialog]'); await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm)); await page.waitForFunction(() => globalThis.f01.nativeCalls.some(call => call[0] === 'sms')); await page.type('input[autocomplete=one-time-code]', '000000'); await page.click('form button:not([type=button])'); await waitText(await page.evaluate(() => globalThis.f01.copy().account.passwordResetComplete)); assert(await page.evaluate(() => globalThis.f01.nativeCalls.some(call => call[0] === 'reset') && !globalThis.f01.calls.some(call => call.url.endsWith('/password-changes') || call.url.endsWith('/password-resets'))), 'plaintext public API used'); });
  await caseRun('personal workspaces preserve drafts on refresh and isolate new account state without duplicate React keys', async () => {
    assert(duplicateKeys.length === 0, `duplicate workspace React keys: ${duplicateKeys.length}`);
    await fresh();
    const input = await page.$('input[autocomplete=username]');
    await input.click(); await input.evaluate(node => node.select()); await input.type('draft-for-a');
    assert(await input.evaluate(node => node.value === 'draft-for-a'), 'profile draft was not entered');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.refresh));
    await page.waitForFunction(() => {
      const refresh = [...document.querySelectorAll('header button')].find(button => button.textContent.trim() === globalThis.f01.copy().account.refresh);
      return refresh && !refresh.disabled && document.querySelector('input[autocomplete=username]')?.value === 'draft-for-a';
    });
    await page.evaluate(() => globalThis.f01.switchIdentity()); await waitText('account-b');
    await page.waitForFunction(() => document.querySelector('input[autocomplete=username]')?.value === 'account-b');
    assert(duplicateKeys.length === 0, `duplicate workspace React keys: ${duplicateKeys.length}`);
  });
  passed += await runSMSConflictCases(page, `http://127.0.0.1:${vite.httpServer.address().port}`);
  console.log(`RESULT ${passed} PASS 0 FAIL 0 SKIP`);
 } catch (error) {
  console.error(`FAIL after ${passed} PASS: ${error.message}`);
  if (page) {
    const diagnostic = await page.evaluate(() => ({ text: document.body.textContent.slice(0, 1000), nativeMethods: window.f01?.nativeCalls.map(call => call[0]), lastRoutes: window.f01?.calls.slice(-5).map(call => call.url) })).catch(() => ({}));
    console.error(JSON.stringify(diagnostic));
  }
  process.exitCode = 1;
}
finally { if (browser) await browser.close(); if (vite) await vite.close(); if (fs.existsSync(profile)) fs.rmSync(profile, { recursive: true, force: true }); }
