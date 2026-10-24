import assert from 'node:assert/strict';

export async function runSMSConflictCases(page, origin) {
 let passed = 0; const errors = [];
 const onError = error => errors.push(error.message); page.on('pageerror', onError);
 const fresh = async (language = 'en') => { errors.length = 0; await page.goto(`${origin}/tests/fixtures/account-platform-sms-conflict.html?language=${language}`); await page.waitForSelector('.adm-sms-form'); };
 const reason = async (text = 'controlled correction reason') => { await page.$eval('textarea', (element, text) => { Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(element, text); element.dispatchEvent(new Event('input', { bubbles: true })); }, text); };
 const save = () => page.click('.adm-sms-form button[type=submit]');
 const error = () => page.waitForSelector('.adm-sms-error');
 const refresh = async () => { await page.click('.adm-sms-error button'); await page.waitForSelector('.adm-sms-form'); };
 const waitWrites = count => page.waitForFunction(count => window.smsConflict.writes.length === count, {}, count);
 const writes = () => page.evaluate(() => window.smsConflict.writes);
 const run = async (name, action) => { await action(); assert.deepEqual(errors, []); console.log(`PASS SMS ${name}`); passed++; };
 try {
  for (const language of ['zh', 'en', 'ja']) await run(`${language}: exact CAS conflict refresh requires explicit save and binds new revision/key`, async () => {
   await fresh(language); await reason(); await page.evaluate(() => { window.smsConflict.revision = 1; }); await save(); await error(); await waitWrites(1);
   await save(); assert.equal((await writes()).length, 1, 'conflicted attempt dispatched before refresh');
   await refresh(); assert.equal((await writes()).length, 1, 'refresh implicitly dispatched a write');
   await reason(); await save(); await waitWrites(2); await page.waitForFunction(() => document.body.textContent.includes(window.smsConflict.copy().settings.sms.saved));
   const [old, next] = await writes(); assert.equal(old.command.expected_revision, 0); assert.equal(next.command.expected_revision, 1); assert.notEqual(old.key, next.key);
   assert.deepEqual({ ...old.command, expected_revision: undefined }, { ...next.command, expected_revision: undefined });
  });
  for (const reply of [{ status: 503, code: 'SMS_CONFIGURATION_CONFLICT' }, { status: 500, code: 'UPSTREAM_UNAVAILABLE' }, 'network', 'abort', 'bad-ack']) await run(`${typeof reply === 'string' ? reply : reply.status}: unknown keeps original command/key despite refreshed facts`, async () => {
   await fresh(); await reason(); await page.evaluate(reply => { window.smsConflict.replies.push(reply); }, reply); await save(); await error();
   await page.evaluate(() => { window.smsConflict.revision = 4; window.smsConflict.enabled = true; }); await refresh(); await reason('different displayed correction reason');
   await page.select('select', 'ja'); await page.evaluate(() => { window.smsConflict.replies.push('replay'); }); await save(); await waitWrites(2);
   const [old, next] = await writes(); assert.deepEqual(next, old, 'unknown changed frozen facts/key');
   await page.waitForFunction(() => document.body.textContent.includes(window.smsConflict.copy().settings.sms.saved));
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
   await page.waitForFunction(() => document.querySelector('.adm-sms-summary strong')?.textContent === '3');
   await page.evaluate(() => window.smsConflict.heldWrites.shift()()); await reason(); await save(); await waitWrites(2);
   const [old, next] = await writes(); assert.notEqual(old.key, next.key); assert.equal(next.command.expected_revision, 3);
   await page.waitForFunction(() => document.body.textContent.includes(window.smsConflict.copy().settings.sms.saved));
  });
  for (const retirement of ['session', 'version']) await run(`${retirement}: late old read cannot restore an earlier revision`, async () => {
   await fresh(); await reason(); await page.evaluate(() => { window.smsConflict.replies.push({ status: 409, code: 'SMS_CONFIGURATION_CONFLICT' }); }); await save(); await error();
   await page.evaluate(() => { window.smsConflict.revision = 1; window.smsConflict.holdRead = true; }); await page.click('.adm-sms-error button'); await page.waitForFunction(() => window.smsConflict.heldReads.length === 1);
   await page.evaluate(retirement => { window.smsConflict.revision = 5; if (retirement === 'session') window.smsConflict.retire(); else window.smsConflict.versionChange(); }, retirement);
   await page.waitForFunction(() => document.querySelector('.adm-sms-summary strong')?.textContent === '5'); await page.evaluate(() => window.smsConflict.heldReads.shift()()); await reason(); await save(); await waitWrites(2);
   assert.equal((await writes())[1].command.expected_revision, 5);
  });
  await run('revoked manage permission retires an unknown intent before permission restoration', async () => {
   await fresh(); await reason(); await page.evaluate(() => window.smsConflict.replies.push('network')); await save(); await error();
   await page.evaluate(() => { window.smsConflict.permissions = ['platform.sms-config.read']; window.smsConflict.render(); }); await page.waitForFunction(() => !document.querySelector('textarea'));
   await page.evaluate(() => { window.smsConflict.revision = 6; window.smsConflict.permissions = ['platform.sms-config.read', 'platform.sms-config.manage']; window.smsConflict.render(); }); await page.waitForSelector('textarea'); await reason(); await save(); await waitWrites(2);
   const [old, next] = await writes(); assert.notEqual(old.key, next.key); assert.equal(next.command.expected_revision, 6);
  });
  return passed;
 } finally { page.off('pageerror', onError); }
}
