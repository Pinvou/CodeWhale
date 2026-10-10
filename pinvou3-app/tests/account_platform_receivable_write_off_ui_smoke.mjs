import assert from 'node:assert/strict';
import fs from 'node:fs';
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import * as puppeteer from 'puppeteer-core';
const root = import.meta.dirname + '/..';
const server = await createServer({ root, configFile: false, plugins: [react()], cacheDir: root + '/node_modules/.vite-write-off-host', server: { host: '127.0.0.1', port: 0, fs: { allow: [root, fs.realpathSync(root + '/node_modules')], deny: ['**/.env*', '**/*.{pem,crt,key}'] } } });
await server.listen();
const browser = await puppeteer.launch({ executablePath: process.env.EDGE_PATH || 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', headless: true, args: ['--disable-gpu'] });
const results = []; let pass = 0, fail = 0;
async function run(name, query, action) {
  const page = await browser.newPage(); page.setDefaultTimeout(5000); const errors = []; page.on('pageerror', error => errors.push(error.message));
  try { await page.goto(server.resolvedUrls.local[0] + 'tests/fixtures/receivable-write-off-host.html?' + query); await page.waitForFunction(() => window.f11 && document.querySelector('main')); await action(page); assert.deepEqual(errors, []); pass++; results.push({ name, pass: true }); }
  catch (error) { fail++; results.push({ name, pass: false, error: error.message, diagnostic: await page.evaluate(() => ({ text: document.body.innerText, calls: window.f11?.calls })).catch(() => null) }); }
  finally { await page.close(); }
}
async function openForm(page) {
  await page.evaluate(() => [...document.querySelectorAll('main > nav button')].find(button => button.textContent === window.f11.financeCopy[window.f11.language].finance).click());
  await page.waitForFunction(() => [...document.querySelectorAll('nav button')].some(button => button.textContent === window.f11.ledgerRepairCopy[window.f11.language].writeOffTitle));
  await page.evaluate(() => [...document.querySelectorAll('nav button')].find(button => button.textContent === window.f11.ledgerRepairCopy[window.f11.language].writeOffTitle).click());
  await page.waitForSelector('.adm-financial-repair-target input');
  await page.evaluate(() => {
    const values = [window.f11.id(1), '9007199254740993', '9223372036854775806'];
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
    [...document.querySelectorAll('.adm-financial-repair-target input')].forEach((input, i) => { setter.call(input, values[i]); input.dispatchEvent(new Event('input', { bubbles: true })); });
  });
  await page.click('.adm-financial-repair-target button');
}
async function create(page) {
  await page.waitForSelector('.adm-financial-repair-draft');
  await page.type('.adm-financial-repair-draft textarea', 'Controlled write-off justification'); await page.type('.adm-financial-repair-draft input', 'controlled-write-off');
  await page.click('.adm-financial-repair-draft button');
}
async function center(page, view) {
  await page.evaluate(() => [...document.querySelectorAll('main > nav button')].find(button => button.textContent === window.f11.adminPlatformCopy[window.f11.language].nav.approvals).click());
  if (view) await page.evaluate(view => [...document.querySelectorAll('.adm-approval-tabs button')].find(button => button.textContent === window.f11.adminPlatformCopy[window.f11.language].approvalCenter[view]).click(), view);
}
try {
  for (const [label, key, valid] of [['ASCII111', 'a'.repeat(111), true], ['ASCII112', 'a'.repeat(112), false], ['UTF8-111bytes', '中'.repeat(37), true], ['UTF8-114bytes', '中'.repeat(38), false]]) {
    await run(`WriteOff prefixed key byte boundary ${label}`, '', async page => {
      await openForm(page); await page.waitForSelector('.adm-financial-repair-draft');
      await page.type('.adm-financial-repair-draft textarea', 'Controlled key boundary justification');
      await page.evaluate(key => { const input = document.querySelector('.adm-financial-repair-draft input'); Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, key); input.dispatchEvent(new Event('input', { bubbles: true })); }, key);
      await page.click('.adm-financial-repair-draft button');
      if (valid) {
        await page.waitForFunction(() => window.f11.status === 'SUBMITTED');
        const draft = await page.evaluate(() => JSON.parse(window.f11.calls.find(call => call.url === '/api/admin/approvals/requests' && call.method === 'POST').body));
        assert.equal(draft.idempotency_key, `approval:${key}`); assert.equal(new TextEncoder().encode(draft.idempotency_key).length, 120);
      } else {
        await page.waitForFunction(() => window.f11.status === 'SUBMITTED' || document.querySelector('.adm-financial-repair [role=alert]'));
        assert.equal(await page.evaluate(() => window.f11.calls.some(call => call.url === '/api/admin/approvals/requests' && call.method === 'POST')), false);
        assert.equal(await page.$eval('.adm-financial-repair [role=alert]', node => node.textContent), await page.evaluate(() => window.f11.ledgerRepairCopy.zh.invalidDraft));
      }
    });
  }
  for (const language of ['zh', 'en', 'ja']) await run(`WriteOff real host/precheck/create/submit ${language}`, `language=${language}`, async page => {
    await openForm(page); await create(page); await page.waitForFunction(() => window.f11.status === 'SUBMITTED' && !document.querySelector('.adm-financial-repair'));
    const calls = await page.evaluate(() => window.f11.calls); const precheck = calls.find(call => call.url.endsWith('/approval-command-prechecks'));
    assert.ok(precheck.body.includes('"expected_version":9223372036854775806')); assert.ok(precheck.body.includes('"write_off_microcredits":9007199254740993'));
    const draft = calls.find(call => call.url === '/api/admin/approvals/requests' && call.method === 'POST'); assert.ok(draft.body.includes('"expected_receivable_version":9223372036854775806'));
    assert.equal(JSON.parse(draft.body).normalized_command.reason_code, 'APPROVED_RECEIVABLE_WRITE_OFF'); assert.equal(draft.headers['X-CSRF-Token'], 'controlled-csrf'); assert.equal(draft.cache, 'no-store');
    assert.ok(calls.some(call => call.url.endsWith('/actions/submit'))); assert.equal(calls.some(call => call.url.includes('/api/admin/billing/')), false);
  });
  await run('WriteOff directory/filter/typed EDIT/current proof/drilldown preserves original CAS', 'existing=1', async page => {
    await center(page, 'directory'); await page.waitForFunction(() => document.querySelector('tbody')?.textContent.includes('RECEIVABLE_WRITE_OFF'));
    assert.equal(await page.evaluate(() => document.querySelector('tbody').textContent.includes('UNKNOWN_REPAIR')), false);
    await page.evaluate(() => [...document.querySelectorAll('.adm-approval-tabs button')].find(button => button.textContent === window.f11.adminPlatformCopy.zh.approvalCenter.mine).click());
    await page.waitForSelector('.adm-approval-toolbar select'); await page.select('.adm-approval-toolbar select', 'RECEIVABLE_WRITE_OFF');
    await page.type('.adm-approval-toolbar input', await page.evaluate(() => window.f11.id(1))); await page.click('.adm-approval-toolbar button');
    await page.waitForFunction(() => window.f11.calls.some(call => call.url.includes('command_type=RECEIVABLE_WRITE_OFF') && call.url.includes('target=')) && document.querySelector('tbody tr button') && !document.querySelector('tbody tr button').disabled);
    await page.evaluate(() => document.querySelector('tbody tr button').click()); await page.waitForSelector('article textarea');
    await page.type('article textarea', 'Updated controlled write-off evidence');
    await page.evaluate(() => [...document.querySelectorAll('article button')].find(button => button.textContent === window.f11.financeCopy.zh.editSubmit).click());
    await page.waitForFunction(() => window.f11.status === 'SUBMITTED');
    const calls = await page.evaluate(() => window.f11.calls); const update = calls.find(call => call.method === 'PUT');
    assert.equal(JSON.parse(update.body).expected_version, 1); assert.ok(update.body.includes('"expected_receivable_version":9223372036854775806')); assert.ok(update.body.includes('"write_off_microcredits":9007199254740993'));
    assert.ok(calls.some(call => call.url.includes('command_type=RECEIVABLE_WRITE_OFF') && call.url.includes('target=')));
    await page.waitForFunction(() => [...document.querySelectorAll('article button')].some(button => button.textContent === window.f11.adminPlatformCopy.zh.approvalCenter.openBusinessArea && !button.disabled));
    await page.evaluate(() => [...document.querySelectorAll('article button')].find(button => button.textContent === window.f11.adminPlatformCopy.zh.approvalCenter.openBusinessArea).click());
    await page.waitForSelector('.adm-financial-repair-target select'); assert.equal(await page.$eval('.adm-financial-repair-target select', select => select.value), 'RECEIVABLE_WRITE_OFF');
  });
  await run('WriteOff approver-only real detail decide without requester or wallet calls', 'approve=1&existing=1', async page => {
    await page.waitForSelector('tbody tr button'); assert.equal(await page.$('.adm-financial-repair'), null);
    await page.evaluate(() => { window.f11.status = 'SUBMITTED'; }); await page.click('tbody tr button'); await page.waitForSelector('article textarea');
    await page.type('article textarea', 'Independent approved write-off review');
    await page.evaluate(() => [...document.querySelectorAll('article button')].find(button => button.textContent === window.f11.financeCopy.zh.approve).click());
    await page.waitForFunction(() => document.querySelector('article')?.textContent.includes('EXECUTED'));
    assert.equal(await page.evaluate(() => window.f11.calls.some(call => call.url.includes('/billing/') || call.url.endsWith('/approval-command-prechecks') || call.method === 'PUT')), false);
  });
  await run('No repair permission hides WriteOff and does not call its receiver', 'deny=1', async page => {
    await page.waitForFunction(() => document.body.textContent.includes(window.f11.adminPlatformCopy.zh.permissionDenied)); assert.equal(await page.$('.adm-financial-repair'), null);
    assert.equal(await page.evaluate(() => window.f11.calls.some(call => call.url.startsWith('/api/admin/'))), false);
  });
  for (const condition of ['unavailable', 'mismatch']) await run(`WriteOff ${condition} owner facts reject before create`, `${condition}=1`, async page => {
    await openForm(page); await page.waitForSelector('.adm-financial-repair [role=alert]'); assert.equal(await page.$('.adm-financial-repair-draft'), null);
    assert.equal(await page.evaluate(() => window.f11.calls.some(call => call.url === '/api/admin/approvals/requests' && call.method === 'POST')), false);
  });
  await run('WriteOff committed unknown + denied replay retains original facts/key then recovers', 'lost=1', async page => {
    await openForm(page); await create(page); await page.waitForFunction(() => document.querySelector('.adm-financial-repair-draft button')?.textContent === window.f11.financeCopy.zh.retry);
    await page.click('.adm-financial-repair-draft button'); await page.waitForFunction(() => window.f11.calls.some(call => call.method === 'PUT') && !document.querySelector('.adm-financial-repair-draft button').disabled);
    assert.equal(await page.$eval('.adm-financial-repair-draft input', input => input.disabled), true); assert.equal(await page.$eval('.adm-financial-repair-draft textarea', input => input.disabled), true);
    await page.evaluate(() => { window.f11.denyReplay = false; }); await page.click('.adm-financial-repair-draft button'); await page.waitForFunction(() => window.f11.status === 'SUBMITTED');
    const calls = await page.evaluate(() => window.f11.calls.filter(call => call.method === 'PUT' || call.url === '/api/admin/approvals/requests' && call.method === 'POST'));
    assert.equal(calls.length, 3); const inputs = calls.map(call => { const { expected_version, ...intent } = JSON.parse(call.body); return { expected_version, intent }; });
    assert.equal(new Set(inputs.map(input => JSON.stringify(input.intent))).size, 1); assert.deepEqual(inputs.slice(1).map(input => input.expected_version), [1, 1]); assert.equal(await page.evaluate(() => window.f11.prechecks), 1);
  });
  await run('WriteOff initial definite rejection unlocks original input for correction', 'initial-deny=1', async page => {
    await openForm(page); await create(page); await page.waitForFunction(() => document.querySelector('.adm-financial-repair [role=alert]') && !document.querySelector('.adm-financial-repair-draft button').disabled);
    assert.equal(await page.$eval('.adm-financial-repair-draft input', input => input.disabled), false); assert.equal(await page.evaluate(() => window.f11.created), false);
  });
  await run('Directory retirement releases only old child; current EDIT UNKNOWN keeps navigation locked', 'existing=1&edit-lost=1', async page => {
    await center(page, 'directory'); await page.waitForFunction(() => document.querySelector('tbody')?.textContent.includes('RECEIVABLE_WRITE_OFF'));
    await page.evaluate(() => [...document.querySelectorAll('.adm-approval-tabs button')].find(button => button.textContent === window.f11.adminPlatformCopy.zh.approvalCenter.mine).click());
    await page.waitForSelector('tbody tr button'); await page.click('tbody tr button'); await page.waitForSelector('article textarea');
    await page.type('article textarea', 'Current original-intent controlled EDIT');
    await page.evaluate(() => [...document.querySelectorAll('article button')].find(button => button.textContent === window.f11.financeCopy.zh.editSubmit).click());
    await page.waitForFunction(() => document.body.textContent.includes(window.f11.financeCopy.zh.unknown) && window.f11.calls.some(call => call.method === 'PUT'));
    assert.ok(await page.evaluate(() => [...document.querySelectorAll('.adm-approval-tabs button')].every(button => button.disabled)));
    await page.evaluate(() => window.f11.replaceIdentity());
    await page.waitForFunction(() => window.f11.calls.filter(call => call.url === '/api/account/v1/account/bootstrap').length === 2 && document.querySelector('.adm-approval-tabs'));
    await page.waitForFunction(() => [...document.querySelectorAll('.adm-approval-tabs button')].every(button => !button.disabled));
    assert.equal(await page.evaluate(() => document.body.textContent.includes(window.f11.financeCopy.zh.unknown)), false);
  });
  await run('Retired identity ignores late WriteOff precheck', 'late=1', async page => {
    await openForm(page); await page.waitForFunction(() => window.f11.releasePrecheck); await page.evaluate(() => { const release = window.f11.releasePrecheck; window.f11.replaceIdentity(); setTimeout(release, 50); });
    await page.waitForFunction(() => window.f11.calls.filter(call => call.url === '/api/account/v1/account/bootstrap').length === 2);
    assert.equal(await page.$('.adm-financial-repair-draft'), null);
  });
} finally { await browser.close(); await server.close(); }
const result = { pass, fail, results, limits: ['Real AdminPlatformView/AdminFinanceWorkspace/AdminPlatformClient under controlled BFF replies; owner effects, independent issuer/NATS, PG, deployed API and full E2E are not exercised.'] };
fs.writeFileSync(process.env.F11_REACT_RESULT || 'receivable-smoke-result.json', JSON.stringify(result, null, 2)); console.log(JSON.stringify(result)); process.exitCode = fail ? 1 : 0;
