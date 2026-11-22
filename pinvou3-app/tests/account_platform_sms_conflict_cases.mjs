import assert from 'node:assert/strict';
import { navigateFixture } from './helpers/navigate-fixture.mjs';
import { smsStatusCopy, smsTemplateCopy } from '../src/shared/i18n.js';

export async function runSMSConflictCases(page, origin) {
 let passed = 0; const errors = [];
 const onError = error => errors.push(error.message); page.on('pageerror', onError);
 const fresh = async (language = 'en', provider = 'http-generated') => { errors.length = 0; await navigateFixture(page, `${origin}/tests/fixtures/account-platform-sms-conflict.html?language=${language}&provider=${provider}`, signal => page.waitForSelector('.adm-sms-form', { signal })); };
 const reason = async (text = 'controlled correction reason') => { await page.$eval('textarea', (element, text) => { Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(element, text); element.dispatchEvent(new Event('input', { bubbles: true })); }, text); };
 const save = () => page.click('.adm-sms-form button[type=submit]');
 const error = () => page.waitForSelector('.adm-sms-error');
 const refresh = async () => { await page.click('.adm-sms-error button'); await page.waitForSelector('.adm-sms-form'); };
 const unknown = async () => {
  const language = await page.$eval('select', select => select.value);
  await page.waitForFunction(text => document.querySelector('.adm-sms-unknown[role=status]')?.textContent.includes(text), {}, smsStatusCopy[language].unknownWrite);
 };
 const refreshUnknown = async () => { await page.click('.adm-sms-unknown button'); await page.waitForSelector('.adm-sms-form'); };
 const saved = async (replay = false) => {
  const { language, revision } = await page.evaluate(() => ({ language: document.querySelector('select').value, revision: window.smsConflict.writes.at(-1).command.expected_revision + 1 }));
  const text = smsTemplateCopy[language][replay ? 'replaySaved' : 'saved'].replace('{revision}', String(revision));
  await page.waitForFunction(text => document.querySelector('.adm-sms-save-feedback[role=status]')?.textContent.includes(text), {}, text);
 };
 const waitRevision = revision => page.waitForFunction(revision => [...document.querySelectorAll('.adm-sms-summary dt')].find(term => term.textContent.trim() === window.smsConflict.copy().settings.sms.revision)?.nextElementSibling?.textContent.trim() === String(revision), {}, revision);
 const waitWrites = count => page.waitForFunction(count => window.smsConflict.writes.length === count, {}, count);
 const writes = () => page.evaluate(() => window.smsConflict.writes);
 const run = async (name, action) => { await action(); assert.deepEqual(errors, []); console.log(`PASS SMS ${name}`); passed++; };
 try {
  for (const language of ['zh', 'en', 'ja']) await run(`${language}: exact CAS conflict refresh requires explicit save and binds new revision/key`, async () => {
   await fresh(language); await reason(); await page.evaluate(() => { window.smsConflict.revision = 1; }); await save(); await error(); await waitWrites(1);
   await save(); assert.equal((await writes()).length, 1, 'conflicted attempt dispatched before refresh');
   await refresh(); assert.equal((await writes()).length, 1, 'refresh implicitly dispatched a write');
   await reason(); await save(); await waitWrites(2); await saved();
   const [old, next] = await writes(); assert.equal(old.command.expected_revision, 0); assert.equal(next.command.expected_revision, 1); assert.notEqual(old.key, next.key);
   assert.deepEqual({ ...old.command, expected_revision: undefined }, { ...next.command, expected_revision: undefined });
  });
  for (const reply of [{ status: 503, code: 'SMS_CONFIGURATION_CONFLICT' }, { status: 500, code: 'UPSTREAM_UNAVAILABLE' }, 'network', 'abort', 'bad-ack']) await run(`${typeof reply === 'string' ? reply : reply.status}: unknown keeps original command/key despite refreshed facts`, async () => {
   await fresh(); await reason(); await page.evaluate(reply => { window.smsConflict.replies.push(reply); }, reply); await save(); await unknown();
   await page.evaluate(() => { window.smsConflict.revision = 4; window.smsConflict.enabled = true; }); await refreshUnknown(); await waitRevision(4);
   assert.equal((await writes()).length, 1, 'unknown refresh dispatched a write');
   await reason('different displayed correction reason');
   await page.select('select', 'ja'); await page.evaluate(() => { window.smsConflict.replies.push('replay'); }); await save(); await waitWrites(2);
   const [old, next] = await writes(); assert.deepEqual(next, old, 'unknown changed frozen facts/key');
   await saved(true);
  });
  for (const outcome of ['saved', 'lost-ack']) await run(`Aliyun ${outcome}: canonical fields complete and unlock the next save`, async () => {
   await fresh('en', 'aliyun-managed'); await reason();
   await page.evaluate(() => {
    const values = { 'input[maxlength="100"]': '\u0085Pinvou\ufeff\u3000' };
    for (const purpose of ['REGISTER', 'LOGIN', 'PROFILE_MOBILE', 'PASSWORD_RESET']) values[`input[aria-describedby="adm-sms-template-${purpose}"]`] = `\u0085 SMS_${purpose}\u3000 `;
    for (const [selector, value] of Object.entries(values)) {
     const input = document.querySelector(selector);
     Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, value);
     input.dispatchEvent(new Event('input', { bubbles: true }));
    }
   });
   if (outcome === 'lost-ack') await page.evaluate(() => window.smsConflict.replies.push('lost-ack'));
   await save(); await waitWrites(1);
   const first = (await writes())[0];
   assert.deepEqual({ sign_name: first.command.sign_name, templates: first.command.templates }, {
    sign_name: 'Pinvou\ufeff', templates: { REGISTER: 'SMS_REGISTER', LOGIN: 'SMS_LOGIN', PROFILE_MOBILE: 'SMS_PROFILE_MOBILE', PASSWORD_RESET: 'SMS_PASSWORD_RESET' },
   }, 'SMS command must use server canonical fields');
   if (outcome === 'lost-ack') {
    await unknown(); await refreshUnknown(); await waitRevision(1);
    assert.equal((await writes()).length, 1, 'canonical receipt refresh dispatched a write');
    await reason('different displayed correction reason'); await page.select('select', 'ja');
    await save(); await waitWrites(2); assert.deepEqual((await writes())[1], first, 'canonical unknown retry changed its command/key');
    await saved(true);
   } else await saved();
   await reason('next independent correction reason'); await save(); await waitWrites(outcome === 'lost-ack' ? 3 : 2);
   const next = (await writes()).at(-1);
   assert.equal(next.command.expected_revision, 1); assert.notEqual(next.key, first.key); await saved();
  });
  await run('a same-revision refresh cannot retire a definite conflict', async () => {
   await fresh(); await reason(); await page.evaluate(() => window.smsConflict.replies.push({ status: 409, code: 'SMS_CONFIGURATION_CONFLICT' })); await save(); await error(); await refresh(); await reason(); await save();
   assert.equal((await writes()).length, 1);
  });
  await run('a failed refresh keeps the conflict blocked until a successful newer read', async () => {
   await fresh(); await reason(); await page.evaluate(() => { window.smsConflict.revision = 1; }); await save(); await error();
   await page.evaluate(() => window.smsConflict.readReplies.push(503)); await page.click('.adm-sms-error button'); await page.waitForFunction(() => document.querySelector('.adm-sms-error')?.textContent.includes(window.smsConflict.copy().settings.sms.loadFailed));
   await save(); assert.equal((await writes()).length, 1); await refresh(); await reason(); await save(); await waitWrites(2); assert.equal((await writes())[1].command.expected_revision, 1);
  });
  await run('a different 409 code does not masquerade as a CAS conflict', async () => {
   await fresh(); await reason(); await page.evaluate(() => window.smsConflict.replies.push({ status: 409, code: 'IDEMPOTENCY_KEY_REUSED' })); await save(); await error();
   await page.evaluate(() => { window.smsConflict.revision = 2; }); await refresh(); await reason(); await page.evaluate(() => window.smsConflict.replies.push('replay')); await save(); await waitWrites(2);
   const [old, next] = await writes(); assert.deepEqual(next, old);
  });
  for (const retirement of ['session', 'version']) await run(`${retirement}: an old conflict response cannot retire a new intent`, async () => {
   await fresh(); await reason(); await page.evaluate(() => { window.smsConflict.holdWrite = true; window.smsConflict.replies.push({ status: 409, code: 'SMS_CONFIGURATION_CONFLICT' }); }); await save(); await waitWrites(1);
   await page.evaluate(retirement => { window.smsConflict.revision = 3; if (retirement === 'session') window.smsConflict.retire(); else window.smsConflict.versionChange(); }, retirement);
   await waitRevision(3);
   await page.evaluate(() => window.smsConflict.heldWrites.shift()()); await reason(); await save(); await waitWrites(2);
   const [old, next] = await writes(); assert.notEqual(old.key, next.key); assert.equal(next.command.expected_revision, 3);
   await saved();
  });
  for (const retirement of ['session', 'version']) await run(`${retirement}: late old read cannot restore an earlier revision`, async () => {
   await fresh(); await reason(); await page.evaluate(() => { window.smsConflict.replies.push({ status: 409, code: 'SMS_CONFIGURATION_CONFLICT' }); }); await save(); await error();
   await page.evaluate(() => { window.smsConflict.revision = 1; window.smsConflict.holdRead = true; }); await page.click('.adm-sms-error button'); await page.waitForFunction(() => window.smsConflict.heldReads.length === 1);
   await page.evaluate(retirement => { window.smsConflict.revision = 5; if (retirement === 'session') window.smsConflict.retire(); else window.smsConflict.versionChange(); }, retirement);
   await waitRevision(5); await page.evaluate(() => window.smsConflict.heldReads.shift()()); await reason(); await save(); await waitWrites(2);
   assert.equal((await writes())[1].command.expected_revision, 5);
  });
  await run('revoked manage permission retires an unknown intent before permission restoration', async () => {
   await fresh(); await reason(); await page.evaluate(() => window.smsConflict.replies.push('network')); await save(); await unknown();
   await page.evaluate(() => { window.smsConflict.permissions = ['platform.sms-config.read']; window.smsConflict.render(); }); await page.waitForFunction(() => !document.querySelector('textarea'));
   await page.evaluate(() => { window.smsConflict.revision = 6; window.smsConflict.permissions = ['platform.sms-config.read', 'platform.sms-config.manage']; window.smsConflict.render(); }); await page.waitForSelector('textarea'); await reason(); await save(); await waitWrites(2);
   const [old, next] = await writes(); assert.notEqual(old.key, next.key); assert.equal(next.command.expected_revision, 6);
  });
  return passed;
 } finally { page.off('pageerror', onError); }
}
