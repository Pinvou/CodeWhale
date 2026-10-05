import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { authorizationCodeLogin } from './oidc-pkce.mjs';

let stage = 'fixture_configuration';
function loopbackOrigin(raw) {
  const parsed = new URL(raw);
  assert.ok(parsed.protocol === 'http:' && ['127.0.0.1', '[::1]', 'localhost'].includes(parsed.hostname) && !parsed.username && !parsed.password && parsed.pathname === '/' && !parsed.search && !parsed.hash, 'acceptance origins must be plain local test HTTP origins');
  return parsed.origin;
}

async function run() {
  const accountOrigin = loopbackOrigin(process.env.PINVOU_TEST_ACCOUNT_ORIGIN || 'http://127.0.0.1:18101');
  const tokenOrigin = loopbackOrigin(process.env.PINVOU_TEST_TOKEN_ORIGIN || 'http://127.0.0.1:18103');
  const paymentRequestOrigin = loopbackOrigin(process.env.PINVOU_TEST_PAYMENT_REQUEST_ORIGIN || 'http://127.0.0.1:18104');
  const oidcTokenURL = process.env.PINVOU_TEST_OIDC_TOKEN_URL;
  const clientID = process.env.PINVOU_TEST_OIDC_CLIENT_ID;
  const username = process.env.PINVOU_TEST_OIDC_USERNAME;
  const password = process.env.PINVOU_TEST_OIDC_PASSWORD;
  const providerSecret = process.env.MOCK_NEWAPI_KEY;
  for (const [name, value] of Object.entries({ oidcTokenURL, clientID, username, password, providerSecret })) {
    assert.ok(value, `${name} must be provided`);
  }

  stage = 'oidc_pkce_login';
  const tokenSet = await authorizationCodeLogin({ tokenURL: oidcTokenURL, clientID, username, password });
  assert.ok(tokenSet.access_token);
  const claims = JSON.parse(Buffer.from(tokenSet.access_token.split('.')[1], 'base64url').toString('utf8'));
  assert.ok(claims.sid || claims.session_state, 'access token must identify its login session');
  assert.ok(Array.isArray(claims.aud) ? claims.aud.includes(clientID) : claims.aud === clientID);
  assert.ok(claims.realm_access?.roles?.includes('platform-admin'), 'the owned acceptance user requires the platform-admin fixture role');

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
    assert.ok(statuses.includes(response.status), `authenticated business request returned ${response.status}`);
    return { body: text ? JSON.parse(text) : null, headers: response.headers };
  }

  stage = 'account_organization_context';
  const { body: me } = await requestJSON(accountOrigin, '/v1/account/bootstrap', { method: 'POST', body: {} });
  assert.ok(me.user_id);
  assert.equal(me.status, 'ACTIVE');
  const { body: organization } = await requestJSON(accountOrigin, '/admin/v1/organizations', {
    method: 'POST', body: { name: `Acceptance ${Date.now()}`, owner_user_id: me.user_id }, statuses: [201],
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

  stage = 'token_credential_management';
  const { body: tokenAccount } = await requestJSON(tokenOrigin, '/v1/token-accounts', {
    method: 'POST',
    body: { owner_type: 'USER', owner_id: me.user_id, name: 'Acceptance Token', share_scope: 'OWNER_UNIT_ONLY', default_priority: 100 },
    statuses: [201],
  });
  assert.ok(tokenAccount.token_account_id);
  const { body: credential } = await requestJSON(tokenOrigin, `/v1/token-accounts/${tokenAccount.token_account_id}/credentials`, {
    method: 'POST',
    body: { provider: 'NEW_API', base_url: 'http://mock-newapi:8080', secret: providerSecret, allowed_models: ['pinvou-mock'] },
    statuses: [201],
  });
  assert.equal(credential.status, 'ACTIVE');

  stage = 'recharge_callback_credit';
  const { body: order } = await requestJSON(tokenOrigin, '/v1/recharge/orders', {
    method: 'POST', body: { product_id: 'PLAN_TRIAL', channel: 'ALIPAY' },
    headers: { 'idempotency-key': `auth-acceptance-recharge-${randomUUID()}` }, statuses: [201],
  });
  const publicPaymentURL = new URL(order.payment_url);
  const completeURL = new URL(`${publicPaymentURL.pathname}/complete${publicPaymentURL.search}`, paymentRequestOrigin);
  assert.ok(publicPaymentURL.pathname.startsWith('/pay/'), 'fixture payment page path expected');
  const paymentResponse = await fetch(completeURL, { method: 'POST', redirect: 'manual', signal: AbortSignal.timeout(10_000) });
  assert.equal(paymentResponse.status, 200);
  await paymentResponse.arrayBuffer();
  for (let attempt = 0; attempt < 40; attempt += 1) {
    const { body: current } = await requestJSON(tokenOrigin, `/v1/recharge/orders/${order.order_id}`);
    if (current.fulfillment_status === 'CREDITED') break;
    assert.ok(attempt < 39, 'authenticated recharge did not converge');
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  const { body: balanceBefore } = await requestJSON(tokenOrigin, '/v1/me/balance?consistency=strong');
  assert.ok(balanceBefore.available_microcredits > 0);
  const { body: packagesBefore } = await requestJSON(tokenOrigin, '/v1/me/entitlement-packages');
  assert.equal(packagesBefore.items.filter((item) => item.pinvou_order_id === order.order_id).length, 1, 'personal PLAN credit must create exactly one entitlement package');
  assert.ok(BigInt(packagesBefore.summary.available_microcredits) > 0n);

  stage = 'mock_model_priced_settlement';
  const chatResponse = await fetch(`${tokenOrigin}/v1/chat/completions`, {
    method: 'POST',
    headers: {
      authorization,
      'content-type': 'application/json',
      'idempotency-key': `auth-acceptance-chat-${randomUUID()}`,
      'x-pinvou-token-account-id': tokenAccount.token_account_id,
    },
    body: JSON.stringify({ model: 'pinvou-mock', messages: [{ role: 'user', content: 'acceptance' }], max_completion_tokens: 16 }),
    redirect: 'manual',
    signal: AbortSignal.timeout(30_000),
  });
  const chatText = await chatResponse.text();
  assert.equal(chatResponse.status, 200, 'model proxy did not return successful fixture response');
  assert.ok(chatResponse.headers.get('x-pinvou-usage-request-id'));
  const chat = JSON.parse(chatText);
  assert.equal(chat.model, 'pinvou-mock');
  const usageID = chatResponse.headers.get('x-pinvou-usage-request-id');
  const { body: usage } = await requestJSON(tokenOrigin, `/v1/usage-requests/${usageID}`);
  assert.equal(usage.status, 'COMMITTED');
  assert.ok(Number.isSafeInteger(usage.actual_microcredits) && usage.actual_microcredits > 0);
  const { body: balanceAfter } = await requestJSON(tokenOrigin, '/v1/me/balance?consistency=strong');
  assert.equal(balanceBefore.posted_microcredits - balanceAfter.posted_microcredits, usage.actual_microcredits);
  assert.equal(balanceAfter.reserved_microcredits, balanceBefore.reserved_microcredits);

  console.log('authenticated business acceptance passed: OIDC, account, organization, token, recharge, model proxy and settlement');
  console.log('limits: controlled local fixture; mock model output and test pricing; not real model/payment production acceptance');
}

await run().catch(() => {
  // Assertions and network errors may contain private identifiers, response
  // bodies or URLs. Report only a fixed stage; never print the original error.
  console.error(`authenticated business acceptance failed: ${stage}`);
  if (stage === 'mock_model_priced_settlement') console.error('setup: provision the approved pinvou-mock price fixture in the owned test Billing database; missing price is a failure');
  process.exitCode = 1;
});
