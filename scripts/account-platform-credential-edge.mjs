#!/usr/bin/env node
// Offline, closed configuration -> isolated nginx fragments. Never installs them.
import { readFile } from 'node:fs/promises';
import { pathToFileURL } from 'node:url';
import { parseReleaseJSON } from './account-platform-model-token-release-plan.mjs';

export const credentialOriginPairs = Object.freeze({
  CLOUD_UAT: Object.freeze({ portal: 'https://account-uat.pinvou.com', credential: 'https://credentials-uat.pinvou.com' }),
  PRODUCTION: Object.freeze({ portal: 'https://account.pinvou.com', credential: 'https://credentials.pinvou.com' }),
});
export function edgeCheck(value, code) { if (!value) throw new Error(`CREDENTIAL_EDGE_${code}`); }
function fields(value, keys) {
  edgeCheck(value && typeof value === 'object' && !Array.isArray(value) && Object.keys(value).length === keys.length && keys.every(key => Object.hasOwn(value, key)), 'FIELDS_INVALID');
}
const reference = value => typeof value === 'string' && /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/.test(value);
const file = value => typeof value === 'string' && /^\/(?:[A-Za-z0-9_-]+\/)*[A-Za-z0-9_.-]+$/.test(value) && !value.split('/').some(part => part === '.' || part === '..');
export function exactHTTPS(value, endpoint = false) {
  edgeCheck(typeof value === 'string' && /^https:\/\/[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?(?:\/[A-Za-z0-9._~/-]*)?$/.test(value), 'HTTPS_URL_INVALID');
  let parsed; try { parsed = new URL(value); } catch { edgeCheck(false, 'HTTPS_URL_INVALID'); }
  edgeCheck(!parsed.username && !parsed.password && !parsed.port && !parsed.search && !parsed.hash && !parsed.pathname.toLowerCase().includes('userinfo')
    && (!endpoint || parsed.pathname !== '/') && !parsed.pathname.split('/').some(part => part === '.' || part === '..')
    && value === `${parsed.origin}${parsed.pathname === '/' && !value.endsWith('/') ? '' : parsed.pathname}`, 'HTTPS_URL_INVALID');
  return parsed;
}
const identityPermissions = Object.freeze({
  'token-credential-portal': ['internal.authorization.check'],
  'token-command-executor': ['internal.approval-execution.begin', 'internal.approval-execution.complete', 'internal.authorization.check', 'internal.token.catalog-rollback.execute', 'internal.token.credential-revoke.execute', 'internal.token.model-lifecycle.execute'],
});

export function validateCredentialEdge(config) {
  fields(config, ['schema_version', 'environment', 'portal_origin', 'credential_origin', 'tls', 'upstreams', 'oidc', 'workload']);
  const pair = typeof config.environment === 'string' && Object.hasOwn(credentialOriginPairs, config.environment) ? credentialOriginPairs[config.environment] : null;
  edgeCheck(config.schema_version === 1 && pair && config.portal_origin === pair.portal && config.credential_origin === pair.credential, 'ORIGIN_PAIR_INVALID');
  fields(config.tls, ['certificate_file', 'private_key_file', 'access_log_file']);
  edgeCheck(Object.values(config.tls).every(file) && new Set(Object.values(config.tls)).size === 3, 'TLS_PATH_INVALID');
  fields(config.upstreams, ['portal', 'credential']);
  for (const upstream of Object.values(config.upstreams)) {
    fields(upstream, ['url', 'mode', 'ca_file', 'security_reference']);
    edgeCheck(typeof upstream.url === 'string' && reference(upstream.security_reference), 'UPSTREAM_INVALID');
    if (upstream.mode === 'verified_tls') {
      const parsed = exactHTTPS(upstream.url);
      edgeCheck(parsed.pathname === '/' && !upstream.url.endsWith('/') && file(upstream.ca_file), 'UPSTREAM_INVALID');
    } else {
      edgeCheck(['loopback', 'encrypted_tunnel'].includes(upstream.mode) && /^http:\/\/(?:127\.0\.0\.1|\[::1\]):[1-9]\d{0,4}$/.test(upstream.url) && upstream.ca_file === null, 'UPSTREAM_INVALID');
      edgeCheck(Number(upstream.url.slice(upstream.url.lastIndexOf(':') + 1)) <= 65535, 'UPSTREAM_INVALID');
    }
    edgeCheck(upstream.ca_file !== config.tls.access_log_file, 'TLS_PATH_INVALID');
  }
  fields(config.oidc, ['issuer', 'authorization_url', 'token_url', 'jwks_url', 'client_id', 'audience', 'portal_client_id', 'portal_audience']);
  const issuer = exactHTTPS(config.oidc.issuer);
  for (const key of ['authorization_url', 'token_url', 'jwks_url']) edgeCheck(exactHTTPS(config.oidc[key], true).origin === issuer.origin, 'OIDC_ORIGIN_INVALID');
  for (const key of ['client_id', 'audience', 'portal_client_id', 'portal_audience']) edgeCheck(reference(config.oidc[key]), 'OIDC_IDENTITY_INVALID');
  edgeCheck(config.oidc.client_id !== config.oidc.portal_client_id && config.oidc.audience !== config.oidc.portal_audience, 'OIDC_IDENTITY_REUSED');
  fields(config.workload, ['issuer', 'jwks_url', 'audience', 'identities']);
  exactHTTPS(config.workload.issuer); exactHTTPS(config.workload.jwks_url, true);
  edgeCheck(reference(config.workload.audience) && Array.isArray(config.workload.identities) && config.workload.identities.length === 2, 'WORKLOAD_INVALID');
  const seen = new Set(); const subjects = new Set();
  for (const identity of config.workload.identities) {
    fields(identity, ['service', 'subject', 'permissions', 'token_source', 'rotation_reference']);
    edgeCheck(typeof identity.service === 'string' && Object.hasOwn(identityPermissions, identity.service) && !seen.has(identity.service) && reference(identity.subject) && !subjects.has(identity.subject)
      && ['projected', 'rotated_file'].includes(identity.token_source) && reference(identity.rotation_reference), 'WORKLOAD_IDENTITY_INVALID');
    // Preserve the legacy complete permission set during N/N-1 rollout. Only
    // the explicit consumer variant adds this permission; validation does not
    // synthesize grants or widen the credential portal identity.
    const expectedPermissions = identityPermissions[identity.service];
    const exactLegacy = JSON.stringify(identity.permissions) === JSON.stringify(expectedPermissions);
    const exactConsumer = identity.service === 'token-command-executor'
      && JSON.stringify(identity.permissions) === JSON.stringify(['internal.approval-authorization.consume', ...expectedPermissions]);
    edgeCheck(Array.isArray(identity.permissions) && (exactLegacy || exactConsumer), 'WORKLOAD_PERMISSION_INVALID');
    seen.add(identity.service); subjects.add(identity.subject);
  }
  return { decision: 'EDGE_CONFIG_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' };
}

function upstreamDirectives(upstream, host) {
  const verified = upstream.mode === 'verified_tls';
  return `proxy_pass ${upstream.url};
        proxy_http_version 1.1;
        proxy_set_header Host ${host};
        proxy_set_header Forwarded "";
        proxy_set_header X-Forwarded-Host ${host};
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Pinvou-Tunnel-Proto ${upstream.mode === 'encrypted_tunnel' ? 'https' : '""'};
        proxy_set_header X-Forwarded-Port 443;
        proxy_set_header X-Forwarded-For $remote_addr;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Pinvou-Client-Source "";
        proxy_set_header X-Pinvou-Absolute-Deadline "";
        proxy_set_header X-Pinvou-Deadline-Class "";
        proxy_set_header Connection "";
        proxy_redirect off;
        proxy_intercept_errors off;
        proxy_hide_header Strict-Transport-Security;
        proxy_cache off;
        proxy_no_cache 1;
        proxy_cache_bypass 1;
        proxy_request_buffering off;
        proxy_buffering off;
        proxy_max_temp_file_size 0;
        proxy_next_upstream off;
        proxy_connect_timeout 2s;
        proxy_send_timeout 7s;
        proxy_read_timeout 7s;${verified ? `
        proxy_ssl_server_name on;
        proxy_ssl_name ${new URL(upstream.url).hostname};
        proxy_ssl_verify on;
        proxy_ssl_trusted_certificate ${upstream.ca_file};
        proxy_ssl_verify_depth 5;` : ''}`;
}

const edgeErrorStatuses = [400, 403, 404, 405, 408, 411, 413, 414, 415, 429, 500, 501, 502, 503, 504, 505];
function errorRoutes(prefix) {
  return `recursive_error_pages off;\n    ${edgeErrorStatuses.map(status => `error_page ${status} = @${prefix}_${status};`).join('\n    ')}`;
}
function errorLocations(prefix, accessLog) {
  return edgeErrorStatuses.map(status => `location @${prefix}_${status} {
    access_log ${accessLog} pinvou_credential_safe;
    error_log /dev/null crit;
    recursive_error_pages off;
    add_header Strict-Transport-Security "max-age=31536000; includeSubDomains" always;
    add_header Cache-Control no-store always;
    add_header Referrer-Policy no-referrer always;
    add_header Cross-Origin-Opener-Policy same-origin always;
    add_header X-Content-Type-Options nosniff always;
    add_header X-Frame-Options DENY always;
    add_header Content-Security-Policy "default-src 'none'; frame-ancestors 'none'; base-uri 'none'" always;
    default_type application/json;
    return ${status} '{"code":"CREDENTIAL_PROXY_REJECTED"}';
}`).join('\n');
}

export async function renderCredentialEdge(config, section) {
  validateCredentialEdge(config);
  edgeCheck(['http', 'credential', 'handoff'].includes(section), 'SECTION_INVALID');
  const template = await readFile(new URL(`../account-platform/deploy/nginx/credential-${section}.conf.template`, import.meta.url), 'utf8');
  const values = {
    CREDENTIAL_HOST: new URL(config.credential_origin).hostname, PORTAL_HOST: new URL(config.portal_origin).hostname,
    CERTIFICATE: config.tls.certificate_file, PRIVATE_KEY: config.tls.private_key_file, ACCESS_LOG: config.tls.access_log_file,
    CREDENTIAL_UPSTREAM: upstreamDirectives(config.upstreams.credential, new URL(config.credential_origin).hostname),
    PORTAL_UPSTREAM: upstreamDirectives(config.upstreams.portal, new URL(config.portal_origin).hostname),
    CREDENTIAL_ERROR_ROUTES: errorRoutes('pinvou_credential_reject'), HANDOFF_ERROR_ROUTES: errorRoutes('pinvou_handoff_reject'),
    CREDENTIAL_ERRORS: errorLocations('pinvou_credential_reject', config.tls.access_log_file), HANDOFF_ERRORS: errorLocations('pinvou_handoff_reject', config.tls.access_log_file),
  };
  return template.replace(/__([A-Z_]+)__/g, (_, key) => { edgeCheck(Object.hasOwn(values, key), 'TEMPLATE_INVALID'); return values[key]; });
}

async function main(args) {
  edgeCheck(args.length === 2 || args.length === 4 && args[2] === '--render', 'USAGE');
  edgeCheck(args[0] === '--config', 'USAGE');
  const config = parseReleaseJSON(await readFile(args[1], 'utf8'));
  if (args.length === 4) process.stdout.write(await renderCredentialEdge(config, args[3]));
  else process.stdout.write(`${JSON.stringify(validateCredentialEdge(config))}\n`);
}
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) main(process.argv.slice(2)).catch(error => {
  process.stderr.write(`${/^CREDENTIAL_EDGE_[A-Z_]+$/.test(error?.message) ? error.message : 'CREDENTIAL_EDGE_INVALID'}\n`); process.exitCode = 1;
});
