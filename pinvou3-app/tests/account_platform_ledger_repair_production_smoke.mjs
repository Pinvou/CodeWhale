import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import puppeteer from 'puppeteer-core';
import { ledgerRepairCopy } from '../src/shared/ledger-repair-i18n.js';
import { financeCopy } from '../src/shared/finance-i18n.js';
import { encodeFinanceJSON } from '../src/features/account-platform/finance-contract.js';

const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const browserPath = [process.env.CHROME, 'C:/Program Files/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe'].find(value => value && existsSync(value));
assert.ok(browserPath, 'Chromium is required');
const id = n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`;
const actorA = id(101), actorB = id(102), draftID = id(9);
const projection = { command_type: 'LEDGER_REPAIR', policy_version: 1, snapshot_schema_version: 1,
  target_resource_type: 'LEDGER_POSTING', target_resource_id: id(1),
  normalized_command: { target_posting_id: id(1), target_journal_entry_id: id(2), wrong_ledger_account_id: id(3), correct_ledger_account_id: id(4), amount_microcredits: '9223372036854775807', reason_code: 'ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT' },
  critical_resource_versions: { policy_version: 1, posting_set_digest: 'a'.repeat(64) },
  financial_snapshot: { amount_microcredits: '9223372036854775807', wallet_id: null } };
let actor, csrf, mode, writes, effects, submits, created, waiting, release;
function reset(next = 'normal') { actor = actorA; csrf = 'csrf-initial'; mode = next; writes = []; effects = 0; submits = 0; created = false; waiting = false; release = null; }
const json = (response, body, status = 200) => { response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); response.end(encodeFinanceJSON(body)); };
const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/auth/login') { csrf = 'csrf-after-reauth'; response.writeHead(302, { Location: '/admin.html?section=finance&finance_tab=ledger' }); return response.end(); }
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: csrf });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: actor, status: 'ACTIVE', personal_permissions: [], platform_permissions: ['platform.billing.repairs.request'], platform_authorization_version: 1 });
  if (url.pathname === '/api/admin/approvals/capability') return json(response, { session_approval_wire_version: 2, control: { version: 1, epoch: 1, authorization_relay_epoch: 1 } });
  if (url.pathname === '/api/admin/account/admin/v2/approval-command-prechecks') return json(response, projection);
  if (url.pathname === '/api/admin/approvals/requests' && request.method === 'GET') return json(response, { items: [], has_more: false });
  if (url.pathname === '/api/admin/approvals/requests' && request.method === 'POST') {
    const chunks = []; for await (const chunk of request) chunks.push(chunk);
    const raw = Buffer.concat(chunks).toString();
    assert.equal(request.headers['x-csrf-token'], csrf);
    assert.match(raw, /"amount_microcredits":9223372036854775807[,}]/);
    assert.equal(JSON.parse(raw).idempotency_key.length, 120);
    writes.push({ raw, actor, csrf });
    if (mode === 'denied') return json(response, { code: 'PERMISSION_DENIED' }, 403);
    if (!created) { effects++; created = true; }
    if (mode === 'unknown' && writes.length <= 4) return json(response, { code: ['COMMAND_RESULT_UNKNOWN', 'RECENT_AUTHENTICATION_REQUIRED', 'PERMISSION_DENIED', 'APPROVAL_FACTS_CHANGED'][writes.length - 1] }, [503, 401, 403, 409][writes.length - 1]);
    if (mode === 'late') { waiting = true; await new Promise(resolve => { release = resolve; }); }
    return json(response, { approval_request_id: draftID }, 201);
  }
  if (url.pathname.includes('/actions/submit')) { submits++; return json(response, {}, 500); }
  if (url.pathname === `/api/admin/approvals/requests/${draftID}`) return json(response, { approval_request_id: draftID, command_type: 'LEDGER_REPAIR', status: 'DRAFT', version: 1, action_control_version: 1, detail_availability: 'AVAILABLE', allowed_actions: ['SUBMIT'], target: { resource_type: 'LEDGER_POSTING', resource_id: id(1) }, command: projection.normalized_command, reason: 'Controlled original Ledger correction reason' });
  if (url.pathname.startsWith('/api/')) return json(response, { items: [], has_more: false });
  const target = path.resolve(staticRoot, `.${url.pathname === '/' ? '/admin.html' : url.pathname}`);
  if (!target.startsWith(staticRoot + path.sep)) { response.writeHead(404); return response.end(); }
  try { const body = await readFile(target); response.writeHead(200, { 'Content-Type': target.endsWith('.js') ? 'text/javascript' : target.endsWith('.css') ? 'text/css' : 'text/html' }); response.end(body); }
  catch { response.writeHead(404); response.end(); }
});
await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
const origin = `http://127.0.0.1:${server.address().port}`;
const browser = await puppeteer.launch({ executablePath: browserPath, headless: true, args: ['--disable-gpu'] });
let passed = 0;
async function open(page, language = 'en') {
  await page.evaluateOnNewDocument(locale => { localStorage.setItem('pinvou.admin.language', locale); }, language);
  await page.goto(`${origin}/admin.html?section=finance&finance_tab=ledger&language=${language}`);
  await enterLedger(page, language);
}
async function enterLedger(page, language = 'en') {
  await page.waitForFunction(text => [...document.querySelectorAll('nav button')].some(button => button.textContent === text), {}, financeCopy[language].finance);
  await page.evaluate(text => [...document.querySelectorAll('nav button')].find(button => button.textContent === text).click(), financeCopy[language].finance);
  await page.waitForSelector('.adm-financial-repair-target');
}
async function prepare(page) {
  const inputs = await page.$$('.adm-financial-repair-target input');
  for (const [index, value] of [id(1), id(2), id(3), id(4), '9223372036854775807'].entries()) await inputs[index].type(value);
  await page.click('.adm-financial-repair-target button'); await page.waitForSelector('.adm-financial-repair-draft');
  await page.type('.adm-financial-repair-draft textarea', 'Controlled original Ledger correction reason');
  await page.type('.adm-financial-repair-draft input', 'k'.repeat(111));
}
async function retry(page) { await page.waitForFunction(() => document.querySelector('.adm-financial-repair-draft button')?.disabled === false); await page.click('.adm-financial-repair-draft button'); }
async function run(name, action) {
  const context = await browser.createBrowserContext(); const page = await context.newPage(); page.setDefaultTimeout(5000);
  const errors = []; page.on('pageerror', error => errors.push(error.message));
  try { await action(page); assert.deepEqual(errors, []); passed++; console.log(`PASS ${name}`); }
  catch (error) { console.error(JSON.stringify({ name, errors, body: await page.evaluate(() => document.body.innerText).catch(() => '') })); throw error; }
  finally { await context.close(); }
}
try {
  await run('same-key different-facts ABA cannot be cleared by a late prior ACK', async page => {
    reset('late'); await open(page); await prepare(page); await retry(page);
    while (!waiting) await new Promise(resolve => setTimeout(resolve, 10));
    await page.evaluate(() => {
      const slot = 'pinvou.ledger-repair.intent.v1:00000000-0000-4000-8000-000000000101';
      const originalGet = document.defaultView.Storage.prototype.getItem;
      const originalSet = document.defaultView.Storage.prototype.setItem;
      let armed = true;
      document.defaultView.Storage.prototype.getItem = function(key) {
        const raw = originalGet.call(this, key);
        if (key === slot && raw && armed) {
          armed = false;
          // Model a newer invocation established after A's last read, before
          // its asynchronous completion resumes the hook's receipt clearing.
          const newer = JSON.parse(raw); newer.facts.reason = 'New immutable facts under the same operator key';
          queueMicrotask(() => originalSet.call(this, key, JSON.stringify(newer)));
        }
        return raw;
      };
    });
    release();
    await page.waitForFunction(() => document.querySelector('article') || document.querySelector('.adm-financial-repair [role=alert]'));
    assert.equal(await page.evaluate(() => JSON.parse(document.defaultView.sessionStorage.getItem('pinvou.ledger-repair.intent.v1:00000000-0000-4000-8000-000000000101'))?.facts.reason), 'New immutable facts under the same operator key');
    assert.equal(await page.$('article'), null); assert.equal(submits, 0);
  });
  for (const language of ['zh', 'en', 'ja']) await run(`compiled Ledger creates DRAFT only ${language}`, async page => {
    reset(); await open(page, language); await prepare(page); await retry(page);
    await page.waitForFunction(value => document.body.textContent.includes(value) && !document.querySelector('.adm-financial-repair'), {}, draftID);
    assert.equal(effects, 1); assert.equal(submits, 0); assert.equal(writes.length, 1);
    assert.equal(JSON.parse(writes[0].raw).idempotency_key, `approval:${'k'.repeat(111)}`);
    // The user's existing approval viewing entry remains available after create.
    assert.ok(await page.evaluate(value => document.querySelector('article')?.textContent.includes(value), draftID));
    assert.ok(ledgerRepairCopy[language].recoveryPending && financeCopy[language].submit);
  });
  await run('unknown 401/403/409 survives real reauthentication and full reload with original body/key', async page => {
    reset('unknown'); await open(page); await prepare(page); await retry(page);
    await page.waitForSelector('.adm-financial-repair [role=alert]');
    await retry(page);
    await page.waitForFunction(text => [...document.querySelectorAll('.adm-financial-repair button')].some(button => button.textContent === text), {}, ledgerRepairCopy.en.reauthenticate);
    await Promise.all([page.waitForNavigation(), page.evaluate(text => [...document.querySelectorAll('.adm-financial-repair button')].find(button => button.textContent === text).click(), ledgerRepairCopy.en.reauthenticate)]);
    await enterLedger(page);
    await page.waitForSelector('.adm-financial-repair-draft');
    assert.equal(await page.$eval('.adm-financial-repair-draft input', element => element.value), 'k'.repeat(111));
    for (let attempt = 3; attempt <= 4; attempt++) { await retry(page); await page.waitForFunction(() => !document.querySelector('.adm-financial-repair-draft button').disabled); assert.equal(writes.length, attempt); await page.reload(); await enterLedger(page); await page.waitForSelector('.adm-financial-repair-draft'); }
    await retry(page); await page.waitForFunction(value => document.body.textContent.includes(value) && !document.querySelector('.adm-financial-repair'), {}, draftID);
    assert.equal(effects, 1); assert.equal(submits, 0); assert.equal(writes.length, 5);
    for (const item of writes) { assert.equal(item.raw, writes[0].raw); assert.equal(item.actor, actorA); }
    assert.equal(writes[2].csrf, 'csrf-after-reauth');
  });
  await run('foreign actor cannot replay unknown intent', async page => {
    reset('unknown'); await open(page); await prepare(page); await retry(page); await page.waitForSelector('.adm-financial-repair [role=alert]');
    actor = actorB; await page.reload(); await enterLedger(page);
    assert.equal(await page.$('.adm-financial-repair-draft'), null); assert.equal(writes.length, 1);
  });
  await run('unwritable storage blocks the first POST', async page => {
    reset(); await open(page); await prepare(page);
    await page.evaluate(() => { document.defaultView.Storage.prototype.setItem = () => { throw new Error('controlled unwritable storage'); }; });
    await retry(page); await page.waitForSelector('.adm-financial-repair [role=alert]'); assert.equal(writes.length, 0);
  });
  await run('storage readback mismatch blocks the first POST', async page => {
    reset(); await open(page); await prepare(page);
    await page.evaluate(() => { document.defaultView.Storage.prototype.setItem = () => {}; });
    await retry(page); await page.waitForSelector('.adm-financial-repair [role=alert]'); assert.equal(writes.length, 0);
  });
  await run('first definite denial clears its intent and leaves editable facts', async page => {
    reset('denied'); await open(page); await prepare(page); await retry(page);
    await page.waitForSelector('.adm-financial-repair [role=alert]');
    await page.waitForFunction(() => !document.querySelector('.adm-financial-repair-draft input').disabled);
    assert.equal(effects, 0); assert.equal(writes.length, 1);
    assert.equal(await page.evaluate(() => document.defaultView.sessionStorage.getItem('pinvou.ledger-repair.intent.v1:00000000-0000-4000-8000-000000000101')), null);
  });
  await run('malformed stored invocation is fail closed before any POST', async page => {
    reset(); await open(page);
    await page.evaluate(() => document.defaultView.sessionStorage.setItem('pinvou.ledger-repair.intent.v1:00000000-0000-4000-8000-000000000101', '{broken'));
    await page.reload(); await enterLedger(page); await page.waitForSelector('.adm-financial-repair [role=alert]');
    assert.equal(await page.$eval('.adm-financial-repair-target button', element => element.disabled), true); assert.equal(writes.length, 0);
  });
  await run('late ACK cannot clear a replacement durable invocation or open its old draft', async page => {
    reset('late'); await open(page); await prepare(page); await retry(page);
    while (!waiting) await new Promise(resolve => setTimeout(resolve, 10));
    await page.evaluate(() => {
      const slot = 'pinvou.ledger-repair.intent.v1:00000000-0000-4000-8000-000000000101';
      const newer = JSON.parse(document.defaultView.sessionStorage.getItem(slot)); newer.key = 'newer-ledger-key'; newer.facts.requestKey = newer.key;
      document.defaultView.sessionStorage.setItem(slot, JSON.stringify(newer));
    });
    release(); await page.waitForSelector('.adm-financial-repair [role=alert]');
    assert.equal(await page.evaluate(() => JSON.parse(document.defaultView.sessionStorage.getItem('pinvou.ledger-repair.intent.v1:00000000-0000-4000-8000-000000000101')).key), 'newer-ledger-key');
    assert.equal(await page.$('article'), null); assert.equal(submits, 0);
  });
  await run('late old-actor ACK cannot navigate or overwrite the new actor form', async page => {
    reset('late'); await open(page); await prepare(page); await retry(page);
    while (!waiting) await new Promise(resolve => setTimeout(resolve, 10));
    actor = actorB; await page.reload(); await enterLedger(page); release();
    await page.waitForFunction(() => document.querySelector('.adm-financial-repair-target'));
    assert.equal(await page.$('article'), null); assert.equal(writes.length, 1); assert.equal(submits, 0);
  });
} finally { release?.(); await browser.close(); await new Promise(resolve => server.close(resolve)); }
console.log(JSON.stringify({ passed, failed: 0, skipped: 0, boundary: 'Built production Portal assets with controlled HTTP; no production financial incident or live backend E2E.' }));
