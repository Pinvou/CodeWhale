import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import test from 'node:test';
import Ajv2020 from 'ajv/dist/2020.js';
import addFormats from 'ajv-formats';
import YAML from 'yaml';

const root = path.resolve(import.meta.dirname, '../..');

test('Billing and Token contracts match production JSON and HTTP validation responses', async () => {
  const result = spawnSync(process.env.PINVOU_ACCOUNT_GO_EXECUTABLE || (process.platform === 'win32' ? 'go.exe' : 'go'),
    ['run', '-mod=readonly', './test/contract/billing-fixtures'],
    { cwd: root, encoding: 'utf8', timeout: 120000 });
  assert.ifError(result.error);
  assert.equal(result.status, 0, result.stderr);
  const fixtures = JSON.parse(result.stdout);
  const document = YAML.parse(await readFile(path.join(root, 'contracts/openapi/billing.yaml'), 'utf8'));
  // Conditional required fields are defined by the enclosing OpenAPI schema.
  const ajv = new Ajv2020({ strict: true, strictRequired: false, allErrors: true });
  // OpenAPI's singular example is an annotation, not a validation constraint.
  ajv.addKeyword({ keyword: 'example', valid: true });
  addFormats(ajv);
  ajv.addFormat('int64', { type: 'number', validate: Number.isInteger });
  const schemas = JSON.parse(JSON.stringify(document.components.schemas).replaceAll('#/components/schemas/', '#/$defs/'));
  ajv.addSchema({ $id: 'urn:pinvou:billing-runtime', $defs: schemas });
  for (const [name, schema] of [['product', 'RechargeProduct'], ['paid', 'RechargeOrder'], ['created', 'RechargeOrder'], ['wallet', 'WalletBalance'], ['recent_created', 'RecentRechargeOrder'], ['recent_empty', 'RecentRechargeOrder']]) {
    const validate = ajv.compile({ $ref: `urn:pinvou:billing-runtime#/$defs/${schema}` });
    assert.ok(validate(fixtures[name]), `${name}: ${JSON.stringify(validate.errors)}`);
    assert.equal(validate({ ...fixtures[name], unexpected: true }), false, 'unknown response fields remain forbidden');
  }
  for (const [operation, template] of [['post', '/recharge/orders'], ['get', '/recharge/orders/{order_id}'], ['get', '/recharge/orders/recent']]) {
    const item = document.paths[template];
    const base = (item.servers ?? document.servers)[0].url;
    const route = `${operation.toUpperCase()} ${base}${template.replace('{order_id}', fixtures.paid.order_id)}`;
    assert.equal(fixtures.routes[route], 400, `${route} must reach production request validation`);
  }
  assert.ok(document.paths['/recharge/orders'].post.responses['200']);
  assert.ok(document.paths['/recharge/orders/{order_id}'].get.parameters.some((p) => p.name === 'requester_id' && p.in === 'query' && p.required));
  const request = ajv.compile({ $ref: 'urn:pinvou:billing-runtime#/$defs/CreateRechargeOrderRequest' });
  const valid = { wallet_id: fixtures.wallet.wallet_id, requester_id: fixtures.paid.requester_id, product_id: 'CNY_1', channel: 'ALIPAY' };
  assert.ok(request(valid), JSON.stringify(request.errors));
  assert.equal(request({ ...valid, requester_id: undefined, requester_user_id: valid.requester_id }), false);

  const waiver = ajv.compile({ $ref: 'urn:pinvou:billing-runtime#/$defs/UsageWaiverAuthority' });
  assert.ok(waiver(fixtures.waiver_authority), JSON.stringify(waiver.errors));
  assert.equal(fixtures.waiver_authority.hold.max_microcredits, '9007199254740993');
  assert.equal(fixtures.waiver_authority.hold.actual_microcredits, '0');
  for (const field of ['usage_request_id', 'wallet_id', 'actual_microcredits']) {
    const malformed = structuredClone(fixtures.waiver_authority);
    delete malformed.hold[field];
    assert.equal(waiver(malformed), false, `waiver authority requires ${field}`);
  }
  const numeric = structuredClone(fixtures.waiver_authority);
  numeric.hold.actual_microcredits = 0;
  assert.equal(waiver(numeric), false, 'waiver keeps the exact financial wire');
  const intent = ajv.compile({ $ref: 'urn:pinvou:billing-runtime#/$defs/UsageWaiverIntent' });
  assert.ok(intent(fixtures.waiver_intent));
  assert.equal(intent({ ...fixtures.waiver_intent, usage_request_id: undefined }), false);

  const token = YAML.parse(await readFile(path.join(root, 'contracts/openapi/token-gateway.yaml'), 'utf8'));
  const catalog = ajv.compile(token.paths['/models'].get.responses['200'].content['application/json'].schema);
  assert.ok(catalog(fixtures.model_catalog), JSON.stringify(catalog.errors));
  assert.deepEqual(fixtures.model_catalog, { data: [{ id: 'fixture-model' }] });
  assert.equal(catalog({ ...fixtures.model_catalog, account_id: 'unexpected' }), false);
  assert.equal(catalog({ data: [{ id: 'fixture-model', credential: 'unexpected' }] }), false);
  assert.ok(catalog({ data: [] }), 'an empty authorized catalog remains valid');

  assert.deepEqual(token.paths['/internal/v1/usage-waivers'].post.security, [{ serviceToken: [] }]);
  assert.deepEqual(token.paths['/internal/v1/usage-waivers'].servers, [{ url: '/' }]);
  const common = YAML.parse(await readFile(path.join(root, 'contracts/openapi/common.yaml'), 'utf8'));
  const tokenSchemas = JSON.parse(JSON.stringify(token.components.schemas).replaceAll('#/components/schemas/', '#/$defs/'));
  ajv.addSchema({ $id: 'urn:pinvou:token-runtime', $defs: tokenSchemas });
  const eligibleSchema = JSON.parse(JSON.stringify(token.paths['/token-accounts/eligible'].get.responses['200'].content['application/json'].schema)
    .replaceAll('#/components/schemas/', 'urn:pinvou:token-runtime#/$defs/'));
  const eligible = ajv.compile(eligibleSchema);
  assert.ok(eligible(fixtures.eligible), JSON.stringify(eligible.errors));
  assert.equal(fixtures.eligible.membership_version, 7);
  assert.equal(fixtures.eligible.items.length, 1);
  assert.equal(eligible({ ...fixtures.eligible, membership_version: undefined }), false);
  const error = ajv.compile(common.components.schemas.Error);
  for (const [route, response] of Object.entries(fixtures.mobile_errors)) {
    assert.equal(response.code, route === 'profile_not_found' ? 'PROFILE_CHANGE_NOT_FOUND' : 'INVALID_REQUEST');
    assert.ok(error(response), JSON.stringify(error.errors));
  }
  assert.equal(fixtures.rate_limited.code, 'RATE_LIMITED');
  assert.ok(error(fixtures.rate_limited), JSON.stringify(error.errors));
  for (const [code, response] of Object.entries(fixtures.token_errors)) {
    assert.equal(response.code, code);
    assert.ok(error(response), `${code}: ${JSON.stringify(error.errors)}`);
  }
  assert.equal(fixtures.token_errors.MODEL_CATALOG_UNAVAILABLE.code, 'MODEL_CATALOG_UNAVAILABLE');
  assert.ok(error(fixtures.token_errors.MODEL_CATALOG_UNAVAILABLE), JSON.stringify(error.errors));
  const admin = YAML.parse(await readFile(path.join(root, 'contracts/openapi/admin.yaml'), 'utf8'));
  const adminSchemas = JSON.parse(JSON.stringify(admin.components.schemas).replaceAll('#/components/schemas/', '#/$defs/'));
  ajv.addSchema({ $id: 'urn:pinvou:admin-runtime', $defs: adminSchemas });
  const publicWaiverSchema = JSON.parse(JSON.stringify(admin.paths['/billing/admin/v1/usage-holds/{hold_id}/release'].post.responses['200'].content['application/json'].schema)
    .replaceAll('billing.yaml#/components/schemas/', 'urn:pinvou:billing-runtime#/$defs/'));
  const publicWaiver = ajv.compile(publicWaiverSchema);
  assert.ok(publicWaiver(fixtures.waiver_public), JSON.stringify(publicWaiver.errors));
  assert.equal(publicWaiver({ ...fixtures.waiver_public, status: 'COMMITTED', actual_microcredits: '7' }), false, 'a charged authority is never public waiver success');
  assert.equal(publicWaiver({ ...fixtures.waiver_public, actual_microcredits: '1' }), false);
  const financialSchemas = [
    ['billing/admin/v1/overview', 'BillingOverview', ['posted_microcredits', 'reserved_microcredits']],
    ['billing/admin/v1/wallets', 'WalletCollection', ['posted_microcredits', 'reserved_microcredits', 'available_microcredits']],
    ['billing/admin/v1/payment-orders', 'PaymentOrderCollection', ['amount_fen', 'credit_microcredits']],
    ['billing/admin/v1/adjustments', 'AdjustmentCollection', ['amount_microcredits']],
    ['billing/admin/v1/usage-holds', 'HoldCollection', ['max_microcredits', 'actual_microcredits']],
    ['billing/admin/v1/pricing-versions', 'PriceVersionCollection', ['input_per_million', 'output_per_million', 'cached_input_per_million']],
    ['billing/admin/v1/wallets/fixture/ledger', 'LedgerCollection', ['delta_microcredits']],
    ['token/admin/v1/usage-requests', 'UsageCollection', ['maximum_microcredits', 'actual_microcredits']],
  ];
  for (const [route, schema, fields] of financialSchemas) {
    const validate = ajv.compile({ $ref: `urn:pinvou:admin-runtime#/$defs/${schema}` });
    const response = fixtures.financial_reads[route];
    assert.ok(validate(response), `${route}: ${JSON.stringify(validate.errors)}`);
    const item = response.items?.[0] ?? response;
    for (const field of fields) {
      assert.equal(typeof item[field], 'string', `${route} ${field} must remain exact`);
      const malformed = structuredClone(response);
      (malformed.items?.[0] ?? malformed)[field] = Number(item[field]);
      assert.equal(validate(malformed), false, `${route} must reject rounded money numbers`);
    }
  }
  const adminUsage = fixtures.financial_reads['token/admin/v1/usage-requests'].items[0];
  assert.equal(adminUsage.newapi_request_id, 'fixture-newapi-request');
  assert.equal(adminUsage.reconciliation_last_error, 'fixture-reconciliation-error');
  const refundKey = admin.components.parameters.RefundIdempotencyKey;
  assert.deepEqual([refundKey.in, refundKey.name, refundKey.required, refundKey.schema.minLength], ['header', 'Idempotency-Key', true, 16]);
  assert.equal(refundKey.schema.maxLength, undefined, 'preserve the existing refund key contract');
  assert.ok(admin.paths['/billing/admin/v1/payment-orders/{order_id}/refunds'].post.parameters.some((entry) => entry.$ref === '#/components/parameters/RefundIdempotencyKey'));
  const newAPIStatus = ajv.compile({ $ref: 'urn:pinvou:token-runtime#/$defs/NewAPIStatus' });
  assert.ok(newAPIStatus({ success: true }));
  assert.equal(newAPIStatus({ success: true, private_detail: 'unexposed' }), false);
  assert.ok(token.paths['/admin/v1/system/newapi-status'].get.responses['503']);
  const snapshot = ajv.compile({ $ref: 'urn:pinvou:admin-runtime#/$defs/FinancialSnapshot' });
  assert.ok(snapshot({ amount_fen: '9007199254740993', credit_microcredits: '100' }));
  assert.equal(snapshot({ amount_fen: 9007199254740993 }), false);
  assert.ok(snapshot(null));
  const account = YAML.parse(await readFile(path.join(root, 'contracts/openapi/account.yaml'), 'utf8'));
  const accountSchemas = JSON.parse(JSON.stringify(account.components.schemas)
    .replaceAll('common.yaml#/components/schemas/', 'urn:pinvou:common-runtime#/$defs/')
    .replaceAll('#/components/schemas/', '#/$defs/'));
  ajv.addSchema({ $id: 'urn:pinvou:common-runtime', $defs: common.components.schemas });
  ajv.addSchema({ $id: 'urn:pinvou:account-runtime', $defs: accountSchemas });
  const me = ajv.compile({ $ref: 'urn:pinvou:account-runtime#/$defs/Me' });
  assert.ok(me(fixtures.me), JSON.stringify(me.errors));
  assert.ok(me({ ...fixtures.me, mobile: '+8613800000000', mobile_verified: true, commerce_customer_status: 'READY' }), JSON.stringify(me.errors));
  assert.equal(me({ ...fixtures.me, mall_sync_status: 'SYNCED' }), false, 'unpublished draft fields do not form a second compatibility contract');
  assert.equal(me({ ...fixtures.me, mobile_verified: undefined }), false);
  assert.equal(me({ ...fixtures.me, mobile: 13800000000 }), false);
  const registration = ajv.compile({ $ref: 'urn:pinvou:account-runtime#/$defs/RegistrationStatus' });
  assert.ok(registration(fixtures.mobile_registration), JSON.stringify(registration.errors));
  assert.ok(registration({ ...fixtures.mobile_registration, password_preserved: true }));
  assert.equal(registration({ ...fixtures.mobile_registration, password_preserved: undefined }), false);
  assert.equal(registration({ ...fixtures.mobile_registration, password_preserved: 'true' }), false);
  assert.equal(registration({ ...fixtures.mobile_registration, unexpected: true }), false);
  const captchaRequest = { mobile: '+8613800000000', purpose: 'REGISTER' };
  const smsRequest = { ...captchaRequest, username: 'fixtureuser', password_handle: 'A'.repeat(43), password_policy_version: 'user-v1', captcha_id: fixtures.slider_captcha.captcha_id, captcha_offset: 72 };
  const registrationRequest = { username: 'fixtureuser', mobile: captchaRequest.mobile, challenge_id: smsRequest.captcha_id, code: '123456', password_handle: smsRequest.password_handle, password_policy_version: smsRequest.password_policy_version, registration_operation_id: fixtures.slider_captcha.captcha_id, registration_query_token: 'A'.repeat(100) };
  for (const [route, schemaName, valid] of [
    ['/auth/captcha/challenges', 'SliderCaptchaRequest', captchaRequest],
    ['/auth/sms/challenges', 'SMSChallengeRequest', smsRequest],
    ['/auth/register', 'MobileRegistrationRequest', registrationRequest],
  ]) {
    const operation = account.paths[route].post;
    assert.equal(operation.requestBody.required, true);
    assert.equal(operation.requestBody.content['application/json'].schema.$ref, `#/components/schemas/${schemaName}`);
    const validate = ajv.compile({ $ref: `urn:pinvou:account-runtime#/$defs/${schemaName}` });
    assert.ok(validate(valid), `${route}: ${JSON.stringify(validate.errors)}`);
    assert.equal(validate({ ...valid, unexpected: true }), false);
    for (const field of account.components.schemas[schemaName].required) {
      assert.equal(validate({ ...valid, [field]: undefined }), false, `${route} requires ${field}`);
    }
    for (const field of ['captcha_id', 'challenge_id']) {
      if (field in valid) assert.equal(validate({ ...valid, [field]: 'invalid-uuid' }), false);
    }
  }
  const smsRequestValidator = ajv.compile({ $ref: 'urn:pinvou:account-runtime#/$defs/SMSChallengeRequest' });
  assert.equal(smsRequestValidator({ ...smsRequest, username: undefined }), false);
  assert.equal(smsRequestValidator({ ...smsRequest, password_handle: undefined }), false);
  assert.equal(smsRequestValidator({ ...smsRequest, password: 'UnexpectedPassword1' }), false);
  assert.ok(smsRequestValidator({ ...smsRequest, purpose: 'LOGIN', username: undefined, password_handle: undefined, password_policy_version: undefined }));
  const captchaResponse = ajv.compile({ $ref: 'urn:pinvou:account-runtime#/$defs/SliderCaptchaResponse' });
  assert.ok(captchaResponse(fixtures.slider_captcha), JSON.stringify(captchaResponse.errors));
  assert.equal(captchaResponse({ ...fixtures.slider_captcha, piece_y: '14' }), false);
  const smsResponse = ajv.compile({ $ref: 'urn:pinvou:account-runtime#/$defs/SMSChallengeResponse' });
  assert.ok(smsResponse({ challenge_id: smsRequest.captcha_id, expires_at: fixtures.slider_captcha.expires_at, retry_after_seconds: 60 }));
  assert.deepEqual(account.paths['/internal/v1/auth/mobile/identity'].servers, [{ url: '/' }]);
  assert.deepEqual(account.paths['/internal/v1/auth/mobile/identity'].post.security, [{ serviceToken: [] }]);
  for (const [name, schema, fields] of [
    ['entitlement', 'EntitlementPackage', ['amount_fen', 'points_total_microcredits', 'points_remaining_microcredits']],
    ['package_summary', 'EntitlementPackageSummary', ['available_microcredits', 'next_expiry_microcredits']],
    ['order_history', 'RechargeOrderHistoryItem', ['amount_fen', 'display_points']],
  ]) {
    const validate = ajv.compile({ $ref: `urn:pinvou:billing-runtime#/$defs/${schema}` });
    assert.ok(validate(fixtures[name]), `${name}: ${JSON.stringify(validate.errors)}`);
    for (const field of fields) assert.equal(validate({ ...fixtures[name], [field]: 9007199254740993 }), false);
  }
  assert.deepEqual(document.paths['/entitlement-packages'].servers, [{ url: '/internal/v1' }]);
	for (const namespace of ['billing', 'token']) {
	  const validate = ajv.compile({ $ref: `urn:pinvou:${namespace}-runtime#/$defs/EntitlementPackage` });
	  for (const name of ['entitlement_no_expiry', 'entitlement_manual']) {
	    const item = fixtures[name];
	    assert.equal(item.valid_until, null);
	    assert.equal(item.components[0].expires_at, null);
	    assert.ok(validate(item), `${namespace}/${name}: ${JSON.stringify(validate.errors)}`);
	    assert.equal(validate({ ...item, valid_until: undefined }), false, 'nullable expiry remains required');
	    assert.equal(validate({ ...item, valid_until: 'invalid-date' }), false, 'non-null expiry retains date-time validation');
	    const missing = structuredClone(item);
	    delete missing.components[0].expires_at;
	    assert.equal(validate(missing), false, 'component nullable expiry remains required');
	    const malformed = structuredClone(item);
	    malformed.components[0].expires_at = 'invalid-date';
	    assert.equal(validate(malformed), false, 'component non-null expiry retains date-time validation');
	  }
	}
  assert.equal(fixtures.routes['GET /internal/v1/entitlement-packages'], 400);
  const member = ajv.compile(account.components.schemas.OrganizationMember);
  assert.ok(member(fixtures.organization_member), JSON.stringify(member.errors));
  assert.equal(member({ ...fixtures.organization_member, unit_admin_unit_ids: undefined }), false);
  assert.equal(member({ ...fixtures.organization_member, unit_admin_unit_ids: ['not-a-uuid'] }), false);
  const history = ajv.compile({ $ref: 'urn:pinvou:token-runtime#/$defs/UserUsageRecord' });
  const historyItem = fixtures.financial_reads['token/v1/me/usage-requests'].items[0];
  assert.ok(history(historyItem), JSON.stringify(history.errors));
  assert.equal(historyItem.actual_microcredits, '9007199254740993');
  assert.equal(history({ ...historyItem, actual_microcredits: 9007199254740993 }), false);
  assert.ok(history({ ...historyItem, actual_microcredits: null }), JSON.stringify(history.errors));
  assert.equal(error({ ...Object.values(fixtures.token_errors)[0], code: 'UNDECLARED_ERROR' }), false);
});
