import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { credentialOriginPairs, renderCredentialEdge, validateCredentialEdge } from '../../../scripts/account-platform-credential-edge.mjs';

const baseline = JSON.parse(await readFile(new URL('../../deploy/nginx/credential-edge.example.json', import.meta.url), 'utf8'));
const copy = () => structuredClone(baseline);
test('credential edge accepts only frozen pairs and never claims environment verification', () => {
  for (const [environment, pair] of Object.entries(credentialOriginPairs)) {
    const config = { ...copy(), environment, portal_origin: pair.portal, credential_origin: pair.credential };
    assert.deepEqual(validateCredentialEdge(config), { decision: 'EDGE_CONFIG_VALID', environment_status: 'ENV_PENDING', deployment_status: 'NOT_DEPLOYED' });
  }
  for (const mutate of [c => c.environment = 'development', c => c.environment = ['CLOUD_UAT'], c => c.portal_origin = credentialOriginPairs.PRODUCTION.portal, c => c.credential_origin += '/', c => c.extra = true, c => c.tls.private_key = 'secret']) {
    const config = copy(); mutate(config); assert.throws(() => validateCredentialEdge(config), /^Error: CREDENTIAL_EDGE_/);
  }
});
test('credential edge rejects OIDC URL coercion, port aliases and identity permission widening', () => {
  for (const field of ['issuer', 'authorization_url', 'token_url', 'jwks_url']) {
    for (const value of [null, [], {}, true, 443, 'http://identity.example.invalid/path', 'https://identity.example.invalid:443/path', 'https://user@identity.example.invalid/path', 'https://identity.example.invalid/path?x=1', 'https://identity.example.invalid/path#x', 'https://identity.example.invalid/a/../path', 'https://identity.example.invalid/%2fpath']) {
      const config = copy(); config.oidc[field] = value; assert.throws(() => validateCredentialEdge(config), /^Error: CREDENTIAL_EDGE_/);
    }
  }
  for (const mutate of [c => c.oidc.jwks_url = 'https://other.example.invalid/jwks', c => c.oidc.client_id = c.oidc.portal_client_id, c => c.oidc.audience = c.oidc.portal_audience, c => c.workload.identities[0].service = ['token-credential-portal'], c => c.workload.identities[0].permissions.push('internal.*'), c => c.workload.identities[1].subject = c.workload.identities[0].subject, c => c.workload.identities.pop(), c => c.workload.identities[0].token_source = 'shared_token']) {
    const config = copy(); mutate(config); assert.throws(() => validateCredentialEdge(config), /^Error: CREDENTIAL_EDGE_/);
  }
});
test('credential edge blocks nginx injection and untrusted plaintext upstream', async () => {
  for (const value of ['/tmp/a;include /tmp/evil', '/tmp/a\nserver{}', '/tmp/../key', 'relative.pem', '/tmp/$host', '/tmp/a b']) {
    const config = copy(); config.tls.certificate_file = value; assert.throws(() => validateCredentialEdge(config), /^Error: CREDENTIAL_EDGE_/);
  }
  for (const value of ['http://example.invalid:8080', 'http://127.0.0.1:65536', 'http://127.0.0.1:18108/path', 'http://127.0.0.1:18108?x=y', 'http://127.0.0.1:18108;include']) {
    const config = copy(); config.upstreams.credential.url = value; assert.throws(() => validateCredentialEdge(config), /^Error: CREDENTIAL_EDGE_/);
  }
  const config = copy(); config.upstreams.credential = { url: 'https://upstream.example.invalid', mode: 'verified_tls', ca_file: '/controlled/ca.pem', security_reference: 'pending-proof' };
  const rendered = await renderCredentialEdge(config, 'credential');
  assert.match(rendered, /proxy_ssl_verify on;/); assert.match(rendered, /proxy_ssl_name upstream\.example\.invalid;/);
  assert.match(rendered, /proxy_ssl_trusted_certificate \/controlled\/ca.pem;/);
  assert.match(rendered, /proxy_set_header X-Pinvou-Tunnel-Proto "";/);
  const tunneled = copy(); tunneled.upstreams.credential.mode = 'encrypted_tunnel';
  assert.match(await renderCredentialEdge(tunneled, 'credential'), /proxy_set_header X-Pinvou-Tunnel-Proto https;/);
});
test('credential nginx keeps staged app headers and safe logs, fixed methods and no write retries', async () => {
  const credential = await renderCredentialEdge(copy(), 'credential');
  const handoff = await renderCredentialEdge(copy(), 'handoff');
  const http = await renderCredentialEdge(copy(), 'http');
  assert.doesNotMatch(credential + handoff + http, /__[A-Z_]+__|\$(?:request_body|http_cookie|http_authorization|http_referer)|proxy_set_header (?:Origin|Referer|Sec-Fetch)|proxy_hide_header (?:Content-Security-Policy|Referrer-Policy)|Access-Control-Allow/);
  assert.doesNotMatch(http, /\$request_uri/); // Raw URI is used only for rejection, never in logs.
  assert.match(http, /"route":"credential-security"/);
  assert.match(credential, /error_log \/dev\/null crit;/);
  assert.match(credential, /proxy_request_buffering off;/);
  assert.match(credential, /proxy_max_temp_file_size 0;/);
  assert.match(credential, /proxy_next_upstream off;/);
  assert.match(credential, /location = \/launch \{\s+if \(\$request_method != POST\)/);
  assert.match(credential, /location = \/session \{\s+if \(\$request_method != GET\)/);
  assert.match(handoff, /location = \/credential-handoff/);
  const successfulHandoff = handoff.slice(handoff.indexOf('location = /credential-handoff'), handoff.indexOf('location ^~'));
  assert.doesNotMatch(successfulHandoff, /script-src|add_header Cross-Origin-Opener-Policy|add_header Referrer-Policy/);
  assert.match(successfulHandoff, /error_page 404 = @pinvou_handoff_reject_404;/);
  assert.match(handoff, /location @pinvou_handoff_reject_404[\s\S]+add_header Cache-Control no-store always;/);
  assert.match(credential, /error_page 404 = @pinvou_credential_reject_404;/);
  for (const source of [credential, handoff]) {
    assert.match(source, /proxy_hide_header Strict-Transport-Security;/);
    assert.match(source, /proxy_intercept_errors off;/);
    for (const status of [400, 404, 413, 502, 504]) {
      assert.match(source, new RegExp(`error_page ${status} = @pinvou_(?:credential|handoff)_reject_${status};`));
      assert.match(source, new RegExp(`return ${status} '\\{"code":"CREDENTIAL_PROXY_REJECTED"\\}';`));
    }
  }
  assert.match(credential, /if \(\$request_uri != "\/launch"\)/);
  assert.match(successfulHandoff, /if \(\$request_uri != "\/credential-handoff"\)/);
  const existing = await readFile(new URL('../../deploy/nginx/account-platform.conf.template', import.meta.url), 'utf8');
  assert.match(existing, /location = \/api\/v1\/payment-callback\/token/);
  assert.match(existing, /location = \/api\/v1\/payment-callbacks/);
});
