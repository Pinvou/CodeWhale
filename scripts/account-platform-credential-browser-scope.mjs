#!/usr/bin/env node
// Structural fresh-context probe only. No web JS, user interaction or handshake.
import { createRequire } from 'node:module';
import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import { isAbsolute } from 'node:path';
import { edgeCheck, validateCredentialEdge } from './account-platform-credential-edge.mjs';
import { parseReleaseJSON } from './account-platform-model-token-release-plan.mjs';
import { handoffBuildEvidence, readPublicHTTPS, validateHandoffResponse } from './account-platform-credential-origin-probe.mjs';

const safeHeaders = ['content-type', 'cache-control', 'content-security-policy', 'referrer-policy', 'cross-origin-opener-policy', 'cross-origin-resource-policy', 'x-content-type-options', 'x-frame-options', 'permissions-policy', 'strict-transport-security'];

// The injectable loader/target supports isolated mechanism tests; the CLI below
// only constructs the frozen target and always uses strict public HTTPS.
export async function inspectHandoffPage(page, target, credentialOrigin, build, loader = readPublicHTTPS) {
  await page.setJavaScriptEnabled(false); // SW is neither disabled nor cleared.
  await page.setRequestInterception(true);
  let failure = ''; let mainRequests = 0;
  const reject = code => { if (!failure) failure = code; };
  page.on('popup', popup => { reject('UNEXPECTED_TARGET'); void popup.close(); });
  page.on('request', request => {
    void (async () => {
      if (request.url() !== target || !['GET', 'HEAD'].includes(request.method()) || !request.isNavigationRequest()
        || request.frame() !== page.mainFrame() || request.redirectChain().length !== 0 || mainRequests !== 0
        || request.headers().cookie || request.headers().authorization) {
        reject('BROWSER_REQUEST_REJECTED'); await request.abort(); return;
      }
      mainRequests++;
      // Pause BEFORE HTML parsing/network hints. No unverified response bytes
      // or reporting/navigation headers are ever given to the browser.
      const response = await loader(target);
      validateHandoffResponse(response, credentialOrigin, build);
      const headers = Object.fromEntries(safeHeaders.filter(key => typeof response.headers[key] === 'string').map(key => [key, response.headers[key]]));
      await request.respond({ status: 200, headers, body: response.body });
    })().catch(async () => {
      reject('BROWSER_DOCUMENT_REJECTED');
      try { if (!request.isInterceptResolutionHandled()) await request.abort(); } catch { /* Page shutdown. */ }
    });
  });
  try {
    await page.goto(target, { waitUntil: 'domcontentloaded', timeout: 15000 });
    edgeCheck(!failure && mainRequests === 1, failure || 'BROWSER_REQUEST_MISSING');
    // Trusted DevTools evaluation is read-only; page scripts remain disabled.
    const state = await page.evaluate(async () => {
      if (!('serviceWorker' in navigator)) return { available: false };
      const registrations = await Promise.race([
        navigator.serviceWorker.getRegistrations(),
        new Promise((_, reject) => setTimeout(() => reject(new Error('SCOPE_QUERY_TIMEOUT')), 5000)),
      ]);
      return { available: true, controlled: navigator.serviceWorker.controller !== null, covered: registrations.some(registration => location.href.startsWith(registration.scope)) };
    });
    edgeCheck(state.available && !state.controlled && !state.covered && !failure, 'SERVICE_WORKER_SCOPE_UNSAFE');
    return { decision: 'SCOPE_PREFLIGHT_PASSED', browser_transport: 'VALIDATED_HTTPS_REPLAY', browser_scope: 'FRESH_CONTEXT_ONLY', page_javascript: 'DISABLED', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED', incomplete: ['NATIVE_BROWSER_TLS_HANDSHAKE', 'AUTHENTICATED_HANDOFF', 'EXISTING_USER_CONTEXTS', 'ALL_LAYER_LOG_SCAN'] };
  } catch (error) {
    edgeCheck(false, failure || (/^CREDENTIAL_EDGE_[A-Z_]+$/.test(error?.message) ? error.message.slice('CREDENTIAL_EDGE_'.length) : 'BROWSER_SCOPE_UNAVAILABLE'));
  }
}

export async function probeCredentialBrowser(config, { probe = false, executablePath } = {}) {
  validateCredentialEdge(config);
  if (!probe) return { decision: 'SCOPE_PLAN_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' };
  edgeCheck(typeof executablePath === 'string' && isAbsolute(executablePath), 'CHROMIUM_REQUIRED');
  const require = createRequire(new URL('../pinvou3-app/package.json', import.meta.url));
  const puppeteer = require('puppeteer-core');
  const browser = await puppeteer.launch({ executablePath, headless: true, timeout: 15000, protocolTimeout: 15000, args: ['--disable-gpu', '--no-first-run'] });
  try {
    const context = await browser.createBrowserContext();
    try {
      const page = await context.newPage();
      return await inspectHandoffPage(page, `${config.portal_origin}/credential-handoff`, config.credential_origin, await handoffBuildEvidence());
    } finally { await context.close(); }
  } finally { await browser.close(); }
}

async function main(args) {
  edgeCheck(args.length === 2 || args.length === 3 && args[2] === '--probe', 'USAGE'); edgeCheck(args[0] === '--config', 'USAGE');
  const config = parseReleaseJSON(await readFile(args[1], 'utf8'));
  process.stdout.write(`${JSON.stringify(await probeCredentialBrowser(config, { probe: args.length === 3, executablePath: process.env.CHROME }))}\n`);
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main(process.argv.slice(2)).catch(error => {
  process.stderr.write(`${/^CREDENTIAL_EDGE_[A-Z_]+$/.test(error?.message) ? error.message : 'CREDENTIAL_EDGE_BROWSER_PREFLIGHT_FAILED'}\n`); process.exitCode = 1;
});
