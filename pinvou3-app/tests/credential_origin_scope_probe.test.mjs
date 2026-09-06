import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import http from 'node:http';
import test from 'node:test';
import puppeteer from 'puppeteer-core';
import { inspectHandoffPage, probeCredentialBrowser } from '../../scripts/account-platform-credential-browser-scope.mjs';
import { handoffBuildEvidence } from '../../scripts/account-platform-credential-origin-probe.mjs';

const config = JSON.parse(await readFile(new URL('../../account-platform/deploy/nginx/credential-edge.example.json', import.meta.url), 'utf8'));
test('browser scope default never launches a browser or requests a page', async () => {
  assert.deepEqual(await probeCredentialBrowser(config), { decision: 'SCOPE_PLAN_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' });
});

test('real Chromium scope gate rejects hostile bytes and passive headers before receiver side effects', { timeout: 90000 }, async t => {
  if (!process.env.CHROME) { t.skip('CHROME is required for the isolated real browser mechanism test'); return; }
  const build = await handoffBuildEvidence();
  const headers = { 'content-type': 'text/html; charset=utf-8', 'cache-control': 'no-store', 'referrer-policy': 'origin', 'cross-origin-opener-policy': 'same-origin', 'x-content-type-options': 'nosniff', 'x-frame-options': 'DENY', 'strict-transport-security': 'max-age=31536000; includeSubDomains', 'content-security-policy': `default-src 'none'; script-src 'sha256-${build.digest}'; form-action ${config.credential_origin}; worker-src 'none'; frame-ancestors 'none'; base-uri 'none'` };
  const valid = Buffer.concat([Buffer.from(`<!doctype html><html><head><meta charset="utf-8"><meta name="referrer" content="origin"><meta name="pinvou-credential-origin" content="${config.credential_origin}"><title>Credential handoff</title></head><body><script>`), build.script, Buffer.from('</script></body></html>')]);
  let body = valid; let extra = {}; let status = 200; let sink = 0; let documents = 0; let upgrades = 0;
  const server = http.createServer((request, response) => {
    if (request.url === '/document') { documents++; response.writeHead(status, { ...headers, ...extra }); response.end(body); return; }
    if (request.url === '/sw.js') { response.writeHead(200, { 'content-type': 'text/javascript', 'Service-Worker-Allowed': '/' }); response.end("self.addEventListener('install',()=>self.skipWaiting());self.addEventListener('activate',event=>event.waitUntil(self.clients.claim()));"); return; }
    if (request.url === '/bootstrap') { response.writeHead(200, { 'content-type': 'text/html' }); response.end('<!doctype html><head><link rel="icon" href="data:,"><title>isolated SW fixture</title></head>'); return; }
    sink++; response.writeHead(204); response.end();
  });
  server.on('upgrade', (_request, socket) => { upgrades++; socket.destroy(); });
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  t.after(async () => { server.closeAllConnections(); await new Promise(resolve => server.close(resolve)); });
  const origin = `http://127.0.0.1:${server.address().port}`;
  const browser = await puppeteer.launch({ executablePath: process.env.CHROME, headless: true, args: ['--disable-gpu', '--no-first-run'] });
  try {
    const load = async () => {
      // Controlled HTTP fixture ONLY; the deployed CLI never injects this loader.
      const response = await fetch(`${origin}/document`, { redirect: 'manual' });
      return { status: response.status, headers: Object.fromEntries(response.headers), body: Buffer.from(await response.arrayBuffer()) };
    };
    const run = async expected => {
      const context = await browser.createBrowserContext();
      try {
        const page = await context.newPage();
        const result = inspectHandoffPage(page, `${config.portal_origin}/credential-handoff`, config.credential_origin, build, load);
        if (expected) {
          const evidence = await result;
          assert.equal(evidence.browser_transport, 'VALIDATED_HTTPS_REPLAY');
          assert.equal(evidence.environment_status, 'ENV_PENDING');
          assert.equal(await page.evaluate(() => typeof window.__maliciousRan), 'undefined');
        } else await assert.rejects(result, /CREDENTIAL_EDGE_BROWSER_DOCUMENT_REJECTED/);
        assert.equal(sink, 0); assert.equal(upgrades, 0);
      } finally { await context.close(); }
    };
    await run(true);
    const before = documents;
    const attacks = [
      `<script src="${origin}/sink"></script>`, `<img src="${origin}/sink">`, `<iframe src="${origin}/sink"></iframe>`,
      `<link rel="preconnect" href="${origin}/sink"><link rel="dns-prefetch" href="//127.0.0.1">`,
      `<meta http-equiv="refresh" content="0;url=${origin}/sink">`,
      `<form method="post" action="${origin}/sink"><input name="private" value="isolated-canary"></form><script>document.forms[0].submit()</script>`,
      `<script>window.__maliciousRan=true;fetch('${origin}/sink',{method:'POST',body:'isolated-canary'});new WebSocket('${origin.replace('http:', 'ws:')}/sink');window.open('${origin}/sink')</script>`,
    ];
    for (const attack of attacks) { body = Buffer.concat([valid, Buffer.from(attack)]); await run(false); }
    body = valid;
    for (const [name, value] of [['link', `<${origin}/sink>; rel=preconnect`], ['refresh', `0;url=${origin}/sink`], ['report-to', `{"group":"x","endpoints":[{"url":"${origin}/sink"}]}`], ['reporting-endpoints', `x="${origin}/sink"`], ['nel', '{"report_to":"x","max_age":3600}'], ['alt-svc', 'h3=":443"'], ['content-security-policy-report-only', `default-src 'none'; report-uri ${origin}/sink`]]) {
      extra = { [name]: value }; await run(false);
    }
    extra = { location: `${origin}/sink` }; status = 302; await run(false);
    assert.equal(documents - before, attacks.length + 8); assert.equal(sink, 0); assert.equal(upgrades, 0);
    // A real existing controller/registration remains visible with page JS off.
    status = 200; extra = {};
    const context = await browser.createBrowserContext();
    try {
      const page = await context.newPage();
      await page.goto(`${origin}/bootstrap`);
      await page.evaluate(async () => { await navigator.serviceWorker.register('/sw.js'); await navigator.serviceWorker.ready; });
      await page.reload();
      assert.equal(await page.evaluate(() => navigator.serviceWorker.controller !== null), true);
      await assert.rejects(inspectHandoffPage(page, `${origin}/credential-handoff`, config.credential_origin, build, load), /CREDENTIAL_EDGE_SERVICE_WORKER_SCOPE_UNSAFE/);
      assert.equal(sink, 0); assert.equal(upgrades, 0);
    } finally { await context.close(); }
  } finally {
    await browser.close();
  }
});
