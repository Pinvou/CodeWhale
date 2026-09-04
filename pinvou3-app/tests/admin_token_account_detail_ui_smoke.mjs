import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { createServer } from 'node:http';
import { existsSync } from 'node:fs';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import puppeteer from 'puppeteer-core';
import { listenOnChromiumSafePort } from './chromium-safe-port.mjs';

const browserPath = [process.env.CHROME, 'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe', 'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe'].find((candidate) => candidate && existsSync(candidate));
assert.ok(browserPath, 'Chromium or Edge is required');
const staticRoot = path.resolve(import.meta.dirname, '../../account-platform/internal/portal/static');
const handoffSource = await readFile(path.resolve(import.meta.dirname, '../../account-platform/internal/portal/credential-handoff.v1.js'), 'utf8');
const actorID = '0199a300-0001-7000-8000-000000000301';
const accountID = '0199a300-0001-7000-8000-000000000302';
const ownerID = '0199a300-0001-7000-8000-000000000303';
const credentialID = '0199a300-0001-7000-8000-000000000304';
const routeID = '0199a300-0001-7000-8000-000000000305';
const modelID = '0199a300-0001-7000-8000-000000000306';
const protocolID = '0199a300-0001-7000-8000-000000000307';
const provisioningID = '0199a300-0001-7000-8000-000000000308';
const launchID = '0199a300-0001-7000-8000-000000000309';
const resultRef = '0199a300-0001-7000-8000-000000000310';
const commandRef = '0199a300-0001-7000-8000-000000000311';
const approvalID = '0199a300-0001-7000-8000-000000000312';
let credentialOrigin = '';
let portalOrigin = '';
let idpOrigin = '';
const forbidden = ['credential-secret-sentinel', 'ciphertext-sentinel', 'provider-key-sentinel', 'https://private-provider.invalid', 'raw-provider-error-sentinel'];
const fullPermissions = ['platform.tokens.read', 'platform.tokens.manage', 'platform.tokens.credentials.validate', 'platform.tokens.credentials.revoke.request', 'platform.audit.read'];
let permissions = [...fullPermissions];
let detailsEnabled = true;
let actionsEnabled = true;
let credentialDelay = false;
let dropHandoffAccepted = false;
let loseRevokeResponse = false;
const observed = { credentials: 0, credentialAborted: 0, safeDetail: 0, legacyInlineDetail: 0, handoffs: [], launch: [], prechecks: [], commands: [], approvals: [], approvalSubmits: 0, audit: 0, isolationLaunches: 0, isolationReplays: 0, isolationReject: null, credentialReject: null, firstHeartbeats: 0, idpAuthorizations: 0, tokenExchanges: 0, credentialSubmissions: 0, revocations: [] };
let isolationPhase = 'NEW';
let blockCredentialLaunch = false;
let blockedLaunchResponse = null;
let credentialResult = { result_ref: resultRef, token_account_id: accountID, action: 'ROTATE', status: 'PENDING', audit_request_id: 'launch-audit', window_confirmed: false, updated_at: '2026-09-04T12:00:00Z' };

const page = (items) => ({ items, next_cursor: '', has_more: false, limit: 50, as_of: '2026-09-04T12:00:00Z', snapshot_expires_at: '2026-09-04T12:15:00Z', request_id: 'token-detail-smoke' });
const account = { token_account_id: accountID, owner_type: 'USER', owner_id: ownerID, wallet_id: actorID, name: 'Isolated account', status: 'ACTIVE', share_scope: 'OWNER_UNIT_ONLY', management_mode: 'PLATFORM', active_credential_count: 1, route_count: 1, account_version: 2, updated_at: '2026-09-04T11:59:00Z' };
const detail = { ...account, as_of: '2026-09-04T12:00:00Z' };
const credential = { credential_id: credentialID, credential_type: 'NEW_API', fingerprint: '6'.repeat(64), version: 1, status: 'ACTIVE', validation_status: 'VALID', allowed_models: ['flash'], validated_at: '2026-09-04T11:58:00Z', responsible_actor_id: actorID, created_at: '2026-09-01T00:00:00Z', updated_at: '2026-09-04T11:58:00Z' };
const dependencies = Object.fromEntries(['routes', 'healthy_routes', 'provisionings', 'ready_provisionings', 'active_leases', 'inflight_requests', 'active_sessions', 'alternative_credentials', 'alternative_accounts', 'affected_coverage_tuples', 'remaining_ready_coverage', 'coverage_gaps'].map((key) => [key, 0]));

function json(response, body, status = 200) {
  if (response.destroyed || response.writableEnded) return;
  response.writeHead(status, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' });
  response.end(JSON.stringify(body));
}
async function body(request) {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  return JSON.parse(Buffer.concat(chunks).toString('utf8') || '{}');
}
async function formBody(request) {
  const chunks = [];
  for await (const chunk of request) chunks.push(chunk);
  return new URLSearchParams(Buffer.concat(chunks).toString('utf8'));
}
function delay(request, response, value) {
  const timer = setTimeout(() => json(response, value), 5000);
  const aborted = () => { clearTimeout(timer); observed.credentialAborted += 1; };
  request.once('aborted', aborted);
  response.once('close', () => { if (!response.writableEnded) aborted(); });
}
function within(promise, label, milliseconds = 20_000) {
  return Promise.race([promise, new Promise((_, reject) => setTimeout(() => reject(new Error(`${label} timed out`)), milliseconds))]);
}
function precheck(input) {
  return {
    action: input.command_type, blockers: [], token_observed_at: '2026-09-04T12:00:00Z', snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', precheck_fingerprint: 'a'.repeat(64), requires_cas_revalidation: true,
    evidence: { resource_type: 'PROVIDER_CREDENTIAL', resource_id: credentialID, current_status: 'ACTIVE', current_version: { account_version: 2, credential_version: 1, evidence_etag: `"impact-${'b'.repeat(64)}"`, evidence_set_sha256: 'c'.repeat(64) }, dependencies },
    expected_versions: { token_account_id: accountID, expected_account_version: 2, credential_id: credentialID, credential_version: 1 },
  };
}
function revokeImpact() {
  return {
    action: 'TOKEN_CREDENTIAL_REVOKE', blockers: [], token_observed_at: '2026-09-04T12:00:00Z', snapshot_semantics: 'INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS', precheck_fingerprint: 'a'.repeat(64), requires_cas_revalidation: true,
    evidence: { resource_type: 'PROVIDER_CREDENTIAL', resource_id: credentialID, current_status: 'ACTIVE', current_version: { account_version: 2, credential_version: 1, evidence_etag: `"impact-${'b'.repeat(64)}"`, evidence_set_sha256: 'c'.repeat(64) }, dependencies },
    critical_resource_versions: { token_account_id: accountID, token_account_version: 2, credential_id: credentialID, credential_version: 1, credential_status: 'ACTIVE', credential_fingerprint: '1'.repeat(64), active_route_set_fingerprint: '2'.repeat(64), provisioning_lease_set_fingerprint: '3'.repeat(64), inflight_request_set_fingerprint: '4'.repeat(64), replacement_credential_version: null, policy_version: 1 },
    financial_snapshot: {},
  };
}
function approval(input, status, version, actions) {
  return { ...input, approval_request_id: approvalID, requester_user_id: actorID, policy_version: 1, approval_mode: 'APPROVAL_REQUIRED', command_fingerprint: 'e'.repeat(64), status, version, expires_at: '2026-09-05T12:30:00Z', created_at: '2026-09-04T12:00:00Z', updated_at: '2026-09-04T12:00:01Z', allowed_actions: actions };
}

const idpServer = createServer(async (request, response) => {
  const url = new URL(request.url, idpOrigin || 'http://127.0.0.1');
  if (url.pathname === '/authorize') {
    observed.idpAuthorizations += 1;
    assert.equal(url.searchParams.get('prompt'), 'login');
    assert.equal(url.searchParams.get('max_age'), '900');
    assert.equal(url.searchParams.get('response_type'), 'code');
    assert.equal(url.searchParams.get('code_challenge_method'), 'S256');
    const callback = new URL(url.searchParams.get('redirect_uri'));
    callback.searchParams.set('code', 'controlled-code');
    callback.searchParams.set('state', url.searchParams.get('state'));
    response.writeHead(302, { Location: callback.toString(), 'Cache-Control': 'no-store', 'Referrer-Policy': 'no-referrer' });
    response.end();
    return;
  }
  if (url.pathname === '/token' && request.method === 'POST') {
    const form = await formBody(request);
    observed.tokenExchanges += 1;
    assert.equal(form.get('grant_type'), 'authorization_code');
    assert.equal(form.get('code'), 'controlled-code');
    assert.ok(form.get('code_verifier'));
    return json(response, { id_token: 'controlled-id-token', token_type: 'Bearer' });
  }
  response.writeHead(404); response.end('not found');
});

const credentialServer = createServer(async (request, response) => {
  const url = new URL(request.url, credentialOrigin || 'http://127.0.0.1');
  const headers = { 'Cache-Control': 'no-store', 'Referrer-Policy': 'no-referrer', 'Cross-Origin-Opener-Policy': 'same-origin', 'Content-Security-Policy': `default-src 'self'; form-action 'self' ${idpOrigin || "http://127.0.0.1"}; frame-ancestors 'none'; base-uri 'none'` };
  if (url.pathname === '/launch' && request.method === 'POST') {
    const form = await formBody(request);
    if (request.headers.origin !== portalOrigin || form.get('launch_id') !== launchID || [...form.keys()].length !== 1 || isolationPhase !== 'NEW') {
      observed.isolationReplays += 1;
      observed.isolationReject = { origin: request.headers.origin || '', expectedOrigin: portalOrigin, launchMatches: form.get('launch_id') === launchID, fieldCount: [...form.keys()].length, phase: isolationPhase };
      response.writeHead(403, headers); response.end('launch rejected'); return;
    }
    observed.isolationLaunches += 1;
    isolationPhase = 'AUTHENTICATING';
    if (blockCredentialLaunch) { blockedLaunchResponse = response; return; }
    response.writeHead(303, { ...headers, Location: '/session', 'Set-Cookie': ['credential-session=controlled; Path=/; HttpOnly; SameSite=Lax', 'credential-binding=browser-one; Path=/; HttpOnly; SameSite=Lax'] });
    response.end(); return;
  }
  if (url.pathname === '/session' && request.method === 'GET') {
    if (!String(request.headers.cookie || '').includes('credential-session=controlled')) { response.writeHead(401, headers); response.end('missing session'); return; }
    if (isolationPhase === 'AUTHENTICATING') {
      observed.firstHeartbeats += 1;
      credentialResult = { ...credentialResult, window_confirmed: true, updated_at: '2026-09-04T12:00:01Z' };
      const authorize = new URL('/authorize', idpOrigin);
      for (const [name, value] of Object.entries({ client_id: 'credential-client', response_type: 'code', redirect_uri: `${credentialOrigin}/oidc/callback`, scope: 'openid', state: 's'.repeat(64), nonce: 'n'.repeat(64), code_challenge: 'challenge', code_challenge_method: 'S256', prompt: 'login', max_age: '900' })) authorize.searchParams.set(name, value);
      response.writeHead(200, { ...headers, 'Content-Type': 'text/html; charset=utf-8' });
      response.end(`<!doctype html><meta name="referrer" content="no-referrer"><form id="credential-auth" method="get" action="${idpOrigin}/authorize">${[...authorize.searchParams].map(([name, value]) => `<input type="hidden" name="${name}" value="${value}">`).join('')}<noscript><button>Continue authentication</button></noscript></form><script src="/assets/credential-auth.js"></script>`); return;
    }
    if (isolationPhase === 'READY') {
      response.writeHead(200, { ...headers, 'Content-Type': 'text/html; charset=utf-8' });
      response.end(`<!doctype html><meta name="referrer" content="no-referrer"><title>Secure credential enrollment</title><script defer src="/assets/credential-submit.js"></script><form id="credential-form" method="post" action="/credentials" autocomplete="off"><input name="csrf" value="csrf-controlled" type="hidden"><input name="provider" value="NEW_API"><input name="base_url" value="https://provider.example"><input name="allowed_models" value="flash"><input name="secret" type="password" autocomplete="new-password"><button type="submit">Submit securely</button><p id="credential-status" hidden></p></form>`); return;
    }
    response.writeHead(200, { ...headers, 'Content-Type': 'text/html; charset=utf-8' }); response.end('<h1>Credential updated securely</h1>'); return;
  }
  if (url.pathname === '/assets/credential-auth.js' && request.method === 'GET') {
    response.writeHead(200, { ...headers, 'Content-Type': 'text/javascript; charset=utf-8' }); response.end("const form=document.getElementById('credential-auth');if(form){form.requestSubmit()}else{const next=document.querySelector('meta[name=pinvou-credential-next]')?.content;if(next==='/session'){location.replace(next)}}"); return;
  }
  if (url.pathname === '/assets/credential-submit.js' && request.method === 'GET') {
    response.writeHead(200, { ...headers, 'Content-Type': 'text/javascript; charset=utf-8' }); response.end(`(()=>{const form=document.getElementById('credential-form');form.addEventListener('submit',async event=>{event.preventDefault();let payload=new URLSearchParams(new FormData(form));const response=await fetch('/credentials',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:payload,credentials:'same-origin',redirect:'follow',referrerPolicy:'no-referrer'});payload=null;form.querySelector('input[name=secret]').value='';if(response.ok&&new URL(response.url).pathname==='/session')location.replace('/session')})})()`); return;
  }
  if (url.pathname === '/oidc/callback' && request.method === 'GET') {
    if (isolationPhase !== 'AUTHENTICATING' || url.searchParams.get('state') !== 's'.repeat(64) || url.searchParams.get('code') !== 'controlled-code') { response.writeHead(403, headers); response.end('callback rejected'); return; }
    const tokenResponse = await fetch(`${idpOrigin}/token`, { method: 'POST', redirect: 'manual', headers: { 'Content-Type': 'application/x-www-form-urlencoded' }, body: new URLSearchParams({ grant_type: 'authorization_code', client_id: 'credential-client', code: 'controlled-code', redirect_uri: `${credentialOrigin}/oidc/callback`, code_verifier: 'v'.repeat(64) }) });
    assert.equal(tokenResponse.status, 200);
    isolationPhase = 'READY';
    response.writeHead(200, { ...headers, 'Content-Type': 'text/html; charset=utf-8' }); response.end('<!doctype html><meta name="pinvou-credential-next" content="/session"><script src="/assets/credential-auth.js"></script><noscript><a href="/session">Continue</a></noscript>'); return;
  }
  if (url.pathname === '/credentials' && request.method === 'POST') {
    const form = await formBody(request);
    if (request.headers.origin !== credentialOrigin || isolationPhase !== 'READY' || form.get('csrf') !== 'csrf-controlled' || [...form.keys()].sort().join(',') !== 'allowed_models,base_url,csrf,provider,secret') { observed.credentialReject = { origin: request.headers.origin || '', phase: isolationPhase, csrf: form.get('csrf'), keys: [...form.keys()].sort() }; response.writeHead(403, headers); response.end('credential rejected'); return; }
    assert.equal(form.get('secret'), 'credential-secret-sentinel');
    observed.credentialSubmissions += 1;
    isolationPhase = 'COMPLETED';
    credentialResult = { ...credentialResult, status: 'SUCCEEDED', credential_id: credentialID, credential_version: 2, credential_status: 'ACTIVE', validation_status: 'VALID', fingerprint: '7'.repeat(64), window_confirmed: true, updated_at: '2026-09-04T12:00:02Z' };
    response.writeHead(303, { ...headers, Location: '/session' }); response.end(); return;
  }
  response.writeHead(404, headers); response.end('not found');
});

const server = createServer(async (request, response) => {
  const url = new URL(request.url, 'http://127.0.0.1');
  if (url.pathname === '/credential-handoff') {
    observed.handoffs.push({ method: request.method, url: request.url, referer: request.headers.referer || '' });
    if (request.method !== 'GET' || request.url !== '/credential-handoff') { response.writeHead(404); response.end('not found'); return; }
    const deliveredHandoffSource = dropHandoffAccepted
      ? handoffSource.replace("    port.postMessage({ version: VERSION, type: 'HANDOFF_ACCEPTED', nonce });\n", '')
      : handoffSource;
    const deliveredHandoffSHA256 = createHash('sha256').update(deliveredHandoffSource).digest('base64');
    response.writeHead(200, {
      'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store', 'Referrer-Policy': 'origin',
      'Cross-Origin-Opener-Policy': 'same-origin', 'Cross-Origin-Resource-Policy': 'same-origin',
      'Content-Security-Policy': `default-src 'none'; script-src 'sha256-${deliveredHandoffSHA256}'; form-action ${credentialOrigin}; worker-src 'none'; frame-ancestors 'none'; base-uri 'none'`,
      'X-Content-Type-Options': 'nosniff', 'X-Frame-Options': 'DENY',
    });
    response.end(`<!doctype html><html><head><meta charset="utf-8"><meta name="referrer" content="origin"><meta name="pinvou-credential-origin" content="${credentialOrigin}"><title>Credential handoff</title></head><body><script>${deliveredHandoffSource}</script></body></html>`);
    return;
  }
  if (url.pathname === '/__test__/configure') {
    if (url.searchParams.has('gate')) detailsEnabled = url.searchParams.get('gate') === '1';
    if (url.searchParams.has('actions')) actionsEnabled = url.searchParams.get('actions') === '1';
    if (url.searchParams.get('permissions') === 'full') permissions = [...fullPermissions];
    if (url.searchParams.get('permissions') === 'read') permissions = ['platform.tokens.read'];
    if (url.searchParams.get('permissions') === 'none') permissions = [];
    credentialDelay = url.searchParams.get('credential-delay') === '1';
    dropHandoffAccepted = url.searchParams.get('drop-accepted') === '1';
    loseRevokeResponse = url.searchParams.get('lose-revoke') === '1';
    if (url.searchParams.get('outcome-reset') === '1') {
      isolationPhase = 'NEW'; blockCredentialLaunch = false;
      credentialResult = { result_ref: resultRef, token_account_id: accountID, action: 'ROTATE', status: 'PENDING', audit_request_id: 'launch-audit', window_confirmed: false, updated_at: '2026-09-04T12:00:00Z' };
    }
    if (url.searchParams.get('popup-reset') === '1') {
      isolationPhase = 'NEW'; blockCredentialLaunch = true;
      credentialResult = { result_ref: resultRef, token_account_id: accountID, action: 'ROTATE', status: 'PENDING', audit_request_id: 'launch-audit', window_confirmed: false, updated_at: '2026-09-04T12:00:00Z' };
    }
    return json(response, { permissions, detailsEnabled, actionsEnabled, credentialDelay });
  }
  if (url.pathname === '/__test__/state') return json(response, observed);
  if (url.pathname === '/__test__/handoff-sw.js') {
    response.writeHead(200, { 'Content-Type': 'text/javascript; charset=utf-8', 'Cache-Control': 'no-store', 'Service-Worker-Allowed': '/' });
    response.end("self.addEventListener('install',event=>event.waitUntil(self.skipWaiting()));self.addEventListener('activate',event=>event.waitUntil(self.clients.claim()));self.addEventListener('fetch',()=>{});"); return;
  }
  if (url.pathname === '/__test__/replay') {
    response.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8', 'Cache-Control': 'no-store' });
    response.end(`<!doctype html><form method="post" action="${credentialOrigin}/launch"><input name="launch_id" value="${launchID}"><button>Replay</button></form>`); return;
  }
  if (url.pathname === '/api/session') return json(response, { authenticated: true, csrf_token: 'csrf', feature_flags: { admin_model_token_ia: true, admin_token_resource_forms: true, admin_token_details: detailsEnabled, admin_model_token_actions: actionsEnabled }, credential_origin: credentialOrigin });
  if (url.pathname === '/api/account/v1/account/bootstrap') return json(response, { user_id: actorID, platform_permissions: permissions });
  if (url.pathname === '/api/admin/token/admin/v1/token-accounts') return json(response, page([account]));
  if (url.pathname === `/api/admin/token/admin/v1/token-accounts/${accountID}`) { observed.legacyInlineDetail += 1; return json(response, { ...detail, credentials: [{ raw_error: forbidden[4] }], routes: [{ base_url: forbidden[3] }] }); }
  if (url.pathname === `/api/admin/token/admin/v2/token-accounts/${accountID}`) { observed.safeDetail += 1; return json(response, detail); }
  if (url.pathname === `/api/admin/token/admin/v1/token-accounts/${accountID}/credentials`) {
    observed.credentials += 1;
    return credentialDelay ? delay(request, response, page([credential])) : json(response, page([credential]));
  }
  if (url.pathname === `/api/admin/token/admin/v1/token-accounts/${accountID}/routes`) return json(response, page([{ route_id: routeID, token_account_id: accountID, model_id: modelID, protocol_version_id: protocolID, protocol_code: 'CHAT', priority: 100, default_scope: 'NONE', reference_status: 'RESOLVED', health_status: 'HEALTHY', health_diagnostic_code: 'PROBE_SUCCEEDED', health_observed_at: '2026-09-04T11:59:00Z', health_expires_at: '2026-09-04T12:04:00Z', route_version: 1, route_generation: 1, updated_at: '2026-09-04T11:59:00Z' }]));
  if (url.pathname === '/api/admin/token/admin/v1/provisionings') return json(response, page([{ provisioning_id: provisioningID, model_id: modelID, token_account_id: accountID, status: 'READY', stage: 'VERIFIED', desired_generation: 1, actual_generation: 1, updated_at: '2026-09-04T11:59:00Z' }]));
  if (url.pathname === '/api/admin/token/admin/v1/resource-audit') { observed.audit += 1; return json(response, page([{ audit_id: actorID, action: 'TOKEN_CREDENTIAL_ROTATED', resource_type: 'TOKEN_ACCOUNT', resource_id: accountID, actor_id: actorID, summary: { status: 'SUCCEEDED' }, occurred_at: '2026-09-04T12:00:00Z' }])); }
  if (url.pathname === `/api/admin/token/admin/v1/token-accounts/${accountID}/credential-launches/rotate` && request.method === 'POST') {
    const input = await body(request); observed.launch.push({ input, key: request.headers['idempotency-key'] });
    await new Promise((resolve) => setTimeout(resolve, 120));
    return json(response, { launch_id: launchID, result_ref: resultRef, token_account_id: accountID, action: 'ROTATE', status: 'CREATED', expires_at: '2026-09-04T12:05:00Z', audit_request_id: 'launch-audit' });
  }
  if (url.pathname === `/api/admin/token/admin/v1/credential-results/${resultRef}` && request.method === 'GET') return json(response, credentialResult);
  if (url.pathname === `/api/admin/token/admin/v1/credential-results/${resultRef}/revoke-window` && request.method === 'POST') {
    const input = await body(request); observed.revocations.push(input.reason_code);
    if (credentialResult.status === 'PENDING') credentialResult = { ...credentialResult, status: 'REVOKED', error_code: input.reason_code, updated_at: '2026-09-04T12:00:03Z' };
    if (blockedLaunchResponse) { blockedLaunchResponse.writeHead(410, { 'Cache-Control': 'no-store' }); blockedLaunchResponse.end('launch revoked'); blockedLaunchResponse = null; }
    if (loseRevokeResponse) { response.writeHead(504, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store' }); response.end(JSON.stringify({ code: 'COMMAND_RESULT_UNKNOWN' })); return; }
    response.writeHead(204, { 'Cache-Control': 'no-store' }); response.end(); return;
  }
  if (url.pathname === '/api/admin/token/admin/v1/commands:precheck') { const input = await body(request); observed.prechecks.push(input); return json(response, precheck(input)); }
  if (url.pathname === '/api/admin/token/admin/v1/commands') {
    const input = await body(request); observed.commands.push({ input, key: request.headers['idempotency-key'] });
    return json(response, { command_reference: commandRef, command_type: 'TOKEN_CREDENTIAL_VALIDATE', policy_version: 2, actor_user_id: actorID, actor_permission: 'platform.tokens.credentials.validate', authorization_version: 3, idempotency_fingerprint: 'd'.repeat(64), target_resource_type: 'PROVIDER_CREDENTIAL', target_resource_id: credentialID, status: 'SUCCEEDED', redacted_result: { validation_status: 'VALID', credential }, stable_error_code: null, approval_request_id: null, audit_request_id: 'validation-audit', created_at: '2026-09-04T12:00:00Z', completed_at: '2026-09-04T12:00:01Z' });
  }
  if (url.pathname === `/api/admin/token/admin/v1/token-accounts/${accountID}/impact/credential` && url.searchParams.get('action') === 'REVOKE') return json(response, revokeImpact());
  if (url.pathname === '/api/admin/account/admin/v1/approval-requests' && request.method === 'POST') {
    const input = await body(request); observed.approvals.push(input); return json(response, approval(input, 'DRAFT', 1, ['EDIT', 'SUBMIT']), 201);
  }
  if (url.pathname === `/api/admin/account/admin/v1/approval-requests/${approvalID}/actions/submit` && request.method === 'POST') {
    observed.approvalSubmits += 1; return json(response, approval(observed.approvals[0], 'SUBMITTED', 2, ['WITHDRAW']));
  }
  const relative = url.pathname === '/admin.html' ? 'admin.html' : url.pathname.replace(/^\//, '');
  const target = path.resolve(staticRoot, relative);
  if (!target.startsWith(staticRoot) || !existsSync(target)) { response.writeHead(404); response.end('not found'); return; }
  const extension = path.extname(target);
  response.writeHead(200, { 'Content-Type': `${extension === '.html' ? 'text/html' : extension === '.css' ? 'text/css' : 'text/javascript'}; charset=utf-8`, 'Cache-Control': 'no-store', 'Referrer-Policy': 'origin', 'Cross-Origin-Opener-Policy': 'same-origin', 'Cross-Origin-Resource-Policy': 'same-origin' });
  response.end(await readFile(target));
});

({ origin: idpOrigin } = await listenOnChromiumSafePort(idpServer));
({ origin: credentialOrigin } = await listenOnChromiumSafePort(credentialServer));
({ origin: portalOrigin } = await listenOnChromiumSafePort(server));
const origin = portalOrigin;
const browser = await puppeteer.launch({ executablePath: browserPath, headless: 'new', args: ['--no-sandbox', '--disable-gpu', '--no-first-run'] });
try {
  const targetURLs = [];
  browser.on('targetcreated', (target) => targetURLs.push(`created:${target.url()}`));
  browser.on('targetchanged', (target) => targetURLs.push(`changed:${target.url()}`));
  const browserPage = await browser.newPage();
  await browserPage.setViewport({ width: 320, height: 720, deviceScaleFactor: 2 });
  const networkBodies = [];
  const browserLogs = [];
  browserPage.on('request', (request) => { if (request.postData()) networkBodies.push(request.postData()); });
  browserPage.on('response', async (response) => { if (response.url().includes('/api/admin/')) try { networkBodies.push(await response.text()); } catch { /* navigation can dispose response */ } });
  browserPage.on('console', (message) => browserLogs.push(message.text()));
  await browserPage.evaluateOnNewDocument(() => localStorage.setItem('pinvou.admin.language', 'en'));
  const listURL = `${origin}/admin.html?section=tokenResources&token_q=isolated&token_cursor=list-page`;
  await browserPage.goto(listURL, { waitUntil: 'networkidle0' });
  await browserPage.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'View').click());
  await browserPage.waitForSelector('.adm-model-detail');
  assert.equal(new URL(browserPage.url()).searchParams.get('token_account_id'), accountID);
  assert.ok(observed.safeDetail > 0, 'detail workspace uses the versioned safe summary');
  assert.equal(observed.legacyInlineDetail, 0, 'detail workspace never requests the legacy inline detail');
  assert.deepEqual(await browserPage.$$eval('.adm-model-detail-tabs button', (nodes) => nodes.map((node) => node.textContent.trim())), ['Overview', 'Credentials', 'Routes', 'Provisioning', 'Audit']);

  await browserPage.evaluate(() => [...document.querySelectorAll('.adm-model-detail-tabs button')].find((button) => button.textContent.trim() === 'Credentials').click());
  await browserPage.waitForFunction(() => document.body.textContent.includes('666666666666'));
  await browserPage.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Rotate credential securely').click());
  await browserPage.waitForSelector('#credential-launch-title');
  await browserPage.type('#credential-launch-title ~ form textarea, .adm-protocol-dialog textarea', 'rotate after controlled security review');
  const popupPromise = browser.waitForTarget((target) => target.type() === 'page' && target !== browserPage.target() && target.url().startsWith(credentialOrigin));
  await browserPage.evaluate(() => { const button = document.querySelector('.adm-protocol-dialog button[type="submit"]'); button.click(); button.click(); });
  const credentialTarget = await within(popupPromise, 'credential handoff popup').catch((error) => {
    throw new Error(`${error.message}\ntargets=${JSON.stringify(browser.targets().map((target) => ({ type: target.type(), url: target.url() })))}\nevents=${JSON.stringify(targetURLs)}\nstate=${JSON.stringify(observed)}`);
  });
  const credentialPage = await credentialTarget.page();
  assert.equal(observed.launch.length, 1, 'same-tick credential launch submits only once');
  assert.equal(observed.handoffs.length, 1, 'one fixed handoff document is opened');
  assert.equal(observed.handoffs[0].url, '/credential-handoff');
  if (observed.handoffs[0].referer) assert.equal(new URL(observed.handoffs[0].referer).origin, portalOrigin);
  assert.equal(observed.handoffs[0].referer.includes('/admin.html'), false, 'handoff Referer is origin-only');
  assert.equal(browserPage.url().includes(launchID) || browserPage.url().includes(resultRef), false, 'opaque launch references never enter the URL');
  const credentialNetworkURLs = [];
  const credentialLogs = [];
  credentialPage.on('request', (request) => credentialNetworkURLs.push(request.url()));
  credentialPage.on('console', (message) => credentialLogs.push(message.text()));
  await credentialPage.waitForSelector('form[action="/credentials"] input[name="secret"]', { timeout: 8_000 }).catch(async (error) => {
    throw new Error(`${error.message}\ncredential_url=${credentialPage.url()}\ncredential_body=${await credentialPage.content()}\nstate=${JSON.stringify(observed)}`);
  });
  assert.equal(await credentialPage.evaluate(() => window.opener === null), true, 'credential origin must not retain a Portal opener');
  assert.equal(observed.isolationLaunches, 1);
  assert.equal(observed.firstHeartbeats, 1);
  assert.equal(observed.tokenExchanges, 1);
  assert.equal(await credentialPage.evaluate(() => document.referrer), '');
  const launchesBeforeRefresh = observed.isolationLaunches;
  await credentialPage.reload({ waitUntil: 'domcontentloaded' });
  await credentialPage.waitForSelector('form[action="/credentials"] input[name="secret"]');
  assert.equal(observed.isolationLaunches, launchesBeforeRefresh, 'refresh never replays the launch POST');
  await credentialPage.type('input[name="secret"]', 'credential-secret-sentinel');
  await credentialPage.click('form[action="/credentials"] button[type="submit"]');
  await credentialPage.waitForFunction(() => document.body.textContent.includes('Credential updated securely')).catch(async (error) => {
    throw new Error(`${error.message}\ncredential_url=${credentialPage.url()}\ncredential_body=${await credentialPage.content()}\nstate=${JSON.stringify(observed)}`);
  });
  assert.equal(observed.credentialSubmissions, 1);
  await browserPage.waitForSelector('.adm-protocol-dialog .adm-inline-message.success');
  assert.equal(new URL(browserPage.url()).search.includes(launchID) || new URL(browserPage.url()).search.includes(resultRef), false);
  const credentialHistoryAndPerformance = await credentialPage.evaluate(() => ({ href: location.href, historyLength: history.length, resources: performance.getEntries().map((entry) => entry.name), html: document.documentElement.outerHTML }));
  for (const privateValue of [launchID, resultRef, 'credential-secret-sentinel']) {
    assert.equal([credentialHistoryAndPerformance.href, ...credentialHistoryAndPerformance.resources, credentialHistoryAndPerformance.html, ...credentialLogs].some((value) => String(value).includes(privateValue)), false, `credential browser surface leaked ${privateValue}`);
  }
  assert.ok(credentialHistoryAndPerformance.historyLength >= 1);
  const replayContext = await browser.createBrowserContext();
  const replayPage = await replayContext.newPage();
  await replayPage.goto(`${origin}/__test__/replay`, { waitUntil: 'domcontentloaded' });
  const replayNavigation = replayPage.waitForNavigation({ waitUntil: 'domcontentloaded' });
  await replayPage.click('button');
  await replayNavigation;
  assert.equal(observed.isolationReplays, 1, 'a second browser cannot consume the launch');
  await replayContext.close();
  await credentialPage.close();
  await browserPage.click('.adm-protocol-dialog footer button');
  await browserPage.waitForFunction(() => !document.querySelector('.adm-protocol-dialog'));

  // If the handoff submits the body but its ACCEPTED acknowledgement is
  // lost, the production component must revoke exactly once and then recover
  // exclusively through the immutable result reference. A lost revoke
  // response must still converge and must never create/re-submit a launch.
  const launchesBeforeUnknown = observed.launch.length;
  const revokesBeforeUnknown = observed.revocations.length;
  await browserPage.evaluate(() => fetch('/__test__/configure?permissions=full&gate=1&actions=1&outcome-reset=1&drop-accepted=1&lose-revoke=1'));
  await browserPage.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Rotate credential securely').click());
  await browserPage.waitForSelector('#credential-launch-title');
  await browserPage.type('#credential-launch-title ~ form textarea, .adm-protocol-dialog textarea', 'recover an acknowledgement loss without replaying the launch');
  const unknownPopupPromise = browser.waitForTarget((target) => target.type() === 'page' && target !== browserPage.target() && target.url() === `${portalOrigin}/credential-handoff`);
  await browserPage.click('.adm-protocol-dialog button[type="submit"]');
  const unknownPopup = await (await within(unknownPopupPromise, 'unknown credential popup')).page();
  await browserPage.bringToFront();
  await browserPage.waitForFunction(() => document.querySelector('.adm-protocol-dialog .adm-inline-message.error') && !document.querySelector('.adm-protocol-dialog .adm-source-state'), { timeout: 10_000 }).catch(async (error) => {
    throw new Error(`${error.message}\nstate=${JSON.stringify(observed)}\ndialog=${await browserPage.$eval('.adm-protocol-dialog', (node) => node.textContent)}`);
  });
  assert.equal(observed.launch.length, launchesBeforeUnknown + 1, 'ACK loss never re-creates the launch');
  assert.equal(observed.revocations.length, revokesBeforeUnknown + 1, 'ACK loss attempts exactly one revoke');
  assert.equal(observed.revocations.at(-1), 'CREDENTIAL_LAUNCH_CANCELLED');
  await unknownPopup.close().catch(() => {});
  await browserPage.click('.adm-protocol-dialog footer button');
  await browserPage.waitForFunction(() => !document.querySelector('.adm-protocol-dialog'));
  await browserPage.evaluate(() => fetch('/__test__/configure?permissions=full&gate=1&actions=1&drop-accepted=0&lose-revoke=0'));

  await browserPage.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Validate now').click());
  await browserPage.waitForSelector('.adm-high-risk-dialog textarea');
  await browserPage.type('.adm-high-risk-dialog textarea', 'validate stored credential after provider review');
  await browserPage.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled).catch(async (error) => {
    throw new Error(`${error.message}\n${await browserPage.$eval('.adm-high-risk-dialog', (node) => node.textContent)}\n${JSON.stringify(observed)}`);
  });
  await browserPage.click('.adm-high-risk-dialog button[type="submit"]');
  await browserPage.waitForSelector('.adm-high-risk-dialog .adm-inline-message.success');
  assert.equal(observed.commands.length, 1);
  assert.deepEqual(observed.commands[0].input.command, { token_account_id: accountID, credential_id: credentialID });

  await browserPage.click('.adm-high-risk-dialog footer button:first-child');
  await browserPage.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));
  await browserPage.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Request revocation').click());
  await browserPage.waitForSelector('.adm-high-risk-dialog textarea');
  await browserPage.type('.adm-high-risk-dialog textarea', 'revoke after complete dependency review');
  await browserPage.click('.adm-high-risk-dialog input[type="checkbox"]');
  await browserPage.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog button[type="submit"]').disabled).catch(async (error) => {
    throw new Error(`${error.message}\n${await browserPage.$eval('.adm-high-risk-dialog', (node) => node.textContent)}\n${JSON.stringify(observed)}`);
  });
  await browserPage.click('.adm-high-risk-dialog button[type="submit"]');
  await browserPage.waitForSelector('.adm-high-risk-dialog .adm-inline-message.success');
  assert.equal(observed.approvals.length, 1);
  assert.equal(observed.approvalSubmits, 1);
  assert.deepEqual(observed.approvals[0].critical_resource_versions, revokeImpact().critical_resource_versions);
  assert.deepEqual(observed.approvals[0].financial_snapshot, {});
  await browserPage.click('.adm-high-risk-dialog footer button:first-child');
  await browserPage.waitForFunction(() => !document.querySelector('.adm-high-risk-dialog'));

  // A popup that never loads the credential document produces no heartbeat.
  // The Portal must revoke the launch after 10 seconds and must not downgrade
  // to a URL, iframe or same-page secret form.
  await browserPage.evaluate(() => fetch('/__test__/configure?permissions=full&gate=1&actions=1&popup-reset=1'));
  await browserPage.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Rotate credential securely').click());
  await browserPage.waitForSelector('#credential-launch-title');
  await browserPage.type('#credential-launch-title ~ form textarea, .adm-protocol-dialog textarea', 'verify blocked credential window is revoked safely');
  const blockedPopupPromise = browser.waitForTarget((target) => target.type() === 'page' && target !== browserPage.target() && target.url() === `${portalOrigin}/credential-handoff`);
  await browserPage.click('.adm-protocol-dialog button[type="submit"]');
  const blockedPopup = await (await within(blockedPopupPromise, 'blocked credential popup')).page();
  await browserPage.bringToFront();
  await browserPage.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((value) => value.revocations.includes('CREDENTIAL_WINDOW_BLOCKED')), { timeout: 15_000 }).catch((error) => {
    throw new Error(`${error.message}\nstate=${JSON.stringify(observed)}`);
  });
  assert.equal(browserPage.url().includes(launchID) || browserPage.url().includes(resultRef), false);
  await blockedPopup.close().catch(() => {});
  await browserPage.waitForSelector('.adm-protocol-dialog footer button');
  await browserPage.click('.adm-protocol-dialog footer button');
  await browserPage.waitForFunction(() => !document.querySelector('.adm-protocol-dialog'));

  // Popup blocking must fail before a launch is created and must never fall
  // back to a direct cross-origin form or same-page navigation.
  const launchesBeforePopupBlock = observed.launch.length;
  await browserPage.evaluate(() => { globalThis.__credentialOriginalOpen = window.open; window.open = () => null; });
  await browserPage.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Rotate credential securely').click());
  await browserPage.waitForSelector('#credential-launch-title');
  await browserPage.type('#credential-launch-title ~ form textarea, .adm-protocol-dialog textarea', 'verify blocked popup creates no credential launch');
  await browserPage.click('.adm-protocol-dialog button[type="submit"]');
  await browserPage.waitForSelector('.adm-protocol-dialog .adm-inline-message.error');
  assert.equal(observed.launch.length, launchesBeforePopupBlock);
  assert.equal(await browserPage.$$eval(`form[action^="${credentialOrigin}"]`, (nodes) => nodes.length), 0);
  await browserPage.click('.adm-protocol-dialog footer button:first-child');
  await browserPage.evaluate(() => { window.open = globalThis.__credentialOriginalOpen; delete globalThis.__credentialOriginalOpen; });
  await browserPage.waitForFunction(() => !document.querySelector('.adm-protocol-dialog'));

  // A Service Worker scope covering the fixed handoff route fails closed
  // during the production READY check and still creates no launch.
  await browserPage.evaluate(async () => { const registration = await navigator.serviceWorker.register('/__test__/handoff-sw.js', { scope: '/' }); await registration.update(); });
  const launchesBeforeServiceWorker = observed.launch.length;
  await browserPage.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Rotate credential securely').click());
  await browserPage.waitForSelector('#credential-launch-title');
  await browserPage.type('#credential-launch-title ~ form textarea, .adm-protocol-dialog textarea', 'verify service worker scope blocks credential handoff');
  await browserPage.click('.adm-protocol-dialog button[type="submit"]');
  await browserPage.waitForSelector('.adm-protocol-dialog .adm-inline-message.error', { timeout: 8_000 });
  assert.equal(observed.launch.length, launchesBeforeServiceWorker);
  await browserPage.click('.adm-protocol-dialog footer button:first-child');
  await browserPage.evaluate(async () => { for (const registration of await navigator.serviceWorker.getRegistrations()) await registration.unregister(); });
  await browserPage.waitForFunction(() => !document.querySelector('.adm-protocol-dialog'));

  // The handoff rejects an opener message with an extra field and closes;
  // the malformed attempt has no API write because no BOUND ACK exists.
  const launchesBeforeMalformed = observed.launch.length;
  await browserPage.evaluate(() => {
    const button = document.createElement('button'); button.id = 'malformed-handoff-test'; button.type = 'button'; button.hidden = false;
    globalThis.__malformedHandoff = new Promise((resolve) => {
      let popup = null;
      const listener = (event) => {
        if (event.source !== popup || event.origin !== location.origin || event.data?.type !== 'HANDOFF_READY') return;
        removeEventListener('message', listener);
        popup.postMessage({ version: 1, type: 'HANDOFF_BIND', nonce: 'a'.repeat(43), extra: true }, location.origin);
        const poll = setInterval(() => { if (popup.closed) { clearInterval(poll); resolve(true); } }, 50);
        setTimeout(() => { clearInterval(poll); resolve(popup.closed); }, 4_000);
      };
      addEventListener('message', listener);
      button.onclick = () => { popup = window.open('/credential-handoff', '_blank'); };
    });
    document.body.append(button);
  });
  await browserPage.click('#malformed-handoff-test');
  assert.equal(await browserPage.evaluate(() => globalThis.__malformedHandoff), true, 'malformed handoff is closed');
  assert.equal(observed.launch.length, launchesBeforeMalformed);
  await browserPage.evaluate(() => { document.getElementById('malformed-handoff-test')?.remove(); delete globalThis.__malformedHandoff; });

  await browserPage.evaluate(() => [...document.querySelectorAll('button')].find((button) => button.textContent.trim() === 'Enroll credential securely' || button.textContent.trim() === 'Rotate credential securely').click());
  await browserPage.waitForSelector('#credential-launch-title');
  await browserPage.evaluate(() => fetch('/__test__/configure?actions=0&permissions=full'));
  await browserPage.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
  await browserPage.waitForFunction(() => !document.querySelector('#credential-launch-title'));
  const writesAfterGate = observed.launch.length + observed.commands.length;
  await browserPage.evaluate(() => fetch('/__test__/configure?actions=1&permissions=full'));
  await browserPage.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
  await new Promise((resolve) => setTimeout(resolve, 80));
  assert.equal(await browserPage.$$eval('#credential-launch-title,.adm-high-risk-dialog', (nodes) => nodes.length), 0, 'restoring permission never revives a credential action');
  assert.equal(observed.launch.length + observed.commands.length, writesAfterGate);

  await browserPage.evaluate(() => fetch('/__test__/configure?permissions=full&gate=1&actions=1&credential-delay=1'));
  const beforeDelayed = observed.credentials;
  await browserPage.goto(`${origin}/admin.html?section=tokenResources&token_account_id=${accountID}&token_detail_tab=credentials&token_credential_metadata_status=ACTIVE`, { waitUntil: 'domcontentloaded' });
  await browserPage.waitForFunction((minimum) => fetch('/__test__/state').then((response) => response.json()).then((value) => value.credentials > minimum), {}, beforeDelayed);
  await browserPage.evaluate(() => fetch('/__test__/configure?permissions=none&gate=1&actions=1'));
  await browserPage.evaluate(() => globalThis.dispatchEvent(new Event('focus')));
  await browserPage.waitForFunction(() => !new URL(location.href).searchParams.has('token_account_id'));
  await browserPage.waitForFunction(() => fetch('/__test__/state').then((response) => response.json()).then((value) => value.credentialAborted > 0));
  assert.equal(await browserPage.$$eval('.adm-model-detail', (nodes) => nodes.length), 0);
  assert.equal(new URL(browserPage.url()).searchParams.has('token_credential_metadata_status'), false);

  const visibleAndTransport = [await browserPage.$eval('html', (node) => node.outerHTML), browserPage.url(), ...networkBodies, ...browserLogs, JSON.stringify(observed)];
  for (const secret of forbidden) assert.equal(visibleAndTransport.some((value) => value.includes(secret)), false, `sensitive value leaked: ${secret}`);
  console.log('Admin Token Account detail UI smoke passed');
} finally {
  await browser.close();
  await new Promise((resolve) => server.close(resolve));
  await new Promise((resolve) => credentialServer.close(resolve));
  await new Promise((resolve) => idpServer.close(resolve));
}
