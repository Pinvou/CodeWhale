import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';

const accountOrigin = process.env.PINVOU_TEST_ACCOUNT_ORIGIN || 'http://127.0.0.1:18101';
const tokenOrigin = process.env.PINVOU_TEST_TOKEN_ORIGIN || 'http://127.0.0.1:18103';
const paymentRequestOrigin = process.env.PINVOU_TEST_PAYMENT_REQUEST_ORIGIN || 'http://127.0.0.1:18104';
const oidcTokenURL = process.env.PINVOU_TEST_OIDC_TOKEN_URL;
const clientID = process.env.PINVOU_TEST_OIDC_CLIENT_ID;
const username = process.env.PINVOU_TEST_OIDC_USERNAME;
const password = process.env.PINVOU_TEST_OIDC_PASSWORD;
for (const [name, value] of Object.entries({ oidcTokenURL, clientID, username, password })) {
  assert.ok(value, `${name} must be provided`);
}

const tokenResponse = await fetch(oidcTokenURL, {
  method: 'POST',
  headers: { 'content-type': 'application/x-www-form-urlencoded' },
  body: new URLSearchParams({ grant_type: 'password', client_id: clientID, username, password }),
  signal: AbortSignal.timeout(15_000),
});
assert.equal(tokenResponse.status, 200, `OIDC token request returned ${tokenResponse.status}`);
const tokenSet = await tokenResponse.json();
assert.ok(tokenSet.access_token);
const claims = JSON.parse(Buffer.from(tokenSet.access_token.split('.')[1], 'base64url').toString('utf8'));
assert.ok(claims.sid || claims.session_state, 'access token must identify its login session');
assert.ok(Array.isArray(claims.aud) ? claims.aud.includes('pinvou-desktop-dev') : claims.aud === 'pinvou-desktop-dev');

const authorization = `Bearer ${tokenSet.access_token}`;
async function requestJSON(origin, path, { method = 'GET', body, headers = {}, statuses = [200] } = {}) {
  const response = await fetch(`${origin}${path}`, {
    method,
    headers: { authorization, ...(body === undefined ? {} : { 'content-type': 'application/json' }), ...headers },
    body: body === undefined ? undefined : JSON.stringify(body),
    redirect: 'manual',
    signal: AbortSignal.timeout(30_000),
  });
  const text = await response.text();
  assert.ok(statuses.includes(response.status), `${method} ${path} returned ${response.status}: ${text.slice(0, 300)}`);
  return { body: text ? JSON.parse(text) : null, headers: response.headers };
}

const { body: me } = await requestJSON(accountOrigin, '/v1/account/bootstrap', { method: 'POST', body: {} });
assert.ok(me.user_id);
assert.equal(me.status, 'ACTIVE');
const { body: organization } = await requestJSON(accountOrigin, '/v1/organizations', {
  method: 'POST', body: { name: `Acceptance ${Date.now()}` }, statuses: [201],
});
assert.ok(organization.organization_id);
const { body: unit } = await requestJSON(accountOrigin, `/v1/organizations/${organization.organization_id}/units`, {
  method: 'POST', body: { name: 'Engineering', parent_id: null }, statuses: [201],
});
assert.ok(unit.unit_id);
await requestJSON(accountOrigin, '/v1/sessions/current/active-context', {
  method: 'POST', body: { context_type: 'ORGANIZATION', organization_id: organization.organization_id },
});
const { body: orgContext } = await requestJSON(accountOrigin, '/v1/me/authorization-context');
assert.equal(orgContext.organization_id, organization.organization_id);
await requestJSON(accountOrigin, '/v1/sessions/current/active-context', {
  method: 'POST', body: { context_type: 'PERSONAL', organization_id: null },
});

const { body: order } = await requestJSON(tokenOrigin, '/v1/recharge/orders', {
  method: 'POST', body: { product_id: 'PLAN_TRIAL', channel: 'ALIPAY' },
  headers: { 'idempotency-key': `auth-acceptance-recharge-${randomUUID()}` }, statuses: [201],
});
const publicPaymentURL = new URL(order.payment_url);
const completeURL = new URL(`${publicPaymentURL.pathname}/complete${publicPaymentURL.search}`, paymentRequestOrigin);
const paymentResponse = await fetch(completeURL, { method: 'POST', signal: AbortSignal.timeout(10_000) });
assert.equal(paymentResponse.status, 200);
for (let attempt = 0; attempt < 40; attempt += 1) {
  const { body: current } = await requestJSON(tokenOrigin, `/v1/recharge/orders/${order.order_id}`);
  if (current.fulfillment_status === 'CREDITED') break;
  assert.ok(attempt < 39, 'authenticated recharge did not converge');
  await new Promise((resolve) => setTimeout(resolve, 250));
}
const { body: balanceBefore } = await requestJSON(tokenOrigin, '/v1/me/balance?consistency=strong');
assert.ok(balanceBefore.available_microcredits > 0);

const chatResponse = await fetch(`${tokenOrigin}/v1/chat/completions`, {
  method: 'POST',
  headers: {
    authorization,
    'content-type': 'application/json',
    'idempotency-key': `auth-acceptance-chat-${randomUUID()}`,
  },
  body: JSON.stringify({ model: 'pinvou-mock', messages: [{ role: 'user', content: 'acceptance' }], max_completion_tokens: 16 }),
  signal: AbortSignal.timeout(30_000),
});
const chatText = await chatResponse.text();
assert.equal(chatResponse.status, 200, chatText.slice(0, 300));
assert.ok(chatResponse.headers.get('x-pinvou-usage-request-id'));
const chat = JSON.parse(chatText);
assert.equal(chat.model, 'pinvou-mock');
const { body: balanceAfter } = await requestJSON(tokenOrigin, '/v1/me/balance?consistency=strong');
assert.ok(balanceAfter.available_microcredits < balanceBefore.available_microcredits);

console.log('authenticated business acceptance passed: OIDC, account, organization, platform route, recharge, model proxy and settlement');
