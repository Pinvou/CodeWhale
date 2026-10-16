import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';

// Deliberately absent from default CI. This writes only an explicitly supplied
// disposable loopback stack; no production defaults or credential fallback.
import { acceptanceConfiguration } from './auth-business-configuration.mjs';
import { integerMoney, parseFinanceJSON } from '../../../pinvou3-app/src/features/account-platform/finance-contract.js';
async function main() {
const { accountOrigin, tokenOrigin, paymentRequestOrigin, oidcTokenURL, clientID, audience, username, password, productID, modelID, responseModel } = acceptanceConfiguration(process.env, process.argv.slice(2));

const tokenResponse = await fetch(oidcTokenURL, {
  method: 'POST',
  headers: { 'content-type': 'application/x-www-form-urlencoded' },
  body: new URLSearchParams({ grant_type: 'password', client_id: clientID, username, password }),
  redirect: 'manual',
  signal: AbortSignal.timeout(15_000),
});
assert.equal(tokenResponse.status, 200, `OIDC token request returned ${tokenResponse.status}`);
const tokenSet = await tokenResponse.json();
assert.ok(tokenSet.access_token);
const claims = JSON.parse(Buffer.from(tokenSet.access_token.split('.')[1], 'base64url').toString('utf8'));
assert.ok(claims.sid || claims.session_state, 'access token must identify its login session');
assert.ok(Array.isArray(claims.aud) ? claims.aud.includes(audience) : claims.aud === audience);

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
  assert.ok(statuses.includes(response.status), `${method} acceptance endpoint returned HTTP ${response.status}`);
  return { body: text ? parseFinanceJSON(text) : null, headers: response.headers };
}

const { body: me } = await requestJSON(accountOrigin, '/v1/account/bootstrap', { method: 'POST', body: {} });
assert.ok(me.user_id);
assert.equal(me.status, 'ACTIVE');
assert.equal(me.mobile_verified, true, 'fixture user needs an already verified test mobile');
assert.ok(me.platform_permissions.includes('platform.organizations.create'), 'fixture user must already have organization administrator permission');
const { body: organization } = await requestJSON(accountOrigin, '/admin/v1/organizations', {
  method: 'POST', body: { name: `Acceptance ${Date.now()}`, owner_user_id: me.user_id },
  headers: { 'idempotency-key': `auth-acceptance-organization-${randomUUID()}` }, statuses: [201],
});
assert.ok(organization.organization_id);
const { body: unit } = await requestJSON(accountOrigin, `/v1/organizations/${organization.organization_id}/units`, {
  method: 'POST', body: { name: 'Engineering', parent_id: null }, statuses: [201],
});
assert.ok(unit.unit?.unit_id);
async function switchContext(contextType, organizationID) {
  const { body: current } = await requestJSON(accountOrigin, '/v1/me/authorization-context');
  assert.ok(Number.isSafeInteger(current.context_version) && current.context_version > 0, 'fixture context version must be exact');
  return requestJSON(accountOrigin, '/v1/sessions/current/active-context', {
    method: 'POST', body: { context_type: contextType, organization_id: organizationID, expected_context_version: current.context_version },
  });
}
await switchContext('ORGANIZATION', organization.organization_id);
const { body: orgContext } = await requestJSON(accountOrigin, '/v1/me/authorization-context');
assert.equal(orgContext.organization_id, organization.organization_id);
await switchContext('PERSONAL', null);

const { body: catalog } = await requestJSON(tokenOrigin, '/v1/recharge/products');
assert.ok(catalog.items.some(product => product.product_id === productID && product.availability_status === 'AVAILABLE' && product.channels.includes('ALIPAY')), 'configured fixture product must be available for the installed ALIPAY test channel');
const { body: models } = await requestJSON(tokenOrigin, '/v1/models');
assert.ok(models.items.some(model => model.model_id === modelID && model.context_eligible === true), 'configured fixture model must be available through the installed model facilities');
const { body: order } = await requestJSON(tokenOrigin, '/v1/recharge/orders', {
  method: 'POST', body: { product_id: productID, channel: 'ALIPAY' },
  headers: { 'idempotency-key': `auth-acceptance-recharge-${randomUUID()}` }, statuses: [201],
});
const publicPaymentURL = new URL(order.payment_url);
assert.ok(!publicPaymentURL.username && !publicPaymentURL.password && !publicPaymentURL.hash && publicPaymentURL.pathname.startsWith('/') && !publicPaymentURL.pathname.startsWith('//'), 'fixture payment path must remain local');
const completeURL = new URL(`${publicPaymentURL.pathname}/complete${publicPaymentURL.search}`, paymentRequestOrigin);
const paymentResponse = await fetch(completeURL, { method: 'POST', redirect: 'manual', signal: AbortSignal.timeout(10_000) });
assert.equal(paymentResponse.status, 200);
for (let attempt = 0; attempt < 40; attempt += 1) {
  const { body: current } = await requestJSON(tokenOrigin, `/v1/recharge/orders/${order.order_id}`);
  if (current.fulfillment_status === 'CREDITED') break;
  assert.ok(attempt < 39, 'authenticated recharge did not converge');
  await new Promise((resolve) => setTimeout(resolve, 250));
}
const { body: balanceBefore } = await requestJSON(tokenOrigin, '/v1/me/balance?consistency=strong');
assert.ok(integerMoney(balanceBefore.available_microcredits) > 0n);

const chatResponse = await fetch(`${tokenOrigin}/v1/chat/completions`, {
  method: 'POST',
  headers: {
    authorization,
    'content-type': 'application/json',
    'idempotency-key': `auth-acceptance-chat-${randomUUID()}`,
  },
  body: JSON.stringify({ model: modelID, messages: [{ role: 'user', content: 'acceptance' }], max_completion_tokens: 16 }),
  redirect: 'manual',
  signal: AbortSignal.timeout(30_000),
});
const chatText = await chatResponse.text();
assert.equal(chatResponse.status, 200, 'controlled fixture chat must complete synchronously');
assert.ok(chatResponse.headers.get('x-pinvou-usage-request-id'));
const chat = JSON.parse(chatText);
assert.equal(chat.model, responseModel);
const { body: balanceAfter } = await requestJSON(tokenOrigin, '/v1/me/balance?consistency=strong');
assert.ok(integerMoney(balanceAfter.available_microcredits) < integerMoney(balanceBefore.available_microcredits));

console.log('authenticated business acceptance passed: OIDC, account, organization, platform route, recharge, model proxy and settlement');
}
main().catch(() => {
  console.error('authenticated business acceptance failed; no acceptance result; inspect controlled fixture diagnostics without logging credentials');
  process.exitCode = 1;
});
