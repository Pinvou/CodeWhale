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
const requesterID = '22222222-2222-4222-8222-222222222222';
const requestID = '33333333-3333-4333-8333-333333333333';
const targetID = '44444444-4444-4444-8444-444444444444';
let decisionBody;

const detail = {
  approval_request_id: requestID,
  command_type: 'WALLET_ADJUSTMENT',
  status: 'SUBMITTED',
  version: 1,
  action_control_version: 1,
  detail_availability: 'AVAILABLE',
  requester: { user_id: requesterID },
  target: { resource_type: 'MANUAL_ADJUSTMENT', resource_id: targetID },
  updated_at: '2026-09-21T08:00:00Z',
  allowed_actions: ['APPROVE', 'REJECT'],
  command: { adjustment_id: targetID },
};

function json(response, payload, status = 200, headers = {}) {
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', ...headers });
  response.end(JSON.stringify(payload));
}

function readRequest(request) {
  return new Promise((resolve, reject) => {
    let body = '';
    request.setEncoding('utf8');
    request.on('data', (chunk) => { body += chunk; });
    request.on('end', () => resolve(body));
    request.on('error', reject);
  });
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'test-csrf' });
  if (url.pathname.includes('/account/bootstrap') || url.pathname.includes('/authorization-context')) {
    return json(response, { user_id: userID, platform_permissions: [
      'platform.approvals.read', 'platform.approvals.decide', 'platform.billing.wallets.read',
    ] });
  }
  if (url.pathname === '/api/admin/approvals/capability') return json(response, {
    session_approval_wire_version: 2,
    control: { mode: 'LEGACY', approval_wire_version: 1, proof_required: false,
      epoch: 1, authorization_relay_state: 'RUNNING', authorization_relay_epoch: 1, version: 1 },
  });
  if (url.pathname === '/api/admin/approvals/requests' && request.method === 'GET') {
    return json(response, { items: [{ ...detail, allowed_actions: [] }], next_cursor: '' });
  }
  if (url.pathname === `/api/admin/approvals/requests/${requestID}` && request.method === 'GET') {
    return json(response, detail);
  }
  if (url.pathname === `/api/admin/approvals/requests/${requestID}/actions/decide` && request.method === 'POST') {
    decisionBody = JSON.parse(await readRequest(request));
    // Simulate a transport failure after the server may have accepted the command.
    // The UI must enter the uncertain state and prevent a duplicate submission.
    response.writeHead(503, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
    response.end(JSON.stringify({ code: 'UPSTREAM_TIMEOUT' }));
    return;
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

await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
const { port } = server.address();
const browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
try {
  const page = await browser.newPage();
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  await page.setViewport({ width: 1440, height: 1000, deviceScaleFactor: 1 });
  await page.evaluateOnNewDocument(() => localStorage.setItem('pinvou.admin.language', 'en'));
  await page.goto(`http://127.0.0.1:${port}/admin.html?section=approvals&approval_view=pending`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-approval-table tbody tr');
  await page.click('.adm-approval-table tbody tr button');
  await page.waitForFunction((id) => new URL(location.href).searchParams.get('approval_request_id') === id, {}, requestID);
  await page.waitForSelector('.adm-approval-detail .adm-approval-actions button');
  const actionNames = await page.$$eval('.adm-approval-actions button', (buttons) => buttons.map((button) => button.textContent.trim()));
  assert.deepEqual(actionNames, ['Approve', 'Reject']);
  await page.click('.adm-approval-actions button');
  await page.waitForSelector('.adm-approval-dialog');
  await page.type('.adm-approval-dialog textarea', 'approve after independent review');
  await page.click('.adm-approval-dialog button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('.adm-approval-uncertain') !== null);
  assert.deepEqual(decisionBody, { expected_version: 1, decision: 'APPROVED', reason: 'approve after independent review', action_control_version: 1 });
  await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 1 });
  assert.ok(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth <= 1));
  assert.deepEqual(pageErrors, []);
  console.log('Admin approval workspace UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
