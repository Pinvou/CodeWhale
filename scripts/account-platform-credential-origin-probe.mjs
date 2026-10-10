#!/usr/bin/env node
// Public, unauthenticated, read-only preflight. This cannot establish deployment GO.
import { readFile } from 'node:fs/promises';
import { Resolver } from 'node:dns/promises';
import https from 'node:https';
import { createHash } from 'node:crypto';
import { pathToFileURL } from 'node:url';
import { parseReleaseJSON } from './account-platform-model-token-release-plan.mjs';
import { edgeCheck, validateCredentialEdge } from './account-platform-credential-edge.mjs';

const hash = bytes => createHash('sha256').update(bytes).digest('hex');
const limited = (promise, cancel, milliseconds, code) => new Promise((resolve, reject) => {
  const timer = setTimeout(() => { cancel(); reject(new Error(`CREDENTIAL_EDGE_${code}`)); }, milliseconds);
  promise.then(value => { clearTimeout(timer); resolve(value); }, error => { clearTimeout(timer); reject(error); });
});

async function publicDNS(host) {
  const resolver = new Resolver();
  try {
    const answers = await limited(Promise.allSettled([resolver.resolve4(host), resolver.resolve6(host)]), () => resolver.cancel(), 5000, 'DNS_TIMEOUT');
    edgeCheck(answers.some(answer => answer.status === 'fulfilled' && answer.value.length > 0), 'DNS_UNAVAILABLE');
  } catch { edgeCheck(false, 'DNS_UNAVAILABLE'); }
}

// Fixed GET, no Cookie/Authorization, no custom CA/resolver/Host, no redirects.
// Absolute timer includes DNS, TLS, headers and full bounded body reception.
export function readPublicHTTPS(target) {
  return new Promise((resolve, reject) => {
    const url = new URL(target);
    if (url.protocol !== 'https:' || url.username || url.password || url.port || url.hash || url.search) {
      reject(new Error('CREDENTIAL_EDGE_PROBE_TARGET_INVALID')); return;
    }
    let timer; let finished = false;
    const finish = (error, value) => {
      if (finished) return; finished = true; clearTimeout(timer);
      if (error) reject(new Error(`CREDENTIAL_EDGE_${error}`)); else resolve(value);
    };
    const request = https.request(url, { method: 'GET', agent: false, rejectUnauthorized: true, maxHeaderSize: 16384, headers: { Accept: '*/*', 'User-Agent': 'Pinvou-Credential-ReadOnly-Preflight/1' } }, response => {
      if (response.statusCode >= 300 && response.statusCode < 400) { response.destroy(); finish('REDIRECT_REJECTED'); return; }
      const certificate = response.socket.getPeerCertificate();
      const expires = Date.parse(certificate.valid_to); const starts = Date.parse(certificate.valid_from);
      if (!response.socket.authorized || !certificate.raw || !Number.isFinite(expires) || !Number.isFinite(starts) || expires <= Date.now() || starts > Date.now()) {
        response.destroy(); finish('TLS_UNVERIFIED'); return;
      }
      const chunks = []; let size = 0;
      response.on('data', chunk => {
        size += chunk.length;
        if (size > 256 * 1024) { response.destroy(); finish('RESPONSE_TOO_LARGE'); }
        else chunks.push(chunk);
      });
      response.on('error', () => finish('HTTP_UNAVAILABLE'));
      response.on('end', () => finish(null, { status: response.statusCode, headers: response.headers, body: Buffer.concat(chunks), certificate_sha256: hash(certificate.raw) }));
    });
    request.on('error', () => finish('HTTP_UNAVAILABLE'));
    timer = setTimeout(() => { request.destroy(); finish('HTTP_TIMEOUT'); }, 5000);
    request.end();
  });
}

function secureHeaders(response, referrer) {
  const h = response.headers;
  edgeCheck(h['cache-control'] === 'no-store' && h['referrer-policy'] === referrer && h['cross-origin-opener-policy'] === 'same-origin'
    && h['x-content-type-options'] === 'nosniff' && h['x-frame-options'] === 'DENY' && !h['set-cookie'], 'RESPONSE_HEADERS_INVALID');
  edgeCheck(typeof h['strict-transport-security'] === 'string' && /^max-age=([1-9]\d*);\s*includeSubDomains(?:;\s*preload)?$/i.test(h['strict-transport-security'])
    && Number(h['strict-transport-security'].match(/^max-age=(\d+)/i)[1]) >= 31536000, 'HSTS_INVALID');
}

export async function handoffBuildEvidence() {
  const script = await readFile(new URL('../account-platform/internal/portal/credential-handoff.v1.js', import.meta.url));
  const generated = await readFile(new URL('../account-platform/internal/portal/credential_handoff_hash_generated.go', import.meta.url), 'utf8');
  const digest = createHash('sha256').update(script).digest('base64');
  edgeCheck(generated.includes(`const credentialHandoffScriptSHA256 = "${digest}"`), 'HANDOFF_BUILD_MISMATCH');
  return { script, digest };
}

export function validateHandoffResponse(response, credentialOrigin, build) {
  secureHeaders(response, 'origin');
  edgeCheck(!['link', 'refresh', 'report-to', 'reporting-endpoints', 'nel', 'alt-svc', 'content-security-policy-report-only'].some(key => Object.hasOwn(response.headers, key)), 'HANDOFF_PASSIVE_CHANNEL_REJECTED');
  edgeCheck(response.status === 200 && response.headers['content-type'] === 'text/html; charset=utf-8', 'HANDOFF_RESPONSE_INVALID');
  const csp = `default-src 'none'; script-src 'sha256-${build.digest}'; form-action ${credentialOrigin}; worker-src 'none'; frame-ancestors 'none'; base-uri 'none'`;
  edgeCheck(response.headers['content-security-policy'] === csp, 'HANDOFF_CSP_INVALID');
  // Validate ALL HTML, not just one script embedded in a malicious surrounding page.
  const expected = Buffer.concat([Buffer.from(`<!doctype html><html><head><meta charset="utf-8"><meta name="referrer" content="origin"><meta name="pinvou-credential-origin" content="${credentialOrigin}"><title>Credential handoff</title></head><body><script>`), build.script, Buffer.from('</script></body></html>')]);
  edgeCheck(response.body.equals(expected), 'HANDOFF_BYTES_INVALID');
}

export function validateCredentialResponse(response, config, asset = false) {
  secureHeaders(response, 'no-referrer');
  const issuerOrigin = new URL(config.oidc.issuer).origin;
  edgeCheck(response.headers['content-security-policy'] === `default-src 'self'; connect-src 'self' ${issuerOrigin}; form-action 'self' ${issuerOrigin}; frame-ancestors 'none'; base-uri 'none'`, 'CREDENTIAL_CSP_INVALID');
  if (asset) edgeCheck(response.status === 200 && response.body.length > 0, 'CREDENTIAL_ASSET_INVALID');
  else {
    edgeCheck(response.status === 401, 'SESSION_REJECTION_INVALID');
    const body = parseReleaseJSON(response.body.toString('utf8'));
    edgeCheck(body.code === 'CREDENTIAL_SESSION_REQUIRED', 'SESSION_REJECTION_INVALID');
  }
}

export function validateCredentialDiscovery(response, config) {
  edgeCheck(response.status === 200, 'OIDC_DISCOVERY_INVALID');
  const value = parseReleaseJSON(response.body.toString('utf8'));
  edgeCheck(value.issuer === config.oidc.issuer && value.authorization_endpoint === config.oidc.authorization_url
    && value.token_endpoint === config.oidc.token_url && value.jwks_uri === config.oidc.jwks_url
    && Array.isArray(value.response_types_supported) && value.response_types_supported.includes('code')
    && Array.isArray(value.code_challenge_methods_supported) && value.code_challenge_methods_supported.includes('S256'), 'OIDC_DISCOVERY_INVALID');
}

export async function probeCredentialOrigins(config, { probe = false, resolve = publicDNS, request = readPublicHTTPS } = {}) {
  validateCredentialEdge(config);
  const evidence = { decision: 'PREFLIGHT_PLAN_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED', config_sha256: hash(JSON.stringify(config)), checks: [] };
  if (!probe) return evidence;
  const build = await handoffBuildEvidence();
  for (const origin of [config.portal_origin, config.credential_origin]) await resolve(new URL(origin).hostname);
  const steps = [
    ['HANDOFF_HTTP', `${config.portal_origin}/credential-handoff`, response => validateHandoffResponse(response, config.credential_origin, build)],
    ['CREDENTIAL_SESSION_READ_ONLY', `${config.credential_origin}/session`, response => validateCredentialResponse(response, config)],
    ...['credential.css', 'credential-auth.js', 'credential-submit.js'].map(asset => ['CREDENTIAL_ASSET_READ_ONLY', `${config.credential_origin}/assets/${asset}`, response => validateCredentialResponse(response, config, true)]),
    ['OIDC_PUBLIC_METADATA', `${config.oidc.issuer.replace(/\/$/, '')}/.well-known/openid-configuration`, response => validateCredentialDiscovery(response, config)],
    ['WORKLOAD_PUBLIC_KEYS', config.workload.jwks_url, response => {
      edgeCheck(response.status === 200, 'WORKLOAD_KEYS_UNAVAILABLE');
      const value = parseReleaseJSON(response.body.toString('utf8'));
      edgeCheck(Array.isArray(value.keys) && value.keys.length > 0 && value.keys.every(key => key && typeof key.kty === 'string' && !['d', 'p', 'q', 'dp', 'dq', 'qi', 'k'].some(field => Object.hasOwn(key, field))), 'WORKLOAD_KEYS_INVALID');
    }],
  ];
  for (const [code, target, validate] of steps) {
    const response = await request(target);
    validate(response);
    edgeCheck(typeof response.certificate_sha256 === 'string' && /^[a-f0-9]{64}$/.test(response.certificate_sha256), 'TLS_UNVERIFIED');
    evidence.checks.push({ code, status: response.status, response_sha256: hash(response.body), certificate_sha256: response.certificate_sha256 });
  }
  return { ...evidence, decision: 'PREFLIGHT_PASSED', incomplete: ['BROWSER_CONTEXT_SCOPE', 'AUTHENTICATED_HANDSHAKE', 'WORKLOAD_AUTHORIZATION_ROTATION', 'ALL_LAYER_LOG_SCAN', 'UPSTREAM_TRUST', 'ENVIRONMENT_RELEASE_GATES'] };
}

async function main(args) {
  edgeCheck(args.length === 2 || args.length === 3 && args[2] === '--probe', 'USAGE');
  edgeCheck(args[0] === '--config', 'USAGE');
  const config = parseReleaseJSON(await readFile(args[1], 'utf8'));
  const result = await probeCredentialOrigins(config, { probe: args.length === 3 });
  process.stdout.write(`${JSON.stringify(result)}\n`);
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main(process.argv.slice(2)).catch(error => {
  process.stderr.write(`${/^CREDENTIAL_EDGE_[A-Z_]+$/.test(error?.message) ? error.message : 'CREDENTIAL_EDGE_PREFLIGHT_FAILED'}\n`); process.exitCode = 1;
});
