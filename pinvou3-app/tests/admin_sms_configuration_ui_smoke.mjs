import assert from 'node:assert/strict';
import { createServer } from 'node:http';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import puppeteer from 'puppeteer-core';

const browserPath = [
  process.env.CHROME,
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium or Edge is required');

const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
let permissions = ['platform.sms-config.read', 'platform.sms-config.manage'];
let configuration = {
  revision: 3,
  source: 'ADMIN',
  enabled: true,
  provider_id: 'aliyun-managed',
  adapter_version: 'v1',
  credential_profile: 'aliyun-default',
  sign_name: 'PINVOU',
  templates: { REGISTER: 'SMS_REGISTER', LOGIN: 'SMS_LOGIN', PROFILE_MOBILE: 'SMS_PROFILE', PASSWORD_RESET: 'SMS_RESET' },
  configuration_fingerprint: 'a'.repeat(64),
  deployment_capability: true,
  readiness: 'READY',
  available_profiles: [
    { profile_id: 'aliyun-default', provider_id: 'aliyun-managed', configured: true },
    { profile_id: 'http-default', provider_id: 'http-generated', configured: false },
  ],
};
const observations = { reads: 0, writes: [] };

function json(response, payload, status = 200) {
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(payload));
}

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/__test__/readonly') {
    permissions = ['platform.sms-config.read'];
    return json(response, { permissions });
  }
  if (url.pathname === '/__test__/state') return json(response, observations);
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf-test' });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: '0199a100-0001-7000-8000-000000000101', platform_permissions: permissions });
  if (url.pathname === '/api/admin/account/admin/v1/sms-configuration' && request.method === 'GET') {
    observations.reads += 1;
    return json(response, configuration);
  }
  if (url.pathname === '/api/admin/account/admin/v1/sms-configuration' && request.method === 'PUT') {
    const chunks = [];
    for await (const chunk of request) chunks.push(chunk);
    const body = JSON.parse(Buffer.concat(chunks).toString('utf8'));
    observations.writes.push({ body, idempotencyKey: request.headers['idempotency-key'], csrf: request.headers['x-csrf-token'] });
    configuration = { ...configuration, ...body, revision: configuration.revision + 1, source: 'ADMIN', readiness: body.enabled ? 'READY' : 'DISABLED', configuration_fingerprint: 'b'.repeat(64) };
    return json(response, configuration);
  }
  const relative = url.pathname === '/admin.html' ? 'admin.html' : url.pathname.replace(/^\//, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot) || !existsSync(target)) {
    response.writeHead(404);
    return response.end('not found');
  }
  const type = path.extname(target) === '.html' ? 'text/html' : path.extname(target) === '.css' ? 'text/css' : 'text/javascript';
  response.writeHead(200, { 'Content-Type': `${type}; charset=utf-8` });
  response.end(await readFile(target));
});

await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
const { port } = server.address();
const browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
try {
  const page = await browser.newPage();
  await page.setViewport({ width: 1440, height: 1000, deviceScaleFactor: 1 });
  await page.evaluateOnNewDocument(() => localStorage.setItem('pinvou.admin.language', 'zh'));
  await page.goto(`http://127.0.0.1:${port}/admin.html?section=settings`, { waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-sms-configuration[data-readiness="READY"]');
  assert.equal(await page.$eval('.adm-sms-summary strong', (node) => node.textContent.trim()), '3');
  assert.equal((await page.$$('input[type="password"]')).length, 0, 'the console must never render secret inputs');
  const controls = await page.$$eval('.adm-sms-configuration input, .adm-sms-configuration textarea, .adm-sms-configuration select', (nodes) => nodes.map((node) => node.outerHTML).join('\n'));
  assert.equal(/access.?key|bearer|endpoint|secret/i.test(controls), false, 'secret-bearing controls must not exist');

  await page.click('.adm-sms-reason textarea');
  await page.type('.adm-sms-reason textarea', '切换模板以匹配当前短信签名');
  await page.click('.adm-sms-fields input[maxlength="100"]');
  await page.keyboard.down('Control');
  await page.keyboard.press('KeyA');
  await page.keyboard.up('Control');
  await page.keyboard.press('Backspace');
  await page.type('.adm-sms-fields input[maxlength="100"]', 'PINVOU-NEW');
  assert.equal(await page.$eval('.adm-sms-form button[type="submit"]', (node) => node.disabled), false);
  await page.click('.adm-sms-form button[type="submit"]');
  await page.waitForFunction(() => document.querySelector('.adm-sms-summary strong')?.textContent.trim() === '4');
  const state = await (await fetch(`http://127.0.0.1:${port}/__test__/state`)).json();
  assert.equal(state.reads, 1);
  assert.equal(state.writes.length, 1);
  assert.equal(state.writes[0].body.expected_revision, 3);
  assert.equal(state.writes[0].body.sign_name, 'PINVOU-NEW');
  assert.equal(state.writes[0].csrf, 'csrf-test');
  assert.match(state.writes[0].idempotencyKey, /^sms-configuration-/);

  await fetch(`http://127.0.0.1:${port}/__test__/readonly`);
  await page.reload({ waitUntil: 'networkidle0' });
  await page.waitForSelector('.adm-sms-configuration[data-readiness="READY"]');
  assert.equal(await page.$('.adm-sms-form button[type="submit"]'), null);
  assert.equal(await page.$eval('.adm-sms-fields select', (node) => node.disabled), true);
  assert.equal((await page.$eval('.adm-sms-configuration', (node) => node.textContent)).includes('只有短信配置查看权限'), true);

  await page.setViewport({ width: 390, height: 844, deviceScaleFactor: 1 });
  await page.reload({ waitUntil: 'networkidle0' });
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
  assert.ok(overflow <= 1, `mobile page must not overflow horizontally: ${overflow}`);
  console.log('Admin SMS configuration UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
}
