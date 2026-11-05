// Explicit disposable loopback fixture only. Importing this module performs no I/O.
import assert from 'node:assert/strict';

export function acceptanceConfiguration(environment, args) {
  assert.deepEqual(args, ['--execute-disposable-fixture'], 'explicit disposable-fixture execution is required');
  const required = name => {
    assert.ok(typeof environment[name] === 'string' && environment[name].length > 0, `${name} must be supplied for the disposable fixture`);
    return environment[name];
  };
  const endpoint = (name, originOnly) => {
    const raw = required(name); let value;
    try { value = new URL(raw); } catch { throw new Error(`${name} must be a disposable loopback URL`); }
    assert.ok(['http:', 'https:'].includes(value.protocol) && ['127.0.0.1', '[::1]'].includes(value.hostname) && !value.username && !value.password && !value.search && !value.hash && (!originOnly || value.pathname === '/'), `${name} must be a disposable loopback URL`);
    return originOnly ? value.origin : value.href;
  };
  return {
    accountOrigin: endpoint('PINVOU_TEST_ACCOUNT_ORIGIN', true),
    tokenOrigin: endpoint('PINVOU_TEST_TOKEN_ORIGIN', true),
    paymentRequestOrigin: endpoint('PINVOU_TEST_PAYMENT_REQUEST_ORIGIN', true),
    oidcTokenURL: endpoint('PINVOU_TEST_OIDC_TOKEN_URL', false),
    clientID: required('PINVOU_TEST_OIDC_CLIENT_ID'),
    audience: required('PINVOU_TEST_OIDC_AUDIENCE'),
    username: required('PINVOU_TEST_OIDC_USERNAME'), password: required('PINVOU_TEST_OIDC_PASSWORD'),
    productID: required('PINVOU_TEST_PRODUCT_ID'), modelID: required('PINVOU_TEST_MODEL_ID'), responseModel: required('PINVOU_TEST_RESPONSE_MODEL'),
  };
}
