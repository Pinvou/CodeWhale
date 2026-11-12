import assert from 'node:assert/strict';
import fs from 'node:fs';
import { adminLayoutCopy } from '../src/shared/account-layout-i18n.js';
import { createServer } from './helpers/vite-fixture.mjs';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { parseFinanceJSON } from '../src/features/account-platform/finance-contract.js';
const root = import.meta.dirname + '/..';
const browserPath = [process.env.CHROME, process.env.EDGE_PATH, process.env.CHROMIUM,
  'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', 'C:/Program Files/Google/Chrome/Application/chrome.exe',
  '/usr/bin/google-chrome', '/usr/bin/chromium', '/usr/bin/chromium-browser',
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].filter(Boolean).find(value => fs.existsSync(value));
assert.ok(browserPath, 'A browser executable is required; set CHROME or EDGE_PATH');
const server = await createServer({ root, configFile: false, plugins: [react()], cacheDir: root + '/../.cache/vite-f16-usage-settlement-host', server: { host: '127.0.0.1', port: 0, fs: { allow: [root, fs.realpathSync(root + '/node_modules')], deny: ['**/.env*', '**/*.{pem,crt,key}'] } } });
await server.listen();
const browser = await puppeteer.launch({ executablePath: browserPath, headless: true, args: ['--disable-gpu'] });
const results = []; let pass = 0, fail = 0;
async function run(name, query, action) {
  const page = await browser.newPage(); page.setDefaultTimeout(5000); page.setDefaultNavigationTimeout(15000); const errors = []; page.on('pageerror', error => errors.push(error.message));
  try { await page.goto(server.resolvedUrls.local[0] + 'tests/fixtures/usage-settlement-financial-repair-host.html?' + query); await page.waitForFunction(() => window.f16 && document.querySelector('main')); await action(page); assert.deepEqual(errors, []); pass++; results.push({ name, pass: true }); }
  catch (error) { fail++; results.push({ name, pass: false, error: error.message, diagnostic: await page.evaluate(() => ({ text: document.body.innerText, calls: window.f16?.calls })).catch(() => null) }); }
  finally { await page.close(); }
}
async function openForm(page) {
  await page.waitForSelector('nav button[data-section="billing"]');
  await page.click('nav button[data-section="billing"]');
  await page.waitForFunction(() => [...document.querySelectorAll('nav button')].some(button => button.textContent === window.f16.ledgerRepairCopy[window.f16.language].usageSettlementRepairTitle));
  await page.evaluate(() => [...document.querySelectorAll('nav button')].find(button => button.textContent === window.f16.ledgerRepairCopy[window.f16.language].usageSettlementRepairTitle).click());
  await page.waitForSelector('.adm-financial-repair-target input');
  await page.evaluate(() => {
    const values = [window.f16.id(1), window.f16.id(2), window.f16.id(3)];
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
    [...document.querySelectorAll('.adm-financial-repair-target input')].forEach((input, i) => { setter.call(input, values[i]); input.dispatchEvent(new Event('input', { bubbles: true })); });
  });
  await page.click('.adm-financial-repair-target button');
}
async function readDisplayFacts(page) {
  await page.waitForSelector('.adm-financial-repair-target input');
  await page.evaluate(() => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set;
    [...document.querySelectorAll('.adm-financial-repair-target input')].forEach((input, i) => {
      setter.call(input, window.f16.id(i + 1)); input.dispatchEvent(new Event('input', { bubbles: true }));
    });
  });
  await page.click('.adm-financial-repair-target button');
}
async function create(page) {
  await page.waitForSelector('.adm-financial-repair-draft');
  await page.type('.adm-financial-repair-draft textarea', 'Controlled usage-settlement justification'); await page.type('.adm-financial-repair-draft input', 'controlled-usage-settlement');
  await page.click('.adm-financial-repair-draft button');
}
async function clickReauthentication(page) {
  const selector = '.adm-usage-settlement-repair-reauthentication button';
  await page.$eval(selector, button => button.scrollIntoView({ block: 'center', inline: 'nearest' }));
  await page.waitForFunction(selector => { const button = document.querySelector(selector), rect = button.getBoundingClientRect(), hit = document.elementFromPoint(rect.x + rect.width / 2, rect.y + rect.height / 2); return hit && button.contains(hit) && !button.disabled; }, {}, selector);
  await page.click(selector);
}
async function center(page, view) {
  await page.waitForSelector('nav button[data-section="approvals"]');
  await page.click('nav button[data-section="approvals"]');
  if (view) await page.evaluate(view => [...document.querySelectorAll('.adm-approval-tabs button')].find(button => button.textContent === window.f16.adminPlatformCopy[window.f16.language].approvalCenter[view]).click(), view);
}
try {
  for (const language of ['zh', 'en', 'ja']) await run(`UsageSettlementRepair exact plan and identity DOM ${language}`, `language=${language}&display=1`, async page => {
    await readDisplayFacts(page); await page.waitForSelector('.adm-financial-repair-facts');
    const rendered = await page.evaluate(() => {
      const words = window.f16.ledgerRepairCopy[window.f16.language];
      const plan = document.querySelector('.adm-financial-repair-facts section');
      return { labels: [...plan.querySelectorAll('dt')].map(n => n.textContent), values: [...plan.querySelectorAll('dd')].map(n => n.textContent),
        identity: [...document.querySelectorAll('.adm-financial-repair-facts>dl dd code')].map(n => n.textContent),
        critical: document.querySelector('.adm-financial-repair-facts details pre').textContent,
        expectedLabels: [words.usageSettlementPlan.expected, words.usageSettlementPlan.settle, words.usageSettlementPlan.overage], points: words.usageSettlementPlan.points };
    });
    assert.deepEqual(rendered.labels, rendered.expectedLabels);
    assert.deepEqual(rendered.values, ['922,337,203,685,477.5807', '900,719,925,474.0993', '921,436,483,760,003.4814'].map(n => n + ' ' + rendered.points));
    assert.deepEqual(rendered.identity, [1, 2, 3, 4].map(n => `00000000-0000-4000-8000-${String(n).padStart(12, '0')}`));
    assert.ok(rendered.critical.includes('2026-10-07T00:00:00.123456789Z') && rendered.critical.includes('a'.repeat(64)));
    assert.equal(await page.$('.adm-financial-repair-target select'), null);
    assert.equal(await page.evaluate(() => window.f16.calls.some(c => c.method !== 'GET' && !c.url.endsWith('/approval-command-prechecks'))), false);
  });
  for (const axis of ['client', 'owner', 'epoch']) for (const late of [false, true]) await run(`UsageSettlementRepair ${late ? 'late' : 'accepted'} display retires on ${axis}`, 'display=1' + (late ? '&late=1' : ''), async page => {
    await readDisplayFacts(page);
    await page.waitForFunction(late => late ? typeof window.f16.releasePrecheck === 'function' : Boolean(document.querySelector('.adm-financial-repair-facts')), {}, late);
    await page.evaluate(axis => window.f16.changeDisplayScope(axis), axis);
    await page.waitForSelector('main[data-display-revision="1"]');
    if (late) {
      await page.evaluate(() => window.f16.releasePrecheck());
      await page.waitForFunction(() => window.f16.precheckReleased && !document.querySelector('.adm-financial-repair-target button').disabled);
    }
    assert.equal(await page.$('.adm-financial-repair-facts'), null, 'old identity/snapshot/amounts must not be presented in the new scope');
    assert.equal(await page.evaluate(() => window.f16.calls.some(c => c.url === '/api/admin/approvals/requests' && c.method === 'POST')), false);
    // Another explicit read remains possible after the previous scope retired.
    if (late) await page.evaluate(() => { window.f16.releasePrecheck = null; });
    await readDisplayFacts(page);
    if (late) { await page.waitForFunction(() => typeof window.f16.releasePrecheck === 'function'); await page.evaluate(() => window.f16.releasePrecheck()); }
    await page.waitForSelector('.adm-financial-repair-facts');
  });
  for (const [label, key, valid] of [['ASCII111', 'a'.repeat(111), true], ['ASCII112', 'a'.repeat(112), false], ['UTF8-111bytes', '中'.repeat(37), true], ['UTF8-114bytes', '中'.repeat(38), false]]) {
    await run(`UsageSettlementRepair prefixed key byte boundary ${label}`, '', async page => {
      await openForm(page); await page.waitForSelector('.adm-financial-repair-draft');
      await page.type('.adm-financial-repair-draft textarea', 'Controlled key boundary justification');
      await page.evaluate(key => { const input = document.querySelector('.adm-financial-repair-draft input'); Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value').set.call(input, key); input.dispatchEvent(new Event('input', { bubbles: true })); }, key);
      await page.click('.adm-financial-repair-draft button');
      if (valid) {
        await page.waitForFunction(() => window.f16.status === 'SUBMITTED');
        const draft = await page.evaluate(() => JSON.parse(window.f16.calls.find(call => call.url === '/api/admin/approvals/requests' && call.method === 'POST').body));
        assert.equal(draft.idempotency_key, `approval:${key}`); assert.equal(new TextEncoder().encode(draft.idempotency_key).length, 120);
      } else {
        await page.waitForFunction(() => window.f16.status === 'SUBMITTED' || document.querySelector('.adm-financial-repair [role=alert]'));
        assert.equal(await page.evaluate(() => window.f16.calls.some(call => call.url === '/api/admin/approvals/requests' && call.method === 'POST')), false);
        assert.equal(await page.$eval('.adm-financial-repair [role=alert]', node => node.textContent), await page.evaluate(() => window.f16.ledgerRepairCopy.zh.invalidDraft));
      }
    });
  }
  for (const language of ['zh', 'en', 'ja']) await run(`UsageSettlementRepair real host/precheck/create/submit ${language}`, `language=${language}`, async page => {
    await openForm(page); await create(page); await page.waitForFunction(() => window.f16.status === 'SUBMITTED' && document.querySelector('nav button[data-section="approvals"][aria-current="page"]') && [...document.querySelectorAll('.adm-financial-repair')].every(form => form.closest('[hidden]')));
    const calls = await page.evaluate(() => window.f16.calls); const precheck = calls.find(call => call.url.endsWith('/approval-command-prechecks'));
    assert.equal(JSON.parse(precheck.body).target_resource_id, await page.evaluate(() => window.f16.id(1))); assert.equal(JSON.parse(precheck.body).usage_request_id, await page.evaluate(() => window.f16.id(2))); assert.equal(JSON.parse(precheck.body).loss_review_id, await page.evaluate(() => window.f16.id(3))); assert.equal(Object.hasOwn(JSON.parse(precheck.body), 'expected_case_version'), false);
    const draft = calls.find(call => call.url === '/api/admin/approvals/requests' && call.method === 'POST'); assert.ok(draft.body.includes('"settlement_microcredits":9007199254740993')); assert.equal(JSON.parse(draft.body).normalized_command.evidence_kind, 'LOSS_REVIEW');
    assert.equal(JSON.parse(draft.body).normalized_command.reason_code, 'MISSING_USAGE_SETTLEMENT'); assert.equal(draft.headers['X-CSRF-Token'], 'controlled-csrf'); assert.equal(draft.cache, 'no-store');
    assert.ok(calls.some(call => call.url.endsWith('/actions/submit'))); assert.equal(calls.some(call => call.url.includes('/api/admin/billing/')), false);
  });
  await run('UsageSettlementRepair directory/filter/typed EDIT/current proof/drilldown preserves original hold/usage/loss review', 'existing=1', async page => {
    await center(page, 'directory'); await page.waitForFunction(() => document.querySelector('tbody')?.textContent.includes('USAGE_SETTLEMENT_REPAIR'));
    assert.equal(await page.evaluate(() => document.querySelector('tbody').textContent.includes('UNKNOWN_REPAIR')), false);
    await page.evaluate(() => [...document.querySelectorAll('.adm-approval-tabs button')].find(button => button.textContent === window.f16.adminPlatformCopy.zh.approvalCenter.mine).click());
    await page.waitForSelector('.adm-approval-toolbar select'); await page.select('.adm-approval-toolbar select', 'USAGE_SETTLEMENT_REPAIR');
    await page.type('.adm-approval-toolbar input', await page.evaluate(() => window.f16.id(1))); await page.click('.adm-approval-toolbar button');
    await page.waitForFunction(() => window.f16.calls.some(call => call.url.includes('command_type=USAGE_SETTLEMENT_REPAIR') && call.url.includes('target=')) && document.querySelector('tbody tr button') && !document.querySelector('tbody tr button').disabled);
    await page.evaluate(() => document.querySelector('tbody tr button').click()); await page.waitForSelector('article textarea');
    assert.ok(await page.$eval('article pre', node => node.textContent.includes('"evidence_kind": "LOSS_REVIEW"') && node.textContent.includes('9007199254740993')));
    await page.type('article textarea', 'Updated controlled usage-settlement evidence');
    await page.evaluate(() => [...document.querySelectorAll('article button')].find(button => button.textContent === window.f16.financeCopy.zh.editSubmit).click());
    await page.waitForFunction(() => window.f16.status === 'SUBMITTED');
    const calls = await page.evaluate(() => window.f16.calls); const update = calls.find(call => call.method === 'PUT');
    assert.equal(JSON.parse(update.body).expected_version, 1); assert.ok(update.body.includes('"settlement_microcredits":9007199254740993')); assert.equal(JSON.parse(update.body).normalized_command.evidence_kind, 'LOSS_REVIEW'); assert.equal(JSON.parse(update.body).normalized_command.hold_id, await page.evaluate(() => window.f16.id(1))); assert.equal(JSON.parse(update.body).normalized_command.usage_request_id, await page.evaluate(() => window.f16.id(2))); assert.equal(JSON.parse(update.body).normalized_command.loss_review_id, await page.evaluate(() => window.f16.id(3)));
    assert.ok(calls.some(call => call.url.includes('command_type=USAGE_SETTLEMENT_REPAIR') && call.url.includes('target=')));
    await page.waitForFunction(() => [...document.querySelectorAll('article button')].some(button => button.textContent === window.f16.adminPlatformCopy.zh.approvalCenter.openBusinessArea && !button.disabled));
    await page.evaluate(() => [...document.querySelectorAll('article button')].find(button => button.textContent === window.f16.adminPlatformCopy.zh.approvalCenter.openBusinessArea).click());
    await page.waitForSelector('[data-workspace="usagesettlementrepair"]:not([hidden]) .adm-financial-repair-target input', { visible: true });
    assert.equal(await page.$('[data-workspace="usagesettlementrepair"]:not([hidden]) .adm-financial-repair-target select'), null);
    assert.equal(await page.$eval('[data-workspace="usagesettlementrepair"]:not([hidden]) .adm-financial-repair-technical code', node => node.textContent), 'USAGE_SETTLEMENT_REPAIR');
  });
  await run('UsageSettlementRepair fresh EDIT refuses changed original loss review without UPDATE', 'existing=1&edit-loss-changed=1', async page => {
    await center(page, 'mine'); await page.waitForSelector('tbody tr button'); await page.click('tbody tr button'); await page.waitForSelector('article textarea');
    await page.type('article textarea', 'Controlled original chargeback CAS intent');
    await page.evaluate(() => [...document.querySelectorAll('article button')].find(button => button.textContent === window.f16.financeCopy.zh.editSubmit).click());
    await page.waitForFunction(() => [...document.querySelectorAll('[role=alert]')].some(node => node.textContent === window.f16.financeCopy.zh.approvalReadUnavailable));
    assert.equal(await page.evaluate(() => window.f16.calls.some(call => call.method === 'PUT' || call.url.endsWith('/actions/submit'))), false);
  });
  await run('UsageSettlementRepair approver-only real detail decide without requester or wallet calls', 'approve=1&existing=1', async page => {
    await page.evaluate(() => { window.f16.status = 'SUBMITTED'; }); await center(page, 'pending');
    await page.waitForSelector('tbody tr button'); assert.equal(await page.$('.adm-financial-repair'), null); await page.click('tbody tr button'); await page.waitForSelector('article textarea');
    await page.type('article textarea', 'Independent approved usage-settlement review');
    await page.evaluate(() => [...document.querySelectorAll('article button')].find(button => button.textContent === window.f16.financeCopy.zh.approve).click());
    await page.waitForFunction(() => document.querySelector('article')?.textContent.includes('EXECUTED'));
    assert.equal(await page.evaluate(() => window.f16.calls.some(call => call.url.includes('/billing/') || call.url.endsWith('/approval-command-prechecks') || call.method === 'PUT')), false);
  });
  await run('No repair permission hides UsageSettlementRepair and does not call its receiver', 'deny=1', async page => {
    await page.waitForFunction(message => document.querySelector('[role="alert"]')?.textContent === message, {}, adminLayoutCopy.zh.unauthorized); assert.equal(await page.$('.adm-financial-repair'), null);
    assert.equal(await page.evaluate(() => window.f16.calls.some(call => call.url.startsWith('/api/admin/'))), false);
  });
  for (const condition of ['unavailable', 'mismatch', 'usage-mismatch', 'loss-mismatch', 'amount-mismatch']) await run(`UsageSettlementRepair ${condition} owner facts reject before create`, `${condition}=1`, async page => {
    await openForm(page); await page.waitForSelector('.adm-financial-repair [role=alert]'); assert.equal(await page.$('.adm-financial-repair-draft'), null);
    assert.equal(await page.evaluate(() => window.f16.calls.some(call => call.url === '/api/admin/approvals/requests' && call.method === 'POST')), false);
  });
  await run('UsageSettlementRepair committed unknown + denied replay retains original facts/key then recovers', 'lost=1', async page => {
    await openForm(page); await create(page); await page.waitForFunction(() => document.querySelector('.adm-financial-repair-draft button')?.textContent === window.f16.financeCopy.zh.retry);
    await page.click('.adm-financial-repair-draft button'); await page.waitForFunction(() => window.f16.calls.some(call => call.method === 'PUT') && !document.querySelector('.adm-financial-repair-draft button').disabled);
    assert.equal(await page.$eval('.adm-financial-repair-draft input', input => input.disabled), true); assert.equal(await page.$eval('.adm-financial-repair-draft textarea', input => input.disabled), true);
    await page.evaluate(() => { window.f16.denyReplay = false; }); await page.click('.adm-financial-repair-draft button'); await page.waitForFunction(() => window.f16.status === 'SUBMITTED');
    const calls = await page.evaluate(() => window.f16.calls.filter(call => call.method === 'PUT' || call.url === '/api/admin/approvals/requests' && call.method === 'POST'));
    assert.equal(calls.length, 3); const inputs = calls.map(call => { const { expected_version, ...intent } = JSON.parse(call.body); return { expected_version, intent }; });
    assert.equal(new Set(inputs.map(input => JSON.stringify(input.intent))).size, 1); assert.deepEqual(inputs.slice(1).map(input => input.expected_version), [1, 1]); assert.equal(await page.evaluate(() => window.f16.prechecks), 1);
  });
  await run('UsageSettlementRepair initial definite rejection unlocks original input for correction', 'initial-deny=1', async page => {
    await openForm(page); await create(page); await page.waitForFunction(() => document.querySelector('.adm-financial-repair [role=alert]') && !document.querySelector('.adm-financial-repair-draft button').disabled);
    assert.equal(await page.$eval('.adm-financial-repair-draft input', input => input.disabled), false); assert.equal(await page.evaluate(() => window.f16.created), false);
  });
  await run('Directory retirement releases only old child; current EDIT UNKNOWN keeps navigation locked', 'existing=1&edit-lost=1', async page => {
    await center(page, 'directory'); await page.waitForFunction(() => document.querySelector('tbody')?.textContent.includes('USAGE_SETTLEMENT_REPAIR'));
    await page.evaluate(() => [...document.querySelectorAll('.adm-approval-tabs button')].find(button => button.textContent === window.f16.adminPlatformCopy.zh.approvalCenter.mine).click());
    await page.waitForSelector('tbody tr button'); await page.click('tbody tr button'); await page.waitForSelector('article textarea');
    await page.type('article textarea', 'Current original-intent controlled EDIT');
    await page.evaluate(() => [...document.querySelectorAll('article button')].find(button => button.textContent === window.f16.financeCopy.zh.editSubmit).click());
    await page.waitForFunction(() => document.body.textContent.includes(window.f16.financeCopy.zh.unknown) && window.f16.calls.some(call => call.method === 'PUT'));
    assert.ok(await page.evaluate(() => [...document.querySelectorAll('.adm-approval-tabs button')].every(button => button.disabled)));
    await page.evaluate(() => window.f16.replaceIdentity());
    await page.waitForFunction(() => window.f16.calls.filter(call => call.url === '/api/account/v1/account/bootstrap').length === 2 && document.querySelector('.adm-approval-tabs'));
    await page.waitForFunction(() => [...document.querySelectorAll('.adm-approval-tabs button')].every(button => !button.disabled));
    assert.equal(await page.evaluate(() => document.body.textContent.includes(window.f16.financeCopy.zh.unknown)), false);
  });
  await run('Retired identity ignores late UsageSettlementRepair precheck', 'late=1', async page => {
    await openForm(page); await page.waitForFunction(() => window.f16.releasePrecheck); await page.evaluate(() => { const release = window.f16.releasePrecheck; window.f16.replaceIdentity(); setTimeout(release, 50); });
    await page.waitForFunction(() => window.f16.calls.filter(call => call.url === '/api/account/v1/account/bootstrap').length === 2);
    assert.equal(await page.$('.adm-financial-repair-draft'), null);
  });
  for (const language of ['zh', 'en', 'ja']) {
    await run(`UsageSettlementRepair recent authentication retains local intent ${language}`, `language=${language}&recent-auth=1`, async page => {
      await openForm(page); await create(page); await page.waitForSelector('.adm-usage-settlement-repair-reauthentication button');
      assert.equal(await page.$eval('.adm-usage-settlement-repair-reauthentication p', node => node.textContent), await page.evaluate(() => window.f16.ledgerRepairCopy[window.f16.language].writeOffRecentAuthenticationRequired));
      assert.ok(await page.$eval('.adm-usage-settlement-repair-reauthentication p', node => node.textContent.includes('EDIT')));
      const first = await page.evaluate(() => window.f16.calls.find(call => call.method === 'POST' && call.url === '/api/admin/approvals/requests').body);
      assert.equal(await page.evaluate(() => window.f16.created), false);
      await clickReauthentication(page);
      assert.equal(await page.evaluate(() => window.f16.reauthentication), '/admin.html');
      assert.equal(await page.$eval('.adm-financial-repair-draft input', input => input.value), 'controlled-usage-settlement');
      await page.evaluate(() => { window.f16.authenticatedAgain = true; });
      // Explicit retry only: the controlled CTA does not navigate or submit.
      assert.equal(await page.evaluate(() => window.f16.calls.filter(call => call.method === 'POST' && call.url === '/api/admin/approvals/requests').length), 1);
      await page.click('.adm-financial-repair-draft button'); await page.waitForFunction(() => window.f16.status === 'SUBMITTED');
      const bodies = await page.evaluate(() => window.f16.calls.filter(call => call.method === 'POST' && call.url === '/api/admin/approvals/requests').map(call => call.body));
      assert.deepEqual(bodies, [first, first]); assert.equal(await page.evaluate(() => window.f16.prechecks), 1);
    });
    await run(`UsageSettlementRepair precheck recent authentication fails before draft ${language}`, `language=${language}&${language === 'ja' ? 'precheck-401' : 'precheck-auth'}=1`, async page => {
      await openForm(page); await page.waitForSelector('.adm-usage-settlement-repair-reauthentication button');
      assert.equal(await page.$('.adm-financial-repair-draft'), null);
      assert.equal(await page.evaluate(() => window.f16.calls.some(call => call.method === 'POST' && call.url === '/api/admin/approvals/requests')), false);
      await clickReauthentication(page); assert.equal(await page.evaluate(() => window.f16.reauthentication), '/admin.html');
    });
  }
  await run('UsageSettlementRepair unknown recent-auth denial retains original frozen facts and explicit replay', 'lost-recent=1', async page => {
    await openForm(page); await create(page); await page.waitForFunction(() => document.querySelector('.adm-financial-repair-draft button')?.textContent === window.f16.financeCopy.zh.retry);
    await page.click('.adm-financial-repair-draft button'); await page.waitForSelector('.adm-usage-settlement-repair-reauthentication button');
    assert.equal(await page.$eval('.adm-financial-repair-draft input', input => input.disabled), true);
    assert.equal(await page.$eval('.adm-financial-repair-draft textarea', input => input.disabled), true);
    await page.evaluate(() => { window.f16.authenticatedAgain = true; });
    await page.click('.adm-financial-repair-draft button'); await page.waitForFunction(() => window.f16.status === 'SUBMITTED');
    const rawBodies = await page.evaluate(() => window.f16.calls.filter(call => call.method === 'PUT' || call.url === '/api/admin/approvals/requests' && call.method === 'POST').map(call => call.body));
    const bodies = rawBodies.map(body => parseFinanceJSON(body));
    assert.equal(bodies.length, 3);
    assert.deepEqual(bodies.slice(1).map(body => body.expected_version), [1, 1]);
    const original = bodies.map(({ expected_version, ...body }) => body);
    assert.equal(new Set(original.map(body => JSON.stringify(body))).size, 1);
    assert.equal(await page.evaluate(() => window.f16.prechecks), 1);
  });
  await run('Retired usage settlement identity ignores late authentication failure', 'late-auth=1', async page => {
    await openForm(page); await page.waitForFunction(() => window.f16.releasePrecheck);
    await page.evaluate(() => { const release = window.f16.releasePrecheck; window.f16.replaceIdentity(); setTimeout(release, 50); });
    await page.waitForFunction(() => window.f16.lateAuthenticationReleased && window.f16.calls.filter(call => call.url === '/api/account/v1/account/bootstrap').length === 2);
    assert.equal(await page.$('.adm-usage-settlement-repair-reauthentication'), null);
    assert.equal(await page.$('.adm-financial-repair-draft'), null);
  });
} finally { await browser.close(); await server.close(); }
const result = { pass, fail, results, limits: ['Real AdminPlatformView/AdminFinanceWorkspace/AdminPlatformClient under controlled BFF replies; owner effects, independent issuer/NATS, PG, deployed API and full E2E are not exercised.'] };
fs.writeFileSync(process.env.F16_REACT_RESULT || 'usage-settlement-repair-smoke-result.json', JSON.stringify(result, null, 2)); console.log(JSON.stringify(result)); process.exitCode = fail ? 1 : 0;
