// Protocol smoke helper, not a browser/UI acceptance test. All credentials,
// cookies, authorization codes and token responses remain in process memory.
import { createHash, randomBytes } from 'node:crypto';

const fail = (reason) => { throw new Error(reason); };
function localURL(value) {
  const url = new URL(value);
  if (!['http:', 'https:'].includes(url.protocol) || !['127.0.0.1', 'localhost', '[::1]'].includes(url.hostname) || url.username || url.password) fail('OIDC_TEST_REQUIRES_LOOPBACK');
  return url;
}
function decodeHTML(value) {
  return value.replace(/&(?:amp|quot|apos|lt|gt|#\d+|#x[\da-f]+);/gi, (entity) => {
    const named = { '&amp;': '&', '&quot;': '"', '&apos;': "'", '&lt;': '<', '&gt;': '>' };
    if (named[entity.toLowerCase()]) return named[entity.toLowerCase()];
    return String.fromCodePoint(Number.parseInt(entity.slice(entity[2]?.toLowerCase() === 'x' ? 3 : 2, -1), entity[2]?.toLowerCase() === 'x' ? 16 : 10));
  });
}
function attributes(tag) {
  const result = {};
  for (const item of tag.matchAll(/([\w-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')/g)) result[item[1].toLowerCase()] = decodeHTML(item[2] ?? item[3]);
  return result;
}

export async function authorizationCodeLogin({ tokenURL, clientID, username, password }) {
  const tokenEndpoint = localURL(tokenURL);
  if (!tokenEndpoint.pathname.endsWith('/protocol/openid-connect/token')) fail('OIDC_TEST_INVALID_TOKEN_PATH');
  const issuer = tokenEndpoint.origin + tokenEndpoint.pathname.replace('/protocol/openid-connect/token', '');
  const realmPath = new URL(issuer).pathname + '/';
  const callback = new URL('http://127.0.0.1/wave-callback');
  const verifier = randomBytes(32).toString('base64url');
  const state = randomBytes(32).toString('base64url');
  const nonce = randomBytes(32).toString('base64url');
  const cookies = new Map();
  let requests = 0;
  async function request(url, options = {}) {
    const target = localURL(url);
    if (target.origin !== tokenEndpoint.origin || !target.pathname.startsWith(realmPath)) fail('OIDC_TEST_REDIRECT_OR_FORM_OUTSIDE_REALM');
    if (++requests > 20) fail('OIDC_TEST_TOO_MANY_REQUESTS');
    const cookie = [...cookies.values()].filter((item) => target.pathname.startsWith(item.path)).map((item) => `${item.name}=${item.value}`).join('; ');
    const response = await fetch(target, { ...options, headers: { ...(options.headers ?? {}), ...(cookie ? { cookie } : {}) }, redirect: 'manual', signal: AbortSignal.timeout(15_000) });
    for (const raw of response.headers.getSetCookie()) {
      const fields = raw.split(';');
      const separator = fields[0].indexOf('=');
      if (separator < 1) continue;
      const name = fields[0].slice(0, separator);
      const value = fields[0].slice(separator + 1);
      const path = fields.slice(1).find((field) => /^\s*path=/i.test(field))?.trim().slice(5) ?? '/';
      const domain = fields.slice(1).find((field) => /^\s*domain=/i.test(field))?.trim().slice(7).replace(/^\./, '');
      if (domain && domain !== target.hostname) fail('OIDC_TEST_UNEXPECTED_COOKIE_DOMAIN');
      // Keycloak marks cookies Secure on localhost even in HTTP dev mode.
      // This helper sends them only to this exact loopback Keycloak origin.
      const key = `${name}:${path}`;
      if (!value || fields.some((field) => /^\s*max-age=0\s*$/i.test(field))) cookies.delete(key);
      else cookies.set(key, { name, value, path });
    }
    return response;
  }
  const authorization = new URL(issuer + '/protocol/openid-connect/auth');
  authorization.search = new URLSearchParams({ response_type: 'code', client_id: clientID, redirect_uri: callback.href, scope: 'openid profile email', state, nonce, code_challenge_method: 'S256', code_challenge: createHash('sha256').update(verifier).digest('base64url'), prompt: 'login' }).toString();
  let response = await request(authorization);
  let code;
  for (let redirects = 0; redirects < 10; redirects++) {
    if (![302, 303].includes(response.status)) break;
    const location = response.headers.get('location');
    if (!location) fail('OIDC_TEST_REDIRECT_MISSING');
    const target = new URL(location, response.url);
    if (target.origin === callback.origin && target.pathname === callback.pathname) {
      if (target.searchParams.get('state') !== state || target.searchParams.has('error')) fail('OIDC_TEST_CALLBACK_REJECTED');
      code = target.searchParams.get('code');
      break; // Capture the redirect; no listener and no request to port 80.
    }
    await response.arrayBuffer();
    response = await request(target);
  }
  if (!code) {
    if (response.status !== 200) fail('OIDC_TEST_LOGIN_PAGE_FAILED');
    const html = await response.text();
    if (html.length > 2_000_000) fail('OIDC_TEST_LOGIN_PAGE_TOO_LARGE');
    const forms = [...html.matchAll(/<form\b([^>]*)>([\s\S]*?)<\/form>/gi)];
    const form = forms.find((match) => attributes(match[1]).id === 'kc-form-login');
    if (!form) fail('OIDC_TEST_LOGIN_FORM_OR_REQUIRED_ACTION_UNSUPPORTED');
    const attrs = attributes(form[1]);
    if (attrs.method?.toLowerCase() !== 'post' || !attrs.action) fail('OIDC_TEST_INVALID_LOGIN_FORM');
    const fields = new URLSearchParams();
    for (const input of form[2].matchAll(/<input\b([^>]*)>/gi)) {
      const attr = attributes(input[1]);
      if (attr.name && ['hidden', 'submit'].includes(attr.type?.toLowerCase())) fields.set(attr.name, attr.value ?? '');
    }
    fields.set('username', username);
    fields.set('password', password);
    response = await request(new URL(attrs.action, response.url), { method: 'POST', headers: { 'content-type': 'application/x-www-form-urlencoded' }, body: fields });
    for (let redirects = 0; redirects < 10 && [302, 303].includes(response.status); redirects++) {
      const location = response.headers.get('location');
      if (!location) fail('OIDC_TEST_REDIRECT_MISSING');
      const target = new URL(location, response.url);
      if (target.origin === callback.origin && target.pathname === callback.pathname) {
        if (target.searchParams.get('state') !== state || target.searchParams.has('error')) fail('OIDC_TEST_CALLBACK_REJECTED');
        code = target.searchParams.get('code');
        break;
      }
      await response.arrayBuffer();
      response = await request(target);
    }
  }
  if (!code) fail('OIDC_TEST_LOGIN_FORM_OR_REQUIRED_ACTION_UNSUPPORTED');
  await response.arrayBuffer();
  const exchange = await request(tokenEndpoint, { method: 'POST', headers: { 'content-type': 'application/x-www-form-urlencoded' }, body: new URLSearchParams({ grant_type: 'authorization_code', client_id: clientID, redirect_uri: callback.href, code, code_verifier: verifier }) });
  if (exchange.status !== 200) fail('OIDC_TEST_CODE_EXCHANGE_FAILED');
  const tokens = await exchange.json();
  const positiveLifetime = (value) => typeof value === 'number' && Number.isFinite(value) && value > 0;
  if (!tokens.access_token || !tokens.id_token || !tokens.refresh_token || !positiveLifetime(tokens.expires_in) || !positiveLifetime(tokens.refresh_expires_in)) fail('OIDC_TEST_INVALID_TOKEN_RESPONSE');
  const access = JSON.parse(Buffer.from(tokens.access_token.split('.')[1], 'base64url').toString('utf8'));
  const identity = JSON.parse(Buffer.from(tokens.id_token.split('.')[1], 'base64url').toString('utf8'));
  if (access.iss !== issuer || identity.iss !== issuer || identity.nonce !== nonce || identity.sub !== access.sub || access.typ !== 'Bearer' || !(access.sid || access.session_state) || !(Array.isArray(access.aud) ? access.aud.includes(clientID) : access.aud === clientID)) fail('OIDC_TEST_TOKEN_CLAIMS_MISMATCH');
  // Claims here are a smoke assertion; the actual APIs independently verify
  // access-token signatures and authorization. This is not an OIDC verifier.
  return tokens;
}
