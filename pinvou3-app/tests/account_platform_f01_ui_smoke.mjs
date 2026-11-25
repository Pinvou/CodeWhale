import { runSMSConflictCases } from './account_platform_sms_conflict_cases.mjs';
import { navigateFixture } from './helpers/navigate-fixture.mjs';
import { accountAuthCopy, accountPlatformCopy, registrationRepairCopy, smsStatusCopy, smsTemplateCopy } from '../src/shared/i18n.js';
import { profileRepairQueueCopy } from '../src/shared/admin-profile-repair-i18n.js';
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
  'C:/Program Files (x86)/Google/Chrome/Application/chrome.exe',
  'C:/Program Files/Microsoft/Edge/Application/msedge.exe',
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe',
  '/snap/bin/chromium', '/usr/bin/chromium', '/usr/bin/chromium-browser',
  '/usr/bin/google-chrome', '/usr/bin/google-chrome-stable',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
].find(fs.existsSync);
if (!chrome) throw new Error('No browser available; UI validation not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-f01-ui-'));
let vite; let browser; let page; let passed = 0; let skipped = 0;
const assert = (condition, message) => { if (!condition) throw new Error(message); };
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
  page = await browser.newPage(); const errors = []; const duplicateKeys = [];
  // Safari 14 exposes secure getRandomValues but has no crypto.randomUUID.
  await page.evaluateOnNewDocument(() => {
    Object.defineProperty(document.defaultView.crypto, 'randomUUID', { value: undefined, configurable: true });
  });
  page.on('pageerror', error => errors.push(error.message)); page.on('dialog', dialog => dialog.accept());
  page.on('console', message => {
    if (/Encountered two children with the same key/.test(message.text())) duplicateKeys.push(message.text());
  });
  const url = `http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f01-ui.html`;
  const fresh = async kind => { errors.length = 0; await navigateFixture(page, url, signal => page.waitForFunction(() => window.f01 && document.body.textContent.includes('account-a'), { signal })); if (kind === 'admin') { await page.evaluate(() => { history.replaceState({}, '', '?section=users'); window.f01.render('admin'); }); await page.waitForSelector('tbody tr'); } };
  const clickText = async text => {
    const section = await page.evaluate(value => { const c = globalThis.f01.copy().admin; return [c.userManagement.registrationRepair, c.userManagement.profileRepair].includes(value) ? 'operations' : value === c.settings.title ? 'settings' : null; }, text);
    if (section) { await page.click(`.adm-sidebar [data-section=${section}]`); await page.waitForSelector('[data-workspace=' + section + ']:not([hidden])'); if (section === 'settings') return; }
 await page.waitForFunction(value => [...document.querySelectorAll('button')].some(item => item.textContent.trim() === value && item.getClientRects().length && !item.disabled), {}, text);
    const clicked = await page.evaluate(value => { const button = [...document.querySelectorAll('button')].find(item => item.textContent.trim() === value && item.getClientRects().length); if (!button || button.disabled) return false; button.click(); return true; }, text); assert(clicked, `button unavailable: ${text}`); };
  const waitText = async text => page.waitForFunction(value => document.body.textContent.includes(value), {}, text);
  const caseRun = async (name, action) => { if (process.env.F01_CASE && !name.includes(process.env.F01_CASE)) { skipped++; return; } await action(); assert(errors.length === 0, `React errors: ${errors.join(',')}`); passed++; console.log(`PASS ${name}`); };
  await caseRun('Safari 14 retains secure Web Crypto without randomUUID for every auth flow', async () => {
    await fresh();
    assert(await page.evaluate(() => typeof document.defaultView.crypto.randomUUID === 'undefined' && typeof document.defaultView.crypto.getRandomValues === 'function'), 'Safari 14 crypto boundary not installed');
  });
  await caseRun('personal exact int64 balance through production BFF', async () => { await fresh(); await waitText('922,337,203,685,477'); await waitText('-1.0001'); });
  await caseRun('missing balance permission does not query token', async () => { await fresh(); await page.evaluate(() => { globalThis.f01.balanceAllowed = false; globalThis.f01.calls.length = 0; globalThis.f01.switchIdentity(); }); await waitText('account-b'); assert(await page.evaluate(() => !globalThis.f01.calls.some(call => call.url.includes('/me/balance'))), 'unauthorized balance queried'); });
  await caseRun('balance unavailability is visible and never substituted with zero', async () => { await fresh(); await page.evaluate(() => { globalThis.f01.balanceFail = true; globalThis.f01.switchIdentity(); }); await page.waitForSelector('[role=alert]'); assert(await page.evaluate(() => document.querySelector('[role=alert]').textContent.length > 0), 'empty error'); assert(await page.evaluate(() => !document.body.textContent.includes('922,337,203')), 'old balance retained'); });
  for (const language of ['zh', 'en', 'ja']) for (const malformed of ['missing', 'null']) await caseRun(`${language}: ${malformed} bootstrap context shows recoverable error before child workspaces`, async () => {
    await fresh(); await page.select('.ap-sidebar-foot select, .adm-sidebar-foot select, header select', language);
    await page.evaluate(malformed => { globalThis.f01.badBootstrapContext = malformed; globalThis.f01.calls.length = 0; }, malformed);
    await page.click('[data-section=accountSettings]');
    await clickText(await page.evaluate(() => globalThis.f01.copy().center.refreshAccount));
    await page.waitForFunction(() => document.querySelector('.ap-login [role=alert]')?.textContent === globalThis.f01.copy().account.requestFailed);
    assert(await page.evaluate(() => !document.querySelector('.ap-app') && !globalThis.f01.calls.some(call => call.url.includes('/me/balance') || call.url.includes('/me/organizations'))), 'invalid context mounted child or queried financial data');
    assert(await page.evaluate(() => document.querySelector('.ap-login a')?.textContent === globalThis.f01.copy().account.login), 'existing login recovery missing');
    await page.evaluate(() => { globalThis.f01.badBootstrapContext = ''; });
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.retry));
    await waitText('account-a'); await waitText('922,337,203,685,477');
    assert(await page.evaluate(() => !globalThis.f01.calls.some(call => /models|catalog/.test(call.url))), 'personal balance unexpectedly required model facilities');
  });
  const openProfile = async () => { await page.click('.ap-sidebar [data-section=accountSettings]'); await page.waitForSelector('.ap-profile-row button'); await page.click('.ap-profile-row button'); await page.waitForSelector('[role=dialog] input[autocomplete=username]'); };
  await caseRun('retired profile creation cannot complete against a new identity', async () => { await fresh(); await openProfile(); await page.evaluate(() => { globalThis.f01.delayedProfile = true; }); const input = await page.$('input[autocomplete=username]'); await input.click({ clickCount: 3 }); await input.type('renamed-a'); const text = await page.evaluate(() => globalThis.f01.copy().account.confirmChange); await clickText(text); await page.waitForFunction(() => typeof globalThis.f01.releaseProfile === 'function'); await page.evaluate(() => globalThis.f01.switchIdentity()); await waitText('account-b'); await page.evaluate(() => globalThis.f01.releaseProfile()); assert(await page.evaluate(() => !globalThis.f01.calls.some(call => call.url.endsWith('/actions/complete'))), 'old workflow continued'); });
  await caseRun('profile completion with failed logout remains recoverable without navigation', async () => { await fresh(); await openProfile(); await page.evaluate(() => { globalThis.f01.completedProfile = true; globalThis.f01.logoutFail = true; }); const input = await page.$('input[autocomplete=username]'); await input.click({ clickCount: 3 }); await input.type('renamed-a'); await clickText(await page.evaluate(() => globalThis.f01.copy().account.confirmChange)); await page.waitForSelector('[role=alert]'); assert(new URL(page.url()).pathname === new URL(url).pathname && new URL(page.url()).searchParams.get('section') === 'accountSettings', 'navigated despite failed logout'); });
  for (const language of ['zh', 'en', 'ja']) await caseRun(`${language}: admin bootstrap context failure preserves existing error retry`, async () => {
    await fresh(); await page.select('.ap-sidebar-foot select, .adm-sidebar-foot select, header select', language);
    await page.evaluate(() => { globalThis.f01.badBootstrapContext = 'missing'; globalThis.f01.calls.length = 0; history.replaceState({}, '', '?section=users'); globalThis.f01.render('admin'); });
    await page.waitForFunction(() => document.querySelector('.adm-shell [role=alert]')?.firstChild?.textContent === globalThis.f01.copy().admin.requestFailed);
    assert(await page.evaluate(() => !document.querySelector('tbody') && !document.querySelector('.adm-shell nav button') && !globalThis.f01.calls.some(call => call.url.includes('/admin/v1/users'))), 'invalid admin bootstrap mounted management workspace');
    await page.evaluate(() => { globalThis.f01.badBootstrapContext = ''; });
    await clickText(await page.evaluate(() => globalThis.f01.copy().admin.retry));
    await page.waitForSelector('tbody tr'); await waitText('managed-account');
  });
  await caseRun('admin users keep enable and disable with no direct delete', async () => {
    await fresh('admin');
    assert(await page.evaluate(() => ![...document.querySelectorAll('button')].some(button => /delete/i.test(button.textContent))), 'delete exposed');
    await clickText(await page.evaluate(() => globalThis.f01.copy().admin.accounts.disable));
    await page.click('[role=dialog] [data-user-status-confirm="DISABLED"]');
    await page.waitForFunction(() => globalThis.f01.status === 'DISABLED');
    await clickText(await page.evaluate(() => globalThis.f01.copy().admin.accounts.enable));
    await page.click('[role=dialog] [data-user-status-confirm="ACTIVE"]');
    await page.waitForFunction(() => globalThis.f01.status === 'ACTIVE');
    assert(await page.evaluate(() => globalThis.f01.calls.filter(call => call.url.endsWith('/status')).every(call => call.method === 'PATCH')), 'wrong status method');
  });
  for (const language of ['zh','en','ja']) await caseRun(`${language}: unknown identity manual query uses original retry route without compensation`, async () => {
    await fresh('admin'); await page.select('.ap-sidebar-foot select, .adm-sidebar-foot select, header select',language);
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
    const language = await page.$eval('.adm-sidebar-foot select', select => select.value);
    const status = await page.$eval(`${row} > td:nth-child(2)`, cell => cell.firstChild.textContent.trim());
    assert(status === registrationRepairCopy[language].statuses.REPAIR_REQUIRED, 'registration repair row lost its localized visible status');
    await page.$eval(row, (element, label) => [...element.querySelectorAll('button')].find(button => button.textContent.trim() === label)?.click(), await page.evaluate(() => globalThis.f01.copy().admin.userManagement.retry)); await page.waitForFunction(() => globalThis.f01.calls.some(call => call.url.endsWith('/actions/retry'))); await page.waitForFunction(row => [...document.querySelector(row)?.querySelectorAll('button') || []].some(button => button.textContent.trim() === globalThis.f01.copy().admin.userManagement.compensate && !button.disabled), {}, row);
    await page.$eval(row, (element, label) => [...element.querySelectorAll('button')].find(button => button.textContent.trim() === label)?.click(), await page.evaluate(() => globalThis.f01.copy().admin.userManagement.compensate)); await page.waitForFunction(() => globalThis.f01.calls.some(call => call.url.endsWith('/actions/compensate'))); });
  await caseRun('profile queue loads on first navigation and rejects wrong retry ACK', async () => { await fresh('admin'); await page.evaluate(() => { globalThis.f01.badRetry = true; }); await clickText(await page.evaluate(() => globalThis.f01.copy().admin.userManagement.profileRepair)); const row='[data-workspace=profile]:not([hidden]) tr[data-operation-status="REPAIR_REQUIRED"]'; await page.waitForSelector(row,{visible:true}); const statusText=await page.$eval(row+' > td:nth-child(2)',cell=>cell.textContent.trim()); const language=await page.$eval('.adm-sidebar-foot select',select=>select.value); assert(statusText===profileRepairQueueCopy[language].statuses.REPAIR_REQUIRED,'profile row lost its localized visible status'); assert(await page.$eval(row, element => !element.querySelector('td:first-child small')), 'profile repair DTO has no registration stage; do not invent a missing-status warning'); await clickText(await page.evaluate(() => globalThis.f01.copy().admin.userManagement.retry)); await page.waitForFunction(() => globalThis.f01.calls.some(call => call.url.includes('/profile-change-requests/') && call.url.endsWith('/actions/retry'))); await page.waitForSelector('[role=alert]'); assert(await page.evaluate(() => document.querySelector('[role=alert]').textContent.length > 0), 'wrong ACK silently accepted'); });
  await caseRun('queue 503 shows error rather than fake empty list', async () => { await fresh('admin'); await page.evaluate(() => { globalThis.f01.adminError = true; }); await clickText(await page.evaluate(() => globalThis.f01.copy().admin.userManagement.profileRepair)); await page.waitForSelector('[role=alert]'); assert(await page.evaluate(() => !document.querySelector('[data-workspace=profile]:not([hidden]) tbody')), 'fake collection displayed'); });
  await caseRun('SMS unknown ACK keeps the original key and body across language change', async () => {
    await fresh('admin');
    await clickText(await page.evaluate(() => globalThis.f01.copy().admin.settings.title));
    await page.waitForSelector('.adm-sms-form textarea');
    await page.type('.adm-sms-form textarea', 'controlled correction reason');
    assert(await page.$eval('.adm-sms-form textarea', node => node.value === 'controlled correction reason'), 'SMS reason input was reset before submission');
    await page.waitForFunction(() => !document.querySelector('.adm-sms-form button[type=submit]').disabled);
    await page.evaluate(() => { globalThis.f01.smsFail = true; });
    await page.click('.adm-sms-form button[type=submit]');
    await page.waitForFunction(() => globalThis.f01.smsCalls.length === 1);
    const language = await page.$eval('.adm-sidebar-foot select', select => select.value);
    await page.waitForFunction(text => document.querySelector('.adm-sms-unknown[role=status]')?.textContent.includes(text), {}, smsStatusCopy[language].unknownWrite);
    await page.select('.adm-sidebar-foot select', 'ja');
    await page.waitForFunction(text => document.querySelector('.adm-sms-unknown[role=status]')?.textContent.includes(text), {}, smsStatusCopy.ja.unknownWrite);
    await page.evaluate(() => { globalThis.f01.smsFail = false; });
    await page.click('.adm-sms-form button[type=submit]');
    await page.waitForFunction(() => globalThis.f01.smsCalls.length === 2);
    assert(await page.evaluate(() => globalThis.f01.smsCalls[0].key === globalThis.f01.smsCalls[1].key && JSON.stringify(globalThis.f01.smsCalls[0].command) === JSON.stringify(globalThis.f01.smsCalls[1].command)), 'lost ACK changed immutable facts');
    const revision = await page.evaluate(() => globalThis.f01.smsCalls[0].command.expected_revision + 1);
    await page.waitForFunction(text => document.querySelector('.adm-sms-save-feedback[role=status]')?.textContent.includes(text), {}, smsTemplateCopy.ja.replaySaved.replace('{revision}', String(revision)));
  });
  for (const [firstError, definiteError] of [[{ status: 503, code: 'UPSTREAM_UNAVAILABLE' }, { status: 400, code: 'INVALID_SMS_CONFIGURATION' }], ['network', { status: 409, code: 'IDEMPOTENCY_KEY_CONFLICT' }]]) await caseRun(`SMS unknown retry exposes definite ${definiteError.status} and keeps the original invocation`, async () => {
    await fresh('admin');
    await clickText(await page.evaluate(() => globalThis.f01.copy().admin.settings.title));
    await page.waitForSelector('.adm-sms-form textarea');
    await page.type('.adm-sms-form textarea', 'controlled correction reason');
    await page.waitForFunction(() => !document.querySelector('.adm-sms-form button[type=submit]').disabled);
    await page.evaluate(error => { globalThis.f01.smsWriteError = error; }, firstError);
    await page.click('.adm-sms-form button[type=submit]');
    await page.waitForSelector('.adm-sms-unknown[role=status]');
    await page.evaluate(error => { globalThis.f01.smsWriteError = error; }, definiteError);
    await page.click('.adm-sms-form button[type=submit]');
    const failure = await page.evaluate(() => globalThis.f01.copy().admin.settings.sms.saveFailed);
    await page.waitForFunction(text => document.querySelector('.adm-sms-error[role=alert] > span')?.textContent === text, {}, failure);
    assert((await page.evaluate(() => globalThis.f01.smsCalls.length)) === 2, 'the definite response belongs to the second attempt');
    assert(await page.$('.adm-sms-unknown[role=status]'), 'a rejected retry does not resolve the original unknown write');
    await page.evaluate(() => { globalThis.f01.smsWriteError = null; });
    await page.click('.adm-sms-form button[type=submit]');
    await page.waitForFunction(() => globalThis.f01.smsCalls.length === 3 && !document.querySelector('.adm-sms-unknown'));
    assert((await page.evaluate(() => new Set(globalThis.f01.smsCalls.map(call => call.key)).size)) === 1, 'all attempts retain the original key');
    assert((await page.evaluate(() => new Set(globalThis.f01.smsCalls.map(call => JSON.stringify(call.command))).size)) === 1, 'all attempts retain the original command');
  });
  await caseRun('all three languages display real personal labels', async () => { await fresh(); for (const language of ['ja', 'zh', 'en']) { await page.select('.ap-sidebar-foot select, .adm-sidebar-foot select, header select', language); await waitText(await page.evaluate(() => globalThis.f01.copy().account.profileSettings)); } });
  const desktopGuest = async () => { await fresh(); await page.evaluate(() => { globalThis.f01.authenticated = false; globalThis.f01.render('desktop'); }); await page.waitForSelector('.ap-login input[autocomplete=tel-national]'); };
  const desktopPassword = async () => {
    await desktopGuest();
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.authModes.password));
    await page.waitForSelector('.ap-login input[autocomplete=username]');
  };
  await caseRun('desktop rejected password displays its localized reason without provider text', async () => {
    await desktopPassword();
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
  await caseRun('changing the SMS signup choice retires the original challenge and sends consent on the new one', async () => {
    await desktopGuest();
    await page.type('input[autocomplete=tel-national]', '13800000000');
    const send = async () => {
      await clickText(await page.evaluate(() => globalThis.f01.copy().account.sendCode));
      await page.waitForSelector('[role=dialog]');
      await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm));
      await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    };
    await send();
    await page.type('input[autocomplete=one-time-code]', '000000');
    await page.click('.ap-auth-signup-consent input');
    assert(await page.$eval('.ap-auth-submit', node => node.disabled), 'changed consent reused the old SMS challenge');
    const sendLabel = await page.evaluate(() => globalThis.f01.copy().account.sendCode);
    await page.waitForFunction(label => [...document.querySelectorAll('.ap-auth-code button')].some(node => node.textContent === label && !node.disabled), {}, sendLabel);
    await send();
    assert(await page.evaluate(() => {
      const sent = globalThis.f01.nativeCalls.filter(([method]) => method === 'sms');
      return sent.length === 2 && !('signup_consent_version' in sent[0][1])
        && sent[1][1].purpose === 'LOGIN' && sent[1][1].signup_consent_version === 'mobile-signup-v1';
    }), 'consent was not scoped to the new SMS challenge');
    await page.type('input[autocomplete=one-time-code]', '000000');
    await page.click('.ap-auth-submit');
    await waitText('account-a');
    assert(await page.evaluate(() => globalThis.f01.nativeCalls.some(([method]) => method === 'sms-login')), 'SMS login was not used');
  });
  for (const [code, role, message] of [
    ['REGISTRATION_RECOVERING', 'status', accountAuthCopy.en.smsSignupRecovering],
    ['REGISTRATION_MANUAL_REVIEW_REQUIRED', 'status', accountAuthCopy.en.smsSignupManualReview],
    ['SMS_UNAVAILABLE', 'alert', accountPlatformCopy.en.smsUnavailable],
  ]) await caseRun('desktop SMS signup feedback ' + code + ' requires a new code', async () => {
    await desktopGuest();
    await page.type('input[autocomplete=tel-national]', '13800000000');
    await page.click('.ap-auth-signup-consent input');
    const send = async () => {
      await clickText(await page.evaluate(() => globalThis.f01.copy().account.sendCode));
      await page.waitForSelector('[role=dialog]');
      await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm));
      await page.waitForFunction(() => !document.querySelector('[role=dialog]') && !document.querySelector('.ap-auth-submit').disabled);
    };
    await send();
    const sessionChecks = await page.evaluate(() => globalThis.f01.nativeSessionChecks);
    await page.evaluate(code => { globalThis.f01.smsLoginError = 'mobile_auth_failed: ' + code; }, code);
    await page.type('input[autocomplete=one-time-code]', '000000');
    await page.click('.ap-auth-submit');
    await page.waitForFunction((role, message) =>
      document.querySelector('.ap-login .ap-alert[role=' + role + ']')?.textContent === message, {}, role, message);
    assert(await page.evaluate(expectedSessionChecks => {
      const { authenticated, authenticatedCallbacks, nativeCalls, nativeSessionChecks, id } = globalThis.f01;
      const attempts = nativeCalls.filter(([method]) => method === 'sms-login');
      return !authenticated && authenticatedCallbacks.length === 0 &&
        nativeSessionChecks === expectedSessionChecks &&
        attempts.length === 1 && attempts[0][1].challenge_id === id &&
        document.querySelector('input[autocomplete=one-time-code]').value === '' &&
        !document.querySelector('input[autocomplete=tel-national]').disabled &&
        document.querySelector('.ap-auth-submit').disabled;
    }, sessionChecks), 'failed SMS login kept the old challenge or started a success refresh');

    await page.evaluate(() => {
      globalThis.f01.smsLoginError = null;
      globalThis.f01.smsChallengeId = globalThis.f01.other;
    });
    await send();
    await page.type('input[autocomplete=one-time-code]', '111111');
    await page.click('.ap-auth-submit');
    await page.waitForFunction(() => globalThis.f01.authenticatedCallbacks.length > 0);
    assert(await page.evaluate(() => {
      const { authenticated, authenticatedCallbacks, nativeCalls, id, other } = globalThis.f01;
      const attempts = nativeCalls.filter(([method]) => method === 'sms-login');
      return authenticated && authenticatedCallbacks.length === 1 && authenticatedCallbacks[0] === id &&
        attempts.length === 2 &&
        attempts[1][1].challenge_id === other && attempts[1][1].code === '111111';
    }), 'fresh code did not authenticate with its own challenge');
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
    await page.select('.ap-sidebar-foot select, .adm-sidebar-foot select, header select', language);
    await page.evaluate(() => { globalThis.f01.authenticated = false; globalThis.f01.render('desktop'); });
    await page.waitForSelector('.ap-login input[autocomplete=tel-national]');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.register));
    await page.type('input[autocomplete=username]', 'controluser'); await page.type('input[autocomplete=tel-national]', '13800000000');
    const inputs = await page.$$('input[type=password]'); await inputs[0].type('ControlPassword9'); await inputs[1].type('ControlPassword9');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.sendCode)); await page.waitForSelector('[role=dialog]');
    assert(await page.evaluate(() => JSON.stringify(globalThis.f01.nativeCalls.map(call => call[0])) === JSON.stringify(['captcha'])), 'availability or SMS requested before challenge completion');
  };
  await caseRun('Safari 14 registration dispatches a secure preparation after captcha', async () => {
    await registrationCaptcha('en');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm));
    await page.waitForFunction(() => globalThis.f01.nativeCalls.some(([method]) => method === 'password-authorization') || Boolean(document.querySelector('[role=alert]')));
    const outcome = await page.evaluate(() => ({
      operations: globalThis.f01.nativeCalls.filter(([method]) => method === 'password-authorization').map(([, body]) => body.operation_id),
      error: document.querySelector('[role=alert]')?.textContent || '',
    }));
    assert(outcome.operations.length === 1, `Safari 14 password preparation was not dispatched: ${outcome.error}`);
    assert(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(outcome.operations[0]), 'preparation operation is not a secure UUID v4');
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert(await page.evaluate(() => globalThis.f01.nativeCalls.filter(([method]) => method === 'sms').length === 1), 'Safari 14 preparation did not continue exactly once to SMS');
  });
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
    assert(await page.evaluate(() => globalThis.f01.nativeCalls.filter(call => call[0] === 'password-reference').length) === 1, 'password handle retry ownership changed');
    assert(await page.evaluate(() => globalThis.f01.nativeCalls.filter(call => call[0] === 'captcha').length === 2 && globalThis.f01.nativeCalls.filter(call => call[0] === 'sms').length === 1), 'retry reused proof or sent SMS before confirmation');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm));
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert(await page.evaluate(() => { const requests = globalThis.f01.nativeCalls.filter(call => call[0] === 'sms'); return requests.length === 2 && requests.every(([, body]) => body.password_handle === 'control-password-handle' && body.password_policy_version === 'control-policy-v1' && !('password' in body)); }), 'retry lost password reference or exposed plaintext');
    assert(await page.evaluate(code => globalThis.f01.nativeCalls.filter(call => call[0] === 'password-reference').length === (code.startsWith('PASSWORD_REFERENCE_') ? 2 : 1), code), 'retry changed accepted reference ownership');
  });
  await caseRun('Safari 14 password preparation uncertain ACK retains one secure operation and does not send SMS', async () => {
    await registrationCaptcha('en');
    await page.evaluate(() => { globalThis.f01.preparationError = { code: 'UPSTREAM_UNAVAILABLE' }; });
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm));
    await page.waitForFunction(() => document.querySelector('[role=alert]') && !document.querySelector('[role=dialog] button[disabled]'));
    assert(await page.evaluate(() => Boolean(document.querySelector('[role=dialog]')) && !globalThis.f01.nativeCalls.some(([method]) => method === 'sms')), 'uncertain preparation sent SMS or discarded recovery');
    await page.evaluate(() => { globalThis.f01.preparationError = null; });
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm));
    await page.waitForFunction(() => !document.querySelector('[role=dialog]'));
    assert(await page.evaluate(() => { const attempts=globalThis.f01.nativeCalls.filter(([method]) => method === 'password-authorization').map(([,body])=>body); return attempts.length === 2 && /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(attempts[0].operation_id) && JSON.stringify(attempts[0]) === JSON.stringify(attempts[1]); }), 'uncertain preparation allocated a new operation');
    assert(await page.evaluate(() => globalThis.f01.nativeCalls.filter(([method])=>method==='sms').length===1), 'preparation retry duplicated SMS');
  });
  for (const language of ['zh','en','ja']) await caseRun(`${language}: reset prepares only after challenge and submits only original reference`, async () => {
    await fresh();await page.select('.ap-sidebar-foot select, .adm-sidebar-foot select, header select',language);
    await page.evaluate(() => {globalThis.f01.authenticated=false;globalThis.f01.render('desktop');});
    await page.waitForSelector('.ap-login input[autocomplete=tel-national]');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.authModes.forgot));
    await page.type('input[autocomplete=tel-national]','13800000000');
    const inputs=await page.$$('input[type=password]');await inputs[0].type('ControlPassword9');await inputs[1].type('ControlPassword9');
    await clickText(await page.evaluate(() => globalThis.f01.copy().account.sendCode));await page.waitForSelector('[role=dialog]');
    assert(await page.evaluate(()=>JSON.stringify(globalThis.f01.nativeCalls.map(([method])=>method))===JSON.stringify(['captcha'])), 'reset prepared password before challenge');
    await clickText(await page.evaluate(()=>globalThis.f01.copy().account.captchaConfirm));await page.waitForFunction(()=>!document.querySelector('[role=dialog]'));
    await page.type('input[autocomplete=one-time-code]','000000');
    await clickText(await page.evaluate(()=>globalThis.f01.copy().account.resetPassword));
    await page.waitForFunction(()=>globalThis.f01.nativeCalls.some(([method])=>method==='reset'));
    assert(await page.evaluate(()=> {const calls=globalThis.f01.nativeCalls;const reset=calls.find(([method])=>method==='reset')[1];return calls.filter(([method])=>method==='password-reference').length===1 && calls.find(([method])=>method==='password-authorization')[1].purpose==='PASSWORD_RESET' && reset.password_handle==='control-password-handle' && reset.password_policy_version==='control-policy-v1' && !('password' in reset);}), 'reset reallocated or exposed password to Account');
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
    await desktopPassword(); await page.type('input[autocomplete=username]', 'controluser'); await page.type('input[type=password]', 'ControlPassword9');
    assert(await page.evaluate(() => document.querySelector('[data-password-reveal-output]').hidden), 'typed password automatically revealed');
    await page.click('[data-password-input] > button');
    await page.waitForFunction(() => !document.querySelector('[data-password-reveal-output]').hidden);
    assert(await page.evaluate(() => getComputedStyle(document.querySelector('[data-password-reveal-output]')).position === 'absolute'), 'mirror changes layout');
    await page.click('form button:not([type=button])'); await waitText('account-a');
    assert(await page.evaluate(() => globalThis.f01.nativeCalls.some(call => call[0] === 'login')), 'reveal/blur prevented submission');
  });
  await caseRun('desktop normal password login uses the explicit Native transport', async () => { await desktopPassword(); await page.type('input[autocomplete=username]', 'controluser'); await page.type('input[type=password]', 'ControlPassword9'); await page.click('form button:not([type=button])'); await waitText('account-a'); assert(await page.evaluate(() => globalThis.f01.nativeCalls.some(call => call[0] === 'login')), 'login not sent'); });
  await caseRun('retired login completion cannot notify the host about the previous identity', async () => {
    await desktopPassword(); await page.evaluate(() => { globalThis.f01.delayNextBalance = true; });
    await page.type('input[autocomplete=username]', 'controluser'); await page.type('input[type=password]', 'ControlPassword9'); await page.click('form button:not([type=button])');
    await page.waitForFunction(() => typeof globalThis.f01.releaseBalance === 'function');
    await page.evaluate(() => globalThis.f01.switchIdentity()); await waitText('account-b');
    await page.evaluate(() => globalThis.f01.releaseBalance());
    assert(await page.evaluate(() => !globalThis.f01.authenticatedCallbacks.includes(globalThis.f01.id)), 'retired account notified the host');
  });
  await caseRun('desktop registration keeps password reference and SMS recovery facts through completion', async () => { await desktopGuest(); await fillRegistration(); await page.click('form button:not([type=button])'); await waitText('account-a'); assert(await page.evaluate(() => { const body = globalThis.f01.nativeCalls.find(call => call[0] === 'register')?.[1]; return body?.password_handle === 'control-password-handle' && body.password_policy_version === 'control-policy-v1' && body.registration_operation_id === globalThis.f01.id && body.registration_query_token === 'control-query-proof' && !('password' in body); }), 'registration facts lost'); });
  await caseRun('registration unknown ACK reads durable recovery and prevents a second new registration', async () => { await desktopGuest(); await fillRegistration(); await page.evaluate(() => { globalThis.f01.registerUnknown = true; }); await page.click('form button:not([type=button])'); await waitText('Manual repair required'); assert(await page.evaluate(() => [...document.querySelectorAll('nav button')].every(button => button.disabled)), 'unknown registration allowed restart'); assert(await page.evaluate(() => globalThis.f01.nativeCalls.filter(call => call[0] === 'register').length === 1), 'duplicate registration sent'); });
  await caseRun('persisted registration repair is recovered before opening a fresh registration', async () => { await fresh(); await page.evaluate(() => { globalThis.f01.authenticated = false; globalThis.f01.registration = { registration_operation_id: globalThis.f01.id, status: 'REPAIR_REQUIRED' }; globalThis.f01.render('desktop'); }); await waitText('Manual repair required'); assert(await page.evaluate(() => globalThis.f01.nativeCalls.filter(call => call[0] === 'register').length === 0), 'new registration used for recovery'); });
  await caseRun('desktop forgot password remains Native prepare/reset rather than plaintext Account API', async () => { await desktopGuest(); await clickText(await page.evaluate(() => globalThis.f01.copy().account.authModes.forgot)); await page.type('input[autocomplete=tel-national]', '13800000000'); const inputs = await page.$$('input[type=password]'); await inputs[0].type('ControlPassword9'); await inputs[1].type('ControlPassword9'); await clickText(await page.evaluate(() => globalThis.f01.copy().account.sendCode)); await page.waitForSelector('[role=dialog]'); await clickText(await page.evaluate(() => globalThis.f01.copy().account.captchaConfirm)); await page.waitForFunction(() => globalThis.f01.nativeCalls.some(call => call[0] === 'sms')); await page.type('input[autocomplete=one-time-code]', '000000'); await page.click('form button:not([type=button])'); await waitText(await page.evaluate(() => globalThis.f01.copy().account.passwordResetComplete)); assert(await page.evaluate(() => globalThis.f01.nativeCalls.some(call => call[0] === 'reset') && !globalThis.f01.calls.some(call => call.url.endsWith('/password-changes') || call.url.endsWith('/password-resets'))), 'plaintext public API used'); });
  await caseRun('personal workspaces preserve drafts on refresh and isolate new account state without duplicate React keys', async () => {
    assert(duplicateKeys.length === 0, `duplicate workspace React keys: ${duplicateKeys.length}`);
    await fresh(); await openProfile();
    const input = await page.$('input[autocomplete=username]');
    await input.click(); await input.evaluate(node => node.select()); await input.type('draft-for-a');
    assert(await input.evaluate(node => node.value === 'draft-for-a'), 'profile draft was not entered');
    await page.click('[data-section=accountSettings]');
    await clickText(await page.evaluate(() => globalThis.f01.copy().center.refreshAccount));
    await page.waitForFunction(() => {
      const refresh = [...document.querySelectorAll('.ap-account-information button, header button')].find(button => button.textContent.trim() === globalThis.f01.copy().center.refreshAccount);
      return refresh && !refresh.disabled && document.querySelector('input[autocomplete=username]')?.value === 'draft-for-a';
    });
    await page.evaluate(() => globalThis.f01.switchIdentity()); await waitText('account-b'); await openProfile();
    await page.waitForFunction(() => document.querySelector('input[autocomplete=username]')?.value === 'account-b');
    assert(duplicateKeys.length === 0, `duplicate workspace React keys: ${duplicateKeys.length}`);
  });
  if (!process.env.F01_CASE) passed += await runSMSConflictCases(page, `http://127.0.0.1:${vite.httpServer.address().port}`);
  console.log(`RESULT ${passed} PASS 0 FAIL ${skipped} SKIP`);
 } catch (error) {
  console.error(`FAIL after ${passed} PASS: ${error.message}`);
  if (page) {
    const diagnostic = await page.evaluate(() => ({ text: document.body.textContent.slice(0, 1000), smsForm: [...document.querySelectorAll('.adm-sms-form textarea, .adm-sms-form button')].map(node => ({ tag: node.tagName, value: node.value, disabled: node.disabled })), nativeMethods: window.f01?.nativeCalls.map(call => call[0]), lastRoutes: window.f01?.calls.slice(-5).map(call => call.url) })).catch(() => ({}));
    console.error(JSON.stringify(diagnostic));
  }
  process.exitCode = 1;
}
finally {
  if (browser) await browser.close();
  if (vite) await vite.close();
  if (fs.existsSync(profile)) {
    const resolved=path.resolve(profile);
    if (path.dirname(resolved)===path.resolve(os.tmpdir()) && path.basename(resolved).startsWith('pinvou-f01-ui-')) {
      fs.rmSync(resolved, { recursive: true, force: true });
    } else {
      console.error('Owned browser profile cleanup boundary');
      process.exitCode=1;
    }
  }
}
