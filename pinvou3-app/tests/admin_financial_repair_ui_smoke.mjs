import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const browserPath = [
  process.env.CHROME,
  'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium or Edge is required');

const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const userID = '11111111-1111-4111-8111-111111111111';
const ids = [
  '22222222-2222-4222-8222-222222222222',
  '33333333-3333-4333-8333-333333333333',
  '44444444-4444-4444-8444-444444444444',
  '55555555-5555-4555-8555-555555555555',
];
const requestID = '66666666-6666-4666-8666-666666666666';
const projection = {
  command_type: 'LEDGER_REPAIR',
  policy_version: 1,
  snapshot_schema_version: 1,
  target_resource_type: 'LEDGER_POSTING',
  target_resource_id: ids[0],
  normalized_command: {
    target_journal_entry_id: ids[1],
    target_posting_id: ids[0],
    wrong_ledger_account_id: ids[2],
    correct_ledger_account_id: ids[3],
    amount_microcredits: 30,
    reason_code: 'ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT',
  },
  critical_resource_versions: { ledger_version: 1 },
  financial_snapshot: { amount_microcredits: 30 },
};
let precheckBody;
let draftBody;

function json(response, payload, status = 200) {
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/state') return json(response, { precheckBody, draftBody });
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'test-csrf' });
  if (url.pathname.includes('/account/bootstrap') || url.pathname.includes('/authorization-context')) {
    return json(response, { user_id: userID, platform_permissions: [
      'platform.billing.repairs.request', 'platform.approvals.read',
      'platform.approvals.decide', 'platform.approvals.write',
    ] });
  }
  if (url.pathname === '/api/admin/approvals/capability') {
    return json(response, { session_approval_wire_version: 2, control: {
      mode: 'LEGACY', approval_wire_version: 1, proof_required: false,
      epoch: 1, authorization_relay_state: 'RUNNING', authorization_relay_epoch: 1, version: 1,
    } });
  }
  if (url.pathname === '/api/admin/approvals/prechecks' && request.method === 'POST') {
    precheckBody = JSON.parse(await readRequest(request));
    return json(response, projection);
  }
  if (url.pathname === '/api/admin/approvals/requests' && request.method === 'POST') {
    draftBody = JSON.parse(await readRequest(request));
    return json(response, { approval_request_id: requestID }, 201);
  }
  if (url.pathname.startsWith('/api/admin/')) return json(response, { items: [], next_cursor: '' });
  const relative = url.pathname === '/admin.html' ? 'admin.html' : url.pathname.replace(/^\//, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot) || !existsSync(target)) {
    response.writeHead(404);
    response.end('not found');
    return;
  }
  const type = path.extname(target) === '.html' ? 'text/html' : path.extname(target) === '.css' ? 'text/css' : 'text/javascript';
  response.writeHead(200, { 'Content-Type': `${type}; charset=utf-8`, 'Cache-Control': 'no-store' });
  response.end(await readFile(target));
});

function readRequest(request) {
  return new Promise((resolve, reject) => {
    let body = '';
    request.setEncoding('utf8');
    request.on('data', (chunk) => { body += chunk; });
    request.on('end', () => resolve(body));
    request.on('error', reject);
  });
}

await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
const { port } = server.address();
const browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
try {
  const page = await browser.newPage();
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.setViewport({ width: 1440, height: 1000, deviceScaleFactor: 1 });
  await page.goto(`http://127.0.0.1:${port}/admin.html?section=approvals&approval_view=repair`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-financial-repair');
  const inputs = await page.$$('.adm-financial-repair-target input');
  assert.equal(inputs.length, 5);
  for (const [index, value] of [...ids, '30'].entries()) await inputs[index].type(value);
  await page.click('.adm-financial-repair-target button[type="submit"]');
  await page.waitForSelector('.adm-financial-repair-facts');
  await page.type('.adm-financial-repair-draft textarea', 'approved ledger correction reason');
  await page.type('.adm-financial-repair-draft input', 'ledger-smoke-key');
  await page.click('.adm-financial-repair-draft button[type="submit"]');
  await page.waitForFunction((pathName, expected) => fetch(pathName).then((response) => response.json()).then((state) => state.draftBody?.idempotency_key === expected), {}, '/__test__/state', 'approval:ledger-smoke-key');
  assert.match(page.url(), new RegExp(`approval_view=mine.*approval_request_id=${requestID}`));
  assert.deepEqual(precheckBody, {
    command_type: 'LEDGER_REPAIR', target_resource_id: ids[0], target_journal_entry_id: ids[1],
    wrong_ledger_account_id: ids[2], correct_ledger_account_id: ids[3], amount_microcredits: 30,
  });
  assert.deepEqual(draftBody, {
    command_type: 'LEDGER_REPAIR', idempotency_key: 'approval:ledger-smoke-key',
    normalized_command: {
      target_journal_entry_id: ids[1], target_posting_id: ids[0], wrong_ledger_account_id: ids[2],
      correct_ledger_account_id: ids[3], amount_microcredits: 30,
      reason_code: 'ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT',
    }, target_resource_type: 'LEDGER_POSTING', target_resource_id: ids[0],
    reason: 'approved ledger correction reason', redacted_evidence: [], expected_impact: {},
    critical_resource_versions: { ledger_version: 1 }, financial_snapshot: { amount_microcredits: 30 },
  });
  await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 1 });
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth <= 1));
  assert.deepEqual(pageErrors, []);
  console.log('Admin financial repair UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
