import assert from 'node:assert/strict';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';
import react from '@vitejs/plugin-react';
import * as puppeteer from './helpers/puppeteer-fixture.mjs';
import { createServer } from './helpers/vite-fixture.mjs';

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const chrome = process.env.CHROME || ['C:/Program Files/Google/Chrome/Application/chrome.exe', 'C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe', '/usr/bin/chromium', '/usr/bin/google-chrome'].find(fs.existsSync);
assert.ok(chrome, 'Browser unavailable; financial completion UI validation not performed');
const profile = fs.mkdtempSync(path.join(os.tmpdir(), 'pinvou-financial-completion-'));
let vite; let browser;
try {
  vite = await createServer({ root, configFile: false, appType: 'mpa', logLevel: 'error', plugins: [react()], server: { host: '127.0.0.1', port: 0, watch: null } });
  await vite.listen();
  browser = await puppeteer.launch({ executablePath: chrome, headless: 'new', userDataDir: profile, args: ['--disable-gpu', '--no-first-run'] });
  for (const language of ['zh', 'en', 'ja']) {
    const page = await browser.newPage(); page.setDefaultTimeout(15000);
    const errors = []; page.on('pageerror', error => errors.push(error.message)); page.on('dialog', dialog => dialog.accept());
    await page.goto(`http://127.0.0.1:${vite.httpServer.address().port}/tests/fixtures/account-platform-f02-ui.html`);
    await page.waitForFunction(() => window.f02);
    await page.evaluate(language => {
      const original = window.fetch; const id = window.f02.id; const receiptID = window.f02.other;
      const item = { case_id: id, payment_order_id: id, anomaly_type: 'DUPLICATE_PAYMENT', status: 'RESOLVED', severity: 'P2', resolution_code: 'ESCALATE_FINANCIAL_REPAIR', financial_action_status: 'PENDING', version: 2 };
      const facts = { proof_kind: 'PAYMENT_REPAIR', reference_id: receiptID, payment_order_id: id, payment_evidence_id: id, payment_transaction_id: receiptID, provider_id: 'controlled', provider_version: 'v1', provider_transaction_id: 'controlled-original-movement', order_amount_fen: '100', amount_fen: '100', currency: 'CNY', receipt: { repair_id: receiptID, package_id: receiptID, journal_id: id }, facts_digest: 'a'.repeat(64) };
      const probe = window.completionProbe = { proposals: [], confirmations: [], proposal: null, item, facts };
      window.fetch = async (url, options = {}) => {
        url = String(url);
        const json = (value, status = 200) => new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
        if (url.includes('/payment-anomalies?')) return json({ items: [probe.item], next_cursor: '' });
        if (url.endsWith(`/payment-anomalies/${id}`)) return json({ case: probe.item, events: [], financial_completion_proofs: [facts], ...(probe.proposal ? { financial_completion_proposal: probe.proposal } : {}) });
        if (url.endsWith('/financial-completion-proposals')) {
          probe.proposals.push({ url, key: options.headers['Idempotency-Key'], body: options.body });
          probe.proposal ||= { proposal_id: receiptID, case_id: id, case_version: 2, proposer_id: window.f02.user, input: { proof_kind: facts.proof_kind, reference_id: receiptID }, facts, reason: JSON.parse(options.body).reason };
          if (probe.proposals.length === 1) throw new TypeError('controlled lost proposal ACK');
          if (probe.proposals.length === 2) return json({ code: 'CONFLICT' }, 409);
          return json({ case_id: id, case: probe.item, proposal: probe.proposal });
        }
        if (url.endsWith(`/financial-completion-proposals/${receiptID}/confirm`)) {
          probe.confirmations.push({ url, key: options.headers['Idempotency-Key'], body: options.body });
          probe.item = { ...item, financial_action_status: 'APPLIED', version: 3 };
          if (probe.confirmations.length === 1) throw new TypeError('controlled lost confirmation ACK');
          return json({ case_id: id, case: probe.item, proposal: probe.proposal });
        }
        return original(url, options);
      };
      window.f02.language = language; window.f02.adminTab = 'anomalies'; window.f02.render('admin');
    }, language);
    const click = async key => {
      const clicked = await page.evaluate(key => {
        const text = window.f02.languageCopy()[key];
        const button = [...document.querySelectorAll('button')].find(value => value.textContent.trim() === text && !value.disabled);
        if (!button) return false; button.click(); return true;
      }, key);
      assert.ok(clicked, `${language}: unavailable action ${key}`);
    };
    const unknownReady = (kind, count) => page.waitForFunction((kind, count) => window.completionProbe[kind].length === count && [...document.querySelectorAll('button')].some(value => value.textContent.trim() === window.f02.languageCopy().retry && !value.disabled), {}, kind, count);
    await page.waitForSelector('tbody tr'); await click('details'); await page.waitForSelector('article textarea');
    await page.type('article textarea', 'Controlled completed financial receipt proposal');
    await click('proposeFinancialCompletion'); await unknownReady('proposals', 1);
    assert.equal(await page.$eval('article textarea', value => value.disabled), true);
    await click('retry'); await unknownReady('proposals', 2); await click('retry');
    await page.waitForFunction(() => window.completionProbe.proposals.length === 3 && [...document.querySelectorAll('button')].some(value => value.textContent.trim() === window.f02.languageCopy().confirmFinancialCompletion));
    assert.equal(await page.evaluate(() => [...document.querySelectorAll('button')].find(value => value.textContent.trim() === window.f02.languageCopy().confirmFinancialCompletion).disabled), true);
    assert.ok(await page.evaluate(() => document.body.textContent.includes('controlled-original-movement')));
    const proposed = await page.evaluate(() => window.completionProbe.proposals);
    assert.match(proposed[0].key, /^finance-/); assert.deepEqual(proposed[1], proposed[0]); assert.deepEqual(proposed[2], proposed[0]);
    assert.equal(JSON.parse(proposed[0].body).proof_kind, 'PAYMENT_REPAIR');
    await page.evaluate(() => { window.f02.user = window.f02.other; window.f02.render('admin'); });
    await page.waitForSelector('tbody tr'); await click('details'); await page.waitForSelector('article textarea');
    await page.type('article textarea', 'Controlled independently verified completion evidence');
    await click('confirmFinancialCompletion'); await unknownReady('confirmations', 1); await click('retry');
    await page.waitForFunction(() => window.completionProbe.confirmations.length === 2 && ![...document.querySelectorAll('button')].some(value => value.textContent.trim() === window.f02.languageCopy().confirmFinancialCompletion));
    const confirmed = await page.evaluate(() => window.completionProbe.confirmations);
    assert.match(confirmed[0].key, /^finance-/); assert.notEqual(confirmed[0].key, proposed[0].key); assert.deepEqual(confirmed[1], confirmed[0]);
    assert.deepEqual(Object.keys(JSON.parse(confirmed[0].body)), ['reason']); assert.deepEqual(errors, []);
    console.log(`PASS ${language}: mounted admin queue, owner-derived receipt selection, self-review blocked, independent reviewer, original keys/facts after lost ACK and conflict`);
    await page.close();
  }
} finally {
  if (browser) await browser.close(); if (vite) await vite.close();
  assert.equal(path.dirname(path.resolve(profile)), path.resolve(os.tmpdir())); assert.ok(path.basename(profile).startsWith('pinvou-financial-completion-'));
  fs.rmSync(profile, { recursive: true, force: true });
}
